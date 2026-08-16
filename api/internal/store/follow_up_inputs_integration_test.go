package store_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestListFollowUpInputsForRunLiveDB exercises the PRD #95 steer-queue query against a
// REAL Postgres — the property a fake store cannot show, since it never runs the SQL.
// It proves the three query guarantees the web/CLI queue rests on:
//   - kind='follow_up' ONLY (approve_plan / cancel / reject_plan rows are excluded), so
//     a mixed steering log never leaks non-follow_up inputs into the queue;
//   - newest-first (id DESC), the order the queue renders in;
//   - UNCAPPED — every follow_up comes back, so the newest are never dropped behind a
//     cap the way the judge's oldest-first ListRunInputsForRun would (Decision 4);
//   - consumed_at rides through NULL vs set, the Queued/Delivered signal.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; the store-IT
// runner (e2e/run-store-it.sh) provides one.
func TestListFollowUpInputsForRunLiveDB(t *testing.T) {
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
		userID, fmt.Sprintf("followup-%s@e2e", userID))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, repoID, connID)
	mustExec(ctx, t, pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status)
		 VALUES ($1, $2, $3, 1, 't', 'd', 'running')`, runID, userID, repoID)

	// Interleave follow_ups with non-follow_up steering inputs, in insert order — so id
	// ascends with insertion. Two follow_ups, one of them consumed (Delivered).
	mustFollowUp := func(body string, consumed bool) {
		id := int64(0)
		if err := pool.QueryRow(ctx,
			`INSERT INTO run_user_inputs (run_id, kind, body) VALUES ($1, 'follow_up', $2) RETURNING id`,
			runID, body).Scan(&id); err != nil {
			t.Fatalf("insert follow_up: %v", err)
		}
		if consumed {
			mustExec(ctx, t, pool, `UPDATE run_user_inputs SET consumed_at = now() WHERE id = $1`, id)
		}
	}
	mustFollowUp("focus on the api first", true) // oldest, delivered
	mustExec(ctx, t, pool, `INSERT INTO run_user_inputs (run_id, kind, body) VALUES ($1, 'approve_plan', '')`, runID)
	mustExec(ctx, t, pool, `INSERT INTO run_user_inputs (run_id, kind, body) VALUES ($1, 'cancel', NULL)`, runID)
	mustFollowUp("also update the changelog", false) // newest, queued

	// A second run's follow_up must never bleed into this run's queue.
	otherRun := uuid.New()
	mustExec(ctx, t, pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status)
		 VALUES ($1, $2, $3, 2, 't', 'd', 'running')`, otherRun, userID, repoID)
	mustExec(ctx, t, pool, `INSERT INTO run_user_inputs (run_id, kind, body) VALUES ($1, 'follow_up', 'other run')`, otherRun)

	rows, err := q.ListFollowUpInputsForRun(ctx, runID)
	if err != nil {
		t.Fatalf("ListFollowUpInputsForRun: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want exactly 2 follow_ups (approve_plan/cancel/other-run excluded)", len(rows))
	}
	for _, r := range rows {
		if r.Kind != "follow_up" {
			t.Fatalf("non-follow_up kind %q leaked into the queue", r.Kind)
		}
		if r.RunID != runID {
			t.Fatalf("row from run %s leaked into run %s's queue", r.RunID, runID)
		}
	}
	// Newest-first: the queued (later-inserted, higher id) row leads, then the delivered.
	if rows[0].Body.String != "also update the changelog" || rows[0].ConsumedAt.Valid {
		t.Errorf("rows[0] = %+v, want the newest, still Queued (consumed_at NULL)", rows[0])
	}
	if rows[1].Body.String != "focus on the api first" || !rows[1].ConsumedAt.Valid {
		t.Errorf("rows[1] = %+v, want the older, Delivered (consumed_at set)", rows[1])
	}
	if rows[0].ID <= rows[1].ID {
		t.Errorf("rows must be newest-first by id: got ids %d then %d", rows[0].ID, rows[1].ID)
	}
}
