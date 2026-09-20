package main

// PRD #1251 M2 — vault-locked detection + tier-1 hint. The board shows a quiet, STEADY,
// non-dismissable faint glyph+text line ("🔒 vault locked") when WhoamiVault reports the
// viewer's vault locked, and nothing otherwise (auto-clearing on unlock, D3). The hint is a
// line of its OWN beside the rate-limit strip, never replacing it (D5), and its signal is
// carried by the WORDS so it survives the colorprofile-Ascii downgrade (D4).

import (
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// vaultModel drives a fresh board model to the given vault-lock state via the real
// vaultStatusMsg path (so the Update wiring is exercised, not just the field).
func vaultModel(t *testing.T, locked bool) tuiModel {
	t.Helper()
	m := tuiTestModel(t, nil, "")
	next, _ := m.Update(vaultStatusMsg{user: apitypes.UserDTO{Email: "me@x.io"}, locked: locked})
	return next.(tuiModel)
}

// TestVaultIndicatorTier1Locked — a locked vault renders the faint glyph+text hint, and the
// board's rendered content carries it. The faint SGR is present (colour reinforces the words).
func TestVaultIndicatorTier1Locked(t *testing.T) {
	m := vaultModel(t, true)
	line := m.vaultIndicatorLine()
	if line == "" {
		t.Fatalf("locked vault produced no tier-1 indicator line")
	}
	if plain := stripANSI(line); !strings.Contains(plain, "vault locked") || !strings.Contains(plain, "🔒") {
		t.Errorf("indicator line missing glyph+text; got stripped %q", plain)
	}
	// The faint SGR reinforces (but does not carry) the signal in a colour terminal.
	if frag := m.pal.faint.Render("🔒 vault locked"); !strings.Contains(line, frag) {
		t.Errorf("indicator line is not painted with m.pal.faint; want fragment %q in %q", frag, line)
	}
	// The whole board render carries the hint text.
	if out := stripANSI(m.View().Content); !strings.Contains(out, "vault locked") {
		t.Errorf("board render does not show the vault-locked hint:\n%s", out)
	}
	// selfEmail is threaded off the same reply for M3's own-run scoping.
	if m.selfEmail != "me@x.io" {
		t.Errorf("selfEmail = %q, want the viewer identity from the whoami reply", m.selfEmail)
	}
}

// TestVaultIndicatorAbsentAndAutoClears — no hint when unlocked, and it CLEARS on its own the
// moment a later fetch reports the vault unlocked (D3: steady, auto-clearing, non-dismissable).
func TestVaultIndicatorAbsentAndAutoClears(t *testing.T) {
	unlocked := vaultModel(t, false)
	if line := unlocked.vaultIndicatorLine(); line != "" {
		t.Errorf("unlocked vault must produce no indicator, got %q", line)
	}
	if out := stripANSI(unlocked.View().Content); strings.Contains(out, "vault locked") {
		t.Errorf("unlocked board must not show the vault-locked hint:\n%s", out)
	}
	// Lock, then unlock: the hint appears and then auto-clears with no dismissal.
	locked := vaultModel(t, true)
	if locked.vaultIndicatorLine() == "" {
		t.Fatalf("locked vault produced no indicator")
	}
	next, _ := locked.Update(vaultStatusMsg{locked: false})
	if cleared := next.(tuiModel); cleared.vaultIndicatorLine() != "" {
		t.Errorf("indicator did not auto-clear when the vault reported unlocked")
	}
}

// TestVaultIndicatorErrorKeepsLastKnown — a failed fetch is swallowed like the rate-limit
// strip's: the last-known lock state is left untouched, so a transient error never flashes a
// spurious lock nor clears a real one.
func TestVaultIndicatorErrorKeepsLastKnown(t *testing.T) {
	locked := vaultModel(t, true)
	next, _ := locked.Update(vaultStatusMsg{locked: false, err: errors.New("whoami failed")})
	if kept := next.(tuiModel); kept.vaultIndicatorLine() == "" {
		t.Errorf("an errored fetch cleared a real lock; the last-known state must survive")
	}
}

// TestVaultIndicatorAsciiFallback — under colorprofile.Ascii / NO_COLOR the glyph is dropped
// for a plain "[locked]" marker with NO colour, so the WORDS carry the signal (D4).
func TestVaultIndicatorAsciiFallback(t *testing.T) {
	m := vaultModel(t, true)
	next, _ := m.Update(tea.ColorProfileMsg{Profile: colorprofile.Ascii})
	m = next.(tuiModel)
	line := m.vaultIndicatorLine()
	if !strings.Contains(line, "[locked] vault locked") {
		t.Errorf("ascii fallback missing the ascii-safe marker; got %q", line)
	}
	if strings.Contains(line, "🔒") {
		t.Errorf("ascii fallback still carries the lock glyph; got %q", line)
	}
	// No SGR/colour under Ascii: the words alone carry it.
	if strings.ContainsRune(line, '\x1b') {
		t.Errorf("ascii fallback still emits an SGR escape; got %q", line)
	}
	if frag := m.pal.faint.Render("🔒 vault locked"); strings.Contains(m.View().Content, frag) {
		t.Errorf("ascii board still paints the faint glyph line; want it absent")
	}
	if out := stripANSI(m.View().Content); !strings.Contains(out, "[locked] vault locked") {
		t.Errorf("ascii board does not show the ascii-safe vault hint:\n%s", out)
	}
}

// TestVaultIndicatorCoexistsWithStrip — D5: the hint and the rate-limit strip are DISTINCT
// signals on their OWN lines and show together; the hint never replaces or hides the strip.
func TestVaultIndicatorCoexistsWithStrip(t *testing.T) {
	m := stripModel(t, []apitypes.TokenRateLimitDTO{
		okMeter("sec-personal", "personal", true, 33, 61),
	}, nil)
	next, _ := m.Update(vaultStatusMsg{locked: true})
	m = next.(tuiModel)

	out := stripANSI(m.View().Content)
	if !strings.Contains(out, "33%") {
		t.Errorf("the rate-limit strip was hidden when the vault hint showed (D5 broken):\n%s", out)
	}
	if !strings.Contains(out, "vault locked") {
		t.Errorf("the vault-locked hint is missing while the strip renders:\n%s", out)
	}
	// They occupy DISTINCT lines — the hint is added beside the strip, not merged into it.
	var stripLine, vaultLine = -1, -1
	for i, ln := range strings.Split(out, "\n") {
		if strings.Contains(ln, "33%") {
			stripLine = i
		}
		if strings.Contains(ln, "vault locked") {
			vaultLine = i
		}
	}
	if stripLine < 0 || vaultLine < 0 || stripLine == vaultLine {
		t.Errorf("strip (line %d) and vault hint (line %d) must be present on distinct lines:\n%s", stripLine, vaultLine, out)
	}
}

// TestBoardCapacityReservesVaultLine — the row window reserves exactly one fewer content row
// when the vault hint is showing, mirroring how the rate-limit strip is accounted, so a full
// board does not overflow the footer off screen by a line.
func TestBoardCapacityReservesVaultLine(t *testing.T) {
	unlocked := vaultModel(t, false)
	locked := vaultModel(t, true)
	capUnlocked := unlocked.boardCapacity()
	capLocked := locked.boardCapacity()
	if capLocked != capUnlocked-1 {
		t.Errorf("boardCapacity: locked = %d, unlocked = %d; the vault hint must reserve exactly one row (want locked == unlocked-1)", capLocked, capUnlocked)
	}
}
