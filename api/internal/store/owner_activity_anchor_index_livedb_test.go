package store

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

// Issue #4395 M2: the owner activity anchor is served by the three per-user indexes on runs
// (00468 runs_user_created_idx, 00469 runs_user_terminal_end_idx, 00470 runs_user_nonterminal_idx).
// The PRD statement is EXPLAINed verbatim, and so is the generated query that embeds the same
// arms (GetOwnerHibernationPoolInput), so a rewritten arm that loses its index is caught in both.
//
// Like idx_run_messages_plan_seq's test, this disables sequential scans for the transaction: it
// proves the index path EXISTS for the statement, not that the planner picks it under default
// settings on any particular table size.

// ownerAnchorStatement is the owner activity anchor and the active-now arm exactly as the PRD
// states them, with the owner as $1. The status lists are literals on purpose.
const ownerAnchorStatement = `SELECT GREATEST(
    (SELECT max(created_at) FROM runs WHERE user_id = $1),
    (SELECT max(COALESCE(finished_at, status_since)) FROM runs WHERE user_id = $1 AND status IN ('completed','failed','cancelled'))
) AS anchor,
EXISTS (SELECT 1 FROM runs WHERE user_id = $1 AND status NOT IN ('completed','failed','cancelled')) AS active_now`

func TestOwnerActivityAnchorUsesPerUserIndexesLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	if err := Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	// Registered before the user cleanups below: t.Cleanup runs last-in first-out, so the pool is
	// still open when they delete the users (a deferred Close would run first and make them no-ops,
	// leaking 8000 runs, 1600 of them non-terminal, into the shared database).
	t.Cleanup(pool.Close)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	user, other := uuid.New(), uuid.New()
	for _, u := range []uuid.UUID{user, other} {
		exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, u, fmt.Sprintf("owner-anchor-%s@example.com", u))
		u := u
		t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, u) })
	}
	// A noisy history: many terminal runs plus a few non-terminal ones for the owner, and runs of
	// another user that must not be scanned.
	for _, u := range []uuid.UUID{user, other} {
		exec(`INSERT INTO runs (id, user_id, kind, issue_title, issue_description, status, created_at, finished_at, status_since)
		      SELECT gen_random_uuid(), $1, 'chat', 't', 'd', CASE WHEN s % 5 = 0 THEN 'running' ELSE 'completed' END,
		             now() - (s || ' hours')::interval, now() - (s || ' hours')::interval, now() - (s || ' hours')::interval
		      FROM generate_series(1, 4000) s`, u)
	}
	exec(`ANALYZE runs`)

	// Behaviour first: the statement returns the newest created_at, the newest terminal finish and
	// active-now for the owner, over the whole history.
	var anchor pgtype.Timestamptz
	var active bool
	if err := pool.QueryRow(ctx, ownerAnchorStatement, user).Scan(&anchor, &active); err != nil {
		t.Fatalf("anchor: %v", err)
	}
	if !anchor.Valid || time.Since(anchor.Time) > 2*time.Hour || !active {
		t.Fatalf("anchor = %v active = %t, want about 1h ago and active", anchor, active)
	}

	explain := func(sql string, args ...any) string {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, `SET LOCAL enable_seqscan = off`); err != nil {
			t.Fatalf("SET enable_seqscan: %v", err)
		}
		rows, err := tx.Query(ctx, "EXPLAIN "+sql, args...)
		if err != nil {
			t.Fatalf("EXPLAIN: %v", err)
		}
		defer rows.Close()
		var plan strings.Builder
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatalf("scan EXPLAIN: %v", err)
			}
			plan.WriteString(line + "\n")
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("EXPLAIN rows: %v", err)
		}
		return plan.String()
	}
	want := []string{"runs_user_created_idx", "runs_user_terminal_end_idx", "runs_user_nonterminal_idx"}
	check := func(name, plan string) {
		t.Helper()
		t.Logf("EXPLAIN %s (enable_seqscan=off):\n%s", name, plan)
		for _, idx := range want {
			if !strings.Contains(plan, idx) {
				t.Errorf("%s: plan does not use %s:\n%s", name, idx, plan)
			}
		}
	}
	check("owner anchor statement", explain(ownerAnchorStatement, user))
	at := pgtype.Timestamptz{Time: time.Now(), Valid: true}
	check("GetOwnerHibernationPoolInput", explain(getOwnerHibernationPoolInput, user, int32(8), at))
	// The surplus-mode long queries compute the same owner arms once per owner.
	check("ListAutoHibernationCandidatesSurplusMode", explain(listAutoHibernationCandidatesSurplusMode, at, at, int32(512)))
	check("ListAutoHibernationBlockersSurplusMode", explain(listAutoHibernationBlockersSurplusMode, int32(50), at))
	check("ListHibernationWarmTargets", explain(listHibernationWarmTargets, at, int32(512)))
	check("GetWorkerLongIdleness", explain(getWorkerLongIdleness, uuid.New()))
}
