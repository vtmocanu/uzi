package store_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// TestCountRunReviseInputsCountsConsumedRowsLiveDB nails down the load-bearing invariant of
// the PRD #41 plan-revision cap: CountRunReviseInputs — the count(*) the server checks
// against PLAN_MAX_REVISIONS — has NO consumed_at filter, so a revise that has already
// been delivered to the worker (consumed_at stamped) STILL counts toward the cap.
//
// A fakeStore + SQL inspection can't prove this: only the real query, run against a real
// Postgres after a real ConsumeRunInputs, shows that consumption/requeue does not
// decrement the count and thus cannot be used to defeat the cap (PRD #41 Decision 6,
// Success Criterion 5: "including after requeue (consumed rows still count)").
//
// It also checks the kind filter: follow_up / approve_plan rows never inflate the count.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; the store-IT
// runner (e2e/run-store-it.sh) provides one.
func TestCountRunReviseInputsCountsConsumedRowsLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	defer pool.Close()
	q := store.New(pool)

	userID, connID, repoID, runID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("revisecap-%s@e2e", userID))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, repoID, connID)
	mustExec(ctx, t, pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status)
		 VALUES ($1, $2, $3, 1, 't', 'd', 'running')`, runID, userID, repoID)

	const n = 3

	// Enqueue N revise_plan inputs via the same insert path the server uses.
	for i := 0; i < n; i++ {
		if _, err := q.CreateRunInput(ctx, store.CreateRunInputParams{
			RunID: runID,
			Kind:  "revise_plan",
			Body:  pgtype.Text{String: fmt.Sprintf("revision %d", i), Valid: true},
		}); err != nil {
			t.Fatalf("CreateRunInput(revise_plan %d): %v", i, err)
		}
	}

	// Non-revise steering inputs must NOT inflate the revise count (kind filter).
	if _, err := q.CreateRunInput(ctx, store.CreateRunInputParams{
		RunID: runID, Kind: "follow_up", Body: pgtype.Text{String: "steer", Valid: true},
	}); err != nil {
		t.Fatalf("CreateRunInput(follow_up): %v", err)
	}
	if _, err := q.CreateRunInput(ctx, store.CreateRunInputParams{
		RunID: runID, Kind: "approve_plan", Body: pgtype.Text{},
	}); err != nil {
		t.Fatalf("CreateRunInput(approve_plan): %v", err)
	}

	// Before consumption: exactly N revises count (follow_up/approve_plan ignored).
	before, err := q.CountRunReviseInputs(ctx, runID)
	if err != nil {
		t.Fatalf("CountRunReviseInputs (before consume): %v", err)
	}
	if before != n {
		t.Fatalf("CountRunReviseInputs before consume = %d, want %d (follow_up/approve_plan must not count)", before, n)
	}

	// Deliver every pending input to the worker — this stamps consumed_at on all of them,
	// exactly as a poll/requeue would.
	consumed, err := q.ConsumeRunInputs(ctx, runID)
	if err != nil {
		t.Fatalf("ConsumeRunInputs: %v", err)
	}
	reviseConsumed := 0
	for _, c := range consumed {
		if c.Kind == "revise_plan" {
			reviseConsumed++
		}
	}
	if reviseConsumed != n {
		t.Fatalf("ConsumeRunInputs stamped %d revise_plan rows, want %d (all revises must be marked consumed)", reviseConsumed, n)
	}

	// After consumption: the count is UNCHANGED. A consumed revise still counts, so the
	// cap is the lifetime number of revisions and cannot be reset by consuming/requeuing.
	after, err := q.CountRunReviseInputs(ctx, runID)
	if err != nil {
		t.Fatalf("CountRunReviseInputs (after consume): %v", err)
	}
	if after != n {
		t.Fatalf("CountRunReviseInputs after consume = %d, want %d — consumption must NOT decrement the cap count", after, n)
	}
}

// TestCreateRunReviseInputIfUnderCapAtomicLiveDB pins the PRD #41 cap ENFORCEMENT (not just the
// reporting count): CreateRunReviseInputIfUnderCap inserts only while the run is under the
// cap, and two concurrent submits racing at the boundary MUST never both persist an
// over-cap row. That is the invariant. This test asserts it.
//
// THEY COULD, until issue #106 was fixed on 2026-07-29. The statement took the run row FOR
// UPDATE but counted run_user_inputs, a DIFFERENT table, at the statement snapshot, so a
// caller that blocked on the lock still counted without the winner's row: the lock did not
// cover the count at all. The cap predicate now lives in the UPDATE's own WHERE, against
// runs.revise_count — a column of the locked row, which EvalPlanQual does re-evaluate.
//
// 🔴 A GREEN HERE STILL DOES NOT VERIFY THE FIX, and that has not changed. This test HOPES
// for the interleave: the two goroutines find their own pooled connections and launch
// jitter is routinely larger than the statement's duration, so most trials never overlap
// at all. It was measured catching the defect about 1 run in 50 under load, meaning a
// 20-run green sweep from it had better than a two-in-three chance of being all-green
// against unconditionally broken code. That is exactly the evidence that first closed
// #106 as "went green on immediate re-run". TestReviseCapForcedInterleaveLiveDB
// (revise_cap_repro_test.go) confirms the interleave via pg_stat_activity before
// asserting, and IT is the gate that can tell a fix from luck.
//
// It is the same TOCTOU the old count-then-CreateRunInput path (two round trips) allowed,
// and which collapsing it into a single statement was meant to close: web + Slack
// submitting at N-1 could both read N-1 and both insert an N+1th row.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.
func TestCreateRunReviseInputIfUnderCapAtomicLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	defer pool.Close()
	q := store.New(pool)

	userID, connID, repoID, runID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("revisecapatomic-%s@e2e", userID))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, repoID, connID)
	mustExec(ctx, t, pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status)
		 VALUES ($1, $2, $3, 1, 't', 'd', 'awaiting_approval')`, runID, userID, repoID)

	const capLimit = 3

	mkParams := func(body string) store.CreateRunReviseInputIfUnderCapParams {
		return store.CreateRunReviseInputIfUnderCapParams{
			RunID: runID, Body: pgtype.Text{String: body, Valid: true}, MaxRevisions: capLimit,
		}
	}

	// Fill to cap-1 (2 revisions, one slot left).
	for i := 0; i < capLimit-1; i++ {
		if _, err := q.CreateRunReviseInputIfUnderCap(ctx, mkParams(fmt.Sprintf("fill %d", i))); err != nil {
			t.Fatalf("CreateRunReviseInputIfUnderCap(fill %d): %v", i, err)
		}
	}

	// Two concurrent submits with one slot left: exactly one must land. They race on
	// their own pooled connections, so the FOR UPDATE row lock is what serializes them.
	type outcome struct {
		landed bool
		err    error
	}
	results := make(chan outcome, 2)
	for i := 0; i < 2; i++ {
		go func(i int) {
			_, err := q.CreateRunReviseInputIfUnderCap(ctx, mkParams(fmt.Sprintf("race %d", i)))
			if errors.Is(err, pgx.ErrNoRows) {
				results <- outcome{landed: false}
				return
			}
			results <- outcome{landed: err == nil, err: err}
		}(i)
	}
	landed := 0
	for i := 0; i < 2; i++ {
		r := <-results
		if r.err != nil && !errors.Is(r.err, pgx.ErrNoRows) {
			t.Fatalf("concurrent revise submit errored: %v", r.err)
		}
		if r.landed {
			landed++
		}
	}
	if landed != 1 {
		t.Fatalf("concurrent submits at N-1 landed %d rows, want exactly 1 (the cap must serialize)", landed)
	}

	// The run now sits exactly at the cap; a further submit is refused with no row.
	final, err := q.CountRunReviseInputs(ctx, runID)
	if err != nil {
		t.Fatalf("CountRunReviseInputs (final): %v", err)
	}
	if final != capLimit {
		t.Fatalf("final revise count = %d, want %d (never over the cap)", final, capLimit)
	}
	// The counter that ENFORCES the cap must agree with the rows that report it. The
	// assertion above reads count(*); this one reads runs.revise_count, and only together
	// do they exclude a race that landed the row without bumping the counter (or vice
	// versa). TestReviseCountMatchesRowCountLiveDB covers the sequential ways they can
	// diverge; this covers the concurrent one.
	var counter int
	if err := pool.QueryRow(ctx, `SELECT revise_count FROM runs WHERE id = $1`, runID).Scan(&counter); err != nil {
		t.Fatalf("read runs.revise_count: %v", err)
	}
	if counter != capLimit {
		t.Fatalf("runs.revise_count = %d, want %d (the enforcing counter must match the %d persisted rows)",
			counter, capLimit, final)
	}
	if _, err := q.CreateRunReviseInputIfUnderCap(ctx, mkParams("over")); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("over-cap submit err = %v, want pgx.ErrNoRows", err)
	}
}

// TestReviseCountMatchesRowCountLiveDB pins the duplication that the issue #106 fix
// introduced. runs.revise_count (what the cap is ENFORCED against, since only a column of
// the locked row survives EvalPlanQual) and count(*) of revise_plan rows (what
// CountRunReviseInputs REPORTS) are two representations of one fact, and keeping them
// equal is a contract rather than an accident. One case per way they can drift apart.
//
// The load-bearing case is the REFUSED insert. Writing the cap predicate on the INSERT
// instead of the UPDATE is the single most likely miswrite of this query, and it leaks
// budget SILENTLY: the counter runs away while the rows sit at cap, so each rejected
// attempt quietly shrinks the run's remaining budget.
//
// It is caught HERE first and most legibly — this test names the divergence
// ("revise_count = 4, revise_plan rows = 3") where the others report a symptom.
// TestCreateRunReviseInputIfUnderCapAtomicLiveDB also reddens under that miswrite, and
// deterministically: its concurrent fan-out produces at least one refusal, each of which
// bumps the counter under that fold, so its runs.revise_count assertion fires. An earlier
// version of this paragraph said nothing else in the suite would see it, which understated
// the coverage — corrected after both were measured. The forced-interleave test and the
// sequential control DO stay green under it, so the miswrite is genuinely invisible to the
// concurrency gates.
//
// Every case asserts that the fixture actually REACHED the state it names — the refusal
// really returned pgx.ErrNoRows, the consume really stamped rows, the interleave really
// blocked — so a case that never happened cannot pass vacuously.
//
// CountRunReviseInputs deliberately still reads count(*) and is not repointed at the
// counter. Repointing it is the tempting move and the wrong one: this test would then
// assert that the counter equals itself, a vacuous green reading as full coverage. Its
// lack of a production call site is exactly what makes it a good auditor.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.
func TestReviseCountMatchesRowCountLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	defer pool.Close()
	q := store.New(pool)

	const capLimit = 3

	// agree reads BOTH representations and requires them to equal want. Reading the rows
	// through CountRunReviseInputs rather than a hand-written count is deliberate: it is
	// the query the server reports with, so a change to its filters is caught here too.
	agree := func(t *testing.T, runID uuid.UUID, want int, stage string) {
		t.Helper()
		var counter int
		if err := pool.QueryRow(ctx, `SELECT revise_count FROM runs WHERE id = $1`, runID).Scan(&counter); err != nil {
			t.Fatalf("%s: read runs.revise_count: %v", stage, err)
		}
		rows, err := q.CountRunReviseInputs(ctx, runID)
		if err != nil {
			t.Fatalf("%s: CountRunReviseInputs: %v", stage, err)
		}
		if counter != want || int(rows) != want {
			t.Fatalf("%s: runs.revise_count = %d, revise_plan rows = %d, want both = %d",
				stage, counter, rows, want)
		}
	}

	mkParams := func(runID uuid.UUID, body string) store.CreateRunReviseInputIfUnderCapParams {
		return store.CreateRunReviseInputIfUnderCapParams{
			RunID: runID, Body: pgtype.Text{String: body, Valid: true}, MaxRevisions: capLimit,
		}
	}

	// repro106SeedRun (revise_cap_repro_test.go, same package) builds the
	// user/connection/repo/run chain. Shared rather than copied so the two files cannot
	// drift into seeding different fixtures.
	runID := repro106SeedRun(ctx, t, pool, "diffcount")

	// 1. A fresh run: both representations are zero. The column's DEFAULT and the absence
	//    of rows have to agree before anything else means much.
	agree(t, runID, 0, "fresh run")

	// 2. After k accepted inserts, both are k — checked at every k, not only at the end,
	//    so a counter that bumps by two would be caught at the first step rather than
	//    hidden by the cap refusing the rest.
	for i := 0; i < capLimit; i++ {
		if _, err := q.CreateRunReviseInputIfUnderCap(ctx, mkParams(runID, fmt.Sprintf("revision %d", i))); err != nil {
			t.Fatalf("accepted insert %d: %v", i, err)
		}
		agree(t, runID, i+1, fmt.Sprintf("after %d accepted insert(s)", i+1))
	}

	// 3. THE CASE THIS TEST EXISTS FOR. A refused insert at cap must move NEITHER. If the
	//    cap predicate sits on the INSERT rather than the UPDATE, the UPDATE still bumps
	//    and the counter reaches 4 while the rows stay at 3.
	if _, err := q.CreateRunReviseInputIfUnderCap(ctx, mkParams(runID, "refused")); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("fixture never reached the refusal: over-cap submit err = %v, want pgx.ErrNoRows", err)
	}
	agree(t, runID, capLimit, "after a REFUSED insert at cap")

	// 4. Consumption stamps consumed_at on the rows; neither representation may move. The
	//    cap is a lifetime budget, so delivering a revision to the worker must not hand
	//    the user another one (PRD #41 Decision 6).
	consumed, err := q.ConsumeRunInputs(ctx, runID)
	if err != nil {
		t.Fatalf("ConsumeRunInputs: %v", err)
	}
	reviseConsumed := 0
	for _, c := range consumed {
		if c.Kind == "revise_plan" {
			reviseConsumed++
		}
	}
	if reviseConsumed == 0 {
		t.Fatalf("fixture never reached the consumed state: ConsumeRunInputs stamped 0 revise_plan rows")
	}
	agree(t, runID, capLimit, fmt.Sprintf("after ConsumeRunInputs stamped %d revise_plan row(s)", reviseConsumed))

	// 5. And after the concurrent interleave that #106 was about: a second caller parked
	//    on the winner's row lock must leave both at exactly the cap. Same forcing device
	//    as TestReviseCapForcedInterleaveLiveDB — that test asserts the cap is not
	//    breached, this one asserts the two representations survive the breach attempt
	//    together.
	raceID := repro106SeedRun(ctx, t, pool, "diffcount-race")
	for i := 0; i < capLimit-1; i++ {
		if _, err := q.CreateRunReviseInputIfUnderCap(ctx, mkParams(raceID, fmt.Sprintf("fill %d", i))); err != nil {
			t.Fatalf("race prefill %d: %v", i, err)
		}
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx) //nolint // no-op after Commit
	if _, err := q.WithTx(tx).CreateRunReviseInputIfUnderCap(ctx, mkParams(raceID, "A")); err != nil {
		t.Fatalf("A's submit should have landed (it was at cap-1): %v", err)
	}
	bCh := make(chan error, 1)
	go func() {
		_, err := q.CreateRunReviseInputIfUnderCap(ctx, mkParams(raceID, "B"))
		bCh <- err
	}()
	if !repro106WaitBlockedOnLock(ctx, t, pool) {
		t.Fatalf("B never blocked on a lock within 15s — interleave NOT established, this run is INVALID (not a red)")
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit A: %v", err)
	}
	if err := <-bCh; !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("blocked caller err = %v, want pgx.ErrNoRows (it must be refused at the cap)", err)
	}
	agree(t, raceID, capLimit, "after the forced concurrent interleave at cap-1")
}
