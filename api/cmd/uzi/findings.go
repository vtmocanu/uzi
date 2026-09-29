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
// rich editor); `dismiss` triages one to `dismissed` with a reason; `resolve` marks one done
// (issue #1723); `undo` clears either human verdict. It mirrors `uzi review
// backlog`/`resolve`/`dismiss`/`undo`: filing is the human-gated forge write, the rest are local.
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
			"finding_id each row prints is what `uzi findings file`/`dismiss`/`resolve` act on.",
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
		Use:   "file <finding-id> [<finding-id>...]",
		Short: "File a forge issue from one or more findings (server-templated, marker-labelled)",
		Long: "File a forge issue from one finding coordinate on your own forge connection. The\n" +
			"title, description and labels are resolved server-side from the stored, sanitised\n" +
			"finding plus a mandatory marker label — the CLI files defaults; edit-before-file is a\n" +
			"web action. Exit 5 if the coordinate is already filed or being filed, 4 if unknown.\n\n" +
			"Give several finding ids (same repo) to file ONE issue covering all of them; the\n" +
			"findings are linked to that issue. Ids that resolve to the same coordinate count once.\n" +
			"If the filing cannot be confirmed (HTTP 202) the operation id is printed and the exit is 5;\n" +
			"once the operation's deadline has passed and you have checked the forge and\n" +
			"no such issue exists, run\n" +
			"`uzi findings release <operation-id> --confirm-no-issue`.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			if len(args) == 1 {
				return runFindingsFile(env, gf, c, cmd, args[0])
			}
			return runFindingsFileGroup(env, gf, c, cmd, args)
		},
	}

	release := &cobra.Command{
		Use:   "release <operation-id> --confirm-no-issue",
		Short: "Release a stuck group filing after confirming no forge issue was created",
		Long: "Release the findings held by a group filing whose outcome could not be confirmed, so\n" +
			"they can be filed again. The server accepts a release only after the operation's\n" +
			"deadline, and you must first check the forge and find that the issue\n" +
			"was NOT created; --confirm-no-issue records that confirmation. Exit 5 if the\n" +
			"operation cannot be released (yet), 4 if unknown.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if confirmed, _ := cmd.Flags().GetBool("confirm-no-issue"); !confirmed {
				return uzicli.Exitf(uzicli.ExitUsage, "release requires --confirm-no-issue: confirm on the forge that no issue was created for this operation")
			}
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			return runFindingsRelease(env, gf, c, cmd, args[0])
		},
	}
	release.Flags().Bool("confirm-no-issue", false, "confirm you checked the forge and no issue exists for this operation")

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

	resolve := &cobra.Command{
		Use:   "resolve <finding-id>",
		Short: "Mark a finding done (you fixed it or it is handled)",
		Long: "Mark one finding coordinate done: you fixed it, or it is otherwise handled. A local\n" +
			"write, nothing touches the forge. Works from to-file, filed (the issue link is kept),\n" +
			"dismissed (the reason is cleared) and done. Prints the disposition id that\n" +
			"`uzi findings undo` takes. Exit 4 if the id is unknown or not yours, 5 if the\n" +
			"coordinate is being filed right now.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			return runFindingsResolve(env, gf, c, cmd, args[0])
		},
	}

	undo := &cobra.Command{
		Use:   "undo <disposition-id>",
		Short: "Undo a dismissal or a done on a finding",
		Long: "Undo a human verdict on a finding coordinate. A dismissal goes back to the to-file\n" +
			"bucket; a done goes back to filed when the coordinate still has its issue, otherwise\n" +
			"to the to-file bucket. The id is the disposition id (present on every backlog row,\n" +
			"including one whose evidence was cascaded away). A coordinate with nothing to undo —\n" +
			"unknown, foreign, or neither dismissed nor done — is treated as already-undone: a\n" +
			"friendly line, exit 0, never a crash (mirroring the judge undo).",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			return runFindingsUndo(env, gf, c, cmd, args[0])
		},
	}

	cmd.AddCommand(list, file, release, dismiss, resolve, stats, undo)
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
			// A nil finding_id is a coordinate whose evidence rows were cascaded away with a
			// deleted run (D12). The CLI's evidence-id verbs (file/dismiss/resolve) cannot act on
			// it; the web marks it done by disposition id. Show a dash so a user does not copy
			// nothing into file/dismiss/resolve.
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
	// A coordinate claimed by an in-flight group filing shows the operation to release/inspect.
	if f.GroupOperationID != nil && *f.GroupOperationID != "" {
		return "pending group " + sanitizeTTY(*f.GroupOperationID)
	}
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

// runFindingsUndo undoes a dismissal or a done on a finding coordinate (PRD #1183 M5, issue
// #1723), mirroring `uzi review undo`: the sentinel ErrFindingNothingToUndo (the endpoint's 404
// for a foreign id or one that is neither dismissed nor done) is softened to a friendly "already
// undone" line and exit 0, never a crash. Any other failure propagates with its real exit code.
// The reported status is where the coordinate landed (open, or filed for a done that kept its
// issue link), read from the server's reply. --json emits a small envelope.
func runFindingsUndo(env Env, gf *globalFlags, c uzicli.Client, cmd *cobra.Command, id string) error {
	row, err := c.UndoFinding(cmd.Context(), id)
	if errors.Is(err, uzicli.ErrFindingNothingToUndo) {
		// Nothing to undo — treat as already-undone: friendly line, exit 0.
		return reportFindingNothingToUndo(env, gf, id)
	}
	if err != nil {
		return err
	}
	p := env.printer(gf)
	if p.Format == uzicli.FormatJSON {
		return p.JSON(map[string]any{"finding": id, "status": row.Status, "undone": true})
	}
	if !gf.quiet {
		// status is a closed enum, so it prints raw.
		p.Printf("finding %s: undone (now %s)\n", id, row.Status)
	}
	return nil
}

// reportFindingNothingToUndo is the friendly already-undone report for `uzi findings undo` when
// the coordinate had no dismissal or done to undo (the endpoint's 404, softened): exit 0, not a
// not-found failure — mirroring reportNoDisposition on the judge undo.
func reportFindingNothingToUndo(env Env, gf *globalFlags, id string) error {
	p := env.printer(gf)
	if p.Format == uzicli.FormatJSON {
		return p.JSON(map[string]any{"finding": id, "status": "nothing to undo", "undone": false})
	}
	if !gf.quiet {
		p.Printf("finding %s: nothing to undo (not dismissed or done)\n", id)
	}
	return nil
}

// runFindingsResolve marks one finding coordinate done (issue #1723), the finding twin of `uzi
// review resolve`. It reports the disposition id the server returned, since that (not the
// evidence id it was called with) is what `uzi findings undo` takes. A 404 (unknown/foreign id)
// and a 409 (being filed) propagate as exit 4 / 5 from statusError. --json emits the coordinate,
// the status and the disposition id.
func runFindingsResolve(env Env, gf *globalFlags, c uzicli.Client, cmd *cobra.Command, id string) error {
	res, err := c.MarkFindingDone(cmd.Context(), id)
	if err != nil {
		return err
	}
	p := env.printer(gf)
	if p.Format == uzicli.FormatJSON {
		return p.JSON(map[string]any{"finding": id, "status": res.Status, "disposition_id": res.DispositionID})
	}
	if !gf.quiet {
		p.Printf("finding %s: resolved (%s); undo with `uzi findings undo %s`\n", id, res.Status, res.DispositionID)
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

// maxGroupFindings is the server's cap on one group filing. The CLI refuses more than this many
// DISTINCT input ids (repeats do not count) before any request so the draft lookups stay bounded.
const maxGroupFindings = 50

// releaseHint is the operator guidance shared by every unsettled group filing report.
const releaseHint = "release is accepted only after the operation's deadline and only once you checked the forge and no such issue exists"

// runFindingsFileGroup files ONE issue from several evidence ids (issue #1724). Each id is
// resolved to its coordinate's disposition id through the issue-draft read (so older evidence
// ids that map to the same coordinate collapse), deduped in first-seen order. A single distinct
// coordinate falls back to the plain single-file path.
func runFindingsFileGroup(env Env, gf *globalFlags, c uzicli.Client, cmd *cobra.Command, ids []string) error {
	// Cap DISTINCT input ids (the server caps distinct ids) before any request.
	var distinct []string
	seenIn := map[string]bool{}
	for _, id := range ids {
		if !seenIn[id] {
			seenIn[id] = true
			distinct = append(distinct, id)
		}
	}
	if len(distinct) > maxGroupFindings {
		return uzicli.Exitf(uzicli.ExitUsage, "at most %d distinct findings can be filed as one issue (got %d)", maxGroupFindings, len(distinct))
	}
	var dispIDs []string
	seen := map[string]bool{}
	for _, id := range distinct {
		d, err := c.FindingIssueDraft(cmd.Context(), id)
		if err != nil {
			return err
		}
		if d.DispositionID == "" {
			return uzicli.Exitf(uzicli.ExitUsage, "finding %s has no triage record yet, so it cannot be filed in a group; leave it out of the group", sanitizeTTY(id))
		}
		if !seen[d.DispositionID] {
			seen[d.DispositionID] = true
			dispIDs = append(dispIDs, d.DispositionID)
		}
	}
	if len(dispIDs) == 1 {
		return runFindingsFile(env, gf, c, cmd, distinct[0])
	}
	res, accepted, err := c.FileFindingGroup(cmd.Context(), dispIDs)
	if err != nil {
		var ee *uzicli.ExitError
		if errors.As(err, &ee) && ee.Code == uzicli.ExitConflict {
			return explainGroupConflict(env, cmd, c, ee, dispIDs)
		}
		return err
	}
	p := env.printer(gf)
	if p.Format == uzicli.FormatJSON {
		if err := p.JSON(res); err != nil {
			return err
		}
	} else {
		if res.Issue != nil && !gf.quiet {
			p.Printf("filed issue #%d: %s\n", res.Issue.IID, sanitizeTTY(res.Issue.Title))
			if res.Issue.WebURL != "" {
				p.Printf("  %s\n", res.Issue.WebURL)
			}
			p.Printf("linked findings: %s\n", strings.Join(res.DispositionIDs, ", "))
		}
		// An unsettled operation (202, or a 201 not yet settled) and any warning are always
		// printed, --quiet included: the operation id is what the user later inspects, and must
		// not be released blindly.
		if res.Phase != "settled" || accepted {
			p.Printf("operation %s (%s)\n", sanitizeTTY(res.OperationID), sanitizeTTY(res.Phase))
		}
		if res.Warning != "" && (res.Phase != "settled" || accepted || !gf.quiet) {
			p.Printf("warning: %s\n", sanitizeTTY(res.Warning))
		}
		if res.Issue == nil {
			p.Printf("check the forge; if no issue exists, run `uzi findings release %s --confirm-no-issue` (%s)\n", sanitizeTTY(res.OperationID), releaseHint)
		}
	}
	if accepted || res.Issue == nil {
		return uzicli.Exitf(uzicli.ExitConflict, "group filing operation %s is not settled (phase %s)", res.OperationID, res.Phase)
	}
	return nil
}

// explainGroupConflict enriches the group POST's plain 409 ("finding not fileable", no operation
// id) from the backlog. The detail goes straight to stderr, one line per pending operation
// (deduped) or non-open coordinate, because root prints the returned error through cellText,
// which folds newlines and cuts at 200 runes and would truncate UUID operation ids. A failed
// backlog read leaves the original error untouched.
func explainGroupConflict(env Env, cmd *cobra.Command, c uzicli.Client, orig *uzicli.ExitError, dispIDs []string) error {
	b, err := c.ListFindings(cmd.Context(), "all", "", "")
	if err != nil {
		return orig
	}
	selected := map[string]bool{}
	for _, d := range dispIDs {
		selected[d] = true
	}
	seenOps := map[string]bool{}
	var lines []string
	for _, f := range b.Findings {
		if !selected[f.DispositionID] {
			continue
		}
		switch {
		case f.GroupOperationID != nil && *f.GroupOperationID != "":
			if op := sanitizeTTY(*f.GroupOperationID); !seenOps[op] {
				seenOps[op] = true
				lines = append(lines, "pending operation "+op)
			}
		case f.Status != "open":
			lines = append(lines, fmt.Sprintf("finding %s is %s", f.DispositionID, sanitizeTTY(f.Status)))
		}
	}
	if len(lines) == 0 {
		return orig
	}
	for _, l := range lines {
		_, _ = fmt.Fprintln(env.Stderr, l)
	}
	return uzicli.Exitf(uzicli.ExitConflict, "%s (details above)", orig.Error())
}

// runFindingsRelease releases a stuck group filing operation the owner confirmed produced no
// issue. --json emits the server's result DTO.
func runFindingsRelease(env Env, gf *globalFlags, c uzicli.Client, cmd *cobra.Command, opID string) error {
	res, err := c.ReleaseFindingGroup(cmd.Context(), opID)
	if err != nil {
		return err
	}
	p := env.printer(gf)
	if p.Format == uzicli.FormatJSON {
		return p.JSON(res)
	}
	if !gf.quiet {
		p.Printf("released operation %s\n", sanitizeTTY(res.OperationID))
	}
	return nil
}
