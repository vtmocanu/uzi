package store_test

// PRD #517 M1 — the parts of interactive, long-lived task runs that only a real Postgres
// can answer: whether migration 00144's four CHECK/column changes actually took, and
// whether runs.interactive round-trips create → row.
//
// (a) The widened CHECKs must ACCEPT the new values and still accept every pre-existing
//     one — a DROP+ADD that silently forgot a value would reject a status/kind that used
//     to be legal, so re-asserting the old values is what makes the widening honest.
// (b) interactive set at create must read back true on the run row (the "silently false"
//     trap: a bool omitted from the params literal builds green but persists false).
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; run via
// e2e/run-store-it.sh. A package that prints `ok` with PASS=0 is INVALID, not green.

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestInteractiveTaskRunConstraintsLiveDB(t *testing.T) {
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

	userID, connID, repoID := uuid.New(), uuid.New(), uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("interactive-%s@e2e", userID))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, repoID, connID)

	// ── (b) create → row: interactive=true persists and reads back true. ──
	id := uuid.New()
	branch := "uzi/task/" + id.String()
	run, err := q.CreateTaskRun(ctx, store.CreateTaskRunParams{
		RunID:            id,
		UserID:           userID,
		RepoID:           repoID,
		Branch:           pgtype.Text{String: branch, Valid: true},
		OpenMr:           false,
		Interactive:      true,
		IssueTitle:       "interactive handoff",
		IssueDescription: "iterate with me",
	})
	if err != nil {
		t.Fatalf("CreateTaskRun (interactive): %v", err)
	}
	if !run.Interactive {
		t.Error("interactive = false on the RETURNING row, want true (the column or the params binding is not wired)")
	}
	// Read it back from the row independently — the returned struct and the persisted
	// column must agree.
	got, err := q.GetRunByID(ctx, id)
	if err != nil {
		t.Fatalf("GetRunByID: %v", err)
	}
	if !got.Interactive {
		t.Error("interactive did not persist: GetRunByID read false, want true")
	}

	// A plain (non-interactive) task defaults interactive=false — the DEFAULT false half
	// of the column, and the "did we accidentally hard-code true" backstop.
	plainID := uuid.New()
	plain, err := q.CreateTaskRun(ctx, store.CreateTaskRunParams{
		RunID:            plainID,
		UserID:           userID,
		RepoID:           repoID,
		Branch:           pgtype.Text{String: "uzi/task/" + plainID.String(), Valid: true},
		Interactive:      false,
		IssueTitle:       "plain",
		IssueDescription: "one-shot",
	})
	if err != nil {
		t.Fatalf("CreateTaskRun (plain): %v", err)
	}
	if plain.Interactive {
		t.Error("interactive = true for a plain handoff, want false by default")
	}

	// ── (a) runs_status_check accepts 'awaiting_followup' AND every pre-existing value. ──
	statuses := []string{
		"queued", "claimed", "running", "awaiting_approval", "awaiting_input",
		"limit_wait", "completed", "failed", "cancelled", "awaiting_followup",
	}
	for _, s := range statuses {
		if _, err := pool.Exec(ctx, `UPDATE runs SET status = $1 WHERE id = $2`, s, id); err != nil {
			t.Errorf("runs_status_check rejected status=%q, want accepted (widening dropped a value?): %v", s, err)
		}
	}
	// And a bogus status is still rejected — the CHECK is not vacuous.
	if _, err := pool.Exec(ctx, `UPDATE runs SET status = 'not_a_status' WHERE id = $1`, id); err == nil {
		t.Error("runs_status_check ACCEPTED a bogus status: the CHECK is vacuous")
	}

	// ── (a) runs_stop_kind_check accepts 'stopped' AND every pre-existing value. ──
	// stop_kind is display-only, so it can be set independently of status.
	for _, k := range []string{"cancelled", "plan_rejected", "auto_stopped", "stopped"} {
		if _, err := pool.Exec(ctx, `UPDATE runs SET stop_kind = $1 WHERE id = $2`, k, id); err != nil {
			t.Errorf("runs_stop_kind_check rejected stop_kind=%q, want accepted: %v", k, err)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE runs SET stop_kind = 'bogus' WHERE id = $1`, id); err == nil {
		t.Error("runs_stop_kind_check ACCEPTED a bogus stop_kind: the CHECK is vacuous")
	}

	// ── (a) run_user_inputs_kind_check accepts 'stop' AND every pre-existing value. ──
	for _, k := range []string{"follow_up", "approve_plan", "reject_plan", "cancel", "revise_plan", "answer", "stop"} {
		if _, err := pool.Exec(ctx, `INSERT INTO run_user_inputs (run_id, kind) VALUES ($1, $2)`, id, k); err != nil {
			t.Errorf("run_user_inputs_kind_check rejected kind=%q, want accepted: %v", k, err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO run_user_inputs (run_id, kind) VALUES ($1, 'bogus')`, id); err == nil {
		t.Error("run_user_inputs_kind_check ACCEPTED a bogus kind: the CHECK is vacuous")
	}
}
