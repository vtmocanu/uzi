package main

import (
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func spendRun(kind, status string) apitypes.RunDTO {
	cause := anthropicSpendLimitCause
	return apitypes.RunDTO{ID: "r1", Kind: kind, Status: status, Health: "ok", IssueTitle: "capped", RecoveryWaitCause: &cause}
}

func TestSpendLimitHoldLineSwitchable(t *testing.T) {
	for _, kind := range []string{"issue", "task"} {
		line := spendLimitHoldLine(spendRun(kind, statusRecoveryWait))
		want := "Anthropic API spend limit reached · Resume uses this run's current credential selection. If it still selects the capped token, raise the limit first: uzi run resume r1"
		if !strings.Contains(line, want) {
			t.Errorf("%s: line lacks Resume wording: %q", kind, line)
		}
		list, set := strings.Index(line, "uzi token list"), strings.Index(line, "uzi run set-token r1 <label>")
		if list < 0 || set < 0 || list > set {
			t.Errorf("%s: token list must precede set-token: %q", kind, line)
		}
		if !strings.HasSuffix(line, "Cancel: uzi run cancel r1") {
			t.Errorf("%s: line lacks Cancel: %q", kind, line)
		}
	}
}

func TestSpendLimitHoldLineNotSwitchable(t *testing.T) {
	for _, kind := range []string{"self_improve", "chat", "judge", "job"} {
		r := spendRun(kind, statusRecoveryWait)
		for _, line := range append([]string{spendLimitHoldLine(r)}, spendLimitHoldTUILines(r)...) {
			if strings.Contains(line, "token list") || strings.Contains(line, "set-token") {
				t.Errorf("%s: must not offer a switch: %q", kind, line)
			}
		}
		line := spendLimitHoldLine(r)
		if !strings.Contains(line, "uzi run resume r1") || !strings.Contains(line, "uzi run cancel r1") {
			t.Errorf("%s: lacks Resume/Cancel: %q", kind, line)
		}
	}
}

func TestSpendLimitStaleCauseOnQueuedRunIsNotAHold(t *testing.T) {
	r := spendRun("issue", "queued")
	if isSpendLimitHold(r) || spendLimitHoldLine(r) != "" {
		t.Fatal("a queued run with a stale cause must not be a hold")
	}
	fc := &uzicli.FakeClient{GetRunHook: scriptHook(waitStep{run: r}, okStep("completed"))}
	_, stderr, code := runCLI(t, fakeEnv(fc), "run", "wait", "r1", "--interval", "1ms", "--timeout", "5s")
	if code != uzicli.ExitOK || strings.Contains(stderr, "spend limit") || !strings.Contains(stderr, "queued → completed") {
		t.Errorf("exit=%d stderr=%q: stale cause must not stop or print a line", code, stderr)
	}
}

func TestRunWaitStopsOnSpendLimitHold(t *testing.T) {
	fc := &uzicli.FakeClient{GetRunHook: scriptHook(okStep("running"), waitStep{run: spendRun("issue", statusRecoveryWait)}, okStep("completed"))}
	_, stderr, code := runCLI(t, fakeEnv(fc), "run", "wait", "r1", "--interval", "1ms", "--timeout", "5s")
	if code != uzicli.ExitOK || !strings.Contains(stderr, "Anthropic API spend limit reached") || strings.Contains(stderr, "→ completed") {
		t.Errorf("default wait must stop on the hold: exit=%d stderr=%q", code, stderr)
	}
}

func TestRunWaitExplicitUntilIgnoresSpendLimitHold(t *testing.T) {
	fc := &uzicli.FakeClient{GetRunHook: scriptHook(waitStep{run: spendRun("issue", statusRecoveryWait)}, okStep("running"))}
	_, stderr, code := runCLI(t, fakeEnv(fc), "run", "wait", "r1", "--until", "running", "--interval", "1ms", "--timeout", "5s")
	if code != uzicli.ExitOK || !strings.Contains(stderr, "recovery_wait → running") {
		t.Errorf("explicit --until must not stop on the hold: exit=%d stderr=%q", code, stderr)
	}
}

func TestSpendLimitHoldRenders(t *testing.T) {
	r := spendRun("issue", statusRecoveryWait)
	item := apitypes.RunListItemDTO{RunDTO: r}
	if cell := runStatusCellAt(item, time.Now()); !strings.Contains(cell, "(spend limit: owner hold)") {
		t.Errorf("status cell = %q", cell)
	}
	fc := &uzicli.FakeClient{RunByID: map[string]apitypes.RunDTO{"r1": r}}
	out, _, code := runCLI(t, fakeEnv(fc), "run", "get", "r1")
	if code != uzicli.ExitOK || !strings.Contains(out, "SPEND_LIMIT") || !strings.Contains(out, "uzi run set-token r1 <label>") {
		t.Errorf("run get exit=%d lacks the row:\n%s", code, out)
	}
	if runBandOf(item) != bandNeedsYou || runStateWord(item) != "spend limit" {
		t.Errorf("band/word = %d/%q", runBandOf(item), runStateWord(item))
	}
	m := applyDetail(tuiTestModel(t, &uzicli.FakeClient{}, r.ID), r, nil)
	raw := m.View().Content
	assertNoRawControls(t, "spend limit", raw)
	if out := stripANSI(raw); !strings.Contains(out, "Anthropic API spend limit reached") || !strings.Contains(out, "uzi run resume r1") {
		t.Errorf("TUI detail lacks the hold:\n%s", out)
	}
	stale := apitypes.RunListItemDTO{RunDTO: spendRun("issue", "queued")}
	if runBandOf(stale) == bandNeedsYou || runStateWord(stale) == "spend limit" || strings.Contains(runStatusCellAt(stale, time.Now()), "spend limit") {
		t.Error("stale cause on a queued run must not render as a hold")
	}
}

func TestSteerStateSpendLimitNotTransient(t *testing.T) {
	r := spendRun("issue", statusRecoveryWait)
	got := steerStateForRun(apitypes.SteerInputDTO{Kind: "note"}, r)
	if strings.Contains(got, "transient") || !strings.Contains(got, "spend limit") {
		t.Errorf("steer state = %q", got)
	}
}
