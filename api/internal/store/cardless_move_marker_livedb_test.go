package store_test

import (
	"context"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestCardlessRunMoveMarkerLiveDB is the regression gate for issue #1482. Terminal run
// writes used to stamp move_pending_since unconditionally, so a run with no board card
// (a judge run: NULL repo_id and issue_iid; a prompt run: a repo but no issue) sat in the
// reconcile loop for the whole retry window, then reappeared in the give-up warning as a
// "manual heal" that cannot exist. Only a card-bearing run may carry the marker.
//
// It drives the real terminal writers the observed runs took (SetRunCompleted for the
// worker-reported judge completion, CancelRunServerSide for a server cancel) and asserts on
// content: the card-less run ids are absent from ListPendingColumnMoves and
// ListGaveUpColumnMoves once their cutoffs pass. Positive control: an ordinary issue run in
// the same fixture, finished the same way, is present in both, so an empty result cannot
// read green.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; e2e/run-store-it.sh
// provides one. A package that prints `ok` with PASS=0 is INVALID, not green.
func TestCardlessRunMoveMarkerLiveDB(t *testing.T) {
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
	// t.Cleanup, not defer: the filler cleanup below needs the pool, and cleanups run after
	// deferred calls, last registered first.
	t.Cleanup(pool.Close)
	q := store.New(pool)

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	userID, connID, repoID, wkr := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, userID, fmt.Sprintf("cml-%s@e2e", userID))
	exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'github', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, 1, 'g/cml', 'https://forge.e2e/g/cml', 'main', true)`, repoID, connID)
	// token_hash carries the worker UUID bytes so a re-run never collides on workers_token_hash_key.
	exec(`INSERT INTO workers (id, user_id, name, token_hash) VALUES ($1, $2, 'w', $3)`, wkr, userID, wkr[:])

	// Two card-bearing issue runs (positive controls) and two card-less runs, all running on wkr.
	issueDone, issueCancelled, judgeRun, promptRun := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, worker_id)
	      VALUES ($1, $2, $3, 'issue', 11, 't', 'd', 'running', $4)`, issueDone, userID, repoID, wkr)
	exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, worker_id)
	      VALUES ($1, $2, $3, 'issue', 12, 't', 'd', 'running', $4)`, issueCancelled, userID, repoID, wkr)
	exec(`INSERT INTO runs (id, user_id, kind, target_run_id, issue_title, issue_description, status, worker_id)
	      VALUES ($1, $2, 'judge', $3, 't', 'd', 'running', $4)`, judgeRun, userID, issueDone, wkr)
	exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_title, issue_description, status, worker_id)
	      VALUES ($1, $2, $3, 'prompt', 't', 'd', 'running', $4)`, promptRun, userID, repoID, wkr)

	complete := func(id uuid.UUID) {
		t.Helper()
		n, err := q.SetRunCompleted(ctx, store.SetRunCompletedParams{ID: id, WorkerID: pgtype.UUID{Bytes: wkr, Valid: true}})
		if err != nil || n != 1 {
			t.Fatalf("SetRunCompleted(%s) = %d, %v; want 1 row", id, n, err)
		}
	}
	cancel := func(id uuid.UUID) {
		t.Helper()
		n, err := q.CancelRunServerSide(ctx, store.CancelRunServerSideParams{ID: id, UserID: userID})
		if err != nil || n != 1 {
			t.Fatalf("CancelRunServerSide(%s) = %d, %v; want 1 row", id, n, err)
		}
	}
	// Order matters: both card-less writes sit between the two positive controls, so a
	// regressed stamp on either would fall inside the give-up window bounded below.
	complete(issueDone)
	complete(judgeRun)
	cancel(promptRun)
	cancel(issueCancelled)

	// The marker itself, read straight off the rows.
	for _, tc := range []struct {
		name string
		id   uuid.UUID
		want bool
	}{
		{"completed issue run", issueDone, true},
		{"cancelled issue run", issueCancelled, true},
		{"completed judge run", judgeRun, false},
		{"cancelled prompt run", promptRun, false},
	} {
		var stamped bool
		if err := pool.QueryRow(ctx, `SELECT move_pending_since IS NOT NULL FROM runs WHERE id = $1`, tc.id).Scan(&stamped); err != nil {
			t.Fatalf("%s: read marker: %v", tc.name, err)
		}
		if stamped != tc.want {
			t.Errorf("%s: move_pending_since set = %v, want %v", tc.name, stamped, tc.want)
		}
	}

	// Crowding (issue #1966): ListGaveUpColumnMoves is ORDER BY move_pending_since LIMIT 100
	// with no user or repo scope, and the serial live-DB suite leaves other tests' stamped rows
	// behind. 100 card-bearing filler runs stamped earlier than the fixtures reproduce that, so a
	// window not bounded by the fixtures' own stamps loses the positive control past the LIMIT.
	fillerUser, fillerConn, fillerRepo := uuid.New(), uuid.New(), uuid.New()
	t.Cleanup(func() {
		// users cascades to forge_connections, repos and runs.
		if _, err := pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, fillerUser); err != nil {
			t.Errorf("cleanup filler user: %v", err)
		}
	})
	exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, fillerUser, fmt.Sprintf("cml-fill-%s@e2e", fillerUser))
	exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'github', 'https://forge.e2e', 'bot', 1, $3)`, fillerConn, fillerUser, []byte{0x1})
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, 1, 'g/cml-fill', 'https://forge.e2e/g/cml-fill', 'main', true)`, fillerRepo, fillerConn)
	exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, move_pending_since)
	      SELECT gen_random_uuid(), $1, $2, 'issue', g, 't', 'd', 'failed', now() - interval '30 minutes'
	      FROM generate_series(1, 100) AS g`, fillerUser, fillerRepo)

	// Reconcile window: grace already passed, give-up not yet reached. The shared DB may
	// hold other packages' rows, so assert membership by id with a large page.
	now := time.Now()
	pending, err := q.ListPendingColumnMoves(ctx, store.ListPendingColumnMovesParams{
		GraceCutoff:  pgtype.Timestamptz{Time: now.Add(time.Minute), Valid: true},
		GiveupCutoff: pgtype.Timestamptz{Time: now.Add(-time.Hour), Valid: true},
		MaxBatch:     10000,
	})
	if err != nil {
		t.Fatalf("ListPendingColumnMoves: %v", err)
	}
	// Give-up window: the markers crossed the give-up boundary during the last interval.
	// The query keeps only the oldest 100 rows in the window, unscoped, so bound the window
	// by the positive controls' own stamps (> prior, <= giveup). The serial suite stamped
	// every leftover row before these fixtures, so they fall outside it, and the positive
	// control proves the window still returns rows, and both card-less writes were stamped
	// between the controls, keeping their absence checks meaningful.
	var stamps []time.Time
	for _, id := range []uuid.UUID{issueDone, issueCancelled} {
		var ts time.Time
		if err := pool.QueryRow(ctx, `SELECT move_pending_since FROM runs WHERE id = $1`, id).Scan(&ts); err != nil {
			t.Fatalf("read move_pending_since of %s: %v", id, err)
		}
		stamps = append(stamps, ts)
	}
	gaveUpRows, err := q.ListGaveUpColumnMoves(ctx, store.ListGaveUpColumnMovesParams{
		GiveupCutoff: pgtype.Timestamptz{Time: slices.MaxFunc(stamps, time.Time.Compare), Valid: true},
		PriorCutoff:  pgtype.Timestamptz{Time: slices.MinFunc(stamps, time.Time.Compare).Add(-time.Microsecond), Valid: true},
	})
	if err != nil {
		t.Fatalf("ListGaveUpColumnMoves: %v", err)
	}
	gaveUp := make([]uuid.UUID, 0, len(gaveUpRows))
	for _, r := range gaveUpRows {
		gaveUp = append(gaveUp, r.ID)
	}

	for _, id := range []uuid.UUID{issueDone, issueCancelled} {
		if !slices.Contains(pending, id) {
			t.Errorf("issue run %s missing from ListPendingColumnMoves (positive control)", id)
		}
		if !slices.Contains(gaveUp, id) {
			t.Errorf("issue run %s missing from ListGaveUpColumnMoves (positive control)", id)
		}
	}
	for _, id := range []uuid.UUID{judgeRun, promptRun} {
		if slices.Contains(pending, id) {
			t.Errorf("card-less run %s is in ListPendingColumnMoves; the reconcile loop can never clear it", id)
		}
		if slices.Contains(gaveUp, id) {
			t.Errorf("card-less run %s is in ListGaveUpColumnMoves; it logs a false manual-heal warning", id)
		}
	}
}
