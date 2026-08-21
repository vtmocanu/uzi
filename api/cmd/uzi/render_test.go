package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// TestMrAbbrev pins the forge-aware merge/pull-request label (PRD #65 D2, #238 D2),
// the CLI twin of the web's mrAbbrev and slacksvc's forgeMrAbbrev: Forgejo and GitHub
// "PR", everything else (GitLab, empty, unknown) "MR".
func TestMrAbbrev(t *testing.T) {
	for _, tc := range []struct {
		forge string
		want  string
	}{
		{"forgejo", "PR"},
		{"github", "PR"},
		{"gitlab", "MR"},
		{"", "MR"},
		{"something_else", "MR"},
	} {
		if got := mrAbbrev(tc.forge); got != tc.want {
			t.Errorf("mrAbbrev(%q) = %q, want %q", tc.forge, got, tc.want)
		}
	}
}

// TestRenderRunDetailForgeAwareMRColumn is the end-to-end proof that `uzi run get`
// labels the request column per the run's forge — a Forgejo run reads "PR", a
// GitLab run "MR", off r.ForgeType (the field the landing merge threads onto
// apitypes.RunDTO). This is the forge-blind "MR" hardcode the CLI had until #65.
func TestRenderRunDetailForgeAwareMRColumn(t *testing.T) {
	iid := int64(42)
	render := func(forge string) string {
		var buf bytes.Buffer
		p := uzicli.NewPrinter(&buf, false, false, true, false) // non-tty, non-json, no colour
		r := apitypes.RunDTO{
			ID:         "run-1",
			Kind:       "issue",
			Status:     "completed",
			IssueTitle: "do the thing",
			ForgeType:  forge,
			MrIID:      &iid,
			Health:     "ok",
		}
		if err := renderRunDetail(p, r); err != nil {
			t.Fatalf("renderRunDetail(%q): %v", forge, err)
		}
		return buf.String()
	}

	fj := render("forgejo")
	if !strings.Contains(fj, "PR") {
		t.Errorf("a Forgejo run must render a PR column, got:\n%s", fj)
	}
	// The GitLab label must not leak onto a Forgejo run's detail.
	if strings.Contains(fj, "MR ") || strings.Contains(fj, "MR\t") {
		t.Errorf("a Forgejo run must NOT render an MR column label, got:\n%s", fj)
	}

	gl := render("gitlab")
	if !strings.Contains(gl, "MR") {
		t.Errorf("a GitLab run must render an MR column, got:\n%s", gl)
	}
}

// TestRenderRunDetailAnthropicToken pins `uzi run get`'s ANTHROPIC_TOKEN row
// (PRD #111 M1) and, more importantly, the sanitization it goes through.
//
// The label is the first genuinely USER-AUTHORED string this block renders. This
// comment used to name two supporting facts and BOTH have since gone false, which is
// the more useful thing to record: "validateSecretLabel permits unicode.Cf" (PRD #111
// M2 made the validator reject it) and "uzicli.Printer.Table does not sanitize what it
// is handed" (#180 put CellText on every cell).
//
// The test is unchanged and is worth MORE, not less, for that. It drives the real
// render path and asserts on the rendered bytes, so it never depended on which layer
// did the stripping — it now pins two independent defences, and it would still redden
// if a refactor moved this row off Printer.Table and dropped cellText in the same
// stroke. A row rendered through the wrong helper looks identical until someone stores
// a hostile label, which is why this asserts the CONTENT and not the row's presence.
func TestRenderRunDetailAnthropicToken(t *testing.T) {
	render := func(label *string) string {
		t.Helper()
		var buf bytes.Buffer
		p := uzicli.NewPrinter(&buf, false, false, true, false) // non-tty, non-json, no colour
		r := apitypes.RunDTO{
			ID: "run-1", Kind: "issue", Status: "completed",
			IssueTitle: "do the thing", ForgeType: "gitlab", Health: "ok",
			AnthropicSecretLabel: label,
		}
		if err := renderRunDetail(p, r); err != nil {
			t.Fatalf("renderRunDetail: %v", err)
		}
		return buf.String()
	}

	label := "console-key"
	if out := render(&label); !strings.Contains(out, "ANTHROPIC_TOKEN") || !strings.Contains(out, "console-key") {
		t.Errorf("a run with a recorded credential must name it, got:\n%s", out)
	}

	// Absent and empty both render NO row: a run that cannot say which account it
	// billed must not appear to have said something. Empty is unreachable through the
	// API (user_secrets.label is NOT NULL with a 1..64 CHECK) and is pinned anyway,
	// because the guard has to be emptiness-based rather than a nil check.
	if out := render(nil); strings.Contains(out, "ANTHROPIC_TOKEN") {
		t.Errorf("a run with no recorded credential must render no row, got:\n%s", out)
	}
	empty := ""
	if out := render(&empty); strings.Contains(out, "ANTHROPIC_TOKEN") {
		t.Errorf("an empty label must render no row, got:\n%s", out)
	}

	// A bidi override (U+202E RIGHT-TO-LEFT OVERRIDE) plus a CSI escape and a newline:
	// the escape would repaint the terminal, the override would visually reverse the
	// label so it READS as a different account, and the newline would break the table
	// rail. All three must be gone, and the printable text must survive.
	//
	// 🔴 THIS FIXTURE IS DELIBERATELY UN-STORABLE THROUGH THE API, AND THE TEST STILL
	// EARNS ITS KEEP. Since PRD #111 M2, validateSecretLabel (handler/secrets.go)
	// rejects both unicode.IsControl and unicode.Cf, so no label containing these bytes
	// can be written through any handler today. Do NOT "tidy" this into a reachable
	// fixture, and do NOT drop the cellText routing it pins:
	//
	//   - The two live on opposite sides of a trust boundary. The validator is what the
	//     SERVER accepts; this is what the RENDERER does with whatever it is handed.
	//     Depending on the far side of a boundary for local safety is exactly the
	//     coupling that turns one regression into two.
	//   - Three routes reach the renderer without passing that validator: a label
	//     stored before M2 landed (existing rows are not re-validated), a future write
	//     path that forgets the check, and a row written straight to the database.
	//
	// 🔴 THE NEWLINE PROBE IS THE ONLY ONE THAT DISCRIMINATES, AND IT WAS WRONG.
	// It read `"\n\nnext-line"` — a DOUBLE newline neither implementation ever emits —
	// so it was satisfied by construction. The bidi and ESC probes do not discriminate
	// either: sanitizeTTY and cellText both strip unicode.Cf and unicode.IsControl, so
	// two of the three agree under either helper. Measured: swapping cellText for
	// sanitizeTTY left this test GREEN.
	//
	// Newline FOLDING is the whole difference between them (cellText → compactText
	// replaces "\n" with a space; sanitizeTTY deliberately spares "\n"), so a single
	// "\nnext-line" is what tells them apart, and it is what is asserted now.
	//
	// The class this belongs to is worth naming: the broken implementation and the
	// correct one AGREE on the case that was picked, so the fixture passed against
	// both and read as proof of something it never tested.
	hostile := "safe\u202ednetsop\x1b[31m\nnext-line"
	out := render(&hostile)
	// The first two pin the shared floor (either helper satisfies them); the third is
	// the discriminating one.
	for _, bad := range []string{"\u202e", "\x1b", "\nnext-line"} {
		if strings.Contains(out, bad) {
			t.Errorf("hostile label reached the terminal carrying %q, got:\n%q", bad, out)
		}
	}
	if !strings.Contains(out, "safe") || !strings.Contains(out, "next-line") {
		t.Errorf("sanitizing dropped the printable text too, got:\n%q", out)
	}
}

// TestRenderRunDetailReportOnly pins `uzi run get`'s report-only surfaces (issue #279):
// the REPORT_ONLY row that explains the empty MR column, and the scrubbed report_md
// printed below the table on its own lines.
//
// report_md is UNTRUSTED worker/model text, so it goes through sanitizeTTY exactly like
// the judge summary — and DELIBERATELY not through cellText: it is multi-line findings
// printed off the table rail, so the newlines must SURVIVE (a table cell would fold
// them). That is the discriminating assertion here, the mirror image of the credential
// row's: sanitizeTTY strips the escape and the bidi override but SPARES "\n".
func TestRenderRunDetailReportOnly(t *testing.T) {
	// An escape (would repaint the terminal) and a bidi override (would visually reorder
	// the text) must be stripped; the two content lines and the newline between them must
	// survive, because the findings are rendered as multi-line prose, not a table cell.
	md := "safe findings\nsecond line\x1b\u202e"
	reportRun := apitypes.RunDTO{
		ID: "run-1", Kind: "issue", Status: "completed",
		IssueTitle: "audit the queries", ForgeType: "gitlab", Health: "ok",
		ReportOnly: true, ReportMd: &md,
	}
	out := renderDetail(t, reportRun)

	// The row that explains why the MR column reads "-".
	if !strings.Contains(out, "REPORT_ONLY") || !strings.Contains(out, "yes") {
		t.Errorf("a report-only run must render a REPORT_ONLY yes row, got:\n%s", out)
	}
	// The findings block: its label plus the scrubbed text.
	if !strings.Contains(out, "findings:") {
		t.Errorf("a report-only run must label its findings block, got:\n%s", out)
	}
	// sanitizeTTY strips the escape and the bidi override …
	for _, bad := range []string{"\x1b", "\u202e"} {
		if strings.Contains(out, bad) {
			t.Errorf("hostile report_md reached the terminal carrying %q, got:\n%q", bad, out)
		}
	}
	// … and SPARES the newline, so the two lines survive as distinct lines. This is the
	// probe that proves report_md is rendered off the table rail (sanitizeTTY), not through
	// cellText, which would fold "\n" to a space.
	if !strings.Contains(out, "safe findings\nsecond line") {
		t.Errorf("the multi-line findings were folded or dropped, got:\n%q", out)
	}

	// A normal completion renders neither the row nor the findings block.
	normal := apitypes.RunDTO{ID: "run-2", Kind: "issue", Status: "completed", Health: "ok"}
	if out := renderDetail(t, normal); strings.Contains(out, "REPORT_ONLY") || strings.Contains(out, "findings:") {
		t.Errorf("a normal completion must render no report-only surfaces, got:\n%s", out)
	}

	// A report-only run whose summary is empty renders the row but no empty findings block
	// — the same guard the judge summary carries (an empty SummaryMd prints nothing).
	empty := ""
	rowOnly := apitypes.RunDTO{
		ID: "run-3", Kind: "issue", Status: "completed", Health: "ok",
		ReportOnly: true, ReportMd: &empty,
	}
	if out := renderDetail(t, rowOnly); !strings.Contains(out, "REPORT_ONLY") || strings.Contains(out, "findings:") {
		t.Errorf("a report-only run with an empty summary must render the row but no findings block, got:\n%s", out)
	}
}

// TestRenderRunDetailPrdLifecycle pins `uzi run get`'s PRD-link lifecycle rows (#150):
// PRD_MOVE (the worker-declared path the run moved a PRD to) and PRD_PATCH_SETTLED_AT
// (the server timestamp for when the link-patch lifecycle settled). Both are
// emit-only-when-set, so a run that moved no PRD — or one predating the feature —
// renders neither, the same back-compat contract the nil pointers carry.
//
// PRD_MOVE is worker-authored text, so it goes through sanitizeTTY: a bidi override
// and a CSI escape must be stripped while the printable path survives. The timestamp
// is rendered in UTC RFC3339, mirroring LIMIT_RESETS_AT.
func TestRenderRunDetailPrdLifecycle(t *testing.T) {
	settled := time.Date(2026, 8, 16, 9, 30, 0, 0, time.UTC)
	path := "prds/done/150-run-dto.md"
	set := apitypes.RunDTO{
		ID: "run-1", Kind: "issue", Status: "completed",
		IssueTitle: "move the PRD", ForgeType: "gitlab", Health: "ok",
		PrdDonePath: &path, PrdPatchSettledAt: &settled,
	}
	out := renderDetail(t, set)
	for _, want := range []string{"PRD_MOVE", "prds/done/150-run-dto.md", "PRD_PATCH_SETTLED_AT", "2026-08-16T09:30:00Z"} {
		if !strings.Contains(out, want) {
			t.Errorf("a run that settled its PRD link must render %q, got:\n%s", want, out)
		}
	}

	// PRD_MOVE is worker-declared, so a hostile path must be neutralised: the escape and
	// the bidi override are stripped, the printable path survives.
	hostile := "prds/done/x\u202e\x1b[31m.md"
	hostileRun := set
	hostileRun.PrdDonePath = &hostile
	hout := renderDetail(t, hostileRun)
	for _, bad := range []string{"\u202e", "\x1b"} {
		if strings.Contains(hout, bad) {
			t.Errorf("a hostile prd_done_path reached the terminal carrying %q, got:\n%q", bad, hout)
		}
	}
	if !strings.Contains(hout, "prds/done/x") || !strings.Contains(hout, ".md") {
		t.Errorf("sanitizing dropped the printable path too, got:\n%q", hout)
	}

	// Both nil ⇒ neither row, byte-for-byte the pre-#150 output.
	none := apitypes.RunDTO{ID: "run-2", Kind: "issue", Status: "completed", Health: "ok"}
	nout := renderDetail(t, none)
	for _, bad := range []string{"PRD_MOVE", "PRD_PATCH_SETTLED_AT"} {
		if strings.Contains(nout, bad) {
			t.Errorf("a run that moved no PRD must render no %q row, got:\n%s", bad, nout)
		}
	}
}

// ---- PRD #122 M5: milestone progress + effective budget --------------------

// milestoneRun is the fixture the milestone tests start from: a three-milestone frozen
// list with the first reported complete, the second in progress and the third untouched,
// plus a scaled effective budget. Built by a helper so a test mutating one field cannot
// silently change another's input.
func milestoneRun() apitypes.RunDTO {
	iters, wall := 18, 5400 // 5400s = 1h30m
	return apitypes.RunDTO{
		ID: "run-1", Kind: "issue", Status: "running",
		IssueTitle: "do the thing", ForgeType: "gitlab", Health: "ok",
		Milestones: []apitypes.Milestone{
			{ID: "m1", Title: "Set up the schema"},
			{ID: "m2", Title: "Wire the API"},
			{ID: "m3", Title: "Write the docs"},
		},
		MilestonesCompleted:  []string{"m1"},
		MilestonesInProgress: []string{"m2"},
		BudgetMaxIterations:  &iters,
		BudgetWallSeconds:    &wall,
	}
}

func renderDetail(t *testing.T, r apitypes.RunDTO) string {
	t.Helper()
	var buf bytes.Buffer
	p := uzicli.NewPrinter(&buf, false, false, true, false) // non-tty, non-json, no colour
	if err := renderRunDetail(p, r); err != nil {
		t.Fatalf("renderRunDetail: %v", err)
	}
	return buf.String()
}

// TestRenderRunDetailMilestones pins `uzi run get`'s milestone block: the
// "reported complete" summary, the per-milestone state breakdown (done / in progress /
// left), and the effective-budget rows — plus the back-compat guard that a run with no
// frozen milestones and a global-default budget renders NONE of it.
func TestRenderRunDetailMilestones(t *testing.T) {
	out := renderDetail(t, milestoneRun())

	// The summary counts only frozen members reported complete — 1 of 3 here — and says
	// "reported complete", the wording PRD Decision 6 requires.
	if !strings.Contains(out, "MILESTONES") || !strings.Contains(out, "1/3 reported complete") {
		t.Errorf("a milestone run must summarise its progress as 1/3 reported complete, got:\n%s", out)
	}
	// 🔴 NEVER "verified" — a green milestone is self-reported, not verified (Decision 6).
	if strings.Contains(out, "verified") {
		t.Errorf("the milestone summary must not claim verification, got:\n%s", out)
	}

	// The per-milestone breakdown carries the same state the web shows: one row each,
	// marked done / in progress / left, with the title.
	for _, want := range []string{"done", "in progress", "left", "Set up the schema", "Wire the API", "Write the docs"} {
		if !strings.Contains(out, want) {
			t.Errorf("the milestone breakdown is missing %q, got:\n%s", want, out)
		}
	}

	// The effective budget, present because this run was scaled off its milestone count.
	for _, want := range []string{"BUDGET_ITERATIONS", "18", "BUDGET_WALL", "1h30m"} {
		if !strings.Contains(out, want) {
			t.Errorf("a scaled run must render its effective budget, missing %q, got:\n%s", want, out)
		}
	}

	// 🔴 A dropped-but-still-completed id must NOT inflate the count past the frozen
	// total. milestones_completed is a monotone union, so it can name a milestone no
	// longer in the approved list; the summary clamps to frozen membership.
	dropped := milestoneRun()
	dropped.MilestonesCompleted = []string{"m1", "m2", "m3", "ghost"}
	if out := renderDetail(t, dropped); !strings.Contains(out, "3/3 reported complete") {
		t.Errorf("a completed id outside the frozen list inflated the count past the total, got:\n%s", out)
	}

	// The back-compat guard: a run with no frozen milestones and a global-default budget
	// renders none of these rows — byte-for-byte the pre-#122 output.
	none := apitypes.RunDTO{ID: "run-2", Kind: "issue", Status: "completed", Health: "ok"}
	nout := renderDetail(t, none)
	for _, bad := range []string{"MILESTONES", "reported complete", "BUDGET_ITERATIONS", "BUDGET_WALL"} {
		if strings.Contains(nout, bad) {
			t.Errorf("a run with no milestones must render no %q row, got:\n%s", bad, nout)
		}
	}
}

// TestRenderRunDetailMilestonesNeutral pins PRD #390 D5's null-vs-`[]` display contract
// on `uzi run get`: a milestone run that NEVER reported progress (MilestonesCompleted nil ⇒
// the milestones_completed column is SQL NULL) must render a NEUTRAL summary with an en-dash
// numerator (`–/N`, matching the web badge's `M–/N`), NOT `0/N` (which reads as a failure).
// A run that GENUINELY reported zero complete (non-nil empty `[]`) still shows `0/N`. The
// distinction is nil vs empty, so it is exactly what a `len()==0` test would destroy.
func TestRenderRunDetailMilestonesNeutral(t *testing.T) {
	// Never reported: nil ⇒ neutral en-dash numerator, and NEVER the `0/N` failure read.
	neverReported := milestoneRun()
	neverReported.MilestonesCompleted = nil
	neverReported.MilestonesInProgress = nil
	out := renderDetail(t, neverReported)
	if !strings.Contains(out, "–/3 reported complete") {
		t.Errorf("a never-reported milestone run must render the neutral –/3, got:\n%s", out)
	}
	if strings.Contains(out, "0/3 reported complete") {
		t.Errorf("a never-reported milestone run must NOT render 0/3 (reads as failure), got:\n%s", out)
	}

	// Genuinely reported zero: non-nil empty `[]` ⇒ 0/N, distinct from never-reported.
	reportedZero := milestoneRun()
	reportedZero.MilestonesCompleted = []string{}
	reportedZero.MilestonesInProgress = nil
	out = renderDetail(t, reportedZero)
	if !strings.Contains(out, "0/3 reported complete") {
		t.Errorf("a genuinely-reported zero-complete run must render 0/3, got:\n%s", out)
	}
	if strings.Contains(out, "–/3") {
		t.Errorf("a reported (non-nil) run must NOT render the neutral en-dash, got:\n%s", out)
	}

	// The existing reported case (the milestoneRun default) still shows 1/N.
	out = renderDetail(t, milestoneRun())
	if !strings.Contains(out, "1/3 reported complete") {
		t.Errorf("a run that reported m1 complete must render 1/3, got:\n%s", out)
	}
}

// TestRenderRunDetailMilestoneTitleSanitized is the milestone twin of
// TestRenderRunDetailAnthropicToken: a milestone TITLE is UNTRUSTED repo/agent-authored
// text (apitypes.Milestone), so it must go through cellText — bidi override and CSI escape
// stripped, newline folded so the table rail holds, and an oversized title capped.
//
// The newline is the discriminating probe (sanitizeTTY spares "\n"; only cellText folds
// it), the tab the second (sanitizeTTY spares "\t" too), and the length cap the third
// (sanitizeTTY has no bound at all) — the same three that pin the credential row.
func TestRenderRunDetailMilestoneTitleSanitized(t *testing.T) {
	r := milestoneRun()
	hostile := "safe\u202ednetsop\x1b[31m\nnext-line\tcol" + strings.Repeat("x", 250)
	r.Milestones = []apitypes.Milestone{{ID: "m1", Title: hostile}}
	r.MilestonesCompleted = []string{"m1"}
	r.MilestonesInProgress = nil

	out := renderDetail(t, r)
	for _, bad := range []string{"\u202e", "\x1b", "\nnext-line", "\t"} {
		if strings.Contains(out, bad) {
			t.Errorf("a hostile milestone title reached the terminal carrying %q, got:\n%q", bad, out)
		}
	}
	if !strings.Contains(out, "safe") || !strings.Contains(out, "next-line") {
		t.Errorf("sanitizing dropped the printable title text too, got:\n%q", out)
	}
	// The cap: the 250-char probe must be truncated and ellipsised, not passed through
	// whole — an uncapped title blows the table rail.
	if strings.Contains(out, strings.Repeat("x", 250)) {
		t.Errorf("a 250-char milestone title reached the terminal uncapped, got:\n%q", out)
	}
	if !strings.Contains(out, "…") {
		t.Errorf("an oversized milestone title was neither truncated nor ellipsised, got:\n%q", out)
	}
}

// ---- PRD #362 M5: plain-English run summaries -------------------------------

// TestRenderRunDetailSummaries pins `uzi run get`'s intent/plan/deltas block (PRD #362
// M5): the INTENT row, the PLAN SUMMARY row, and one DELTA row per entry rendered as
// `<glyph> <kind>: <text>`. All three are emit-only-when-set — a run with no summaries
// (the common pre-feature / still-queued case) must render none of them and leave the
// existing rows untouched.
func TestRenderRunDetailSummaries(t *testing.T) {
	intent := "Add rate-limit headroom to the scheduler poll."
	plan := "Introduce a token-bucket guard and back off on 429s."
	full := apitypes.RunDTO{
		ID: "run-1", Kind: "issue", Status: "awaiting_approval",
		IssueTitle: "do the thing", ForgeType: "gitlab", Health: "ok",
		SummaryIntent: &intent,
		SummaryPlan:   &plan,
		SummaryDeltas: []apitypes.RunSummaryDelta{
			{Kind: "added", Text: "a retry budget"},
			{Kind: "changed", Text: "the poll cadence"},
			{Kind: "dropped", Text: "the eager prefetch"},
		},
	}
	out := renderDetail(t, full)

	for _, want := range []string{
		"INTENT", intent,
		"PLAN SUMMARY", plan,
		"DELTA",
		"+ added: a retry budget",
		"~ changed: the poll cadence",
		"- dropped: the eager prefetch",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("a run with summaries must render %q, got:\n%s", want, out)
		}
	}

	// A run with no summaries renders none of the rows — the back-compat contract the nil
	// pointers and nil slice carry, so a pre-feature run's detail is unchanged.
	none := apitypes.RunDTO{
		ID: "run-2", Kind: "issue", Status: "running",
		IssueTitle: "do the thing", ForgeType: "gitlab", Health: "ok",
	}
	bare := renderDetail(t, none)
	for _, unwanted := range []string{"INTENT", "PLAN SUMMARY", "DELTA"} {
		if strings.Contains(bare, unwanted) {
			t.Errorf("a run with no summaries must not render %q, got:\n%s", unwanted, bare)
		}
	}

	// Empty (non-nil) strings and a delta whose text is blank OR sanitizes to empty are
	// the same as absent: no bare rows, no `+ added:` with nothing after it. The second
	// delta's text is a lone bidi override — non-whitespace, so it must be dropped on the
	// SANITIZED text (cellText), not on raw TrimSpace which would leak a bare-label row.
	// Empty is reachable through the API as `""`.
	empty := ""
	edge := apitypes.RunDTO{
		ID: "run-3", Kind: "issue", Status: "awaiting_approval",
		IssueTitle: "do the thing", ForgeType: "gitlab", Health: "ok",
		SummaryIntent: &empty,
		SummaryPlan:   &empty,
		SummaryDeltas: []apitypes.RunSummaryDelta{
			{Kind: "added", Text: "   "},
			{Kind: "changed", Text: "\u202e"},
		},
	}
	edgeOut := renderDetail(t, edge)
	for _, unwanted := range []string{"INTENT", "PLAN SUMMARY", "DELTA"} {
		if strings.Contains(edgeOut, unwanted) {
			t.Errorf("empty summaries must render no %q row, got:\n%s", unwanted, edgeOut)
		}
	}
}

// TestRenderRunDetailSummariesSanitized is the summary twin of
// TestRenderRunDetailMilestoneTitleSanitized: intent, plan and delta text are all
// model-authored UNTRUSTED strings (Decision 10), so each must go through cellText —
// bidi override and CSI escape stripped, newline folded so the table rail holds, tab
// folded, and an oversized value capped. The newline, tab and cap are the discriminating
// probes (sanitizeTTY spares "\n" and "\t" and has no bound); a plain sanitizeTTY here
// would leave all three and break the rail.
func TestRenderRunDetailSummariesSanitized(t *testing.T) {
	hostile := "safe\u202ednetsop\x1b[31m\nnext-line\tcol" + strings.Repeat("x", 250)
	r := apitypes.RunDTO{
		ID: "run-1", Kind: "issue", Status: "awaiting_approval",
		IssueTitle: "do the thing", ForgeType: "gitlab", Health: "ok",
		SummaryIntent: &hostile,
		SummaryPlan:   &hostile,
		SummaryDeltas: []apitypes.RunSummaryDelta{{Kind: "added", Text: hostile}},
	}
	out := renderDetail(t, r)

	for _, bad := range []string{"\u202e", "\x1b", "\nnext-line", "\t"} {
		if strings.Contains(out, bad) {
			t.Errorf("hostile summary text reached the terminal carrying %q, got:\n%q", bad, out)
		}
	}
	if !strings.Contains(out, "safe") || !strings.Contains(out, "next-line") {
		t.Errorf("sanitizing dropped the printable summary text too, got:\n%q", out)
	}
	// The cap: cellText's alone (Printer.Table's per-cell pass folds newlines but does not
	// bound length), so an uncapped value would blow the table rail.
	if strings.Contains(out, strings.Repeat("x", 250)) {
		t.Errorf("a 250-char summary value reached the terminal uncapped, got:\n%q", out)
	}
	if !strings.Contains(out, "…") {
		t.Errorf("an oversized summary value was neither truncated nor ellipsised, got:\n%q", out)
	}
}

// TestDeltaGlyph pins the per-kind delta prefix, including the unknown-kind pass-through:
// a newer server can send a delta kind this binary has never heard of, and it must render
// a neutral bullet rather than being dropped.
func TestDeltaGlyph(t *testing.T) {
	for _, tc := range []struct{ kind, want string }{
		{"added", "+"},
		{"changed", "~"},
		{"dropped", "-"},
		{"resurrected", "•"},
		{"", "•"},
	} {
		if got := deltaGlyph(tc.kind); got != tc.want {
			t.Errorf("deltaGlyph(%q) = %q, want %q", tc.kind, got, tc.want)
		}
	}
}

// ---- PRD #35: the usage-limit park -----------------------------------------

// parkedRun is the fixture every test below starts from: a run parked on a five-hour
// window with a promotion stamped an hour out. Built by a helper rather than shared as
// a package var so a test that mutates one field cannot silently change another's
// input.
func parkedRun(now time.Time) apitypes.RunDTO {
	rlt := "five_hour"
	retry := now.Add(time.Hour)
	resets := now.Add(90 * time.Minute)
	return apitypes.RunDTO{
		ID: "run-1", Kind: "issue", Status: statusLimitWait,
		IssueTitle: "do the thing", ForgeType: "gitlab", Health: "ok",
		WaitOnLimit: true, RateLimitType: &rlt,
		RetryNotBefore: &retry, LimitResetsAt: &resets, LimitWaitCount: 2,
	}
}

// TestLimitWaitLine pins the ONE sentence every CLI surface renders for a park.
func TestLimitWaitLine(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)

	got := limitWaitLine(parkedRun(now), now)
	for _, want := range []string{"paused", "five_hour", "resumes in 1h00m", "attempt 2"} {
		if !strings.Contains(got, want) {
			t.Errorf("limitWaitLine = %q, want it to contain %q", got, want)
		}
	}

	// 🔴 THE COUNTDOWN IS OFF RetryNotBefore, NOT LimitResetsAt, and this fixture is
	// built so the two disagree (60m vs 90m) precisely so the wrong field cannot pass.
	// They differ in normal operation, not in a corner case: RetryNotBefore carries
	// jitter, is clamped, and is pool-aware — a user with a second credential that
	// still has headroom is promoted long before the window they hit reopens. Reading
	// LimitResetsAt would tell that user to wait half an hour longer than they must.
	if strings.Contains(got, "1h30m") {
		t.Errorf("limitWaitLine = %q — the countdown is being computed from limit_resets_at; it must come from retry_not_before, which is when the server actually promotes the run", got)
	}

	// Not parked ⇒ empty, which is what makes the call safe to make unconditionally at
	// every surface that renders it.
	for _, status := range []string{"running", "queued", "completed", "failed", "cancelled", "awaiting_approval"} {
		r := parkedRun(now)
		r.Status = status
		if line := limitWaitLine(r, now); line != "" {
			t.Errorf("limitWaitLine(status=%q) = %q, want \"\" — only a parked run has a park to describe", status, line)
		}
	}

	// A missing stamp is a real case (an older server, or a park whose stamp failed to
	// record), and it must neither promise a time nor go silent.
	noStamp := parkedRun(now)
	noStamp.RetryNotBefore = nil
	if line := limitWaitLine(noStamp, now); !strings.Contains(line, "no resume time recorded") {
		t.Errorf("limitWaitLine(no retry_not_before) = %q, want it to say the resume time is unknown rather than imply one", line)
	}

	// A stamp already past is the NORMAL steady state for a few seconds — the sweeper
	// runs on a ticker — so it must read as imminent, never as a negative countdown.
	past := parkedRun(now)
	elapsed := now.Add(-30 * time.Second)
	past.RetryNotBefore = &elapsed
	if line := limitWaitLine(past, now); !strings.Contains(line, "resuming shortly") {
		t.Errorf("limitWaitLine(retry_not_before in the past) = %q, want \"resuming shortly\"", line)
	}

	// No count ⇒ no "attempt" clause at all, rather than "attempt 0".
	if line := limitWaitLine(func() apitypes.RunDTO { r := parkedRun(now); r.LimitWaitCount = 0; return r }(), now); strings.Contains(line, "attempt") {
		t.Errorf("limitWaitLine(count=0) = %q, want no attempt clause", line)
	}

	// An UNRECOGNISED type prints as itself. The server allowlists this field, but the
	// CLI ships on its own cadence, so a newer server can send a member this binary has
	// never heard of — and dropping it, or inventing a rendering, would both be worse
	// than passing it through. Same stance as credentialCell's unknown reason.
	novel := parkedRun(now)
	future := "seven_day_opus_max"
	novel.RateLimitType = &future
	if line := limitWaitLine(novel, now); !strings.Contains(line, future) {
		t.Errorf("limitWaitLine(unknown rate_limit_type) = %q, want it to render %q verbatim", line, future)
	}
}

// TestLimitWaitLineSanitizesTheRateLimitType is the Risk 13 half.
//
// rate_limit_type is server-allowlisted today, which is exactly why this test states
// what it is defending: the renderer's safety must not depend on the far side of a
// trust boundary. The same three routes that reach credentialCell without passing a
// validator reach here — a row written before the allowlist existed, a future write
// path that skips it, and a direct database write.
//
// The NEWLINE is the discriminating probe, for the reason the ANTHROPIC_TOKEN test
// records at length: sanitizeTTY and cellText both strip Cf and control characters, so
// the bidi and ESC probes are satisfied by either helper and prove nothing about which
// one is wired. Only cellText folds "\n", and only cellText keeps the table rail intact.
func TestLimitWaitLineSanitizesTheRateLimitType(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	r := parkedRun(now)
	hostile := "five\u202eruoh_\x1b[31m\nnext-line"
	r.RateLimitType = &hostile

	got := limitWaitLine(r, now)
	for _, bad := range []string{"\u202e", "\x1b", "\nnext-line"} {
		if strings.Contains(got, bad) {
			t.Errorf("a hostile rate_limit_type reached the terminal carrying %q, got:\n%q", bad, got)
		}
	}
	if !strings.Contains(got, "five") || !strings.Contains(got, "next-line") {
		t.Errorf("sanitizing dropped the printable text too, got:\n%q", got)
	}

	// TAB — the second discriminator. sanitizeTTY spares "\t" DELIBERATELY (a tab is
	// ordinary content in the free-text columns it also serves); only cellText folds it
	// to a space. A tab survives into a tabwriter cell and walks the whole column right,
	// with the rune offset pinned throughout — which is why the table's own alignment
	// assertions cannot see it.
	tabbed := parkedRun(now)
	withTab := "five\thour"
	tabbed.RateLimitType = &withTab
	if line := limitWaitLine(tabbed, now); strings.Contains(line, "\t") {
		t.Errorf("a tab in rate_limit_type reached a table cell: %q — sanitizeTTY spares tab, so this is the probe that proves cellText is wired", line)
	}

	// THE LENGTH CAP — the third, and the one with the most reach. sanitizeTTY has no
	// length bound at all; cellText inherits compactText's 200-char cap. This is what
	// stops an oversized value blowing the table rail.
	//
	// By this test's own stated standard, the DB CHECK is not the answer here: a
	// constraint forbidding an oversized value is a guarantee made in ANOTHER package,
	// and rate_limit_type carries no CHECK at all by design (the vocabulary is the SDK's,
	// so 00091 deliberately left it open and put the guard in Go). The renderer must
	// bound what it is handed.
	oversized := parkedRun(now)
	long := strings.Repeat("x", 250)
	oversized.RateLimitType = &long
	line := limitWaitLine(oversized, now)
	if strings.Contains(line, long) {
		t.Errorf("a 250-char rate_limit_type reached the terminal uncapped (line is %d bytes) — sanitizeTTY has no length bound, so this probe is what proves the cap is applied", len(line))
	}
	if !strings.Contains(line, "…") {
		t.Errorf("an oversized rate_limit_type was neither truncated nor ellipsised: %q", line)
	}
}

// TestFmtUntil pins the TWO-unit rendering, which is where this parts company with
// relAge's single unit next door.
func TestFmtUntil(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{5 * time.Second, "5s"},
		{59 * time.Second, "59s"},
		{time.Minute, "1m"},
		{45 * time.Minute, "45m"},
		{time.Hour, "1h00m"},
		// The pair that justifies two units at all: at one unit both of these read "3h",
		// an hour of error on the only number the user came for.
		{3*time.Hour + 1*time.Minute, "3h01m"},
		{3*time.Hour + 59*time.Minute, "3h59m"},
		{25 * time.Hour, "1d1h"},
		{7 * 24 * time.Hour, "7d0h"},
	} {
		if got := fmtUntil(tc.d); got != tc.want {
			t.Errorf("fmtUntil(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

// TestRunAgeCell pins `uzi run list`'s AGE column (issue #256 M5): the per-state anchor
// (Decision 2) and that terminal is a STATIC ran-span independent of now. now is fixed so
// every live bucket is deterministic; the buckets themselves are formatUptimeDuration's,
// covered next door — this test proves only that the RIGHT anchor feeds them.
func TestRunAgeCell(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) *time.Time { t := now.Add(-d); return &t }

	for _, tc := range []struct {
		name string
		r    apitypes.RunDTO
		want string
	}{
		{
			name: "running off StartedAt",
			r:    apitypes.RunDTO{Status: "running", StartedAt: ago(90 * time.Minute), CreatedAt: now.Add(-3 * time.Hour)},
			want: "1h 30m",
		},
		{
			// StartedAt unstamped ⇒ fall back to CreatedAt, not "-".
			name: "running falls back to CreatedAt",
			r:    apitypes.RunDTO{Status: "running", CreatedAt: now.Add(-45 * time.Minute)},
			want: "45m",
		},
		{
			name: "claimed off ClaimedAt",
			r:    apitypes.RunDTO{Status: "claimed", ClaimedAt: ago(2 * time.Minute), CreatedAt: now.Add(-2 * time.Hour)},
			want: "2m",
		},
		{
			name: "queued off CreatedAt",
			r:    apitypes.RunDTO{Status: "queued", CreatedAt: now.Add(-3 * time.Minute)},
			want: "3m",
		},
		{
			// awaiting_approval / awaiting_input / limit_wait all anchor on UpdatedAt.
			name: "awaiting_approval off UpdatedAt",
			r:    apitypes.RunDTO{Status: "awaiting_approval", UpdatedAt: now.Add(-25 * time.Hour), CreatedAt: now.Add(-30 * time.Hour)},
			want: "1d 1h",
		},
		{
			name: "limit_wait off UpdatedAt",
			r:    apitypes.RunDTO{Status: statusLimitWait, UpdatedAt: now.Add(-10 * time.Minute)},
			want: "10m",
		},
		{
			// awaiting_followup (PRD #517) is a park like the other two — it anchors on
			// UpdatedAt and renders a waiting duration, NOT "-" (the CLI twin of the web's
			// runDuration, which added awaiting_followup too).
			name: "awaiting_followup off UpdatedAt",
			r:    apitypes.RunDTO{Status: "awaiting_followup", UpdatedAt: now.Add(-20 * time.Minute), CreatedAt: now.Add(-30 * time.Minute)},
			want: "20m",
		},
		{
			// Terminal is a static span FinishedAt−StartedAt, so a now far from either end
			// does not change it: this run ran for 2h whenever it is listed.
			name: "completed is a static ran-span",
			r:    apitypes.RunDTO{Status: "completed", StartedAt: ago(5 * time.Hour), FinishedAt: ago(3 * time.Hour), CreatedAt: now.Add(-6 * time.Hour)},
			want: "2h 0m",
		},
		{
			// Cancelled/failed before it ever started ⇒ never ran ⇒ "-".
			name: "terminal with no StartedAt renders dash",
			r:    apitypes.RunDTO{Status: "cancelled", FinishedAt: ago(1 * time.Hour), CreatedAt: now.Add(-2 * time.Hour)},
			want: "-",
		},
		{
			// Clock skew: an anchor in the future floors to "<1m", never a negative age.
			name: "future anchor floors to <1m",
			r:    apitypes.RunDTO{Status: "running", StartedAt: ago(-30 * time.Second)},
			want: "<1m",
		},
		{
			name: "unknown status renders dash",
			r:    apitypes.RunDTO{Status: "teleported", CreatedAt: now.Add(-time.Hour)},
			want: "-",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := runAgeCell(tc.r, now); got != tc.want {
				t.Errorf("runAgeCell(%s) = %q, want %q", tc.name, got, tc.want)
			}
		})
	}

	// Terminal span independence from now, asserted directly: two very different clocks,
	// same rendered span.
	ran := apitypes.RunDTO{Status: "completed", StartedAt: ago(5 * time.Hour), FinishedAt: ago(3 * time.Hour)}
	if a, b := runAgeCell(ran, now), runAgeCell(ran, now.Add(100*time.Hour)); a != b {
		t.Errorf("terminal ran-span moved with now: %q vs %q", a, b)
	}
}

// TestRenderRunDetailLimitWait pins `uzi run get`'s park block — and specifically the
// distinction the block exists to make, since the server leaves the same three columns
// populated after a promotion as history.
func TestRenderRunDetailLimitWait(t *testing.T) {
	now := time.Now()
	render := func(r apitypes.RunDTO) string {
		t.Helper()
		var buf bytes.Buffer
		p := uzicli.NewPrinter(&buf, false, false, true, false) // non-tty, non-json, no colour
		if err := renderRunDetail(p, r); err != nil {
			t.Fatalf("renderRunDetail: %v", err)
		}
		return buf.String()
	}

	parked := render(parkedRun(now))
	for _, want := range []string{"LIMIT_WAIT", "five_hour", "resumes in", "attempt 2", "LIMIT_RESETS_AT"} {
		if !strings.Contains(parked, want) {
			t.Errorf("a parked run's detail is missing %q, got:\n%s", want, parked)
		}
	}

	// 🔴 THE HISTORY CASE, and the one an implementation that keys the block on the
	// COLUMNS rather than the STATUS gets wrong. limit_resets_at / retry_not_before /
	// rate_limit_type are deliberately left in place across a promotion, so a completed
	// run still carries all three — and rendering "resumes in 4h12m" on a run that
	// finished yesterday is nonsense. Same columns, different question.
	resumed := parkedRun(now)
	resumed.Status = "completed"
	out := render(resumed)
	for _, bad := range []string{"resumes in", "resuming shortly", "LIMIT_RESETS_AT"} {
		if strings.Contains(out, bad) {
			t.Errorf("a run that has already resumed rendered %q — the park columns are history once the status moves on, not a live countdown, got:\n%s", bad, out)
		}
	}
	// The history it SHOULD carry: this run spent part of its wall clock waiting.
	if !strings.Contains(out, "LIMIT_WAITS") || !strings.Contains(out, "resumed") {
		t.Errorf("a run that parked and resumed must still say so, got:\n%s", out)
	}

	// A run that never parked says nothing about parks beyond the opt-in itself.
	never := apitypes.RunDTO{ID: "run-2", Kind: "issue", Status: "completed", Health: "ok"}
	if out := render(never); strings.Contains(out, "LIMIT_WAIT ") || strings.Contains(out, "LIMIT_WAITS") || strings.Contains(out, "LIMIT_RESETS_AT") {
		t.Errorf("a run that never hit a usage limit must render no park rows, got:\n%s", out)
	}
}

// TestRenderRunDetailWaitOnLimitIsAlwaysPresent — unlike every other conditional row in
// this block, the opt-in rides EVERY run.
//
// It is meaningful before any park has happened: it answers "will this run survive a
// usage limit, or just die on one?", which is a question about a queued run as much as
// a parked one. Rendering it only when true would make "off" indistinguishable from an
// older CLI that does not know the field — and "off" is the default, so that is the
// answer users most need to be able to read.
func TestRenderRunDetailWaitOnLimitIsAlwaysPresent(t *testing.T) {
	render := func(on bool) string {
		t.Helper()
		var buf bytes.Buffer
		p := uzicli.NewPrinter(&buf, false, false, true, false)
		r := apitypes.RunDTO{ID: "run-1", Kind: "issue", Status: "running", Health: "ok", WaitOnLimit: on}
		if err := renderRunDetail(p, r); err != nil {
			t.Fatalf("renderRunDetail: %v", err)
		}
		return buf.String()
	}
	if out := render(true); !strings.Contains(out, "WAIT_ON_LIMIT") || !strings.Contains(out, "true") {
		t.Errorf("opted-in run: want WAIT_ON_LIMIT true, got:\n%s", out)
	}
	if out := render(false); !strings.Contains(out, "WAIT_ON_LIMIT") || !strings.Contains(out, "false") {
		t.Errorf("opted-out run: want an explicit WAIT_ON_LIMIT false — a missing row is indistinguishable from an older CLI, got:\n%s", out)
	}
}

// TestSteerStateOnAParkedRun. A park suspends the run, not the queue: nothing is
// consuming follow-ups while parked, and everything queued drains when it resumes.
func TestSteerStateOnAParkedRun(t *testing.T) {
	consumed := time.Now()

	// The queue state is UNCHANGED by the park — the suffix explains why nothing is
	// moving, and must not overwrite the queued/delivered answer itself.
	if got := steerState(nil, statusLimitWait); !strings.HasPrefix(got, "queued") {
		t.Errorf("steerState(unconsumed, limit_wait) = %q, want it to still read as queued", got)
	}
	if got := steerState(&consumed, statusLimitWait); !strings.HasPrefix(got, "delivered") {
		t.Errorf("steerState(consumed, limit_wait) = %q, want it to still read as delivered", got)
	}

	// Both say WHY nothing is happening, which is the whole point: a follow-up sitting
	// untouched for four hours is otherwise indistinguishable from a wedged run.
	for _, got := range []string{steerState(nil, statusLimitWait), steerState(&consumed, statusLimitWait)} {
		if !strings.Contains(got, "usage limit") {
			t.Errorf("steerState on a parked run = %q, want it to name the usage-limit park", got)
		}
	}

	// 🔴 NOT "not delivered (run finished)". A parked run is not finished, and that
	// label would tell a user their follow-up had been dropped when it is about to be
	// delivered. This is the shape the terminal-status map would produce if limit_wait
	// were ever added to it.
	if strings.Contains(steerState(nil, statusLimitWait), "run finished") {
		t.Error(`steerState(unconsumed, limit_wait) claims the run finished — a parked run resumes and its queue drains, so the follow-up has NOT been dropped`)
	}

	// Every other status is untouched.
	if got := steerState(nil, "running"); got != "queued" {
		t.Errorf("steerState(unconsumed, running) = %q, want %q", got, "queued")
	}
	if got := steerState(&consumed, "awaiting_approval"); got != "delivered (applies after approval)" {
		t.Errorf("steerState(consumed, awaiting_approval) = %q — the gate label regressed", got)
	}
	// PRD #517: a delivered follow-up while the run is parked awaiting_followup gets the
	// tailored "resumes the run" copy (the web twin's wording), not the generic "delivered".
	if got := steerState(&consumed, "awaiting_followup"); got != "delivered (resumes the run)" {
		t.Errorf("steerState(consumed, awaiting_followup) = %q, want the tailored follow-up label", got)
	}
	if got := steerState(nil, "completed"); got != "not delivered (run finished)" {
		t.Errorf("steerState(unconsumed, completed) = %q — the terminal label regressed", got)
	}
}
