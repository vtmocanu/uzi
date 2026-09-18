package workersvc

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres. These pin PRD #1390 M2b's
// bidirectional heartbeat reconciliation end to end through Service.Heartbeat: ReadoptRunsFromSnapshot
// (restore a queued run the worker still lists to its exact phase, D5/D2) and
// Fail/RequeueRunsMissingFromSnapshot (a running run the worker owns but no longer lists, SC2).

// ---- small column readers -------------------------------------------------

func budgetPausedOf(t *testing.T, env codexTestEnv, id uuid.UUID) int {
	t.Helper()
	var v int
	if err := env.pool.QueryRow(env.ctx, `SELECT budget_paused_seconds FROM runs WHERE id = $1`, id).Scan(&v); err != nil {
		t.Fatalf("read budget_paused_seconds: %v", err)
	}
	return v
}

func requeueCountOf(t *testing.T, env codexTestEnv, id uuid.UUID) int {
	t.Helper()
	var v int
	if err := env.pool.QueryRow(env.ctx, `SELECT requeue_count FROM runs WHERE id = $1`, id).Scan(&v); err != nil {
		t.Fatalf("read requeue_count: %v", err)
	}
	return v
}

// staleReqGenOf returns (value, valid). valid=false means the column is NULL.
func staleReqGenOf(t *testing.T, env codexTestEnv, id uuid.UUID) (int64, bool) {
	t.Helper()
	var v *int64
	if err := env.pool.QueryRow(env.ctx, `SELECT stale_requeue_generation FROM runs WHERE id = $1`, id).Scan(&v); err != nil {
		t.Fatalf("read stale_requeue_generation: %v", err)
	}
	if v == nil {
		return 0, false
	}
	return *v, true
}

func failOriginOf(t *testing.T, env codexTestEnv, id uuid.UUID) string {
	t.Helper()
	var v *string
	if err := env.pool.QueryRow(env.ctx, `SELECT fail_origin FROM runs WHERE id = $1`, id).Scan(&v); err != nil {
		t.Fatalf("read fail_origin: %v", err)
	}
	if v == nil {
		return ""
	}
	return *v
}

// hbWorker reads the current worker row for a Heartbeat call.
func hbWorker(t *testing.T, env codexTestEnv, workerID uuid.UUID) store.Worker {
	t.Helper()
	wkr, err := env.q.GetWorkerByID(env.ctx, workerID)
	if err != nil {
		t.Fatalf("GetWorkerByID: %v", err)
	}
	return wkr
}

// ---- readopt (D5, D2) -----------------------------------------------------

// TestReadoptRestoresEachPhaseLiveDB: a queued run the worker still lists as a LIVE entry at the
// same generation is restored to its listed phase; the queued interval is banked into
// budget_paused_seconds ONLY for awaiting_approval/awaiting_input, never for running/awaiting_followup.
func TestReadoptRestoresEachPhaseLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, _, repoID := env.seedCodexInfra(t)
	svc := snapshotSvc(env, testParams())

	cases := []struct {
		phase    string
		wantBank bool
	}{
		{"running", false},
		{"awaiting_approval", true},
		{"awaiting_input", true},
		{"awaiting_followup", false},
	}
	for i, c := range cases {
		t.Run(c.phase, func(t *testing.T) {
			wk := seedSnapshotWorker(t, env, userID, "nonce-A")
			run := seedOutageRun(t, env, userID, repoID, wk, "queued", "issue", 1, 0)

			snap := &ActiveSnapshot{
				SnapshotEpoch: 1, RegisterNonce: "nonce-A",
				Active: []ActiveRunEntry{entry(run, 1, c.phase, false)},
			}
			if _, err := svc.Heartbeat(env.ctx, hbWorker(t, env, wk), nil, nil, snap); err != nil {
				t.Fatalf("Heartbeat: %v", err)
			}
			if got := statusOf(t, env, run); got != c.phase {
				t.Fatalf("run status = %q, want restored to %q", got, c.phase)
			}
			banked := budgetPausedOf(t, env, run)
			if c.wantBank && banked <= 0 {
				t.Fatalf("[%d] budget_paused_seconds = %d, want > 0 (queued interval banked for %s)", i, banked, c.phase)
			}
			if !c.wantBank && banked != 0 {
				t.Fatalf("[%d] budget_paused_seconds = %d, want 0 (never banked for %s)", i, banked, c.phase)
			}
		})
	}
}

// TestReadoptRefundProvenanceLiveDB (D2): requeue_count is refunded (-1, floored at 0) ONLY when
// stale_requeue_generation = claim_generation; a NULL or mismatched provenance never refunds. After
// a readopt stale_requeue_generation is always cleared.
func TestReadoptRefundProvenanceLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, _, repoID := env.seedCodexInfra(t)
	svc := snapshotSvc(env, testParams())

	// setProvenance stamps stale_requeue_generation (nil clears it to NULL).
	setProvenance := func(run uuid.UUID, gen *int64) {
		if gen == nil {
			env.exec(`UPDATE runs SET stale_requeue_generation = NULL WHERE id = $1`, run)
		} else {
			env.exec(`UPDATE runs SET stale_requeue_generation = $2 WHERE id = $1`, run, *gen)
		}
	}
	g1 := int64(1)
	g7 := int64(7)

	cases := []struct {
		name       string
		requeue    int32
		provenance *int64
		wantCount  int
	}{
		{"matching provenance refunds", 2, &g1, 1},
		{"matching provenance never below zero", 0, &g1, 0},
		{"null provenance never refunds", 1, nil, 1},
		{"mismatched provenance never refunds", 1, &g7, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wk := seedSnapshotWorker(t, env, userID, "nonce-A")
			run := seedOutageRun(t, env, userID, repoID, wk, "queued", "issue", 1, c.requeue)
			setProvenance(run, c.provenance)

			snap := &ActiveSnapshot{SnapshotEpoch: 1, RegisterNonce: "nonce-A", Active: []ActiveRunEntry{entry(run, 1, "running", false)}}
			if _, err := svc.Heartbeat(env.ctx, hbWorker(t, env, wk), nil, nil, snap); err != nil {
				t.Fatalf("Heartbeat: %v", err)
			}
			if got := requeueCountOf(t, env, run); got != c.wantCount {
				t.Fatalf("requeue_count = %d, want %d", got, c.wantCount)
			}
			if _, valid := staleReqGenOf(t, env, run); valid {
				t.Fatal("stale_requeue_generation still set after readopt; it must be cleared")
			}
		})
	}
}

// TestReadoptSkipsReleasedClaimLiveDB (#1247 fence): a queued run whose claim_released_at is set is
// NEVER restored — dropping the claim_released_at predicate from the query reddens THIS test.
func TestReadoptSkipsReleasedClaimLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, _, repoID := env.seedCodexInfra(t)
	svc := snapshotSvc(env, testParams())

	wk := seedSnapshotWorker(t, env, userID, "nonce-A")
	run := seedOutageRun(t, env, userID, repoID, wk, "queued", "issue", 1, 0)
	env.exec(`UPDATE runs SET claim_released_at = now() WHERE id = $1`, run)

	snap := &ActiveSnapshot{SnapshotEpoch: 1, RegisterNonce: "nonce-A", Active: []ActiveRunEntry{entry(run, 1, "running", false)}}
	if _, err := svc.Heartbeat(env.ctx, hbWorker(t, env, wk), nil, nil, snap); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if got := statusOf(t, env, run); got != "queued" {
		t.Fatalf("released-claim run status = %q, want unchanged queued (the #1247 fence must block the revive)", got)
	}
}

// TestReadoptSkipsWrongGenerationLiveDB (D4): a queued run listed at a DIFFERENT generation is not
// restored; the same worker's run listed at the matching generation IS restored, as the contrast.
func TestReadoptSkipsWrongGenerationLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, _, repoID := env.seedCodexInfra(t)
	svc := snapshotSvc(env, testParams())

	wk := seedSnapshotWorker(t, env, userID, "nonce-A")
	mismatch := seedOutageRun(t, env, userID, repoID, wk, "queued", "issue", 2, 0) // run at gen 2
	matching := seedOutageRun(t, env, userID, repoID, wk, "queued", "issue", 3, 0) // run at gen 3

	snap := &ActiveSnapshot{SnapshotEpoch: 1, RegisterNonce: "nonce-A", Active: []ActiveRunEntry{
		entry(mismatch, 1, "running", false), // listed at gen 1 ≠ run's gen 2
		entry(matching, 3, "awaiting_input", false),
	}}
	if _, err := svc.Heartbeat(env.ctx, hbWorker(t, env, wk), nil, nil, snap); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if got := statusOf(t, env, mismatch); got != "queued" {
		t.Fatalf("wrong-generation run status = %q, want unchanged queued (generation must match)", got)
	}
	if got := statusOf(t, env, matching); got != "awaiting_input" {
		t.Fatalf("matching-generation run status = %q, want restored to awaiting_input", got)
	}
}

// TestReadoptIgnoresForeignWorkerRunLiveDB: another worker's run is never restored by this worker's
// heartbeat — the ownership drop in ReplaceWorkerActiveRuns removes the entry, and readopt is
// scoped to r.worker_id = @worker_id.
func TestReadoptIgnoresForeignWorkerRunLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, _, repoID := env.seedCodexInfra(t)
	svc := snapshotSvc(env, testParams())

	wk := seedSnapshotWorker(t, env, userID, "nonce-A")
	other := seedSnapshotWorker(t, env, userID, "nonce-B")
	foreign := seedOutageRun(t, env, userID, repoID, other, "queued", "issue", 1, 0)

	snap := &ActiveSnapshot{SnapshotEpoch: 1, RegisterNonce: "nonce-A", Active: []ActiveRunEntry{entry(foreign, 1, "running", false)}}
	if _, err := svc.Heartbeat(env.ctx, hbWorker(t, env, wk), nil, nil, snap); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if got := statusOf(t, env, foreign); got != "queued" {
		t.Fatalf("foreign run status = %q, want unchanged queued (a worker never restores another's run)", got)
	}
}

// TestReadoptLeavesHeldStateContentLiveDB (D5, fact 4): a readopt is a status-only write — the
// held-state content columns (here open_question_id) that survived the stale requeue are UNTOUCHED.
func TestReadoptLeavesHeldStateContentLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, _, repoID := env.seedCodexInfra(t)
	svc := snapshotSvc(env, testParams())

	wk := seedSnapshotWorker(t, env, userID, "nonce-A")
	run := seedOutageRun(t, env, userID, repoID, wk, "queued", "issue", 1, 0)
	env.exec(`UPDATE runs SET open_question_id = 'q-held-123' WHERE id = $1`, run)

	snap := &ActiveSnapshot{SnapshotEpoch: 1, RegisterNonce: "nonce-A", Active: []ActiveRunEntry{entry(run, 1, "awaiting_input", false)}}
	if _, err := svc.Heartbeat(env.ctx, hbWorker(t, env, wk), nil, nil, snap); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if got := statusOf(t, env, run); got != "awaiting_input" {
		t.Fatalf("run status = %q, want awaiting_input", got)
	}
	var oqid *string
	if err := env.pool.QueryRow(env.ctx, `SELECT open_question_id FROM runs WHERE id = $1`, run).Scan(&oqid); err != nil {
		t.Fatalf("read open_question_id: %v", err)
	}
	if oqid == nil || *oqid != "q-held-123" {
		t.Fatalf("open_question_id = %v, want unchanged 'q-held-123' (held-state content must survive the readopt)", oqid)
	}
}

// TestReadoptPublishesPostCommitLiveDB: a readopt transition is fanned out through the broadcaster
// (post-commit), so the board updates. The spy proves the exact (run, restored-phase) event landed.
func TestReadoptPublishesPostCommitLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, _, repoID := env.seedCodexInfra(t)
	svc := snapshotSvc(env, testParams())
	spy := &stateSpy{}
	svc.SetBroadcaster(spy)

	wk := seedSnapshotWorker(t, env, userID, "nonce-A")
	run := seedOutageRun(t, env, userID, repoID, wk, "queued", "issue", 1, 0)

	snap := &ActiveSnapshot{SnapshotEpoch: 1, RegisterNonce: "nonce-A", Active: []ActiveRunEntry{entry(run, 1, "awaiting_approval", false)}}
	if _, err := svc.Heartbeat(env.ctx, hbWorker(t, env, wk), nil, nil, snap); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if st, ok := spy.statusFor(run); !ok || st != "awaiting_approval" {
		t.Fatalf("readopt publish = (%q,%v), want (awaiting_approval,true)", st, ok)
	}
}

// ---- missing path (SC2) ---------------------------------------------------

// TestMissingRequeuedUnderCapLiveDB: a running run this worker owns but no longer lists, past the
// fence and within budget, is requeued — and stale_requeue_generation stays NULL (a genuine loss is
// never refunded, D2). The transition is published post-commit.
func TestMissingRequeuedUnderCapLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, _, repoID := env.seedCodexInfra(t)
	svc := snapshotSvc(env, testParams())
	spy := &stateSpy{}
	svc.SetBroadcaster(spy)

	wk := seedSnapshotWorker(t, env, userID, "nonce-A")
	run := seedOutageRun(t, env, userID, repoID, wk, "running", "issue", 1, 0)

	// An empty valid snapshot is APPLIED (applied=true) and lists nothing, so the running run is
	// absent and the reconciliation requeues it.
	snap := &ActiveSnapshot{SnapshotEpoch: 1, RegisterNonce: "nonce-A", Active: []ActiveRunEntry{}}
	if _, err := svc.Heartbeat(env.ctx, hbWorker(t, env, wk), nil, nil, snap); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if got := statusOf(t, env, run); got != "queued" {
		t.Fatalf("missing run status = %q, want requeued to queued", got)
	}
	if got := requeueCountOf(t, env, run); got != 1 {
		t.Fatalf("requeue_count = %d, want 1", got)
	}
	if _, valid := staleReqGenOf(t, env, run); valid {
		t.Fatal("stale_requeue_generation set after a missing requeue; a genuine loss is never refunded (D2)")
	}
	if st, ok := spy.statusFor(run); !ok || st != "queued" {
		t.Fatalf("missing-requeue publish = (%q,%v), want (queued,true)", st, ok)
	}
}

// TestMissingFailedOverCapFailFirstLiveDB: over-cap missing run is FAILED (fail-first) while an
// under-cap sibling in the SAME heartbeat is requeued — proving the fail-before-requeue ordering and
// that the fail funnels the worker-lost transition to the judge/broadcaster.
func TestMissingFailedOverCapFailFirstLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, _, repoID := env.seedCodexInfra(t)
	svc := snapshotSvc(env, testParams())
	spy := &stateSpy{}
	svc.SetBroadcaster(spy)

	wk := seedSnapshotWorker(t, env, userID, "nonce-A")
	overCap := seedOutageRun(t, env, userID, repoID, wk, "running", "issue", 1, 1)  // requeue_count == max → failed
	underCap := seedOutageRun(t, env, userID, repoID, wk, "running", "issue", 1, 0) // within budget → requeued

	snap := &ActiveSnapshot{SnapshotEpoch: 1, RegisterNonce: "nonce-A", Active: []ActiveRunEntry{}}
	if _, err := svc.Heartbeat(env.ctx, hbWorker(t, env, wk), nil, nil, snap); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if got := statusOf(t, env, overCap); got != "failed" {
		t.Fatalf("over-cap missing run status = %q, want failed", got)
	}
	if got := failOriginOf(t, env, overCap); got != "worker_lost" {
		t.Fatalf("over-cap fail_origin = %q, want worker_lost", got)
	}
	if got := statusOf(t, env, underCap); got != "queued" {
		t.Fatalf("under-cap missing run status = %q, want requeued to queued", got)
	}
	if st, ok := spy.statusFor(overCap); !ok || st != "failed" {
		t.Fatalf("missing-fail publish = (%q,%v), want (failed,true)", st, ok)
	}
	if st, ok := spy.statusFor(underCap); !ok || st != "queued" {
		t.Fatalf("missing-requeue publish = (%q,%v), want (queued,true)", st, ok)
	}
}

// TestMissingFenceRaceLeavesJustClaimedRunLiveDB (D4): a running run whose status_since is fresh (a
// just-claimed run whose first snapshot has not yet arrived) is NOT requeued — the missing fence is
// the stale window plus one heartbeat interval.
func TestMissingFenceRaceLeavesJustClaimedRunLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, _, repoID := env.seedCodexInfra(t)
	svc := snapshotSvc(env, testParams())

	wk := seedSnapshotWorker(t, env, userID, "nonce-A")
	run := seedOutageRun(t, env, userID, repoID, wk, "running", "issue", 1, 0)
	// Fresh status_since: inside the fence (60s), so the run is a just-claimed race, not a loss.
	env.exec(`UPDATE runs SET status_since = now() WHERE id = $1`, run)

	snap := &ActiveSnapshot{SnapshotEpoch: 1, RegisterNonce: "nonce-A", Active: []ActiveRunEntry{}}
	if _, err := svc.Heartbeat(env.ctx, hbWorker(t, env, wk), nil, nil, snap); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if got := statusOf(t, env, run); got != "running" {
		t.Fatalf("just-claimed run status = %q, want unchanged running (inside the missing fence)", got)
	}
}

// TestMissingIgnoresChatRunLiveDB (D10): a live chat run is never requeued by absence — the missing
// query carries kind <> 'chat' (chat has its own sweeps).
func TestMissingIgnoresChatRunLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, _, repoID := env.seedCodexInfra(t)
	svc := snapshotSvc(env, testParams())

	wk := seedSnapshotWorker(t, env, userID, "nonce-A")
	chatRun := seedOutageChatRun(t, env, userID, wk, 1, 0)
	// A non-chat sibling as the contrast: it IS requeued by absence.
	issueRun := seedOutageRun(t, env, userID, repoID, wk, "running", "issue", 1, 0)

	snap := &ActiveSnapshot{SnapshotEpoch: 1, RegisterNonce: "nonce-A", Active: []ActiveRunEntry{}}
	if _, err := svc.Heartbeat(env.ctx, hbWorker(t, env, wk), nil, nil, snap); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if got := statusOf(t, env, chatRun); got != "running" {
		t.Fatalf("chat run status = %q, want unchanged running (D10: chat is never requeued by absence)", got)
	}
	if got := statusOf(t, env, issueRun); got != "queued" {
		t.Fatalf("issue sibling status = %q, want requeued (the missing path targets the run lane)", got)
	}
}

// TestMissingProtectedByOverflowLiveDB (D11): a worker flagged pending_overflow (unexpired) closes
// its owned runs to the missing path even with NO worker_active_runs row of their own; expiry
// reopens it. Driven through the direct query so the worker-level closure is isolated from the
// snapshot replace.
func TestMissingProtectedByOverflowLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, _, repoID := env.seedCodexInfra(t)

	missingCutoff := pgconv.Time(time.Now().Add(-60 * time.Second))
	fireRequeue := func(t *testing.T, workerID uuid.UUID) {
		t.Helper()
		if _, err := env.q.RequeueRunsMissingFromSnapshot(env.ctx, store.RequeueRunsMissingFromSnapshotParams{
			WorkerID:      pgconv.UUID(workerID),
			MissingCutoff: missingCutoff,
			MaxRequeues:   1,
		}); err != nil {
			t.Fatalf("RequeueRunsMissingFromSnapshot: %v", err)
		}
	}

	t.Run("unexpired overflow protects", func(t *testing.T) {
		wk := seedSnapshotWorker(t, env, userID, "nonce-A")
		setOverflow(t, env, wk, "1 hour")
		run := seedOutageRun(t, env, userID, repoID, wk, "running", "issue", 1, 0)
		fireRequeue(t, wk)
		if got := statusOf(t, env, run); got != "running" {
			t.Fatalf("run under overflow status = %q, want unchanged running (closure protects the missing path)", got)
		}
	})

	t.Run("expired overflow sweeps", func(t *testing.T) {
		wk := seedSnapshotWorker(t, env, userID, "nonce-A")
		setOverflow(t, env, wk, "-1 minute")
		run := seedOutageRun(t, env, userID, repoID, wk, "running", "issue", 1, 0)
		fireRequeue(t, wk)
		if got := statusOf(t, env, run); got != "queued" {
			t.Fatalf("run under expired overflow status = %q, want requeued (closure lapsed)", got)
		}
	})
}

// TestMissingProtectedByTerminalPendingLeaseLiveDB (D11): a run the worker lists as terminal_pending
// at its current generation is NOT requeued by the missing path — through the heartbeat the pending
// entry keeps the run listed (protected), while dropping it from the next snapshot lets the loss be
// swept. This is the user-facing form of the defense-in-depth lease predicate (which, under a
// full snapshot replace, overlaps the absent-check — the sweeper-side lease has its own isolated
// coverage in TestD11TerminalPendingLeaseProtectsAllWritersLiveDB).
func TestMissingProtectedByTerminalPendingLeaseLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, _, repoID := env.seedCodexInfra(t)
	svc := snapshotSvc(env, testParams())

	wk := seedSnapshotWorker(t, env, userID, "nonce-A")
	run := seedOutageRun(t, env, userID, repoID, wk, "running", "issue", 1, 0)

	// Heartbeat 1 lists the run as terminal_pending at its current generation: its outcome is
	// journaled on the worker (#1391), so it must not be requeued.
	pending := &ActiveSnapshot{SnapshotEpoch: 1, RegisterNonce: "nonce-A", Active: []ActiveRunEntry{entry(run, 1, "running", true)}}
	if _, err := svc.Heartbeat(env.ctx, hbWorker(t, env, wk), nil, nil, pending); err != nil {
		t.Fatalf("Heartbeat (pending): %v", err)
	}
	if got := statusOf(t, env, run); got != "running" {
		t.Fatalf("terminal-pending run status = %q, want unchanged running (lease protects it)", got)
	}

	// Heartbeat 2 no longer lists it (the outcome was accepted and the worker really lost it): the
	// absent run is now requeued.
	absent := &ActiveSnapshot{SnapshotEpoch: 2, RegisterNonce: "nonce-A", Active: []ActiveRunEntry{}}
	if _, err := svc.Heartbeat(env.ctx, hbWorker(t, env, wk), nil, nil, absent); err != nil {
		t.Fatalf("Heartbeat (absent): %v", err)
	}
	if got := statusOf(t, env, run); got != "queued" {
		t.Fatalf("now-absent run status = %q, want requeued to queued", got)
	}
}

// TestInvalidSnapshotSkipsReconciliationLiveDB: an ignored snapshot (applied=false, here a
// wrong-nonce heartbeat) drives NO reconciliation — a queued run it "lists" is not readopted and a
// running run it omits is not requeued — while the heartbeat's liveness still refreshes (never a 400).
func TestInvalidSnapshotSkipsReconciliationLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, _, repoID := env.seedCodexInfra(t)
	svc := snapshotSvc(env, testParams())

	wk := seedSnapshotWorker(t, env, userID, "nonce-A")
	env.exec(`UPDATE workers SET last_heartbeat_at = now() - interval '30 seconds' WHERE id = $1`, wk)
	queuedRun := seedOutageRun(t, env, userID, repoID, wk, "queued", "issue", 1, 0)
	runningRun := seedOutageRun(t, env, userID, repoID, wk, "running", "issue", 1, 0)

	// Wrong nonce ⇒ applied=false ⇒ no rows written, no reconciliation.
	bad := &ActiveSnapshot{SnapshotEpoch: 1, RegisterNonce: "nonce-WRONG", Active: []ActiveRunEntry{entry(queuedRun, 1, "running", false)}}
	if _, err := svc.Heartbeat(env.ctx, hbWorker(t, env, wk), nil, nil, bad); err != nil {
		t.Fatalf("Heartbeat with invalid snapshot returned error: %v (must never fail the heartbeat)", err)
	}
	if got := statusOf(t, env, queuedRun); got != "queued" {
		t.Fatalf("queued run status = %q, want unchanged queued (no readopt on an ignored snapshot)", got)
	}
	if got := statusOf(t, env, runningRun); got != "running" {
		t.Fatalf("running run status = %q, want unchanged running (no missing requeue on an ignored snapshot)", got)
	}
	assertHeartbeatFresh(t, env, wk)
}

// TestHeartbeatReconcileDoesNotDeadlockLiveDB: the canonical pre-lock (LockOwnedRunsByIDs) blocks on
// a concurrently-held FOR UPDATE of a listed run and then proceeds once that transaction commits —
// it does not deadlock or error. A lightweight interleave; the full Heartbeat-vs-Claim two-phase
// test is deferred to M3 (the claim side does not yet take the pre-lock).
func TestHeartbeatReconcileDoesNotDeadlockLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, _, repoID := env.seedCodexInfra(t)
	svc := snapshotSvc(env, testParams())

	wk := seedSnapshotWorker(t, env, userID, "nonce-A")
	run := seedOutageRun(t, env, userID, repoID, wk, "queued", "issue", 1, 0)

	// A sibling transaction holds the run's row FOR UPDATE.
	tx, err := env.pool.Begin(env.ctx)
	if err != nil {
		t.Fatalf("begin sibling tx: %v", err)
	}
	if _, err := tx.Exec(env.ctx, `SELECT id FROM runs WHERE id = $1 FOR UPDATE`, run); err != nil {
		_ = tx.Rollback(env.ctx)
		t.Fatalf("sibling FOR UPDATE: %v", err)
	}

	// Release the sibling lock shortly, then the heartbeat's pre-lock proceeds.
	go func() {
		time.Sleep(300 * time.Millisecond)
		_ = tx.Commit(env.ctx)
	}()

	done := make(chan error, 1)
	go func() {
		snap := &ActiveSnapshot{SnapshotEpoch: 1, RegisterNonce: "nonce-A", Active: []ActiveRunEntry{entry(run, 1, "running", false)}}
		_, herr := svc.Heartbeat(env.ctx, hbWorker(t, env, wk), nil, nil, snap)
		done <- herr
	}()

	select {
	case herr := <-done:
		if herr != nil {
			t.Fatalf("Heartbeat under a concurrent FOR UPDATE returned error: %v", herr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Heartbeat did not complete within 10s — a deadlock or lost wakeup on the pre-lock")
	}
	if got := statusOf(t, env, run); got != "running" {
		t.Fatalf("run status = %q, want restored to running once the sibling lock released", got)
	}
}
