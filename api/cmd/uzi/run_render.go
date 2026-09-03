package main

// run_render.go holds the `uzi run get` detail/report rendering helpers moved out of
// run.go (PRD #1009 M1): the run-detail block, milestone/summary rows, message and
// steer-input rendering, and the wait-limit line formatting.

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/autoselect"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// inputOutcome is the human confirmation line for a submitted input.
func inputOutcome(kind string, serverSide bool) string {
	switch kind {
	case kindApprovePlan:
		return "plan approved"
	case kindRejectPlan:
		if serverSide {
			return "plan rejected (run stopped)"
		}
		return "plan rejection sent"
	case kindCancel:
		if serverSide {
			return "run cancelled"
		}
		return "cancellation sent"
	case kindStop:
		// A stop always enqueues (never a server-side transition), so serverSide is always
		// false here — the worker finalizes the graceful wind-down.
		return "stop sent"
	case kindFollowUp:
		return "follow-up sent"
	case kindRevisePlan:
		return "plan revision sent"
	case kindAnswer:
		return "answer sent"
	case kindScope:
		return "scope directive sent"
	default:
		return "input submitted"
	}
}

// renderRunDetail prints a run as an aligned key/value block. Health + the health
// reason are surfaced here (Risk 4): a run parked behind a locked vault carries
// its reason on the DTO, so `uzi run get` shows it without a webui round-trip.
//
// STOP_KIND is here for PRD #108 M9b, and it is the one row this block genuinely
// owed. Without it an auto-stopped run reads
//
//	STATUS           failed
//	FAILURE_REASON   run cancelled
//
// — byte-identical to a user cancel, because on the live-poller half the worker's
// own SetRunFailed overwrites failure_reason with REASON_CANCELLED. stop_kind is
// the ONLY field that survives both halves of that stop, so without this row the
// CLI cannot distinguish "you stopped this" from "uzi stopped this because your
// updates could not be saved". The reasons need no change here: this block already
// prints HEALTH_REASON and FAILURE_REASON generically, switching on no vocabulary,
// so PRD #108's new reason strings flow through untouched.
func renderRunDetail(p *uzicli.Printer, r apitypes.RunDTO) error {
	rows := [][]string{
		{"ID", r.ID},
		{"KIND", r.Kind},
		// issue #857: what/how/who started the run. A NOT NULL server enum (DEFAULT
		// 'manual'), so it is always set and printed unconditionally like KIND above.
		{"TRIGGER", r.TriggerSource},
		// RunDTO carries no is_revising (issue #750): the detail page keeps its own
		// derivePlanRevision panel, so revising is never surfaced through this helper here.
		{"STATUS", effectiveRunStatus(r.Status, r.IsPlanning, false)},
		{"TITLE", runTitle(r)},
		{"BRANCH", strOr(r.Branch, "-")},
		{mrAbbrev(r.ForgeType), int64Or(r.MrIID, "-")},
		{"HEALTH", r.Health},
	}
	if r.HealthReason != nil && *r.HealthReason != "" {
		rows = append(rows, []string{"HEALTH_REASON", sanitizeTTY(*r.HealthReason)})
	}
	if r.FailureReason != nil && *r.FailureReason != "" {
		rows = append(rows, []string{"FAILURE_REASON", sanitizeTTY(*r.FailureReason)})
	}
	// The run's inferred scheduling requirement set (PRD #84 M4), the CLI twin of the
	// three DTO fields added in 4c. All three are model/inference-derived, hence UNTRUSTED,
	// and each is emit-only-when-set exactly like HEALTH_REASON/FAILURE_REASON above: a run
	// predating the feature, or one whose plan-time inference produced nothing, carries no
	// blank row. The two slices are comma-joined into a single cell before sanitizeTTY, so
	// the untrusted join goes through the same scrub the sibling free-text rows use.
	if len(r.RequiredCapabilities) > 0 {
		rows = append(rows, []string{"REQUIRED_CAPABILITIES", sanitizeTTY(strings.Join(r.RequiredCapabilities, ","))})
	}
	if len(r.RequiredTools) > 0 {
		rows = append(rows, []string{"REQUIRED_TOOLS", sanitizeTTY(strings.Join(r.RequiredTools, ","))})
	}
	if r.SizeClass != "" {
		rows = append(rows, []string{"SIZE_CLASS", sanitizeTTY(r.SizeClass)})
	}
	// Emitted only when set, like its two neighbours — every non-stopped run would
	// otherwise carry a blank row. It is a server-controlled enum, so sanitizeTTY is
	// not strictly required; applied anyway for uniformity with the free text above,
	// and because "server-controlled today" is exactly the assumption that rots.
	if r.StopKind != nil && *r.StopKind != "" {
		rows = append(rows, []string{"STOP_KIND", sanitizeTTY(*r.StopKind)})
	}
	// STOP_REASON is the operator's OPTIONAL free-text cancel reason (issue #525),
	// captured beside stop_kind on the cancel paths. Unlike the STOP_KIND enum it is
	// free text, so it goes through sanitizeTTY like FAILURE_REASON above; emitted only
	// when set, so a non-stopped run (or a stop with no reason given) carries no blank row.
	if r.StopReason != nil && *r.StopReason != "" {
		rows = append(rows, []string{"STOP_REASON", sanitizeTTY(*r.StopReason)})
	}
	// issue #279: a report-only completion opened no MR on purpose, so the `MR -` above
	// would otherwise read as a run that never produced its deliverable. Emitted only when
	// set (a server-controlled bool, false on every normal completion), like STOP_KIND.
	// The scrubbed report_md itself is printed below the table, off the rail — see the
	// tail of this function.
	if r.ReportOnly {
		rows = append(rows, []string{"REPORT_ONLY", "yes"})
	}
	// Plain-English run summaries (PRD #362 M5): the intent ("what this run will
	// implement"), the plan summary ("what the proposed plan will do"), and the deltas
	// (how the plan diverged from the ask). All three are model-authored UNTRUSTED text
	// and every one is emit-only-when-set, so a pre-feature run or one whose summaries
	// have not landed yet is byte-for-byte unchanged. Routed through cellText below.
	rows = append(rows, summaryRows(r)...)
	// PRD-link lifecycle (#150), the CLI twin of the fields exposed on the DTO in the
	// prior commit. Both rows are emit-only-when-set: a run that moved no PRD, or one
	// predating the feature, must not print a blank row. PRD_MOVE carries the run's own
	// worker-declared path, so it is untrusted text and goes through sanitizeTTY like the
	// free-text rows above; PRD_PATCH_SETTLED_AT is a server timestamp rendered exactly
	// like LIMIT_RESETS_AT below.
	if r.PrdDonePath != nil {
		rows = append(rows, []string{"PRD_MOVE", sanitizeTTY(*r.PrdDonePath)})
	}
	if r.PrdPatchSettledAt != nil {
		rows = append(rows, []string{"PRD_PATCH_SETTLED_AT", r.PrdPatchSettledAt.UTC().Format(time.RFC3339)})
	}
	// Milestone progress + effective budget (PRD #122 M5), the CLI twin of the web's
	// MilestoneChecklist / MilestoneBadge. Both blocks are conditional so a run with no
	// frozen milestone list and a global-default budget is byte-for-byte unchanged — the
	// same back-compat contract the DTO's nil slices and null budgets carry.
	rows = append(rows, milestoneRows(r)...)
	// The NOW row (PRD #1064 M5, D7): the run's server-derived current_activity, placed
	// right after the MILESTONES block. nowRow returns nil for a terminal run or one with
	// no activity, so a finished run and any pre-#1064 run are byte-for-byte unchanged.
	if row := nowRow(r); row != nil {
		rows = append(rows, row)
	}
	if r.BudgetMaxIterations != nil {
		rows = append(rows, []string{"BUDGET_ITERATIONS", itoa(*r.BudgetMaxIterations)})
	}
	if r.BudgetWallSeconds != nil {
		rows = append(rows, []string{"BUDGET_WALL", fmtUntil(time.Duration(*r.BudgetWallSeconds) * time.Second)})
	}
	// Which Anthropic credential this run spent (PRD #111 M1). Emitted only when the
	// server recorded one — a pre-feature or still-queued run has nothing to say and
	// must not print a blank row.
	//
	// Through cellText, NOT sanitizeTTY, and the difference is load-bearing here in a
	// way it is not for the rows above. This is the first genuinely USER-AUTHORED
	// string in this block. cellText is the table-cell wrapper: sanitizeTTY plus
	// newline folding, tab folding and a length cap.
	//
	// This used to justify itself with "uzicli.Printer.Table does not sanitize what it
	// is handed", which #180 made false: Table now runs CellText over every cell, so
	// the newline fold happens with or without this call. What is left is a genuine
	// reason rather than a residual one — the length cap is cellText's alone, and a
	// call site that names its own untrusted input keeps stating that fact if the row
	// is ever moved off Printer.Table.
	//
	// 🔴 THE PREMISE THIS USED TO GIVE IS NOW FALSE, AND THE CONCLUSION STILL HOLDS —
	// which is exactly why the premise is being corrected rather than deleted. It read
	// "validateSecretLabel rejects control characters and U+FFFD but NOT unicode.Cf, so
	// a bidi-override label is storable". Since PRD #111 M2 the validator DOES reject
	// Cf, so a reader checking that premise would find it false and reasonably conclude
	// that sanitizeTTY now suffices here.
	//
	// It does not, for two reasons that survive the validator:
	//   - NEWLINE FOLDING. sanitizeTTY deliberately spares "\n"; a newline in a table
	//     cell breaks the rail. Only cellText folds it. That is the difference the
	//     mutation control in render_test.go pins, and the one an earlier version of
	//     that control failed to test.
	//   - The validator governs what the SERVER ACCEPTS ON WRITE. Rows stored before
	//     it landed were never re-validated and nothing re-validates on read, so a
	//     hostile label still reaches this line through history, through a future
	//     write path that skips validation, and through a direct database write.
	if r.AnthropicSecretLabel != nil && *r.AnthropicSecretLabel != "" {
		rows = append(rows, []string{"ANTHROPIC_TOKEN", credentialCell(r)})
	}
	rows = append(rows, limitWaitRows(r, time.Now())...)
	// MR_REWORK rides every run like WAIT_ON_LIMIT above, and is tri-state (PRD #841):
	// "inherit" (nil → follow the owner's account default), "on" or "off". An always-present
	// row keeps "inherit" distinguishable from an old CLI that does not know the field.
	rows = append(rows, []string{"MR_REWORK", triStateStr(r.MrReworkEnabled)})
	if err := p.Table(nil, rows); err != nil {
		return err
	}
	// issue #279: the report-only run's actual deliverable is report_md — the lead's
	// server-scrubbed findings. It is UNTRUSTED worker/model text and is multi-line, so it
	// prints on its own lines below the table (a table cell would fold the newlines),
	// through sanitizeTTY exactly like the judge summary. Guarded so a normal completion
	// and a report_only run with an empty summary print nothing.
	if r.ReportOnly && r.ReportMd != nil {
		if s := sanitizeTTY(strings.TrimSpace(*r.ReportMd)); s != "" {
			p.Println()
			p.Println("findings:")
			p.Println(s)
		}
	}
	// PRD #212: the plan turn's worktree writes (git status --porcelain lines), surfaced
	// at the approval gate exactly like the web PlanPanel (M3). Multi-line by nature, so it
	// prints below the table like report_md above, not in a table cell. Each element is
	// UNTRUSTED repo-controlled path text; a synthetic non-porcelain "… (+K more)" marker
	// may be present (print it verbatim, do not parse porcelain status codes).
	//
	// 🔴 sanitizeTTY, NOT cellText. cellText ends in strings.TrimSpace (termsafe.CellText),
	// which strips the LEADING space of the porcelain XY status code (" M path" =
	// modified-in-worktree vs "M  path" = modified-in-index) — the exact corruption M1
	// fixed server-side, and the whole point of storing porcelain lines (status + path).
	// sanitizeTTY preserves the leading space and still strips control/bidi runes. The
	// server already stripped \n/\t per line (M1's sanitizePlanChangedLine), so cellText's
	// newline fold buys nothing here; and this is off-table free text (like the report_md
	// tail), not a tabwriter rail an embedded newline could forge a row in. Emitted only
	// when non-empty: a clean plan turn and any pre-#212 run carry [] and print nothing.
	if len(r.PlanChangedFiles) > 0 {
		p.Println()
		p.Println("files changed during planning:")
		for _, line := range r.PlanChangedFiles {
			p.Println(sanitizeTTY(line))
		}
	}
	return nil
}

// limitWaitRows is the usage-limit park block of `uzi run get` (PRD #35), split out
// because it is the one part of this render with a live clock in it and therefore the
// one part worth testing without a Printer.
//
// It answers TWO different questions off the SAME columns, and conflating them would
// be the bug. The server leaves limit_resets_at / retry_not_before / rate_limit_type in
// place across a promotion, deliberately, as history — so on a parked run they say
// "here is when this resumes", and on a run that has already resumed the identical
// columns say "this run spent part of its wall clock waiting on a usage limit". A
// completed run rendering "resumes in 4h12m" would be the natural result of ignoring
// the distinction, and it would be nonsense.
func limitWaitRows(r apitypes.RunDTO, now time.Time) [][]string {
	// WAIT_ON_LIMIT rides every run, unlike its neighbours above, because it is
	// meaningful BEFORE any park has happened — it is the answer to "will this run
	// survive a usage limit, or just die on one?", which is a question about a queued
	// run as much as a parked one. A conditional row would make "off" indistinguishable
	// from an old CLI that does not know the field.
	rows := [][]string{{"WAIT_ON_LIMIT", boolStr(r.WaitOnLimit)}}
	if r.Status == statusLimitWait {
		rows = append(rows, []string{"LIMIT_WAIT", limitWaitLine(r, now)})
		// The reported window reset, as CONTEXT for the countdown above and never as a
		// substitute for it: a pool-aware promotion can land hours earlier than this.
		if r.LimitResetsAt != nil {
			rows = append(rows, []string{"LIMIT_RESETS_AT", r.LimitResetsAt.UTC().Format(time.RFC3339)})
		}
		return rows
	}
	if r.LimitWaitCount > 0 {
		rows = append(rows, []string{"LIMIT_WAITS", itoa(int(r.LimitWaitCount)) + " (resumed)"})
	}
	return rows
}

// milestoneRows renders a milestone-structured run's progress block for `uzi run get`
// (PRD #122 M5): a `{done}/{total} reported complete` summary followed by one indented
// row per milestone in FROZEN order, marked done / in progress / left. It is the CLI twin
// of the web's MilestoneChecklist, so both surfaces show the same state off the same fields.
// PRD #390 M4: a run that NEVER reported progress (MilestonesCompleted == nil) renders a
// NEUTRAL `–/{total}` numerator instead of `0/{total}`, matching the web badge's `M–/N`
// (see the summary branch below for the null-vs-`[]` contract).
//
// Empty for a run with no frozen milestone list, so a pre-#122 (or non-milestone) run is
// byte-for-byte unchanged — the same back-compat contract the nil Milestones slice carries.
//
// The summary says "reported complete", NEVER "verified" (PRD Decision 6): the worker
// REPORTS a milestone done and nothing in uzi has verified it, so the wording must not
// imply it has. `done` counts only completed ids that are MEMBERS of the frozen list —
// milestones_completed is a monotone union that can still name a milestone dropped after
// it was ticked, and counting those would let the summary read 8/7. Iterating the frozen
// list (rather than reducing over the completed set) also makes the count immune to a
// duplicate id, matching the web's milestoneBadge exactly.
//
// Titles are UNTRUSTED repo/agent-authored text (apitypes.Milestone), so each goes through
// cellText — the same newline-fold, tab-fold and 200-char cap the ANTHROPIC_TOKEN row
// relies on, which is what keeps a hostile title from breaking the table rail.
func milestoneRows(r apitypes.RunDTO) [][]string {
	if len(r.Milestones) == 0 {
		return nil
	}
	completed := make(map[string]bool, len(r.MilestonesCompleted))
	for _, id := range r.MilestonesCompleted {
		completed[id] = true
	}
	inProgress := make(map[string]bool, len(r.MilestonesInProgress))
	for _, id := range r.MilestonesInProgress {
		inProgress[id] = true
	}
	done := 0
	perMilestone := make([][]string, 0, len(r.Milestones))
	for _, m := range r.Milestones {
		state := "left"
		switch {
		case completed[m.ID]:
			state = "done"
			done++
		case inProgress[m.ID]:
			state = "in progress"
		}
		perMilestone = append(perMilestone, []string{"  " + state, cellText(m.Title)})
	}
	rows := make([][]string, 0, len(r.Milestones)+1)
	// PRD #390 D5: distinguish "never reported" from "reported zero". The null-vs-`[]`
	// contract is carried by MilestonesCompleted: nil ⇒ the milestones_completed column
	// is SQL NULL (the run never reported progress), non-nil (even an empty `[]`) ⇒ a
	// report landed. A never-reported run must render a NEUTRAL numerator (en-dash `–`,
	// matching the web badge's `M–/N`), NOT `0/N`, which reads as a failure. Test nil
	// exactly — len()==0 would conflate a reported empty list with never-reported.
	var summary string
	if r.MilestonesCompleted == nil {
		summary = fmt.Sprintf("–/%d reported complete", len(r.Milestones))
	} else {
		summary = fmt.Sprintf("%d/%d reported complete", done, len(r.Milestones))
	}
	rows = append(rows, []string{"MILESTONES", summary})
	rows = append(rows, perMilestone...)
	return rows
}

// nowRow renders the NOW line of `uzi run get` (PRD #1064 M5, D7): the run's
// server-derived current_activity folded into one row,
// `<agent> · <agent_label> · <tool> <detail> · <age> ago`. It is the CLI twin of the
// web run view's now line, the TUI board's second line (boardSecondLine) and the crew
// rail's `↳` line, and reads RunDTO.CurrentActivity directly — no milestone
// precondition (D5): any non-terminal run that has an activity gets the row.
//
// Rendered ONLY when current_activity is present AND the run is non-terminal. The
// server already returns null for a terminal run and for a run with no tool_use frame,
// so the nil guard alone matches the wire; the terminal-status guard is the same
// belt-and-braces the board's boardShowSecondLine carries — a finished run has no
// "now" — so a hostile/buggy server that leaves an activity on a terminal run still
// prints nothing. A pre-#1064 run, a chat/self-improve run with no activity, and every
// terminal run therefore render byte-for-byte as before.
//
// Each display part is joined with " · " and EMPTY parts are dropped, so a lead frame
// with no dispatch label (AgentLabel "") or a tool with no detail never leaves a
// dangling separator. Age is rendered by relAge — the CLI's existing relative-age
// helper (the steer-queue AGE column) — with the " ago" suffix the PRD mock carries.
//
// Agent, AgentLabel, Tool and Detail are UNTRUSTED, model-authored text. The server
// caps only Detail/AgentLabel, leaving Agent/Tool unsanitized on the wire (D7), so
// every part goes through cellText — the render-time backstop that strips
// terminal-unsafe runes, folds newlines/tabs and caps width, the same wrapper the
// milestone-title and ANTHROPIC_TOKEN rows rely on to keep a hostile value from
// breaking the table rail. The runactivity rule that builds current_activity NEVER
// puts a Bash command in Detail (it uses the Bash tool's description), so the NOW row
// inherits that exclusion and can never surface a raw command.
func nowRow(r apitypes.RunDTO) []string {
	act := r.CurrentActivity
	if act == nil || terminalRunStatuses[r.Status] {
		return nil
	}
	parts := make([]string, 0, 4)
	if agent := cellText(act.Agent); agent != "" {
		parts = append(parts, agent)
	}
	if label := cellText(act.AgentLabel); label != "" {
		parts = append(parts, label)
	}
	// Tool + its most identifying argument as ONE segment: "Edit <path>",
	// "Agent <description>", or a bare tool name when the rule left no detail.
	tool := cellText(act.Tool)
	detail := cellText(act.Detail)
	switch {
	case tool != "" && detail != "":
		parts = append(parts, tool+" "+detail)
	case tool != "":
		parts = append(parts, tool)
	case detail != "":
		parts = append(parts, detail)
	}
	parts = append(parts, relAge(act.At)+" ago")
	return []string{"NOW", strings.Join(parts, " · ")}
}

// summaryRows is the CLI surface of the plain-English run summaries (PRD #362 M5): the
// intent summary, the plan summary, and the per-delta rows describing how the proposed
// plan diverged from the original ask. It is the `run get` twin of RunView's summary
// cards; the web derives a "proposed"/"approved" label from run status, but the CLI
// keeps a single plain "PLAN SUMMARY" label — a status-derived label is a web concern.
//
// EVERYTHING here is model-authored UNTRUSTED text (Decision 10: the summary runner is
// tool-less and a crafted issue/PRD could bias it), so every value goes through cellText
// — the same newline-fold, tab-fold and length cap the ANTHROPIC_TOKEN and milestone-title
// rows rely on to keep a hostile string from breaking the table rail.
//
// Every row is emit-only-when-non-empty: a run with no summaries (pre-feature, still
// queued, or a seeded run that never planned) adds nothing, so its detail is byte-for-byte
// what it was before this milestone. Malformed deltas are already coerced to null
// server-side (Decision 6), so r.SummaryDeltas is nil/[] here rather than junk; the
// per-entry emptiness guard covers a stray blank entry without crashing regardless.
func summaryRows(r apitypes.RunDTO) [][]string {
	var rows [][]string
	if r.SummaryIntent != nil {
		if s := cellText(*r.SummaryIntent); s != "" {
			rows = append(rows, []string{"INTENT", s})
		}
	}
	if r.SummaryPlan != nil {
		if s := cellText(*r.SummaryPlan); s != "" {
			rows = append(rows, []string{"PLAN SUMMARY", s})
		}
	}
	for _, d := range r.SummaryDeltas {
		// The whole line (glyph, kind and text) is sanitized together: kind is a
		// server-validated enum today, but "tolerated on read" means the CLI must not
		// trust it, and cellText over the composed line caps and folds the untrusted
		// text in one pass. Drop an entry whose text SANITIZES to empty (whitespace, or
		// a control/bidi rune that cellText strips) rather than render a bare "+ added:".
		if cellText(d.Text) == "" {
			continue
		}
		rows = append(rows, []string{"DELTA", cellText(deltaGlyph(d.Kind) + " " + d.Kind + ": " + d.Text)})
	}
	return rows
}

// deltaGlyph is the one-rune prefix for a plan-summary delta kind, mirroring the web's
// added/changed/dropped affordance in ASCII the table rail can hold. An unrecognised
// kind (a newer server than this binary) renders a neutral bullet rather than being
// dropped — same pass-through-the-unknown stance as limitWaitLine's rate_limit_type.
func deltaGlyph(kind string) string {
	switch kind {
	case "added":
		return "+"
	case "changed":
		return "~"
	case "dropped":
		return "-"
	default:
		return "•"
	}
}

// credentialCell renders WHICH credential a run spent and WHY (PRD #111 M5, D20).
//
// The label alone was never enough, and this is the sentence that says why: an auto
// pick and a default fallback can name the SAME token, so "console-key" answers
// "which account paid" and leaves "why that account" unanswered. PRD #104's
// compatibility path also creates a row labelled literally `default`, so the label is
// not even a reliable hint at the mode.
//
// Since #754 the two live non-pick reasons name a POOLED token, not the default:
// `pool_stale` renders `auto (pooled token, no fresh readings)` and `open_failed`
// `auto (fell to another pooled token; …)` — the auto lane floors onto a pooled token
// and never spends the out-of-pool default. Only the LEGACY `pool_empty` still renders
// `default (auto: …)`, and only on pre-#754 rows where the default genuinely was spent
// (an empty pool now HOLDS in pool_wait instead). The mode is spelled out rather than
// left as a bare label because an auto pick and a legacy default can name the same
// token, so the label alone cannot say which account paid or why.
//
// An UNRECOGNISED reason prints as itself. The CLI is versioned separately from the
// API, so a newer server can ship a ninth reason this binary has never heard of, and
// inventing a rendering for it — or dropping it — would be worse than passing it
// through. Same stance as the web's autoStatusChip.
//
// A nil reason prints the bare label: a run claimed before M1 recorded no mode, and
// guessing one is exactly the wrong answer.
func credentialCell(r apitypes.RunDTO) string {
	label := cellText(*r.AnthropicSecretLabel)
	// F8's CLI half. The id goes null when the token is deleted while the snapshotted
	// label survives (00086's SET NULL), so this is a normal historical run — and the
	// difference between "go look at this token" and "this token is gone" is worth one
	// word. The web chip says the same thing; CLI parity is a rule here, not a nicety.
	if r.AnthropicSecretID == nil {
		label += " (deleted)"
	}
	if r.AnthropicSelectReason == nil || *r.AnthropicSelectReason == "" {
		return label
	}
	return label + " — " + selectReasonText(autoselect.Reason(*r.AnthropicSelectReason), r.AnthropicHeadroomPct)
}

// selectReasonText is the mode half, EXHAUSTIVE over autoselect.AllReasons() and
// pinned as such by TestCredentialCellCoversEveryReason — asserted over the
// vocabulary rather than by sampling, because a reason with no rendering is invisible
// until the one user it happens to reaches support.
//
// headroom is rendered only where it exists. It is present on an auto pick and nil
// everywhere else, including on D14's retry, where the measurement described the
// credential that would NOT open.
//
// 🔴 A LATENT COUPLING WITH THE WEB, recorded because no test can currently see it.
// This appends the suffix only under `auto` and `best_of_pool`; web's
// lib/runCredential.ts appends it for ANY reason with a non-null headroom. The two
// agree today by accident of the server rather than by construction — headroom is
// written only on an auto pick, and D14 explicitly nulls it on the retry, so no
// reachable state distinguishes them. The day something records a headroom on a
// fallback, the CLI silently drops it and the web silently shows it. Whichever
// surface changes first should make the other match.
func selectReasonText(reason autoselect.Reason, headroom *int) string {
	pct := ""
	if headroom != nil {
		pct = ", " + strconv.Itoa(*headroom) + "% headroom"
	}
	switch reason {
	case autoselect.ReasonDefault:
		return "default"
	case autoselect.ReasonPinned:
		return "pinned"
	case autoselect.ReasonJudge:
		return "judge binding"
	case autoselect.ReasonAuto:
		return "auto" + pct
	case autoselect.ReasonBestOfPool:
		// Named rather than folded into `auto`: every pooled token was under the
		// headroom floor and the emptiest was picked anyway (D10). The run worked, and
		// the user's pool is nearly exhausted — a thing to know, not an error.
		return "auto (best of pool)" + pct
	case autoselect.ReasonPoolEmpty:
		// LEGACY VALUE — no longer produced on new runs (#754). An auto worker with
		// a genuinely empty pool now HOLDS the run in pool_wait and spends nothing,
		// rather than falling back to the out-of-pool default. This string is kept
		// only for PRE-#754 historical rows, where the default genuinely WAS spent;
		// it is deliberately worded to stay true for those without implying that an
		// empty pool spends the default today.
		return "default (auto: pool was empty — legacy)"
	case autoselect.ReasonPoolStale:
		// The run FLOORED onto a POOLED token that had no fresh usage reading to rank
		// on — it spent one of the user's OWN pooled tokens as a last resort, not the
		// out-of-pool default (#754). A floored stale token carries no headroom, so no
		// pct is appended here.
		return "auto (pooled token, no fresh readings)"
	case autoselect.ReasonOpenFailed:
		// The selector's picked token would not decrypt, so the run FLOORED onto
		// ANOTHER POOLED token — again not the default (#754).
		return "auto (fell to another pooled token; the chosen one would not open)"
	}
	return string(reason)
}

// renderMessage prints one run message. In --json mode it emits one compact JSON
// object per line (NDJSON), so `--follow` streams cleanly and an agent parses it
// line by line — and since it marshals the DTO whole, `agent_instance` and
// `agent_label` have been in the JSON output since they were added to MessageDTO
// (PRD #99 M1), with no work here. In human mode it prints a single
// #seq/kind/actor/payload line, where the actor cell is what M4 widens.
func renderMessage(p *uzicli.Printer, m apitypes.MessageDTO) error {
	if p.Format == uzicli.FormatJSON {
		b, err := json.Marshal(m)
		if err != nil {
			return err
		}
		p.Println(string(b))
		return nil
	}
	// %-*s pads to a fixed RUNE width (Go's fmt measures width in runes, not bytes),
	// and actorCell has already capped its result to that width, so the payload column
	// starts at the same rune offset on every line.
	p.Printf("#%-4d %-16s %-*s %s\n", m.Seq, m.Kind, actorCellWidth, actorCell(m), compactPayload(m.Payload))
	return nil
}

// actorCellWidth bounds the human table's actor column. The payload column beside
// it already runs to 200 characters, so terminal width is not the scarce resource
// this trades against — the cap exists to keep the columns ALIGNED down the stream,
// which is what `uzi run logs --follow` is read through. (Rune alignment, like the
// rest of this CLI's tables: a CJK label still occupies two terminal columns per
// rune, which no part of this tool accounts for — out of scope, not a regression.)
const actorCellWidth = 34

// actorCell renders WHO produced a message: the role, the short id of the
// invocation that produced it when the frame carries one, and that invocation's
// task label. Two parallel `coder` subagents therefore read distinctly (PRD #99
// Decision 10) instead of as two identical `coder` rows:
//
//	#12   tool_use   coder/3v6ptu · API wi…  {"name":"Edit"}
//	#13   tool_use   coder/2k9xqf · web ga…  {"name":"Edit"}
//
// The short id alone already separates them, so a truncated label costs clarity,
// never correctness. A frame with no instance — the orchestrator's own turns, infra
// frames, and every pre-migration message — renders as the bare role exactly as it
// did before, and a missing role still renders "-".
//
// Both new fields are worker-supplied text bound for a TTY, and `agent_label` is
// free model-authored prose rather than a role drawn from a fixed roster, so each
// goes through cellText's sanitize-and-fold (Risk 13: a raw CSI sequence in this
// cell could clear the screen or spoof output). That also closes the same hole for
// `agent`, which this function inherited printing verbatim.
func actorCell(m apitypes.MessageDTO) string {
	cell := "-"
	if m.Agent != nil {
		if role := cellText(*m.Agent); role != "" {
			cell = role
		}
	}
	if m.AgentInstance != nil {
		if id := cellText(shortInstanceID(*m.AgentInstance)); id != "" {
			cell += "/" + id
		}
	}
	if m.AgentLabel != nil {
		if label := cellText(*m.AgentLabel); label != "" {
			cell += " · " + label
		}
	}
	return capCell(cell, actorCellWidth)
}

// compactPayload renders a message payload as a single truncated line for the
// human table. The payload is server-forwarded run content; it is DATA, printed
// verbatim (never interpreted).
func compactPayload(raw json.RawMessage) string {
	return compactText(string(raw))
}

// renderRunInputs prints the steer queue (follow-ups and scope directives,
// newest-first) as a kind/body/state/age table (PRD #95 M4, #634). The body is the
// user's own text, sanitized like any free text bound for a TTY. State is derived per
// kind — from (consumed_at, runStatus) for a follow_up, from disposition for a scope
// directive; age is relative to created_at.
func renderRunInputs(p *uzicli.Printer, inputs []apitypes.SteerInputDTO, runStatus string) error {
	rows := make([][]string, 0, len(inputs))
	for _, in := range inputs {
		body := "-"
		if in.Body != nil {
			body = compactText(*in.Body)
		}
		rows = append(rows, []string{
			steerKindLabel(in.Kind),
			body,
			steerState(in.Kind, in.ConsumedAt, in.Disposition, runStatus),
			relAge(in.CreatedAt),
		})
	}
	return p.Table([]string{"KIND", "BODY", "STATE", "AGE"}, rows)
}

// steerKindLabel renders the KIND column: a scope directive shows "scope", everything
// else (a follow_up, or an empty kind on a degraded read) shows "follow-up".
func steerKindLabel(kind string) string {
	if kind == kindScope {
		return "scope"
	}
	return "follow-up"
}

// steerState derives a steer-queue row's state label. For a kind='scope' operator
// directive (PRD #634) the state IS its disposition (a scope row is never consumed):
// nil → "active (scope ceiling set)", applied/declined/superseded → their explained
// forms, any other value → the raw string. For a follow_up it derives the delivery
// label from its consumed_at and the run's live status, mirroring PRD #95 Decision 7
// as closely as the CLI can:
//   - not consumed, run terminal  → "not delivered (run finished)"
//   - not consumed, run parked    → "queued (run paused on a usage limit)"
//   - not consumed, otherwise     → "queued"
//   - consumed, run at plan gate  → "delivered (applies after approval)"
//   - consumed, awaiting follow-up → "delivered (resumes the run)"
//   - consumed, run parked        → "delivered (run paused on a usage limit)"
//   - consumed, otherwise         → "delivered"
//
// runStatus may be "" when the run's status could not be fetched (Decision 10
// floor): the gate/terminal nuance is then dropped and only queued/delivered
// show — the acceptable CLI minimum.
//
// The two statusLimitWait arms are a SUFFIX on the existing answer, never a
// replacement for it (PRD #35). A park changes nothing about whether a follow-up was
// handed to the worker; it changes only whether anything is currently acting on it.
// Rewriting "queued" to something else on a parked run would be the same lie as
// dropping the park entirely, one level down: the queue state is still queued.
func steerState(kind string, consumedAt *time.Time, disposition *string, runStatus string) string {
	// PRD #634: a scope directive's state IS its disposition — it is never consumed, so
	// consumed_at/runStatus carry no delivery signal for it. A nil disposition means the
	// ceiling is still pending (only reachable on a live run).
	if kind == kindScope {
		if disposition == nil {
			return "active (scope ceiling set)"
		}
		switch *disposition {
		case "applied":
			return "applied (finalized at the ceiling)"
		case "declined":
			return "declined (not acted on)"
		case "superseded":
			return "superseded (a later directive replaced it)"
		default:
			return *disposition
		}
	}
	// A parked/held run is deliberately NOT terminal here, matching terminalRunStatuses:
	// its queue survives the park and drains when the run resumes, so "not delivered
	// (run finished)" would be false.
	const parkedSuffix = " (run paused on a usage limit)"
	// PRD #754: a pool_wait run is HELD on an empty token pool, not a usage limit, so its
	// suffix names the actual reason (distinct copy for a distinct hold).
	const heldSuffix = " (run held on an empty token pool)"
	if consumedAt == nil {
		if terminalRunStatuses[runStatus] {
			return "not delivered (run finished)"
		}
		if runStatus == statusLimitWait {
			return "queued" + parkedSuffix
		}
		if runStatus == statusPoolWait {
			return "queued" + heldSuffix
		}
		return "queued"
	}
	if runStatus == "awaiting_approval" {
		return "delivered (applies after approval)"
	}
	if runStatus == "awaiting_input" {
		// PRD #88: the run is parked on a question, so a follow-up sits behind the
		// answer rather than behind an approval. Distinct wording because the action
		// the user owes is different, and telling them to approve something would be
		// simply wrong.
		return "delivered (applies after the question is answered)"
	}
	if runStatus == "awaiting_followup" {
		// PRD #517: the interactive task is parked awaiting the user's next follow-up.
		// Tailored copy mirroring the web twin (SteerQueueCard's "Delivered — resumes
		// the run") — a delivered follow-up here is what wakes the parked run.
		return "delivered (resumes the run)"
	}
	if runStatus == statusLimitWait {
		return "delivered" + parkedSuffix
	}
	if runStatus == statusPoolWait {
		return "delivered" + heldSuffix
	}
	return "delivered"
}

// limitWaitLine is the ONE sentence every CLI surface renders for a parked run
// (PRD #35): `uzi run get`'s detail block, the `--follow` notice and the TUI's run
// header all call this, so the terminal cannot give three readings of one park.
//
// It returns "" for a run that is not parked, which is what makes it safe to call
// unconditionally at each of those sites.
//
// The countdown is off RetryNotBefore, NOT LimitResetsAt, and the two routinely
// differ — RetryNotBefore carries jitter, is clamped to RUN_LIMIT_MAX_PARK, is
// cross-checked against the owner's gauge and is pool-aware, so a user with a second
// credential that still has headroom is promoted long BEFORE the window it hit
// reopens. RetryNotBefore is when work resumes; LimitResetsAt is context, and
// renderRunDetail prints it as its own row rather than folding it in here.
//
// RateLimitType is server-allowlisted (anything outside the SDK union is coerced to
// "unknown"), so it is an enum by the time it reaches here — and it still goes through
// cellText, for the same reason the STOP_KIND row does: "server-controlled today" is
// exactly the assumption that rots, and this string lands in a table cell where a
// newline breaks the rail. An UNRECOGNISED type prints as itself; the CLI is versioned
// separately from the API, so inventing a rendering for a member a newer server ships,
// or dropping it, would both be worse than passing it through.
func limitWaitLine(r apitypes.RunDTO, now time.Time) string {
	if r.Status != statusLimitWait {
		return ""
	}
	line := "paused: Anthropic usage limit"
	if t := cellText(strOr(r.RateLimitType, "")); t != "" {
		line += " (" + t + ")"
	}
	line += " · " + resumesIn(r.RetryNotBefore, now)
	// Attempt N, never "N/cap": the cap is one server constant and is deliberately not
	// on RunDTO, so a denominator here would have to be a second copy of it in a binary
	// that ships on its own release cadence. 0 is a run that has not parked, which this
	// function has already excluded — but the guard stays, because a server that
	// reports a park without a count should print no number rather than "attempt 0".
	if r.LimitWaitCount > 0 {
		line += " · attempt " + itoa(int(r.LimitWaitCount))
	}
	return line
}

// resumesIn renders the promotion clock for a parked run.
//
// A nil RetryNotBefore is a real case, not a defensive branch: an older server, or a
// park whose stamp failed to record. It must not render as "resumes in 0s" (a promise
// the CLI cannot keep) nor be silently omitted (the park then reads as an unexplained
// hang), so it says what is actually known.
//
// A stamp already in the past is the NORMAL steady state for a few seconds — the
// sweeper runs on a ticker, so there is always a window where the clock has passed and
// the promotion has not fired yet.
func resumesIn(retryNotBefore *time.Time, now time.Time) string {
	if retryNotBefore == nil {
		return "no resume time recorded"
	}
	d := retryNotBefore.Sub(now)
	if d <= 0 {
		return "resuming shortly"
	}
	return "resumes in " + fmtUntil(d)
}

// fmtUntil renders a forward-looking duration at TWO units, which is where it parts
// company with relAge's single unit next door.
//
// relAge answers "how old is this row" for a table column, where 3h is precise enough.
// This answers "when do I come back", over a range that runs from seconds to the
// seven-day window RUN_LIMIT_MAX_PARK clamps to — and across that range a single unit
// is unusable: a park with 3h59m left and one with 3h01m left both read "3h", which is
// an hour of error on the only number the user came for.
func fmtUntil(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd%dh", int(d.Hours())/24, int(d.Hours())%24)
	}
}
