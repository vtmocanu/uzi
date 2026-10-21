package main

import (
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// issue #3194: a `--require-base` run whose kept work carries no attestation is held
// (paused, base_attestation_missing). Resume and extend are refused server-side, so every CLI
// surface names the recovery verbs instead, and a bare `run wait` stops on it rather than waiting
// on a hold nothing automatic will ever leave.

func TestHoldRowBaseAttestation(t *testing.T) {
	now := time.Date(2026, 10, 21, 15, 12, 0, 0, time.UTC)
	run := heldRun("r1", holdBaseAttestation, now.Add(-70*time.Minute))
	want := "base not attested · parked 14:02 (1h10m) · 4/7 milestones · recover the kept work: " +
		"uzi run recovery r1, uzi run export r1 --output <path>, or start a successor run"
	if row := holdRow(run, now); row == nil || row[1] != want {
		t.Errorf("holdRow = %v, want [HOLD %q]", row, want)
	}
	notice := pausedHoldNotice(run)
	for _, part := range []string{"not attested against the planned base", "uzi run recovery r1", "uzi run export r1"} {
		if !strings.Contains(notice, part) {
			t.Errorf("pausedHoldNotice = %q, want it to contain %q", notice, part)
		}
	}
	for _, offer := range []string{"uzi run resume", "uzi run extend"} {
		if strings.Contains(strings.Join(holdRow(run, now), " "), offer) || strings.Contains(notice, offer) {
			t.Errorf("the hold must not offer %q (the server refuses it)", offer)
		}
	}
}

func TestRunGetBaseAttestationHold(t *testing.T) {
	run := heldRun("r1", holdBaseAttestation, time.Now().Add(-5*time.Minute))
	fc := &uzicli.FakeClient{RunByID: map[string]apitypes.RunDTO{"r1": run}}
	out, _, code := runCLI(t, fakeEnv(fc), "run", "get", "r1")
	if code != uzicli.ExitOK {
		t.Fatalf("run get exit = %d, want 0", code)
	}
	if !strings.Contains(out, "HOLD") || !strings.Contains(out, "base not attested") || !strings.Contains(out, "uzi run export r1") {
		t.Errorf("run get lacks the base_attestation_missing HOLD row:\n%s", out)
	}
}

func TestRunWaitStopsOnBaseAttestationHold(t *testing.T) {
	held := heldRun("r1", holdBaseAttestation, time.Now())
	fc := &uzicli.FakeClient{GetRunHook: scriptHook(okStep("running"), waitStep{run: held})}
	_, stderr, code := runCLI(t, fakeEnv(fc), "run", "wait", "r1", "--interval", "1ms", "--timeout", "2s")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0: a bare wait stops on a hold nothing automatic leaves (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stderr, "uzi run recovery r1") {
		t.Errorf("stderr should name the recovery verbs, got:\n%s", stderr)
	}
	// An explicit --until does not widen to the hold: it keeps waiting until its timeout.
	fc = &uzicli.FakeClient{GetRunHook: scriptHook(waitStep{run: held})}
	_, _, code = runCLI(t, fakeEnv(fc), "run", "wait", "r1", "--until", "completed", "--interval", "1ms", "--timeout", "50ms")
	if code != uzicli.ExitTimeout {
		t.Errorf("explicit --until completed exit = %d, want %d (timeout)", code, uzicli.ExitTimeout)
	}
}
