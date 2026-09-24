package store_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestRunUsageGenericPlanLiveDB is the issue #1620 regression: /api/runs timed out because
// pgx's cached prepared statements let PostgreSQL switch ListRunsForUser to a GENERIC plan,
// and that plan nested-looped the run_usage_totals view once per run row, re-folding every
// run_usage leg each time (13.9 s against 60 ms for the custom plan).
//
// The assertion is on PLAN SHAPE, never wall-clock: under plan_cache_mode =
// force_generic_plan, every plan node whose subtree scans run_usage may emit at most
// 4 × count(*) run_usage rows in total (Actual Rows × Actual Loops). One full fold of the
// view is ~1× (the window/aggregate levels above the scan each see at most the scanned
// rows), so a correct plan stays within a small constant of the table, while a per-row
// re-fold is (runs on the page) × table — here ≥ 300×, far outside the bound. A second,
// tighter bound applies to the user-scoped reads (see scopedBound): their run_usage scans
// may touch no more legs than the user owns, which catches a plan that folds EVERY user's
// usage once per call (the old SelfUsage hash-joined the whole view).
//
// The denominator is read in the SAME repeatable-read transaction as the EXPLAINs, so
// other LiveDB tests writing run_usage concurrently cannot move it between the count and
// the plans.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (the store IT runner
// provides one), like TestUpsertRunUsageMergeLiveDB.
func TestRunUsageGenericPlanLiveDB(t *testing.T) {
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

	// One fresh user with a repo, the shape of a real Runs page owner.
	userID, connID, repoID := uuid.New(), uuid.New(), uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("genericplan-%s@e2e", userID))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/generic-plan', 'https://forge.e2e/g/generic-plan', 'main', true)`, repoID, connID)

	// 300 non-chat issue runs, each with 4 per_leg run_usage legs: 2 models × 2 lineage
	// epochs, so every level of the view's fold (per leg, per lineage, per run) has work.
	// A second, unrelated user gets the same shape, so run_usage always holds legs this
	// user does NOT own, whatever else the shared LiveDB database carries when this runs:
	// a user-scoped read that folds the whole table is then visible (see scopedBound).
	const nRuns = 300
	otherUserID := uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		otherUserID, fmt.Sprintf("genericplan-other-%s@e2e", otherUserID))
	for _, owner := range []uuid.UUID{userID, otherUserID} {
		mustExec(ctx, t, pool,
			`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind, created_at)
			 SELECT gen_random_uuid(), $1, $2, g, 't', 'd', 'completed', 'issue', now() - make_interval(mins => g)
			 FROM generate_series(1, $3::int) g`, owner, repoID, nRuns)
		mustExec(ctx, t, pool,
			`INSERT INTO run_usage (run_id, session_id, model, lineage_epoch, input_tokens, cache_read_tokens,
			                        cache_creation_tokens, output_tokens, cost_usd, harness, cost_status, usage_basis)
			 SELECT r.id, 'sess-' || e, m, e, 100 + e, 10, 5, 50, 0.001, 'claude', 'metered', 'per_leg'
			 FROM runs r
			 CROSS JOIN (VALUES ('model-a'), ('model-b')) AS models(m)
			 CROSS JOIN generate_series(0, 1) AS e
			 WHERE r.user_id = $1`, owner)
	}

	// A judge run + its review, so GetJudgeRunUsageForTarget has a row whose LEFT JOIN
	// onto the view actually resolves a usage-bearing judge.
	var targetID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM runs WHERE user_id = $1 ORDER BY created_at DESC LIMIT 1`, userID).Scan(&targetID); err != nil {
		t.Fatalf("pick judge target: %v", err)
	}
	judge, err := q.CreateJudgeRun(ctx, store.CreateJudgeRunParams{
		Harness: "claude", UserID: userID, TargetRunID: pgtype.UUID{Bytes: targetID, Valid: true},
		IssueTitle: "Judge: generic plan", IssueDescription: "", TriggerSource: "judge",
	})
	if err != nil {
		t.Fatalf("CreateJudgeRun: %v", err)
	}
	mustExec(ctx, t, pool,
		`INSERT INTO run_reviews (target_run_id, judge_run_id, user_id, verdict) VALUES ($1, $2, $3, 'ok')`,
		targetID, judge.ID, userID)
	mustExec(ctx, t, pool,
		`INSERT INTO run_usage (run_id, session_id, model, lineage_epoch, input_tokens, output_tokens, cost_usd, usage_basis)
		 VALUES ($1, 'judge-sess', 'model-a', 0, 70, 30, 0.002, 'per_leg')`, judge.ID)

	// Many other owners with one usage-less run each, so runs.user_id has production's
	// shape (many distinct owners): the GENERIC estimate for "r.user_id = $1" is then a
	// handful of rows, which is exactly what led the planner to nest-loop the view per run
	// row in issue #1620. Without them the test's outcome would hinge on how many other
	// users earlier LiveDB tests happened to leave in the shared database.
	mustExec(ctx, t, pool,
		`WITH u AS (
		     INSERT INTO users (id, email, password_hash)
		     SELECT gen_random_uuid(), 'genericplan-noise-' || g || '-' || $1::text || '@e2e', 'x'
		     FROM generate_series(1, 200) g
		     RETURNING id
		 )
		 INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind)
		 SELECT gen_random_uuid(), u.id, $2, 1, 't', 'd', 'completed', 'issue' FROM u`,
		userID.String(), repoID)

	// Fresh statistics, so the plans below are the ones production's autovacuum-analyzed
	// tables would get rather than an artefact of never-analyzed tables.
	mustExec(ctx, t, pool, `ANALYZE runs`)
	mustExec(ctx, t, pool, `ANALYZE run_usage`)

	// The Runs page's own ids, exactly what the handler hands ListRunUsageTotalsForRuns.
	page, err := q.ListRunsForUser(ctx, store.ListRunsForUserParams{UserID: userID})
	if err != nil {
		t.Fatalf("ListRunsForUser: %v", err)
	}
	if len(page) == 0 {
		t.Fatal("ListRunsForUser returned no rows for the seeded user")
	}
	pageIDs := make([]string, 0, len(page))
	for _, row := range page {
		pageIDs = append(pageIDs, row.Run.ID.String())
	}

	u := func(id uuid.UUID) string { return "'" + id.String() + "'::uuid" }
	cutoff := "'" + time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano) + "'::timestamptz"
	// scoped marks a read whose answer depends only on this user's legs: its run_usage
	// SCAN nodes must emit at most the user's own leg count (scopedBound), which catches a
	// plan that folds every user's usage once per call — linear in the whole table, the
	// slow shape even without the per-row loop. GetJudgeRunUsageForTarget is not scoped:
	// its LEFT JOIN onto the view has no pushable qual, so it folds the table once (inside
	// the 4× bound) and is guarded here only against the per-row re-fold.
	cases := []struct {
		name, sql, args string
		scoped          bool
	}{
		// $1 background_grace_cutoff, $2 user_id, $3 repo_id (NULL = no filter), $4 issue_iid.
		{"ListRunsForUser", store.ListRunsForUserSQL, cutoff + ", " + u(userID) + ", NULL::uuid, NULL::bigint", true},
		{"ListRunUsageTotalsForRuns", store.ListRunUsageTotalsForRunsSQL, "'{" + strings.Join(pageIDs, ",") + "}'::uuid[]", true},
		{"SelfUsage", store.SelfUsageSQL, u(userID), true},
		{"GetJudgeRunUsageForTarget", store.GetJudgeRunUsageForTargetSQL, u(targetID), false},
		{"GetRunUsageTotal", store.GetRunUsageTotalSQL, u(targetID), true},
	}

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	// Simple protocol throughout: these are session/utility statements, and the plan under
	// test is the one the server-side PREPARE produces, not a pgx-cached one.
	simple := pgx.QueryExecModeSimpleProtocol
	exec := func(sql string) {
		t.Helper()
		if _, err := conn.Exec(ctx, sql, simple); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	exec(`BEGIN ISOLATION LEVEL REPEATABLE READ`)
	defer func() { _, _ = conn.Exec(context.Background(), `ROLLBACK`, simple) }()
	exec(`SET LOCAL plan_cache_mode = force_generic_plan`)

	var denominator int64
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM run_usage`, simple).Scan(&denominator); err != nil {
		t.Fatalf("count run_usage: %v", err)
	}
	if denominator < 4*nRuns {
		t.Fatalf("run_usage count %d < the %d legs this test seeded; the snapshot is wrong", denominator, 4*nRuns)
	}
	bound := 4 * float64(denominator)
	var ownLegs int64
	if err := conn.QueryRow(ctx,
		`SELECT count(*) FROM run_usage ru JOIN runs r ON r.id = ru.run_id WHERE r.user_id = $1`,
		simple, userID).Scan(&ownLegs); err != nil {
		t.Fatalf("count own run_usage: %v", err)
	}
	if ownLegs >= denominator {
		t.Fatalf("own legs %d >= table %d: the other user's legs are missing, so scopedBound proves nothing", ownLegs, denominator)
	}
	scopedBound := float64(ownLegs)

	for i, c := range cases {
		stmt := fmt.Sprintf("usage_plan_%d", i)
		exec(fmt.Sprintf("PREPARE %s AS %s", stmt, c.sql))
		var raw []byte
		if err := conn.QueryRow(ctx, fmt.Sprintf("EXPLAIN (ANALYZE, FORMAT JSON) EXECUTE %s(%s)", stmt, c.args), simple).Scan(&raw); err != nil {
			t.Fatalf("%s: EXPLAIN EXECUTE: %v", c.name, err)
		}
		var generic int64
		if err := conn.QueryRow(ctx, `SELECT generic_plans FROM pg_prepared_statements WHERE name = $1`, simple, stmt).Scan(&generic); err != nil {
			t.Fatalf("%s: read pg_prepared_statements: %v", c.name, err)
		}
		if generic < 1 {
			t.Fatalf("%s: generic_plans = %d; the EXPLAIN did not run the GENERIC plan this test exists to measure", c.name, generic)
		}
		exec("DEALLOCATE " + stmt)

		var doc []struct {
			Plan planNode `json:"Plan"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil || len(doc) != 1 {
			t.Fatalf("%s: decode EXPLAIN JSON (%d docs): %v", c.name, len(doc), err)
		}
		var offenders []string
		walkUsagePlan(&doc[0].Plan, func(n *planNode) {
			if total := n.ActualRows * n.ActualLoops; total > bound {
				offenders = append(offenders, fmt.Sprintf("%s (rows %.0f × loops %.0f = %.0f)", n.NodeType, n.ActualRows, n.ActualLoops, total))
			}
			if c.scoped && n.RelationName == "run_usage" {
				// EXPLAIN prints Actual Rows as the per-loop average ROUNDED to an integer, so
				// rows × loops can overshoot the true total by up to half a row per loop
				// (1201 legs over 301 loops prints as 4 × 301 = 1204); allow exactly that.
				if total := n.ActualRows * n.ActualLoops; total > scopedBound+n.ActualLoops/2 {
					offenders = append(offenders, fmt.Sprintf("%s on run_usage (rows %.0f × loops %.0f = %.0f > the user's own %d legs: folds other users' usage)",
						n.NodeType, n.ActualRows, n.ActualLoops, total, ownLegs))
				}
			}
		})
		if len(offenders) > 0 {
			t.Errorf("%s: under a generic plan, run_usage work exceeds its bound (4 × count(run_usage) = %.0f, count %d, own legs %d), i.e. the view is re-folded per outer row or folded beyond this user (issue #1620):\n  %s",
				c.name, bound, denominator, ownLegs, strings.Join(offenders, "\n  "))
		}
		t.Logf("%s generic plan (run_usage count %d):\n%s", c.name, denominator, renderPlan(&doc[0].Plan, 0))
	}
}

// planNode is the subset of EXPLAIN (ANALYZE, FORMAT JSON) this test reads. InitPlans and
// SubPlans appear as ordinary children in "Plans", so walking Plans covers them.
type planNode struct {
	NodeType     string     `json:"Node Type"`
	RelationName string     `json:"Relation Name"`
	IndexName    string     `json:"Index Name"`
	ActualRows   float64    `json:"Actual Rows"`
	ActualLoops  float64    `json:"Actual Loops"`
	Plans        []planNode `json:"Plans"`
}

// walkUsagePlan calls visit on every node whose subtree (itself included) scans run_usage,
// and reports whether n's subtree does.
func walkUsagePlan(n *planNode, visit func(*planNode)) bool {
	scans := n.RelationName == "run_usage"
	for i := range n.Plans {
		if walkUsagePlan(&n.Plans[i], visit) {
			scans = true
		}
	}
	if scans {
		visit(n)
	}
	return scans
}

// renderPlan is a compact indented plan for the test log, so the report can quote the
// generic plan's shape without re-running EXPLAIN by hand.
func renderPlan(n *planNode, depth int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s%s", strings.Repeat("  ", depth), n.NodeType)
	if n.RelationName != "" {
		fmt.Fprintf(&b, " on %s", n.RelationName)
	}
	if n.IndexName != "" {
		fmt.Fprintf(&b, " using %s", n.IndexName)
	}
	fmt.Fprintf(&b, " (rows=%.0f loops=%.0f)\n", n.ActualRows, n.ActualLoops)
	for i := range n.Plans {
		b.WriteString(renderPlan(&n.Plans[i], depth+1))
	}
	return b.String()
}
