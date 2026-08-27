package store_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestClearRunMilestonesCompletedLiveDB pins PRD #628 M4's targeted clear against a REAL
// Postgres. milestones_completed is otherwise a monotone UNION (SetRunRunning /
// SetRunCompleted, jsonb_agg(DISTINCT existing || reported)) so a worker cannot walk it
// back by reporting []; ClearRunMilestonesCompleted is the ONLY writer that resets it to
// empty, fired on a cross-worker re-claim that reseeded from the DEFAULT branch (no
// committed work recovered). This exercises the reset end to end (clear, then the union
// refills from EMPTY) plus both SQL guards (ownership, terminal status) — none of which
// live in Go, so a fake store cannot cover them.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (the store IT
// runner provides one).
func TestClearRunMilestonesCompletedLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via the store integration runner for live-DB coverage")
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

	// Budget config a real SetState/approve passes; irrelevant to the clear, but
	// SetRunRunning's freeze CASE reads it, so supply the same testParams defaults.
	const (
		runMaxIter  = 5
		runTimeout  = 7200
		budgetCap   = 12
		wallCeiling = 28800
	)

	userID, connID, repoID := uuid.New(), uuid.New(), uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("mclear-%s@e2e", userID))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, repoID, connID)

	owner, err := q.CreateWorker(ctx, store.CreateWorkerParams{
		UserID: userID, Name: "owner", TokenHash: append([]byte("mclear-own-"), userID[:]...),
		AnthropicBindMode: "default",
	})
	if err != nil {
		t.Fatalf("CreateWorker(owner): %v", err)
	}
	ownerID := pgtype.UUID{Bytes: owner.ID, Valid: true}
	other, err := q.CreateWorker(ctx, store.CreateWorkerParams{
		UserID: userID, Name: "other", TokenHash: append([]byte("mclear-oth-"), userID[:]...),
		AnthropicBindMode: "default",
	})
	if err != nil {
		t.Fatalf("CreateWorker(other): %v", err)
	}
	otherID := pgtype.UUID{Bytes: other.ID, Valid: true}

	nextIID := int64(1)
	newRun := func(status string) uuid.UUID {
		id := uuid.New()
		mustExec(ctx, t, pool,
			`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, worker_id, kind)
			 VALUES ($1, $2, $3, $4, 't', 'd', $5, $6, 'issue')`, id, userID, repoID, nextIID, status, owner.ID)
		nextIID++
		return id
	}

	// decodeIDs reads a jsonb id-array column as a sorted []string. Unlike the sibling
	// test's readIDs it distinguishes SQL NULL (nil) from an empty jsonb array `[]`
	// (non-nil, len 0) — the exact state the clear produces.
	decodeIDs := func(id uuid.UUID, col string) []string {
		t.Helper()
		var raw []byte
		if err := pool.QueryRow(ctx, fmt.Sprintf(`SELECT %s FROM runs WHERE id = $1`, col), id).Scan(&raw); err != nil {
			t.Fatalf("read %s of %s: %v", col, id, err)
		}
		if raw == nil {
			return nil
		}
		var ids []string
		if err := json.Unmarshal(raw, &ids); err != nil {
			t.Fatalf("decode %s of %s: %v (%s)", col, id, err, raw)
		}
		sort.Strings(ids)
		return ids
	}

	setRunning := func(run uuid.UUID, completed []byte) {
		t.Helper()
		if _, err := q.SetRunRunning(ctx, store.SetRunRunningParams{
			ID: run, WorkerID: ownerID,
			MilestonesCompleted: completed,
			RunMaxIterations:    runMaxIter, RunTimeoutSeconds: runTimeout,
			MilestoneBudgetCap: budgetCap, BudgetWallCeilingSeconds: wallCeiling,
		}); err != nil {
			t.Fatalf("SetRunRunning(%s): %v", completed, err)
		}
	}

	// ── (1)+(2): the end-to-end reset. The union builds {m0,m1}; the clear empties it;
	//    a later union of {m0} then yields {m0} — proving the union REFILLS FROM EMPTY,
	//    not from the stale set (a union that had kept {m0,m1} would read {m0,m1}). ──
	t.Run("clear empties the set and the union refills from empty", func(t *testing.T) {
		run := newRun("running")
		setRunning(run, []byte(`["m0","m1"]`))
		if got := decodeIDs(run, "milestones_completed"); !eqIDs(got, []string{"m0", "m1"}) {
			t.Fatalf("precondition: completed = %v, want {m0,m1}", got)
		}

		rows, err := q.ClearRunMilestonesCompleted(ctx, store.ClearRunMilestonesCompletedParams{
			ID: run, WorkerID: ownerID,
		})
		if err != nil {
			t.Fatalf("ClearRunMilestonesCompleted: %v", err)
		}
		if rows != 1 {
			t.Fatalf("clear by the owner must affect exactly 1 row, got %d", rows)
		}
		if got := decodeIDs(run, "milestones_completed"); got == nil || len(got) != 0 {
			t.Fatalf("completed after clear = %#v, want a non-NULL empty [] (RUN>0)", got)
		}

		// The refill: a union of {m0} onto the now-empty column yields exactly {m0}.
		setRunning(run, []byte(`["m0"]`))
		if got := decodeIDs(run, "milestones_completed"); !eqIDs(got, []string{"m0"}) {
			t.Fatalf("completed after refill = %v, want {m0} (the union refilled from EMPTY, not the stale {m0,m1})", got)
		}
	})

	// ── (3): the ownership guard. A clear from a worker that does NOT own the run affects
	//    0 rows and leaves the live owner's progress intact — a superseded/zombie worker
	//    cannot wipe it. ──
	t.Run("a non-owning worker cannot clear", func(t *testing.T) {
		run := newRun("running")
		setRunning(run, []byte(`["m0","m1"]`))

		rows, err := q.ClearRunMilestonesCompleted(ctx, store.ClearRunMilestonesCompletedParams{
			ID: run, WorkerID: otherID,
		})
		if err != nil {
			t.Fatalf("ClearRunMilestonesCompleted(non-owner): %v", err)
		}
		if rows != 0 {
			t.Fatalf("a non-owning clear must affect 0 rows, got %d", rows)
		}
		if got := decodeIDs(run, "milestones_completed"); !eqIDs(got, []string{"m0", "m1"}) {
			t.Fatalf("the owner's progress must be intact after a non-owner clear = %v, want {m0,m1}", got)
		}
	})

	// ── (4): the status guard is an ALLOWLIST of the active-claim states (claimed/running).
	//    Every status OUTSIDE it — a TERMINAL run (completed), and — the tightening PRD #628
	//    M4's review added — a run PARKED in limit_wait or awaiting_approval — affects 0 rows
	//    and leaves the column untouched. The parked cases matter beyond the terminal one:
	//    SetRunRunning refuses to refill a parked run, so a clear there would empty the list
	//    WITHOUT the paired refill (emptying-without-refill). The allowlist forbids it. ──
	for _, status := range []string{"completed", "limit_wait", "awaiting_approval"} {
		status := status
		t.Run("a "+status+" run cannot be cleared (allowlist guard)", func(t *testing.T) {
			run := newRun(status)
			mustExec(ctx, t, pool,
				`UPDATE runs SET milestones_completed = '["m0","m1"]'::jsonb WHERE id = $1`, run)

			rows, err := q.ClearRunMilestonesCompleted(ctx, store.ClearRunMilestonesCompletedParams{
				ID: run, WorkerID: ownerID,
			})
			if err != nil {
				t.Fatalf("ClearRunMilestonesCompleted(%s): %v", status, err)
			}
			if rows != 0 {
				t.Fatalf("a clear on a %s run must affect 0 rows (outside the claimed/running allowlist), got %d", status, rows)
			}
			if got := decodeIDs(run, "milestones_completed"); !eqIDs(got, []string{"m0", "m1"}) {
				t.Fatalf("a %s run's completed must be untouched = %v, want {m0,m1}", status, got)
			}
		})
	}
}
