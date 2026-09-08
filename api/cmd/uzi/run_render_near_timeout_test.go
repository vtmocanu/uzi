package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// slowRun builds a running issue run flagged near-timeout (PRD #1170): the `slow` enum
// (kept, D1) plus a server-computed deadline_at and a frozen 8h wall budget, so the
// near-timeout row carries all three clauses. deadline is `left` after now.
func slowRun(now time.Time, left time.Duration) apitypes.RunDTO {
	deadline := now.Add(left)
	bw := 8 * 60 * 60 // 8h, the frozen ceiling every ≥2-milestone run rides
	return apitypes.RunDTO{
		ID: "run-nt-1", Kind: "issue", Status: "running", IssueTitle: "do the thing",
		Health: "slow", DeadlineAt: &deadline, BudgetWallSeconds: &bw,
	}
}

// TestNearTimeoutLinePresentForFlaggedRun pins the near-timeout row's clauses for a run
// that is flagged and carries a deadline: the countdown (`left`), the stop time
// (`stops at`), and the `of <budget>` clause when a wall budget is frozen.
func TestNearTimeoutLinePresentForFlaggedRun(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	got := nearTimeoutLine(slowRun(now, 65*time.Minute), now)

	for _, want := range []string{"▲ near timeout", "left", "stops at", "· of 8h ·"} {
		if !strings.Contains(got, want) {
			t.Errorf("nearTimeoutLine = %q, want it to contain %q", got, want)
		}
	}
	// The budget clause is the PRD's compact "of 8h" (shortDuration elides a whole-hour
	// budget's "00m"), NOT the two-unit "of 8h00m".
	if strings.Contains(got, "8h00m") {
		t.Errorf("nearTimeoutLine = %q, want compact budget %q not %q", got, "of 8h", "of 8h00m")
	}
	// The countdown is off deadline_at, at two units (fmtUntil), so 65m reads "1h05m".
	if !strings.Contains(got, "1h05m left") {
		t.Errorf("nearTimeoutLine = %q, want the two-unit countdown %q", got, "1h05m left")
	}
	// The `of <budget>` clause is present ONLY because a wall budget is set.
	noBudget := slowRun(now, 65*time.Minute)
	noBudget.BudgetWallSeconds = nil
	if line := nearTimeoutLine(noBudget, now); strings.Contains(line, "of ") {
		t.Errorf("nearTimeoutLine(no budget) = %q, want no `of <budget>` clause", line)
	}
}

// TestNearTimeoutLineAbsentWhenNotFlaggable is the emit-only-when-set contract: the row is
// silent unless the run is BOTH flagged `slow` AND carries a deadline. A stalled run with a
// deadline, or a slow run whose deadline never came (a chat/judge/non-running run), draws
// nothing — which is what makes it safe to call unconditionally beside limitWaitLine.
func TestNearTimeoutLineAbsentWhenNotFlaggable(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)

	// slow but no deadline_at (a mid-rollout server, or a kind the sweeper never times out).
	noDeadline := slowRun(now, time.Hour)
	noDeadline.DeadlineAt = nil
	if line := nearTimeoutLine(noDeadline, now); line != "" {
		t.Errorf("nearTimeoutLine(slow, nil deadline) = %q, want \"\"", line)
	}

	// A different flag must not borrow the deadline: only `slow`/near-timeout counts down.
	stalled := slowRun(now, time.Hour)
	stalled.Health = "stalled"
	if line := nearTimeoutLine(stalled, now); line != "" {
		t.Errorf("nearTimeoutLine(stalled, deadline set) = %q, want \"\" — only the near-timeout flag renders this row", line)
	}
}

// TestNearTimeoutLineStoppingPastDeadline: once now has reached the deadline the row reads
// `▲ near timeout · stopping` and nothing else — the sweeper runs on a ticker, so a passed
// deadline is a run waiting on the next tick, not a countdown to a negative number.
func TestNearTimeoutLineStoppingPastDeadline(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)

	past := slowRun(now, -1*time.Minute) // deadline one minute ago
	got := nearTimeoutLine(past, now)
	if got != "▲ near timeout · stopping" {
		t.Errorf("nearTimeoutLine(past deadline) = %q, want %q", got, "▲ near timeout · stopping")
	}
	if strings.Contains(got, "left") {
		t.Errorf("nearTimeoutLine(past deadline) = %q, must not show a `left` countdown once the deadline has passed", got)
	}

	// The boundary now == deadline is already "stopping" (now >= deadline), not "0s left".
	atDeadline := slowRun(now, 0)
	if got := nearTimeoutLine(atDeadline, now); got != "▲ near timeout · stopping" {
		t.Errorf("nearTimeoutLine(now == deadline) = %q, want %q", got, "▲ near timeout · stopping")
	}
}

// TestFitNearTimeoutLineShedsClauses is the #379 one-row invariant on the clause-shedding
// helper: as the width narrows, `· of <budget>` sheds FIRST, then `· stops at HH:MM`, and
// the floor (`▲ near timeout · 1h05m left`) survives every width. The widths are derived
// from the rendered pieces so the test is timezone-independent (the HH:MM local time only
// affects the stops-at clause's width, which is measured, not assumed).
func TestFitNearTimeoutLineShedsClauses(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	r := slowRun(now, 65*time.Minute)

	full := nearTimeoutLine(r, now) // floor + " · of 8h" + " · stops at HH:MM"
	ofIdx := strings.Index(full, " · of ")
	stopsIdx := strings.Index(full, " · stops at ")
	if ofIdx < 0 || stopsIdx < 0 || stopsIdx < ofIdx {
		t.Fatalf("full line %q does not carry both clauses in display order (of before stops at)", full)
	}
	floor := full[:ofIdx]
	floorPlusStops := floor + full[stopsIdx:]

	// Wide: nothing sheds.
	if got := fitNearTimeoutLine(r, now, 200); got != full {
		t.Errorf("fitNearTimeoutLine(wide) = %q, want the full line %q", got, full)
	}

	// Narrow to exactly floor+stops: `of <budget>` sheds first (it drops by priority, not by
	// position, even though it sits before stops-at in the display order), stops-at survives.
	w1 := visualWidth(floorPlusStops)
	if got := fitNearTimeoutLine(r, now, w1); got != floorPlusStops {
		t.Errorf("fitNearTimeoutLine(width=%d) = %q, want the `of <budget>` clause shed: %q", w1, got, floorPlusStops)
	}

	// Narrow to exactly the floor: stops-at sheds too, but `left` still survives.
	w2 := visualWidth(floor)
	got := fitNearTimeoutLine(r, now, w2)
	if got != floor {
		t.Errorf("fitNearTimeoutLine(width=%d) = %q, want the floor alone: %q", w2, got, floor)
	}
	if !strings.Contains(got, "left") {
		t.Errorf("the floor %q lost the `left` countdown — it is the one clause that must never be cut", got)
	}
	// Below even the floor's width the floor is returned uncut (realistic terminals never
	// reach it, the same contract the header floor carries).
	if got := fitNearTimeoutLine(r, now, w2-5); got != floor {
		t.Errorf("fitNearTimeoutLine(narrower than the floor) = %q, want the floor uncut: %q", got, floor)
	}
}

// TestRenderRunDetailNearTimeout is the `uzi run get` human table: the HEALTH row reads the
// display word "near timeout" (never the raw `slow` enum), a DEADLINE row appears, and the
// machine-facing --json still carries "health":"slow" (D1).
func TestRenderRunDetailNearTimeout(t *testing.T) {
	now := time.Now()
	future := now.Add(90 * time.Minute)
	bw := 8 * 60 * 60
	r := apitypes.RunDTO{
		ID: "run-get-nt", Kind: "issue", Status: "running", IssueTitle: "implement the feature",
		Health: "slow", DeadlineAt: &future, BudgetWallSeconds: &bw,
	}

	var buf bytes.Buffer
	p := uzicli.NewPrinter(&buf, false, false, true, false) // non-tty, non-json, no colour
	if err := renderRunDetail(p, r); err != nil {
		t.Fatalf("renderRunDetail: %v", err)
	}
	out := buf.String()

	if !strings.Contains(out, "near timeout") {
		t.Errorf("run get human output does not render the display word \"near timeout\":\n%s", out)
	}
	// The raw enum word must not leak to a human surface.
	if strings.Contains(out, "slow") {
		t.Errorf("run get human output leaked the raw enum \"slow\":\n%s", out)
	}
	if !strings.Contains(out, "DEADLINE") {
		t.Errorf("run get human output is missing the DEADLINE row:\n%s", out)
	}

	// --json is machine-facing and keeps the enum verbatim (D1).
	j, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(j), `"health":"slow"`) {
		t.Errorf("JSON must keep the raw enum for machines, got:\n%s", j)
	}
}

// TestRenderRunDetailDeadlineRowAbsentWithoutDeadline: a run the sweeper never times out
// (no deadline_at — a chat/judge/non-running run) prints no DEADLINE row, so the field is
// emit-only-when-set exactly like HEALTH_REASON.
func TestRenderRunDetailDeadlineRowAbsentWithoutDeadline(t *testing.T) {
	r := apitypes.RunDTO{
		ID: "run-no-dl", Kind: "chat", Status: "running", IssueTitle: "chat", Health: "ok",
	}
	var buf bytes.Buffer
	p := uzicli.NewPrinter(&buf, false, false, true, false)
	if err := renderRunDetail(p, r); err != nil {
		t.Fatalf("renderRunDetail: %v", err)
	}
	if strings.Contains(buf.String(), "DEADLINE") {
		t.Errorf("a run with no deadline_at must print no DEADLINE row:\n%s", buf.String())
	}
}

// TestDeadlineCell pins the DEADLINE row value: HH:MM local plus the countdown, or
// "stopping" once passed. The HH:MM is timezone-dependent, so this asserts the countdown
// suffix rather than the exact clock.
func TestDeadlineCell(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)

	if got := deadlineCell(now.Add(65*time.Minute), now); !strings.HasSuffix(got, " · 1h05m left") {
		t.Errorf("deadlineCell(future) = %q, want it to end with the countdown \" · 1h05m left\"", got)
	}
	if got := deadlineCell(now.Add(-time.Minute), now); !strings.HasSuffix(got, " · stopping") {
		t.Errorf("deadlineCell(past) = %q, want it to end with \" · stopping\"", got)
	}
}
