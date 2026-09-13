package main

// pr.go holds `uzi pr` — the CLI twins of the read-only forge PR/MR views (PRD
// #1255 M3, D10): `pr list` (a repo's open PRs) and `pr checks <iid>` (one PR's
// checks + reviews + merge state, with an optional `--watch`). Both are second
// consumers of the same api routes the TUI (run 2) will use; they decode the
// apitypes DTOs directly (no CLI DTO) and follow the `--json`/table idiom.

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// prWatchInterval is the `uzi pr checks --watch` re-poll cadence (PRD #1255 D10,
// the TUI's 5 s drill-in poll). A package var, not a const, so a test can shrink it
// to ~1 ms and drive the watch deterministically without sleeping for real seconds
// — the established pattern (blinkInterval tui.go, runWaitPollInterval run.go).
var prWatchInterval = 5 * time.Second

// prWatchMaxTransient bounds how many CONSECUTIVE transient failures (a server
// unreachable / 5xx / 429 rate-limit shed, all ExitUnreachable) `--watch` rides out
// before giving up with that error. A single blip must not end the watch, but an
// endlessly failing server must not spin forever; a successful poll resets it.
const prWatchMaxTransient = 10

// newPRCmd builds `uzi pr` with the list and checks subcommands.
func newPRCmd(env Env, gf *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pr",
		Short: "List open pull/merge requests and watch their checks",
		Long: "Read the forge PR/MR views the api serves from the connection's stored PAT " +
			"(PRD #1255): open PRs on a repo, and one PR's live checks, reviews and merge " +
			"state. No `gh`, no personal token. `--repo` defaults to your single enabled " +
			"repo; with several, pass `--repo <id>` (see `uzi repo list`).",
	}

	list := &cobra.Command{
		Use:   "list",
		Short: "List a repo's open pull/merge requests",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			repoFlag, _ := cmd.Flags().GetString("repo")
			repoID, forgeType, err := resolveRepo(cmd.Context(), c, repoFlag)
			if err != nil {
				return err
			}
			pulls, err := c.ListPulls(cmd.Context(), repoID)
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(pulls)
			}
			return renderPullList(p, pulls, mrAbbrev(forgeType))
		},
	}
	list.Flags().String("repo", "", "repo id (defaults to your single enabled repo)")

	checks := &cobra.Command{
		Use:   "checks <iid>",
		Short: "Show one PR's checks, reviews and merge state",
		Long: "Fetch one open PR by its iid and print its checks (name, state, description, " +
			"elapsed), a reviews summary and the merge blocked-reason. With `--watch`, re-fetch " +
			"and re-print on a fixed cadence and exit 0 once no check is still pending.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			iid, err := parseIID(args[0])
			if err != nil {
				return err
			}
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			repoFlag, _ := cmd.Flags().GetString("repo")
			repoID, forgeType, err := resolveRepo(cmd.Context(), c, repoFlag)
			if err != nil {
				return err
			}
			watch, _ := cmd.Flags().GetBool("watch")
			p := env.printer(gf)
			noun := mrAbbrev(forgeType)
			if watch {
				return watchPRChecks(cmd.Context(), env, p, c, repoID, iid, noun)
			}
			detail, err := c.GetPull(cmd.Context(), repoID, iid)
			if err != nil {
				return err
			}
			if p.Format == uzicli.FormatJSON {
				return p.JSON(detail)
			}
			renderPRChecks(p, detail, noun)
			return nil
		},
	}
	checks.Flags().String("repo", "", "repo id (defaults to your single enabled repo)")
	checks.Flags().Bool("watch", false, "re-poll and re-print until no check is pending, then exit 0")

	cmd.AddCommand(list, checks)
	return cmd
}

// watchPRChecks re-fetches the PR on prWatchInterval, re-printing each poll, and
// returns nil (exit 0) the moment no check is still pending (D10: "exit 0 when no
// check is pending"). A transient failure (ExitUnreachable — a 5xx or a 429 shed) is
// ridden out: it prints ONE stderr line and backs off for the server's Retry-After
// (or prWatchInterval when none), keeping the watch alive, up to prWatchMaxTransient
// consecutive failures. Any other error (404/401/…) is returned at once.
func watchPRChecks(ctx context.Context, env Env, p *uzicli.Printer, c uzicli.Client, repoID string, iid int64, noun string) error {
	transient := 0
	for {
		detail, err := c.GetPull(ctx, repoID, iid)
		if err != nil {
			// Only a transient failure — server unreachable / 5xx / a 429 rate-limit shed,
			// all ExitUnreachable — is ridden out; a 404/401/… is a fact about the request
			// and is returned at once.
			var ee *uzicli.ExitError
			if !errors.As(err, &ee) || ee.Code != uzicli.ExitUnreachable {
				return err
			}
			transient++
			if transient >= prWatchMaxTransient {
				return err
			}
			// ONE stderr line about the transient failure (a 429's message names the
			// rate-limit); the run keeps watching. cellText folds a newline a rotted
			// server message could carry so one failure is one line.
			_, _ = fmt.Fprintf(env.Stderr, "pr checks %d: %s; still watching\n", iid, cellText(err.Error()))
			backoff := ee.RetryAfter
			if backoff <= 0 {
				backoff = prWatchInterval
			}
			if done := sleepOrDone(ctx, backoff); done {
				return nil
			}
			continue
		}
		transient = 0
		if p.Format == uzicli.FormatJSON {
			if jerr := p.JSON(detail); jerr != nil {
				return jerr
			}
		} else {
			renderPRChecks(p, detail, noun)
		}
		if !anyCheckPending(detail.Checks) {
			return nil // all checks settled — exit 0
		}
		if done := sleepOrDone(ctx, prWatchInterval); done {
			return nil
		}
	}
}

// sleepOrDone sleeps for d, returning true if the context is cancelled first (a
// clean stop, exit 0 — mirroring `run logs --follow` / `run wait`).
func sleepOrDone(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return true
	case <-time.After(d):
		return false
	}
}

// renderPRChecks prints a PR's checks + reviews + merge state as a human view
// (PRD #1255 D3). Every forge string (branch, title, author, check name/description,
// review author/state, merge reason) is untrusted; the Printer's Table/Printf both
// sanitize at the render boundary, so raw forge text cannot drive the terminal (#180).
func renderPRChecks(p *uzicli.Printer, d apitypes.PullDetailDTO, noun string) {
	p.Printf("%s #%d  %s → %s\n", noun, d.IID, d.SourceBranch, d.TargetBranch)
	p.Printf("%s · %s · +%d −%d\n", d.Title, d.Author, d.Additions, d.Deletions)

	p.Printf("\nCHECKS\n")
	if len(d.Checks) == 0 {
		p.Printf("  (no checks reported)\n")
	} else {
		rows := make([][]string, 0, len(d.Checks))
		for _, ck := range d.Checks {
			rows = append(rows, []string{
				capCell(ck.Name, 40),
				forgeState(ck.Status, ck.Conclusion),
				capCell(ck.Description, 48),
				checkElapsed(ck),
			})
		}
		_ = p.Table([]string{"CHECK", "STATE", "DESCRIPTION", "ELAPSED"}, rows)
	}

	p.Printf("\nREVIEWS\n")
	if len(d.Reviews) == 0 {
		p.Printf("  (none)\n")
	} else {
		for _, rv := range d.Reviews {
			p.Printf("  %s  %s  %s\n", rv.Author, rv.State, relAge(rv.SubmittedAt))
		}
	}

	p.Printf("\nMERGE\n")
	p.Printf("  %s\n", triBoolLabel(d.Merge.Conflicts, "conflicts with the target branch", "no conflicts", "conflict state unknown"))
	if d.Merge.BlockedReason != "" {
		p.Printf("  blocked: %s\n", d.Merge.BlockedReason)
	} else {
		p.Printf("  no merge blockers reported\n")
	}
}

// renderPullList prints the open-PR list as a table (PRD #1255 D3). The first
// column header is the forge noun (PR/MR); the list carries no per-PR checks by
// design (D4), so there is no checks column.
func renderPullList(p *uzicli.Printer, pulls []apitypes.PullDTO, noun string) error {
	rows := make([][]string, 0, len(pulls))
	for _, pr := range pulls {
		rows = append(rows, []string{
			fmt.Sprintf("#%d", pr.IID),
			reviewLabel(pr.ReviewDecision),
			triBoolLabel(pr.Conflicts, "yes", "no", "?"),
			capCell(pr.SourceBranch, 30),
			capCell(pr.Title, 50),
			runLink(pr.RunID),
		})
	}
	return p.Table([]string{noun, "REVIEW", "CONFLICTS", "BRANCH", "TITLE", "↳ RUN"}, rows)
}

// anyCheckPending reports whether any check has not settled — the `--watch`
// termination predicate (D10). A check is SETTLED once its status is "completed"
// (every driver normalises a finished check to that phase) OR it carries a terminal
// conclusion; pending otherwise. Zero checks is "nothing pending" (exit 0).
func anyCheckPending(checks []apitypes.CheckDTO) bool {
	for _, ck := range checks {
		if ck.Status != "completed" && ck.Conclusion == "" {
			return true
		}
	}
	return false
}

// forgeState folds a check/run/job (status, conclusion) pair into one human word
// (PRD #1255 D8: colour is never the only carrier — the word is). While not
// completed it reads the phase (running/queued); once completed it reads the
// conclusion (passed/failed/…). An unrecognised value passes through verbatim and is
// sanitised by the render boundary before it reaches the terminal.
func forgeState(status, conclusion string) string {
	if status != "completed" {
		switch status {
		case "in_progress", "running":
			return "running"
		case "queued", "pending", "waiting":
			return "queued"
		case "":
			return "—"
		default:
			return status
		}
	}
	switch conclusion {
	case "success":
		return "passed"
	case "failure":
		return "failed"
	case "":
		return "done"
	default:
		return conclusion
	}
}

// reviewLabel renders a PR's derived review decision (the closed D6 enum) for a
// table cell, mapping the "none"/"" no-review case to a dash.
func reviewLabel(decision string) string {
	switch decision {
	case "", "none":
		return "—"
	case "changes_requested":
		return "changes"
	case "review_required":
		return "required"
	default:
		return decision
	}
}

// runLink renders the `↳ run` cell for a PR row: the short (8-char) run id when a
// uzi run opened the PR, else a dash.
func runLink(runID *string) string {
	if runID == nil || *runID == "" {
		return "—"
	}
	return "↳ " + shortID(*runID)
}

// triBoolLabel renders a tri-state *bool: nil → unknown, true → yes, false → no.
func triBoolLabel(b *bool, yes, no, unknown string) string {
	if b == nil {
		return unknown
	}
	if *b {
		return yes
	}
	return no
}

// checkElapsed renders a check's wall-clock elapsed from its start: to CompletedAt
// when finished, to now while still running, and a dash when it never started.
func checkElapsed(c apitypes.CheckDTO) string {
	return elapsedBetween(c.StartedAt, c.CompletedAt, c.Status == "completed")
}

// elapsedBetween renders shortDuration(end-start): to `finish` when settled and a
// real finish is recorded, to now otherwise (a still-running unit), and a dash when
// there is no start.
func elapsedBetween(start, finish time.Time, settled bool) string {
	if start.IsZero() {
		return "—"
	}
	end := finish
	if !settled || end.IsZero() || end.Before(start) {
		end = time.Now()
	}
	return shortDuration(end.Sub(start))
}

// shortID is the git-style short id: the first 8 characters. --json always carries
// the full id.
func shortID(id string) string {
	if len(id) >= 8 {
		return id[:8]
	}
	return id
}

// parseIID parses a PR iid / integer argument, returning a usage error (exit 2) on a
// non-integer so a typo fails cleanly before any request.
func parseIID(s string) (int64, error) {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0, uzicli.Exitf(uzicli.ExitUsage, "invalid id %q: want an integer", s)
	}
	return n, nil
}

// resolveRepo resolves the repo id and forge type for a repo-scoped forge-view
// command (PRD #1255 D10). With --repo set it is used verbatim (the target route
// validates it); the forge type is looked up from ListRepos for the human noun.
// With --repo empty it defaults to the caller's single ENABLED repo, and errors with
// exit 2 naming the choices when several are enabled. RepoDTO carries no forge_type,
// so the noun is inferred best-effort from the repo's web-URL host (see
// forgeTypeFromWebURL) — cosmetic only, never control flow.
func resolveRepo(ctx context.Context, c uzicli.Client, repoFlag string) (repoID, forgeType string, err error) {
	repos, lerr := c.ListRepos(ctx)
	if lerr != nil {
		// With an explicit --repo we can still act (the noun is cosmetic and the target
		// route validates the id); without one we cannot pick a default.
		if repoFlag != "" {
			return repoFlag, "", nil
		}
		return "", "", lerr
	}
	if repoFlag != "" {
		for _, r := range repos {
			if r.ID == repoFlag {
				return r.ID, forgeTypeFromWebURL(r.WebURL), nil
			}
		}
		return repoFlag, "", nil // not one of the caller's repos — let the route 404 it
	}
	var enabled []apitypes.RepoDTO
	for _, r := range repos {
		if r.Enabled {
			enabled = append(enabled, r)
		}
	}
	switch len(enabled) {
	case 0:
		return "", "", uzicli.Exitf(uzicli.ExitUsage, "no enabled repositories; pass --repo <id> (see `uzi repo list`)")
	case 1:
		return enabled[0].ID, forgeTypeFromWebURL(enabled[0].WebURL), nil
	default:
		var b strings.Builder
		for i, r := range enabled {
			if i > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "%s (%s)", r.ID, r.PathWithNamespace)
		}
		return "", "", uzicli.Exitf(uzicli.ExitUsage,
			"several enabled repositories; pass --repo <id>: %s", b.String())
	}
}

// forgeTypeFromWebURL best-effort infers a repo's forge kind from its web-URL host,
// the only forge signal RepoDTO carries (it has no forge_type field, and adding one
// is an M2a DTO change, out of this milestone's scope). Only GitLab calls these
// "MR", so a host that looks like GitLab returns "gitlab" and everything else
// returns "github" — the default that yields the "PR" noun mrAbbrev gives GitHub and
// Forgejo, matching the `uzi pr` command name. It feeds ONLY the human table's PR/MR
// noun, never control flow, so a mis-inferred self-hosted host is purely cosmetic.
func forgeTypeFromWebURL(webURL string) string {
	if u, perr := url.Parse(webURL); perr == nil {
		if host := strings.ToLower(u.Hostname()); host == "gitlab.com" || strings.Contains(host, "gitlab") {
			return "gitlab"
		}
	}
	return "github"
}
