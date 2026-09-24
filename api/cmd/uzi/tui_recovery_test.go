package main

import (
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// Issue #1197 M5: the TUI/CLI must render recovery_wait — a transient-recovery park that
// auto-resumes on a capped backoff — as a wait-family state, never falling through to a
// default arm that draws the raw enum or a generic dot. These pin the shared render
// helpers plus the lane ladder, modelled on the limit_wait/pool_wait siblings.

// TestStateGlyphWordRecoveryWait pins the glyph + word. Reddening mutation: remove the
// statusRecoveryWait case from stateGlyphWord → the default arm returns ("·", cellText of
// the lowercased status), i.e. glyph "·" and word "recovery_wait", so both assertions
// fail. It shares the wait-family "~" glyph with limit_wait/pool_wait but carries a
// DISTINCT word so a user can tell the three holds apart at a glance.
func TestStateGlyphWordRecoveryWait(t *testing.T) {
	glyph, word := stateGlyphWord(statusRecoveryWait, "", false, false, "")
	if glyph != "~" {
		t.Errorf("recovery_wait glyph = %q, want %q (the shared wait-family glyph)", glyph, "~")
	}
	if word != "recovery wait" {
		t.Errorf("recovery_wait word = %q, want %q (must not be the raw enum)", word, "recovery wait")
	}
	// Same wait-family glyph as its sibling holds — the word distinguishes them, not the glyph.
	if limitGlyph, _ := stateGlyphWord(statusLimitWait, "", false, false, ""); glyph != limitGlyph {
		t.Errorf("recovery_wait glyph %q differs from limit_wait's %q — the non-terminal holds share one wait glyph", glyph, limitGlyph)
	}
	// Distinct WORD from the neighbouring holds, so the spine reads three different states.
	if _, poolWord := stateGlyphWord(statusPoolWait, "", false, false, ""); word == poolWord {
		t.Errorf("recovery_wait and pool_wait share word %q — they must be distinguishable", word)
	}
}

// TestStateGlyphWordForgePark pins the PRD #1392 M5 forge branch: a recovery_wait run whose
// cause is "forge_unreachable" reads the DISTINCT word "forge wait", while the empty-turn /
// null cause keeps "recovery wait". The glyph and wait-family colour stay the same for both —
// only the word distinguishes them. Reddening mutation: drop the cause branch in
// stateGlyphWord → the forge park falls back to "recovery wait" and the first assertion fails.
func TestStateGlyphWordForgePark(t *testing.T) {
	glyph, word := stateGlyphWord(statusRecoveryWait, "", false, false, "", "forge_unreachable")
	if glyph != "~" {
		t.Errorf("forge park glyph = %q, want %q (still a wait-family hold)", glyph, "~")
	}
	if word != "forge wait" {
		t.Errorf("forge park word = %q, want %q", word, "forge wait")
	}
	// The empty-turn / null cause keeps the issue #1197 wording.
	if _, w := stateGlyphWord(statusRecoveryWait, "", false, false, "", ""); w != "recovery wait" {
		t.Errorf("empty-cause recovery park word = %q, want %q", w, "recovery wait")
	}
	if _, w := stateGlyphWord(statusRecoveryWait, "", false, false, ""); w != "recovery wait" {
		t.Errorf("cause-less recovery park word = %q, want %q (the pre-M5 call shape)", w, "recovery wait")
	}
	// An unrecognised cause is not the forge one, so it keeps the generic wording.
	if _, w := stateGlyphWord(statusRecoveryWait, "", false, false, "", "provider_outage"); w != "recovery wait" {
		t.Errorf("other-cause recovery park word = %q, want %q", w, "recovery wait")
	}
	// The forge park still shares the wait ink with its sibling holds (colour unchanged).
	p := newPalette(true)
	if bgFillSGR(p.stateColor(statusRecoveryWait, "", false, false)) != bgFillSGR(p.wait) {
		t.Error("forge park colour is not the wait ink; it must stay in the wait family")
	}
}

// TestStateColorRecoveryWait pins the colour to the shared wait ink. Reddening mutation:
// remove statusRecoveryWait from stateColor's wait case → it falls to the faintC default,
// so it stops matching p.wait (and its sibling holds) and matches faintC.
func TestStateColorRecoveryWait(t *testing.T) {
	p := newPalette(true)
	recovery := bgFillSGR(p.stateColor(statusRecoveryWait, "", false, false))
	if recovery != bgFillSGR(p.wait) {
		t.Error("recovery_wait colour is not the wait ink; a non-terminal hold must share the wait colour with limit_wait/pool_wait")
	}
	if recovery != bgFillSGR(p.stateColor(statusLimitWait, "", false, false)) {
		t.Error("recovery_wait and limit_wait resolve to different colours; both are non-terminal holds and share the wait ink")
	}
	if recovery == bgFillSGR(p.faintC) {
		t.Error("recovery_wait resolved to the faint default; its explicit wait case is not being read")
	}
}

// A recovery park performs no work, so health frozen at park time cannot replace its
// wait token. Verified 2026-09-08: the old health-first ladder hid the recovery state
// behind stalled/looping/near-timeout text and stall ink on every TUI surface.
func TestRecoveryWaitIgnoresStaleHealth(t *testing.T) {
	for _, health := range []string{"stalled", "looping", "slow"} {
		t.Run(health, func(t *testing.T) {
			glyph, word := stateGlyphWord(statusRecoveryWait, health, false, false, "")
			if glyph != "~" || word != "recovery wait" {
				t.Errorf("recovery park with stale %s = (%q, %q), want (~, recovery wait)", health, glyph, word)
			}
			for _, dark := range []bool{false, true} {
				p := newPalette(dark)
				if got := bgFillSGR(p.stateColor(statusRecoveryWait, health, false, false)); got != bgFillSGR(p.wait) {
					t.Errorf("recovery park with stale %s, dark=%t uses %q, want wait ink", health, dark, got)
				}
				// Positive control: an actually running unhealthy agent still needs attention.
				if got := bgFillSGR(p.stateColor("running", health, false, false)); got != bgFillSGR(p.stall) {
					t.Errorf("running %s agent, dark=%t lost its stall ink", health, dark)
				}
			}
			if glyph, word := stateGlyphWord("running", health, false, false, ""); glyph != "▲" || word != displayHealth(health) {
				t.Errorf("running %s agent lost its health token: (%q, %q)", health, glyph, word)
			}
		})
	}
}

// The board summary must agree with the recovery row: a frozen health flag cannot
// inflate the attention count while that row correctly displays a recovery wait.
func TestRecoveryWaitBoardSummaryIgnoresStaleHealth(t *testing.T) {
	runs := []apitypes.RunListItemDTO{
		{RunDTO: apitypes.RunDTO{ID: "recovery-1", Kind: "issue", Status: statusRecoveryWait, Health: "stalled", IssueTitle: "recovering"}},
		{RunDTO: apitypes.RunDTO{ID: "running-2", Kind: "issue", Status: "running", Health: "looping", IssueTitle: "needs attention"}},
	}
	m := tuiTestModel(t, &uzicli.FakeClient{}, "")
	m = step(m, boardRunsMsg{reqID: m.board.waitID, runs: runs})
	out := stripANSI(m.View().Content)
	if !strings.Contains(out, "▲ 1") || strings.Contains(out, "▲ 2") {
		t.Errorf("only the running unhealthy agent belongs in the attention count:\n%s", out)
	}
	// Keep the parked row as a positive control when the warning cluster disappears.
	m = step(m, boardRunsMsg{reqID: m.board.waitID, runs: runs[:1]})
	out = stripANSI(m.View().Content)
	if !strings.Contains(out, "recover") || !strings.Contains(out, "1 runs") {
		t.Fatalf("the recovery run disappeared instead of losing its health warning:\n%s", out)
	}
	if strings.Contains(out, "▲") {
		t.Errorf("a recovery-only board must not show stale health attention:\n%s", out)
	}
}

// TestCrewStateForRecoveryWait pins the lane ladder: a recovery_wait run reads crewWaiting,
// never crewIdle or crewStalled — it rides the same gate/park rung as the other holds.
// Reddening mutation: remove statusRecoveryWait from crewStateFor's waiting condition → a
// long-parked run falls to the recency split (crewIdle) or the active-speaker rung
// (crewStalled off a frozen health flag).
func TestCrewStateForRecoveryWait(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	const me = "toolu_me"

	if got := crewStateFor(statusRecoveryWait, "ok", me, "", now.Add(-4*time.Hour), now); got != crewWaiting {
		t.Errorf("crewStateFor(recovery_wait, quiet for 4h) = %s, want %s — a run parked on a scheduled promotion is waiting, not idle", got, crewWaiting)
	}
	for _, health := range []string{"stalled", "slow", "looping"} {
		if got := crewStateFor(statusRecoveryWait, health, me, me, now.Add(-4*time.Hour), now); got != crewWaiting {
			t.Errorf("crewStateFor(recovery_wait, health=%q, active speaker) = %s, want %s — a stale health flag frozen at park time must not make a parked run read stalled",
				health, got, crewWaiting)
		}
	}
	// The park is NOT terminal — the inverse property, which would truncate `uzi run logs
	// --follow` on a run that auto-resumes if it regressed.
	if isTerminalRunStatus(statusRecoveryWait) {
		t.Error("isTerminalRunStatus(recovery_wait) = true — the transient-recovery park auto-resumes and must not read terminal")
	}
	if terminalRunStatuses[statusRecoveryWait] {
		t.Error("run.go's terminalRunStatuses contains recovery_wait — `uzi run logs --follow` would exit mid-run and truncate the capture")
	}
}

// TestStateGlyphWordCodexHold pins the PRD #1590 token: a run held on its Codex account reads
// "codex wait" in the wait family. Reddening mutation: drop the codex cause branch in
// stateGlyphWord → it falls back to "recovery wait".
func TestStateGlyphWordCodexHold(t *testing.T) {
	glyph, word := stateGlyphWord(statusRecoveryWait, "", false, false, "", codexAccountUnavailableCause)
	if glyph != "~" || word != "codex wait" {
		t.Errorf("codex hold token = (%q, %q), want (~, codex wait)", glyph, word)
	}
}

// TestCodexHoldTUIDetailAndBoard drives the real model: the run detail draws the action line
// (with a hostile label rendered inert, and no retry countdown), and the board's selected
// held row draws it as its second line. Mutation-checked: removing the detail draw in
// tui_detail.go, and the codex branch in boardSecondLine, each reddened this test.
func TestCodexHoldTUIDetailAndBoard(t *testing.T) {
	cause := codexAccountUnavailableCause
	action := "relogin_required"
	// Hostile label: ESC, BEL and the bidi override are stripped, so only the printable
	// residue ("[2J", "]8;;http://evil") survives, inert text that no terminal interprets.
	label := "\x1b[2J\u202E\x07work\x1b]8;;http://evil\x07"
	retry := time.Now().Add(20 * time.Minute)
	run := apitypes.RunDTO{ID: "99999999-codex", Kind: "issue", Status: statusRecoveryWait, IssueTitle: "held run",
		RecoveryWaitCause: &cause, CodexAccountAction: &action, CodexSecretLabel: &label, RecoveryRetryNotBefore: &retry}

	m := tuiTestModel(t, &uzicli.FakeClient{}, run.ID)
	m = applyDetail(m, run, nil)
	raw := m.View().Content
	assertNoRawControls(t, "codex hold detail", raw)
	out := stripANSI(raw)
	if !strings.Contains(out, "re-log in Codex credential [2Jwork]8;;http://evil to continue") {
		t.Errorf("detail lacks the Codex account action line:\n%s", out)
	}
	if !strings.Contains(out, "codex wait") {
		t.Errorf("detail header lacks the codex wait token:\n%s", out)
	}
	if strings.Contains(out, "retry at") || strings.Contains(out, "resumes in") {
		t.Errorf("a Codex account hold must show no retry countdown:\n%s", out)
	}

	board := tuiTestModel(t, &uzicli.FakeClient{}, "")
	board = step(board, boardRunsMsg{reqID: board.board.waitID, runs: []apitypes.RunListItemDTO{{RunDTO: run}}})
	raw = board.View().Content
	assertNoRawControls(t, "codex hold board", raw)
	out = stripANSI(raw)
	if !strings.Contains(out, "codex wait") || !strings.Contains(out, "▸ re-log in Codex credential [2Jwork]8;;http://evil to continue") {
		t.Errorf("board lacks the codex wait token or the selected row's action line:\n%s", out)
	}
}
