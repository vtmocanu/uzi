package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// PRD #1429 M5: `schedule get`/`schedule list` render the schedule's pinned harness. A null
// pin shows an explicit "implicit" (D11 resolves it fresh per fire), distinct from an
// explicit claude/codex pin.

// TestRenderScheduleDetailHarness pins the HARNESS row in `schedule get`.
func TestRenderScheduleDetailHarness(t *testing.T) {
	render := func(h *string) string {
		t.Helper()
		var buf bytes.Buffer
		p := uzicli.NewPrinter(&buf, false, false, true, false) // non-tty, non-json, no colour
		s := apitypes.ScheduleDTO{
			ID: "sch-1", Target: "issue", Timing: "recurring", CronExpr: "0 9 * * 1",
			Status: "active", Harness: h,
		}
		if err := renderScheduleDetail(p, s); err != nil {
			t.Fatalf("renderScheduleDetail: %v", err)
		}
		return buf.String()
	}

	// An explicit pin renders the pinned harness verbatim.
	for _, h := range []string{"claude", "codex"} {
		if out := render(&h); !strings.Contains(out, "HARNESS") || !strings.Contains(out, h) {
			t.Errorf("pinned harness %q: want a HARNESS row naming it, got:\n%s", h, out)
		}
	}

	// A null pin (no stored harness column) renders the explicit word "implicit", not a blank
	// row — the pin is a meaningful configuration state (D11 resolves it fresh per fire).
	if out := render(nil); !strings.Contains(out, "HARNESS") || !strings.Contains(out, "implicit") {
		t.Errorf("null harness: want a HARNESS row reading implicit, got:\n%s", out)
	}
}

// TestScheduleListHarnessColumn pins the HARNESS column in `schedule list`: a pinned schedule
// shows the pin, an unpinned one shows "-".
func TestScheduleListHarnessColumn(t *testing.T) {
	codex := "codex"
	fc := &uzicli.FakeClient{Schedules: []apitypes.ScheduleDTO{
		{ID: "sch-pinned", Target: "issue", Timing: "recurring", CronExpr: "0 9 * * 1", Status: "active", Harness: &codex},
		{ID: "sch-implicit", Target: "issue", Timing: "recurring", CronExpr: "0 9 * * 2", Status: "active", Harness: nil},
	}}
	out, _, code := runCLI(t, fakeEnv(fc), "schedule", "list")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, "HARNESS") {
		t.Fatalf("expected a HARNESS column header, got:\n%s", out)
	}
	lines := strings.Split(out, "\n")
	var pinnedLine, implicitLine string
	for _, ln := range lines {
		if strings.Contains(ln, "sch-pinned") {
			pinnedLine = ln
		}
		if strings.Contains(ln, "sch-implicit") {
			implicitLine = ln
		}
	}
	if !strings.Contains(pinnedLine, "codex") {
		t.Errorf("a codex-pinned schedule should show codex in its row, got: %q", pinnedLine)
	}
	if !strings.Contains(implicitLine, "-") {
		t.Errorf("an unpinned schedule should show - in its row, got: %q", implicitLine)
	}
}
