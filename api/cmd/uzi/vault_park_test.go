package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// vaultParkRun is a recovery_wait run parked because its owner's vault locked while it was
// saving its work (issue #1766). A retry stamp and forge counters are set on purpose: this
// cause must never borrow the forge wording.
func vaultParkRun(id string) apitypes.RunDTO {
	cause := vaultLockedCause
	retry := time.Now().Add(10 * time.Minute)
	return apitypes.RunDTO{
		ID: id, Kind: "issue", Status: statusRecoveryWait, IssueTitle: "parked", Health: "ok",
		RecoveryWaitCause: &cause, RecoveryRetryNotBefore: &retry, ForgeParkCount: 2, ForgeParkMax: 5,
	}
}

// TestSteerStateVaultLocked: a steer row on a vault_locked park names the vault unlock, in
// the "waiting for vault unlock" wording the web runs list uses, and not the generic
// transient interruption. Reddening mutation: drop the vaultLockedCause arm in steerState.
func TestSteerStateVaultLocked(t *testing.T) {
	consumed := time.Now()
	if got, want := steerState(kindFollowUp, nil, nil, statusRecoveryWait, vaultLockedCause), "queued (run waiting for vault unlock)"; got != want {
		t.Errorf("steerState(unconsumed, vault_locked) = %q, want %q", got, want)
	}
	if got, want := steerState(kindFollowUp, &consumed, nil, statusRecoveryWait, vaultLockedCause), "delivered (run waiting for vault unlock)"; got != want {
		t.Errorf("steerState(consumed, vault_locked) = %q, want %q", got, want)
	}
	// The cause only matters on a recovery_wait run.
	if got := steerState(kindFollowUp, nil, nil, "running", vaultLockedCause); got != "queued" {
		t.Errorf("steerState(running, vault_locked) = %q, want %q", got, "queued")
	}
}

// TestStateGlyphWordVaultLocked pins the TUI token: a vault_locked park reads "vault wait" in
// the wait family (same ~ glyph and wait colour as its siblings), distinct from the forge,
// Codex and generic recovery words, and it fits the board's status-word cell. Reddening
// mutation: drop the vaultLockedCause arm in stateGlyphWord (it falls back to "recovery wait").
func TestStateGlyphWordVaultLocked(t *testing.T) {
	glyph, word := stateGlyphWord(statusRecoveryWait, "", false, false, "", vaultLockedCause)
	if glyph != "~" || word != "vault wait" {
		t.Errorf("vault_locked token = (%q, %q), want (~, vault wait)", glyph, word)
	}
	if n := len([]rune(word)); n >= boardStatusWordWidth {
		t.Errorf("vault_locked word %q is %d runes, does not fit boardStatusWordWidth %d", word, n, boardStatusWordWidth)
	}
	// The board row and its `/` filter read the same word off the run's cause.
	if got := runStateWord(apitypes.RunListItemDTO{RunDTO: vaultParkRun("r1")}); got != "vault wait" {
		t.Errorf("runStateWord(vault_locked) = %q, want %q", got, "vault wait")
	}
	p := newPalette(true)
	tok := p.runStateToken(vaultParkRun("r1"), false)
	if tok.glyph != "~" || tok.word != "vault wait" || tok.color != p.wait {
		t.Errorf("runStateToken(vault_locked) = (%q, %q, %v), want (~, vault wait, wait colour)", tok.glyph, tok.word, tok.color)
	}
}

// TestIsVaultLockedPark keys off both status and cause.
func TestIsVaultLockedPark(t *testing.T) {
	if !isVaultLockedPark(vaultParkRun("r1")) {
		t.Error("isVaultLockedPark(recovery_wait + vault_locked) = false, want true")
	}
	running := vaultParkRun("r1")
	running.Status = "running"
	if isVaultLockedPark(running) {
		t.Error("isVaultLockedPark(running + vault_locked) = true, want false")
	}
	if isVaultLockedPark(apitypes.RunDTO{Status: statusRecoveryWait}) {
		t.Error("isVaultLockedPark(recovery_wait + null cause) = true, want false")
	}
}

// TestRunLogsFollowVaultLockedNotice: `run logs --follow` rides out a vault_locked park and
// prints its one-shot stderr notice naming the vault unlock and the next retry, not the
// transient-recovery or forge wording, and never promising an instant resume. Reddening
// mutation: drop the isVaultLockedPark branch in run_get.go (the generic "recovering"
// notice prints instead).
func TestRunLogsFollowVaultLockedNotice(t *testing.T) {
	t.Setenv("UZI_URL", "")
	t.Setenv("UZI_TOKEN", "")
	old := logsPollInterval
	logsPollInterval = time.Millisecond
	defer func() { logsPollInterval = old }()

	parked := vaultParkRun("r1")
	pf := &codexParkFake{
		FakeClient: &uzicli.FakeClient{LogsByID: map[string][]apitypes.MessageDTO{
			"r1": {{Seq: 1, Kind: "assistant", Payload: []byte(`{"text":"hi"}`)}},
		}},
		seq: []apitypes.RunDTO{
			{ID: "r1", Status: "running"},
			parked, parked, parked,
			{ID: "r1", Status: "completed"},
		},
	}
	var out, errBuf bytes.Buffer
	env := fakeEnv(pf)
	env.Stdout, env.Stderr = &out, &errBuf
	done := make(chan int, 1)
	go func() { done <- Main(env, []string{"run", "logs", "r1", "--follow"}) }()
	select {
	case code := <-done:
		if code != uzicli.ExitOK {
			t.Fatalf("--follow exit = %d, want 0", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run logs --follow hung on a vault_locked park")
	}
	stderr := errBuf.String()
	const want = "run r1 paused — waiting for vault unlock; still following, unlock your vault and it resumes at its next retry"
	if n := strings.Count(stderr, want); n != 1 {
		t.Errorf("vault_locked notice appeared %d times, want exactly 1:\n%s", n, stderr)
	}
	if strings.Contains(stderr, "transient interruption") || strings.Contains(stderr, "waiting for the forge") {
		t.Errorf("a vault_locked park got the transient-recovery or forge wording:\n%s", stderr)
	}
	if strings.Contains(out.String(), "vault") {
		t.Errorf("the park notice reached STDOUT:\n%s", out.String())
	}
}
