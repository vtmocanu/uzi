package main

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/spf13/cobra"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// taskBranchNamespace is the server-owned prefix every handoff task branch lives in
// (PRD #400 Decision 4). The server mints uzi/task/<run-id>; the CLI treats it as the
// only namespace `uzi handoff rm` will delete within (a lifecycle guardrail).
const taskBranchNamespace = "uzi/task/"

// newHandoffCmd builds `uzi handoff` (alias `task`): the CLI half of PRD #400's
// ephemeral, MR-less task runs. The parent command's RunE is the CREATE action and
// implements Decision 6's explicit, non-circular ordering:
//
//	(1) create the task run  -> receive its id and the server-named uzi/task/<id> branch
//	(2) push local HEAD      -> to that branch, with the USER's own git credentials
//	(3) dispatch the run     -> the moment the worker may claim it
//
// The push sits BETWEEN create and dispatch on purpose: the run is not claimable
// until dispatch, so if the push fails we stop and never dispatch — the branch has no
// seed content and the worker must not start. `uzi handoff rm <id>` deletes the
// remote branch client-side (an --mr branch is exempt). Continuation is the EXISTING
// `uzi run follow-up <id>`; watching is `uzi run get/logs --follow` and `uzi tui`.
func newHandoffCmd(env Env, gf *globalFlags) *cobra.Command {
	handoff := &cobra.Command{
		Use:     "handoff",
		Aliases: []string{"task"},
		Short:   "Hand a throwaway task to uzi: push local HEAD, let the worker take it, pull the result",
		Long: "Hand off a long-running, throwaway task to a uzi worker, agent-style. This command\n" +
			"creates a task run, pushes your current HEAD to a server-named uzi/task/<id> branch\n" +
			"with your own git credentials, and dispatches it; the worker commits onto that same\n" +
			"branch, which you pull. No forge issue, no merge request (pass --mr to open one).\n\n" +
			"Context comes from -m, or -f <file> ('-' for stdin), or piped stdin. The repo is\n" +
			"auto-detected from the origin remote; pass --repo to override.\n\n" +
			"The dispatch confirmation prints how to pull, watch, continue, and clean up the run.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runHandoffCreate(env, gf, cmd)
		},
	}
	handoff.Flags().StringP("message", "m", "", "the task context/instruction (or use -f, or pipe it on stdin)")
	handoff.Flags().StringP("file", "f", "", "read the task context from a file (or '-' for stdin)")
	handoff.Flags().String("base", "", "branch the task from this ref instead of local HEAD (pushes <ref> as the seed)")
	handoff.Flags().Bool("mr", false, "have the worker open a merge request for the branch (exempts it from 'uzi handoff rm')")
	handoff.Flags().Bool("review", false, "after the task completes, run a diff-review and produce findings (fetch with 'uzi handoff review <id>')")
	handoff.Flags().Bool("then-fix", false, "after the review, auto-apply a fix run for its findings to the same branch (turns on --review)")
	handoff.Flags().Bool("interactive", false, "Keep the task alive after signal_done to iterate conversationally; wind down with 'uzi run stop'")
	handoff.Flags().String("repo", "", "repo id to run against (overrides origin auto-detection; see 'uzi repo list')")

	handoff.AddCommand(newHandoffRmCmd(env, gf))
	handoff.AddCommand(newHandoffReviewCmd(env, gf))
	return handoff
}

func runHandoffCreate(env Env, gf *globalFlags, cmd *cobra.Command) error {
	msg, _ := cmd.Flags().GetString("message")
	file, _ := cmd.Flags().GetString("file")
	base, _ := cmd.Flags().GetString("base")
	mr, _ := cmd.Flags().GetBool("mr")
	review, _ := cmd.Flags().GetBool("review")
	thenFix, _ := cmd.Flags().GetBool("then-fix")
	interactive, _ := cmd.Flags().GetBool("interactive")
	repoFlag, _ := cmd.Flags().GetString("repo")

	// --then-fix IMPLIES --review: a chained fix consumes the review's findings, so a fix
	// without a review is meaningless. Turning --review on here (rather than erroring) keeps
	// `--then-fix` a single convenient flag.
	reviewRequested := review || thenFix

	// --interactive and --then-fix are mutually exclusive: --then-fix auto-terminates the
	// run into a chained review+fix, while --interactive keeps it alive to iterate. Asking
	// for both is contradictory, so reject it as a usage error (exit 2) rather than silently
	// picking one.
	if interactive && thenFix {
		return uzicli.Exitf(uzicli.ExitUsage, "--interactive and --then-fix are mutually exclusive: --then-fix winds the run down into a review+fix, --interactive keeps it alive")
	}

	// Context: -m > -f (file, or '-' for stdin) > bare stdin (non-TTY).
	context, err := resolveHandoffContext(env, msg, file)
	if err != nil {
		return err
	}
	if strings.TrimSpace(context) == "" {
		return uzicli.Exitf(uzicli.ExitUsage, "a handoff needs context: pass -m <text>, -f <file>, or pipe it on stdin")
	}
	// Validate --base BEFORE creating the run, so a bad ref never leaves an orphaned
	// task run behind. A git refname cannot begin with '-', and a leading-dash srcRef
	// would be misparsed by `git push` as an option (e.g. --receive-pack=...) inside the
	// refspec argv element rather than as a ref.
	if b := strings.TrimSpace(base); strings.HasPrefix(b, "-") {
		return uzicli.Exitf(uzicli.ExitUsage, "--base %q is not a valid ref (a ref cannot start with '-')", b)
	}

	c, err := env.client(gf)
	if err != nil {
		return err
	}

	// Repo: --repo wins; otherwise auto-detect from origin. Resolving the repo can need
	// a git call and an API call, so it happens before the create.
	repoID, err := resolveHandoffRepo(env, cmd.Context(), c, repoFlag)
	if err != nil {
		return err
	}

	// (1) Create — receive the id and the server-named uzi/task/<id> branch.
	run, err := c.CreateTaskRun(cmd.Context(), repoID, context, strings.TrimSpace(base), mr, reviewRequested, thenFix, interactive)
	if err != nil {
		return err
	}
	if run.Branch == nil || strings.TrimSpace(*run.Branch) == "" {
		return uzicli.Exitf(uzicli.ExitGeneric, "task run %s was created without a branch (server contract error); nothing was pushed", run.ID)
	}
	branch := *run.Branch

	// (2) Push — local HEAD (or --base) to the server-named branch, user's creds. The
	// source ref is `base` when --base is set, else HEAD.
	srcRef := "HEAD"
	if b := strings.TrimSpace(base); b != "" {
		srcRef = b // validated non-leading-dash above, before the run was created
	}
	if _, err := env.Git(".", "push", "origin", srcRef+":refs/heads/"+branch); err != nil {
		return uzicli.Exitf(uzicli.ExitGeneric,
			"pushing %s to %s failed: %v\nthe task run %s was created but NOT dispatched, so no worker will claim it; clean it up with 'uzi handoff rm %s'",
			srcRef, branch, err, run.ID, run.ID)
	}

	// (3) Dispatch — only now may the worker claim it.
	dispatched, err := c.DispatchTaskRun(cmd.Context(), run.ID)
	if err != nil {
		return uzicli.Exitf(uzicli.ExitGeneric,
			"dispatching task %s failed: %v\nlocal HEAD was pushed to %s but the run was not dispatched, so no worker will claim it; clean it up with 'uzi handoff rm %s'",
			run.ID, err, branch, run.ID)
	}

	return renderHandoff(env, gf, dispatched, branch, reviewRequested, thenFix)
}

// resolveHandoffContext applies the -m > -f > bare-stdin precedence, reusing the
// run.go helpers: readPlanFile for -f (file or '-'), resolveMessage for the bare
// non-TTY stdin fallback.
func resolveHandoffContext(env Env, msg, file string) (string, error) {
	if strings.TrimSpace(msg) != "" {
		return msg, nil
	}
	if strings.TrimSpace(file) != "" {
		return readPlanFile(env, file)
	}
	return resolveMessage(env, ""), nil
}

// resolveHandoffRepo returns the repo id for the handoff. --repo takes precedence;
// otherwise the origin remote URL is parsed to owner/namespace and matched against the
// caller's repos by PathWithNamespace. Exactly one match resolves; zero or many is a
// usage error naming --repo as the escape hatch.
func resolveHandoffRepo(env Env, ctx context.Context, c uzicli.Client, repoFlag string) (string, error) {
	if strings.TrimSpace(repoFlag) != "" {
		return strings.TrimSpace(repoFlag), nil
	}
	origin, err := env.Git(".", "remote", "get-url", "origin")
	if err != nil {
		return "", uzicli.Exitf(uzicli.ExitUsage,
			"not in a git repo with an 'origin' remote (%v); run from a checkout or pass --repo <id> (see 'uzi repo list')", err)
	}
	path := parseRepoPath(origin)
	if path == "" {
		return "", uzicli.Exitf(uzicli.ExitUsage,
			"could not parse origin %q into an owner/repo path; pass --repo <id> (see 'uzi repo list')", origin)
	}
	repos, err := c.ListRepos(ctx)
	if err != nil {
		return "", err
	}
	var matches []apitypes.RepoDTO
	for _, r := range repos {
		if r.PathWithNamespace == path {
			matches = append(matches, r)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0].ID, nil
	case 0:
		return "", uzicli.Exitf(uzicli.ExitUsage,
			"could not match origin %s to a uzi repo; pass --repo <id> (see 'uzi repo list')", path)
	default:
		return "", uzicli.Exitf(uzicli.ExitUsage,
			"origin %s is ambiguous — it matches %d uzi repos; pass --repo <id> to pick one (see 'uzi repo list')", path, len(matches))
	}
}

// parseRepoPath extracts the owner/namespace/repo path from a git remote URL,
// handling both the https form (`https://host/owner/repo(.git)`, including nested
// groups) and the scp-like SSH form (`git@host:owner/repo(.git)`). The `.git` suffix
// and any leading/trailing slashes are stripped. Returns "" if it cannot find a path.
func parseRepoPath(remote string) string {
	s := strings.TrimSpace(remote)
	s = strings.TrimSuffix(s, ".git")
	if s == "" {
		return ""
	}
	if strings.Contains(s, "://") {
		// URL form: scheme://[user@]host[:port]/owner/repo
		if u, err := url.Parse(s); err == nil && u.Host != "" {
			return strings.Trim(u.Path, "/")
		}
		return ""
	}
	// scp-like form: [user@]host:owner/repo — the path is everything after the FIRST
	// colon (the host/port separator does not appear in this syntax).
	if i := strings.Index(s, ":"); i >= 0 {
		return strings.Trim(s[i+1:], "/")
	}
	return strings.Trim(s, "/")
}

// renderHandoff prints the dispatched task run. --json emits the run DTO (the agent
// contract); the human render leads with the id and branch, then the pull hint and how
// to watch/continue.
func renderHandoff(env Env, gf *globalFlags, run apitypes.RunDTO, branch string, review, thenFix bool) error {
	p := env.printer(gf)
	if p.Format == uzicli.FormatJSON {
		return p.JSON(run)
	}
	if gf.quiet {
		return nil
	}
	p.Printf("handoff dispatched: run %s on %s\n", run.ID, branch)
	p.Printf("  pull it with:  git fetch origin %s && git switch %s\n", branch, branch)
	p.Printf("  watch it with: uzi run get %s   (or: uzi run logs %s --follow, or: uzi tui)\n", run.ID, run.ID)
	p.Printf("  send more:     uzi run follow-up %s -m \"...\"\n", run.ID)
	if review {
		p.Printf("  a diff-review will run when the task completes; read it with: uzi handoff review %s\n", run.ID)
	}
	if thenFix {
		p.Printf("  a fix run will then auto-apply the review's findings to %s (--then-fix)\n", branch)
	}
	if run.OpenMr {
		p.Printf("  an MR will be opened for this branch; it is exempt from 'uzi handoff rm'\n")
	} else {
		p.Printf("  clean up with: uzi handoff rm %s\n", run.ID)
	}
	return nil
}

// newHandoffRmCmd builds `uzi handoff rm <id>`: delete the task's remote uzi/task/<id>
// branch client-side, with the user's own credentials (`git push origin --delete`).
// A non-task run, or a task that opened an MR (the MR needs its source branch), is
// refused.
func newHandoffRmCmd(env Env, gf *globalFlags) *cobra.Command {
	rm := &cobra.Command{
		Use:   "rm <run-id>",
		Short: "Delete a finished no-MR handoff's remote branch",
		Long: "Delete the remote uzi/task/<id> branch of a handoff task, with your own git\n" +
			"credentials. A task that opened a merge request (--mr) is exempt — delete it via\n" +
			"the merge request instead.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runHandoffRm(env, gf, cmd, args[0])
		},
	}
	return rm
}

// newHandoffReviewCmd builds `uzi handoff review <task-run-id>`: fetch the diff-review a
// `--review` handoff produced (PRD #400 M4a). --json emits the review DTO; the human view
// prints a findings table (file:line severity — summary). A task with no review yet prints
// a hint rather than erroring.
func newHandoffReviewCmd(env Env, gf *globalFlags) *cobra.Command {
	review := &cobra.Command{
		Use:   "review <run-id>",
		Short: "Show the diff-review findings a --review handoff produced",
		Long: "Fetch the structured diff-review findings for a handoff task launched with --review.\n" +
			"Each finding carries a file:line location, a severity (info|warning|error), and a\n" +
			"summary. Pass --json for the machine-readable review. If the task is still running\n" +
			"or was not launched with --review, a hint is printed instead.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runHandoffReview(env, gf, cmd, args[0])
		},
	}
	return review
}

func runHandoffReview(env Env, gf *globalFlags, cmd *cobra.Command, id string) error {
	c, err := env.client(gf)
	if err != nil {
		return err
	}
	rev, err := c.GetTaskReview(cmd.Context(), id)
	if err != nil {
		return err
	}
	p := env.printer(gf)
	if p.Format == uzicli.FormatJSON {
		// A null review still round-trips as JSON `null` so a scripted caller can branch on it.
		return p.JSON(rev)
	}
	if gf.quiet {
		return nil
	}
	if rev == nil {
		p.Printf("no review available yet for task %s (it may still be running, or --review was not requested)\n", id)
		return nil
	}
	p.Printf("review of task %s: %s · %s\n", rev.TargetRunID, rev.Status, findingsPhrase(len(rev.Findings)))
	if s := strings.TrimSpace(rev.SummaryMd); s != "" {
		p.Printf("  %s\n", sanitizeTTY(s))
	}
	for _, f := range rev.Findings {
		// file/severity/summary are agent-authored free text (server-scrubbed at rest), so
		// they go through sanitizeTTY before reaching the terminal — defence in depth,
		// matching the findings backlog render.
		loc := sanitizeTTY(f.File)
		if f.Line > 0 {
			loc = fmt.Sprintf("%s:%d", loc, f.Line)
		}
		if sym := strings.TrimSpace(f.Symbol); sym != "" {
			loc = fmt.Sprintf("%s (%s)", loc, sanitizeTTY(sym))
		}
		p.Printf("  [%s] %s — %s\n", f.Severity, loc, sanitizeTTY(f.Summary))
	}
	return nil
}

// findingsPhrase renders the singular/plural finding count for the review header line.
func findingsPhrase(n int) string {
	if n == 1 {
		return "1 finding"
	}
	return fmt.Sprintf("%d findings", n)
}

func runHandoffRm(env Env, gf *globalFlags, cmd *cobra.Command, id string) error {
	c, err := env.client(gf)
	if err != nil {
		return err
	}
	run, err := c.GetRun(cmd.Context(), id)
	if err != nil {
		return err
	}
	if run.Kind != "task" {
		return uzicli.Exitf(uzicli.ExitUsage, "run %s is not a handoff task (kind=%s)", id, run.Kind)
	}
	// An open merge request needs its source branch, so a task that ACTUALLY opened one
	// (MrWebURL set once the worker opens it) is exempt. Keying on MrWebURL, not the
	// OpenMr *intent* flag: a --mr handoff that failed at push/dispatch before the worker
	// ever opened an MR carries OpenMr=true but has no MR — its branch would otherwise be
	// orphaned with no CLI path to delete it (and the create-time failure hints recommend
	// exactly this command).
	if run.MrWebURL != nil {
		return uzicli.Exitf(uzicli.ExitGeneric,
			"task %s opened a merge request; its branch is exempt from rm — delete it via the merge request", id)
	}
	if run.Branch == nil || strings.TrimSpace(*run.Branch) == "" {
		return uzicli.Exitf(uzicli.ExitGeneric, "task %s has no branch to delete", id)
	}
	branch := strings.TrimSpace(*run.Branch)
	// Lifecycle guardrail (PRD #400 M6): rm only ever deletes inside the server-owned
	// uzi/task/* namespace. A task's branch is minted server-side in that namespace, so
	// this can only fail on a corrupted/unexpected DTO — refuse rather than run
	// `git push --delete` against whatever the field happens to hold.
	if !strings.HasPrefix(branch, taskBranchNamespace) {
		return uzicli.Exitf(uzicli.ExitGeneric,
			"task %s branch %q is not in the %s namespace; refusing to delete it", id, branch, taskBranchNamespace)
	}
	if _, err := env.Git(".", "push", "origin", "--delete", branch); err != nil {
		return uzicli.Exitf(uzicli.ExitGeneric, "deleting %s failed: %v", branch, err)
	}
	p := env.printer(gf)
	if p.Format == uzicli.FormatJSON {
		return p.JSON(map[string]string{"deleted": branch})
	}
	if !gf.quiet {
		p.Printf("deleted %s\n", branch)
	}
	return nil
}
