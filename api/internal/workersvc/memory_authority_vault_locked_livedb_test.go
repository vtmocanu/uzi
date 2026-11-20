package workersvc

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// memory_authority_vault_locked_livedb_test.go pins issue #4492 over a real database: a poll that
// the owner's locked protected vault turns idle still records the worker's memory-authority report
// (held receipt or sticky loss), through the real claim path. Skipped without
// UZI_TEST_DATABASE_URL.

// lockOwner enrolls the pool owner in a protected vault and locks it.
func (p *poolFx) lockOwner() {
	p.t.Helper()
	enrollCodexProtectedVault(p.t, p.env, p.env.vlt, p.user, codexVaultLockedTestPassword)
	p.env.vlt.Lock(p.user)
	if !p.env.vlt.RequiresUnlock(p.env.ctx, p.user) {
		p.t.Fatal("setup: the owner's vault is not locked")
	}
}

func (p *poolFx) unlockOwner() {
	p.t.Helper()
	if err := p.env.vlt.Unlock(p.env.ctx, p.user, codexVaultLockedTestPassword); err != nil {
		p.t.Fatalf("Unlock: %v", err)
	}
	if p.env.vlt.RequiresUnlock(p.env.ctx, p.user) {
		p.t.Fatal("setup: the owner's vault is still locked")
	}
}

// claimAt is one run-lane claim with an explicit registration nonce and snapshot epoch.
func (m *memHeld) claimAt(nonce string, epoch int64, report string) (*ClaimPayload, error) {
	m.p.t.Helper()
	row, err := m.p.env.q.GetWorkerByID(m.p.env.ctx, m.id)
	if err != nil {
		m.p.t.Fatal(err)
	}
	snap := &ActiveSnapshot{SnapshotEpoch: epoch, RegisterNonce: nonce, Active: []ActiveRunEntry{}}
	return m.p.svc.ClaimReported(m.p.env.ctx, row, snap, MemoryAuthorityReport{Value: &report})
}

// mustIdle claims at the given epoch under the current registration and requires an idle poll.
func (m *memHeld) mustIdle(epoch int64, report string) {
	m.p.t.Helper()
	p, err := m.claimAt(m.nonce, epoch, report)
	if err != nil || p != nil {
		m.p.t.Fatalf("claim(%s@%d) = %v, %v; want idle, nil", report, epoch, p, err)
	}
}

func (m *memHeld) reregister() {
	m.p.t.Helper()
	_, nonce, err := m.p.svc.Register(m.p.env.ctx, store.Worker{ID: m.id, UserID: m.p.user}, "v1", "base", nil, nil, capableCaps(), nil)
	if err != nil {
		m.p.t.Fatalf("Register: %v", err)
	}
	m.nonce, m.epoch = nonce, 0
}

func (p *poolFx) lostAtOf(id uuid.UUID) (time.Time, bool) {
	p.t.Helper()
	var at *time.Time
	if err := p.env.pool.QueryRow(p.env.ctx, `SELECT memory_authority_lost_at FROM workers WHERE id=$1`, id).Scan(&at); err != nil {
		p.t.Fatal(err)
	}
	if at == nil {
		return time.Time{}, false
	}
	return *at, true
}

func (p *poolFx) snapshotEpochOf(id uuid.UUID) int64 {
	p.t.Helper()
	var e int64
	if err := p.env.pool.QueryRow(p.env.ctx, `SELECT snapshot_epoch FROM workers WHERE id=$1`, id).Scan(&e); err != nil {
		p.t.Fatal(err)
	}
	return e
}

func TestMemoryAuthorityVaultLockedHeldCreatesAndRefreshesReceiptLiveDB(t *testing.T) {
	p := newPoolFx(t)
	keeper := p.capableMember(5 * time.Hour)
	p.lockOwner()
	keeper.mustIdle(1, "held")
	first, ok := p.heldAt(keeper.id)
	if !ok || !first.Equal(p.clock.Truncate(time.Microsecond)) {
		t.Fatalf("held_at = %v (set %t), want the poll's clock %v", first, ok, p.clock)
	}
	p.advance(time.Minute)
	keeper.mustIdle(2, "held")
	second, ok := p.heldAt(keeper.id)
	if !ok || !second.After(first) {
		t.Fatalf("held_at %v -> %v (set %t), want refreshed", first, second, ok)
	}
	if _, lost := p.lostAtOf(keeper.id); lost {
		t.Fatal("a held report recorded a loss")
	}
	if e := p.snapshotEpochOf(keeper.id); e != 0 {
		t.Fatalf("snapshot_epoch = %d, want 0 (a locked poll applies no snapshot)", e)
	}
}

func TestMemoryAuthorityVaultLockedLostRecordsLossAndClearsReceiptLiveDB(t *testing.T) {
	p := newPoolFx(t)
	keeper := p.capableMember(5 * time.Hour)
	p.lockOwner()
	keeper.mustIdle(1, "held")
	if _, ok := p.heldAt(keeper.id); !ok {
		t.Fatal("setup: held receipt not stamped")
	}
	p.advance(time.Second)
	keeper.mustIdle(2, "lost")
	lost, ok := p.lostAtOf(keeper.id)
	if !ok || !lost.Equal(p.clock.Truncate(time.Microsecond)) {
		t.Fatalf("lost_at = %v (set %t), want the poll's clock %v", lost, ok, p.clock)
	}
	if _, ok := p.heldAt(keeper.id); ok {
		t.Fatal("a lost report left the earlier receipt set")
	}
}

// A delayed report from a replaced registration changes neither column on the locked path, and the
// poll still reports idle without an error.
func TestMemoryAuthorityVaultLockedStaleRegistrationChangesNothingLiveDB(t *testing.T) {
	p := newPoolFx(t)
	keeper := p.capableMember(5 * time.Hour)
	p.lockOwner()
	old := keeper.nonce
	keeper.reregister()
	for _, report := range []string{"held", "lost"} {
		pl, err := keeper.claimAt(old, 1, report)
		if err != nil || pl != nil {
			t.Fatalf("stale %s poll = %v, %v; want idle, nil", report, pl, err)
		}
		if _, ok := p.heldAt(keeper.id); ok {
			t.Fatalf("a delayed %s report from the old registration stamped the receipt", report)
		}
		if _, ok := p.lostAtOf(keeper.id); ok {
			t.Fatalf("a delayed %s report from the old registration recorded a loss", report)
		}
	}
	// Control: the current nonce stamps.
	pl, err := keeper.claimAt(keeper.nonce, 1, "held")
	if err != nil || pl != nil {
		t.Fatalf("current-nonce poll = %v, %v; want idle, nil", pl, err)
	}
	if _, ok := p.heldAt(keeper.id); !ok {
		t.Fatal("control: a current-nonce held poll did not stamp the receipt")
	}
}

// The loss is sticky on the locked path: later held reports neither clear it nor stamp a receipt.
func TestMemoryAuthorityVaultLockedLossIsStickyLiveDB(t *testing.T) {
	p := newPoolFx(t)
	keeper := p.capableMember(5 * time.Hour)
	p.lockOwner()
	keeper.mustIdle(11, "lost")
	lost, ok := p.lostAtOf(keeper.id)
	if !ok {
		t.Fatal("setup: loss not recorded")
	}
	p.advance(time.Minute)
	keeper.mustIdle(10, "held")
	keeper.mustIdle(12, "held")
	if got, ok := p.lostAtOf(keeper.id); !ok || !got.Equal(lost) {
		t.Fatalf("lost_at = %v (set %t), want %v kept", got, ok, lost)
	}
	if _, ok := p.heldAt(keeper.id); ok {
		t.Fatal("a held report after a loss stamped the receipt")
	}
	p.beatOnly(keeper.id)
	if p.ready(keeper.id) {
		t.Fatal("a keeper with a recorded loss must not be ready")
	}
}

// A loss recorded while locked survives the unlock: an ordinary accepted held claim neither clears
// it nor stamps a receipt, the sibling is not admitted, and only Register restores readiness.
func TestMemoryAuthorityVaultLockedLossSurvivesUnlockUntilRegisterLiveDB(t *testing.T) {
	p := newPoolFx(t)
	cand, keeper := p.capableMember(6*time.Hour), p.capableMember(5*time.Hour)
	// Register shapes the worker row (pool key), so both members go through it BEFORE the scenario
	// and the refusal below is about the keeper's loss, not about the members sitting in different
	// pools.
	cand.reregister()
	keeper.reregister()
	ck, cok := poolKey(p.poolRowOf(cand.id))
	kk, kok := poolKey(p.poolRowOf(keeper.id))
	if !cok || !kok || ck != kk {
		t.Fatalf("setup: members not in one pool (cand %q %t, keeper %q %t)", ck, cok, kk, kok)
	}
	p.env.exec(`UPDATE workers SET snapshot_epoch = 5 WHERE id = $1`, keeper.id)
	p.lockOwner()
	keeper.mustIdle(11, "lost")
	if e := p.snapshotEpochOf(keeper.id); e != 5 {
		t.Fatalf("snapshot_epoch = %d after a locked lost poll, want 5 unchanged", e)
	}
	lost, ok := p.lostAtOf(keeper.id)
	if !ok {
		t.Fatal("setup: loss not recorded")
	}
	p.unlockOwner()
	p.advance(time.Minute)
	p.beatOnly(cand.id, keeper.id)
	keeper.mustIdle(10, "held") // accepted: 10 > the stored epoch 5
	if e := p.snapshotEpochOf(keeper.id); e != 10 {
		t.Fatalf("snapshot_epoch = %d, want 10 (the unlocked claim was accepted)", e)
	}
	if got, ok := p.lostAtOf(keeper.id); !ok || !got.Equal(lost) {
		t.Fatalf("lost_at = %v (set %t), want %v unchanged", got, ok, lost)
	}
	if _, ok := p.heldAt(keeper.id); ok {
		t.Fatal("an unlocked held claim after a loss stamped the receipt")
	}
	if p.ready(keeper.id) {
		t.Fatal("a keeper with a recorded loss must not be ready")
	}
	if d := p.surplusWhy(cand.id); d.Action == decisionAutoSurplus {
		t.Fatalf("decision = %+v, want no surplus while the only keeper has a recorded loss", d)
	}
	if n := p.pass(cand.id, keeper.id); n != 0 {
		t.Fatalf("pass admitted %d, want 0", n)
	}
	keeper.reregister()
	if k, ok := poolKey(p.poolRowOf(keeper.id)); !ok || k != ck {
		t.Fatalf("keeper left the pool after Register: %q %t, want %q", k, ok, ck)
	}
	p.beatOnly(cand.id, keeper.id)
	keeper.claim("held") // epoch 1 of the new registration
	if _, ok := p.lostAtOf(keeper.id); ok {
		t.Fatal("Register did not clear the loss")
	}
	if _, ok := p.heldAt(keeper.id); !ok {
		t.Fatal("a held claim after Register did not stamp the receipt")
	}
	if !p.ready(keeper.id) {
		t.Fatal("the keeper must be ready again after Register and a fresh held claim")
	}
	if d := p.surplusWhy(cand.id); d.Action != decisionAutoSurplus || d.Keeper != keeper.id {
		t.Fatalf("decision = %+v, want auto_surplus with keeper %s", d, keeper.id)
	}
}

// A queued run of the locked owner stays untouched: the locked poll returns no payload, claims
// nothing and writes only the memory-authority columns.
func TestMemoryAuthorityVaultLockedPollTouchesNoRunLiveDB(t *testing.T) {
	p := newPoolFx(t)
	keeper := p.capableMember(5 * time.Hour)
	run := p.runOf(runkind.Issue, "queued", nil)
	p.lockOwner()

	type runState struct {
		status     string
		worker     *uuid.UUID
		generation int64
		updatedAt  time.Time
	}
	readRun := func() runState {
		t.Helper()
		var s runState
		if err := p.env.pool.QueryRow(p.env.ctx, `SELECT status, worker_id, claim_generation, updated_at FROM runs WHERE id=$1`, run).
			Scan(&s.status, &s.worker, &s.generation, &s.updatedAt); err != nil {
			t.Fatal(err)
		}
		return s
	}
	activeRuns := func() int {
		t.Helper()
		var n int
		if err := p.env.pool.QueryRow(p.env.ctx, `SELECT count(*) FROM worker_active_runs WHERE worker_id=$1`, keeper.id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before, beforeActive, beforeEpoch := readRun(), activeRuns(), p.snapshotEpochOf(keeper.id)
	for i, report := range []string{"held", "lost"} {
		keeper.mustIdle(int64(i+1), report)
		if got := readRun(); got != before {
			t.Fatalf("after a locked %s poll the run is %+v, want %+v", report, got, before)
		}
		if n := activeRuns(); n != beforeActive {
			t.Fatalf("worker_active_runs = %d, want %d", n, beforeActive)
		}
		if e := p.snapshotEpochOf(keeper.id); e != beforeEpoch {
			t.Fatalf("snapshot_epoch = %d, want %d", e, beforeEpoch)
		}
		if i == 0 {
			if _, ok := p.heldAt(keeper.id); !ok {
				t.Fatal("a locked held poll did not stamp the receipt")
			}
		}
	}
	if _, ok := p.lostAtOf(keeper.id); !ok {
		t.Fatal("a locked lost poll did not record the loss")
	}
}

// Two idle capable workers of a locked owner that both report held make the surplus member
// admissible, exactly as for an unlocked owner.
func TestMemoryAuthorityVaultLockedPoolAdmitsSurplusLiveDB(t *testing.T) {
	p := newPoolFx(t)
	cand, keeper := p.capableMember(6*time.Hour), p.capableMember(5*time.Hour)
	p.lockOwner()
	p.advance(time.Second)
	p.beatOnly(cand.id, keeper.id)
	cand.mustIdle(1, "held")
	keeper.mustIdle(1, "held")
	if d := p.surplusWhy(cand.id); d.Action != decisionAutoSurplus || d.Keeper != keeper.id {
		t.Fatalf("decision = %+v, want auto_surplus with keeper %s", d, keeper.id)
	}
	if n, err := p.runPass(); err != nil || n != 1 {
		t.Fatalf("pass = %d, %v; want 1", n, err)
	}
	p.wantStates(map[uuid.UUID]string{cand.id: "preparing", keeper.id: "none"})
}

// The locked path persists nothing without a fenceable snapshot, for cross-check claims and for
// ephemeral workers.
func TestMemoryAuthorityVaultLockedNoOpsLiveDB(t *testing.T) {
	assertNothing := func(t *testing.T, p *poolFx, id uuid.UUID) {
		t.Helper()
		if _, ok := p.heldAt(id); ok {
			t.Fatal("a receipt was stamped")
		}
		if _, ok := p.lostAtOf(id); ok {
			t.Fatal("a loss was recorded")
		}
	}
	row := func(p *poolFx, id uuid.UUID) store.Worker {
		t.Helper()
		w, err := p.env.q.GetWorkerByID(p.env.ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return w
	}
	for _, report := range []string{"held", "lost"} {
		report := report
		t.Run("nil snapshot "+report, func(t *testing.T) {
			p := newPoolFx(t)
			m := p.capableMember(5 * time.Hour)
			p.lockOwner()
			if pl, err := p.svc.ClaimReported(p.env.ctx, row(p, m.id), nil, MemoryAuthorityReport{Value: &report}); err != nil || pl != nil {
				t.Fatalf("claim = %v, %v; want idle, nil", pl, err)
			}
			assertNothing(t, p, m.id)
		})
		t.Run("empty nonce "+report, func(t *testing.T) {
			p := newPoolFx(t)
			m := p.capableMember(5 * time.Hour)
			p.lockOwner()
			snap := &ActiveSnapshot{SnapshotEpoch: 1, RegisterNonce: "", Active: []ActiveRunEntry{}}
			if pl, err := p.svc.ClaimReported(p.env.ctx, row(p, m.id), snap, MemoryAuthorityReport{Value: &report}); err != nil || pl != nil {
				t.Fatalf("claim = %v, %v; want idle, nil", pl, err)
			}
			assertNothing(t, p, m.id)
		})
		t.Run("cross check "+report, func(t *testing.T) {
			p := newPoolFx(t)
			m := p.capableMember(5 * time.Hour)
			p.lockOwner()
			snap := &ActiveSnapshot{SnapshotEpoch: 1, RegisterNonce: m.nonce, Active: []ActiveRunEntry{}}
			if pl, err := p.svc.ClaimCrossCheck(p.env.ctx, row(p, m.id), snap); err != nil || pl != nil {
				t.Fatalf("cross-check claim = %v, %v; want idle, nil", pl, err)
			}
			assertNothing(t, p, m.id)
		})
		t.Run("ephemeral worker "+report, func(t *testing.T) {
			p := newPoolFx(t)
			m := p.capableMember(5 * time.Hour)
			p.lockOwner()
			w := row(p, m.id)
			w.Ephemeral = true
			snap := &ActiveSnapshot{SnapshotEpoch: 1, RegisterNonce: m.nonce, Active: []ActiveRunEntry{}}
			if pl, err := p.svc.ClaimReported(p.env.ctx, w, snap, MemoryAuthorityReport{Value: &report}); err != nil || pl != nil {
				t.Fatalf("claim = %v, %v; want idle, nil", pl, err)
			}
			assertNothing(t, p, m.id)
		})
	}
}
