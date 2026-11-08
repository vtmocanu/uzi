package main

import (
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// PRD #3989: a run whose owner had an admin reset their account parks as paused with
// hold_reason credentials_reset. It resumes by itself once a credential is added, and the
// server refuses resume, so every surface names Settings and no CLI verb.

const wantCredsResetAction = "add a credential in Settings; the run resumes automatically" //nolint:gosec // G101: expected owner-facing guidance text, not a credential.

func TestHoldRowCredentialsReset(t *testing.T) {
	now := time.Date(2026, 9, 12, 15, 12, 0, 0, time.UTC)
	run := heldRun("r1", holdCredentialsReset, now.Add(-70*time.Minute))
	want := "credentials were reset · parked 14:02 (1h10m) · 4/7 milestones · " + wantCredsResetAction
	if row := holdRow(run, now); row == nil || row[1] != want {
		t.Errorf("holdRow = %v, want [HOLD %q]", row, want)
	}
}

func TestRunGetAndListCredentialsReset(t *testing.T) {
	run := heldRun("r1", holdCredentialsReset, time.Now().Add(-5*time.Minute))
	fc := &uzicli.FakeClient{
		RunByID: map[string]apitypes.RunDTO{"r1": run},
		Runs:    []apitypes.RunListItemDTO{{RunDTO: run}},
	}
	out, _, code := runCLI(t, fakeEnv(fc), "run", "get", "r1")
	if code != uzicli.ExitOK {
		t.Fatalf("run get exit = %d, want 0", code)
	}
	if !strings.Contains(out, "credentials were reset") || !strings.Contains(out, wantCredsResetAction) {
		t.Errorf("run get lacks the credentials_reset HOLD row:\n%s", out)
	}
	list, _, code := runCLI(t, fakeEnv(fc), "run", "list")
	if code != uzicli.ExitOK {
		t.Fatalf("run list exit = %d, want 0", code)
	}
	if !strings.Contains(list, "paused (credentials were reset)") {
		t.Errorf("run list STATUS lacks the credentials_reset qualifier:\n%s", list)
	}
}

func TestCredentialsResetNoticeAndLine(t *testing.T) {
	run := heldRun("r1", holdCredentialsReset, time.Now())
	if got, want := pausedHoldNotice(run), "held, your credentials were reset; "+wantCredsResetAction; got != want {
		t.Errorf("pausedHoldNotice = %q, want %q", got, want)
	}
	if got, want := credentialHoldLine(run), "waiting: credentials were reset · "+wantCredsResetAction; got != want {
		t.Errorf("credentialHoldLine = %q, want %q", got, want)
	}
	// Only a paused run is held; a resumed run keeps the marker through queued.
	run.Status = "queued"
	if isCredentialsResetHold(run) || credentialHoldLine(run) != "" {
		t.Errorf("a non-paused run must not read as a credentials_reset hold")
	}
	if isCredentialsResetHold(heldRun("r2", holdCredentialDisabled, time.Now())) {
		t.Errorf("credential_disabled must not read as credentials_reset")
	}
}

func TestTUICredentialsResetHold(t *testing.T) {
	run := heldRun("99999999-reset", holdCredentialsReset, time.Now().Add(-5*time.Minute))
	run.Health = "ok"

	m := tuiTestModel(t, &uzicli.FakeClient{}, run.ID)
	m = applyDetail(m, run, nil)
	raw := m.View().Content
	assertNoRawControls(t, "credentials_reset detail", raw)
	out := stripANSI(raw)
	if !strings.Contains(out, "waiting: credentials were reset · "+wantCredsResetAction) {
		t.Errorf("detail lacks the credentials_reset line:\n%s", out)
	}
	if strings.Contains(out, "paused by you") {
		t.Errorf("a credentials_reset hold must not read as an owner pause:\n%s", out)
	}
	if !strings.Contains(out, credDisabledGlyph+" "+credsResetWord) {
		t.Errorf("detail header lacks the %s %s token:\n%s", credDisabledGlyph, credsResetWord, out)
	}

	item := apitypes.RunListItemDTO{RunDTO: run}
	if b := runBandOf(item); b != bandNeedsYou {
		t.Errorf("runBandOf(credentials_reset) = %d, want bandNeedsYou (%d)", b, bandNeedsYou)
	}
	if w := runStateWord(item); w != credsResetWord {
		t.Errorf("runStateWord = %q, want %q", w, credsResetWord)
	}
	board := tuiTestModel(t, &uzicli.FakeClient{}, "")
	board = step(board, boardRunsMsg{reqID: board.board.waitID, runs: []apitypes.RunListItemDTO{item}})
	bout := stripANSI(board.View().Content)
	for _, want := range []string{"NEEDS YOU", credsResetWord, "▸ waiting: credentials were reset", credDisabledGlyph + " 1"} {
		if !strings.Contains(bout, want) {
			t.Errorf("board lacks %q:\n%s", want, bout)
		}
	}
}

func TestRunWaitCredentialsResetLine(t *testing.T) {
	reason := holdCredentialsReset
	held := apitypes.RunDTO{ID: "r1", Kind: "issue", Status: statusPaused, HoldReason: &reason}
	fc := &uzicli.FakeClient{GetRunHook: scriptHook(okStep("running"), waitStep{run: held}, okStep("completed"))}

	_, stderr, code := runCLI(t, fakeEnv(fc), "run", "wait", "r1", "--interval", "1ms")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stderr, "held, your credentials were reset; "+wantCredsResetAction) {
		t.Errorf("expected the credentials_reset line, stderr = %q", stderr)
	}
	if !strings.Contains(stderr, "paused → completed") {
		t.Errorf("wait must keep going through the hold, stderr = %q", stderr)
	}
}

func TestRunLogsFollowCredentialsResetNotice(t *testing.T) {
	held := pausedWithHold(holdCredentialsReset)
	stderr := followLogs(t,
		apitypes.RunDTO{ID: "r1", Status: "running"},
		held, held, held,
		apitypes.RunDTO{ID: "r1", Status: "queued"},
		apitypes.RunDTO{ID: "r1", Status: "completed"},
	)
	want := "run r1 held, your credentials were reset; " + wantCredsResetAction + "; still following"
	if n := strings.Count(stderr, want); n != 1 {
		t.Errorf("credentials_reset notice appeared %d times, want exactly 1:\n%s", n, stderr)
	}
	if !strings.Contains(stderr, "run r1 resumed (queued)") {
		t.Errorf("missing the resume line:\n%s", stderr)
	}
}
