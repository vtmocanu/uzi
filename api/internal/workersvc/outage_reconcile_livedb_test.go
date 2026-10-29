package workersvc

import (
	"bytes"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/capability"
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

// TestMissingCompletionHoldDeferralLifecycleLiveDB follows an unresolved completion-hold
// deferral through presence, fenced absence, and under-cap requeue while a paused sibling stays held.
func TestMissingCompletionHoldDeferralLifecycleLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, _, repoID := env.seedCodexInfra(t)
	p := testParams()
	svc := snapshotSvc(env, p)
	initialTime := time.Now().UTC().Truncate(time.Microsecond)
	now := initialTime
	svc.SetClock(func() time.Time { return now })
	spy := &stateSpy{}
	svc.SetBroadcaster(spy)

	wk := seedSnapshotWorker(t, env, userID, "nonce-A")
	const generation int64 = 3
	run := seedOutageRun(t, env, userID, repoID, wk, "running", "issue", generation, 0)
	held := seedOutageRun(t, env, userID, repoID, wk, "paused", "issue", generation, 0)
	// seedOutageRun backdates status_since; restore a fresh resumed claim for the fence.
	// A completion hold retains its content until completion, including during a resume.
	for _, id := range []uuid.UUID{run, held} {
		env.exec(`UPDATE runs SET status_since = $2, hold_reason = 'completion_blocked',
		          hold_captured_head = 'completion-head', completion_attempts = 1 WHERE id = $1`, id, now)
	}
	env.exec(`UPDATE runs SET claim_released_at = $2 WHERE id = $1`, held, now)

	assertRun := func(t *testing.T, id uuid.UUID, wantStatus string, wantCount int, wantReleased bool) {
		t.Helper()
		var status, reason, head string
		var gen int64
		var count, attempts int
		var released, finished bool
		var releaseAt *time.Time
		if err := env.pool.QueryRow(env.ctx, `SELECT status, claim_generation, requeue_count,
		        COALESCE(hold_reason, ''), COALESCE(hold_captured_head, ''), completion_attempts,
		        claim_released_at IS NOT NULL, finished_at IS NOT NULL, claim_released_at
		        FROM runs WHERE id = $1`, id).Scan(
			&status, &gen, &count, &reason, &head, &attempts, &released, &finished, &releaseAt); err != nil {
			t.Fatalf("read lifecycle run: %v", err)
		}
		if status != wantStatus || gen != generation || count != wantCount || released != wantReleased || finished {
			t.Fatalf("run %s: status=%q generation=%d requeues=%d released=%v finished=%v; want %q/%d/%d/%v/false",
				id, status, gen, count, released, finished, wantStatus, generation, wantCount, wantReleased)
		}
		if reason != "completion_blocked" || head != "completion-head" || attempts != 1 {
			t.Fatalf("completion hold changed: reason=%q head=%q attempts=%d", reason, head, attempts)
		}
		if id == held && (releaseAt == nil || !releaseAt.Equal(initialTime)) {
			t.Fatalf("paused sibling claim release changed: %v, want %v", releaseAt, initialTime)
		}
		if _, valid := staleReqGenOf(t, env, id); valid {
			t.Fatal("missing lifecycle fabricated stale requeue provenance")
		}
	}
	heartbeat := func(t *testing.T, epoch int64, active []ActiveRunEntry) {
		t.Helper()
		_, err := svc.Heartbeat(env.ctx, hbWorker(t, env, wk), nil, nil,
			&ActiveSnapshot{SnapshotEpoch: epoch, RegisterNonce: "nonce-A", Active: active})
		if err != nil {
			t.Fatalf("Heartbeat epoch %d: %v", epoch, err)
		}
		if got := hbWorker(t, env, wk).SnapshotEpoch; got != epoch {
			t.Fatalf("persisted snapshot epoch: got %d, want %d", got, epoch)
		}
	}
	assertHeld := func(t *testing.T) {
		t.Helper()
		assertRun(t, held, "paused", 0, true)
		if status, ok := spy.statusFor(held); ok {
			t.Fatalf("paused sibling published a transition to %q", status)
		}
	}
	assertNoTransition := func(t *testing.T) {
		t.Helper()
		if status, ok := spy.statusFor(run); ok {
			t.Fatalf("deferred run published a transition to %q", status)
		}
	}

	if !t.Run("present_exact_generation_control", func(t *testing.T) {
		heartbeat(t, 1, []ActiveRunEntry{entry(run, generation, "running", false)})
		row, ok := readActiveRun(t, env, wk, run)
		if !ok || row.gen != generation || row.phase != "running" || row.epoch != 1 ||
			row.terminalPending || row.untilValid {
			t.Fatalf("present snapshot = %+v, exists=%v; want exact-generation live running without terminal pending", row, ok)
		}
		assertRun(t, run, "running", 0, false)
		assertHeld(t)
		assertNoTransition(t)
	}) {
		return
	}
	if !t.Run("empty_before_missing_fence_defers", func(t *testing.T) {
		now = initialTime.Add(p.WorkerHeartbeatInterval)
		heartbeat(t, 2, []ActiveRunEntry{})
		if got := countActiveRuns(t, env, wk); got != 0 {
			t.Fatalf("empty snapshot retained %d active rows (including terminal pending)", got)
		}
		assertRun(t, run, "running", 0, false)
		assertHeld(t)
		assertNoTransition(t)
	}) {
		return
	}
	t.Run("newer_empty_after_missing_fence_requeues_control", func(t *testing.T) {
		now = initialTime.Add(p.WorkerHeartbeatStale + p.WorkerHeartbeatInterval + time.Second)
		heartbeat(t, 3, []ActiveRunEntry{})
		assertRun(t, run, "queued", 1, false)
		assertHeld(t)
		if got := countActiveRuns(t, env, wk); got != 0 {
			t.Fatalf("requeue fabricated %d active rows (including terminal pending)", got)
		}
		if status, ok := spy.statusFor(run); !ok || status != "queued" || len(spy.events) != 1 {
			t.Fatalf("requeue broadcast = (%q,%v), events=%v; want one queued transition", status, ok, spy.events)
		}
	})
	t.Run("paused_completion_blocked_control", func(t *testing.T) {
		assertHeld(t)
	})
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
			WorkerID:             pgconv.UUID(workerID),
			MissingCutoff:        missingCutoff,
			MaxRequeues:          1,
			Now:                  pgconv.Time(time.Now()),
			GlobalTimeoutSeconds: int32(testParams().RunTimeout.Seconds()),
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

// outageCodexFlight starts at generation zero. Credential/binding fixtures do not
// prove a claim: only Service.Claim below supplies the capability used by these tests.
type outageCodexFlight struct {
	env     codexTestEnv
	f       codexRunFixture
	p       *ClaimPayload
	initial store.Run
}

func newOutageCodexFlight(t *testing.T, mode string) outageCodexFlight {
	t.Helper()
	env := setupCodexLiveDB(t)
	var f codexRunFixture
	if mode == codexAuthModeAPIKey {
		f = newAPIKeyFixture(t, env)
	} else {
		f = newSubscriptionFixture(t, env)
	}
	resealBotPAT(t, env, f.userID)
	env.exec("UPDATE runs SET status='queued', worker_id=NULL WHERE id=$1", f.runID)
	before := mustRun(t, env, f.runID)
	if before.ClaimGeneration != 0 || before.CodexClaimEpoch != 0 || len(before.CodexCapHash) != 0 {
		t.Fatal("initial fixture must not seed generation, epoch or capability")
	}
	f.svc = New(env.q, env.box, testParams())
	f.svc.SetVault(env.vlt)
	f.svc.SetTxBeginner(env.pool)
	w, nonce, err := f.svc.Register(env.ctx, hbWorker(t, env, f.workerID), "m1-test", "base", nil, nil,
		[]string{capability.CodexHarnessV1, capability.CodexRuntimeV2, capability.RecoveryArchiveV1, capability.CodexAccountParkV1}, nil)
	if err != nil || nonce == "" {
		t.Fatalf("initial Register: nonce present=%v err=%v", nonce != "", err)
	}
	f.wkr = w
	payload, err := f.svc.Claim(env.ctx, w, nil)
	if err != nil || payload == nil || payload.RunID != f.runID.String() || payload.Secrets.Codex == nil || payload.Secrets.Codex.Capability == "" {
		t.Fatalf("initial Service.Claim: payload=%v err=%v", payload != nil, err)
	}
	initial := mustRun(t, env, f.runID)
	if initial.ClaimGeneration != 1 || initial.ClaimReleasedAt.Valid || len(initial.CodexCapHash) == 0 {
		t.Fatal("initial real claim did not install generation one authority")
	}
	if payload.Secrets.Codex.AccessToken != f.accessToken {
		t.Fatal("initial claim did not return the bound access credential")
	}
	x := outageCodexFlight{env: env, f: f, p: payload, initial: initial}
	x.assertAuthority(t, w, payload.Secrets.Codex.Capability, nil)
	return x
}

func (x outageCodexFlight) scopes() []CodexOpScope {
	if x.initial.CodexAuthMode.String == codexAuthModeAPIKey {
		return []CodexOpScope{ScopeReleaseAccessToken}
	}
	return []CodexOpScope{ScopeReleaseAccessToken, ScopeStartRefresh, ScopePersistRecovery}
}

func (x outageCodexFlight) assertAuthority(t *testing.T, w store.Worker, wire string, want error) {
	t.Helper()
	for _, scope := range x.scopes() {
		_, err := x.f.svc.AuthorizeCodexCredentialOp(x.env.ctx, w, x.f.runID, wire, scope)
		if !errors.Is(err, want) {
			t.Errorf("%s authority: got %v, want %v", scope, err, want)
		}
	}
}

func (x outageCodexFlight) beat(t *testing.T, epoch int64, nonce string, active []ActiveRunEntry) {
	t.Helper()
	if _, err := x.f.svc.Heartbeat(x.env.ctx, hbWorker(t, x.env, x.f.workerID), nil, nil,
		&ActiveSnapshot{SnapshotEpoch: epoch, RegisterNonce: nonce, Active: active}); err != nil {
		t.Fatalf("Heartbeat epoch %d: %v", epoch, err)
	}
}

func (x outageCodexFlight) liveEntry(phase string) []ActiveRunEntry {
	return []ActiveRunEntry{entry(x.f.runID, x.initial.ClaimGeneration, phase, false)}
}

// provisional executes the two production sweeper queries, with elapsed-time
// fixtures rather than sleeping or disturbing a shared database.
func (x outageCodexFlight) provisional(t *testing.T, path string) {
	t.Helper()
	x.env.exec("UPDATE workers SET last_heartbeat_at=now()-interval '3 hours' WHERE id=$1", x.f.workerID)
	x.env.exec("UPDATE runs SET claimed_at=now()-interval '3 hours' WHERE id=$1", x.f.runID)
	found := false
	cutoff := pgconv.Time(time.Now().Add(-time.Hour))
	switch path {
	case "stale_worker":
		rows, err := x.env.q.RequeueRunsOfStaleWorkers(x.env.ctx, store.RequeueRunsOfStaleWorkersParams{MaxRequeues: 5, Cutoff: cutoff})
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			if row.ID == x.f.runID {
				found = true
			}
		}
	case "claimed_never_started":
		rows, err := x.env.q.SweepClaimedNeverStarted(x.env.ctx, cutoff)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			if row.ID == x.f.runID {
				found = true
			}
		}
	default:
		t.Fatalf("unknown provisional path %q", path)
	}
	if !found || statusOf(t, x.env, x.f.runID) != "queued" {
		t.Fatal("real provisional requeue did not return the flight")
	}
	if got := claimGenOf(t, x.env, x.f.runID); got != x.initial.ClaimGeneration {
		t.Fatalf("provisional requeue advanced generation to %d", got)
	}
	wantCount := 0
	if path == "stale_worker" {
		wantCount = 1
	}
	if got := requeueCountOf(t, x.env, x.f.runID); got != wantCount {
		t.Fatalf("provisional requeue charge=%d want=%d", got, wantCount)
	}
	gen, valid := staleReqGenOf(t, x.env, x.f.runID)
	if valid != (path == "stale_worker") || (valid && gen != x.initial.ClaimGeneration) {
		t.Fatalf("provisional provenance=%d/%v", gen, valid)
	}
}

// custody reads immutable provenance and live pointers, not only the aggregate.
type outageCustody struct {
	ID, Owner, Run, Worker, LiveWorker, LiveRun uuid.UUID
	Generation                                  int64
	Identity, State                             string
}

func (x outageCodexFlight) custody(t *testing.T) []outageCustody {
	t.Helper()
	rows, err := x.env.pool.Query(x.env.ctx, `SELECT id,user_id,run_id,original_worker_id,
		live_worker_id,live_run_id,generation,original_worker_identity,state
		FROM recovery_custody_holds WHERE run_id=$1 ORDER BY generation,id`, x.f.runID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var result []outageCustody
	for rows.Next() {
		var h outageCustody
		if err := rows.Scan(&h.ID, &h.Owner, &h.Run, &h.Worker, &h.LiveWorker, &h.LiveRun, &h.Generation, &h.Identity, &h.State); err != nil {
			t.Fatal(err)
		}
		result = append(result, h)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestCodexProvisionalReadoptAuthorityLiveDB(t *testing.T) {
	for _, path := range []string{"stale_worker", "claimed_never_started"} {
		for _, mode := range []string{codexAuthModeSubscription, codexAuthModeAPIKey} {
			for _, phase := range []string{"running", "awaiting_approval"} {
				t.Run(path+"/"+mode+"/"+phase, func(t *testing.T) {
					x := newOutageCodexFlight(t, mode)
					holds := x.custody(t)
					if len(holds) != 1 || holds[0].Generation != 1 || holds[0].Worker != x.f.workerID ||
						holds[0].LiveWorker != x.f.workerID || holds[0].LiveRun != x.f.runID ||
						holds[0].Owner != x.f.userID || holds[0].Run != x.f.runID ||
						holds[0].Identity != workerIdentity(x.f.wkr) || holds[0].State != "open" {
						t.Fatalf("initial real claim custody=%+v", holds)
					}
					nonce := hbWorker(t, x.env, x.f.workerID).SnapshotRegisterNonce.String
					x.beat(t, 1, nonce, x.liveEntry(phase))
					x.provisional(t, path)
					// Queued denies every applicable scope; exact state-gate controls
					// below keep an epoch failure from masking that predicate.
					for _, scope := range x.scopes() {
						if _, err := x.f.svc.AuthorizeCodexCredentialOp(x.env.ctx, x.f.wkr, x.f.runID, x.p.Secrets.Codex.Capability, scope); err == nil {
							t.Errorf("queued accepted %s", scope)
						}
					}
					x.beat(t, 2, nonce, x.liveEntry(phase))
					r := mustRun(t, x.env, x.f.runID)
					if r.Status != phase || r.ClaimGeneration != x.initial.ClaimGeneration || r.WorkerID != x.initial.WorkerID ||
						r.ClaimReleasedAt.Valid || r.RequeueCount != 0 || r.StaleRequeueGeneration.Valid {
						t.Fatalf("readopt: status=%s gen=%d count=%d provenance=%v released=%v", r.Status, r.ClaimGeneration, r.RequeueCount, r.StaleRequeueGeneration, r.ClaimReleasedAt)
					}
					if got := x.custody(t); !reflect.DeepEqual(got, holds) {
						t.Fatalf("readopt changed custody: %+v", got)
					}
					beforeRepeat := r
					x.beat(t, 2, nonce, x.liveEntry(phase))
					x.beat(t, 3, nonce, x.liveEntry(phase))
					if !reflect.DeepEqual(beforeRepeat, mustRun(t, x.env, x.f.runID)) || !reflect.DeepEqual(holds, x.custody(t)) {
						t.Fatal("repeated/newer identical snapshot changed run, charge or custody")
					}
					// These assertions must reach authorization AFTER a real readopt.
					// Old SQL independently fails preservation and returns the epoch sentinel.
					t.Run("preserved_capability", func(t *testing.T) {
						if r.CodexClaimEpoch != x.initial.CodexClaimEpoch || !bytes.Equal(r.CodexCapHash, x.initial.CodexCapHash) {
							t.Fatalf("provisional readopt revoked capability: epoch %d -> %d hash retained=%v", x.initial.CodexClaimEpoch, r.CodexClaimEpoch, bytes.Equal(r.CodexCapHash, x.initial.CodexCapHash))
						}
					})
					for _, scope := range x.scopes() {
						t.Run(scope.String(), func(t *testing.T) {
							if _, err := x.f.svc.AuthorizeCodexCredentialOp(x.env.ctx, x.f.wkr, x.f.runID, x.p.Secrets.Codex.Capability, scope); err != nil {
								t.Fatalf("after real %s readopt: %s: %v (ErrCodexCapabilityEpoch=%v)", path, scope, err, errors.Is(err, ErrCodexCapabilityEpoch))
							}
							if scope == ScopeReleaseAccessToken {
								got, err := x.f.svc.ReleaseCodexCredential(x.env.ctx, x.f.wkr, x.f.runID, x.p.Secrets.Codex.Capability)
								if err != nil || got.AccessToken != x.f.accessToken {
									t.Fatalf("release after readopt: %v token matches=%v", err, got.AccessToken == x.f.accessToken)
								}
							}
						})
					}
				})
			}
		}
	}
}

func TestCodexOutageAuthorityNegativeControlsLiveDB(t *testing.T) {
	for _, state := range []string{"wrong_worker", "released", "queued", "completed", "failed", "cancelled", "limit_park", "account_park"} {
		t.Run(state, func(t *testing.T) {
			x := newOutageCodexFlight(t, codexAuthModeSubscription)
			w := x.f.wkr
			want := ErrCodexRunNotActivelyClaimed
			switch state {
			case "wrong_worker":
				w.ID = seedSnapshotWorker(t, x.env, x.f.userID, "foreign-"+uuid.NewString())
				want = ErrCodexWorkerMismatch
			case "released":
				x.env.exec("UPDATE runs SET claim_released_at=now() WHERE id=$1", x.f.runID)
			case "limit_park", "account_park":
				gen := x.initial.ClaimGeneration
				if _, applied, err := x.f.svc.SetState(x.env.ctx, w, x.f.runID, StateRequest{State: "running", ClaimGeneration: &gen}); err != nil || !applied {
					t.Fatalf("start: %v/%v", applied, err)
				}
				if state == "limit_park" {
					x.f.svc.p.RunLimitMaxWaits = 5
					x.f.svc.p.RunLimitMaxPark = 8 * 24 * time.Hour
					x.env.exec("UPDATE runs SET wait_on_limit=true WHERE id=$1", x.f.runID)
					parkCodexFlight(t, x.env, x.f.svc, w, x.f.runID, gen)
				} else {
					x.env.quarantineAccount(t, x.f.aliasID)
					_, applied, err := x.f.svc.SetState(x.env.ctx, w, x.f.runID, StateRequest{
						State: "recovery_wait", RecoveryCause: strPtr(recoveryCauseCodexAccountUnavailable),
						ClaimGeneration: &gen, CheckpointContainsLatest: boolPtr(true), SessionID: strPtr("outage-park"),
					})
					if err != nil || !applied {
						t.Fatalf("account park: %v/%v", applied, err)
					}
				}
				r := mustRun(t, x.env, x.f.runID)
				if len(r.CodexCapHash) != 0 || r.CodexClaimEpoch != x.initial.CodexClaimEpoch+1 {
					t.Fatal("real park did not revoke")
				}
				want = ErrCodexCapabilityEpoch
			default:
				// Keep the genuine claim capability to isolate the status predicate.
				x.env.exec("UPDATE runs SET status=$2 WHERE id=$1", x.f.runID, state)
			}
			x.assertAuthority(t, w, x.p.Secrets.Codex.Capability, want)
		})
	}
}

// The negative readoption controls also start with a real returned capability;
// a rejected snapshot must never change phase/generation or refund a stale charge.
func TestCodexProvisionalReadoptFencesLiveDB(t *testing.T) {
	for _, path := range []string{"stale_worker", "claimed_never_started"} {
		for _, control := range []string{"wrong_worker", "wrong_generation", "released", "terminal", "terminal_pending"} {
			t.Run(path+"/"+control, func(t *testing.T) {
				x := newOutageCodexFlight(t, codexAuthModeSubscription)
				x.provisional(t, path)
				w := hbWorker(t, x.env, x.f.workerID)
				active := x.liveEntry("running")
				switch control {
				case "wrong_worker":
					id := seedSnapshotWorker(t, x.env, x.f.userID, "foreign-"+uuid.NewString())
					w = hbWorker(t, x.env, id)
				case "wrong_generation":
					active = []ActiveRunEntry{entry(x.f.runID, x.initial.ClaimGeneration+1, "running", false)}
				case "released":
					x.env.exec("UPDATE runs SET claim_released_at=now() WHERE id=$1", x.f.runID)
				case "terminal":
					x.env.exec("UPDATE runs SET status='completed' WHERE id=$1", x.f.runID)
				case "terminal_pending":
					active = []ActiveRunEntry{entry(x.f.runID, x.initial.ClaimGeneration, "running", true)}
				}
				before := mustRun(t, x.env, x.f.runID)
				holds := x.custody(t)
				if _, err := x.f.svc.Heartbeat(x.env.ctx, w, nil, nil,
					&ActiveSnapshot{SnapshotEpoch: 1, RegisterNonce: w.SnapshotRegisterNonce.String, Active: active}); err != nil {
					t.Fatal(err)
				}
				after := mustRun(t, x.env, x.f.runID)
				if !reflect.DeepEqual(before, after) || !reflect.DeepEqual(holds, x.custody(t)) {
					t.Fatalf("negative snapshot changed run/custody: status=%s generation=%d charge=%d", after.Status, after.ClaimGeneration, after.RequeueCount)
				}
				for _, scope := range x.scopes() {
					if _, err := x.f.svc.AuthorizeCodexCredentialOp(x.env.ctx, w, x.f.runID, x.p.Secrets.Codex.Capability, scope); err == nil {
						t.Errorf("negative snapshot authorized %s", scope)
					}
				}
			})
		}
	}
}

func TestCodexOutageReregisterOmissionLiveDB(t *testing.T) {
	x := newOutageCodexFlight(t, codexAuthModeSubscription)
	oldNonce := hbWorker(t, x.env, x.f.workerID).SnapshotRegisterNonce.String
	x.beat(t, 1, oldNonce, x.liveEntry("running"))
	w, nonce, err := x.f.svc.Register(x.env.ctx, x.f.wkr, "m1-restart", "base", nil, nil, x.f.wkr.ProtocolCapabilities,
		&ActiveSnapshot{SnapshotEpoch: 2, RegisterNonce: oldNonce, Active: []ActiveRunEntry{}})
	if err != nil || nonce == oldNonce || nonce == "" {
		t.Fatalf("real re-register: %v", err)
	}
	x.f.wkr = w
	x.assertAuthority(t, w, x.p.Secrets.Codex.Capability, ErrCodexCapabilityEpoch)
	before := mustRun(t, x.env, x.f.runID)
	registeredEpoch := hbWorker(t, x.env, x.f.workerID).SnapshotEpoch
	x.beat(t, 3, oldNonce, x.liveEntry("running"))
	if !reflect.DeepEqual(before, mustRun(t, x.env, x.f.runID)) || hbWorker(t, x.env, x.f.workerID).SnapshotEpoch != registeredEpoch {
		t.Fatal("old nonce replay changed run or snapshot epoch")
	}
	x.beat(t, registeredEpoch+1, nonce, []ActiveRunEntry{})
	if statusOf(t, x.env, x.f.runID) != "queued" || countActiveRuns(t, x.env, x.f.workerID) != 0 ||
		hbWorker(t, x.env, x.f.workerID).SnapshotEpoch != registeredEpoch+1 {
		t.Fatal("valid new-nonce omission revived absent flight")
	}
	x.assertAuthority(t, w, x.p.Secrets.Codex.Capability, ErrCodexCapabilityEpoch)
}

func TestCodexOutageFreshClaimFenceLiveDB(t *testing.T) {
	for _, path := range []string{"stale_worker", "claimed_never_started"} {
		for _, sameWorker := range []bool{true, false} {
			name := "different_worker"
			if sameWorker {
				name = "same_worker"
			}
			t.Run(path+"/"+name, func(t *testing.T) {
				x := newOutageCodexFlight(t, codexAuthModeSubscription)
				nonce := hbWorker(t, x.env, x.f.workerID).SnapshotRegisterNonce.String
				x.beat(t, 1, nonce, x.liveEntry("running"))
				x.provisional(t, path)
				// The still-listed flight cannot be freshly claimed. Clear dedupe via
				// a valid newer omission before testing ClaimRun's new-generation fence.
				if p, err := x.f.svc.Claim(x.env.ctx, x.f.wkr, nil); err != nil || p != nil {
					t.Fatalf("fresh snapshot dedupe: payload=%v err=%v", p != nil, err)
				}
				x.beat(t, 2, nonce, []ActiveRunEntry{})
				if countActiveRuns(t, x.env, x.f.workerID) != 0 {
					t.Fatal("omission did not clear snapshot dedupe")
				}
				w := hbWorker(t, x.env, x.f.workerID)
				if !sameWorker {
					id := seedSnapshotWorker(t, x.env, x.f.userID, "next-"+uuid.NewString())
					x.env.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", id, w.ProtocolCapabilities)
					w = hbWorker(t, x.env, id)
					x.env.exec("UPDATE runs SET status_since=now()-interval '3 hours',updated_at=now()-interval '3 hours' WHERE id=$1", x.f.runID)
					x.env.exec("UPDATE workers SET last_heartbeat_at=now()-interval '3 hours' WHERE id=$1", x.f.workerID)
				}
				before := mustRun(t, x.env, x.f.runID)
				claimed, err := x.env.q.ClaimRun(x.env.ctx, claimRunParamsFor(w))
				if err != nil || claimed.ID != x.f.runID {
					t.Fatalf("real next ClaimRun: %v", err)
				}
				if claimed.ClaimGeneration != before.ClaimGeneration+1 || claimed.WorkerID != pgconv.UUID(w.ID) ||
					claimed.StaleRequeueGeneration.Valid || claimed.ClaimReleasedAt.Valid {
					t.Fatal("fresh claim generation/ownership/provenance")
				}
				t.Run("null_hash_and_epoch_before_mint", func(t *testing.T) {
					if len(claimed.CodexCapHash) != 0 || claimed.CodexClaimEpoch != before.CodexClaimEpoch+1 {
						t.Fatalf("fresh ClaimRun before mint: null hash=%v epoch=%d want=%d", len(claimed.CodexCapHash) == 0, claimed.CodexClaimEpoch, before.CodexClaimEpoch+1)
					}
				})
				x.assertAuthority(t, w, x.p.Secrets.Codex.Capability, ErrCodexCapabilityEpoch)
				fresh, err := x.f.svc.assembleClaim(x.env.ctx, w, claimed)
				if err != nil || fresh == nil || fresh.Secrets.Codex == nil {
					t.Fatalf("real assembly mint: %v", err)
				}
				after := mustRun(t, x.env, x.f.runID)
				if after.CodexClaimEpoch != claimed.CodexClaimEpoch+1 || len(after.CodexCapHash) == 0 || fresh.Secrets.Codex.Capability == x.p.Secrets.Codex.Capability {
					t.Fatal("assembly did not mint a new capability")
				}
				t.Run("epoch_after_claim_and_mint", func(t *testing.T) {
					if after.CodexClaimEpoch != before.CodexClaimEpoch+2 {
						t.Fatalf("after fresh ClaimRun and mint epoch=%d want=%d", after.CodexClaimEpoch, before.CodexClaimEpoch+2)
					}
				})
				x.assertAuthority(t, w, fresh.Secrets.Codex.Capability, nil)
				x.assertAuthority(t, w, x.p.Secrets.Codex.Capability, ErrCodexCapabilityEpoch)
				holds := x.custody(t)
				if len(holds) != 2 || holds[0].ID == holds[1].ID || holds[0].Worker != x.f.workerID || holds[0].Generation != 1 ||
					holds[1].Worker != w.ID || holds[1].Generation != 2 || holds[0].State != "open" || holds[1].State != "open" {
					t.Fatalf("next generation custody=%+v", holds)
				}
			})
		}
	}
}
