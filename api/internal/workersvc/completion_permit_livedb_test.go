package workersvc

import (
	"errors"
	"sort"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
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

// attemptHeadFingerprint reads the single stored run_completion_attempts row's head and
// worktree_fingerprint TEXT columns.
func (e interlockLiveDB) attemptHeadFingerprint(t *testing.T, runID uuid.UUID) (head, fingerprint string) {
	t.Helper()
	if err := e.pool.QueryRow(e.ctx,
		`SELECT head, worktree_fingerprint FROM run_completion_attempts WHERE run_id = $1`, runID).
		Scan(&head, &fingerprint); err != nil {
		t.Fatalf("read attempt head/worktree_fingerprint: %v", err)
	}
	return head, fingerprint
}

// latestAttemptHeadFingerprint reads runs.latest_completion_attempt's head and
// worktree_fingerprint out of the jsonb summary — the SECOND 22021 surface a NUL would hit.
func (e interlockLiveDB) latestAttemptHeadFingerprint(t *testing.T, runID uuid.UUID) (head, fingerprint string) {
	t.Helper()
	if err := e.pool.QueryRow(e.ctx,
		`SELECT latest_completion_attempt->>'head', latest_completion_attempt->>'worktree_fingerprint' FROM runs WHERE id = $1`, runID).
		Scan(&head, &fingerprint); err != nil {
		t.Fatalf("read latest_completion_attempt summary: %v", err)
	}
	return head, fingerprint
}

// TestAttemptNULStrippedLiveDB (PRD #1226 M3 hardening): a worker-authored head or
// worktree_fingerprint carrying a NUL byte is stored NUL-STRIPPED — the attempt is recorded, not
// 500'd. A NUL in a text/jsonb column raises Postgres 22021, and here it would fire on BOTH the
// run_completion_attempts text columns AND the latest_completion_attempt jsonb summary, aborting
// the attempt and leaving an otherwise-complete run's completion unrecorded. This is the same
// worker-field discipline every other worker-authored text field already carries (stripNULParam /
// sanitizeFailureReason). It is mutation-sensitive: revert the strip in persistCompletionAttempt
// and RecordCompletionAttempt returns a 22021 error here (the first assertion reddens).
func TestAttemptNULStrippedLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	wid := e.seedWorker(t, []string{"completion_interlock_v1"})
	runID := e.seedFrozenRun(t, wid, []string{"m1"}, nil, false)
	wkr := store.Worker{ID: wid}

	res, err := svc.RecordCompletionAttempt(e.ctx, wkr, runID,
		CompletionAttemptRequest{Head: "de\x00adbeef", WorktreeFingerprint: "wf\x001"})
	if err != nil {
		t.Fatalf("a NUL in head/worktree_fingerprint must be stripped, not error (22021 -> 500): %v", err)
	}
	if res.AttemptCount != 1 {
		t.Fatalf("attempt_count = %d, want 1 (the attempt must still be recorded)", res.AttemptCount)
	}

	// The attempt row's text columns store the NUL-stripped values.
	head, wf := e.attemptHeadFingerprint(t, runID)
	if head != "deadbeef" {
		t.Fatalf("stored head = %q, want %q (NUL stripped)", head, "deadbeef")
	}
	if wf != "wf1" {
		t.Fatalf("stored worktree_fingerprint = %q, want %q (NUL stripped)", wf, "wf1")
	}

	// The jsonb summary carries the same stripped values (the second 22021 surface).
	jHead, jWF := e.latestAttemptHeadFingerprint(t, runID)
	if jHead != "deadbeef" || jWF != "wf1" {
		t.Fatalf("latest_completion_attempt summary = (%q, %q), want (deadbeef, wf1)", jHead, jWF)
	}
}

// permitBranchHead reads the single stored run_completion_permits row's branch and head TEXT
// columns (both `text NOT NULL`, migration 00212) — the two surfaces a NUL would 22021 on issue.
func (e interlockLiveDB) permitBranchHead(t *testing.T, runID uuid.UUID) (branch, head string) {
	t.Helper()
	if err := e.pool.QueryRow(e.ctx,
		`SELECT branch, head FROM run_completion_permits WHERE run_id = $1`, runID).
		Scan(&branch, &head); err != nil {
		t.Fatalf("read permit branch/head: %v", err)
	}
	return branch, head
}

// TestPermitNULStrippedEndToEndLiveDB (PRD #1226 hardening): a permit REQUESTED with a NUL in head
// and/or branch is ISSUED NUL-stripped (no 22021 -> 500), and a completion REPORTED with the SAME
// NUL-containing head strips to the same value and still matches and consumes that permit — end to
// end, issue -> consume. This closes on the PERMIT path the symmetric NUL gap the attempt path
// already fixed, where a 500 is WORSE: an unissued permit (both branch/head are `text NOT NULL`,
// migration 00212) blocks the interlocked run's terminal completion transaction outright, not just
// an attempt log, because that transaction can only consume a permit that was issued. It is the
// same worker-field discipline every other worker-authored text field carries (stripNULParam /
// persistCompletionAttempt). Mutation-sensitive: revert the strip in RequestCompletionPermit and
// the issue 22021s (the permit-granted assertion reddens); revert it in completeRunWithPermit and
// the consume 22021s (the SetState assertion reddens).
func TestPermitNULStrippedEndToEndLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	wid := e.seedWorker(t, []string{"completion_interlock_v1"})
	runID := e.seedFrozenRun(t, wid, []string{"m1"}, []string{"m1"}, false)
	wkr := store.Worker{ID: wid}
	// A hostile NUL spliced into an otherwise-real head/branch; both strip to the SAME value on the
	// issue and consume sides, so a legitimate completion still finds its permit.
	const (
		nulHead     = "ca\x00fef00d"
		nulBranch   = "agent/\x00issue-1"
		cleanHead   = "cafef00d"
		cleanBranch = "agent/issue-1"
	)

	// ISSUE: a NUL in head/branch must not 22021 -> 500; the permit is issued NUL-stripped.
	res, err := svc.RequestCompletionPermit(e.ctx, wkr, runID,
		CompletionPermitRequest{ContractRevision: 1, Branch: nulBranch, Head: nulHead})
	if err != nil {
		t.Fatalf("a NUL in permit head/branch must be stripped, not error (22021 -> 500): %v", err)
	}
	if !res.Granted || res.Permit == nil {
		t.Fatalf("the permit must be granted (NUL stripped): %+v", res)
	}
	if res.Permit.Head != cleanHead || res.Permit.Branch != cleanBranch {
		t.Fatalf("issued permit DTO = (head %q, branch %q), want (%q, %q) NUL stripped",
			res.Permit.Head, res.Permit.Branch, cleanHead, cleanBranch)
	}
	// The stored `text NOT NULL` columns carry the stripped values.
	if b, h := e.permitBranchHead(t, runID); b != cleanBranch || h != cleanHead {
		t.Fatalf("stored permit = (branch %q, head %q), want (%q, %q) NUL stripped", b, h, cleanBranch, cleanHead)
	}

	// CONSUME: a completed report carrying the SAME NUL head strips to the same value, matches the
	// issued permit, terminates the interlocked run, and consumes the permit exactly once.
	run, applied, err := svc.SetState(e.ctx, wkr, runID,
		StateRequest{State: "completed", Head: strPtr(nulHead), Branch: strPtr(nulBranch)})
	if err != nil {
		t.Fatalf("a NUL head on the consume side must be stripped, not error (22021 -> 500): %v", err)
	}
	if !applied || run.Status != "completed" {
		t.Fatalf("a NUL head that strips to the issued permit's head must complete the run; applied=%v status=%q", applied, run.Status)
	}
	var consumed bool
	if err := e.pool.QueryRow(e.ctx, `SELECT consumed_at IS NOT NULL FROM run_completion_permits WHERE run_id = $1`, runID).Scan(&consumed); err != nil {
		t.Fatalf("read permit: %v", err)
	}
	if !consumed {
		t.Fatal("the permit must be consumed by the NUL-head completion")
	}
}

// openQuestionIDNull reports whether runs.open_question_id is NULL for the run.
func (e interlockLiveDB) openQuestionIDNull(t *testing.T, runID uuid.UUID) bool {
	t.Helper()
	var isNull bool
	if err := e.pool.QueryRow(e.ctx,
		`SELECT open_question_id IS NULL FROM runs WHERE id = $1`, runID).Scan(&isNull); err != nil {
		t.Fatalf("read open_question_id: %v", err)
	}
	return isNull
}

// TestSetRunCompletionHoldLiveDB (PRD #1226 M4, D6) proves the dedicated completion-HOLD
// transition. An OWNED, INTERLOCKED run with at least one recorded completion attempt holds from
// BOTH running and awaiting_input (-> paused, hold_reason='completion_blocked', captured head
// recorded, open_question_id cleared). A run with completion_attempts==0, in a non-
// running/awaiting_input status, or LEGACY (completion_contract_version NULL) is REFUSED (0 rows ->
// applied=false, status unchanged). And SetRunPaused — the SIBLING this must not weaken — still
// pauses an owner-pause run, unchanged.
func TestSetRunCompletionHoldLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	wid := e.seedWorker(t, []string{"completion_interlock_v1"})
	wkr := store.Worker{ID: wid}

	// Case 1: owned interlocked RUNNING run with completion_attempts>0 holds; the captured head
	// is recorded and a resolved open_question_id is cleared.
	running := e.seedFrozenRun(t, wid, []string{"m1"}, []string{"m1"}, false)
	e.exec(t, `UPDATE runs SET completion_attempts = 1, open_question_id = 'q-should-clear' WHERE id = $1`, running)
	run, applied, err := svc.SetRunCompletionHold(e.ctx, wkr, running, "capturedhead1")
	if err != nil {
		t.Fatalf("SetRunCompletionHold (running): %v", err)
	}
	if !applied {
		t.Fatal("an owned interlocked running run with an attempt must hold (applied)")
	}
	if run.Status != "paused" {
		t.Fatalf("held run status = %q, want paused", run.Status)
	}
	if !run.HoldReason.Valid || run.HoldReason.String != "completion_blocked" {
		t.Fatalf("hold_reason = %+v, want 'completion_blocked'", run.HoldReason)
	}
	if !run.HoldCapturedHead.Valid || run.HoldCapturedHead.String != "capturedhead1" {
		t.Fatalf("hold_captured_head = %+v, want 'capturedhead1'", run.HoldCapturedHead)
	}
	if !e.openQuestionIDNull(t, running) {
		t.Fatal("open_question_id must be cleared by the hold (NO SETTER MAY LEAVE A RESOLVED open_question_id BEHIND)")
	}

	// Case 2: owned interlocked AWAITING_INPUT run with an attempt also holds.
	awaiting := e.seedFrozenRun(t, wid, []string{"m1"}, []string{"m1"}, false)
	e.exec(t, `UPDATE runs SET status = 'awaiting_input', completion_attempts = 2 WHERE id = $1`, awaiting)
	run2, applied2, err := svc.SetRunCompletionHold(e.ctx, wkr, awaiting, "")
	if err != nil {
		t.Fatalf("SetRunCompletionHold (awaiting_input): %v", err)
	}
	if !applied2 || run2.Status != "paused" {
		t.Fatalf("an awaiting_input interlocked run with an attempt must hold; applied=%v status=%q", applied2, run2.Status)
	}
	// An empty captured head is allowed and stored NULL.
	if run2.HoldCapturedHead.Valid {
		t.Fatalf("an empty captured head must store NULL; got %+v", run2.HoldCapturedHead)
	}

	// Case 3: completion_attempts==0 is REFUSED (the guard fails); status is unchanged. THIS is
	// the assertion the completion_attempts>0 mutation check reddens.
	noAttempts := e.seedFrozenRun(t, wid, []string{"m1"}, []string{"m1"}, false) // attempts defaults to 0
	run3, applied3, err := svc.SetRunCompletionHold(e.ctx, wkr, noAttempts, "h")
	if err != nil {
		t.Fatalf("SetRunCompletionHold (attempts=0): %v", err)
	}
	if applied3 {
		t.Fatal("a run with completion_attempts=0 must NOT hold")
	}
	if run3.Status != "running" {
		t.Fatalf("a refused hold must leave status unchanged; got %q, want running", run3.Status)
	}
	if s := e.runStatus(t, noAttempts); s != "running" {
		t.Fatalf("db status after refused hold = %q, want running", s)
	}

	// Case 4: a non-running/awaiting_input status (queued) is REFUSED even with an attempt.
	queued := e.seedFrozenRun(t, wid, []string{"m1"}, []string{"m1"}, false)
	e.exec(t, `UPDATE runs SET status = 'queued', completion_attempts = 3 WHERE id = $1`, queued)
	run4, applied4, err := svc.SetRunCompletionHold(e.ctx, wkr, queued, "h")
	if err != nil {
		t.Fatalf("SetRunCompletionHold (queued): %v", err)
	}
	if applied4 {
		t.Fatal("a queued run must NOT hold (source guard is running/awaiting_input only)")
	}
	if run4.Status != "queued" {
		t.Fatalf("a refused hold must leave status unchanged; got %q, want queued", run4.Status)
	}

	// Case 5: SetRunPaused — the sibling this must not weaken — still pauses an owner-pause run.
	ownerPause := e.seedFrozenRun(t, wid, []string{"m1"}, []string{"m1"}, false)
	e.exec(t, `UPDATE runs SET pause_requested_at = now() WHERE id = $1`, ownerPause)
	rows, err := e.q.SetRunPaused(e.ctx, store.SetRunPausedParams{ID: ownerPause, WorkerID: pgconv.UUID(wid)})
	if err != nil {
		t.Fatalf("SetRunPaused: %v", err)
	}
	if rows != 1 {
		t.Fatalf("SetRunPaused must still pause an owner-pause running run; rows=%d", rows)
	}
	if s := e.runStatus(t, ownerPause); s != "paused" {
		t.Fatalf("owner-pause run status = %q, want paused", s)
	}

	// Case 6: a LEGACY (non-interlocked) running run with completion_attempts>0 is REFUSED — the
	// completion_contract_version IS NOT NULL guard clause keeps a run that never interlocks out of
	// the hold, even though it satisfies the status and attempt predicates. Seed a frozen run, then
	// NULL its contract version so only that clause distinguishes it from Case 1. Mutation-check:
	// drop `completion_contract_version IS NOT NULL` from SetRunCompletionHold and this reddens.
	legacy := e.seedFrozenRun(t, wid, []string{"m1"}, []string{"m1"}, false)
	e.exec(t, `UPDATE runs SET completion_contract_version = NULL, completion_attempts = 4 WHERE id = $1`, legacy)
	run6, applied6, err := svc.SetRunCompletionHold(e.ctx, wkr, legacy, "h")
	if err != nil {
		t.Fatalf("SetRunCompletionHold (legacy): %v", err)
	}
	if applied6 {
		t.Fatal("a LEGACY (completion_contract_version NULL) run must NOT hold, even with an attempt " +
			"(completion_contract_version IS NOT NULL guard)")
	}
	if run6.Status != "running" {
		t.Fatalf("a refused legacy hold must leave status unchanged; got %q, want running", run6.Status)
	}
	if s := e.runStatus(t, legacy); s != "running" {
		t.Fatalf("db status after refused legacy hold = %q, want running", s)
	}

	// Case 7: a FOREIGN worker cannot hold a run it does not own — this pins the worker_id
	// authorization predicate (SetRunCompletionHold binds WHERE worker_id = @worker_id, so a
	// non-owning worker matches 0 rows and the service re-read fails ErrRunNotOwned). Seed a second
	// worker and a fresh owned interlocked RUNNING run with an attempt (mirror Case 1), then have
	// the foreign worker attempt the hold: it is refused (ErrRunNotOwned), applied=false, the run
	// stays running with its hold columns untouched, and no decision/follow_up/answer row is written.
	otherWid := e.seedWorker(t, []string{"completion_interlock_v1"})
	other := store.Worker{ID: otherWid}
	foreign := e.seedFrozenRun(t, wid, []string{"m1"}, []string{"m1"}, false)
	e.exec(t, `UPDATE runs SET completion_attempts = 1 WHERE id = $1`, foreign)
	run7, applied7, err := svc.SetRunCompletionHold(e.ctx, other, foreign, "foreignhead")
	if !errors.Is(err, ErrRunNotOwned) {
		t.Fatalf("a foreign worker must be refused ErrRunNotOwned; got run=%+v applied=%v err=%v", run7, applied7, err)
	}
	if applied7 {
		t.Fatal("a foreign worker's hold must NOT be applied")
	}
	if s := e.runStatus(t, foreign); s != "running" {
		t.Fatalf("a foreign hold attempt must leave status unchanged; got %q, want running", s)
	}
	// The hold columns must be untouched (never entered the hold).
	reason, head, _ := e.holdColumns(t, foreign)
	if reason != nil {
		t.Fatalf("hold_reason must stay NULL after a foreign hold attempt; got %q", *reason)
	}
	if head != nil {
		t.Fatalf("hold_captured_head must stay NULL after a foreign hold attempt; got %q", *head)
	}
	// And no run_user_inputs rows were written by the refused attempt.
	for _, kind := range []string{"completion_decision", "follow_up", "answer"} {
		if got := e.countInputs(t, foreign, kind); got != 0 {
			t.Fatalf("a foreign hold attempt must write no %s row; got %d", kind, got)
		}
	}
}

// permitIssuedByWorker reads the single stored run_completion_permits row's issued_by_worker_id.
func (e interlockLiveDB) permitIssuedByWorker(t *testing.T, runID uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := e.pool.QueryRow(e.ctx,
		`SELECT issued_by_worker_id FROM run_completion_permits WHERE run_id = $1`, runID).Scan(&id); err != nil {
		t.Fatalf("read permit issued_by_worker_id: %v", err)
	}
	return id
}

// TestPermitRequeueRebindsWorkerLiveDB (PRD #1226 M4.0, FIX 1): on an A->B requeue the permit
// upsert's ON CONFLICT must REBIND issued_by_worker_id to the re-requesting worker. Worker A
// issues a permit for (run, rev, H); the run requeues to worker B (still running, now owned by
// B); B re-requests the SAME (rev, branch, H) and the row's issued_by_worker_id becomes B, so B's
// completeRunWithPermit (which fences on B's id) can find and consume it and the run completes.
// Mutation-sensitive: revert the upsert SET to `branch = EXCLUDED.branch` only and the row keeps
// A's id, so B's completion can never match its permit -> the "B completes" assertion reddens.
func TestPermitRequeueRebindsWorkerLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	workerA := e.seedWorker(t, []string{"completion_interlock_v1"})
	workerB := e.seedWorker(t, []string{"completion_interlock_v1"})
	runID := e.seedFrozenRun(t, workerA, []string{"m1"}, []string{"m1"}, false)
	const (
		head   = "d00df00d"
		branch = "agent/issue-1"
	)

	// Worker A obtains the permit; the row is issued by A.
	if p, err := svc.RequestCompletionPermit(e.ctx, store.Worker{ID: workerA}, runID,
		CompletionPermitRequest{ContractRevision: 1, Branch: branch, Head: head}); err != nil || !p.Granted {
		t.Fatalf("worker A permit must be granted: %+v err=%v", p, err)
	}
	if got := e.permitIssuedByWorker(t, runID); got != workerA {
		t.Fatalf("permit must initially be issued by A (%s), got %s", workerA, got)
	}

	// Requeue: the run moves to worker B, still in a live claimed (running) state.
	e.exec(t, `UPDATE runs SET worker_id = $1 WHERE id = $2`, workerB, runID)

	// Worker B re-requests the SAME identity: the ON CONFLICT must rebind the permit to B.
	second, err := svc.RequestCompletionPermit(e.ctx, store.Worker{ID: workerB}, runID,
		CompletionPermitRequest{ContractRevision: 1, Branch: branch, Head: head})
	if err != nil || !second.Granted {
		t.Fatalf("worker B re-request must be granted: %+v err=%v", second, err)
	}
	if got := e.permitIssuedByWorker(t, runID); got != workerB {
		t.Fatalf("after B's re-request the permit must be REBOUND to B (%s); got %s (stale worker A binding)", workerB, got)
	}

	// B's completion must terminate the run — impossible if the permit still carries A's id.
	run, applied, err := svc.SetState(e.ctx, store.Worker{ID: workerB}, runID,
		StateRequest{State: "completed", Head: strPtr(head), Branch: strPtr(branch)})
	if err != nil {
		t.Fatalf("SetState completed (worker B): %v", err)
	}
	if !applied || run.Status != "completed" {
		t.Fatalf("worker B must complete the requeued run via its rebound permit; applied=%v status=%q", applied, run.Status)
	}
}

// TestPermitHeadNormalizationLiveDB (PRD #1226 M4.0, FIX 2): the issue side must normalize head
// with the SAME NUL-strip-then-TrimSpace the consume side uses. A head with surrounding
// whitespace is stored TRIMMED (so a subsequent trimmed consume-side lookup matches and the run
// completes), and an all-whitespace head that normalizes to "" is a NON-TERMINAL empty_head
// denial that writes no permit. Mutation-sensitive: drop the TrimSpace on the issue side and the
// stored head stays untrimmed (the whitespace completion no longer matches -> that assertion
// reddens) AND the all-whitespace head is no longer empty-after-normalize (Granted becomes true ->
// the empty_head assertion reddens); independently, remove the empty-head guard and the
// all-whitespace request issues a head="" permit (Granted becomes true -> the empty_head assertion
// reddens).
func TestPermitHeadNormalizationLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	wid := e.seedWorker(t, []string{"completion_interlock_v1"})
	wkr := store.Worker{ID: wid}

	// Part A: a head with surrounding whitespace is stored trimmed and completes end to end.
	runID := e.seedFrozenRun(t, wid, []string{"m1"}, []string{"m1"}, false)
	const (
		paddedHead = "  cafebabe  "
		cleanHead  = "cafebabe"
	)
	res, err := svc.RequestCompletionPermit(e.ctx, wkr, runID,
		CompletionPermitRequest{ContractRevision: 1, Branch: "b", Head: paddedHead})
	if err != nil {
		t.Fatalf("padded-head permit request: %v", err)
	}
	if !res.Granted || res.Permit == nil {
		t.Fatalf("a padded head must be granted (normalized): %+v", res)
	}
	if res.Permit.Head != cleanHead {
		t.Fatalf("issued permit head = %q, want %q (TrimSpace at issue)", res.Permit.Head, cleanHead)
	}
	if _, h := e.permitBranchHead(t, runID); h != cleanHead {
		t.Fatalf("stored permit head = %q, want %q (TrimSpace at issue)", h, cleanHead)
	}
	// The consume side trims identically; a completed report carrying the padded head matches the
	// trimmed stored head and terminates the run.
	run, applied, err := svc.SetState(e.ctx, wkr, runID,
		StateRequest{State: "completed", Head: strPtr(paddedHead), Branch: strPtr("b")})
	if err != nil {
		t.Fatalf("SetState completed (padded head): %v", err)
	}
	if !applied || run.Status != "completed" {
		t.Fatalf("a padded head must complete via the symmetric trim; applied=%v status=%q", applied, run.Status)
	}

	// Part B: an all-whitespace head normalizes to "" -> a non-terminal empty_head denial, no permit.
	emptyRun := e.seedFrozenRun(t, wid, []string{"m1"}, []string{"m1"}, false)
	deny, err := svc.RequestCompletionPermit(e.ctx, wkr, emptyRun,
		CompletionPermitRequest{ContractRevision: 1, Branch: "b", Head: "   "})
	if err != nil {
		t.Fatalf("all-whitespace-head request: %v", err)
	}
	if deny.Granted || deny.DenyReason != CompletionDenyEmptyHead {
		t.Fatalf("an all-whitespace head must be a non-terminal empty_head denial; got %+v", deny)
	}
	var n int
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM run_completion_permits WHERE run_id = $1`, emptyRun).Scan(&n); err != nil {
		t.Fatalf("count permits: %v", err)
	}
	if n != 0 {
		t.Fatalf("an empty_head denial must write no permit; got %d rows", n)
	}
}

// TestPermitEmptyBranchDeniedLiveDB (PRD #1226 M4.0): a branch that normalizes (NUL-strip +
// TrimSpace) to the empty string is a NON-TERMINAL empty_branch denial that writes no permit — the
// symmetric guard to empty_head, closing the reachable NUL path. A NUL-only branch survives the
// handler's post-TrimSpace `== ""` reject (NUL is not unicode.IsSpace), so this calls
// RequestCompletionPermit DIRECTLY with Branch="\x00" and a VALID non-empty head, mirroring
// TestPermitHeadNormalizationLiveDB Part B. Mutation-check: drop the empty-branch guard in
// RequestCompletionPermit and the NUL branch strips to "" and a permit is ISSUED (Granted becomes
// true) -> the empty_branch assertion reddens.
func TestPermitEmptyBranchDeniedLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	wid := e.seedWorker(t, []string{"completion_interlock_v1"})
	wkr := store.Worker{ID: wid}
	runID := e.seedFrozenRun(t, wid, []string{"m1"}, []string{"m1"}, false)

	deny, err := svc.RequestCompletionPermit(e.ctx, wkr, runID,
		CompletionPermitRequest{ContractRevision: 1, Branch: "\x00", Head: "cafebabe"})
	if err != nil {
		t.Fatalf("a NUL-only branch must be a non-terminal denial, not an error: %v", err)
	}
	if deny.Granted || deny.DenyReason != CompletionDenyEmptyBranch {
		t.Fatalf("a branch that normalizes to empty must be a non-terminal empty_branch denial; got %+v", deny)
	}
	var n int
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM run_completion_permits WHERE run_id = $1`, runID).Scan(&n); err != nil {
		t.Fatalf("count permits: %v", err)
	}
	if n != 0 {
		t.Fatalf("an empty_branch denial must write no permit; got %d rows", n)
	}
}

// TestPermitBoundToBranchLiveDB (PRD #1226 M4.0, FIX 3): the permit is bound to the
// worker-reported source branch, so a permit issued for branch A at head H cannot be consumed by
// a completion reporting branch B at the same head H. A completion reporting branch B is
// non-terminal (no permit match); reporting branch A completes. Mutation-sensitive: remove
// `AND branch = @branch` from the Get queries (and its call-site field) and the branch-B
// completion succeeds -> the "branch B stays non-terminal" assertion reddens.
func TestPermitBoundToBranchLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	wid := e.seedWorker(t, []string{"completion_interlock_v1"})
	runID := e.seedFrozenRun(t, wid, []string{"m1"}, []string{"m1"}, false)
	wkr := store.Worker{ID: wid}
	const head = "feedface"

	// Permit issued for branch "A".
	if p, err := svc.RequestCompletionPermit(e.ctx, wkr, runID,
		CompletionPermitRequest{ContractRevision: 1, Branch: "A", Head: head}); err != nil || !p.Granted {
		t.Fatalf("permit for branch A must be granted: %+v err=%v", p, err)
	}

	// A completion reporting branch "B" at the same head must NOT match the branch-A permit.
	run, applied, err := svc.SetState(e.ctx, wkr, runID,
		StateRequest{State: "completed", Head: strPtr(head), Branch: strPtr("B")})
	if err != nil {
		t.Fatalf("SetState completed (branch B): %v", err)
	}
	if applied || run.Status == "completed" {
		t.Fatalf("a completion reporting branch B must stay non-terminal against a branch-A permit; applied=%v status=%q", applied, run.Status)
	}
	if s := e.runStatus(t, runID); s != "running" {
		t.Fatalf("run must remain running after the branch-mismatch completion; status = %q", s)
	}

	// A completion reporting the matching branch "A" completes the run.
	run2, applied2, err := svc.SetState(e.ctx, wkr, runID,
		StateRequest{State: "completed", Head: strPtr(head), Branch: strPtr("A")})
	if err != nil {
		t.Fatalf("SetState completed (branch A): %v", err)
	}
	if !applied2 || run2.Status != "completed" {
		t.Fatalf("a completion reporting the matching branch A must complete the run; applied=%v status=%q", applied2, run2.Status)
	}
}
