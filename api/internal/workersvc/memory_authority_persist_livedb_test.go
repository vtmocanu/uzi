package workersvc

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// memory_authority_persist_livedb_test.go pins issue #4395's persisted memory-authority loss
// (workers.memory_authority_lost_at) through the real claim path and Register, on the real
// queries. Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.

// memAuthFix is one capable worker with a current registration. Every claim carries a snapshot
// stamped with that registration's nonce and a strictly advancing epoch, like the agent.
type memAuthFix struct {
	f      *isoFix
	w      store.Worker
	nonce  string
	epoch  int64
	writes string // table the write-log trigger fills
}

func newMemAuthFix(t *testing.T) *memAuthFix {
	t.Helper()
	f := newIsoFix(t)
	m := &memAuthFix{f: f, w: f.seedWorker(t, laneWorkerSpec{protoCaps: capableCaps()})}
	m.register(t)
	m.installWriteLog(t)
	return m
}

// register replaces the registration: a fresh nonce, epoch 0, the column cleared.
func (m *memAuthFix) register(t *testing.T) {
	t.Helper()
	_, nonce, err := m.f.svc.Register(m.f.env.ctx, store.Worker{ID: m.w.ID, UserID: m.f.userID}, "v1", "base", nil, nil, capableCaps(), nil)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	m.nonce, m.epoch = nonce, 0
}

// installWriteLog counts every UPDATE whose SET list names memory_authority_lost_at on this
// worker, whether or not the value changes (UPDATE OF fires on the SET list). A held-to-held
// claim must add none. The worker's other writes (snapshot epoch, heartbeat) are not counted.
func (m *memAuthFix) installWriteLog(t *testing.T) {
	t.Helper()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	m.writes = "memauth_writes_" + suffix
	fn := "memauth_fn_" + suffix
	trg := "memauth_trg_" + suffix
	m.f.env.exec(`CREATE TABLE ` + m.writes + ` (n bigserial PRIMARY KEY)`)
	m.f.env.exec(`CREATE FUNCTION ` + fn + `() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN INSERT INTO ` + m.writes + ` DEFAULT VALUES; RETURN NEW; END $$`)
	m.f.env.exec(`CREATE TRIGGER ` + trg + ` AFTER UPDATE OF memory_authority_lost_at ON workers
	              FOR EACH ROW WHEN (OLD.id = '` + m.w.ID.String() + `') EXECUTE FUNCTION ` + fn + `()`)
	t.Cleanup(func() {
		_, _ = m.f.env.pool.Exec(m.f.env.ctx, `DROP TRIGGER IF EXISTS `+trg+` ON workers`)
		_, _ = m.f.env.pool.Exec(m.f.env.ctx, `DROP FUNCTION IF EXISTS `+fn+`()`)
		_, _ = m.f.env.pool.Exec(m.f.env.ctx, `DROP TABLE IF EXISTS `+m.writes)
	})
}

func (m *memAuthFix) writeCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := m.f.env.pool.QueryRow(m.f.env.ctx, `SELECT count(*) FROM `+m.writes).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// claimWith claims under the given registration nonce with the next epoch and the report.
func (m *memAuthFix) claimWith(t *testing.T, nonce, report string) (*ClaimPayload, error) {
	t.Helper()
	m.epoch++
	row, err := m.f.env.q.GetWorkerByID(m.f.env.ctx, m.w.ID)
	if err != nil {
		t.Fatal(err)
	}
	snap := &ActiveSnapshot{SnapshotEpoch: m.epoch, RegisterNonce: nonce, Active: []ActiveRunEntry{}}
	return m.f.svc.ClaimReported(m.f.env.ctx, row, snap, MemoryAuthorityReport{Value: &report})
}

func (m *memAuthFix) claim(t *testing.T, report string) *ClaimPayload {
	t.Helper()
	p, err := m.claimWith(t, m.nonce, report)
	if err != nil {
		t.Fatalf("claim(%s): %v", report, err)
	}
	return p
}

func (m *memAuthFix) lostAt(t *testing.T) (time.Time, bool) {
	t.Helper()
	var at *time.Time
	if err := m.f.env.pool.QueryRow(m.f.env.ctx, `SELECT memory_authority_lost_at FROM workers WHERE id=$1`, m.w.ID).Scan(&at); err != nil {
		t.Fatal(err)
	}
	if at == nil {
		return time.Time{}, false
	}
	return *at, true
}

func TestMemoryAuthorityLostPersistsFirstLostTimeLiveDB(t *testing.T) {
	m := newMemAuthFix(t)
	run := m.f.queuedRun(t, 4395, false)
	if _, ok := m.lostAt(t); ok {
		t.Fatal("column set before any report")
	}
	if p := m.claim(t, "lost"); p != nil {
		t.Fatalf("lost claim returned %s, want idle", p.RunID)
	}
	first, ok := m.lostAt(t)
	if !ok {
		t.Fatal("lost report did not set memory_authority_lost_at")
	}
	time.Sleep(20 * time.Millisecond)
	if p := m.claim(t, "lost"); p != nil {
		t.Fatalf("second lost claim returned %s, want idle", p.RunID)
	}
	if again, _ := m.lostAt(t); !again.Equal(first) {
		t.Fatalf("second lost report moved the first-lost time: %s -> %s", first, again)
	}
	var status string
	if err := m.f.env.pool.QueryRow(m.f.env.ctx, `SELECT status FROM runs WHERE id=$1`, run).Scan(&status); err != nil || status != "queued" {
		t.Fatalf("run status=%q err=%v, want queued (claim enforcement unchanged)", status, err)
	}
	if n := m.writeCount(t); n != 1 {
		t.Fatalf("column writes after two lost claims = %d, want 1", n)
	}
}

func TestMemoryAuthorityLostPersistsOnEmptyPollLiveDB(t *testing.T) {
	m := newMemAuthFix(t) // no queued run anywhere: the poll claims nothing
	if p := m.claim(t, "lost"); p != nil {
		t.Fatalf("empty poll returned %s", p.RunID)
	}
	if _, ok := m.lostAt(t); !ok {
		t.Fatal("a lost report on a poll that claims no run was not persisted")
	}
}

func TestMemoryAuthorityHeldDoesNotClearLossLiveDB(t *testing.T) {
	m := newMemAuthFix(t)
	m.claim(t, "lost")
	lost, ok := m.lostAt(t)
	if !ok {
		t.Fatal("setup: lost not persisted")
	}
	before := m.writeCount(t)
	for i := 0; i < 3; i++ {
		m.claim(t, "held")
	}
	if got, ok := m.lostAt(t); !ok || !got.Equal(lost) {
		t.Fatalf("held reports changed the recorded loss: %v (set %t), want %v kept", got, ok, lost)
	}
	if _, ok := m.heldAt(t); ok {
		t.Fatal("a held report after a loss stamped the receipt")
	}
	if n := m.writeCount(t); n != before {
		t.Fatalf("held reports after a loss wrote memory_authority_lost_at %d times, want 0", n-before)
	}
	m.register(t)
	if _, ok := m.lostAt(t); ok {
		t.Fatal("Register did not clear the loss")
	}
	m.claim(t, "held")
	if _, ok := m.heldAt(t); !ok {
		t.Fatal("a held report after Register did not stamp the receipt")
	}
	before = m.writeCount(t)
	for i := 0; i < 3; i++ {
		m.claim(t, "held")
	}
	if n := m.writeCount(t); n != before {
		t.Fatalf("held-to-held claims wrote the column %d times, want 0", n-before)
	}
}

func TestMemoryAuthorityRegisterClearsLiveDB(t *testing.T) {
	m := newMemAuthFix(t)
	m.claim(t, "lost")
	if _, ok := m.lostAt(t); !ok {
		t.Fatal("setup: lost not persisted")
	}
	m.register(t)
	if _, ok := m.lostAt(t); ok {
		t.Fatal("Register did not clear memory_authority_lost_at")
	}
}

// A delayed report bound to the previous registration, applied after Register, changes nothing:
// the claim refuses the stale nonce, and the writer queries themselves match zero rows for it.
func TestMemoryAuthorityStaleRegistrationReportsChangeNothingLiveDB(t *testing.T) {
	t.Run("delayed lost does not set the new registration's column", func(t *testing.T) {
		m := newMemAuthFix(t)
		old := m.nonce
		m.register(t)
		before := m.writeCount(t)
		if _, err := m.claimWith(t, old, "lost"); !errors.Is(err, ErrActiveSnapshotInvalid) {
			t.Fatalf("stale-nonce claim err=%v, want ErrActiveSnapshotInvalid", err)
		}
		if _, ok := m.lostAt(t); ok {
			t.Fatal("a delayed lost report from the old registration set the column")
		}
		if n := m.writeCount(t); n != before {
			t.Fatalf("stale lost report wrote the column %d times", n-before)
		}
		n, err := m.f.env.q.SetWorkerMemoryAuthorityLost(m.f.env.ctx, store.SetWorkerMemoryAuthorityLostParams{
			ID: m.w.ID, RegisterNonce: pgconv.TextOrNull(old), LostAt: pgconv.Time(time.Now()),
		})
		if err != nil || n != 0 {
			t.Fatalf("Set with the old nonce: rows=%d err=%v, want 0", n, err)
		}
	})
	t.Run("delayed held does not change the new registration's column or receipt", func(t *testing.T) {
		m := newMemAuthFix(t)
		old := m.nonce
		m.register(t)
		m.claim(t, "lost")
		set, ok := m.lostAt(t)
		if !ok {
			t.Fatal("setup: lost not persisted on the new registration")
		}
		if _, err := m.claimWith(t, old, "held"); !errors.Is(err, ErrActiveSnapshotInvalid) {
			t.Fatalf("stale-nonce claim err=%v, want ErrActiveSnapshotInvalid", err)
		}
		if got, ok := m.lostAt(t); !ok || !got.Equal(set) {
			t.Fatalf("a delayed held report from the old registration changed the column: %v %v", got, ok)
		}
		n, err := m.f.env.q.StampWorkerMemoryAuthorityHeld(m.f.env.ctx, store.StampWorkerMemoryAuthorityHeldParams{
			ID: m.w.ID, RegisterNonce: pgconv.TextOrNull(old), HeldAt: pgconv.Time(time.Now()),
		})
		if err != nil || n != 0 {
			t.Fatalf("Stamp with the old nonce: rows=%d err=%v, want 0", n, err)
		}
		// SetWorkerSnapshotState with a receipt while the loss is recorded leaves the receipt NULL.
		rows, err := m.f.env.q.SetWorkerSnapshotState(m.f.env.ctx, store.SetWorkerSnapshotStateParams{
			ID: m.w.ID, SnapshotEpoch: 99, MemoryAuthorityHeldAt: pgconv.Time(time.Now()), HeldRegisterNonce: pgconv.TextOrNull(m.nonce),
		})
		if err != nil || rows != 1 {
			t.Fatalf("SetWorkerSnapshotState rows=%d err=%v", rows, err)
		}
		if _, ok := m.heldAt(t); ok {
			t.Fatal("SetWorkerSnapshotState stamped a receipt while a loss is recorded")
		}
		// StampWorkerMemoryAuthorityHeld under the CURRENT nonce while the loss is recorded also
		// stamps nothing: its lost_at IS NULL fence, not only its nonce fence, refuses.
		n, err = m.f.env.q.StampWorkerMemoryAuthorityHeld(m.f.env.ctx, store.StampWorkerMemoryAuthorityHeldParams{
			ID: m.w.ID, RegisterNonce: pgconv.TextOrNull(m.nonce), HeldAt: pgconv.Time(time.Now()),
		})
		if err != nil || n != 0 {
			t.Fatalf("Stamp with the current nonce under a loss: rows=%d err=%v, want 0", n, err)
		}
		if _, ok := m.heldAt(t); ok {
			t.Fatal("StampWorkerMemoryAuthorityHeld stamped a receipt while a loss is recorded")
		}
	})
}

// The bodyless (snapshotless) claim path carries no registration identity, so it persists
// nothing (PRD #4395: the column is set or cleared only for the current registration, and a
// delayed report from an older registration changes nothing). A bodyless lost report must not
// set the column, and a bodyless held report must not clear one a fenced claim set.
func TestMemoryAuthorityBodylessClaimWritesNothingLiveDB(t *testing.T) {
	m := newMemAuthFix(t)
	bodyless := func(report string) *ClaimPayload {
		t.Helper()
		row, err := m.f.env.q.GetWorkerByID(m.f.env.ctx, m.w.ID)
		if err != nil {
			t.Fatal(err)
		}
		p, err := m.f.svc.ClaimReported(m.f.env.ctx, row, nil, MemoryAuthorityReport{Value: &report})
		if err != nil {
			t.Fatalf("bodyless claim(%s): %v", report, err)
		}
		return p
	}
	before := m.writeCount(t)
	if p := bodyless("lost"); p != nil {
		t.Fatalf("bodyless lost claim returned %s, want idle", p.RunID)
	}
	if _, ok := m.lostAt(t); ok || m.writeCount(t) != before {
		t.Fatal("a bodyless lost report wrote memory_authority_lost_at; it cannot be fenced and must write nothing")
	}
	m.claim(t, "lost")
	first, ok := m.lostAt(t)
	if !ok {
		t.Fatal("a snapshot-carrying lost claim did not set the column")
	}
	bodyless("held")
	if again, ok := m.lostAt(t); !ok || !again.Equal(first) {
		t.Fatal("a bodyless held report changed the column; it cannot be fenced and must write nothing")
	}
}

// A reader on another connection during Register sees the old registration with its lost mark, or
// the new registration with the column cleared, never a mixed pair.
func TestMemoryAuthorityRegisterIsAtomicToReadersLiveDB(t *testing.T) {
	m := newMemAuthFix(t)
	m.claim(t, "lost")
	if _, ok := m.lostAt(t); !ok {
		t.Fatal("setup: lost not persisted")
	}
	oldNonce := m.nonce

	read := func() (string, bool) {
		t.Helper()
		var nonce *string
		var lost *time.Time
		if err := m.f.env.pool.QueryRow(m.f.env.ctx,
			`SELECT snapshot_register_nonce, memory_authority_lost_at FROM workers WHERE id=$1`, m.w.ID).Scan(&nonce, &lost); err != nil {
			t.Fatal(err)
		}
		if nonce == nil {
			return "", lost != nil
		}
		return *nonce, lost != nil
	}

	held, release := make(chan struct{}), make(chan struct{})
	m.f.svc.registerAfterRegisterWorker = func() {
		close(held)
		select {
		case <-release:
		case <-time.After(10 * time.Second):
			t.Error("register hook never released")
		}
	}
	t.Cleanup(func() { m.f.svc.registerAfterRegisterWorker = nil })

	done := make(chan error, 1)
	var newNonce string
	go func() {
		_, n, err := m.f.svc.Register(m.f.env.ctx, store.Worker{ID: m.w.ID, UserID: m.f.userID}, "v1", "base", nil, nil, capableCaps(), nil)
		newNonce = n
		done <- err
	}()
	select {
	case <-held:
	case <-time.After(10 * time.Second):
		t.Fatal("Register never reached the hook")
	}
	if nonce, lost := read(); nonce != oldNonce || !lost {
		t.Errorf("reader during uncommitted Register saw nonce=%q lost=%t, want the old pair (%q, true)", nonce, lost, oldNonce)
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Register: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Register never finished")
	}
	if nonce, lost := read(); nonce != newNonce || nonce == oldNonce || lost {
		t.Fatalf("reader after Register saw nonce=%q lost=%t, want the new registration (%q) with the column NULL", nonce, lost, newNonce)
	}
}

// A worker that is not capable and sends no report is neither lost nor held: no write.
func TestMemoryAuthorityAbsentReportWithoutCapabilitiesWritesNothingLiveDB(t *testing.T) {
	f := newIsoFix(t)
	m := &memAuthFix{f: f, w: f.seedWorker(t, laneWorkerSpec{})}
	_, nonce, err := f.svc.Register(f.env.ctx, store.Worker{ID: m.w.ID, UserID: f.userID}, "v1", "base", nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	m.nonce = nonce
	m.installWriteLog(t)
	row, err := f.env.q.GetWorkerByID(f.env.ctx, m.w.ID)
	if err != nil {
		t.Fatal(err)
	}
	snap := &ActiveSnapshot{SnapshotEpoch: 1, RegisterNonce: nonce, Active: []ActiveRunEntry{}}
	if _, err := f.svc.ClaimReported(f.env.ctx, row, snap, MemoryAuthorityReport{}); err != nil {
		t.Fatal(err)
	}
	if n := m.writeCount(t); n != 0 {
		t.Fatalf("absent report from an incapable worker wrote the column %d times", n)
	}
	if _, ok := m.lostAt(t); ok {
		t.Fatal("column set for an incapable worker with no report")
	}
}
