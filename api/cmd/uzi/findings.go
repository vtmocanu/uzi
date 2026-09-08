package main

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// newFindingsCmd builds `uzi findings` and its verbs (PRD #333 M6): the terminal form of the
// per-repo Findings backlog the worker fills mid-run with off-task bugs. `list` reads the
// coordinate-deduped backlog; `file` turns one coordinate into a real forge issue on the user's
// own connection (server-templated, marker-labelled — the CLI files defaults, the web is the
// rich editor); `dismiss` triages one to `dismissed` with a reason. It mirrors
// `uzi review backlog`/`resolve`/`dismiss`: filing is the human-gated forge write, dismissal a
// local one.
func newFindingsCmd(env Env, gf *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "findings",
		Short: "List and triage the off-task bugs agents flagged mid-run",
	}

	list := &cobra.Command{
		Use:   "list",
		Short: "List your incidental findings, deduped by (repo, location) across runs",
		Long: "List every finding coordinate you own, deduped by (repo, location) so a bug seen in\n" +
			"three runs is ONE row carrying \"seen in 3 runs\", grouped under its repo. The\n" +
			"finding_id each row prints is what `uzi findings file`/`dismiss` act on.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			return runFindingsList(env, gf, c, cmd)
		},
	}
	list.Flags().String("bucket", "", findingsBucketFlagUsage)
	// --repo and --run are forwarded VERBATIM (empty omits the parameter). A well-formed but
	// foreign/unknown id is an EMPTY list, never a 404 — the owner-scoped read matches none of
	// another user's coordinates, so a typo cannot be told from a repo/run with nothing in it
	// (no existence oracle, mirroring `uzi review backlog --run`).
	list.Flags().String("repo", "", "narrow to one repo id (as `uzi repo list` prints it); a foreign/unknown id is an EMPTY list, never a 404")
	list.Flags().String("run", "", "narrow to the coordinates that also occur in this run id; a foreign/unknown id is an EMPTY list, never a 404")

	file := &cobra.Command{
		Use:   "file <finding-id>",
		Short: "File a forge issue from a finding (server-templated, marker-labelled)",
		Long: "File a forge issue from one finding coordinate on your own forge connection. The\n" +
			"title, description and labels are resolved server-side from the stored, sanitised\n" +
			"finding plus a mandatory marker label — the CLI files defaults; edit-before-file is a\n" +
			"web action. Exit 5 if the coordinate is already filed or being filed, 4 if unknown.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			return runFindingsFile(env, gf, c, cmd, args[0])
		},
	}

	dismiss := &cobra.Command{
		Use:   "dismiss <finding-id> --reason wont-do|not-an-issue",
		Short: "Dismiss a finding with a reason",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Validate the invocation BEFORE any network call: a missing/invalid --reason is a
			// usage error (exit 2), raised so the CLI writes nothing when the invocation itself
			// is wrong. Reuses mapDismissReason (the same hyphen→underscore map the judge dismiss
			// uses: wont-do→wont_do, not-an-issue→not_an_issue).
			reason, err := mapDismissReason(cmd)
			if err != nil {
				return err
			}
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			return runFindingsDismiss(env, gf, c, cmd, args[0], reason)
		},
	}
	dismiss.Flags().String("reason", "", "why it is dismissed: wont-do (valid, not worth it) | not-an-issue (false positive)")

	stats := &cobra.Command{
		Use:   "stats",
		Short: "Show your findings triage totals across all your repos",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			return runFindingsStats(env, gf, c, cmd)
		},
	}
	// --repo narrows the tally to one repo, forwarded VERBATIM (empty omits the parameter, so the
	// server tallies every repo). A well-formed but foreign/unknown id is an all-zero tally, never
	// a 404 (no existence oracle); an unparseable id is the server's 400 → exit 2.
	stats.Flags().String("repo", "", "narrow the tally to one repo id (as `uzi repo list` prints it); a foreign/unknown id is an all-zero tally, never a 404")

	undo := &cobra.Command{
		Use:   "undo <finding-id>",
		Short: "Reopen a dismissed finding (undo a dismissal)",
		Long: "Reopen a dismissed finding coordinate, undoing a dismissal (back to the to-file\n" +
			"bucket). The id is the disposition id (present on every backlog row, including a\n" +
			"dismissed one whose evidence was cascaded away). A coordinate that is not dismissed —\n" +
			"unknown, foreign, or never dismissed — is treated as already-undone: a friendly line,\n" +
			"exit 0, never a crash (mirroring the judge undo).",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			return runFindingsUndo(env, gf, c, cmd, args[0])
		},
	}

	cmd.AddCommand(list, file, dismiss, stats, undo)
	return cmd
}

// findingsBucketFlagUsage is the --bucket help text for `uzi findings list`. Like
// backlogBucketFlagUsage it is the ONE place the CLI writes the finding bucket set down, and it
// is documentation, not enforcement: the value is forwarded VERBATIM to the server, which owns
// the validator, so a stale line here misinforms but cannot misbehave (an unknown bucket is the
// server's 400 → exit 2, never a silently empty list). TestFindingsBucketUsageMatchesServerEnum
// pins it to workersvc's finding-bucket set both ways so it cannot drift unnoticed.
const findingsBucketFlagUsage = "filter by disposition bucket: to_file (default) | filed | done | dismissed | all"

// runFindingsList fetches and renders the deduped backlog. --bucket/--repo/--run are forwarded
// verbatim (empty omits the parameter, so the server's default applies — to_file for bucket, no
// filter for repo/run); an unknown bucket is the server's 400, a foreign repo/run an empty list.
// --json passes the server's envelope through unchanged.
func runFindingsList(env Env, gf *globalFlags, c uzicli.Client, cmd *cobra.Command) error {
	bucket, _ := cmd.Flags().GetString("bucket")
	repo, _ := cmd.Flags().GetString("repo")
	run, _ := cmd.Flags().GetString("run")
	b, err := c.ListFindings(cmd.Context(), strings.TrimSpace(bucket), strings.TrimSpace(repo), strings.TrimSpace(run))
	if err != nil {
		return err
	}
	p := env.printer(gf)
	if p.Format == uzicli.FormatJSON {
		return p.JSON(b)
	}
	return renderFindingsBacklog(p, b)
}

// renderFindingsBacklog is the human view: the open-count meta line, then one block per repo
// with a row per coordinate carrying the actionable finding_id, the latest title, "seen in N
// runs", and the disposition status.
//
// Sanitisation follows D4/R4: last_title and repo_path are agent/forge-influenced free text, so
// both go through sanitizeTTY before reaching the terminal; finding_id (a uuid), repo_id (a
// uuid) and status (a closed enum) are structural and print raw.
func renderFindingsBacklog(p *uzicli.Printer, b apitypes.IncidentalFindingBacklogDTO) error {
	p.Printf("open (need filing): %d\n", b.OpenCount)
	if len(b.Findings) == 0 {
		p.Println("no findings in this bucket")
		return nil
	}
	// Group by repo, preserving the first-appearance order the server returned so the output is
	// stable and does not re-sort under the caller.
	var repoOrder []string
	byRepo := map[string][]apitypes.IncidentalFindingDTO{}
	for _, f := range b.Findings {
		if _, seen := byRepo[f.RepoID]; !seen {
			repoOrder = append(repoOrder, f.RepoID)
		}
		byRepo[f.RepoID] = append(byRepo[f.RepoID], f)
	}
	for _, repoID := range repoOrder {
		rows := byRepo[repoID]
		p.Printf("%s (%s):\n", sanitizeTTY(rows[0].RepoPath), repoID)
		for _, f := range rows {
			// A nil finding_id is a display-only, non-actionable coordinate whose evidence rows
			// were cascaded away with a deleted run (D12) — show a dash so a user does not copy
			// nothing into file/dismiss.
			id := "-"
			if f.FindingID != nil {
				id = *f.FindingID
			}
			p.Printf("  %s  %s · seen in %s · %s\n",
				id, sanitizeTTY(f.LastTitle), runsPhrase(f.SeenInRuns), findingStateLabel(f))
		}
	}
	return nil
}

// findingStateLabel renders a coordinate's disposition state for the human view: the raw status,
// enriched where the DTO now carries provenance (PRD #1183 M5). A dismissed row shows its reason
// ("Dismissed · Won't do" / "Dismissed · Not an issue") so the terminal reads the same as the web
// chip; a done row set by the issue-close sync shows the issue it was closed via ("Done via #N").
// dismiss_reason and set_via are closed enums and filed_issue_iid is a structural int, so all three
// print raw — no sanitizeTTY, unlike the agent-authored last_title.
func findingStateLabel(f apitypes.IncidentalFindingDTO) string {
	switch f.Status {
	case dispStatusDismissed:
		if label := dismissReasonLabel(f.DismissReason); label != "" {
			return "Dismissed · " + label
		}
	case dispStatusDone:
		if f.SetVia == findingSetViaIssueClose && f.FiledIssueIID != nil {
			return fmt.Sprintf("Done via #%d", *f.FiledIssueIID)
		}
	}
	return f.Status
}

// dismissReasonLabel maps a dismissal's wire reason to the human phrase the web chip uses, or ""
// for an absent/unknown reason (an open coordinate carries none). Shared vocabulary with the
// judge dismiss (wont_do → "Won't do", not_an_issue → "Not an issue").
func dismissReasonLabel(reason string) string {
	switch reason {
	case dispReasonWontDo:
		return "Won't do"
	case dispReasonNotAnIssue:
		return "Not an issue"
	default:
		return ""
	}
}

// findingSetViaIssueClose is the one set_via provenance value a finding can carry today (PRD #1183
// M3): a `done` coordinate the issue-close sync settled. Sourced as a local literal, not from
// workersvc, so cmd/uzi never links the server stack (TestNoServerDeps).
const findingSetViaIssueClose = "issue_close"

// runFindingsStats fetches and renders the caller's Findings triage totals (PRD #1183 M5),
// mirroring `uzi review stats`. --repo narrows the tally to one repo, forwarded verbatim (empty
// omits the parameter, so the server tallies every repo); --json emits the raw (unenveloped)
// TriageDTO.
func runFindingsStats(env Env, gf *globalFlags, c uzicli.Client, cmd *cobra.Command) error {
	repo, _ := cmd.Flags().GetString("repo")
	t, err := c.GetFindingsStats(cmd.Context(), strings.TrimSpace(repo))
	if err != nil {
		return err
	}
	p := env.printer(gf)
	if p.Format == uzicli.FormatJSON {
		return p.JSON(t)
	}
	// The findings vocabulary is "To triage" for the open bucket, so the row reads TO TRIAGE
	// rather than the judge's TO DO (they share the TriageDTO but not the label).
	rows := [][]string{
		{"TOTAL", strconv.Itoa(t.Total)},
		{"TO TRIAGE", strconv.Itoa(t.Todo)},
		{"FILED", strconv.Itoa(t.Filed)},
		{"DONE", strconv.Itoa(t.Done)},
		{"DISMISSED", strconv.Itoa(t.Dismissed)},
		{"FALSE POSITIVES", strconv.Itoa(t.FalsePositives)},
	}
	return p.Table(nil, rows)
}

// runFindingsUndo reopens a dismissed finding coordinate (PRD #1183 M5), mirroring `uzi review
// undo`: the sentinel ErrFindingNotDismissed (the endpoint's 404 for a non-dismissed/foreign id)
// is softened to a friendly "already undone" line and exit 0, never a crash. Any other failure
// propagates with its real exit code. --json emits a small envelope.
func runFindingsUndo(env Env, gf *globalFlags, c uzicli.Client, cmd *cobra.Command, id string) error {
	err := c.UndoDismissFinding(cmd.Context(), id)
	if errors.Is(err, uzicli.ErrFindingNotDismissed) {
		// Nothing to undo — treat as already-undone: friendly line, exit 0.
		return reportFindingNotDismissed(env, gf, id)
	}
	if err != nil {
		return err
	}
	p := env.printer(gf)
	if p.Format == uzicli.FormatJSON {
		return p.JSON(map[string]any{"finding": id, "status": "reopened", "undone": true})
	}
	if !gf.quiet {
		p.Printf("finding %s: dismissal undone (reopened)\n", id)
	}
	return nil
}

// reportFindingNotDismissed is the friendly already-undone report for `uzi findings undo` when the
// coordinate had no dismissal to undo (the endpoint's 404, softened): exit 0, not a not-found
// failure — mirroring reportNoDisposition on the judge undo.
func reportFindingNotDismissed(env Env, gf *globalFlags, id string) error {
	p := env.printer(gf)
	if p.Format == uzicli.FormatJSON {
		return p.JSON(map[string]any{"finding": id, "status": "no dismissal to undo", "undone": false})
	}
	if !gf.quiet {
		p.Printf("finding %s: no dismissal to undo\n", id)
	}
	return nil
}

// runFindingsFile files the forge issue and reports the created issue. --json emits the server's
// result DTO whole (issue + any warning); the human view prints the iid, web_url and a
// created-with-warning note. A 409 (already filed/filing) and a 404 (unknown/foreign id) arrive
// as *ExitError with exit 5 / 4 from statusError, propagated unchanged.
func runFindingsFile(env Env, gf *globalFlags, c uzicli.Client, cmd *cobra.Command, id string) error {
	res, err := c.FileFinding(cmd.Context(), id)
	if err != nil {
		return err
	}
	p := env.printer(gf)
	if p.Format == uzicli.FormatJSON {
		return p.JSON(res)
	}
	if !gf.quiet {
		// The title is server-sanitised at rest, but it is agent-authored text reaching a TTY,
		// so it goes through sanitizeTTY here too (defence in depth, matching the backlog render).
		p.Printf("filed issue #%d: %s\n", res.Issue.IID, sanitizeTTY(res.Issue.Title))
		if res.Issue.WebURL != "" {
			p.Printf("  %s\n", res.Issue.WebURL)
		}
		// A warning is created-with-warning: the forge issue is REAL, so this is a note on a
		// success (exit 0), never a retry signal.
		if res.Warning != "" {
			p.Printf("warning: %s\n", sanitizeTTY(res.Warning))
		}
	}
	return nil
}

// runFindingsDismiss records the dismissal and reports it. --json emits the coordinate + verdict;
// the human view a terse line. A 409 (not dismissable — already filed/filing/dismissed) and a
// 404 (unknown/foreign id) propagate as exit 5 / 4 from statusError.
func runFindingsDismiss(env Env, gf *globalFlags, c uzicli.Client, cmd *cobra.Command, id, reason string) error {
	if err := c.DismissFinding(cmd.Context(), id, reason); err != nil {
		return err
	}
	p := env.printer(gf)
	if p.Format == uzicli.FormatJSON {
		return p.JSON(map[string]any{"finding": id, "status": "dismissed", "reason": reason})
	}
	if !gf.quiet {
		p.Printf("finding %s: dismissed (%s)\n", id, reason)
	}
	return nil
}
