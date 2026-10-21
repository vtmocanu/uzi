package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// Issue #3181: the two credential-switch holds a retained-recovery run can park in.

func TestHoldRowCredentialSwitch(t *testing.T) {
	now := time.Date(2026, 9, 12, 15, 12, 0, 0, time.UTC)
	parkedAt := now.Add(-70 * time.Minute)

	settling := heldRun("r1", holdCredentialSwitchSettling, parkedAt)
	want := "credential switch settling · parked 14:02 (1h10m) · 4/7 milestones · worker is stopping the run's processes; Cancel is available: uzi run cancel r1"
	if row := holdRow(settling, now); row == nil || row[1] != want {
		t.Errorf("holdRow(settling) = %v, want [HOLD %q]", row, want)
	}
	unsettled := heldRun("r1", holdCredentialSwitchUnsettled, parkedAt)
	want = "credential switch unsettled · parked 14:02 (1h10m) · 4/7 milestones · Resume or Cancel: uzi run resume r1 / uzi run cancel r1"
	if row := holdRow(unsettled, now); row == nil || row[1] != want {
		t.Errorf("holdRow(unsettled) = %v, want [HOLD %q]", row, want)
	}
	if isCredentialSwitchHold(heldRun("r1", holdCredentialDisabled, parkedAt)) {
		t.Error("credential_disabled must not read as a credential-switch hold")
	}
}

func TestRunGetAndListCredentialSwitch(t *testing.T) {
	for reason, wantRow := range map[string]string{
		holdCredentialSwitchSettling:  "Cancel is available",
		holdCredentialSwitchUnsettled: "Resume or Cancel",
	} {
		run := heldRun("r1", reason, time.Now().Add(-5*time.Minute))
		fc := &uzicli.FakeClient{
			RunByID: map[string]apitypes.RunDTO{"r1": run},
			Runs:    []apitypes.RunListItemDTO{{RunDTO: run}},
		}
		out, _, code := runCLI(t, fakeEnv(fc), "run", "get", "r1")
		if code != uzicli.ExitOK || !strings.Contains(out, "HOLD") || !strings.Contains(out, wantRow) {
			t.Errorf("%s: run get exit=%d lacks %q:\n%s", reason, code, wantRow, out)
		}
		list, _, code := runCLI(t, fakeEnv(fc), "run", "list")
		if code != uzicli.ExitOK || !strings.Contains(list, "paused (credential switch ") {
			t.Errorf("%s: run list lacks the qualifier (exit %d):\n%s", reason, code, list)
		}
	}
}

// TestRunWaitStopsOnCredentialSwitchHold: a default wait stops (exit 0) on either hold and
// names the owner's next step on stderr.
func TestRunWaitStopsOnCredentialSwitchHold(t *testing.T) {
	for reason, wantLine := range map[string]string{
		holdCredentialSwitchSettling:  "worker is stopping the run's processes; Cancel is available",
		holdCredentialSwitchUnsettled: "Resume or Cancel",
	} {
		held := pausedWithHold(reason)
		fc := &uzicli.FakeClient{GetRunHook: scriptHook(okStep("running"), waitStep{run: held}, okStep("completed"))}
		_, stderr, code := runCLI(t, fakeEnv(fc), "run", "wait", "r1", "--interval", "1ms")
		if code != uzicli.ExitOK {
			t.Fatalf("%s: exit = %d (stderr: %s)", reason, code, stderr)
		}
		if !strings.Contains(stderr, wantLine) || strings.Contains(stderr, "paused → completed") {
			t.Errorf("%s: wait must stop on the hold and name %q, stderr = %q", reason, wantLine, stderr)
		}
	}
}

func TestRunLogsFollowCredentialSwitchNotice(t *testing.T) {
	stderr := followLogs(t, pausedWithHold(holdCredentialSwitchSettling), pausedWithHold(holdCredentialSwitchUnsettled), runAt("completed"))
	if !strings.Contains(stderr, "credential switch settling") || !strings.Contains(stderr, "credential switch unsettled") {
		t.Errorf("follow must name both holds, stderr = %q", stderr)
	}
}

func TestTUICredentialSwitchHold(t *testing.T) {
	for reason, c := range map[string]struct{ word, line string }{
		holdCredentialSwitchSettling:  {credSwitchSettlingWord, "waiting: credential switch settling · worker is stopping"},
		holdCredentialSwitchUnsettled: {credSwitchUnsettledWord, "held: credential switch, processes not proven stopped · Resume or Cancel"},
	} {
		run := heldRun("99999999-sw", reason, time.Now().Add(-5*time.Minute))
		run.Health = "ok"
		m := tuiTestModel(t, &uzicli.FakeClient{}, run.ID)
		m = applyDetail(m, run, nil)
		raw := m.View().Content
		assertNoRawControls(t, reason, raw)
		out := stripANSI(raw)
		if !strings.Contains(out, c.line) || strings.Contains(out, "paused by you") {
			t.Errorf("%s: detail lacks %q:\n%s", reason, c.line, out)
		}
		item := apitypes.RunListItemDTO{RunDTO: run}
		if runBandOf(item) != bandNeedsYou || runStateWord(item) != c.word {
			t.Errorf("%s: band/word = %d/%q", reason, runBandOf(item), runStateWord(item))
		}
		if n := len([]rune(c.word)); n >= boardStatusWordWidth {
			t.Errorf("word %q does not fit boardStatusWordWidth", c.word)
		}
		board := tuiTestModel(t, &uzicli.FakeClient{}, "")
		board = step(board, boardRunsMsg{reqID: board.board.waitID, runs: []apitypes.RunListItemDTO{item}})
		bout := stripANSI(board.View().Content)
		for _, want := range []string{"NEEDS YOU", c.word, credSwitchGlyph + " 1"} {
			if !strings.Contains(bout, want) {
				t.Errorf("%s: board lacks %q:\n%s", reason, want, bout)
			}
		}
	}
}

// TestCredentialSwitchRefusalSurfacedVerbatim: the server's 409 prose from resume and
// set-token reaches stderr unchanged with the conflict exit code.
func TestCredentialSwitchRefusalSurfacedVerbatim(t *testing.T) {
	const msg = "the worker is still stopping this run's processes; Resume becomes available when it finishes, or Cancel the run"
	refusal := &uzicli.ExitError{Code: uzicli.ExitConflict, Err: errors.New(msg)}
	for name, args := range map[string][]string{
		"resume":    {"run", "resume", "r1"},
		"set-token": {"run", "set-token", "r1", "work"},
	} {
		fc := &uzicli.FakeClient{Err: refusal}
		_, stderr, code := runCLI(t, fakeEnv(fc), args...)
		if code != uzicli.ExitConflict || !strings.Contains(stderr, msg) {
			t.Errorf("%s: exit=%d stderr=%q, want %d and the verbatim message", name, code, stderr, uzicli.ExitConflict)
		}
	}
}
