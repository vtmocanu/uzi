package store_test

// PRD #517 M2 — the SQL status-set decisions for the interactive-task park
// (`awaiting_followup`) against a REAL Postgres. This is the milestone's core risk:
// a missed predicate is a zombie parked run or fleet mis-scheduling, and none of it
// is visible to `sqlc generate` + `go build` + `go vet` (sqlc's type deduction is not
// Postgres's — see awaiting_input_integration_test.go). Every assertion here is chosen
// to be DISCRIMINATING: it distinguishes "awaiting_followup is in the predicate" from
// "it isn't", so removing the value from the IN-list / guard reddens the positive case.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; run via
// e2e/run-store-it.sh. A package that prints `ok` with PASS=0 is INVALID, not green.

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/store"
)

type followupFixture struct {
	q      *store.Queries
	pool   *pgxpool.Pool
	userID uuid.UUID
	repoID uuid.UUID
}

func setupFollowup(ctx context.Context, t *testing.T, dsn string) (*followupFixture, func()) {
	t.Helper()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	userID, connID, repoID := uuid.New(), uuid.New(), uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("followup-%s@e2e", userID))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, repoID, connID)
	return &followupFixture{q: store.New(pool), pool: pool, userID: userID, repoID: repoID},
		func() { pool.Close() }
}

// seedWorker inserts a worker directly so the test controls status, heartbeat and cap
// (the concurrency/recovery predicates key off exactly these). staleHeartbeat pushes
// last_heartbeat_at an hour into the past so RequeueRunsOfStaleWorkers's cutoff sees it.
func (f *followupFixture) seedWorker(ctx context.Context, t *testing.T, status string, maxRuns pgtype.Int4, staleHeartbeat bool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	hb := "now()"
	if staleHeartbeat {
		hb = "now() - interval '1 hour'"
	}
	mustExec(ctx, t, f.pool,
		`INSERT INTO workers (id, user_id, name, token_hash, status, max_concurrent_runs, last_heartbeat_at)
		 VALUES ($1, $2, 'w', $3, $4, $5, `+hb+`)`,
		id, f.userID, tokenHash(), status, maxRuns)
	return id
}

// seedInteractiveTaskRun creates a real interactive kind='task' run (CreateTaskRun,
// so the runs_kind_shape constraint is satisfied honestly) and moves it into `status`
// held by worker — the exact shape a park produces. A NULL worker leaves it unheld.
func (f *followupFixture) seedInteractiveTaskRun(ctx context.Context, t *testing.T, status string, worker *uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := f.q.CreateTaskRun(ctx, store.CreateTaskRunParams{
		RunID:            id,
		UserID:           f.userID,
		RepoID:           f.repoID,
		Branch:           pgtype.Text{String: "uzi/task/" + id.String(), Valid: true},
		Interactive:      true,
		IssueTitle:       "interactive handoff",
		IssueDescription: "iterate with me",
	}); err != nil {
		t.Fatalf("CreateTaskRun: %v", err)
	}
	mustExec(ctx, t, f.pool, `UPDATE runs SET status = $2, worker_id = $3 WHERE id = $1`, id, status, worker)
	return id
}

// seedNonInteractiveTaskRun creates a NON-interactive kind='task' run (a plain handoff)
// held by worker — the contrast case for the recovery/sweep tests: it exercises the
// pre-#517 requeue behavior the awaiting_followup additions must leave unchanged.
func (f *followupFixture) seedNonInteractiveTaskRun(ctx context.Context, t *testing.T, status string, worker *uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := f.q.CreateTaskRun(ctx, store.CreateTaskRunParams{
		RunID:            id,
		UserID:           f.userID,
		RepoID:           f.repoID,
		Branch:           pgtype.Text{String: "uzi/task/" + id.String(), Valid: true},
		Interactive:      false,
		IssueTitle:       "plain handoff",
		IssueDescription: "no interaction",
	}); err != nil {
		t.Fatalf("CreateTaskRun(non-interactive): %v", err)
	}
	mustExec(ctx, t, f.pool, `UPDATE runs SET status = $2, worker_id = $3 WHERE id = $1`, id, status, worker)
	return id
}

func (f *followupFixture) status(ctx context.Context, t *testing.T, id uuid.UUID) string {
	t.Helper()
	run, err := f.q.GetRunByID(ctx, id)
	if err != nil {
		t.Fatalf("GetRunByID: %v", err)
	}
	return run.Status
}

// 1. RECOVERY, BOTH PATHS. A parked interactive task whose worker dies must be
// requeued — via the REGISTER path (RequeueWorkerRuns / FailWorkerRunsOverCap, which a
// fast-restarting worker hits because it is never "stale") AND via the stale-heartbeat
// sweep (RequeueRunsOfStaleWorkers / FailRunsOfStaleWorkersOverCap). Miss the IN-list
// and the park becomes a zombie: awaiting_followup forever, pointing at dead execution.
//
// Non-vacuity: each path also holds a genuinely TERMINAL run (completed) under the same
// worker and asserts it is NOT touched — so the positive assertion is not satisfiable by
// a query that requeues everything. MUTATION PROOF: dropping 'awaiting_followup' from
// the IN-list makes RequeueWorkerRuns/RequeueRunsOfStaleWorkers skip the park, so the
// rows==1 / status=="queued" assertions fail (the park stays awaiting_followup).
func TestAwaitingFollowupRecoveryLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	f, done := setupFollowup(ctx, t, dsn)
	defer done()

	noCap := pgtype.Int4{}

	// ── Register-time path (worker-scoped, no heartbeat dependence — the
	//    `docker compose down && up` restart RequeueWorkerRuns' comment names). ──
	regWkr := f.seedWorker(ctx, t, "online", noCap, false)
	parked := f.seedInteractiveTaskRun(ctx, t, "awaiting_followup", &regWkr)
	terminal := f.seedInteractiveTaskRun(ctx, t, "completed", &regWkr)

	rows, err := f.q.RequeueWorkerRuns(ctx, store.RequeueWorkerRunsParams{
		WorkerID: pgU(regWkr), MaxRequeues: 3,
	})
	if err != nil {
		t.Fatalf("RequeueWorkerRuns: %v", err)
	}
	if rows != 1 {
		t.Fatalf("RequeueWorkerRuns requeued %d runs, want exactly 1 — a parked interactive task must be "+
			"requeued on register (remove awaiting_followup from the IN-list and this is 0, the zombie)", rows)
	}
	if got := f.status(ctx, t, parked); got != "queued" {
		t.Fatalf("parked run status = %q after RequeueWorkerRuns, want queued", got)
	}
	if got := f.status(ctx, t, terminal); got != "completed" {
		t.Fatalf("a completed run was requeued (status = %q) — the IN-list swept a terminal run", got)
	}

	// ── Stale-heartbeat sweep path (the periodic sweeper; worker is stale). ──
	staleWkr := f.seedWorker(ctx, t, "online", noCap, true)
	staleParked := f.seedInteractiveTaskRun(ctx, t, "awaiting_followup", &staleWkr)
	staleTerminal := f.seedInteractiveTaskRun(ctx, t, "completed", &staleWkr)

	requeued, err := f.q.RequeueRunsOfStaleWorkers(ctx, store.RequeueRunsOfStaleWorkersParams{
		MaxRequeues: 3, Cutoff: pgtype.Timestamptz{Time: time.Now(), Valid: true},
	})
	if err != nil {
		t.Fatalf("RequeueRunsOfStaleWorkers: %v", err)
	}
	var sawStaleParked bool
	for _, r := range requeued {
		if r.ID == staleTerminal {
			t.Fatalf("the stale sweep requeued a completed run — the IN-list swept a terminal run")
		}
		if r.ID == staleParked {
			sawStaleParked = true
		}
	}
	if !sawStaleParked {
		t.Fatalf("the stale-heartbeat sweep did NOT requeue the parked interactive task — remove "+
			"awaiting_followup from RequeueRunsOfStaleWorkers' IN-list and it becomes a zombie (requeued=%d)", len(requeued))
	}
	if got := f.status(ctx, t, staleParked); got != "queued" {
		t.Fatalf("stale-worker parked run status = %q after the sweep, want queued", got)
	}
	if got := f.status(ctx, t, staleTerminal); got != "completed" {
		t.Fatalf("a completed run under the stale worker changed to %q", got)
	}
}

// 1a. SC4b LOCK-IN — a DEAD-worker park is REQUEUED for worker finalize, NEVER completed
// in place, and its branch is PRESERVED. This is the PRD's highest single risk: a server
// sweep that flipped a git-backed park to `completed` cannot push the branch, so the
// checkpoint-pushed work is stranded (Decision 6). There is deliberately NO new
// server-side park-age (TASK_IDLE_TIMEOUT) sweep — the EXISTING stale-worker requeue (M2)
// is the whole server-side recovery. This test locks that in: it asserts the WORK/BRANCH
// outcome (not merely the status label), so it cannot codify the lost-work bug, and any
// future chat-style UPDATE→completed idle sweep for tasks reddens it.
//
// Controls that make the claim non-vacuous:
//   - a FRESH-heartbeat worker's park is UNTOUCHED (still awaiting_followup, branch intact):
//     a live park is finalized by its OWN worker-side idle (30m), never by a server sweep;
//   - a NON-interactive run under the dead worker is requeued exactly as before, so the
//     awaiting_followup addition changed nothing for non-interactive recovery.
//
// MUTATION PROOF: a code path that completed a dead park in place leaves status != "queued"
// and reddens the queued/requeue_count asserts; clearing the branch on requeue reddens the
// branch-preserved assert; a sweep that swept the fresh park reddens the untouched assert.
func TestAwaitingFollowupDeadWorkerRequeuedNotCompletedLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	f, done := setupFollowup(ctx, t, dsn)
	defer done()

	noCap := pgtype.Int4{}

	// DEAD (stale-heartbeat) worker: an interactive park + a non-interactive run.
	deadWkr := f.seedWorker(ctx, t, "online", noCap, true)
	deadParked := f.seedInteractiveTaskRun(ctx, t, "awaiting_followup", &deadWkr)
	deadNonInteractive := f.seedNonInteractiveTaskRun(ctx, t, "running", &deadWkr)

	// FRESH (live-heartbeat) worker: an interactive park the sweep must NOT touch.
	freshWkr := f.seedWorker(ctx, t, "online", noCap, false)
	freshParked := f.seedInteractiveTaskRun(ctx, t, "awaiting_followup", &freshWkr)

	// Snapshot the dead park's branch so we can assert it survives the requeue verbatim.
	before, err := f.q.GetRunByID(ctx, deadParked)
	if err != nil {
		t.Fatalf("GetRunByID(before): %v", err)
	}
	if !before.Branch.Valid || before.Branch.String == "" {
		t.Fatalf("test setup: the parked run has no branch to preserve")
	}

	// The cutoff must sit BETWEEN the two workers' heartbeats: the fresh worker
	// heartbeats at DB now(), the dead one an hour ago. A now() cutoff wrongly
	// catches the fresh worker because Go's time.Now() runs a few ms after the DB
	// stamped now() at seed. Use now-1min (production uses now-WorkerHeartbeatStale,
	// 45s) so fresh > cutoff > dead.
	requeued, err := f.q.RequeueRunsOfStaleWorkers(ctx, store.RequeueRunsOfStaleWorkersParams{
		MaxRequeues: 3, Cutoff: pgtype.Timestamptz{Time: time.Now().Add(-1 * time.Minute), Valid: true},
	})
	if err != nil {
		t.Fatalf("RequeueRunsOfStaleWorkers: %v", err)
	}

	var sawDeadParked, sawDeadNon, sawFresh bool
	for _, r := range requeued {
		switch r.ID {
		case deadParked:
			sawDeadParked = true
		case deadNonInteractive:
			sawDeadNon = true
		case freshParked:
			sawFresh = true
		}
		if r.Status != "queued" {
			t.Fatalf("RequeueRunsOfStaleWorkers returned run %s with status %q, want queued — the sweep must "+
				"REQUEUE a dead-worker park for worker finalize, never complete it in place", r.ID, r.Status)
		}
	}
	if !sawDeadParked {
		t.Fatalf("the dead-worker interactive park was NOT requeued (SC4b) — a missed IN-list entry makes it a " +
			"zombie, and a completing sweep would strand its unpushed work")
	}
	if !sawDeadNon {
		t.Fatalf("the dead-worker non-interactive run was NOT requeued — the awaiting_followup addition must not " +
			"have changed non-interactive recovery")
	}
	if sawFresh {
		t.Fatalf("the FRESH-heartbeat worker's park was requeued — the stale-worker sweep must not touch a live park")
	}

	// The dead park: queued (NOT completed), requeue_count incremented, branch PRESERVED —
	// the work outcome, not merely the status label.
	after, err := f.q.GetRunByID(ctx, deadParked)
	if err != nil {
		t.Fatalf("GetRunByID(after): %v", err)
	}
	if after.Status != "queued" {
		t.Fatalf("dead park status = %q after the sweep, want queued (NEVER completed)", after.Status)
	}
	if after.RequeueCount != before.RequeueCount+1 {
		t.Fatalf("dead park requeue_count = %d, want %d — the requeue must increment the counter",
			after.RequeueCount, before.RequeueCount+1)
	}
	if !after.Branch.Valid || after.Branch.String != before.Branch.String {
		t.Fatalf("dead park branch = %q (valid=%v) after requeue, want %q preserved — a requeue must NOT clear the "+
			"branch, or the user's checkpoint-pushed work is orphaned", after.Branch.String, after.Branch.Valid, before.Branch.String)
	}

	// The fresh park is untouched: still awaiting_followup, branch intact. This is the guard
	// that NO server sweep completes (or otherwise moves) a LIVE park in place.
	freshAfter, err := f.q.GetRunByID(ctx, freshParked)
	if err != nil {
		t.Fatalf("GetRunByID(fresh): %v", err)
	}
	if freshAfter.Status != "awaiting_followup" {
		t.Fatalf("the fresh-worker park status = %q, want awaiting_followup — a live park must be left for its OWN "+
			"worker-side idle to finalize; no server sweep may complete it", freshAfter.Status)
	}
	if !freshAfter.Branch.Valid || freshAfter.Branch.String == "" {
		t.Fatalf("the fresh-worker park lost its branch (%q) — an untouched park must keep its work", freshAfter.Branch.String)
	}
}

// 1b. OVER-CAP RECOVERY, BOTH FAIL PATHS. A parked interactive task whose worker dies
// AND which has already spent its re-queue budget must be FAILED (not requeued) — via the
// REGISTER path (FailWorkerRunsOverCap) and the stale-heartbeat sweep
// (FailRunsOfStaleWorkersOverCap). Test 1 seeds requeue_count=0, so only the requeue
// branch runs and the awaiting_followup additions to these two over-cap queries go
// unexercised; this test drives them at/over the cap.
//
// Non-vacuity: each path also holds a genuinely TERMINAL run (completed) at the same
// over-cap requeue_count under the same worker and asserts it is NOT failed — so the
// positive assertion is not satisfiable by a query that fails everything. MUTATION PROOF:
// drop 'awaiting_followup' from either over-cap IN-list and the park is skipped (rows==0,
// status stays awaiting_followup) — the zombie the recovery is meant to prevent, now a
// PERMANENT one because the requeue path also skipped it.
func TestAwaitingFollowupOverCapFailedLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	f, done := setupFollowup(ctx, t, dsn)
	defer done()

	noCap := pgtype.Int4{}
	// atCap pins requeue_count to the cap so the over-cap predicate (requeue_count >=
	// max_requeues, with MaxRequeues=3 below) selects the run and the within-budget
	// requeue predicate (requeue_count < max_requeues) does not.
	atCap := func(id uuid.UUID) {
		mustExec(ctx, t, f.pool, `UPDATE runs SET requeue_count = 3 WHERE id = $1`, id)
	}

	// ── Register-time over-cap path (worker-scoped: FailWorkerRunsOverCap). ──
	regWkr := f.seedWorker(ctx, t, "online", noCap, false)
	parked := f.seedInteractiveTaskRun(ctx, t, "awaiting_followup", &regWkr)
	terminal := f.seedInteractiveTaskRun(ctx, t, "completed", &regWkr)
	atCap(parked)
	atCap(terminal)

	failed, err := f.q.FailWorkerRunsOverCap(ctx, store.FailWorkerRunsOverCapParams{
		WorkerID: pgU(regWkr), MaxRequeues: 3, FailureReason: pgT("worker restarted over cap"),
	})
	if err != nil {
		t.Fatalf("FailWorkerRunsOverCap: %v", err)
	}
	if len(failed) != 1 || failed[0] != parked {
		t.Fatalf("FailWorkerRunsOverCap returned %+v, want exactly the parked run — an over-cap parked "+
			"interactive task must be FAILED on register (drop awaiting_followup from the IN-list and this "+
			"is empty, a permanent zombie)", failed)
	}
	if got := f.status(ctx, t, parked); got != "failed" {
		t.Fatalf("over-cap parked run status = %q after FailWorkerRunsOverCap, want failed", got)
	}
	if got := f.status(ctx, t, terminal); got != "completed" {
		t.Fatalf("a completed run was failed (status = %q) — the over-cap IN-list swept a terminal run", got)
	}

	// ── Stale-heartbeat over-cap sweep path (FailRunsOfStaleWorkersOverCap). ──
	staleWkr := f.seedWorker(ctx, t, "online", noCap, true)
	staleParked := f.seedInteractiveTaskRun(ctx, t, "awaiting_followup", &staleWkr)
	staleTerminal := f.seedInteractiveTaskRun(ctx, t, "completed", &staleWkr)
	atCap(staleParked)
	atCap(staleTerminal)

	staleFailed, err := f.q.FailRunsOfStaleWorkersOverCap(ctx, store.FailRunsOfStaleWorkersOverCapParams{
		MaxRequeues: 3, Cutoff: pgtype.Timestamptz{Time: time.Now(), Valid: true},
		FailureReason: pgT("stale worker over cap"),
	})
	if err != nil {
		t.Fatalf("FailRunsOfStaleWorkersOverCap: %v", err)
	}
	var sawStaleParked bool
	for _, r := range staleFailed {
		if r.ID == staleTerminal {
			t.Fatalf("the stale over-cap sweep failed a completed run — the IN-list swept a terminal run")
		}
		if r.ID == staleParked {
			sawStaleParked = true
		}
	}
	if !sawStaleParked {
		t.Fatalf("the stale over-cap sweep did NOT fail the parked interactive task — remove awaiting_followup "+
			"from FailRunsOfStaleWorkersOverCap's IN-list and the over-cap park becomes a permanent zombie "+
			"(failed=%d)", len(staleFailed))
	}
	if got := f.status(ctx, t, staleParked); got != "failed" {
		t.Fatalf("stale-worker over-cap parked run status = %q after the sweep, want failed", got)
	}
	if got := f.status(ctx, t, staleTerminal); got != "completed" {
		t.Fatalf("a completed run under the stale worker changed to %q", got)
	}
}

// 2. CONCURRENCY / LOAD. A park holds an in-process slot (Decision 3), so it must count
// as ACTIVE — or the fleet schedules new runs onto a worker that is actually full and
// the UI under-reports load. Assert it counts in ListWorkersByUser (active_runs/busy),
// ListAllWorkers (admin), and that it reduces free slots in
// CountOnlineWorkersWithFreeSlotForUser.
//
// Non-vacuity: the worker also holds a terminal (completed) run that must NOT count, and
// the cap is 1 so a single park SATURATES the worker. MUTATION PROOF: drop
// 'awaiting_followup' from these IN-lists and active_runs reads 0 (want 1),
// busy reads false (want true), and the free-slot count reads 1 (want 0) — every
// positive assertion flips.
func TestAwaitingFollowupCountsAsActiveLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	f, done := setupFollowup(ctx, t, dsn)
	defer done()

	cap1 := pgtype.Int4{Int32: 1, Valid: true}
	wkr := f.seedWorker(ctx, t, "online", cap1, false)
	f.seedInteractiveTaskRun(ctx, t, "awaiting_followup", &wkr) // the ONLY active run
	f.seedInteractiveTaskRun(ctx, t, "completed", &wkr)         // terminal: must not count

	byUser, err := f.q.ListWorkersByUser(ctx, f.userID)
	if err != nil {
		t.Fatalf("ListWorkersByUser: %v", err)
	}
	var seen bool
	for _, row := range byUser {
		if row.ID != wkr {
			continue
		}
		seen = true
		if row.ActiveRuns != 1 {
			t.Fatalf("ListWorkersByUser active_runs = %d, want 1 — a park holds a run-lane slot "+
				"(exclude awaiting_followup and this is 0, so the fleet over-schedules a full worker)", row.ActiveRuns)
		}
		if !row.Busy {
			t.Fatalf("ListWorkersByUser busy = false, want true (a parked interactive task keeps the worker busy)")
		}
	}
	if !seen {
		t.Fatalf("ListWorkersByUser did not return the worker")
	}

	all, err := f.q.ListAllWorkers(ctx)
	if err != nil {
		t.Fatalf("ListAllWorkers: %v", err)
	}
	seen = false
	for _, row := range all {
		if row.Worker.ID != wkr {
			continue
		}
		seen = true
		if row.ActiveRuns != 1 || !row.Busy {
			t.Fatalf("ListAllWorkers active_runs=%d busy=%v, want 1/true (admin load view must count the park)", row.ActiveRuns, row.Busy)
		}
	}
	if !seen {
		t.Fatalf("ListAllWorkers did not return the worker")
	}

	// cap=1, one active park ⇒ the worker is SATURATED, so it has no free slot.
	free, err := f.q.CountOnlineWorkersWithFreeSlotForUser(ctx, f.userID)
	if err != nil {
		t.Fatalf("CountOnlineWorkersWithFreeSlotForUser: %v", err)
	}
	if free != 0 {
		t.Fatalf("CountOnlineWorkersWithFreeSlotForUser = %d, want 0 — a park fills the cap-1 worker's only "+
			"slot (exclude awaiting_followup and it reads 1, so the resolver mislabels a full fleet as idle)", free)
	}
}

// 3. AUTOSTOP CANNOT FAIL A PARK. FailRunAutoStop is the SQL backstop for the day the
// Go guard is relaxed; awaiting_followup must be excluded so a park can never be
// auto-failed (its message writes have STOPPED, not looped).
//
// Non-vacuity: the SAME query IS otherwise applicable — a `running` run is failed by it
// — so the awaiting_followup no-op is a property of the exclusion, not of an inert query.
// MUTATION PROOF: drop `AND status <> 'awaiting_followup'` and the park is failed
// (rows==1, status "failed"), reddening the rows==0 / status-unchanged assertions.
func TestFailRunAutoStopCannotFailAwaitingFollowupLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	f, done := setupFollowup(ctx, t, dsn)
	defer done()

	wkr := f.seedWorker(ctx, t, "online", pgtype.Int4{}, false)
	parked := f.seedInteractiveTaskRun(ctx, t, "awaiting_followup", &wkr)

	rows, err := f.q.FailRunAutoStop(ctx, store.FailRunAutoStopParams{
		ID: parked, FailureReason: pgT("should never land on a park"),
	})
	if err != nil {
		t.Fatalf("FailRunAutoStop(parked): %v", err)
	}
	if rows != 0 {
		t.Fatalf("FailRunAutoStop touched %d rows on an awaiting_followup run, want 0 — the SQL backstop "+
			"must never auto-fail a park", rows)
	}
	if got := f.status(ctx, t, parked); got != "awaiting_followup" {
		t.Fatalf("the park was auto-failed to %q — the FailRunAutoStop exclusion is not holding", got)
	}

	// Positive control: the query IS applicable to a running run, so the no-op above is
	// the exclusion working rather than an inert statement.
	running := f.seedInteractiveTaskRun(ctx, t, "running", &wkr)
	rows, err = f.q.FailRunAutoStop(ctx, store.FailRunAutoStopParams{
		ID: running, FailureReason: pgT("a live loop is fair game"),
	})
	if err != nil {
		t.Fatalf("FailRunAutoStop(running): %v", err)
	}
	if rows != 1 || f.status(ctx, t, running) != "failed" {
		t.Fatalf("FailRunAutoStop did NOT fail a running run (rows=%d) — the control is broken, so the "+
			"awaiting_followup no-op above proves nothing", rows)
	}
}

// 4. WAKE GUARD (Decision 7). awaiting_followup → running is admitted ONLY once a
// `follow_up` steering input has been CONSUMED, so a delayed/duplicate pre-park
// `running` report cannot spuriously un-park an idle task and re-arm the wall clock.
// This mirrors the awaiting_input answer guard.
//
// Non-vacuity / MUTATION PROOF: the "no follow_up ⇒ refused" case (rows==0) fails the
// instant someone "simply adds awaiting_followup to a permissive path" — because
// awaiting_followup is NOT in SetRunRunning's terminal NOT-IN list, dropping the guard
// clause lets the resume through (rows==1). The consumed-follow_up case (rows==1) fails
// if the guard is too strict.
func TestSetRunRunningAwaitingFollowupWakeGuardLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	f, done := setupFollowup(ctx, t, dsn)
	defer done()

	wkr := f.seedWorker(ctx, t, "online", pgtype.Int4{}, false)
	// Seed running, then park via the new writer (exercises SetRunAwaitingFollowup too).
	run := f.seedInteractiveTaskRun(ctx, t, "running", &wkr)
	if rows, err := f.q.SetRunAwaitingFollowup(ctx, store.SetRunAwaitingFollowupParams{
		ID: run, WorkerID: pgU(wkr),
	}); err != nil || rows != 1 {
		t.Fatalf("SetRunAwaitingFollowup: rows=%d err=%v", rows, err)
	}
	if got := f.status(ctx, t, run); got != "awaiting_followup" {
		t.Fatalf("park did not land: status = %q", got)
	}

	resume := func(worker uuid.UUID) int64 {
		t.Helper()
		rows, err := f.q.SetRunRunning(ctx, store.SetRunRunningParams{
			ID: run, WorkerID: pgU(worker), IterationCount: 1,
		})
		if err != nil {
			t.Fatalf("SetRunRunning: %v", err)
		}
		return rows
	}

	// (a) Parked, NO follow_up at all ⇒ a stale pre-park `running` report is refused.
	if rows := resume(wkr); rows != 0 {
		t.Fatalf("a parked run resumed with no follow_up (%d rows) — a delayed pre-park report un-parked "+
			"an idle task (Decision 7 guard missing, or awaiting_followup added to a permissive path)", rows)
	}

	// (b) An UNCONSUMED follow_up is not enough: the worker must have taken delivery.
	if _, err := f.q.CreateRunInput(ctx, store.CreateRunInputParams{
		RunID: run, Kind: "follow_up", Body: pgT(`{"body":"do the next thing"}`),
	}); err != nil {
		t.Fatalf("CreateRunInput(follow_up): %v", err)
	}
	if rows := resume(wkr); rows != 0 {
		t.Fatalf("an UNCONSUMED follow_up satisfied the wake guard (%d rows) — consumed_at is not being checked", rows)
	}

	// (c) A FOREIGN worker cannot wake the park even once the follow_up is consumed:
	//     the wake is tied to the in-process worker on the current claim.
	if _, err := f.q.ConsumeRunInputs(ctx, run); err != nil {
		t.Fatalf("ConsumeRunInputs: %v", err)
	}
	if rows := resume(uuid.New()); rows != 0 {
		t.Fatalf("a FOREIGN worker un-parked the run (%d rows) — the worker_id claim tie is not holding", rows)
	}

	// (d) The in-process worker WITH a consumed follow_up resumes ⇒ running.
	if rows := resume(wkr); rows != 1 {
		t.Fatalf("the in-process worker with a consumed follow_up did NOT resume the park (%d rows)", rows)
	}
	if got := f.status(ctx, t, run); got != "running" {
		t.Fatalf("status = %q after wake, want running", got)
	}
}

// The park WRITER itself (SetRunAwaitingFollowup): status write, the load-bearing health
// clear (what makes leaving awaiting_followup out of ListActiveRunsForHealth safe, exactly
// as on awaiting_input), and the ownership + terminality guards. sqlc's type deduction is
// not Postgres's, so this executes the statement to prove it prepares and runs.
func TestSetRunAwaitingFollowupLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	f, done := setupFollowup(ctx, t, dsn)
	defer done()

	wkr := f.seedWorker(ctx, t, "online", pgtype.Int4{}, false)
	run := f.seedInteractiveTaskRun(ctx, t, "running", &wkr)

	// Flag the run unhealthy first, so the clear below is observable rather than vacuous.
	if _, err := f.q.SetRunHealth(ctx, store.SetRunHealthParams{
		ID: run, Health: "stalled", HealthReason: pgT("no activity"),
	}); err != nil {
		t.Fatalf("SetRunHealth: %v", err)
	}

	rows, err := f.q.SetRunAwaitingFollowup(ctx, store.SetRunAwaitingFollowupParams{
		ID: run, WorkerID: pgU(wkr),
	})
	if err != nil {
		t.Fatalf("SetRunAwaitingFollowup: %v", err)
	}
	if rows != 1 {
		t.Fatalf("SetRunAwaitingFollowup affected %d rows, want 1", rows)
	}
	got, err := f.q.GetRunByID(ctx, run)
	if err != nil {
		t.Fatalf("GetRunByID: %v", err)
	}
	if got.Status != "awaiting_followup" {
		t.Fatalf("status = %q, want awaiting_followup", got.Status)
	}
	if got.Health != "ok" || got.HealthReason.Valid || got.HealthSince.Valid {
		t.Fatalf("health not cleared on entry: health=%q reason=%v since=%v — the ListActiveRunsForHealth "+
			"omission depends on this", got.Health, got.HealthReason, got.HealthSince)
	}

	// Foreign worker cannot park it.
	other := f.seedInteractiveTaskRun(ctx, t, "running", &wkr)
	if rows, err := f.q.SetRunAwaitingFollowup(ctx, store.SetRunAwaitingFollowupParams{
		ID: other, WorkerID: pgU(uuid.New()),
	}); err != nil {
		t.Fatalf("SetRunAwaitingFollowup(foreign): %v", err)
	} else if rows != 0 {
		t.Fatalf("a foreign worker parked the run (%d rows) — the worker_id guard is not holding", rows)
	}

	// A terminal run cannot be parked (status NOT IN terminal).
	if _, err := f.q.MarkRunFailedByID(ctx, store.MarkRunFailedByIDParams{
		ID: other, FailureReason: pgT("done"),
	}); err != nil {
		t.Fatalf("MarkRunFailedByID: %v", err)
	}
	if rows, err := f.q.SetRunAwaitingFollowup(ctx, store.SetRunAwaitingFollowupParams{
		ID: other, WorkerID: pgU(wkr),
	}); err != nil {
		t.Fatalf("SetRunAwaitingFollowup(terminal): %v", err)
	} else if rows != 0 {
		t.Fatalf("a terminal run was parked (%d rows) — the terminal guard is not holding", rows)
	}
}
