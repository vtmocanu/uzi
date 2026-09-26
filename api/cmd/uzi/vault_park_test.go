package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// vaultParkRun is a recovery_wait run parked because a Codex credential refresh or release
// found its owner's vault locked (issue #1766). Forge counters are set on purpose: this cause
// must never borrow the forge wording or park count. The retry stamp is the real next retry the
// DTO carries for this cause.
func vaultParkRun(id string) apitypes.RunDTO {
	cause := vaultLockedCause
	retry := time.Now().Add(10 * time.Minute)
	return apitypes.RunDTO{
		ID: id, Kind: "issue", Status: statusRecoveryWait, IssueTitle: "parked", Health: "ok",
		RecoveryWaitCause: &cause, RecoveryRetryNotBefore: &retry, ForgeParkCount: 2, ForgeParkMax: 5,
	}
}

// TestSteerStateVaultLocked: a steer row on a vault_locked park names the vault unlock, in
// the "waiting for vault unlock" wording the other vault_locked surfaces use, and not the generic
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
// prints its one-shot stderr notice naming the vault unlock and the next retry time, not the
// transient-recovery or forge wording, never promising an instant resume, never calling the
// park "paused" (a different status) and never telling the follower (who may be an admin) to
// unlock "your" vault. Reddening mutation: drop the vaultParkLine branch in run_get.go (the
// generic "recovering" notice prints instead).
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
	want := "run r1 waiting for vault unlock — the run owner's vault was locked when this Codex run needed its credential; once the vault is unlocked it resumes at its next retry (" +
		parked.RecoveryRetryNotBefore.Local().Format("15:04") + "); still following\n"
	if n := strings.Count(stderr, want); n != 1 {
		t.Errorf("vault_locked notice appeared %d times, want exactly 1:\n%s", n, stderr)
	}
	if strings.Contains(stderr, "paused") || strings.Contains(stderr, "your vault") {
		t.Errorf("the vault_locked notice says \"paused\" or addresses \"your vault\":\n%s", stderr)
	}
	if strings.Contains(stderr, "transient interruption") || strings.Contains(stderr, "waiting for the forge") {
		t.Errorf("a vault_locked park got the transient-recovery or forge wording:\n%s", stderr)
	}
	if strings.Contains(out.String(), "vault") {
		t.Errorf("the park notice reached STDOUT:\n%s", out.String())
	}
}

// TestVaultParkLine pins the shared owner-neutral sentence: the retry clause carries the next
// retry as local HH:MM, is dropped without a stamp, and any other run gets "".
func TestVaultParkLine(t *testing.T) {
	r := vaultParkRun("r1")
	want := "waiting for vault unlock — the run owner's vault was locked when this Codex run needed its credential; once the vault is unlocked it resumes at its next retry (" +
		r.RecoveryRetryNotBefore.Local().Format("15:04") + ")"
	if got := vaultParkLine(r); got != want {
		t.Errorf("vaultParkLine = %q, want %q", got, want)
	}
	r.RecoveryRetryNotBefore = nil
	if got := vaultParkLine(r); !strings.HasSuffix(got, "it resumes at its next retry") {
		t.Errorf("vaultParkLine(no stamp) = %q, want the retry clause without a time", got)
	}
	if got := vaultParkLine(apitypes.RunDTO{Status: statusRecoveryWait}); got != "" {
		t.Errorf("vaultParkLine(untyped park) = %q, want \"\"", got)
	}
}

// TestRunTablesVaultStatusCell drives the real `uzi run list` and `uzi admin runs`: a
// vault_locked park's STATUS cell reads "recovery_wait (waiting for vault unlock)", and an
// unparked row gains nothing. Reddening mutation: drop the isVaultLockedPark arm in
// runStatusCell.
func TestRunTablesVaultStatusCell(t *testing.T) {
	rows := []apitypes.RunListItemDTO{
		{RunDTO: vaultParkRun("vault-1")},
		{RunDTO: apitypes.RunDTO{ID: "plain-2", Kind: "issue", Status: "running", IssueTitle: "plain"}},
	}
	for _, args := range [][]string{{"run", "list"}, {"admin", "runs"}} {
		fc := &uzicli.FakeClient{Runs: rows, AdminRuns: rows}
		out, stderr, code := runCLI(t, fakeEnv(fc), args...)
		if code != uzicli.ExitOK {
			t.Fatalf("%v: exit = %d, stderr:\n%s", args, code, stderr)
		}
		if line := lineWith(t, out, "vault-1"); !strings.Contains(line, "recovery_wait (waiting for vault unlock)") {
			t.Errorf("%v: vault_locked row lacks the vault suffix: %q", args, line)
		}
		if line := lineWith(t, out, "plain-2"); strings.Contains(line, "vault") {
			t.Errorf("%v: an unparked row gained a vault suffix: %q", args, line)
		}
	}
}

// TestRenderRunDetailVaultRow: `uzi run get` prints the VAULT row (with the next retry) for a
// vault_locked park and no such row for any other recovery park. Reddening mutation: drop the
// VAULT row in renderRunDetail.
func TestRenderRunDetailVaultRow(t *testing.T) {
	render := func(r apitypes.RunDTO) string {
		var buf bytes.Buffer
		if err := renderRunDetail(uzicli.NewPrinter(&buf, false, false, true, false), r); err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}
	parked := vaultParkRun("r1")
	out := render(parked)
	if line := lineWith(t, out, "VAULT"); !strings.Contains(line, vaultParkLine(parked)) {
		t.Errorf("VAULT row = %q, want it to carry %q", line, vaultParkLine(parked))
	}
	if out := render(apitypes.RunDTO{ID: "r2", Kind: "issue", Status: statusRecoveryWait}); strings.Contains(out, "VAULT") {
		t.Errorf("an untyped recovery park gained a VAULT row:\n%s", out)
	}
}

// TestVaultBandCountsOwnVaultLockedParks (N1): the TUI's amber band counts the viewer's own
// vault_locked recovery parks, not only queued runs whose health reason names the lock, on the
// own board and (scoped to the viewer's email) on the admin board. Reddening mutation: drop the
// isVaultLockedPark term in ownParkedOnVaultCount (the band falls back to the tier-1 hint).
func TestVaultBandCountsOwnVaultLockedParks(t *testing.T) {
	const self, other = "me@x.io", "stranger@x.io"
	own := apitypes.RunListItemDTO{RunDTO: vaultParkRun("r-own")}
	m := vaultBoardModel(t, false, self, true, []apitypes.RunListItemDTO{own})
	if got := m.ownParkedOnVaultCount(); got != 1 {
		t.Errorf("own board: ownParkedOnVaultCount = %d, want 1", got)
	}
	if plain := stripANSI(m.vaultIndicatorLine()); !strings.Contains(plain, "VAULT LOCKED") || !strings.Contains(plain, "1 run parked") {
		t.Errorf("own vault_locked park did not escalate the band; got %q", plain)
	}
	// Mixed with a queued run parked on the health reason: both count.
	m = vaultBoardModel(t, false, self, true, []apitypes.RunListItemDTO{own, vaultParkedRun("r-q", serverVaultLockedReason, "")})
	if got := m.ownParkedOnVaultCount(); got != 2 {
		t.Errorf("own board, park + queued: ownParkedOnVaultCount = %d, want 2", got)
	}
	// Admin board: only the viewer's own park counts.
	mine, theirs := own, apitypes.RunListItemDTO{RunDTO: vaultParkRun("r-theirs")}
	mine.OwnerEmail, theirs.OwnerEmail = sptr(self), sptr(other)
	m = vaultBoardModel(t, true, self, true, []apitypes.RunListItemDTO{mine, theirs})
	if got := m.ownParkedOnVaultCount(); got != 1 {
		t.Errorf("admin board: ownParkedOnVaultCount = %d, want 1 (the stranger's park excluded)", got)
	}
	// Any other recovery park does not count.
	m = vaultBoardModel(t, false, self, true, []apitypes.RunListItemDTO{{RunDTO: apitypes.RunDTO{ID: "r-x", Kind: "issue", Status: statusRecoveryWait}}})
	if got := m.ownParkedOnVaultCount(); got != 0 {
		t.Errorf("untyped recovery park counted: ownParkedOnVaultCount = %d, want 0", got)
	}
}
