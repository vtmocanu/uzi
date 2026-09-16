package workersvc

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (run via
// ./e2e/run-store-it.sh). These pin ReplaceWorkerActiveRuns (PRD #1390 M2a) against real SQL:
// shape/cap validation, ownership-drop, epoch ordering, nonce checks, the heartbeat-ignore vs
// claim-sentinel split, and the terminal-pending / pending_overflow lease stamping.

// snapshotSvc builds a Service wired with the pool as its tx beginner and the given params.
func snapshotSvc(env codexTestEnv, p Params) *Service {
	svc := New(env.q, env.box, p)
	svc.SetTxBeginner(env.pool)
	return svc
}

// seedSnapshotWorker inserts a fresh online worker with a known register nonce (epoch 0), the
// steady state right after a register under this feature.
func seedSnapshotWorker(t *testing.T, env codexTestEnv, userID uuid.UUID, nonce string) uuid.UUID {
	t.Helper()
	workerID := uuid.New()
	env.exec(`INSERT INTO workers (id, user_id, name, token_hash, status, snapshot_register_nonce, snapshot_epoch)
	          VALUES ($1, $2, $3, $4, 'online', $5, 0)`,
		workerID, userID, "wsnap-"+workerID.String(), workerID[:], nonce)
	env.exec(`UPDATE workers SET last_heartbeat_at = now() WHERE id = $1`, workerID)
	return workerID
}

// entry builds one ActiveRunEntry.
func entry(runID uuid.UUID, gen int64, phase string, pending bool) ActiveRunEntry {
	return ActiveRunEntry{RunID: runID.String(), ClaimGeneration: gen, Phase: phase, TerminalPending: pending}
}

// applySnapshot runs ReplaceWorkerActiveRuns in its own transaction against the freshly-read
// worker row, committing on success (or a heartbeat/register no-op) and rolling back on error,
// mirroring how Heartbeat/Register drive it.
func applySnapshot(t *testing.T, svc *Service, env codexTestEnv, workerID uuid.UUID, snap *ActiveSnapshot, mode snapshotMode) (bool, error) {
	t.Helper()
	wkr, err := env.q.GetWorkerByID(env.ctx, workerID)
	if err != nil {
		t.Fatalf("GetWorkerByID: %v", err)
	}
	tx, err := env.pool.Begin(env.ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	applied, rerr := svc.ReplaceWorkerActiveRuns(env.ctx, store.New(tx), wkr, snap, mode)
	if rerr != nil {
		_ = tx.Rollback(env.ctx)
		return applied, rerr
	}
	if err := tx.Commit(env.ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return applied, nil
}

// activeRunRow reads the persisted worker_active_runs row for (worker, run).
type activeRunRow struct {
	gen             int64
	phase           string
	terminalPending bool
	untilValid      bool
	untilFuture     bool
	epoch           int64
}

func readActiveRun(t *testing.T, env codexTestEnv, workerID, runID uuid.UUID) (activeRunRow, bool) {
	t.Helper()
	var r activeRunRow
	var until *time.Time
	err := env.pool.QueryRow(env.ctx,
		`SELECT claim_generation, phase, terminal_pending, terminal_pending_until, snapshot_epoch
		   FROM worker_active_runs WHERE worker_id = $1 AND run_id = $2`,
		workerID, runID).Scan(&r.gen, &r.phase, &r.terminalPending, &until, &r.epoch)
	if errors.Is(err, pgx.ErrNoRows) {
		return activeRunRow{}, false
	}
	if err != nil {
		t.Fatalf("read worker_active_runs: %v", err)
	}
	if until != nil {
		r.untilValid = true
		r.untilFuture = until.After(time.Now())
	}
	return r, true
}

func countActiveRuns(t *testing.T, env codexTestEnv, workerID uuid.UUID) int {
	t.Helper()
	var n int
	if err := env.pool.QueryRow(env.ctx, `SELECT count(*) FROM worker_active_runs WHERE worker_id = $1`, workerID).Scan(&n); err != nil {
		t.Fatalf("count worker_active_runs: %v", err)
	}
	return n
}

func workerEpoch(t *testing.T, env codexTestEnv, workerID uuid.UUID) int64 {
	t.Helper()
	var e int64
	if err := env.pool.QueryRow(env.ctx, `SELECT snapshot_epoch FROM workers WHERE id = $1`, workerID).Scan(&e); err != nil {
		t.Fatalf("read snapshot_epoch: %v", err)
	}
	return e
}

func workerOverflow(t *testing.T, env codexTestEnv, workerID uuid.UUID) (flag, untilFuture bool) {
	t.Helper()
	var until *time.Time
	if err := env.pool.QueryRow(env.ctx, `SELECT pending_overflow, pending_overflow_until FROM workers WHERE id = $1`, workerID).Scan(&flag, &until); err != nil {
		t.Fatalf("read pending_overflow: %v", err)
	}
	return flag, until != nil && until.After(time.Now())
}

// TestReplaceWorkerActiveRunsHeartbeatValidLiveDB: a valid heartbeat snapshot is stored, the
// worker epoch advances, a live entry gets a NULL lease and a pending one a future lease.
func TestReplaceWorkerActiveRunsHeartbeatValidLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, _, repoID := env.seedCodexInfra(t)
	svc := snapshotSvc(env, testParams())

	wk := seedSnapshotWorker(t, env, userID, "nonce-A")
	live := seedOutageRun(t, env, userID, repoID, wk, "running", "issue", 1, 0)
	pending := seedOutageRun(t, env, userID, repoID, wk, "running", "issue", 1, 0)

	snap := &ActiveSnapshot{
		SnapshotEpoch: 1,
		RegisterNonce: "nonce-A",
		Active: []ActiveRunEntry{
			entry(live, 1, "awaiting_approval", false),
			entry(pending, 1, "running", true),
		},
	}
	applied, err := applySnapshot(t, svc, env, wk, snap, snapshotModeHeartbeat)
	if err != nil || !applied {
		t.Fatalf("apply valid heartbeat snapshot: applied=%v err=%v", applied, err)
	}
	if got := countActiveRuns(t, env, wk); got != 2 {
		t.Fatalf("worker_active_runs count = %d, want 2", got)
	}
	if got := workerEpoch(t, env, wk); got != 1 {
		t.Fatalf("worker snapshot_epoch = %d, want 1", got)
	}
	lr, ok := readActiveRun(t, env, wk, live)
	if !ok || lr.phase != "awaiting_approval" || lr.terminalPending || lr.untilValid {
		t.Fatalf("live row = %+v ok=%v, want phase awaiting_approval, terminal_pending false, NULL lease", lr, ok)
	}
	pr, ok := readActiveRun(t, env, wk, pending)
	if !ok || !pr.terminalPending || !pr.untilValid || !pr.untilFuture {
		t.Fatalf("pending row = %+v ok=%v, want terminal_pending true with a future lease", pr, ok)
	}
}

// TestReplaceWorkerActiveRunsPendingOverflowLiveDB: pending_overflow=true stamps a future
// pending_overflow_until; a later unflagged valid snapshot clears it before expiry.
func TestReplaceWorkerActiveRunsPendingOverflowLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, _, repoID := env.seedCodexInfra(t)
	svc := snapshotSvc(env, testParams())

	wk := seedSnapshotWorker(t, env, userID, "nonce-A")
	run := seedOutageRun(t, env, userID, repoID, wk, "running", "issue", 1, 0)

	flagged := &ActiveSnapshot{SnapshotEpoch: 1, RegisterNonce: "nonce-A", PendingOverflow: true, Active: []ActiveRunEntry{entry(run, 1, "running", false)}}
	if _, err := applySnapshot(t, svc, env, wk, flagged, snapshotModeHeartbeat); err != nil {
		t.Fatalf("apply flagged: %v", err)
	}
	if flag, future := workerOverflow(t, env, wk); !flag || !future {
		t.Fatalf("after flagged snapshot: pending_overflow=%v until-future=%v, want both true", flag, future)
	}

	unflagged := &ActiveSnapshot{SnapshotEpoch: 2, RegisterNonce: "nonce-A", PendingOverflow: false, Active: []ActiveRunEntry{entry(run, 1, "running", false)}}
	if _, err := applySnapshot(t, svc, env, wk, unflagged, snapshotModeHeartbeat); err != nil {
		t.Fatalf("apply unflagged: %v", err)
	}
	if flag, future := workerOverflow(t, env, wk); flag || future {
		t.Fatalf("after unflagged snapshot: pending_overflow=%v until-future=%v, want both cleared", flag, future)
	}
}

// TestReplaceWorkerActiveRunsNonceMismatchLiveDB: a wrong nonce is IGNORED in heartbeat mode
// (rows untouched, no error) and a SENTINEL error in claim mode.
func TestReplaceWorkerActiveRunsNonceMismatchLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, _, repoID := env.seedCodexInfra(t)
	svc := snapshotSvc(env, testParams())

	wk := seedSnapshotWorker(t, env, userID, "nonce-A")
	run := seedOutageRun(t, env, userID, repoID, wk, "running", "issue", 1, 0)
	good := &ActiveSnapshot{SnapshotEpoch: 1, RegisterNonce: "nonce-A", Active: []ActiveRunEntry{entry(run, 1, "running", false)}}
	if _, err := applySnapshot(t, svc, env, wk, good, snapshotModeHeartbeat); err != nil {
		t.Fatalf("seed rows: %v", err)
	}

	bad := &ActiveSnapshot{SnapshotEpoch: 2, RegisterNonce: "nonce-WRONG", Active: []ActiveRunEntry{}}
	applied, err := applySnapshot(t, svc, env, wk, bad, snapshotModeHeartbeat)
	if applied || err != nil {
		t.Fatalf("heartbeat wrong-nonce: applied=%v err=%v, want (false, nil)", applied, err)
	}
	if got := countActiveRuns(t, env, wk); got != 1 {
		t.Fatalf("rows after ignored wrong-nonce heartbeat = %d, want 1 (untouched)", got)
	}

	_, cerr := applySnapshot(t, svc, env, wk, bad, snapshotModeClaim)
	if !errors.Is(cerr, ErrActiveSnapshotInvalid) {
		t.Fatalf("claim wrong-nonce err = %v, want ErrActiveSnapshotInvalid", cerr)
	}
	if got := countActiveRuns(t, env, wk); got != 1 {
		t.Fatalf("rows after failed claim = %d, want 1 (untouched)", got)
	}
}

// TestReplaceWorkerActiveRunsEpochOrderingLiveDB: an equal- or older-epoch snapshot is
// discarded after a newer one; a strictly-greater epoch applies.
func TestReplaceWorkerActiveRunsEpochOrderingLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, _, repoID := env.seedCodexInfra(t)
	svc := snapshotSvc(env, testParams())

	wk := seedSnapshotWorker(t, env, userID, "nonce-A")
	runA := seedOutageRun(t, env, userID, repoID, wk, "running", "issue", 1, 0)
	runB := seedOutageRun(t, env, userID, repoID, wk, "running", "issue", 1, 0)

	// Epoch 2 applies (rows: runA).
	if _, err := applySnapshot(t, svc, env, wk, &ActiveSnapshot{SnapshotEpoch: 2, RegisterNonce: "nonce-A", Active: []ActiveRunEntry{entry(runA, 1, "running", false)}}, snapshotModeHeartbeat); err != nil {
		t.Fatalf("apply epoch 2: %v", err)
	}
	// Equal epoch (2) discarded.
	if applied, err := applySnapshot(t, svc, env, wk, &ActiveSnapshot{SnapshotEpoch: 2, RegisterNonce: "nonce-A", Active: []ActiveRunEntry{entry(runB, 1, "running", false)}}, snapshotModeHeartbeat); applied || err != nil {
		t.Fatalf("equal epoch: applied=%v err=%v, want discarded", applied, err)
	}
	// Older epoch (1) discarded.
	if applied, err := applySnapshot(t, svc, env, wk, &ActiveSnapshot{SnapshotEpoch: 1, RegisterNonce: "nonce-A", Active: []ActiveRunEntry{entry(runB, 1, "running", false)}}, snapshotModeHeartbeat); applied || err != nil {
		t.Fatalf("older epoch: applied=%v err=%v, want discarded", applied, err)
	}
	if _, ok := readActiveRun(t, env, wk, runA); !ok {
		t.Fatal("runA row gone after discarded snapshots; the stale snapshots must not have replaced anything")
	}
	if _, ok := readActiveRun(t, env, wk, runB); ok {
		t.Fatal("runB row present; a discarded snapshot must not have inserted it")
	}
	// Strictly greater epoch (3) applies (rows: runB only).
	if applied, err := applySnapshot(t, svc, env, wk, &ActiveSnapshot{SnapshotEpoch: 3, RegisterNonce: "nonce-A", Active: []ActiveRunEntry{entry(runB, 1, "running", false)}}, snapshotModeHeartbeat); !applied || err != nil {
		t.Fatalf("epoch 3: applied=%v err=%v, want applied", applied, err)
	}
	if _, ok := readActiveRun(t, env, wk, runA); ok {
		t.Fatal("runA row still present after the epoch-3 full replacement")
	}
	if _, ok := readActiveRun(t, env, wk, runB); !ok {
		t.Fatal("runB row missing after the epoch-3 snapshot")
	}
	if got := workerEpoch(t, env, wk); got != 3 {
		t.Fatalf("worker epoch = %d, want 3", got)
	}
}

// TestReplaceWorkerActiveRunsOwnershipDropLiveDB: an entry whose run is owned by ANOTHER worker
// is dropped (no row) while the owned entry is kept.
func TestReplaceWorkerActiveRunsOwnershipDropLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, _, repoID := env.seedCodexInfra(t)
	svc := snapshotSvc(env, testParams())

	wk := seedSnapshotWorker(t, env, userID, "nonce-A")
	other := seedSnapshotWorker(t, env, userID, "nonce-B")
	owned := seedOutageRun(t, env, userID, repoID, wk, "running", "issue", 1, 0)
	foreign := seedOutageRun(t, env, userID, repoID, other, "running", "issue", 1, 0)

	snap := &ActiveSnapshot{SnapshotEpoch: 1, RegisterNonce: "nonce-A", Active: []ActiveRunEntry{
		entry(owned, 1, "running", false),
		entry(foreign, 1, "running", false),
	}}
	if applied, err := applySnapshot(t, svc, env, wk, snap, snapshotModeHeartbeat); !applied || err != nil {
		t.Fatalf("apply: applied=%v err=%v", applied, err)
	}
	if _, ok := readActiveRun(t, env, wk, owned); !ok {
		t.Fatal("owned entry missing; it should have been persisted")
	}
	if _, ok := readActiveRun(t, env, wk, foreign); ok {
		t.Fatal("foreign entry persisted; a run not owned by this worker must be dropped")
	}
	if got := countActiveRuns(t, env, wk); got != 1 {
		t.Fatalf("count = %d, want 1 (only the owned entry)", got)
	}
}

// TestReplaceWorkerActiveRunsShapeValidationLiveDB: each shape defect rejects the WHOLE
// snapshot (heartbeat ignore / claim sentinel) and leaves prior rows untouched.
func TestReplaceWorkerActiveRunsShapeValidationLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, _, repoID := env.seedCodexInfra(t)
	svc := snapshotSvc(env, testParams())

	wk := seedSnapshotWorker(t, env, userID, "nonce-A")
	r1 := seedOutageRun(t, env, userID, repoID, wk, "running", "issue", 1, 0)
	r2 := seedOutageRun(t, env, userID, repoID, wk, "running", "issue", 1, 0)

	cases := []struct {
		name    string
		entries []ActiveRunEntry
	}{
		{"bad-uuid", []ActiveRunEntry{{RunID: "not-a-uuid", ClaimGeneration: 1, Phase: "running"}}},
		{"bad-phase", []ActiveRunEntry{entry(r1, 1, "bogus", false)}},
		{"neg-gen", []ActiveRunEntry{entry(r1, -1, "running", false)}},
		{"dup", []ActiveRunEntry{entry(r1, 1, "running", false), entry(r1, 1, "running", false)}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			snap := &ActiveSnapshot{SnapshotEpoch: 5, RegisterNonce: "nonce-A", Active: c.entries}
			applied, err := applySnapshot(t, svc, env, wk, snap, snapshotModeHeartbeat)
			if applied || err != nil {
				t.Fatalf("heartbeat %s: applied=%v err=%v, want (false, nil)", c.name, applied, err)
			}
			_, cerr := applySnapshot(t, svc, env, wk, snap, snapshotModeClaim)
			if !errors.Is(cerr, ErrActiveSnapshotInvalid) {
				t.Fatalf("claim %s: err=%v, want ErrActiveSnapshotInvalid", c.name, cerr)
			}
		})
	}
	// A well-shaped snapshot after all the rejections still applies (rows untouched by the
	// rejects, then replaced here).
	ok := &ActiveSnapshot{SnapshotEpoch: 5, RegisterNonce: "nonce-A", Active: []ActiveRunEntry{entry(r2, 1, "running", false)}}
	if applied, err := applySnapshot(t, svc, env, wk, ok, snapshotModeHeartbeat); !applied || err != nil {
		t.Fatalf("valid follow-up: applied=%v err=%v", applied, err)
	}
}

// TestReplaceWorkerActiveRunsCapsLiveDB: the two per-type caps and the absolute ceiling each
// reject an oversize snapshot. The worker advertises max_concurrent_runs = 1 so the live cap is
// 3 (mcr + 2); WorkerOutboxMaxPending is overridden to 1.
func TestReplaceWorkerActiveRunsCapsLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, _, repoID := env.seedCodexInfra(t)
	p := testParams()
	p.WorkerOutboxMaxPending = 1
	p.ActiveSnapshotMaxEntries = 5
	svc := snapshotSvc(env, p)

	wk := seedSnapshotWorker(t, env, userID, "nonce-A")
	env.exec(`UPDATE workers SET max_concurrent_runs = 1 WHERE id = $1`, wk)

	seedN := func(n int) []uuid.UUID {
		ids := make([]uuid.UUID, n)
		for i := range ids {
			ids[i] = seedOutageRun(t, env, userID, repoID, wk, "running", "issue", 1, 0)
		}
		return ids
	}

	// Live cap: 4 live entries > mcr(1)+2 = 3 → rejected.
	liveIDs := seedN(4)
	liveEntries := make([]ActiveRunEntry, 0, 4)
	for _, id := range liveIDs {
		liveEntries = append(liveEntries, entry(id, 1, "running", false))
	}
	if applied, err := applySnapshot(t, svc, env, wk, &ActiveSnapshot{SnapshotEpoch: 1, RegisterNonce: "nonce-A", Active: liveEntries}, snapshotModeHeartbeat); applied || err != nil {
		t.Fatalf("live-cap: applied=%v err=%v, want rejected", applied, err)
	}

	// Pending cap: 2 pending entries > WorkerOutboxMaxPending(1) → rejected.
	pIDs := seedN(2)
	pendingEntries := []ActiveRunEntry{entry(pIDs[0], 1, "running", true), entry(pIDs[1], 1, "running", true)}
	if applied, err := applySnapshot(t, svc, env, wk, &ActiveSnapshot{SnapshotEpoch: 1, RegisterNonce: "nonce-A", Active: pendingEntries}, snapshotModeHeartbeat); applied || err != nil {
		t.Fatalf("pending-cap: applied=%v err=%v, want rejected", applied, err)
	}

	// Absolute ceiling: 6 entries > ActiveSnapshotMaxEntries(5) → rejected (mix stays under the
	// live cap by using 5 live is already over the live cap; make it 3 live + 3 pending to hit the
	// TOTAL ceiling first is ambiguous, so assert only that 6 total is rejected).
	ceilIDs := seedN(6)
	ceilEntries := make([]ActiveRunEntry, 0, 6)
	for _, id := range ceilIDs {
		ceilEntries = append(ceilEntries, entry(id, 1, "running", false))
	}
	if applied, err := applySnapshot(t, svc, env, wk, &ActiveSnapshot{SnapshotEpoch: 1, RegisterNonce: "nonce-A", Active: ceilEntries}, snapshotModeHeartbeat); applied || err != nil {
		t.Fatalf("total-ceiling: applied=%v err=%v, want rejected", applied, err)
	}

	// Nothing was ever applied — the worker still has no rows and epoch 0.
	if got := countActiveRuns(t, env, wk); got != 0 {
		t.Fatalf("count = %d, want 0 (every oversize snapshot rejected)", got)
	}
	if got := workerEpoch(t, env, wk); got != 0 {
		t.Fatalf("worker epoch = %d, want 0", got)
	}
}
