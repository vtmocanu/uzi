package workersvc

import (
	"sort"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #1226 M2 (D4/D5) LiveDB coverage of the attempt/permit/completion-transaction boundary.
// This is a DATA-INTEGRITY boundary, so these run against a real throwaway Postgres (skipped
// unless UZI_TEST_DATABASE_URL). They reuse interlockLiveDB (setup + seed helpers from
// completion_interlock_livedb_test.go) and drive the real Service (real *store.Queries + the
// pool as the tx beginner), not fakes.

// permitService builds a Service over the live store with the pool wired as the completion
// transaction beginner, and background work dropped so no detached goroutine can outlive the
// test's pool.
func (e interlockLiveDB) permitService(t *testing.T) *Service {
	t.Helper()
	svc := New(e.q, newBox(t), testParams())
	svc.SetTxBeginner(e.pool)
	svc.SetBackground(func(func()) {})
	return svc
}

// seedFrozenRun inserts an INTERLOCKED, FROZEN, running issue run owned by workerID:
// completion_contract_version=1, contract_revision=1, completion_contract built from
// milestoneIDs (NULL when contractNull — the split-state fixture), milestones_frozen set to the
// [{id,title}] objects, milestones_completed to the bare id array completedIDs.
func (e interlockLiveDB) seedFrozenRun(t *testing.T, workerID uuid.UUID, milestoneIDs, completedIDs []string, contractNull bool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	iid := *e.nextIID
	*e.nextIID++
	frozen := frozenJSON(t, milestoneIDs...)
	completed := idsJSON(t, completedIDs...)
	var contract []byte
	if !contractNull {
		c, err := buildCompletionContract(frozen)
		if err != nil {
			t.Fatalf("buildCompletionContract: %v", err)
		}
		contract = c
	}
	e.exec(t, `INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, kind, status, worker_id,
	               completion_contract_version, contract_revision, completion_contract, milestones_frozen, milestones_completed)
	           VALUES ($1, $2, $3, $4, 't', 'd', 'issue', 'running', $5, 1, 1, $6, $7, $8)`,
		id, e.userID, e.repoID, iid, workerID, contract, frozen, completed)
	return id
}

// seedLegacyRunningRun inserts a LEGACY (completion_contract_version NULL) running issue run
// owned by workerID — the passthrough fixture.
func (e interlockLiveDB) seedLegacyRunningRun(t *testing.T, workerID uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	iid := *e.nextIID
	*e.nextIID++
	e.exec(t, `INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, kind, status, worker_id)
	           VALUES ($1, $2, $3, $4, 't', 'd', 'issue', 'running', $5)`,
		id, e.userID, e.repoID, iid, workerID)
	return id
}

func (e interlockLiveDB) runCompletionAttempts(t *testing.T, runID uuid.UUID) (count int, latestSet bool) {
	t.Helper()
	if err := e.pool.QueryRow(e.ctx,
		`SELECT completion_attempts, latest_completion_attempt IS NOT NULL FROM runs WHERE id = $1`, runID).
		Scan(&count, &latestSet); err != nil {
		t.Fatalf("read completion_attempts: %v", err)
	}
	return count, latestSet
}

func (e interlockLiveDB) attemptRowCount(t *testing.T, runID uuid.UUID) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM run_completion_attempts WHERE run_id = $1`, runID).Scan(&n); err != nil {
		t.Fatalf("count attempt rows: %v", err)
	}
	return n
}

// TestPermitIdempotentReissueLiveDB: a duplicate permit request for the same identity returns
// the SAME permit id (idempotent), not a second permit.
func TestPermitIdempotentReissueLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	wid := e.seedWorker(t, []string{"completion_interlock_v1"})
	runID := e.seedFrozenRun(t, wid, []string{"m1", "m2"}, []string{"m1", "m2"}, false)
	wkr := store.Worker{ID: wid}
	req := CompletionPermitRequest{ContractRevision: 1, Branch: "agent/issue-1", Head: "deadbeef"}

	first, err := svc.RequestCompletionPermit(e.ctx, wkr, runID, req)
	if err != nil || !first.Granted || first.Permit == nil {
		t.Fatalf("first request must grant: res=%+v err=%v", first, err)
	}
	second, err := svc.RequestCompletionPermit(e.ctx, wkr, runID, req)
	if err != nil || !second.Granted || second.Permit == nil {
		t.Fatalf("second request must grant: res=%+v err=%v", second, err)
	}
	if first.Permit.ID != second.Permit.ID {
		t.Fatalf("duplicate request issued a NEW permit (%s != %s); the upsert must be idempotent", first.Permit.ID, second.Permit.ID)
	}
	var n int
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM run_completion_permits WHERE run_id = $1`, runID).Scan(&n); err != nil {
		t.Fatalf("count permits: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected exactly ONE permit row, got %d", n)
	}
}

// TestPermitStaleClaimLiveDB: a wrong-worker request is ErrRunNotOwned; a not-live run is a
// stale_claim denial. Neither issues a permit.
func TestPermitStaleClaimLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	owner := e.seedWorker(t, []string{"completion_interlock_v1"})
	other := e.seedWorker(t, []string{"completion_interlock_v1"})
	runID := e.seedFrozenRun(t, owner, []string{"m1"}, []string{"m1"}, false)
	req := CompletionPermitRequest{ContractRevision: 1, Branch: "b", Head: "h"}

	// Wrong worker.
	if _, err := svc.RequestCompletionPermit(e.ctx, store.Worker{ID: other}, runID, req); err == nil {
		t.Fatal("a non-owning worker must not get a permit (want ErrRunNotOwned)")
	}

	// Owned but not live (paused).
	e.exec(t, `UPDATE runs SET status = 'paused' WHERE id = $1`, runID)
	res, err := svc.RequestCompletionPermit(e.ctx, store.Worker{ID: owner}, runID, req)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.Granted || res.DenyReason != CompletionDenyStaleClaim {
		t.Fatalf("a not-live run must be a stale_claim denial; got %+v", res)
	}
	var n int
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM run_completion_permits WHERE run_id = $1`, runID).Scan(&n); err != nil {
		t.Fatalf("count permits: %v", err)
	}
	if n != 0 {
		t.Fatalf("no permit should be issued for a stale claim; got %d", n)
	}
}

// TestPermitRevisionDriftLiveDB: a request whose contract_revision differs from the run's frozen
// revision is denied revision_drift.
func TestPermitRevisionDriftLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	wid := e.seedWorker(t, []string{"completion_interlock_v1"})
	runID := e.seedFrozenRun(t, wid, []string{"m1"}, []string{"m1"}, false)
	res, err := svc.RequestCompletionPermit(e.ctx, store.Worker{ID: wid}, runID,
		CompletionPermitRequest{ContractRevision: 2, Branch: "b", Head: "h"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.Granted || res.DenyReason != CompletionDenyRevisionDrift {
		t.Fatalf("want revision_drift; got %+v", res)
	}
}

// TestPermitMissingMilestonesLiveDB: an incomplete run is denied missing_milestones with the
// unmet list AND a run_completion_attempts row is written, completion_attempts incremented, and
// latest_completion_attempt set.
func TestPermitMissingMilestonesLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	wid := e.seedWorker(t, []string{"completion_interlock_v1"})
	runID := e.seedFrozenRun(t, wid, []string{"m1", "m2"}, []string{"m1"}, false)
	res, err := svc.RequestCompletionPermit(e.ctx, store.Worker{ID: wid}, runID,
		CompletionPermitRequest{ContractRevision: 1, Branch: "b", Head: "h1"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.Granted || res.DenyReason != CompletionDenyMissingMilestones {
		t.Fatalf("want missing_milestones; got %+v", res)
	}
	if len(res.Unmet) != 1 || res.Unmet[0] != "m2" {
		t.Fatalf("unmet = %v, want [m2]", res.Unmet)
	}
	if got := e.attemptRowCount(t, runID); got != 1 {
		t.Fatalf("run_completion_attempts rows = %d, want 1", got)
	}
	count, latestSet := e.runCompletionAttempts(t, runID)
	if count != 1 {
		t.Fatalf("completion_attempts = %d, want 1", count)
	}
	if !latestSet {
		t.Fatal("latest_completion_attempt must be set after an attempt")
	}
}

// TestPermitAttemptLogBoundedLiveDB proves the attempt log is pruned to <= 50 rows per run.
func TestPermitAttemptLogBoundedLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	wid := e.seedWorker(t, []string{"completion_interlock_v1"})
	runID := e.seedFrozenRun(t, wid, []string{"m1", "m2"}, []string{"m1"}, false)
	wkr := store.Worker{ID: wid}
	for i := 0; i < 60; i++ {
		if _, err := svc.RequestCompletionPermit(e.ctx, wkr, runID,
			CompletionPermitRequest{ContractRevision: 1, Branch: "b", Head: "h"}); err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	if got := e.attemptRowCount(t, runID); got > 50 {
		t.Fatalf("attempt log must be bounded to <= 50 rows; got %d", got)
	}
	count, _ := e.runCompletionAttempts(t, runID)
	if count != 60 {
		t.Fatalf("completion_attempts counter must be monotone (60), got %d", count)
	}
}

// TestCompletionWithPermitLiveDB: a granted permit lets the completed report terminate the run,
// and a retry after response loss is idempotent success (not a spurious denial).
func TestCompletionWithPermitLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	wid := e.seedWorker(t, []string{"completion_interlock_v1"})
	runID := e.seedFrozenRun(t, wid, []string{"m1"}, []string{"m1"}, false)
	wkr := store.Worker{ID: wid}
	const head = "cafef00d"

	permit, err := svc.RequestCompletionPermit(e.ctx, wkr, runID,
		CompletionPermitRequest{ContractRevision: 1, Branch: "agent/issue-1", Head: head})
	if err != nil || !permit.Granted {
		t.Fatalf("permit must be granted: %+v err=%v", permit, err)
	}

	run, applied, err := svc.SetState(e.ctx, wkr, runID, StateRequest{State: "completed", Head: strPtr(head), Branch: strPtr("agent/issue-1")})
	if err != nil {
		t.Fatalf("SetState completed: %v", err)
	}
	if !applied || run.Status != "completed" {
		t.Fatalf("a permitted completion must terminate the run; applied=%v status=%q", applied, run.Status)
	}
	// The permit is consumed exactly once.
	var consumed bool
	if err := e.pool.QueryRow(e.ctx, `SELECT consumed_at IS NOT NULL FROM run_completion_permits WHERE run_id = $1`, runID).Scan(&consumed); err != nil {
		t.Fatalf("read permit: %v", err)
	}
	if !consumed {
		t.Fatal("the permit must be consumed by the completion")
	}

	// Retry after response loss: a SECOND completed report for the same identity is idempotent
	// success, NOT a denial.
	run2, applied2, err := svc.SetState(e.ctx, wkr, runID, StateRequest{State: "completed", Head: strPtr(head), Branch: strPtr("agent/issue-1")})
	if err != nil {
		t.Fatalf("idempotent retry errored: %v", err)
	}
	if !applied2 || run2.Status != "completed" {
		t.Fatalf("a retry after success must be idempotent success; applied=%v status=%q", applied2, run2.Status)
	}
}

// TestCompletionWithoutPermitStaysNonTerminalLiveDB: a gated completion with NO matching
// unconsumed permit updates nothing and the run stays non-terminal. THIS is the assertion the
// mutation self-check must be able to redden.
func TestCompletionWithoutPermitStaysNonTerminalLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	wid := e.seedWorker(t, []string{"completion_interlock_v1"})
	runID := e.seedFrozenRun(t, wid, []string{"m1"}, []string{"m1"}, false)
	wkr := store.Worker{ID: wid}

	// No permit issued.
	run, applied, err := svc.SetState(e.ctx, wkr, runID, StateRequest{State: "completed", Head: strPtr("nope"), Branch: strPtr("b")})
	if err != nil {
		t.Fatalf("SetState completed: %v", err)
	}
	if applied {
		t.Fatal("a gated completion without a permit must NOT be applied")
	}
	if run.Status == "completed" {
		t.Fatalf("run must stay non-terminal without a permit; status = %q", run.Status)
	}
	if s := e.runStatus(t, runID); s != "running" {
		t.Fatalf("run must remain running; status = %q", s)
	}
}

// TestCompletionWrongHeadStaysNonTerminalLiveDB: a permit issued for head H does not let a
// completed report for a DIFFERENT head terminate the run.
func TestCompletionWrongHeadStaysNonTerminalLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	wid := e.seedWorker(t, []string{"completion_interlock_v1"})
	runID := e.seedFrozenRun(t, wid, []string{"m1"}, []string{"m1"}, false)
	wkr := store.Worker{ID: wid}

	if _, err := svc.RequestCompletionPermit(e.ctx, wkr, runID,
		CompletionPermitRequest{ContractRevision: 1, Branch: "b", Head: "H1"}); err != nil {
		t.Fatalf("permit: %v", err)
	}
	run, applied, err := svc.SetState(e.ctx, wkr, runID, StateRequest{State: "completed", Head: strPtr("H2"), Branch: strPtr("b")})
	if err != nil {
		t.Fatalf("SetState completed: %v", err)
	}
	if applied || run.Status == "completed" {
		t.Fatalf("a completion for the wrong head must stay non-terminal; applied=%v status=%q", applied, run.Status)
	}
}

// TestCompletionRevisionBumpUnderLockStaysNonTerminalLiveDB proves the permit identity fences on
// the LOCKED row's contract_revision, not the pre-tx snapshot (the #1227 revision-bump TOCTOU).
// A permit is issued for the frozen revision (1); then, simulating a #1227 revision bump that
// races BETWEEN the pre-tx `owned` snapshot and the FOR UPDATE lock, the row's contract_revision
// is bumped to 2 while `owned` still reflects revision 1. Completion must read the LOCKED row's
// revision (2), find no permit for it, and stay non-terminal (fail-closed) WITHOUT consuming the
// old-revision permit — forcing a re-permit at the new revision. (With the pre-fix code, which
// derived rev from the stale `owned` snapshot, this consumed the revision-1 permit and completed.)
func TestCompletionRevisionBumpUnderLockStaysNonTerminalLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	wid := e.seedWorker(t, []string{"completion_interlock_v1"})
	runID := e.seedFrozenRun(t, wid, []string{"m1"}, []string{"m1"}, false)
	wkr := store.Worker{ID: wid}
	const head = "beefcafe"

	if p, err := svc.RequestCompletionPermit(e.ctx, wkr, runID,
		CompletionPermitRequest{ContractRevision: 1, Branch: "b", Head: head}); err != nil || !p.Granted {
		t.Fatalf("permit must be granted: %+v err=%v", p, err)
	}

	// The pre-tx snapshot the completion carries reflects the OLD (frozen) revision 1.
	owned, err := svc.runOwnedByWorker(e.ctx, runID, wkr)
	if err != nil {
		t.Fatalf("load owned snapshot: %v", err)
	}
	if owned.ContractRevision.Int32 != 1 {
		t.Fatalf("snapshot revision must be 1, got %d", owned.ContractRevision.Int32)
	}

	// A #1227 revision bump lands AFTER the snapshot but before the FOR UPDATE lock.
	e.exec(t, `UPDATE runs SET contract_revision = 2 WHERE id = $1`, runID)

	rows, idempotent, err := svc.completeRunWithPermit(e.ctx, wkr, owned,
		StateRequest{Head: strPtr(head)},
		store.SetRunCompletedParams{ID: runID, WorkerID: owned.WorkerID})
	if err != nil {
		t.Fatalf("completeRunWithPermit: %v", err)
	}
	if rows != 0 || idempotent {
		t.Fatalf("a completion racing a revision bump must stay non-terminal; rows=%d idempotent=%v", rows, idempotent)
	}
	if s := e.runStatus(t, runID); s != "running" {
		t.Fatalf("run must remain running; status = %q", s)
	}
	// The OLD-revision permit must NOT have been consumed.
	var consumed bool
	if err := e.pool.QueryRow(e.ctx, `SELECT consumed_at IS NOT NULL FROM run_completion_permits WHERE run_id = $1`, runID).Scan(&consumed); err != nil {
		t.Fatalf("read permit: %v", err)
	}
	if consumed {
		t.Fatal("the old-revision permit must NOT be consumed under a racing revision bump")
	}
}

// TestLegacyCompletionPassthroughLiveDB: a legacy run (completion_contract_version NULL)
// completes through the unchanged path, needing no permit.
func TestLegacyCompletionPassthroughLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	wid := e.seedWorker(t, nil)
	runID := e.seedLegacyRunningRun(t, wid)
	wkr := store.Worker{ID: wid}

	run, applied, err := svc.SetState(e.ctx, wkr, runID, StateRequest{State: "completed", Branch: strPtr("agent/issue-9"), Head: strPtr("ignored")})
	if err != nil {
		t.Fatalf("SetState completed (legacy): %v", err)
	}
	if !applied || run.Status != "completed" {
		t.Fatalf("a legacy run must complete through the unchanged path; applied=%v status=%q", applied, run.Status)
	}
}

// TestSplitStateDeniedLiveDB: an interlocked run with milestones_frozen set but
// completion_contract NULL is denied contract_not_frozen (fail-closed), and cannot complete.
func TestSplitStateDeniedLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	wid := e.seedWorker(t, []string{"completion_interlock_v1"})
	runID := e.seedFrozenRun(t, wid, []string{"m1"}, nil, true) // contractNull=true
	wkr := store.Worker{ID: wid}

	res, err := svc.RequestCompletionPermit(e.ctx, wkr, runID,
		CompletionPermitRequest{ContractRevision: 1, Branch: "b", Head: "h"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.Granted || res.DenyReason != CompletionDenyContractNotFrozen {
		t.Fatalf("split-state must be denied contract_not_frozen; got %+v", res)
	}
	// And a direct completed report cannot terminate it either (no permit exists).
	run, applied, err := svc.SetState(e.ctx, wkr, runID, StateRequest{State: "completed", Head: strPtr("h"), Branch: strPtr("b")})
	if err != nil {
		t.Fatalf("SetState completed: %v", err)
	}
	if applied || run.Status == "completed" {
		t.Fatalf("a split-state run must not complete; applied=%v status=%q", applied, run.Status)
	}
}

// milestonesCompletedIDs reads and decodes runs.milestones_completed (the bare id array).
func (e interlockLiveDB) milestonesCompletedIDs(t *testing.T, runID uuid.UUID) []string {
	t.Helper()
	var raw []byte
	if err := e.pool.QueryRow(e.ctx, `SELECT milestones_completed FROM runs WHERE id = $1`, runID).Scan(&raw); err != nil {
		t.Fatalf("read milestones_completed: %v", err)
	}
	ids, err := DecodeMilestoneIDs(raw)
	if err != nil {
		t.Fatalf("decode milestones_completed: %v", err)
	}
	sort.Strings(ids)
	return ids
}

// TestAttemptUnionMergesDeclarationLiveDB (PRD #1226 M3): the attempt endpoint union-merges the
// lead's declared milestones into runs.milestones_completed and recomputes unmet over the merged
// set in ONE call — removing the M2-review "persist THEN attempt" ordering hazard. Declaring m2 via
// the attempt endpoint shrinks unmet from [m2 m3] to [m3] AND persists m2 into milestones_completed;
// a non-member declared id is dropped by the subset-validation and never persisted or counted.
func TestAttemptUnionMergesDeclarationLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	wid := e.seedWorker(t, []string{"completion_interlock_v1"})
	// Frozen m1,m2,m3; only m1 declared complete at seed.
	runID := e.seedFrozenRun(t, wid, []string{"m1", "m2", "m3"}, []string{"m1"}, false)
	wkr := store.Worker{ID: wid}

	// Attempt with NO declaration: unmet is [m2 m3] over the existing set.
	res1, err := svc.RecordCompletionAttempt(e.ctx, wkr, runID, CompletionAttemptRequest{Head: "h1", WorktreeFingerprint: "wf1"})
	if err != nil {
		t.Fatalf("attempt1: %v", err)
	}
	if len(res1.Unmet) != 2 || res1.Unmet[0] != "m2" || res1.Unmet[1] != "m3" {
		t.Fatalf("unmet1 = %v, want [m2 m3]", res1.Unmet)
	}
	if got := e.milestonesCompletedIDs(t, runID); len(got) != 1 || got[0] != "m1" {
		t.Fatalf("milestones_completed after no-declaration attempt = %v, want [m1]", got)
	}

	// Declare m2 THROUGH the attempt endpoint: it is union-merged into milestones_completed and
	// the recomputed unmet shrinks to [m3] in the same call.
	res2, err := svc.RecordCompletionAttempt(e.ctx, wkr, runID,
		CompletionAttemptRequest{MilestonesCompleted: []string{"m2"}, Head: "h2", WorktreeFingerprint: "wf2"})
	if err != nil {
		t.Fatalf("attempt2: %v", err)
	}
	if len(res2.Unmet) != 1 || res2.Unmet[0] != "m3" {
		t.Fatalf("unmet2 = %v, want [m3] (declaration union-merged)", res2.Unmet)
	}
	if res2.AttemptCount != 2 {
		t.Fatalf("attempt_count = %d, want 2", res2.AttemptCount)
	}
	if got := e.milestonesCompletedIDs(t, runID); len(got) != 2 || got[0] != "m1" || got[1] != "m2" {
		t.Fatalf("milestones_completed after declaring m2 = %v, want [m1 m2] merged (the ordering-hazard fix)", got)
	}

	// A non-member declared id is DROPPED by the subset-validation (progressParams): it neither
	// shrinks unmet nor gets persisted.
	res3, err := svc.RecordCompletionAttempt(e.ctx, wkr, runID,
		CompletionAttemptRequest{MilestonesCompleted: []string{"bogus"}, Head: "h3"})
	if err != nil {
		t.Fatalf("attempt3: %v", err)
	}
	if len(res3.Unmet) != 1 || res3.Unmet[0] != "m3" {
		t.Fatalf("unmet3 = %v, want [m3] (bogus id dropped)", res3.Unmet)
	}
	if got := e.milestonesCompletedIDs(t, runID); len(got) != 2 || got[0] != "m1" || got[1] != "m2" {
		t.Fatalf("milestones_completed after declaring a non-member = %v, want unchanged [m1 m2]", got)
	}
}
