package main

// Schedule renderers (created-schedule, detail, last-fire, run-now) and the skip-reason
// label/hint maps, split out of schedule.go (PRD #1009 M2). Declaration motion only.

import (
	"fmt"
	"strings"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// renderCreatedSchedule prints a freshly created schedule. Under --json it dumps the
// DTO; in human mode it confirms the id and when it fires next.
func renderCreatedSchedule(env Env, gf *globalFlags, s apitypes.ScheduleDTO) error {
	p := env.printer(gf)
	if p.Format == uzicli.FormatJSON {
		return p.JSON(s)
	}
	if gf.quiet {
		return nil
	}
	when := scheduleWhen(s)
	next := scheduleNext(s, time.Now())
	p.Printf("created schedule %s · %s · next %s\n", s.ID, when, next)
	return nil
}

// renderCreatedSchedules prints the schedules created by a multi-repo `schedule create`
// fan-out: under --json the whole slice, in human mode one "created schedule …" line per
// row. Called both on success and, with the partial slice, on a mid-loop failure so the
// schedules that already landed are reported before the error propagates.
func renderCreatedSchedules(env Env, gf *globalFlags, created []apitypes.ScheduleDTO) error {
	p := env.printer(gf)
	if p.Format == uzicli.FormatJSON {
		return p.JSON(created)
	}
	if gf.quiet {
		return nil
	}
	for _, s := range created {
		p.Printf("created schedule %s · %s · next %s\n", s.ID, scheduleWhen(s), scheduleNext(s, time.Now()))
	}
	return nil
}

// renderScheduleDetail prints a schedule as an aligned key/value block, mirroring
// renderRunDetail. Optional rows (issue/labels/prompt, cron/run_at, last fired, next
// fires) are emitted only when set, so a row is never blank.
func renderScheduleDetail(p *uzicli.Printer, s apitypes.ScheduleDTO) error {
	rows := [][]string{
		{"ID", s.ID},
		{"TARGET", scheduleTarget(s)},
		{"REPO", strOr(&s.RepoPath, "-")},
		{"TIMING", s.Timing},
		{"WHEN", scheduleWhen(s)},
	}
	if s.Timing == schedTimingOnce && s.RunAt != nil {
		rows = append(rows, []string{"RUN_AT", s.RunAt.UTC().Format(time.RFC3339)})
	}
	if s.Timing == schedTimingRecurring {
		rows = append(rows, []string{"TIMEZONE", s.Timezone})
	}
	if s.Target == schedTargetPrompt && s.Prompt != "" {
		rows = append(rows, []string{"PROMPT", s.Prompt})
	}
	if s.Target == schedTargetSweep {
		rows = append(rows, []string{"MAX_ISSUES", maxIssuesStr(s.MaxIssues)})
	}
	if s.Target == schedTargetIssue || s.Target == schedTargetSweep {
		rows = append(rows, []string{"GUIDANCE", strOr(s.Guidance, "-")})
	}
	// A sweep default surfaces the read-only baked catalog guidance separately from the owner
	// overlay (issue #675); BakedGuidance is nil for every other row, so the row is emitted
	// only when present.
	if s.BakedGuidance != nil {
		rows = append(rows, []string{"BAKED_GUIDANCE", strOr(s.BakedGuidance, "-")})
	}
	rows = append(rows,
		[]string{"MODEL", strOr(s.Model, "-")},
		[]string{"APPLY_MODEL_TO_AGENTS", boolStr(s.OverrideSubagentModel != nil && *s.OverrideSubagentModel)},
		[]string{"AUTO_APPROVE", boolStr(s.AutoApprove)},
		[]string{"WAIT_ON_LIMIT", boolStr(s.WaitOnLimit)},
		[]string{"MR_REWORK", triStateStr(s.MrReworkEnabled)},
		[]string{"TOKEN", scheduleTokenCell(s)},
		// HARNESS (PRD #1429 M5, D2): the schedule's per-run harness pin. A null pin is an
		// EXPLICIT "implicit" (D11 resolves it fresh per fire), distinct from "-" (the row's
		// other "not set at all" convention elsewhere in this block), since a nil pin here is
		// a meaningful, deliberate configuration state rather than an absent optional field.
		[]string{"HARNESS", strOr(s.Harness, "implicit")},
		[]string{"ENABLED", boolStr(s.Enabled)},
		[]string{"STATUS", s.Status},
	)
	if s.NextFireAt != nil {
		rows = append(rows, []string{"NEXT_FIRE_AT", s.NextFireAt.UTC().Format(time.RFC3339)})
	}
	if s.LastFiredAt != nil {
		rows = append(rows, []string{"LAST_FIRED_AT", s.LastFiredAt.UTC().Format(time.RFC3339)})
	}
	for i, f := range s.NextFires {
		rows = append(rows, []string{fmt.Sprintf("  next[%d]", i), f.UTC().Format(time.RFC3339)})
	}
	if err := p.Table(nil, rows); err != nil {
		return err
	}
	renderLastFire(p, s.LastFire)
	return nil
}

// scheduleTokenCell renders the TOKEN detail row (PRD #1247 M6): the pinned token's LABEL for
// a pinned override (falling back to the bare "pinned" mode when the label could not be
// resolved — a renamed/deleted token), the bare mode for auto/default, and "inherit" when the
// schedule carries no override (the fired run follows the worker binding).
func scheduleTokenCell(s apitypes.ScheduleDTO) string {
	co := s.CredentialOverride
	if co == nil {
		return "inherit"
	}
	if co.Mode == "pinned" && co.Label != nil && *co.Label != "" {
		return *co.Label
	}
	return co.Mode
}

// skipReasonLabels maps a schedsvc.SkipReason wire string to a short human label for CLI
// output (PRD #308 M5). This is PRESENTATIONAL only — it is NOT the cross-language drift
// guard (that is the Go↔TS test in web/src/lib/scheduleSkipReasons.test.ts). An unknown
// reason falls back to the raw wire string in skipReasonLabel, so a new server-side reason
// degrades gracefully rather than rendering blank.
var skipReasonLabels = map[string]string{
	"not_eligible":          "not eligible",
	"already_running":       "already running",
	"description_too_large": "description too large",
	"fetch_failed":          "fetch failed",
	"schedules_paused":      "all schedules paused",
}

// skipReasonLabel renders a skip reason as its human label, falling back to the raw wire
// string for an unmapped value (graceful degradation — the wire is the source of truth).
func skipReasonLabel(reason string) string {
	if label, ok := skipReasonLabels[reason]; ok {
		return label
	}
	return reason
}

// skipReasonHints carries an optional remediation hint per skip reason for the run-now
// per-candidate breakdown. A reason with no actionable hint is absent (empty), and the
// caller omits the trailing `# …` for it.
var skipReasonHints = map[string]string{
	"not_eligible": "add the configured uzi label or assign the issue to uzi",
}

// skipReasonHint returns the remediation hint for a skip reason, or "" when none applies.
func skipReasonHint(reason string) string { return skipReasonHints[reason] }

// lastFireCappedHint is the one-line steer shown when a capped fire started nothing and
// every examined candidate was skipped — newer eligible issues were never reached. It
// names no reason: the per-candidate skip lines above already say why each was skipped.
const lastFireCappedHint = "newer eligible issues not reached — the candidates ahead of them were skipped; see the reasons above"

// ineligibleMatchedLine returns the diagnostic for a label sweep whose selector matched
// open issues that are not eligible (issue #1543), or "" when the count is unknown (nil:
// a historical fire or a non-label sweep) or zero.
func ineligibleMatchedLine(n *int64) string {
	if n == nil || *n <= 0 {
		return ""
	}
	if *n == 1 {
		return "  1 open issue matches the selector but is not eligible — add the configured uzi label or assign it to uzi"
	}
	return fmt.Sprintf("  %d open issues match the selector but are not eligible — add the configured uzi label or assign them to uzi", *n)
}

// printIneligibleMatched prints the ineligible-matched diagnostic line when it applies.
func printIneligibleMatched(p *uzicli.Printer, n *int64) {
	if line := ineligibleMatchedLine(n); line != "" {
		p.Printf("%s\n", line)
	}
}

// fireCandidateLabel renders a started/skipped candidate's identity: "#<iid>" for an
// issue/sweep candidate, or "prompt" for a prompt schedule (which carries a nil iid).
func fireCandidateLabel(iid *int64) string {
	if iid == nil {
		return "prompt"
	}
	return fmt.Sprintf("#%d", *iid)
}

// renderLastFire appends the "Last fire" block to a schedule detail (PRD #308 M5),
// summarising the schedule's most recent persisted fire: a one-line summary, the runs it
// started, the candidates it skipped (with human reason labels), when a capped fire
// reached nobody the not-reached hint, and (label sweeps, issue #1543) how many selector
// matches are not eligible. A nil last_fire means the schedule never fired.
func renderLastFire(p *uzicli.Printer, lf *apitypes.LastFire) {
	if lf == nil {
		p.Printf("Last fire: never fired\n")
		return
	}
	p.Printf("Last fire:\n")
	p.Printf("  fired %s · examined %d · started %d · skipped %d\n",
		lf.FiredAt.UTC().Format(time.RFC3339), lf.Matched, len(lf.Started), len(lf.Skips))
	for _, st := range lf.Started {
		p.Printf("    %s → run %s  %s\n", fireCandidateLabel(st.IssueIID), st.RunID, st.Title)
	}
	for _, sk := range lf.Skips {
		p.Printf("    %s  %s  %s\n", fireCandidateLabel(sk.IssueIID), skipReasonLabel(sk.Reason), sk.Title)
	}
	if lf.Capped && len(lf.Skips) > 0 && len(lf.Started) == 0 {
		p.Printf("  %s\n", lastFireCappedHint)
	}
	printIneligibleMatched(p, lf.IneligibleMatched)
}

// renderRunNow prints the human outcome of a `schedule run-now` fire (PRD #308 M5) from
// the widened RunNowResponse: a header with the started run ids, a per-started line, and —
// when candidates were skipped — the examined/skipped tally with a human reason label and
// an optional remediation hint per skip. A fire that started nothing AND skipped nothing is
// a benign dedup (a prior run still live), reported as such rather than as "started 0" —
// unless the label sweep reports ineligible selector matches, in which case it says there
// were no eligible candidates (issue #1543). The ineligible line ends every other path.
func renderRunNow(p *uzicli.Printer, id string, res apitypes.RunNowResponse) {
	if res.Created == 0 && len(res.Skips) == 0 {
		if ineligibleMatchedLine(res.IneligibleMatched) != "" {
			// Nothing was a candidate because every selector match is ineligible (issue
			// #1543): say so instead of blaming a benign dedup.
			p.Printf("no run started from %s: no eligible candidates\n", id)
			printIneligibleMatched(p, res.IneligibleMatched)
			return
		}
		p.Printf("no run started from %s (a matching run may already be active)\n", id)
		return
	}
	if res.Created == 0 {
		// The flagship case (a sweep that skipped every candidate): lead with a clean
		// period-terminated clause rather than "Started 0 run(s) from <id>" trailing into
		// the skip breakdown below.
		p.Printf("Started 0 runs from %s.\n", id)
	} else {
		p.Printf("Started %d run(s) from %s", res.Created, id)
		if len(res.RunIDs) > 0 {
			p.Printf(": %s", strings.Join(res.RunIDs, ", "))
		}
		p.Printf("\n")
	}
	for _, st := range res.Started {
		p.Printf("  %s → run %s  %s\n", fireCandidateLabel(st.IssueIID), st.RunID, st.Title)
	}
	if len(res.Skips) > 0 {
		p.Printf("Examined %d candidate(s), skipped %d:\n", res.Matched, len(res.Skips))
		for _, sk := range res.Skips {
			line := fmt.Sprintf("  %s  %s", fireCandidateLabel(sk.IssueIID), skipReasonLabel(sk.Reason))
			if hint := skipReasonHint(sk.Reason); hint != "" {
				line += "   # " + hint
			}
			p.Printf("%s\n", line)
		}
	}
	printIneligibleMatched(p, res.IneligibleMatched)
}
