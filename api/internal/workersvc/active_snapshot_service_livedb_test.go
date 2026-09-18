package workersvc

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres. These pin the Register
// transaction (nonce rotation + epoch reset, register-carried snapshot preservation, post-commit
// publish of the orphan pass) and the Heartbeat snapshot storage (PRD #1390 M2a) end to end
// through the Service.

// stateSpy records (run, status) PublishState events so a test can assert exactly which
// transitions publishSwept fanned out post-commit.
type stateSpy struct{ events []stateEvt }

type stateEvt struct {
	id     uuid.UUID
	status string
}

func (s *stateSpy) PublishMessage(uuid.UUID, int32, string, string, string, string, []byte, time.Time) {
}
func (s *stateSpy) PublishState(id uuid.UUID, status string) {
	s.events = append(s.events, stateEvt{id: id, status: status})
}
func (s *stateSpy) PublishHealth(uuid.UUID, string, string, bool) {}
func (s *stateSpy) PublishInput(uuid.UUID)                        {}

func (s *stateSpy) statusFor(id uuid.UUID) (string, bool) {
	for _, e := range s.events {
		if e.id == id {
			return e.status, true
		}
	}
	return "", false
}

// TestRegisterRotatesNonceResetsEpochLiveDB: Register mints a fresh nonce, persists it on the
// worker row, resets the snapshot epoch to 0, and returns the nonce.
func TestRegisterRotatesNonceResetsEpochLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, _, _ := env.seedCodexInfra(t)
	svc := snapshotSvc(env, testParams())

	wk := seedSnapshotWorker(t, env, userID, "OLD-nonce")
	env.exec(`UPDATE workers SET snapshot_epoch = 7 WHERE id = $1`, wk)

	_, nonce, err := svc.Register(env.ctx, store.Worker{ID: wk, UserID: userID}, "v1", "base", nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if nonce == "" || nonce == "OLD-nonce" {
		t.Fatalf("returned nonce = %q, want a fresh non-empty value", nonce)
	}
	var storedNonce string
	var epoch int64
	if err := env.pool.QueryRow(env.ctx, `SELECT snapshot_register_nonce, snapshot_epoch FROM workers WHERE id = $1`, wk).Scan(&storedNonce, &epoch); err != nil {
		t.Fatalf("read worker: %v", err)
	}
	if storedNonce != nonce {
		t.Fatalf("stored nonce = %q, want the returned %q", storedNonce, nonce)
	}
	if epoch != 0 {
		t.Fatalf("snapshot_epoch = %d, want 0 (reset under the new nonce)", epoch)
	}
}

// TestRegisterPublishesOrphanTransitionsLiveDB: Register publishes BOTH the failed (over-cap)
// and the requeued (within-budget) orphan transitions post-commit — the gap M2a closes.
func TestRegisterPublishesOrphanTransitionsLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, _, repoID := env.seedCodexInfra(t)
	svc := snapshotSvc(env, testParams())
	spy := &stateSpy{}
	svc.SetBroadcaster(spy)

	wk := seedSnapshotWorker(t, env, userID, "nonce-A")
	overCap := seedOutageRun(t, env, userID, repoID, wk, "running", "issue", 1, 1) // requeue_count == max → failed
	within := seedOutageRun(t, env, userID, repoID, wk, "running", "issue", 1, 0)  // within budget → requeued

	if _, _, err := svc.Register(env.ctx, store.Worker{ID: wk, UserID: userID}, "v1", "base", nil, nil, nil, nil); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if got := statusOf(t, env, overCap); got != "failed" {
		t.Fatalf("over-cap run status = %q, want failed", got)
	}
	if got := statusOf(t, env, within); got != "queued" {
		t.Fatalf("within-budget run status = %q, want queued", got)
	}
	if st, ok := spy.statusFor(overCap); !ok || st != "failed" {
		t.Fatalf("over-cap publish = (%q,%v), want (failed,true)", st, ok)
	}
	if st, ok := spy.statusFor(within); !ok || st != "queued" {
		t.Fatalf("within-budget publish = (%q,%v), want (queued,true)", st, ok)
	}
}

// TestRegisterCarriedSnapshotPreservesLeaseLiveDB: a register carrying a snapshot preserves an
// unexpired terminal-pending-leased row while clearing an ordinary row, and its orphan pass
// never fails/requeues the leased run (D11) but DOES requeue the now-lease-less ordinary run.
func TestRegisterCarriedSnapshotPreservesLeaseLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, _, repoID := env.seedCodexInfra(t)
	svc := snapshotSvc(env, testParams())
	spy := &stateSpy{}
	svc.SetBroadcaster(spy)

	wk := seedSnapshotWorker(t, env, userID, "nonce-A")
	leased := seedOutageRun(t, env, userID, repoID, wk, "running", "issue", 1, 0)
	ordinary := seedOutageRun(t, env, userID, repoID, wk, "running", "issue", 1, 0)
	insertActiveLease(t, env, wk, leased, 1, true, "1 hour")    // unexpired terminal-pending lease
	insertActiveLease(t, env, wk, ordinary, 1, false, "1 hour") // ordinary row (terminal_pending=false)

	// An empty register snapshot: no live attempts declared, so the ordinary row is cleared and
	// the leased row preserved.
	snap := &ActiveSnapshot{SnapshotEpoch: 0, Active: []ActiveRunEntry{}}
	if _, _, err := svc.Register(env.ctx, store.Worker{ID: wk, UserID: userID}, "v1", "base", nil, nil, nil, snap); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// The leased row is preserved; the ordinary row is gone.
	if _, ok := readActiveRun(t, env, wk, leased); !ok {
		t.Fatal("leased worker_active_runs row was not preserved across register")
	}
	if _, ok := readActiveRun(t, env, wk, ordinary); ok {
		t.Fatal("ordinary worker_active_runs row was not cleared by register")
	}
	// The leased run is untouched by the orphan pass (D11); the ordinary run, now lease-less, is
	// requeued.
	if got := statusOf(t, env, leased); got != "running" {
		t.Fatalf("leased run status = %q, want unchanged running (D11 lease protects the orphan pass)", got)
	}
	if got := statusOf(t, env, ordinary); got != "queued" {
		t.Fatalf("ordinary run status = %q, want queued (no lease, orphan requeue fires)", got)
	}
	if _, ok := spy.statusFor(leased); ok {
		t.Fatal("leased run was published a transition; it must never be swept while leased")
	}
	if st, ok := spy.statusFor(ordinary); !ok || st != "queued" {
		t.Fatalf("ordinary publish = (%q,%v), want (queued,true)", st, ok)
	}
}

// TestHeartbeatStoresValidSnapshotLiveDB: a valid snapshot carried on a heartbeat is stored in
// worker_active_runs and the heartbeat still refreshes liveness.
func TestHeartbeatStoresValidSnapshotLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, _, repoID := env.seedCodexInfra(t)
	svc := snapshotSvc(env, testParams())

	wk := seedSnapshotWorker(t, env, userID, "nonce-A")
	env.exec(`UPDATE workers SET last_heartbeat_at = now() - interval '30 seconds' WHERE id = $1`, wk)
	run := seedOutageRun(t, env, userID, repoID, wk, "running", "issue", 1, 0)

	wkr, err := env.q.GetWorkerByID(env.ctx, wk)
	if err != nil {
		t.Fatalf("GetWorkerByID: %v", err)
	}
	snap := &ActiveSnapshot{SnapshotEpoch: 1, RegisterNonce: "nonce-A", Active: []ActiveRunEntry{entry(run, 1, "running", false)}}
	if _, err := svc.Heartbeat(env.ctx, wkr, nil, nil, snap); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if _, ok := readActiveRun(t, env, wk, run); !ok {
		t.Fatal("valid heartbeat snapshot was not stored in worker_active_runs")
	}
	if got := workerEpoch(t, env, wk); got != 1 {
		t.Fatalf("worker epoch = %d, want 1", got)
	}
	assertHeartbeatFresh(t, env, wk)
}

// TestHeartbeatIgnoresInvalidSnapshotLiveDB: an invalid (wrong-nonce) snapshot on a heartbeat is
// ignored (no rows, no error) and the heartbeat still refreshes liveness — never a 400/500.
func TestHeartbeatIgnoresInvalidSnapshotLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, _, repoID := env.seedCodexInfra(t)
	svc := snapshotSvc(env, testParams())

	wk := seedSnapshotWorker(t, env, userID, "nonce-A")
	env.exec(`UPDATE workers SET last_heartbeat_at = now() - interval '30 seconds' WHERE id = $1`, wk)
	run := seedOutageRun(t, env, userID, repoID, wk, "running", "issue", 1, 0)

	wkr, err := env.q.GetWorkerByID(env.ctx, wk)
	if err != nil {
		t.Fatalf("GetWorkerByID: %v", err)
	}
	bad := &ActiveSnapshot{SnapshotEpoch: 1, RegisterNonce: "nonce-WRONG", Active: []ActiveRunEntry{entry(run, 1, "running", false)}}
	if _, err := svc.Heartbeat(env.ctx, wkr, nil, nil, bad); err != nil {
		t.Fatalf("Heartbeat with invalid snapshot returned an error: %v (must never fail the heartbeat)", err)
	}
	if got := countActiveRuns(t, env, wk); got != 0 {
		t.Fatalf("worker_active_runs count = %d, want 0 (invalid snapshot ignored)", got)
	}
	if got := workerEpoch(t, env, wk); got != 0 {
		t.Fatalf("worker epoch = %d, want 0 (invalid snapshot must not advance it)", got)
	}
	assertHeartbeatFresh(t, env, wk)
}

// assertHeartbeatFresh confirms the worker's last_heartbeat_at is within the last few seconds.
func assertHeartbeatFresh(t *testing.T, env codexTestEnv, workerID uuid.UUID) {
	t.Helper()
	var age float64
	if err := env.pool.QueryRow(env.ctx, `SELECT EXTRACT(EPOCH FROM (now() - last_heartbeat_at)) FROM workers WHERE id = $1`, workerID).Scan(&age); err != nil {
		t.Fatalf("read last_heartbeat_at: %v", err)
	}
	if age > 5 {
		t.Fatalf("last_heartbeat_at age = %.1fs, want fresh (< 5s) — liveness was not refreshed", age)
	}
}
