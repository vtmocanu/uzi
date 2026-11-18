package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode"

	"github.com/spf13/cobra"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/runkind"
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
			"branch, which you pull. Pass --issue to link an existing issue, --mr to open a merge request.\n\n" +
			"Context comes from -m, or -f <file> ('-' for stdin), or piped stdin. The repo is\n" +
			"auto-detected from the origin remote; pass --repo to pick among the repos origin matches (path and host).\n\n" +
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
	handoff.Flags().Int64("issue", 0, "link this handoff to an existing issue IID")
	handoff.Flags().String("repo", "", "repo id to run against (picks among the repos origin matches by path and host; see 'uzi repo list')")

	handoff.AddCommand(newHandoffRmCmd(env, gf))
	handoff.AddCommand(newHandoffReviewCmd(env, gf))
	return handoff
}

func runHandoffCreate(env Env, gf *globalFlags, cmd *cobra.Command) error {
	var continuedIssueIID *int64
	if cmd.Flags().Changed("issue") {
		iid, err := cmd.Flags().GetInt64("issue")
		if err != nil || iid <= 0 {
			return uzicli.Exitf(uzicli.ExitUsage, "--issue must be a positive issue IID")
		}
		continuedIssueIID = &iid
	}
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

	// issue #403 F3: the auto-review diffs against base_branch; when --base is omitted the seed is
	// local HEAD, so record HEAD's commit SHA as the review base. Otherwise the review defaults to
	// the repo's default branch and its diff would include the user's own seeded commits — findings
	// against code the user wrote, and --then-fix would try to "fix" the seed. When --base IS given
	// the seed already IS that ref, so its name stays the base (the push source is that ref too).
	baseBranch := strings.TrimSpace(base)
	if baseBranch == "" {
		if head, herr := env.Git(".", "rev-parse", "HEAD"); herr == nil {
			baseBranch = strings.TrimSpace(head)
		}
		// On rev-parse failure fall back to empty (today's behavior); the seed push below would
		// fail anyway if HEAD is unresolvable, so nothing is silently created against a bad base.
	}

	// (1) Create — receive the id and the server-named uzi/task/<id> branch.
	run, err := c.CreateTaskRun(cmd.Context(), repoID, context, baseBranch, mr, reviewRequested, thenFix, interactive, continuedIssueIID)
	if err != nil {
		return err
	}
	if run.Branch == nil || strings.TrimSpace(*run.Branch) == "" {
		return uzicli.Exitf(uzicli.ExitGeneric, "task run %s was created without a branch (server contract error); nothing was pushed", run.ID)
	}
	branch := *run.Branch
	printNotice(env, run.Notice)

	// (2) Push — local HEAD (or --base) to the server-named branch, user's creds. The
	// source ref is `base` when --base is set, else HEAD.
	srcRef := "HEAD"
	if b := strings.TrimSpace(base); b != "" {
		srcRef = b // validated non-leading-dash above, before the run was created
	}
	if _, err := env.Git(".", "push", "origin", srcRef+":refs/heads/"+branch); err != nil {
		// The remediation is multi-clause and would exceed the 200-rune cap root.go's
		// one-line "uzi: <err>" report applies (render.go compactText), which silently
		// truncates the actionable tail. Print the guidance in full here and return a
		// short summary error — the pattern used at run_steer.go.
		_, _ = fmt.Fprintf(env.Stderr,
			"the task run %s was created but NOT dispatched, so no worker will claim it; cancel it with 'uzi run cancel %s'\n",
			run.ID, run.ID)
		return uzicli.Exitf(uzicli.ExitGeneric, "pushing %s to %s failed: %v", srcRef, branch, err)
	}

	// (3) Dispatch — only now may the worker claim it.
	dispatched, err := c.DispatchTaskRun(cmd.Context(), run.ID)
	if err != nil {
		// Full remediation via a direct Fprintln so it survives root.go's 200-rune
		// one-line error cap (render.go compactText); the returned error stays short.
		_, _ = fmt.Fprintf(env.Stderr,
			"local HEAD was pushed to %s, but the dispatch did not confirm — the run may or may not have become claimable. Check it with 'uzi run get %s': if it shows dispatched or running, the handoff succeeded; if it is still queued and undispatched, the server will expire it automatically within its setup deadline, or you can cancel it with 'uzi run cancel %s' once you have confirmed it is still undispatched.\n",
			branch, run.ID, run.ID)
		return uzicli.Exitf(uzicli.ExitGeneric, "dispatching task %s failed: %v", run.ID, err)
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

// resolveHandoffRepo returns the repo id for the handoff. Handoff ALWAYS resolves origin,
// because the seed push targets the local `origin` remote (runHandoffCreate step 2). When
// --repo is set it is validated to name the SAME forge repo origin points at (path and host);
// otherwise origin is matched against the caller's repos by PathWithNamespace and host
// (exactly one match resolves). --repo only picks among the repos origin matches; it never
// targets one origin does not point at.
func resolveHandoffRepo(env Env, ctx context.Context, c uzicli.Client, repoFlag string) (string, error) {
	// issue #403 F2: handoff pushes local HEAD to origin (git push origin …:refs/heads/<branch>),
	// so the run's repo MUST be the same forge repo origin points at — else the worker clones a
	// different repo, finds no seed branch, and silently seeds from that repo's default branch,
	// dropping the user's HEAD. We therefore always resolve+parse origin and require --repo (when
	// given) to match it; --repo remains only the disambiguator for an origin that matches several
	// of the caller's repos, never a way to target a repo origin does NOT point at.
	origin, err := env.Git(".", "remote", "get-url", "origin")
	if err != nil {
		return "", uzicli.Exitf(uzicli.ExitUsage,
			"not in a git repo with an 'origin' remote (%v); handoff pushes local HEAD to origin, so run it from a checkout", err)
	}
	path := parseRepoPath(origin)
	if path == "" {
		return "", uzicli.Exitf(uzicli.ExitUsage,
			"could not parse origin %q into an owner/repo path; handoff pushes local HEAD to origin, so it must be a resolvable remote", origin)
	}
	info, err := parseRemote(origin)
	if err != nil {
		return "", uzicli.Exitf(uzicli.ExitUsage, "%v", err)
	}
	repos, err := c.ListRepos(ctx)
	if err != nil {
		return "", err
	}
	oh := &originHost{info: info, origin: origin}
	originName := info.host + "/" + path

	if repoFlag := strings.TrimSpace(repoFlag); repoFlag != "" {
		for _, r := range repos {
			if r.ID != repoFlag {
				continue
			}
			if r.PathWithNamespace != path {
				return "", uzicli.Exitf(uzicli.ExitUsage,
					"--repo %s (%s/%s) does not match origin %s; handoff pushes local HEAD to origin, so they must be the same repo",
					repoFlag, displayHost(webHost(r)), r.PathWithNamespace, originName)
			}
			resolved, rerr := oh.resolve(ctx, env)
			if rerr != nil {
				return "", rerr
			}
			if !hostsMatch(info.form, resolved, webHost(r)) {
				return "", uzicli.Exitf(uzicli.ExitUsage,
					"--repo %s (%s/%s) does not match origin %s%s; handoff pushes local HEAD to origin, so they must be the same repo",
					repoFlag, displayHost(webHost(r)), r.PathWithNamespace, originName, oh.note(resolved))
			}
			return repoFlag, nil
		}
		return "", uzicli.Exitf(uzicli.ExitUsage,
			"--repo %s is not one of your uzi repos (see 'uzi repo list')", repoFlag)
	}

	var pathMatches []apitypes.RepoDTO
	for _, r := range repos {
		if r.PathWithNamespace == path {
			pathMatches = append(pathMatches, r)
		}
	}
	if len(pathMatches) == 0 {
		return "", uzicli.Exitf(uzicli.ExitUsage,
			"could not match origin %s to a uzi repo; pass --repo <id> (see 'uzi repo list')", originName)
	}
	// Origin facts are evaluated once, and only now that a path-equal candidate exists.
	resolved, err := oh.resolve(ctx, env)
	if err != nil {
		return "", err
	}
	var matches []apitypes.RepoDTO
	for _, r := range pathMatches {
		if hostsMatch(info.form, resolved, webHost(r)) {
			matches = append(matches, r)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0].ID, nil
	case 0:
		return "", uzicli.Exitf(uzicli.ExitUsage,
			"could not match origin %s%s to a uzi repo on the same host; handoff pushes local HEAD to origin, so use an origin on the forge's host (an https remote works)", originName, oh.note(resolved))
	default:
		return "", uzicli.Exitf(uzicli.ExitUsage,
			"origin %s is ambiguous — it matches %d uzi repos; pass --repo <id> to pick one (see 'uzi repo list')", originName, len(matches))
	}
}

// remoteForm is the syntax family of an origin URL.
type remoteForm int

const (
	formHTTPS  remoteForm = iota + 1 // http:// and https://
	formSCP                          // scp-like [user@]host:path
	formSSHURL                       // ssh://[user@]host[:port]/path
)

// isSSH reports whether the remote is reached over ssh (scp-like or ssh://).
func (f remoteForm) isSSH() bool { return f == formSCP || f == formSSHURL }

// remoteInfo is what parseRemote extracts from an origin URL. host is lowercased only
// (a trailing dot is kept, as ssh would pass it through).
type remoteInfo struct {
	form    remoteForm
	host    string
	user    string
	port    string
	hasUser bool
	hasPort bool
	path    string // ssh forms: the repo path git sends in its remote command
}

// parseRemote classifies an origin URL and extracts its host (and, for ssh, the user and
// port that ssh -G needs). It fails closed: anything that is not http(s), ssh:// or the
// scp-like form (git://, file://, git+ssh://, local paths, bracketed hosts) is an error
// naming the origin, because the host cannot be compared with the forge's.
func parseRemote(remote string) (remoteInfo, error) {
	s := strings.TrimSpace(remote)
	refuse := func(why string) (remoteInfo, error) {
		return remoteInfo{}, fmt.Errorf("origin %q is not supported for handoff (%s); handoff pushes local HEAD to origin and must verify its host, so use an https or ssh remote", remote, why)
	}
	if s == "" || strings.HasPrefix(s, "[") {
		return refuse("unrecognized remote")
	}
	if i := strings.Index(s, "://"); i >= 0 {
		scheme := strings.ToLower(s[:i])
		switch scheme {
		case "http", "https":
			u, err := url.Parse(s)
			if err != nil || u.Hostname() == "" {
				return refuse("unparsable URL")
			}
			if hasSpaceOrControl(u.Hostname()) {
				return refuse("whitespace or control character in host")
			}
			return remoteInfo{form: formHTTPS, host: strings.ToLower(u.Hostname())}, nil
		case "ssh":
			// git percent-decodes the whole ssh:// URL before splitting it, while
			// url.Parse decodes only per component, so any '%' can make the host we
			// verify differ from the one git connects to.
			if strings.Contains(s, "%") {
				return refuse("percent-encoding in an ssh:// URL")
			}
			u, err := url.Parse(s)
			if err != nil || u.Hostname() == "" {
				return refuse("unparsable URL")
			}
			info := remoteInfo{form: formSSHURL, host: strings.ToLower(u.Hostname())}
			if u.User != nil {
				if _, hasPass := u.User.Password(); hasPass {
					return refuse("password in an ssh:// user")
				}
				if u.User.Username() == "" {
					return refuse("empty ssh user")
				}
				info.user, info.hasUser = u.User.Username(), true
			}
			if strings.HasSuffix(u.Host, ":") {
				return refuse("empty ssh port")
			}
			if p := u.Port(); p != "" {
				info.port, info.hasPort = p, true
			}
			if hasSpaceOrControl(info.host) || hasSpaceOrControl(info.user) || hasSpaceOrControl(info.port) {
				return refuse("whitespace or control character in ssh host, user or port")
			}
			// git takes the path raw from the first '/' after the authority.
			rest := s[i+3:]
			if j := strings.Index(rest, "/"); j >= 0 {
				info.path = gitSSHPath(rest[j:])
			}
			return info, nil
		default:
			return refuse("unsupported scheme " + scheme + "://")
		}
	}
	// scp-like: only when a ':' appears before the first '/' (git's rule).
	colon := strings.Index(s, ":")
	slash := strings.Index(s, "/")
	if colon < 0 || (slash >= 0 && slash < colon) {
		return refuse("local path or unrecognized remote")
	}
	hostPart := s[:colon]
	if strings.Contains(hostPart, "[") {
		return refuse("bracketed host")
	}
	info := remoteInfo{form: formSCP}
	if at := strings.LastIndex(hostPart, "@"); at >= 0 {
		info.user, info.hasUser = hostPart[:at], true
		hostPart = hostPart[at+1:]
		if info.user == "" {
			return refuse("empty ssh user")
		}
	}
	if hostPart == "" {
		return refuse("empty host")
	}
	info.host = strings.ToLower(hostPart)
	if hasSpaceOrControl(info.host) || hasSpaceOrControl(info.user) {
		return refuse("whitespace or control character in ssh host or user")
	}
	info.path = gitSSHPath(s[colon+1:])
	return info, nil
}

// gitSSHPath applies git's ssh path rule: "/~user/repo" is sent as "~user/repo".
func gitSSHPath(p string) string {
	if len(p) > 1 && p[0] == '/' && p[1] == '~' {
		return p[1:]
	}
	return p
}

// receivePackCommand is the remote command git push runs over ssh for path: the default
// receive-pack program with the path single-quoted as git's sq_quote does. ssh -G gets it
// so that Match command / sessiontype rules resolve as they will for the push.
func receivePackCommand(path string) string {
	return "git-receive-pack '" + strings.ReplaceAll(path, "'", `'\''`) + "'"
}

// hasSpaceOrControl reports whether s holds any whitespace or control rune (newline, tab,
// NUL, DEL, ...). Such a value could forge a line of ssh -G output or garble a message.
func hasSpaceOrControl(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0
}

// webHost is the lowercased host of a connected repo's forge web URL, "" when it is
// empty or unparsable (and so never matches).
func webHost(r apitypes.RepoDTO) string {
	u, err := url.Parse(r.WebURL)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

func displayHost(h string) string {
	if h == "" {
		return "(unknown host)"
	}
	return h
}

// hostsMatch is the pure per-candidate host comparison: both hosts must be known; https
// compares them directly; ssh additionally accepts GitHub's ssh.github.com alias for
// github.com (ssh only: an https remote never resolves through it).
func hostsMatch(form remoteForm, resolved, web string) bool {
	if resolved == "" || web == "" {
		return false
	}
	if resolved == web {
		return true
	}
	return form.isSSH() && resolved == "ssh.github.com" && web == "github.com"
}

// sshConfigTimeout bounds one ssh -G evaluation; tests shrink it.
var sshConfigTimeout = 5 * time.Second

// originHost memoizes the facts about origin's effective host for one handoff: the
// resolved host, a refusal, or a failed ssh -G evaluation. It never memoizes a match
// result, which is per candidate (hostsMatch).
type originHost struct {
	info     remoteInfo
	origin   string
	done     bool
	host     string
	err      error // refusal (usage error)
	evalFail error // ssh -G evaluation failure: no resolved host, not a refusal
}

func (o *originHost) resolve(ctx context.Context, env Env) (string, error) {
	if !o.done {
		o.done = true
		o.host, o.err = o.compute(ctx, env)
	}
	return o.host, o.err
}

// note renders the " (ssh -G resolved X)" / evaluation-failure suffix for messages.
func (o *originHost) note(resolved string) string {
	if !o.info.form.isSSH() {
		return ""
	}
	if resolved != "" {
		return " (ssh -G resolved " + resolved + ")"
	}
	if o.evalFail != nil {
		return " (ssh -G evaluation failed: " + o.evalFail.Error() + ")"
	}
	return ""
}

func (o *originHost) compute(ctx context.Context, env Env) (string, error) {
	info := o.info
	if !info.form.isSSH() {
		return info.host, nil
	}
	refuse := func(format string, a ...any) (string, error) {
		return "", uzicli.Exitf(uzicli.ExitUsage, "origin %q: "+format+"; use an https remote instead", append([]any{o.origin}, a...)...)
	}
	// (a) transport overrides change where ssh actually connects, in ways ssh -G cannot see.
	for _, k := range []string{"GIT_SSH", "GIT_SSH_COMMAND"} {
		if _, ok := env.lookupEnv(k); ok {
			return refuse("%s is set, which overrides the ssh transport, so the real host cannot be verified", k)
		}
	}
	// GIT_CONFIG redirects `git config` (only) to one file, so the core.sshCommand lookup
	// below would not see the scopes git push reads.
	if _, ok := env.lookupEnv("GIT_CONFIG"); ok {
		return refuse("GIT_CONFIG is set, which hides the git configuration push uses, so core.sshCommand cannot be checked")
	}
	out, gerr := env.Git(".", "config", "--get", "core.sshCommand")
	present, cerr := sshCommandConfigured(out, gerr)
	if cerr != nil {
		return refuse("could not determine whether core.sshCommand is set (%v)", cerr)
	}
	if present {
		return refuse("core.sshCommand is set, which overrides the ssh transport, so the real host cannot be verified")
	}
	// (b) input validation: nothing option-shaped or explicitly empty reaches ssh.
	if info.host == "" || (info.hasUser && info.user == "") || (info.hasPort && info.port == "") {
		return refuse("the ssh host, user or port is empty")
	}
	for _, v := range []string{info.host, info.user, info.port} {
		if strings.HasPrefix(v, "-") {
			return refuse("the ssh host, user or port starts with '-'")
		}
	}
	// (c) ssh -G: evaluates (does not connect) the user's ssh config.
	args := []string{"-G"}
	if info.hasUser {
		args = append(args, "-l", info.user)
	}
	if info.hasPort {
		args = append(args, "-p", info.port)
	}
	args = append(args, "--", info.host, receivePackCommand(info.path))
	cctx, cancel := context.WithTimeout(ctx, sshConfigTimeout)
	defer cancel()
	cfg, err := env.sshConfig(cctx, args...)
	if err != nil {
		o.evalFail = err
		return "", nil
	}
	if len(cfg) > sshConfigMaxOutput {
		o.evalFail = errSSHConfigOverflow
		return "", nil
	}
	// (d) the effective hostname.
	// Exactly one line of exactly two fields may name the hostname; anything else (a
	// repeated or malformed line, control characters in the value) is an evaluation failure.
	hostname := ""
	seen := 0
	for _, line := range strings.Split(cfg, "\n") {
		f := strings.Fields(line)
		if len(f) >= 1 && strings.EqualFold(f[0], "hostname") {
			seen++
			if len(f) == 2 {
				hostname = f[1]
			}
		}
	}
	switch {
	case seen == 0:
		o.evalFail = errors.New("no hostname in ssh -G output")
	case seen > 1 || hostname == "":
		o.evalFail = errors.New("ambiguous hostname in ssh -G output")
	case hasSpaceOrControl(hostname):
		o.evalFail = errors.New("control character in ssh -G hostname")
	default:
		return strings.ToLower(hostname), nil
	}
	return "", nil
}

// sshCommandConfigured classifies the result of `git config --get core.sshCommand`:
// success means set (any value, even empty); exit status 1 means unset; anything else
// is an error the caller must treat as a refusal.
func sshCommandConfigured(_ string, err error) (bool, error) {
	if err == nil {
		return true, nil
	}
	var ec interface{ ExitCode() int }
	if errors.As(err, &ec) && ec.ExitCode() == 1 {
		return false, nil
	}
	return false, err
}

// parseRepoPath extracts the owner/namespace/repo path from a git remote URL,
// handling both the https form (`https://host/owner/repo(.git)`, including nested
// groups) and the scp-like SSH form (`git@host:owner/repo(.git)`). The `.git` suffix
// and any leading/trailing slashes are stripped. Returns "" if it cannot find a path.
func parseRepoPath(remote string) string {
	// Trim trailing slashes BEFORE stripping .git, so `…/repo.git/` loses its .git too.
	s := strings.TrimRight(strings.TrimSpace(remote), "/")
	s = strings.TrimRight(strings.TrimSuffix(s, ".git"), "/")
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
	if run.Kind != runkind.Task {
		return uzicli.Exitf(uzicli.ExitUsage, "run %s is not a handoff task (kind=%s)", id, run.Kind)
	}
	// issue #403 F1: an open merge request needs its source branch. The exemption is BRANCH-WIDE
	// (branch_has_open_mr) because a handoff's original task, its auto-review and its --then-fix
	// fix run all SHARE the uzi/task/<id> branch — running rm against a review/fix child (whose
	// own row has no MR) must not delete the open MR's source branch. `|| MrWebURL != nil` keeps
	// the per-run exemption working against a pre-feature api pod that omits the branch field.
	if run.BranchHasOpenMr || run.MrWebURL != nil {
		return uzicli.Exitf(uzicli.ExitGeneric,
			"task %s's branch has an open merge request; it is exempt from rm — delete it via the merge request", id)
	}
	// issue #403 F6: refuse rm while the branch is still in use. The run's own status catches a
	// still-running original (and is robust to a pre-feature api pod that omits branch_has_active_run);
	// branch_has_active_run catches an in-flight review/fix child pushing to the same branch after
	// the original completed. Deleting under either races the worker's push.
	if !isTerminalRunStatus(run.Status) {
		return uzicli.Exitf(uzicli.ExitGeneric,
			"task %s is not finished yet (status %s); wait for it to complete before rm", id, run.Status)
	}
	if run.BranchHasActiveRun {
		return uzicli.Exitf(uzicli.ExitGeneric,
			"task %s still has an active review or fix run on its branch; wait for it to finish before rm", id)
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
