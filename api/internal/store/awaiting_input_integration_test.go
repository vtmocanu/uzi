package store_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// PRD #88 M1 against a REAL Postgres. This file is a gate, not a nicety: sqlc's type
// deduction is not Postgres's, so `sqlc generate` + `go build` + `go vet` all pass on
// statements the server rejects at prepare time. Two things here are only knowable by
// executing them —
//
//   - SetRunRunning's new guard correlates a subquery on run_user_inputs against
//     runs.open_question_id, a column of the statement's own UPDATE target;
//   - SetRunAwaitingInput writes a column added in the same migration as two widened
//     CHECK constraints.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; the store-IT
// runner (e2e/run-store-it.sh) provides one.

type awaitingInputFixture struct {
	q        *store.Queries
	pool     *pgxpool.Pool
	userID   uuid.UUID
	repoID   uuid.UUID
	workerID uuid.UUID
	runID    uuid.UUID
}

func setupAwaitingInput(ctx context.Context, t *testing.T, dsn string) (*awaitingInputFixture, func()) {
	t.Helper()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	userID, connID, repoID, workerID, runID := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("askuser-%s@e2e", userID))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, repoID, connID)
	mustExec(ctx, t, pool,
		`INSERT INTO workers (id, user_id, name, token_hash, max_concurrent_runs)
		 VALUES ($1, $2, 'w', $3, 4)`, workerID, userID, fmt.Sprintf("hash-%s", workerID))
	mustExec(ctx, t, pool,
		`INSERT INTO runs (id, user_id, repo_id, worker_id, issue_iid, issue_title, issue_description, status)
		 VALUES ($1, $2, $3, $4, 1, 't', 'd', 'running')`, runID, userID, repoID, workerID)

	return &awaitingInputFixture{q: store.New(pool), pool: pool, userID: userID, repoID: repoID, workerID: workerID, runID: runID},
		func() { pool.Close() }
}

func pgT(s string) pgtype.Text { return pgtype.Text{String: s, Valid: true} }
func pgU(u uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: u, Valid: true}
}

// The park itself, plus the health-exit contract. That contract is what makes leaving
// `awaiting_input` OUT of ListActiveRunsForHealth safe: a run flagged stalled/looping
// while running would otherwise carry the flag through the whole park with nothing
// able to clear it. The two are coupled, so this pins the pair rather than the write.
func TestSetRunAwaitingInputLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	f, done := setupAwaitingInput(ctx, t, dsn)
	defer done()

	// Flag the run unhealthy first, so the clear below is observable rather than vacuous.
	if _, err := f.q.SetRunHealth(ctx, store.SetRunHealthParams{
		ID: f.runID, Health: "stalled", HealthReason: pgT("no activity"),
	}); err != nil {
		t.Fatalf("SetRunHealth: %v", err)
	}

	rows, err := f.q.SetRunAwaitingInput(ctx, store.SetRunAwaitingInputParams{
		OpenQuestionID: pgT("q-1"), ID: f.runID, WorkerID: pgU(f.workerID),
	})
	if err != nil {
		t.Fatalf("SetRunAwaitingInput: %v", err)
	}
	if rows != 1 {
		t.Fatalf("SetRunAwaitingInput affected %d rows, want 1", rows)
	}

	run, err := f.q.GetRunByID(ctx, f.runID)
	if err != nil {
		t.Fatalf("GetRunByID: %v", err)
	}
	if run.Status != "awaiting_input" {
		t.Fatalf("status = %q, want awaiting_input", run.Status)
	}
	if run.OpenQuestionID.String != "q-1" {
		t.Fatalf("open_question_id = %q, want q-1", run.OpenQuestionID.String)
	}
	if run.Health != "ok" || run.HealthReason.Valid || run.HealthSince.Valid {
		t.Fatalf("health not cleared on entry: health=%q reason=%v since=%v — the ListActiveRunsForHealth omission depends on this",
			run.Health, run.HealthReason, run.HealthSince)
	}
}

// Ownership and terminality guards, mirroring SetRunAwaitingApproval's.
func TestSetRunAwaitingInputGuardsLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	f, done := setupAwaitingInput(ctx, t, dsn)
	defer done()

	rows, err := f.q.SetRunAwaitingInput(ctx, store.SetRunAwaitingInputParams{
		OpenQuestionID: pgT("q-1"), ID: f.runID, WorkerID: pgU(uuid.New()),
	})
	if err != nil {
		t.Fatalf("SetRunAwaitingInput (foreign worker): %v", err)
	}
	if rows != 0 {
		t.Fatalf("a foreign worker parked the run (%d rows) — the worker_id guard is not holding", rows)
	}

	if _, err := f.q.MarkRunFailedByID(ctx, store.MarkRunFailedByIDParams{
		ID: f.runID, FailureReason: pgT("done"),
	}); err != nil {
		t.Fatalf("MarkRunFailedByID: %v", err)
	}
	rows, err = f.q.SetRunAwaitingInput(ctx, store.SetRunAwaitingInputParams{
		OpenQuestionID: pgT("q-1"), ID: f.runID, WorkerID: pgU(f.workerID),
	})
	if err != nil {
		t.Fatalf("SetRunAwaitingInput (terminal): %v", err)
	}
	if rows != 0 {
		t.Fatalf("a terminal run was parked (%d rows) — the terminal guard is not holding", rows)
	}
}

// The resume guard, and the reason it is keyed on question IDENTITY.
//
// The discriminating fixture matters here (D-B): a run carrying BOTH a consumed
// approve_plan and a consumed answer would satisfy either clause alone, so no
// assertion on it could observe one clause being deleted. Each case below has exactly
// ONE of the two.
func TestSetRunRunningAnswerGuardLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	f, done := setupAwaitingInput(ctx, t, dsn)
	defer done()

	park := func(qid string) {
		t.Helper()
		if _, err := f.q.SetRunAwaitingInput(ctx, store.SetRunAwaitingInputParams{
			OpenQuestionID: pgT(qid), ID: f.runID, WorkerID: pgU(f.workerID),
		}); err != nil {
			t.Fatalf("park on %s: %v", qid, err)
		}
	}
	resume := func() int64 {
		t.Helper()
		rows, err := f.q.SetRunRunning(ctx, store.SetRunRunningParams{
			ID: f.runID, WorkerID: pgU(f.workerID), IterationCount: 1,
		})
		if err != nil {
			t.Fatalf("SetRunRunning: %v", err)
		}
		return rows
	}
	answer := func(qid string, consume bool) {
		t.Helper()
		row, err := f.q.CreateRunAnswerInput(ctx, store.CreateRunAnswerInputParams{
			RunID: f.runID, Body: pgT(`{"question_id":"` + qid + `","answers":["yes"]}`), QuestionID: pgT(qid),
		})
		if err != nil {
			t.Fatalf("CreateRunAnswerInput(%s): %v", qid, err)
		}
		if !consume {
			return
		}
		if _, err := f.q.ConsumeRunInputs(ctx, f.runID); err != nil {
			t.Fatalf("ConsumeRunInputs: %v", err)
		}
		_ = row
	}

	// 1. Parked, no answer at all ⇒ the resume is refused. This is #44 F2: a
	//    retry-delayed fire-and-forget `running` report must not un-park the run.
	park("q-1")
	if rows := resume(); rows != 0 {
		t.Fatalf("a parked run resumed with no answer (%d rows) — #44 F2 is re-opened", rows)
	}

	// 2. An UNCONSUMED answer is not enough either: the guard requires the worker to
	//    have actually taken delivery.
	answer("q-1", false)
	if rows := resume(); rows != 0 {
		t.Fatalf("an unconsumed answer satisfied the guard (%d rows)", rows)
	}

	// 3. Consumed answer naming the OPEN question ⇒ the resume goes through, and the
	//    health exit-contract fires on the way back to running.
	if _, err := f.q.ConsumeRunInputs(ctx, f.runID); err != nil {
		t.Fatalf("ConsumeRunInputs: %v", err)
	}
	if rows := resume(); rows != 1 {
		t.Fatalf("a consumed answer for the open question did not resume the run (%d rows)", rows)
	}
	run, err := f.q.GetRunByID(ctx, f.runID)
	if err != nil {
		t.Fatalf("GetRunByID: %v", err)
	}
	if run.Status != "running" {
		t.Fatalf("status = %q, want running", run.Status)
	}

	// 4. THE CASE A has-ever-been-answered GUARD GETS WRONG. The run parks again on a
	//    NEW question. The consumed answer to q-1 is still in the table, so the plan
	//    gate's shipped predicate ("a consumed input of this kind exists") would be
	//    permanently true from here on and would un-park every later question. Keyed
	//    on identity, it is correctly refused.
	park("q-2")
	if rows := resume(); rows != 0 {
		t.Fatalf("a consumed answer to the PREVIOUS question resumed a park on the current one (%d rows) — "+
			"the guard has degenerated to has-ever-been-answered, leaving questions 2..QUESTION_MAX unprotected", rows)
	}

	// 5. And the answer to the CURRENT question resumes it.
	answer("q-2", true)
	if rows := resume(); rows != 1 {
		t.Fatalf("a consumed answer for the current question did not resume the run (%d rows)", rows)
	}
}

// B2 regression, store half: leaving the park CLEARS the question id.
//
// Nothing else clears it, and a stale id is not inert — the claim payload re-delivers
// open_question_id on every resume, so a worker restarting after an ANSWERED park
// would seed its map with the resolved id and re-use it for a genuinely new question.
// The guard would then degenerate to has-ever-been-answered and the old question's
// answer would satisfy the new one. The worker half of this regression lives in
// agent/test, because choosing the id is worker behaviour that no store test reaches.
func TestSetRunRunningClearsOpenQuestionLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	f, done := setupAwaitingInput(ctx, t, dsn)
	defer done()

	if _, err := f.q.SetRunAwaitingInput(ctx, store.SetRunAwaitingInputParams{
		OpenQuestionID: pgT("q-1"), ID: f.runID, WorkerID: pgU(f.workerID),
	}); err != nil {
		t.Fatalf("park: %v", err)
	}
	if _, err := f.q.CreateRunAnswerInput(ctx, store.CreateRunAnswerInputParams{
		RunID: f.runID, Body: pgT(`{"question_id":"q-1","answers":["yes"]}`), QuestionID: pgT("q-1"),
	}); err != nil {
		t.Fatalf("CreateRunAnswerInput: %v", err)
	}
	if _, err := f.q.ConsumeRunInputs(ctx, f.runID); err != nil {
		t.Fatalf("ConsumeRunInputs: %v", err)
	}
	if rows, err := f.q.SetRunRunning(ctx, store.SetRunRunningParams{
		ID: f.runID, WorkerID: pgU(f.workerID), IterationCount: 1,
	}); err != nil || rows != 1 {
		t.Fatalf("SetRunRunning: rows=%d err=%v", rows, err)
	}

	run, err := f.q.GetRunByID(ctx, f.runID)
	if err != nil {
		t.Fatalf("GetRunByID: %v", err)
	}
	if run.OpenQuestionID.Valid {
		t.Fatalf("open_question_id survived the resume as %q — a later resume would re-deliver it "+
			"on the claim and the worker would re-use a RESOLVED question's id for a new question",
			run.OpenQuestionID.String)
	}

	// And the clear is what makes the guard bite again: the run parks on a NEW
	// question, and the old consumed answer must not resume it.
	if _, err := f.q.SetRunAwaitingInput(ctx, store.SetRunAwaitingInputParams{
		OpenQuestionID: pgT("q-2"), ID: f.runID, WorkerID: pgU(f.workerID),
	}); err != nil {
		t.Fatalf("re-park: %v", err)
	}
	rows, err := f.q.SetRunRunning(ctx, store.SetRunRunningParams{
		ID: f.runID, WorkerID: pgU(f.workerID), IterationCount: 2,
	})
	if err != nil {
		t.Fatalf("SetRunRunning: %v", err)
	}
	if rows != 0 {
		t.Fatalf("the answer to q-1 resumed a park on q-2 (%d rows)", rows)
	}
}

// D-AG regression: the PRE-RUN path, FALLBACK case. Post-#307 the worker normally DOES
// emit a `running` report when a clarification resolves with an answer, so SetRunRunning
// usually intervenes on the pre-run resume and clears open_question_id first. This test
// deliberately exercises the case where that report is DROPPED or ABSENT (a worker that
// dies between consuming the answer and reporting, or a lost/late fire-and-forget
// report): it drives SetRunAwaitingInput -> answer -> ConsumeRunInputs ->
// SetRunAwaitingApproval with NO intervening SetRunRunning, so SetRunAwaitingApproval's
// own clear is the ONLY thing that can save the run. That path is still reachable and
// still necessary — the #307 report is best-effort, so the pre-run gate must not depend
// on it having landed — which is why this remains a valid regression.
//
// This is the chain the runner-layer "B2, WORKER half" tests cannot express, and the
// reason is worth stating: they reason about the id lifecycle correctly but assume
// SetRunRunning always intervenes between a park and the next question. True mid-run,
// and true pre-run too WHEN the #307 report lands — but this test pins the case where it
// does not. M4 added the pre-run path AFTER those tests were written; the tests never
// became wrong, only insufficient, which is why nothing went red.
func TestSetRunAwaitingApprovalClearsOpenQuestionLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	f, done := setupAwaitingInput(ctx, t, dsn)
	defer done()

	// 1. Park PRE-RUN on q-1 and answer it.
	if _, err := f.q.SetRunAwaitingInput(ctx, store.SetRunAwaitingInputParams{
		OpenQuestionID: pgT("q-1"), ID: f.runID, WorkerID: pgU(f.workerID),
	}); err != nil {
		t.Fatalf("pre-run park: %v", err)
	}
	if _, err := f.q.CreateRunAnswerInput(ctx, store.CreateRunAnswerInputParams{
		RunID: f.runID, Body: pgT(`{"question_id":"q-1","answers":["postgres"]}`), QuestionID: pgT("q-1"),
	}); err != nil {
		t.Fatalf("CreateRunAnswerInput: %v", err)
	}
	if _, err := f.q.ConsumeRunInputs(ctx, f.runID); err != nil {
		t.Fatalf("ConsumeRunInputs: %v", err)
	}

	// 2. The lead re-plans and submits. NO `running` report happens on this path —
	//    that absence is the whole defect, so the test must not insert one.
	if rows, err := f.q.SetRunAwaitingApproval(ctx, store.SetRunAwaitingApprovalParams{
		PlanMd: pgT("# plan"), ID: f.runID, WorkerID: pgU(f.workerID),
	}); err != nil || rows != 1 {
		t.Fatalf("SetRunAwaitingApproval: rows=%d err=%v", rows, err)
	}

	run, err := f.q.GetRunByID(ctx, f.runID)
	if err != nil {
		t.Fatalf("GetRunByID: %v", err)
	}
	if run.OpenQuestionID.Valid {
		t.Fatalf("a resolved question id survived to the plan gate as %q — a worker death here "+
			"re-delivers it on the claim, the worker re-uses it for a NEW question, and q-1's "+
			"consumed answer then resumes that question (D-AG)", run.OpenQuestionID.String)
	}

	// 3. The consequence, asserted directly rather than inferred: park on a NEW
	//    question and confirm q-1's still-consumed answer does not resume it.
	if _, err := f.q.SetRunAwaitingInput(ctx, store.SetRunAwaitingInputParams{
		OpenQuestionID: pgT("q-2"), ID: f.runID, WorkerID: pgU(f.workerID),
	}); err != nil {
		t.Fatalf("re-park on q-2: %v", err)
	}
	rows, err := f.q.SetRunRunning(ctx, store.SetRunRunningParams{
		ID: f.runID, WorkerID: pgU(f.workerID), IterationCount: 1,
	})
	if err != nil {
		t.Fatalf("SetRunRunning: %v", err)
	}
	if rows != 0 {
		t.Fatalf("q-1's consumed answer resumed a park on q-2 via the pre-run path (%d rows)", rows)
	}
}

// PRD #71 M5 (load-bearing security): parking at the plan gate must CLEAR auto_approve,
// symmetric with the plan_source='agent' clear the sibling test above pins. A PRD #71
// auto ci_fix run whose plan is CI-config-classified parks here (the M5 forceGate) with
// auto_approve STILL true; if the park leaves it true, a worker-restart requeue
// (Register orphan-recovery) resumes with plan_approved = run.AutoApprove || ... = true,
// which short-circuits the executor's gate and pushes the CI-config edit with NO human
// in the loop. Clearing auto_approve here is what makes the resume re-gate.
func TestSetRunAwaitingApprovalClearsAutoApproveLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	f, done := setupAwaitingInput(ctx, t, dsn)
	defer done()

	// An auto-triggered ci_fix run carries auto_approve=true into the park (the only
	// run that parks with it still set — a normal autopilot run short-circuits gatePlan).
	mustExec(ctx, t, f.pool, `UPDATE runs SET auto_approve = true WHERE id = $1`, f.runID)

	if rows, err := f.q.SetRunAwaitingApproval(ctx, store.SetRunAwaitingApprovalParams{
		PlanMd: pgT("# ci-config plan"), ID: f.runID, WorkerID: pgU(f.workerID),
	}); err != nil || rows != 1 {
		t.Fatalf("SetRunAwaitingApproval: rows=%d err=%v", rows, err)
	}

	run, err := f.q.GetRunByID(ctx, f.runID)
	if err != nil {
		t.Fatalf("GetRunByID: %v", err)
	}
	if run.AutoApprove {
		t.Fatal("auto_approve survived the plan-gate park — a restart-requeued resume would " +
			"ship plan_approved=true and skip the gate with no human in the loop (PRD #71 M5)")
	}
	// The sibling decouple is asserted here too, so both halves of the plan_approved
	// derivation's non-human disjuncts are pinned to stop firing after a park.
	if run.PlanSource != "agent" {
		t.Fatalf("plan_source = %q after park, want \"agent\" (PRD #209 D8 decouple)", run.PlanSource)
	}
}

// B1 regression: the REGISTER-time orphan sweeps must see a parked run.
//
// The stale-heartbeat sweeps cannot cover this and a test of them cannot stand in:
// RegisterWorker stamps last_heartbeat_at = now(), so a worker restarting faster than
// WORKER_HEARTBEAT_STALE is never stale. This is the `docker compose down && up`
// path that RequeueWorkerRuns' own comment names.
func TestRegisterSweepsMoveAwaitingInputLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()

	// Within budget → re-queued to the same worker for an affinity resume.
	f, done := setupAwaitingInput(ctx, t, dsn)
	defer done()
	if _, err := f.q.SetRunAwaitingInput(ctx, store.SetRunAwaitingInputParams{
		OpenQuestionID: pgT("q-1"), ID: f.runID, WorkerID: pgU(f.workerID),
	}); err != nil {
		t.Fatalf("park: %v", err)
	}
	rows, err := f.q.RequeueWorkerRuns(ctx, store.RequeueWorkerRunsParams{
		WorkerID: pgU(f.workerID), MaxRequeues: 3,
	})
	if err != nil {
		t.Fatalf("RequeueWorkerRuns: %v", err)
	}
	if rows != 1 {
		t.Fatalf("RequeueWorkerRuns left the parked run alone (%d rows) — an ordinary worker "+
			"restart strands it in awaiting_input forever, pointing at dead execution", rows)
	}
	run, err := f.q.GetRunByID(ctx, f.runID)
	if err != nil {
		t.Fatalf("GetRunByID: %v", err)
	}
	if run.Status != "queued" {
		t.Fatalf("status = %q, want queued", run.Status)
	}

	// Over budget → failed rather than re-queued.
	f2, done2 := setupAwaitingInput(ctx, t, dsn)
	defer done2()
	if _, err := f2.q.SetRunAwaitingInput(ctx, store.SetRunAwaitingInputParams{
		OpenQuestionID: pgT("q-1"), ID: f2.runID, WorkerID: pgU(f2.workerID),
	}); err != nil {
		t.Fatalf("park: %v", err)
	}
	ids, err := f2.q.FailWorkerRunsOverCap(ctx, store.FailWorkerRunsOverCapParams{
		FailureReason: pgT("worker restarted"), WorkerID: pgU(f2.workerID), MaxRequeues: 0,
	})
	if err != nil {
		t.Fatalf("FailWorkerRunsOverCap: %v", err)
	}
	if len(ids) != 1 {
		t.Fatalf("FailWorkerRunsOverCap left the parked run non-terminal (%d rows) — it would sit "+
			"forever holding move_pending_since and a board card", len(ids))
	}
}

// The two clauses are independent, pinned in BOTH directions.
//
// Direction 1 (this test): a consumed approve_plan must not resume a run parked on a
// QUESTION. Direction 2 (the subtest below): a consumed answer must not resume a run
// parked at the PLAN gate. A merged `status NOT IN (...) OR kind IN (...)` predicate
// passes every happy path above and fails exactly here.
//
// Both live in this file on purpose, and what they uniquely catch is narrower than
// two earlier versions of this comment claimed. Measured against a real Postgres, by
// mutating the GENERATED const in runtime.sql.go (the .sql file is semantically inert
// until sqlc regenerates):
//
//	                                    plan clause    the two clauses
//	                                     DELETED         MERGED
//	ClausesAreIndependent (dir 1)         pass            FAIL
//	AnswerDoesNotSatisfyPlanGate (dir 2)  FAIL            FAIL
//	SetRunRunningAnswerGuard              pass            FAIL
//	AwaitingApprovalGuard (pre-existing)  FAIL            pass   ← blind to the merge
//
// So DELETING the plan clause was never invisible: the pre-existing PRD #44 F2 guard
// test has always caught it. MERGING the two into one
// `status NOT IN (...) OR kind IN (...)` predicate is the hole these tests exist for —
// a merged predicate still satisfies every fixture holding the matching input kind,
// so only a fixture holding the WRONG kind for its gate can tell the difference, and
// that is exactly what direction 2 builds.
//
// Two corrections got us here and the second is the instructive one. The first
// version pointed at "its mirror in the plan-gate tests", which did not exist —
// run_running_guard_integration_test.go contains no `answer` at all. The second said
// deletion left the suite green; it did not. Both were claims about what a test
// guards, written without running the mutation they named. A comment that
// misdescribes its own test's value fails the same way as one pointing at the wrong
// file: it stops the next reader checking.
func TestSetRunRunningClausesAreIndependentLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	f, done := setupAwaitingInput(ctx, t, dsn)
	defer done()

	if _, err := f.q.SetRunAwaitingInput(ctx, store.SetRunAwaitingInputParams{
		OpenQuestionID: pgT("q-1"), ID: f.runID, WorkerID: pgU(f.workerID),
	}); err != nil {
		t.Fatalf("park: %v", err)
	}
	// Exactly ONE of the two input kinds — an approve_plan, which belongs to the OTHER
	// clause. If the clauses were merged this would wrongly resume the run.
	if _, err := f.q.CreateRunInput(ctx, store.CreateRunInputParams{
		RunID: f.runID, Kind: "approve_plan", Body: pgT(`{"source":"own","exclusions":[]}`),
	}); err != nil {
		t.Fatalf("CreateRunInput(approve_plan): %v", err)
	}
	if _, err := f.q.ConsumeRunInputs(ctx, f.runID); err != nil {
		t.Fatalf("ConsumeRunInputs: %v", err)
	}
	rows, err := f.q.SetRunRunning(ctx, store.SetRunRunningParams{
		ID: f.runID, WorkerID: pgU(f.workerID), IterationCount: 1,
	})
	if err != nil {
		t.Fatalf("SetRunRunning: %v", err)
	}
	if rows != 0 {
		t.Fatalf("a consumed approve_plan resumed a run parked on a QUESTION (%d rows) — "+
			"the two guard clauses have been merged, re-opening #44 F2 sideways", rows)
	}
}

// Direction 2: a consumed `answer` must not satisfy the PLAN gate. This is the half
// that was missing, and it is the one that catches a merged predicate.
func TestSetRunRunningAnswerDoesNotSatisfyPlanGateLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	f, done := setupAwaitingInput(ctx, t, dsn)
	defer done()

	if _, err := f.q.SetRunAwaitingApproval(ctx, store.SetRunAwaitingApprovalParams{
		PlanMd: pgT("# plan"), ID: f.runID, WorkerID: pgU(f.workerID),
	}); err != nil {
		t.Fatalf("park at the plan gate: %v", err)
	}
	// EXACTLY ONE input kind, and it is the wrong one for this gate (D-B): on a run
	// holding both, either clause alone would satisfy the predicate and no assertion
	// could observe one being deleted.
	if _, err := f.q.CreateRunAnswerInput(ctx, store.CreateRunAnswerInputParams{
		RunID: f.runID, Body: pgT(`{"question_id":"q-1","answers":["yes"]}`), QuestionID: pgT("q-1"),
	}); err != nil {
		t.Fatalf("CreateRunAnswerInput: %v", err)
	}
	if _, err := f.q.ConsumeRunInputs(ctx, f.runID); err != nil {
		t.Fatalf("ConsumeRunInputs: %v", err)
	}
	rows, err := f.q.SetRunRunning(ctx, store.SetRunRunningParams{
		ID: f.runID, WorkerID: pgU(f.workerID), IterationCount: 1,
	})
	if err != nil {
		t.Fatalf("SetRunRunning: %v", err)
	}
	if rows != 0 {
		t.Fatalf("a consumed answer resumed a run parked at the PLAN gate (%d rows) — the two guard "+
			"clauses have been merged, so an answer now approves a plan no human saw", rows)
	}
}

// The CHECK constraints the migration widened, asserted directly: a bogus value must
// still be rejected, so the widening did not simply drop the constraint.
func TestAwaitingInputConstraintsLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	f, done := setupAwaitingInput(ctx, t, dsn)
	defer done()

	// revise_plan is the one a migration written from the PRD's stale four-value list
	// would have silently deleted, breaking PRD #41's shipped feature.
	for _, kind := range []string{"follow_up", "approve_plan", "reject_plan", "cancel", "revise_plan", "answer"} {
		if _, err := f.q.CreateRunInput(ctx, store.CreateRunInputParams{
			RunID: f.runID, Kind: kind, Body: pgT("x"),
		}); err != nil {
			t.Fatalf("kind %q rejected by the CHECK, but it is a shipped kind: %v", kind, err)
		}
	}
	if _, err := f.q.CreateRunInput(ctx, store.CreateRunInputParams{
		RunID: f.runID, Kind: "not_a_kind", Body: pgT("x"),
	}); err == nil {
		t.Fatal("the kind CHECK accepted a bogus value — the widening dropped the constraint instead of extending it")
	}

	// D-AF regression: runs_status_check must hold BOTH new statuses.
	//
	// This migration DROPs and re-ADDs runs_status_check, and it was authored when
	// 00090 was head — before PRD #35 landed 'limit_wait'. Renumbering it above #35's
	// migration made it run second, so a value list that forgot 'limit_wait' would
	// silently narrow the domain back and reject every later park. The failure is
	// invisible to every test that does not put a run in that status, which is why it
	// is asserted here rather than left to the migration applying cleanly.
	for _, st := range []string{"awaiting_input", "limit_wait", "awaiting_approval", "running"} {
		if _, err := pgExec(ctx, f, st); err != nil {
			t.Fatalf("runs_status_check rejected %q, which is a shipped status: %v", st, err)
		}
	}
}

// pgExec sets the fixture run's status directly, bypassing the typed setters, so the
// CHECK is what is under test rather than a query's own guards.
func pgExec(ctx context.Context, f *awaitingInputFixture, status string) (int64, error) {
	tag, err := f.pool.Exec(ctx, `UPDATE runs SET status = $2 WHERE id = $1`, f.runID, status)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
