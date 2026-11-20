package workersvc

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// memory_authority_held_livedb_test.go pins issue #4395 PRD D16 over a real database: a worker with
// both memory capabilities is a ready keeper only while its last fenced held report on a
// snapshot-carrying run-lane claim is fresh (workers.memory_authority_held_at). Skipped without
// UZI_TEST_DATABASE_URL.

// memHeld seeds a pool member of both capabilities with a registration nonce, as Register leaves
// it, and claims on it through the real run-lane path.
type memHeld struct {
	p     *poolFx
	id    uuid.UUID
	nonce string
	epoch int64
}

func (p *poolFx) capableMember(ago time.Duration) *memHeld {
	p.t.Helper()
	id := p.member(ago)
	return p.makeCapable(id)
}

func (p *poolFx) makeCapable(id uuid.UUID) *memHeld {
	p.t.Helper()
	m := &memHeld{p: p, id: id, nonce: "nonce-" + uuid.NewString()}
	p.env.exec(`UPDATE workers SET protocol_capabilities = $2, snapshot_register_nonce = $3, snapshot_epoch = 0 WHERE id = $1`,
		id, capableCaps(), m.nonce)
	return m
}

// claim is one run-lane claim carrying a snapshot of the current registration.
func (m *memHeld) claim(report string) {
	m.p.t.Helper()
	m.epoch++
	row, err := m.p.env.q.GetWorkerByID(m.p.env.ctx, m.id)
	if err != nil {
		m.p.t.Fatal(err)
	}
	snap := &ActiveSnapshot{SnapshotEpoch: m.epoch, RegisterNonce: m.nonce, Active: []ActiveRunEntry{}}
	if _, err := m.p.svc.ClaimReported(m.p.env.ctx, row, snap, MemoryAuthorityReport{Value: &report}); err != nil {
		m.p.t.Fatalf("claim(%s): %v", report, err)
	}
}

// claimBodyless is a run-lane claim without a snapshot: it carries no registration identity.
func (m *memHeld) claimBodyless(report string) {
	m.p.t.Helper()
	row, err := m.p.env.q.GetWorkerByID(m.p.env.ctx, m.id)
	if err != nil {
		m.p.t.Fatal(err)
	}
	if _, err := m.p.svc.ClaimReported(m.p.env.ctx, row, nil, MemoryAuthorityReport{Value: &report}); err != nil {
		m.p.t.Fatalf("bodyless claim(%s): %v", report, err)
	}
}

func (p *poolFx) heldAt(id uuid.UUID) (time.Time, bool) {
	p.t.Helper()
	var at *time.Time
	if err := p.env.pool.QueryRow(p.env.ctx, `SELECT memory_authority_held_at FROM workers WHERE id=$1`, id).Scan(&at); err != nil {
		p.t.Fatal(err)
	}
	if at == nil {
		return time.Time{}, false
	}
	return *at, true
}

func (p *poolFx) poolRowOf(id uuid.UUID) poolRow {
	p.t.Helper()
	rows, err := p.env.q.GetOwnerHibernationPoolInput(p.env.ctx, p.user)
	if err != nil {
		p.t.Fatal(err)
	}
	for _, r := range rows {
		if r.ID == id {
			return r
		}
	}
	p.t.Fatalf("worker %s is not in the owner's pool input", id)
	return poolRow{}
}

func (p *poolFx) ready(id uuid.UUID) bool {
	p.t.Helper()
	return memberReady(p.poolRowOf(id), p.clock, p.svc.p.WorkerHeartbeatStale)
}

func (p *poolFx) surplusWhy(cand uuid.UUID) poolDecision {
	p.t.Helper()
	rows, err := p.env.q.GetOwnerHibernationPoolInput(p.env.ctx, p.user)
	if err != nil {
		p.t.Fatal(err)
	}
	return decideSurplus(rows, cand, p.clock, p.svc.p.WorkerHeartbeatStale, p.clock.Add(-poolSurplusIdle), true)
}

// A capable worker that registered and claimed only without a snapshot is not ready: its memory
// authority is unknown, so its idle surplus sibling is not hibernated on its account and the
// decision names the reason.
func TestHibernationSurplusMemoryAuthorityUnknownKeeperLiveDB(t *testing.T) {
	p := newPoolFx(t)
	cand, keeper := p.capableMember(6*time.Hour), p.capableMember(5*time.Hour)
	keeper.claimBodyless("held")
	cand.claimBodyless("held")
	if _, ok := p.heldAt(keeper.id); ok {
		t.Fatal("a snapshot-less held claim stamped memory_authority_held_at")
	}
	if p.ready(keeper.id) {
		t.Fatal("a capable worker with no held receipt must not be ready")
	}
	if d := p.surplusWhy(cand.id); d.Action != decisionNone || d.Why != noneMemoryAuthUnsure {
		t.Fatalf("decision = %+v, want none/%s", d, noneMemoryAuthUnsure)
	}
	if n, err := p.runPass(); err != nil || n != 0 {
		t.Fatalf("pass = %d, %v; want 0", n, err)
	}
	p.wantStates(map[uuid.UUID]string{cand.id: "none", keeper.id: "none"})
}

// One fenced held snapshot-carrying claim stamps the receipt with the claim's clock; the worker is
// then ready and its sibling is admitted.
func TestHibernationSurplusMemoryAuthorityHeldReceiptLiveDB(t *testing.T) {
	p := newPoolFx(t)
	cand, keeper := p.capableMember(6*time.Hour), p.capableMember(5*time.Hour)
	if _, ok := p.heldAt(keeper.id); ok {
		t.Fatal("setup: held_at set before any claim")
	}
	p.advance(1500 * time.Millisecond)
	p.beatOnly(cand.id, keeper.id)
	keeper.claim("held")
	got, ok := p.heldAt(keeper.id)
	if !ok || !got.Equal(p.clock.Truncate(time.Microsecond)) {
		t.Fatalf("held_at = %v (set %t), want the claim's clock %v", got, ok, p.clock)
	}
	if !p.ready(keeper.id) {
		t.Fatal("a capable worker with a fresh held receipt must be ready")
	}
	if n, err := p.runPass(); err != nil || n != 1 {
		t.Fatalf("pass = %d, %v; want 1", n, err)
	}
	p.wantStates(map[uuid.UUID]string{cand.id: "preparing", keeper.id: "none"})
}

// The receipt expires one heartbeat-staleness window after the held claim: snapshot-less polls with
// fresh heartbeats do not refresh it, so the worker stops being a keeper.
func TestHibernationSurplusMemoryAuthorityReceiptExpiresLiveDB(t *testing.T) {
	p := newPoolFx(t)
	cand, keeper := p.capableMember(6*time.Hour), p.capableMember(5*time.Hour)
	keeper.claim("held")
	held, _ := p.heldAt(keeper.id)
	stale := p.svc.p.WorkerHeartbeatStale
	p.advance(stale - time.Second) // still inside the window
	p.beatOnly(cand.id, keeper.id)
	keeper.claimBodyless("held")
	if !p.ready(keeper.id) {
		t.Fatal("setup: the receipt is still fresh inside the window")
	}
	p.advance(2 * time.Second) // past held_at + stale
	p.beatOnly(cand.id, keeper.id)
	keeper.claimBodyless("lost")
	keeper.claimBodyless("held")
	if again, _ := p.heldAt(keeper.id); !again.Equal(held) {
		t.Fatalf("snapshot-less polls moved held_at: %v -> %v", held, again)
	}
	if p.ready(keeper.id) {
		t.Fatal("a worker whose receipt is older than the staleness window must not be ready")
	}
	if n, err := p.runPass(); err != nil || n != 0 {
		t.Fatalf("pass = %d, %v; want 0", n, err)
	}
	p.wantStates(map[uuid.UUID]string{cand.id: "none", keeper.id: "none"})
}

// Fenced held claims keep refreshing the receipt: with w the staleness window, held claims at t0,
// t0+0.6w and t0+1.2w advance held_at each time, and at t0+1.5w (past 1.5 windows from t0) the
// worker is still ready and is the keeper that admits its sibling.
func TestHibernationSurplusMemoryAuthorityReceiptRefreshLiveDB(t *testing.T) {
	p := newPoolFx(t)
	cand, keeper := p.capableMember(6*time.Hour), p.capableMember(5*time.Hour)
	w := p.svc.p.WorkerHeartbeatStale
	var elapsed time.Duration
	var prevAt time.Time
	for i, off := range []time.Duration{0, w * 6 / 10, w * 12 / 10} {
		p.advance(off - elapsed)
		elapsed = off
		p.beatOnly(cand.id, keeper.id)
		keeper.claim("held")
		got, ok := p.heldAt(keeper.id)
		if !ok || (i > 0 && !got.After(prevAt)) {
			t.Fatalf("claim %d: held_at = %v (set %t), want it advanced past %v", i, got, ok, prevAt)
		}
		prevAt = got
	}
	p.advance(w * 3 / 10) // t0 + 1.5w
	p.beatOnly(cand.id, keeper.id)
	if !p.ready(keeper.id) {
		t.Fatal("a worker refreshed within the window must stay ready past 1.5 windows from the first claim")
	}
	if n, err := p.runPass(); err != nil || n != 1 {
		t.Fatalf("pass = %d, %v; want 1", n, err)
	}
	p.wantStates(map[uuid.UUID]string{cand.id: "preparing", keeper.id: "none"})
}

// A lost report clears the receipt; a later held claim does not stamp it again (issue #4492: a
// loss is sticky until Register).
func TestMemoryAuthorityLostClearsHeldReceiptLiveDB(t *testing.T) {
	p := newPoolFx(t)
	keeper := p.capableMember(5 * time.Hour)
	keeper.claim("held")
	if _, ok := p.heldAt(keeper.id); !ok {
		t.Fatal("setup: held receipt not stamped")
	}
	p.advance(time.Second)
	keeper.claim("lost")
	if _, ok := p.heldAt(keeper.id); ok {
		t.Fatal("a lost report left memory_authority_held_at set")
	}
	p.advance(time.Second)
	p.beatOnly(keeper.id)
	keeper.claim("held")
	if _, ok := p.heldAt(keeper.id); ok {
		t.Fatal("a held claim after a loss stamped the receipt; the loss is sticky until Register")
	}
	if p.ready(keeper.id) {
		t.Fatal("a keeper with a recorded loss must not be ready")
	}
}

// A delayed held report from an older registration, arriving after Register, leaves the receipt
// NULL and the worker not ready; the writer query matches nothing for the old nonce.
func TestMemoryAuthorityStaleHeldReportLeavesReceiptNullLiveDB(t *testing.T) {
	t.Run("receipt", staleHeldReceiptNull)
	t.Run("readiness", staleHeldNotReady)
}

// staleHeldNotReady: a keeper that held fresh under registration 1 is ready; after it
// re-registers, a delayed held report from registration 1 is refused, the keeper is not ready, and
// its idle surplus sibling is not admitted on its account. A held claim of the new registration
// makes it ready again, so the not-ready state came from the missing receipt alone.
func staleHeldNotReady(t *testing.T) {
	p := newPoolFx(t)
	cand, keeper := p.capableMember(6*time.Hour), p.capableMember(5*time.Hour)
	register := func(m *memHeld) {
		t.Helper()
		_, nonce, err := p.svc.Register(p.env.ctx, store.Worker{ID: m.id, UserID: p.user}, "v1", "base", nil, nil, capableCaps(), nil)
		if err != nil {
			t.Fatalf("Register: %v", err)
		}
		m.nonce, m.epoch = nonce, 0
	}
	register(cand)
	register(keeper)
	p.beatOnly(cand.id, keeper.id)
	keeper.claim("held")
	if !p.ready(keeper.id) {
		t.Fatal("setup: a held keeper must be ready")
	}
	old := keeper.nonce
	register(keeper)
	p.beatOnly(cand.id, keeper.id)
	keeper.epoch++
	row, err := p.env.q.GetWorkerByID(p.env.ctx, keeper.id)
	if err != nil {
		t.Fatal(err)
	}
	held := "held"
	snap := &ActiveSnapshot{SnapshotEpoch: keeper.epoch, RegisterNonce: old, Active: []ActiveRunEntry{}}
	if _, err := p.svc.ClaimReported(p.env.ctx, row, snap, MemoryAuthorityReport{Value: &held}); !errors.Is(err, ErrActiveSnapshotInvalid) {
		t.Fatalf("stale-nonce claim err=%v, want ErrActiveSnapshotInvalid", err)
	}
	if _, ok := p.heldAt(keeper.id); ok {
		t.Fatal("a delayed held report from the old registration stamped the receipt")
	}
	if p.ready(keeper.id) {
		t.Fatal("a keeper whose only held report is from a replaced registration must not be ready")
	}
	if n, err := p.runPass(); err != nil || n != 0 {
		t.Fatalf("pass = %d, %v; want 0 (no keeper)", n, err)
	}
	p.wantStates(map[uuid.UUID]string{cand.id: "none", keeper.id: "none"})
	keeper.claim("held")
	if !p.ready(keeper.id) {
		t.Fatal("control: a held claim of the new registration must make the keeper ready")
	}
}

func staleHeldReceiptNull(t *testing.T) {
	m := newMemAuthFix(t)
	old := m.nonce
	m.register(t)
	if _, err := m.claimWith(t, old, "held"); !errors.Is(err, ErrActiveSnapshotInvalid) {
		t.Fatalf("stale-nonce claim err=%v, want ErrActiveSnapshotInvalid", err)
	}
	if _, ok := m.heldAt(t); ok {
		t.Fatal("a delayed held report from the old registration stamped the receipt")
	}
	rows, err := m.f.env.q.SetWorkerSnapshotState(m.f.env.ctx, store.SetWorkerSnapshotStateParams{
		ID: m.w.ID, SnapshotEpoch: 99, MemoryAuthorityHeldAt: pgconv.Time(time.Now()), HeldRegisterNonce: pgconv.TextOrNull(old),
	})
	if err != nil || rows != 1 {
		t.Fatalf("SetWorkerSnapshotState rows=%d err=%v", rows, err)
	}
	if _, ok := m.heldAt(t); ok {
		t.Fatal("SetWorkerSnapshotState stamped the receipt for a replaced registration's nonce")
	}
	// Control: the same call under the current nonce stamps it.
	if _, err := m.f.env.q.SetWorkerSnapshotState(m.f.env.ctx, store.SetWorkerSnapshotStateParams{
		ID: m.w.ID, SnapshotEpoch: 100, MemoryAuthorityHeldAt: pgconv.Time(time.Now()), HeldRegisterNonce: pgconv.TextOrNull(m.nonce),
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.heldAt(t); !ok {
		t.Fatal("control: the current nonce did not stamp the receipt")
	}
}

// Register clears the receipt (and a held claim after Register stamps the new registration's).
func TestMemoryAuthorityRegisterClearsHeldReceiptLiveDB(t *testing.T) {
	m := newMemAuthFix(t)
	m.claim(t, "held")
	if _, ok := m.heldAt(t); !ok {
		t.Fatal("setup: held receipt not stamped")
	}
	m.register(t)
	if _, ok := m.heldAt(t); ok {
		t.Fatal("Register did not clear memory_authority_held_at")
	}
	m.f.env.exec(`UPDATE workers SET memory_authority_held_at = now() WHERE id = $1`, m.w.ID)
	m.register(t)
	if _, ok := m.heldAt(t); ok {
		t.Fatal("Register did not clear a seeded memory_authority_held_at")
	}
}

func (m *memAuthFix) heldAt(t *testing.T) (time.Time, bool) {
	t.Helper()
	var at pgtype.Timestamptz
	if err := m.f.env.pool.QueryRow(m.f.env.ctx, `SELECT memory_authority_held_at FROM workers WHERE id=$1`, m.w.ID).Scan(&at); err != nil {
		t.Fatal(err)
	}
	return at.Time, at.Valid
}

// Held-to-held claims refresh the receipt but never rewrite memory_authority_lost_at (the write
// log counts only updates naming that column).
func TestMemoryAuthorityHeldRefreshDoesNotTouchLostColumnLiveDB(t *testing.T) {
	m := newMemAuthFix(t)
	m.claim(t, "held")
	first, _ := m.heldAt(t)
	before := m.writeCount(t)
	time.Sleep(20 * time.Millisecond)
	m.claim(t, "held")
	second, ok := m.heldAt(t)
	if !ok || !second.After(first) {
		t.Fatalf("held_at %v -> %v, want advanced", first, second)
	}
	if n := m.writeCount(t); n != before {
		t.Fatalf("held-to-held claims rewrote memory_authority_lost_at %d times", n-before)
	}
}
