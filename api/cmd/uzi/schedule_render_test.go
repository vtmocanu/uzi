package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// TestRenderScheduleDetailTokenCell pins the TOKEN detail row (PRD #1247 M6,
// scheduleTokenCell) in `schedule get`, mirroring the sibling MR_REWORK tri-state test:
//   - a pinned override with a resolvable label shows the LABEL,
//   - a pinned override whose label could not be resolved falls back to the bare "pinned" mode,
//   - auto/default show their bare mode,
//   - no override at all (co == nil) shows "inherit".
//
// The hostile-label case pins that a control-byte-bearing token label is neutralised before
// it reaches the terminal. scheduleTokenCell itself returns the label RAW; the sanitization
// is provided by the render boundary (renderScheduleDetail -> Printer.Table -> uzicli.CellText,
// which sanitizes every cell), exactly as the run-detail ANTHROPIC_TOKEN row relies on. The
// test therefore drives the real render path and asserts on the rendered bytes, not on
// scheduleTokenCell in isolation — so it stays honest about WHERE the safety lives.
func TestRenderScheduleDetailTokenCell(t *testing.T) {
	render := func(co *apitypes.CredentialOverrideDTO) string {
		t.Helper()
		var buf bytes.Buffer
		p := uzicli.NewPrinter(&buf, false, false, true, false) // non-tty, non-json, no colour
		s := apitypes.ScheduleDTO{
			ID: "sch-1", Target: "issue", Timing: "recurring", CronExpr: "0 9 * * 1",
			Status: "active", CredentialOverride: co,
		}
		if err := renderScheduleDetail(p, s); err != nil {
			t.Fatalf("renderScheduleDetail: %v", err)
		}
		return buf.String()
	}

	// A pinned override with a resolvable label surfaces the LABEL.
	if out := render(&apitypes.CredentialOverrideDTO{Mode: "pinned", Label: ptr("console-key")}); !strings.Contains(out, "TOKEN") || !strings.Contains(out, "console-key") {
		t.Errorf("pinned override: want TOKEN row naming the label console-key, got:\n%s", out)
	}

	// A pinned override whose label could not be resolved (nil, or empty) falls back to the
	// bare "pinned" mode — a renamed/deleted token must still render honestly, not blank.
	for _, label := range []*string{nil, ptr("")} {
		if out := render(&apitypes.CredentialOverrideDTO{Mode: "pinned", Label: label}); !strings.Contains(out, "TOKEN") || !strings.Contains(out, "pinned") {
			t.Errorf("pinned override with unresolved label %v: want TOKEN row showing the bare mode \"pinned\", got:\n%s", label, out)
		}
	}

	// auto/default show their bare mode.
	for _, mode := range []string{"auto", "default"} {
		if out := render(&apitypes.CredentialOverrideDTO{Mode: mode}); !strings.Contains(out, "TOKEN") || !strings.Contains(out, mode) {
			t.Errorf("%s override: want TOKEN row showing the mode %q, got:\n%s", mode, mode, out)
		}
	}

	// No override at all → the fired run follows the worker binding: "inherit".
	if out := render(nil); !strings.Contains(out, "TOKEN") || !strings.Contains(out, "inherit") {
		t.Errorf("no override: want TOKEN row showing inherit, got:\n%s", out)
	}

	// A control-byte-bearing pinned label must be neutralised before it reaches the terminal:
	// the CSI escape would repaint the terminal, U+202E (RIGHT-TO-LEFT OVERRIDE) would visually
	// reverse the label so it READS as a different token, and the newline would break the table
	// rail (forge a row). All three must be gone; the printable text must survive. This mirrors
	// the run-detail ANTHROPIC_TOKEN hostile-label probe: the label is a user-authored token
	// name snapshotted into the DTO, and rows written before the validator landed / straight to
	// the DB reach the renderer without passing validateSecretLabel, so the render-side defence
	// is load-bearing on its own.
	hostile := "safe\u202ednetsop\x1b[31m\nnext-line"
	out := render(&apitypes.CredentialOverrideDTO{Mode: "pinned", Label: ptr(hostile)})
	for _, bad := range []string{"\u202e", "\x1b", "\nnext-line"} {
		if strings.Contains(out, bad) {
			t.Errorf("hostile token label reached the terminal carrying %q, got:\n%q", bad, out)
		}
	}
	if !strings.Contains(out, "safe") || !strings.Contains(out, "next-line") {
		t.Errorf("sanitizing dropped the printable text too, got:\n%q", out)
	}
}
