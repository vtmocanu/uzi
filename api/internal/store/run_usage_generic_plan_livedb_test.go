package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/analytics"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// TestRunUsageGenericPlanLiveDB is the issue #1620 regression: /api/runs timed out because
// pgx's cached prepared statements let PostgreSQL switch ListRunsForUser to a GENERIC plan,
// and that plan nested-looped the run_usage_totals view once per run row, re-folding every
// run_usage leg each time (13.9 s against 60 ms for the custom plan).
//
// The assertion is on PLAN SHAPE, never wall-clock: under plan_cache_mode =
// force_generic_plan, every plan node whose subtree scans run_usage may do at most
// 4 × count(*) run_usage rows of work in total (see nodeWork: Actual Rows × Actual Loops,
// plus the rows a scan read and discarded by its Filter). One full fold of the view is
// ~1× (the window/aggregate levels above the scan each see at most the scanned rows), so
// a correct plan stays within a small constant of the table, while a per-row re-fold
// repeats the fold once per run row on the page, two orders of magnitude over the table
// at this seed, far outside the bound. A second,
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
	e := newGenericPlanEnv(t)
	ctx, q := e.ctx, e.q
	userID, targetID := e.userID, e.targetID

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
	cases := []genericPlanCase{
		// $1 background_grace_cutoff, $2 user_id, $3 repo_id (NULL = no filter), $4 issue_iid.
		// Since #1620 M1 ListRunsForUser's plan does not touch run_usage at all: this case guards
		// against the view join coming back into the run-list query.
		{name: "ListRunsForUser", sql: store.ListRunsForUserSQL, args: []string{cutoff + ", " + u(userID) + ", NULL::uuid, NULL::bigint"}, scoped: true},
		{name: "ListRunUsageTotalsForRuns", sql: store.ListRunUsageTotalsForRunsSQL, args: []string{"'{" + strings.Join(pageIDs, ",") + "}'::uuid[]"}, scoped: true},
		{name: "SelfUsage", sql: store.SelfUsageSQL, args: []string{u(userID)}, scoped: true},
		{name: "GetJudgeRunUsageForTarget", sql: store.GetJudgeRunUsageForTargetSQL, args: []string{u(targetID)}, scoped: false},
		{name: "GetRunUsageTotal", sql: store.GetRunUsageTotalSQL, args: []string{u(targetID)}, scoped: true},
	}
	// Issue #4684: the Spend statements, in the same table. OwnerSpend is scoped to the
	// Spend-only owner and is run with @reveal_repos false and true against one prepared
	// statement (one cached generic plan serves both routes); AllSpend is unscoped.
	cases = append(cases,
		genericPlanCase{name: "AnalyticsOwnerSpend", sql: store.AnalyticsOwnerSpendSQL, args: e.spendArgs(t, e.spendOwnerID, false, true), scoped: true, spend: true, ownLegs: e.spendOwnLegs},
		genericPlanCase{name: "AnalyticsAllSpend", sql: store.AnalyticsAllSpendSQL, args: e.spendArgs(t, uuid.Nil, false, false), scoped: false, spend: true},
	)
	// Issue #4730: the Quality statements, owner-scoped to the Quality owner (whose PRs carry
	// mr_rework runs, merge witnesses, cross-checks and reviews in the seed) and all-scope.
	cases = append(cases,
		genericPlanCase{name: "AnalyticsOwnerQuality", sql: store.AnalyticsOwnerQualitySQL, args: e.qualityArgs(t, e.qualityOwnerID), quality: true},
		genericPlanCase{name: "AnalyticsAllQuality", sql: store.AnalyticsAllQualitySQL, args: e.qualityArgs(t, uuid.Nil), quality: true},
	)

	for i, c := range cases {
		stmt := fmt.Sprintf("usage_plan_%d", i)
		e.exec(t, fmt.Sprintf("PREPARE %s AS %s", stmt, c.sql))
		for v, args := range c.args {
			name := c.name
			if len(c.args) > 1 {
				name = fmt.Sprintf("%s[variant %d]", c.name, v)
			}
			doc := e.explain(t, name, stmt, args)
			e.checkPlan(t, name, c, doc)
			t.Logf("%s generic plan (run_usage count %d):\n%s", name, e.denominator, renderPlan(doc, 0))
		}
		e.exec(t, "DEALLOCATE "+stmt)
	}
}

// genericPlanCase is one statement under test: its SQL and one or more EXECUTE argument
// lists (variants of one prepared statement). spend marks the #4684 Spend statements, which
// get extra checks keyed to them; ownLegs overrides the scoped bound's own-leg count.
type genericPlanCase struct {
	name, sql string
	args      []string
	scoped    bool
	spend     bool
	ownLegs   int64
	// quality marks the #4730 Quality statements: they read no run_usage, so qualityOffenders
	// applies instead (the looped-work class limited to CTE Scans and runs, no join-discard
	// class, plus qualityShapeProblems and qualityExecutedProblems).
	quality bool
}

// genericPlanEnv is the seeded database, the repeatable-read connection with
// plan_cache_mode = force_generic_plan, and the bounds the plan checks compare against.
type genericPlanEnv struct {
	ctx  context.Context
	pool *pgxpool.Pool
	q    *store.Queries
	conn *pgxpool.Conn

	userID, targetID uuid.UUID
	// The Spend-only owner (and its runs), seeded after the five usage cases' data.
	spendOwnerID uuid.UUID
	// Issue #4685: the latest-failure seed (see seedGenericPlanLatestFailure).
	latest genericPlanLatest
	// relOwnerID is the Reliability seed's owner (zero unless newGenericPlanEnvWith asked for
	// it) and relOwnLegs its run_usage legs; seededUsers are the base seed's named users.
	relOwnerID  uuid.UUID
	relOwnLegs  int64
	seededUsers []uuid.UUID
	// The Quality-only owner (PRs, reworks, cross-checks, reviews), seeded last.
	qualityOwnerID uuid.UUID

	// denominator, ownLegs, bound and scopedBound serve the five original cases and exclude
	// the Spend owner's legs; the spend* fields serve the Spend cases.
	denominator      int64
	ownLegs          int64
	spendOwnLegs     int64
	spendDenominator int64
	bound            float64
	spendBound       float64
	scopedBound      float64
	runsTotal        int64
}

func newGenericPlanEnv(t *testing.T) *genericPlanEnv { return newGenericPlanEnvWith(t, false) }

// newGenericPlanEnvWith is newGenericPlanEnv plus, when withReliability, the Reliability seed
// (seedGenericPlanReliability) inserted before the repeatable-read snapshot is taken.
func newGenericPlanEnvWith(t *testing.T, withReliability bool) *genericPlanEnv {
	t.Helper()
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
	t.Cleanup(pool.Close)
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

	// Issue #4684 Spend-only seed, appended after everything the five usage cases read. A
	// dedicated owner keeps those cases' data untouched. Its runs spread over ~37 days (so
	// both the current and the previous 30d window hold runs) and a fraction carry mr_iid
	// (some shared by several runs) and mr_state merged, so the witness semi-join and the
	// per-PR attribution actually execute. Each run's legs spread over 25 distinct models, so
	// the usage_model split and the primary_model breakdown have many groups and a
	// runs × groups join is visible against 4 × count(runs). Two owners' worth of legs exist already (userID,
	// otherUserID), so an unscoped fold of this owner's usage is visible against its own legs.
	spendOwnerID, spendConnID, spendRepoID := uuid.New(), uuid.New(), uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		spendOwnerID, fmt.Sprintf("genericplan-spend-%s@e2e", spendOwnerID))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, spendConnID, spendOwnerID, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/generic-plan-spend', 'https://forge.e2e/g/generic-plan-spend', 'main', true)`, spendRepoID, spendConnID)
	mustExec(ctx, t, pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind, created_at,
		                   mr_iid, mr_state)
		 SELECT gen_random_uuid(), $1, $2, g, 't', 'd', 'completed', 'issue', now() - make_interval(hours => g * 3),
		        CASE WHEN g % 2 = 0 THEN (g % 60) + 1 END,
		        CASE WHEN g % 4 = 0 THEN 'merged' WHEN g % 2 = 0 THEN 'opened' END
		 FROM generate_series(1, $3::int) g`, spendOwnerID, spendRepoID, nRuns)
	mustExec(ctx, t, pool,
		`INSERT INTO run_usage (run_id, session_id, model, lineage_epoch, input_tokens, cache_read_tokens,
		                        cache_creation_tokens, output_tokens, cost_usd, harness, cost_status, usage_basis)
		 SELECT r.id, 'sess-' || e, 'spend-model-' || lpad(((r.issue_iid + k * 7) % 25)::text, 2, '0'), e,
		        100 + e, 10, 5, 50, 0.001, 'claude', 'metered', 'per_leg'
		 FROM runs r
		 CROSS JOIN generate_series(0, 1) AS k
		 CROSS JOIN generate_series(0, 1) AS e
		 WHERE r.user_id = $1`, spendOwnerID)
	// Issue #4730 Quality seed: a dedicated owner (so the usage and Spend cases' data is
	// untouched) whose 300 issue runs share ~120 PRs (three quarters merged), with verified and
	// mismatched mr_rework runs, plan and code cross-checks, retained reviews and judge skips,
	// so the witness fold, the rework-link membership and the judge joins all execute.
	qualityOwnerID, qualityConnID, qualityRepoID := uuid.New(), uuid.New(), uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		qualityOwnerID, fmt.Sprintf("genericplan-quality-%s@e2e", qualityOwnerID))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, qualityConnID, qualityOwnerID, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/generic-plan-quality', 'https://forge.e2e/g/generic-plan-quality', 'main', true)`, qualityRepoID, qualityConnID)
	mustExec(ctx, t, pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind, created_at, status_since,
		                   mr_iid, mr_state)
		 SELECT gen_random_uuid(), $1, $2, g, 't', 'd', CASE WHEN g % 9 = 0 THEN 'failed' ELSE 'completed' END, 'issue',
		        now() - make_interval(hours => g * 2), now() - make_interval(hours => g * 2),
		        (g % 120) + 1, CASE WHEN g % 4 = 0 THEN 'opened' ELSE 'merged' END
		 FROM generate_series(1, $3::int) g`, qualityOwnerID, qualityRepoID, nRuns)
	mustExec(ctx, t, pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_title, issue_description, status, kind, created_at, status_since, mr_iid,
		                   pipeline_ref, target_run_id, trigger_source)
		 SELECT gen_random_uuid(), r.user_id, r.repo_id, 't', 'd', 'completed', 'mr_rework', r.created_at + interval '1 hour',
		        r.created_at + interval '1 hour', r.mr_iid, 'refs/mr/' || r.id || '-' || k,
		        CASE WHEN r.issue_iid % 10 = 0 AND k = 1 THEN (SELECT o.id FROM runs o WHERE o.user_id = r.user_id AND o.kind = 'issue' AND o.mr_iid <> r.mr_iid LIMIT 1) ELSE r.id END,
		        CASE WHEN r.issue_iid % 3 = 0 THEN 'manual' ELSE 'mr_rework' END
		 FROM runs r CROSS JOIN generate_series(1, 2) k
		 WHERE r.user_id = $1 AND r.kind = 'issue'
		   AND ((r.issue_iid % 4 = 1 AND (k = 1 OR r.issue_iid % 8 = 1)) OR (r.issue_iid % 16 = 0 AND k = 1))`, qualityOwnerID)
	mustExec(ctx, t, pool,
		`INSERT INTO cross_checks (lead_run_id, stage, round, lead_claim_generation, plan_md, milestones, size_class,
		     base_commit, candidate_digest, verdict, reason_class, decided_at, deadline_at, created_at, checker_harness)
		 SELECT r.id, 'plan', 1, 0, 'p', '[]'::jsonb, 'S', 'base', '\x01'::bytea, 'approve', 'approve',
		        r.created_at + interval '10 minutes', r.created_at + interval '1 hour', r.created_at, 'codex'
		 FROM runs r WHERE r.user_id = $1 AND r.kind = 'issue' AND r.issue_iid % 5 = 0`, qualityOwnerID)
	mustExec(ctx, t, pool,
		`INSERT INTO cross_checks (lead_run_id, stage, round, lead_claim_generation, base_commit, head_commit, candidate_digest,
		     verdict, outcome, findings, decided_at, deadline_at, created_at, checker_harness,
		     code_context, guidance_snapshot, guidance_text, guidance_digest, repo_instructions_enabled,
		     repo_instructions_text, repo_instructions_digest)
		 SELECT r.id, 'code', 1, 0, repeat('a', 40), repeat('b', 40), '\x01'::bytea,
		     'failed', 'completed', '[]'::jsonb, r.created_at + interval '20 minutes', r.created_at + interval '1 hour', r.created_at, 'codex',
		     '{}'::jsonb, '', '', sha256(convert_to('', 'UTF8')), false, '', sha256(convert_to('', 'UTF8'))
		 FROM runs r WHERE r.user_id = $1 AND r.kind = 'issue' AND r.issue_iid % 7 = 0`, qualityOwnerID)
	mustExec(ctx, t, pool,
		`INSERT INTO run_reviews (target_run_id, user_id, verdict, status, created_at, updated_at)
		 SELECT r.id, r.user_id, 'ok', 'complete', r.created_at + interval '5 minutes', r.created_at + interval '5 minutes'
		 FROM runs r WHERE r.user_id = $1 AND r.kind = 'issue' AND r.issue_iid % 6 = 0`, qualityOwnerID)
	mustExec(ctx, t, pool,
		`INSERT INTO judge_skips (target_run_id, harness, reason)
		 SELECT r.id, 'claude', 'no_usable_credential'
		 FROM runs r WHERE r.user_id = $1 AND r.kind = 'issue' AND r.issue_iid % 11 = 1`, qualityOwnerID)
	for _, tbl := range []string{"cross_checks", "run_reviews", "judge_skips"} {
		mustExec(ctx, t, pool, `ANALYZE `+tbl)
	}
	mustExec(ctx, t, pool, `ANALYZE runs`)
	mustExec(ctx, t, pool, `ANALYZE run_usage`)
	latest := seedGenericPlanLatestFailure(ctx, t, pool)
	var relOwnerID uuid.UUID
	if withReliability {
		relOwnerID = seedGenericPlanReliability(ctx, t, pool)
	}

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	t.Cleanup(conn.Release)
	e := &genericPlanEnv{ctx: ctx, pool: pool, q: q, conn: conn, userID: userID, targetID: targetID, spendOwnerID: spendOwnerID, latest: latest,
		relOwnerID: relOwnerID, seededUsers: []uuid.UUID{userID, otherUserID, spendOwnerID}, qualityOwnerID: qualityOwnerID}
	// Simple protocol throughout: these are session/utility statements, and the plan under
	// test is the one the server-side PREPARE produces, not a pgx-cached one.
	e.exec(t, `BEGIN ISOLATION LEVEL REPEATABLE READ`)
	t.Cleanup(func() { _, _ = conn.Exec(context.Background(), `ROLLBACK`, pgx.QueryExecModeSimpleProtocol) })
	e.exec(t, `SET LOCAL plan_cache_mode = force_generic_plan`)

	// The five original cases' counts are the table WITHOUT the Spend owner's legs, read in
	// this same snapshot, so their sanity checks and bounds are exactly what they were
	// before the Spend seed existed (a missing seed for them still fails here).
	var fullCount int64
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM run_usage`, pgx.QueryExecModeSimpleProtocol).Scan(&fullCount); err != nil {
		t.Fatalf("count run_usage: %v", err)
	}
	if err := conn.QueryRow(ctx,
		`SELECT count(*) FROM run_usage ru JOIN runs r ON r.id = ru.run_id WHERE r.user_id = $1`,
		pgx.QueryExecModeSimpleProtocol, spendOwnerID).Scan(&e.spendOwnLegs); err != nil {
		t.Fatalf("count spend owner run_usage: %v", err)
	}
	e.denominator = fullCount - e.spendOwnLegs
	if e.denominator < 4*nRuns {
		t.Fatalf("run_usage count %d < the %d legs this test seeded; the snapshot is wrong", e.denominator, 4*nRuns)
	}
	e.bound = 4 * float64(e.denominator)
	if err := conn.QueryRow(ctx,
		`SELECT count(*) FROM run_usage ru JOIN runs r ON r.id = ru.run_id WHERE r.user_id = $1`,
		pgx.QueryExecModeSimpleProtocol, userID).Scan(&e.ownLegs); err != nil {
		t.Fatalf("count own run_usage: %v", err)
	}
	if e.ownLegs >= e.denominator {
		t.Fatalf("own legs %d >= table %d: the other user's legs are missing, so scopedBound proves nothing", e.ownLegs, e.denominator)
	}
	e.scopedBound = float64(e.ownLegs)
	// The Spend cases compare against the whole table, Spend seed included.
	if e.spendOwnLegs < 4*nRuns || e.spendOwnLegs >= fullCount {
		t.Fatalf("spend owner legs %d (table %d, need at least %d): the Spend seed or the other users' legs are missing", e.spendOwnLegs, fullCount, 4*nRuns)
	}
	e.spendDenominator = fullCount
	e.spendBound = 4 * float64(fullCount)
	if relOwnerID != uuid.Nil {
		if err := conn.QueryRow(ctx,
			`SELECT count(*) FROM run_usage ru JOIN runs r ON r.id = ru.run_id WHERE r.user_id = $1`,
			pgx.QueryExecModeSimpleProtocol, relOwnerID).Scan(&e.relOwnLegs); err != nil {
			t.Fatalf("count reliability owner run_usage: %v", err)
		}
	}
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM runs`, pgx.QueryExecModeSimpleProtocol).Scan(&e.runsTotal); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	return e
}

func (e *genericPlanEnv) exec(t *testing.T, sql string) {
	t.Helper()
	if _, err := e.conn.Exec(e.ctx, sql, pgx.QueryExecModeSimpleProtocol); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

// explain runs EXPLAIN (ANALYZE, VERBOSE, FORMAT JSON) EXECUTE on the prepared statement and returns
// the plan, failing unless PostgreSQL ran the GENERIC plan.
func (e *genericPlanEnv) explain(t *testing.T, name, stmt, args string) *planNode {
	t.Helper()
	var raw []byte
	if err := e.conn.QueryRow(e.ctx, fmt.Sprintf("EXPLAIN (ANALYZE, VERBOSE, FORMAT JSON) EXECUTE %s(%s)", stmt, args), pgx.QueryExecModeSimpleProtocol).Scan(&raw); err != nil {
		t.Fatalf("%s: EXPLAIN EXECUTE: %v", name, err)
	}
	var generic int64
	if err := e.conn.QueryRow(e.ctx, `SELECT generic_plans FROM pg_prepared_statements WHERE name = $1`, pgx.QueryExecModeSimpleProtocol, stmt).Scan(&generic); err != nil {
		t.Fatalf("%s: read pg_prepared_statements: %v", name, err)
	}
	if generic < 1 {
		t.Fatalf("%s: generic_plans = %d; the EXPLAIN did not run the GENERIC plan this test exists to measure", name, generic)
	}
	var doc []struct {
		Plan planNode `json:"Plan"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil || len(doc) != 1 {
		t.Fatalf("%s: decode EXPLAIN JSON (%d docs): %v", name, len(doc), err)
	}
	return &doc[0].Plan
}

// planOffenders is the issue #1620 check: every node whose subtree scans run_usage may do at
// most 4 × count(run_usage) rows of work, and a scoped statement's run_usage scans may touch
// no more legs than the scope's owner has.
func (e *genericPlanEnv) planOffenders(c genericPlanCase, root *planNode) []string {
	own := e.ownLegs
	if c.ownLegs != 0 {
		own = c.ownLegs
	}
	scopedBound := float64(own)
	bound := e.bound
	if c.spend {
		bound = e.spendBound
	}
	var offenders []string
	walkUsagePlan(root, func(n *planNode) {
		if total, _ := nodeWork(n); total > bound {
			offenders = append(offenders, fmt.Sprintf("%s (rows %.0f + removed by filter %.0f, × loops %.0f = %.0f)",
				n.NodeType, n.ActualRows, n.RowsRemovedByFilter, n.ActualLoops, total))
		}
		if c.scoped && n.RelationName == "run_usage" {
			// EXPLAIN prints each per-loop count as an average ROUNDED to an integer, so
			// count × loops can overshoot the true total by up to half a row per loop per
			// rounded count (1201 legs over 301 loops prints as 4 × 301 = 1204); allow
			// exactly that.
			if total, rounded := nodeWork(n); total > scopedBound+rounded*n.ActualLoops/2 {
				offenders = append(offenders, fmt.Sprintf("%s on run_usage (rows %.0f + removed by filter %.0f, × loops %.0f = %.0f > the user's own %d legs: folds other users' usage)",
					n.NodeType, n.ActualRows, n.RowsRemovedByFilter, n.ActualLoops, total, own))
			}
		}
	})
	return offenders
}

// checkPlan asserts the plan stays inside its bounds and, for the Spend statements, the
// extra #4684 conditions.
func (e *genericPlanEnv) checkPlan(t *testing.T, name string, c genericPlanCase, root *planNode) {
	t.Helper()
	if c.quality {
		for _, o := range e.qualityOffenders(root) {
			t.Errorf("%s: %s", name, o)
		}
		return
	}
	own := e.ownLegs
	if c.ownLegs != 0 {
		own = c.ownLegs
	}
	if offenders := e.planOffenders(c, root); len(offenders) > 0 {
		bound, denominator := e.bound, e.denominator
		if c.spend {
			bound, denominator = e.spendBound, e.spendDenominator
		}
		t.Errorf("%s: under a generic plan, run_usage work exceeds its bound (4 × count(run_usage) = %.0f, count %d, own legs %d), i.e. the view is re-folded per outer row or folded beyond this user (issue #1620):\n  %s",
			name, bound, denominator, own, strings.Join(offenders, "\n  "))
	}
	if !c.spend {
		return
	}
	for _, o := range e.spendOffenders(root) {
		t.Errorf("%s: %s", name, o)
	}
	var usageScans []*planNode
	var runsScans []*planNode
	var visit func(n *planNode)
	visit = func(n *planNode) {
		switch n.RelationName {
		case "run_usage":
			usageScans = append(usageScans, n)
		case "runs":
			runsScans = append(runsScans, n)
		}
		for i := range n.Plans {
			visit(&n.Plans[i])
		}
	}
	visit(root)

	// Non-vacuity: the fold really read run_usage, so the bounds above compare something.
	var nonEmpty bool
	for _, n := range usageScans {
		if n.ActualRows*n.ActualLoops > 0 {
			nonEmpty = true
		}
	}
	if !nonEmpty {
		t.Errorf("%s: no run_usage scan node returned rows; the seed or the window is empty and the plan bounds prove nothing", name)
	}
	// Fold once (unscoped statement): the run_usage SCAN nodes together read each leg about
	// once. Only scan nodes of run_usage count, never an ancestor aggregate, window or CTE.
	if !c.scoped {
		var sum, slack float64
		for _, n := range usageScans {
			total, rounded := nodeWork(n)
			sum += total
			slack += rounded * n.ActualLoops / 2
		}
		if sum > float64(e.spendDenominator)+slack {
			t.Errorf("%s: run_usage scan nodes did %.0f rows of work in total, more than one fold of the table (%d, rounding slack %.0f)",
				name, sum, e.spendDenominator, slack)
		}
	}
	// No per-cohort-row re-scan of runs: a scan node that looped more than once may not do
	// more than 4 × count(runs) work in total.
	for _, n := range runsScans {
		if total, _ := nodeWork(n); n.ActualLoops > 1 && total > 4*float64(e.runsTotal) {
			t.Errorf("%s: %s on runs looped %.0f times for %.0f rows of work (> 4 × count(runs) = %d): a per-row re-scan of runs",
				name, n.NodeType, n.ActualLoops, total, 4*e.runsTotal)
		}
	}
}

// spendArgs renders the EXECUTE arguments of a Spend statement for a 30d window ending at
// the transaction's now(), with the bucket and rolling arrays from the real calendar code.
// owner == uuid.Nil selects the all-scope statement. When bothReveal is true the owner
// statement yields two variants, @reveal_repos false then true. The argument order is read
// from what the generated Go method actually passes (a capturing DBTX), never assumed.
func (e *genericPlanEnv) spendArgs(t *testing.T, owner uuid.UUID, reveal, bothReveal bool) []string {
	t.Helper()
	var now time.Time
	// now() read inside the plan transaction (newGenericPlanEnv has already begun it), so
	// DB/host clock skew cannot empty the cohort.
	if err := e.conn.QueryRow(e.ctx, `SELECT now()`, pgx.QueryExecModeSimpleProtocol).Scan(&now); err != nil {
		t.Fatalf("select now(): %v", err)
	}
	b, err := analytics.ComputeBounds(now, time.UTC, analytics.Window30d, nil)
	if err != nil {
		t.Fatalf("ComputeBounds: %v", err)
	}
	ts := func(v time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: v, Valid: true} }
	tss := func(vs []time.Time) []pgtype.Timestamptz {
		out := make([]pgtype.Timestamptz, len(vs))
		for i, v := range vs {
			out[i] = ts(v)
		}
		return out
	}
	var prev pgtype.Timestamptz
	if b.PreviousFrom != nil {
		prev = ts(*b.PreviousFrom)
	}
	common := func() (from, previous, fold, nowTS pgtype.Timestamptz) {
		return ts(b.From), prev, ts(b.FoldFrom), ts(b.Now)
	}
	from, previous, fold, nowTS := common()
	var variants [][]any
	capture := &capturingDBTX{}
	cq := store.New(capture)
	if owner != uuid.Nil {
		revs := []bool{reveal}
		if bothReveal {
			revs = []bool{false, true}
		}
		for _, r := range revs {
			capture.args = nil
			_, _ = cq.AnalyticsOwnerSpend(e.ctx, store.AnalyticsOwnerSpendParams{
				UserID: owner, RevealRepos: r, Now: nowTS, From: from, PreviousFrom: previous, FoldFrom: fold,
				BucketStarts: tss(b.BucketStarts), BucketEnds: tss(b.BucketEnds),
				RollStarts: tss(b.RollStarts), RollEnds: tss(b.RollEnds),
				Kinds: runkind.All(), DeliveryKinds: runkind.Delivery(), Split: "usage_model", Breakdown: "repo",
			})
			variants = append(variants, capture.args)
		}
	} else {
		_, _ = cq.AnalyticsAllSpend(e.ctx, store.AnalyticsAllSpendParams{
			Now: nowTS, From: from, PreviousFrom: previous, FoldFrom: fold,
			BucketStarts: tss(b.BucketStarts), BucketEnds: tss(b.BucketEnds),
			RollStarts: tss(b.RollStarts), RollEnds: tss(b.RollEnds),
			Kinds: runkind.All(), DeliveryKinds: runkind.Delivery(), Split: "usage_model", Breakdown: "primary_model",
		})
		variants = append(variants, capture.args)
	}
	out := make([]string, 0, len(variants))
	for _, args := range variants {
		lits := make([]string, len(args))
		for i, a := range args {
			lits[i] = sqlLiteral(t, a)
		}
		out = append(out, strings.Join(lits, ", "))
	}
	return out
}

// qualityArgs renders the EXECUTE arguments of a Quality statement for a 30d window ending at
// the transaction's now(). owner == uuid.Nil selects the all-scope statement. The argument
// order is read from what the generated Go method actually passes, as in spendArgs.
func (e *genericPlanEnv) qualityArgs(t *testing.T, owner uuid.UUID) []string {
	t.Helper()
	var now time.Time
	if err := e.conn.QueryRow(e.ctx, `SELECT now()`, pgx.QueryExecModeSimpleProtocol).Scan(&now); err != nil {
		t.Fatalf("select now(): %v", err)
	}
	b, err := analytics.ComputeBounds(now, time.UTC, analytics.Window30d, nil)
	if err != nil {
		t.Fatalf("ComputeBounds: %v", err)
	}
	return []string{qualityExecuteArgs(t, e.ctx, owner, b)}
}

// qualityExecuteArgs is the EXECUTE argument list of the owner (owner != uuid.Nil) or all-scope
// Quality statement for the given bounds, all kinds, no repo or harness filter.
func qualityExecuteArgs(t *testing.T, ctx context.Context, owner uuid.UUID, b analytics.Bounds) string {
	t.Helper()
	ts := func(v time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: v, Valid: true} }
	tss := func(vs []time.Time) []pgtype.Timestamptz {
		out := make([]pgtype.Timestamptz, len(vs))
		for i, v := range vs {
			out[i] = ts(v)
		}
		return out
	}
	var judge []string
	for _, k := range runkind.All() {
		if runkind.JudgeEligible(k) {
			judge = append(judge, k)
		}
	}
	planLead, codeLead := qualityCall{}.leadKinds()
	always, prestart := workersvc.JudgeSkipFailOrigins()
	capture := &capturingDBTX{}
	cq := store.New(capture)
	if owner != uuid.Nil {
		_, _ = cq.AnalyticsOwnerQuality(ctx, store.AnalyticsOwnerQualityParams{
			UserID: owner, Now: ts(b.Now), From: ts(b.From), BucketStarts: tss(b.BucketStarts), BucketEnds: tss(b.BucketEnds),
			Kinds: runkind.All(), DeliveryKinds: runkind.Delivery(), AlwaysSkipOrigins: always, PrestartOrigins: prestart, JudgeKinds: judge,
			PlanLeadKinds: planLead, CodeLeadKinds: codeLead,
		})
	} else {
		_, _ = cq.AnalyticsAllQuality(ctx, store.AnalyticsAllQualityParams{
			Now: ts(b.Now), From: ts(b.From), BucketStarts: tss(b.BucketStarts), BucketEnds: tss(b.BucketEnds),
			Kinds: runkind.All(), DeliveryKinds: runkind.Delivery(), AlwaysSkipOrigins: always, PrestartOrigins: prestart, JudgeKinds: judge,
			PlanLeadKinds: planLead, CodeLeadKinds: codeLead,
		})
	}
	lits := make([]string, len(capture.args))
	for i, a := range capture.args {
		lits[i] = sqlLiteral(t, a)
	}
	return strings.Join(lits, ", ")
}

// capturingDBTX records the arguments of the last QueryRow so a test can read the argument
// order of a generated sqlc method.
type capturingDBTX struct{ args []any }

func (c *capturingDBTX) Exec(context.Context, string, ...interface{}) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}

func (c *capturingDBTX) Query(context.Context, string, ...interface{}) (pgx.Rows, error) {
	return nil, errors.New("capturingDBTX: Query unsupported")
}

func (c *capturingDBTX) QueryRow(_ context.Context, _ string, args ...interface{}) pgx.Row {
	c.args = args
	return capturedRow{}
}

type capturedRow struct{}

func (capturedRow) Scan(...any) error { return nil }

// sqlLiteral renders one generated-method argument as a typed SQL literal for EXECUTE.
func sqlLiteral(t *testing.T, a any) string {
	t.Helper()
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
	tsLit := func(v pgtype.Timestamptz) string {
		if !v.Valid {
			return "NULL::timestamptz"
		}
		return quote(v.Time.UTC().Format(time.RFC3339Nano)) + "::timestamptz"
	}
	switch v := a.(type) {
	case pgtype.Timestamptz:
		return tsLit(v)
	case []pgtype.Timestamptz:
		parts := make([]string, len(v))
		for i, x := range v {
			parts[i] = tsLit(x)
		}
		return "ARRAY[" + strings.Join(parts, ", ") + "]::timestamptz[]"
	case string:
		return quote(v) + "::text"
	case []string:
		parts := make([]string, len(v))
		for i, x := range v {
			parts[i] = quote(x)
		}
		return "ARRAY[" + strings.Join(parts, ", ") + "]::text[]"
	case bool:
		return fmt.Sprintf("%t::boolean", v)
	case uuid.UUID:
		return quote(v.String()) + "::uuid"
	case pgtype.UUID:
		if !v.Valid {
			return "NULL::uuid"
		}
		return quote(uuid.UUID(v.Bytes).String()) + "::uuid"
	case pgtype.Text:
		if !v.Valid {
			return "NULL::text"
		}
		return quote(v.String) + "::text"
	}
	t.Fatalf("sqlLiteral: unsupported argument type %T", a)
	return ""
}

// TestAnalyticsSpendGenericPlanNegativeControlLiveDB proves the Spend plan check can fail:
// an unscoped owner-shaped fold (a plain LEFT JOIN of run_usage_model_totals with no run-id
// correlation, which is what the lateral fence in AnalyticsOwnerSpend exists to prevent)
// must produce the scoped run_usage offender, i.e. a plan that folds other users' usage.
func TestAnalyticsSpendGenericPlanNegativeControlLiveDB(t *testing.T) {
	e := newGenericPlanEnv(t)
	const unscoped = `SELECT r.id, m.model, m.cost_usd
	FROM runs r
	LEFT JOIN run_usage_model_totals m ON m.run_id = r.id
	WHERE r.user_id = $1::uuid`
	c := genericPlanCase{name: "UnscopedOwnerFold", sql: unscoped, scoped: true, spend: true, ownLegs: e.spendOwnLegs}
	e.exec(t, "PREPARE usage_plan_control AS "+unscoped)
	doc := e.explain(t, c.name, "usage_plan_control", "'"+e.spendOwnerID.String()+"'::uuid")
	offenders := e.planOffenders(c, doc)
	t.Logf("negative control plan:\n%s", renderPlan(doc, 0))
	var found bool
	for _, o := range offenders {
		if strings.Contains(o, "on run_usage") && strings.Contains(o, "folds other users' usage") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the unscoped owner-shaped fold did not trip the scoped run_usage offender (offenders: %v); the Spend plan check cannot fail", offenders)
	}
}

// TestAnalyticsSpendWitnessNegativeControlLiveDB proves the witness guard can fail: the
// previous EXISTS form of the merged-PR witness, put back into the real owner statement (the
// generated text with only that one expression swapped), must be reported by witnessProblems
// as an un-hashed read of merged_witness.
func TestAnalyticsSpendWitnessNegativeControlLiveDB(t *testing.T) {
	e := newGenericPlanEnv(t)
	const inForm = "(c.repo_id, c.mr_iid) IN (SELECT mw.repo_id, mw.mr_iid FROM merged_witness mw)"
	const existsForm = "EXISTS (SELECT 1 FROM merged_witness mw WHERE mw.repo_id = c.repo_id AND mw.mr_iid = c.mr_iid)"
	if strings.Count(store.AnalyticsOwnerSpendSQL, inForm) != 1 {
		t.Fatalf("the owner statement no longer carries the witness IN form exactly once; update this control")
	}
	control := strings.Replace(store.AnalyticsOwnerSpendSQL, inForm, existsForm, 1)
	args := e.spendArgs(t, e.spendOwnerID, false, false)
	e.exec(t, "PREPARE usage_plan_witness_control AS "+control)
	doc := e.explain(t, "ExistsWitness", "usage_plan_witness_control", args[0])
	t.Logf("witness negative control plan:\n%s", renderPlan(doc, 0))
	problems, _ := witnessProblems(doc)
	var found bool
	for _, p := range problems {
		if strings.Contains(p, "un-hashed") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the correlated EXISTS witness did not trip the un-hashed offender (problems: %v); the witness guard cannot fail", problems)
	}
}

// planNode is the subset of EXPLAIN (ANALYZE, FORMAT JSON) this test reads. InitPlans and
// SubPlans appear as ordinary children in "Plans", so walking Plans covers them.
type planNode struct {
	NodeType            string     `json:"Node Type"`
	RelationName        string     `json:"Relation Name"`
	Alias               string     `json:"Alias"`
	IndexName           string     `json:"Index Name"`
	ActualRows          float64    `json:"Actual Rows"`
	ActualLoops         float64    `json:"Actual Loops"`
	RowsRemovedByFilter float64    `json:"Rows Removed by Filter"`
	RowsRemovedByJoin   float64    `json:"Rows Removed by Join Filter"`
	SubplanName         string     `json:"Subplan Name"`
	ParentRelationship  string     `json:"Parent Relationship"`
	CTEName             string     `json:"CTE Name"`
	Filter              string     `json:"Filter"`
	JoinFilter          string     `json:"Join Filter"`
	Output              []string   `json:"Output"`
	Plans               []planNode `json:"Plans"`
}

// loopOffenders reports every plan node that looped more than once for more than
// 4 × count(runs) rows of work (counting the rows it emitted plus the rows its Filter and Join
// Filter discarded, CTE Scans included), and every join that discarded more than that by its
// Join Filter.
func (e *genericPlanEnv) loopOffenders(root *planNode, counts func(*planNode) bool, joinDiscards bool) []string {
	limit := 4 * float64(e.runsTotal)
	var out []string
	var loops func(n *planNode)
	loops = func(n *planNode) {
		if work := (n.ActualRows + n.RowsRemovedByFilter + n.RowsRemovedByJoin) * n.ActualLoops; n.ActualLoops > 1 && work > limit && counts(n) {
			out = append(out, fmt.Sprintf("%s %s%s looped %.0f times for %.0f rows of work (> 4 × count(runs) = %.0f): a per-row re-scan or a runs × groups join",
				n.NodeType, n.CTEName, n.RelationName, n.ActualLoops, work, limit))
		}
		if joinDiscards && n.RowsRemovedByJoin > limit {
			out = append(out, fmt.Sprintf("%s discarded %.0f rows by its Join Filter (> 4 × count(runs) = %.0f): a cross product filtered after the join",
				n.NodeType, n.RowsRemovedByJoin, limit))
		}
		for i := range n.Plans {
			loops(&n.Plans[i])
		}
	}
	loops(root)
	return out
}

// spendOffenders holds the Spend-only plan conditions (issue #4684) that work on every node,
// whether or not it scans run_usage:
//   - a node that looped more than once may not do more than 4 × count(runs) work in total,
//     counting the rows it emitted plus the rows its Filter and Join Filter discarded, CTE
//     Scans included (a materialized CTE re-scanned once per outer row shows up as a CTE
//     Scan with loops > 1, which a runs-relation-only check cannot see); a join that
//     discarded more than that by its Join Filter is an offender whatever its loops;
//   - every scan of the merged_witness CTE is a hashed SubPlan or the build side of a hash
//     join, never a correlated SubPlan that re-scans the CTE per cohort row; and at least
//     one such hashed read exists, so the check is not vacuous.
func (e *genericPlanEnv) spendOffenders(root *planNode) []string {
	out := e.loopOffenders(root, func(*planNode) bool { return true }, true)
	problems, hashed := witnessProblems(root)
	out = append(out, problems...)
	if hashed == 0 {
		out = append(out, "no hashed read of merged_witness found in the plan: the witness check is vacuous or the plan shape changed")
	}
	return out
}

// hashedSubPlanRef matches how EXPLAIN VERBOSE prints a hashed SubPlan where an expression
// uses it. The node's own "Subplan Name" is just "SubPlan N" whether or not it is hashed;
// only the expression that references it says "hashed".
var hashedSubPlanRef = regexp.MustCompile(`hashed (SubPlan \d+)`)

// hashedSubPlanNames returns the SubPlans that some expression in the plan (Output, Filter or
// Join Filter) references as "hashed SubPlan N".
func hashedSubPlanNames(root *planNode) map[string]bool {
	hashedSubPlans := map[string]bool{}
	var collect func(n *planNode)
	collect = func(n *planNode) {
		texts := append([]string{n.Filter, n.JoinFilter}, n.Output...)
		for _, txt := range texts {
			for _, m := range hashedSubPlanRef.FindAllStringSubmatch(txt, -1) {
				hashedSubPlans[m[1]] = true
			}
		}
		for i := range n.Plans {
			collect(&n.Plans[i])
		}
	}
	collect(root)
	return hashedSubPlans
}

// witnessProblems reports each scan of the merged_witness CTE that is neither inside a
// hashed SubPlan nor under a Hash node, and counts the hashed ones. A SubPlan is hashed when
// some expression in the plan (Output, Filter or Join Filter) references it as
// "hashed SubPlan N".
func witnessProblems(root *planNode) (problems []string, hashed int) {
	hashedSubPlans := hashedSubPlanNames(root)
	var visit func(n *planNode, sub *planNode, underHash bool)
	visit = func(n *planNode, sub *planNode, underHash bool) {
		if n.ParentRelationship == "SubPlan" {
			sub = n
		}
		if n.NodeType == "Hash" {
			underHash = true
		}
		if n.NodeType == "CTE Scan" && n.CTEName == "merged_witness" {
			switch {
			case sub != nil && !hashedSubPlans[sub.SubplanName]:
				problems = append(problems, fmt.Sprintf("merged_witness is read by the un-hashed %q (CTE Scan loops %.0f): a correlated per-row re-scan", sub.SubplanName, n.ActualLoops))
			case sub != nil || underHash:
				hashed++
			default:
				problems = append(problems, "merged_witness is scanned outside a hashed SubPlan or a Hash build side")
			}
		}
		for i := range n.Plans {
			visit(&n.Plans[i], sub, underHash)
		}
	}
	visit(root, nil, false)
	return problems, hashed
}

// cteSubPlanReport walks the plan once and reports (a) every CTE Scan that sits inside a
// SubPlan which no expression references as hashed (an unhashed per-row SubPlan over a CTE,
// which re-scans it once per outer row and degrades quadratically once the CTE outgrows
// hash_mem), (b) how many CTE Scans sit inside a hashed SubPlan, per CTE name, and (c) how many
// CTE Scans of each name sit outside any SubPlan. The plan may be EXPLAIN with or without ANALYZE.
func cteSubPlanReport(root *planNode) (unhashed []string, hashedByCTE, outsideByCTE map[string]int) {
	hashedSubPlans := hashedSubPlanNames(root)
	hashedByCTE, outsideByCTE = map[string]int{}, map[string]int{}
	var visit func(n *planNode, sub *planNode)
	visit = func(n *planNode, sub *planNode) {
		if n.ParentRelationship == "SubPlan" {
			sub = n
		}
		if n.NodeType == "CTE Scan" {
			switch {
			case sub == nil:
				outsideByCTE[n.CTEName]++
			case hashedSubPlans[sub.SubplanName]:
				hashedByCTE[n.CTEName]++
			default:
				unhashed = append(unhashed, fmt.Sprintf("CTE Scan on %s is read by the un-hashed %q: a per-row SubPlan (loops %.0f)", n.CTEName, sub.SubplanName, n.ActualLoops))
			}
		}
		for i := range n.Plans {
			visit(&n.Plans[i], sub)
		}
	}
	visit(root, nil)
	return unhashed, hashedByCTE, outsideByCTE
}

// qualityShapeProblems holds the issue #4730 conditions that need no row counts, so they also
// hold for a plain EXPLAIN: no unhashed SubPlan over any CTE Scan, no nested loop whose Join
// Filter is the (repo_id, mr_iid) pair, the merge witness (rw_witness) folded in by a plain scan
// outside any SubPlan, and the rework-link membership (rw_sel) still a hashed SubPlan.
func qualityShapeProblems(root *planNode) []string {
	unhashed, hashedByCTE, outsideByCTE := cteSubPlanReport(root)
	out := append([]string{}, unhashed...)
	var walk func(n *planNode)
	walk = func(n *planNode) {
		if n.NodeType == "Nested Loop" && strings.Contains(n.JoinFilter, "mr_iid") {
			out = append(out, fmt.Sprintf("Nested Loop with Join Filter on the (repo_id, mr_iid) pair: %s", n.JoinFilter))
		}
		for i := range n.Plans {
			walk(&n.Plans[i])
		}
	}
	walk(root)
	if outsideByCTE["rw_witness"] == 0 {
		out = append(out, "rw_witness is not read by a plain scan outside a SubPlan: the witness fold changed shape or the check is vacuous")
	}
	if hashedByCTE["rw_witness"] != 0 {
		out = append(out, "rw_witness is read under a hashed SubPlan: the witness lookup is a membership SubPlan again")
	}
	if hashedByCTE["rw_sel"] == 0 {
		out = append(out, "no hashed SubPlan over rw_sel: the rework-link membership changed shape or the check is vacuous")
	}
	return out
}

// qualityExecutedProblems needs an EXPLAIN ANALYZE plan: it requires that at least one CTE Scan of
// rw_sel under a hashed SubPlan actually returned rows. Otherwise the rework-link membership was
// never exercised (an empty rw_sel or rework set in the seed, or a predicate that empties it), and
// the shape and row-count checks pass vacuously. Scans of rw_sel outside the SubPlan do not count.
func qualityExecutedProblems(root *planNode) []string {
	hashedSubPlans := hashedSubPlanNames(root)
	executed := false
	var visit func(n *planNode, sub *planNode)
	visit = func(n *planNode, sub *planNode) {
		if n.ParentRelationship == "SubPlan" {
			sub = n
		}
		if n.NodeType == "CTE Scan" && n.CTEName == "rw_sel" && sub != nil &&
			hashedSubPlans[sub.SubplanName] && n.ActualRows*n.ActualLoops > 0 {
			executed = true
		}
		for i := range n.Plans {
			visit(&n.Plans[i], sub)
		}
	}
	visit(root, nil)
	if executed {
		return nil
	}
	return []string{qualityNoExecutedRwSel}
}

// qualityNoExecutedRwSel is qualityExecutedProblems' one diagnostic.
const qualityNoExecutedRwSel = "no CTE Scan on rw_sel under a hashed SubPlan returned rows: the seed must give both rw_sel and rework a non-empty set, or the membership check is vacuous"

// qualityOffenders is the generic-plan check of the Quality statements: the row-count classes
// shared with Spend (loopOffenders) plus qualityShapeProblems and qualityExecutedProblems.
//
// The looped-work bound counts CTE Scans and scans of runs only, and the join-discard bound is
// not applied: a per-row probe of a small side table (the judge_skips EXISTS, a nested loop
// against repos) is a Seq Scan on a freshly seeded table and an index probe or hash join on a
// large one, so its total work tracks that table's statistics, not the statement's shape. The
// (repo_id, mr_iid) nested-loop class is checked separately by qualityShapeProblems.
func (e *genericPlanEnv) qualityOffenders(root *planNode) []string {
	counted := func(n *planNode) bool { return n.CTEName != "" || n.RelationName == "runs" }
	out := append(e.loopOffenders(root, counted, false), qualityShapeProblems(root)...)
	return append(out, qualityExecutedProblems(root)...)
}

// nodeWork is a node's total row work across all its loops: the rows it emitted, plus,
// for a scan node (one with a Relation Name), the rows it read and discarded by its
// Filter. Without the second term a per-outer-row Seq Scan of run_usage filtered down to
// that row's legs would emit only a few rows per loop and pass, while reading the whole
// table every loop. It also returns how many per-loop rounded counts the total sums, for
// the rounding tolerance.
func nodeWork(n *planNode) (total, roundedCounts float64) {
	total, roundedCounts = n.ActualRows*n.ActualLoops, 1
	if n.RelationName != "" {
		total += n.RowsRemovedByFilter * n.ActualLoops
		roundedCounts++
	}
	return total, roundedCounts
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
	if n.CTEName != "" {
		fmt.Fprintf(&b, " on CTE %s", n.CTEName)
	}
	if n.SubplanName != "" {
		fmt.Fprintf(&b, " [%s %s]", n.ParentRelationship, n.SubplanName)
	}
	if n.IndexName != "" {
		fmt.Fprintf(&b, " using %s", n.IndexName)
	}
	fmt.Fprintf(&b, " (rows=%.0f loops=%.0f", n.ActualRows, n.ActualLoops)
	if n.RowsRemovedByFilter > 0 {
		fmt.Fprintf(&b, " removed_by_filter=%.0f", n.RowsRemovedByFilter)
	}
	b.WriteString(")\n")
	for i := range n.Plans {
		b.WriteString(renderPlan(&n.Plans[i], depth+1))
	}
	return b.String()
}

// genericPlanLatest is the latest-failure seed of issue #4685: per scope one eligible failure
// with completed runs before it, exactly at its terminal instant, and after it.
type genericPlanLatest struct {
	ownerID                    uuid.UUID
	ownerRun, allRun, allOwner uuid.UUID
	ownerSince, allSince       int64
	ownerFailedAt, allFailedAt time.Time
}

// seedGenericPlanLatestFailure seeds two users in the year 2098, which no other test uses, after
// deleting what an earlier call left (they are matched by the genericplan-lf email prefix):
//
//	owner L: completed at 08:00 and exactly at the failure's instant (neither counts), a failed
//	         run at 2098-01-01 10:00, then completed at 11:00 and 12:00 (both count): since 2;
//	user M:  a newer failure at 2098-01-02 10:00 (the instance-wide latest), completed exactly at
//	         that instant (not counted) and at 11:00, 12:00, 13:00 (counted); L's runs all end
//	         before it: since 3.
func seedGenericPlanLatestFailure(ctx context.Context, t *testing.T, pool *pgxpool.Pool) genericPlanLatest {
	t.Helper()
	del := func() {
		for _, q := range []string{
			`DELETE FROM runs WHERE user_id IN (SELECT id FROM users WHERE email LIKE 'genericplan-lf-%@e2e')`,
			`DELETE FROM users WHERE email LIKE 'genericplan-lf-%@e2e'`,
		} {
			if _, err := pool.Exec(context.Background(), q); err != nil {
				t.Fatalf("clear latest-failure seed: %v", err)
			}
		}
	}
	del()
	t.Cleanup(del)
	mkUser := func(tag string) (user, repo uuid.UUID) {
		user, conn, repo := uuid.New(), uuid.New(), uuid.New()
		mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, user, fmt.Sprintf("genericplan-lf-%s-%s@e2e", tag, user))
		mustExec(ctx, t, pool,
			`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
			 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, conn, user, []byte{0x1})
		mustExec(ctx, t, pool,
			`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
			 VALUES ($1, $2, 1, $3, 'https://forge.e2e/g/lf', 'main', true)`, repo, conn, fmt.Sprintf("g/genericplan-lf-%s", repo))
		return user, repo
	}
	var iid int64 = 5_000_000
	ins := func(user, repo uuid.UUID, status, origin string, finished time.Time) uuid.UUID {
		iid++
		id := uuid.New()
		var fo any
		if origin != "" {
			fo = origin
		}
		mustExec(ctx, t, pool,
			`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind, created_at, finished_at, status_since, fail_origin)
			 VALUES ($1, $2, $3, $4, 't', 'd', $5, 'issue', $6, $6, $6, $7)`, id, user, repo, iid, status, finished, fo)
		return id
	}
	day := func(d, h int) time.Time { return time.Date(2098, 1, d, h, 0, 0, 0, time.UTC) }
	var l genericPlanLatest
	owner, ownerRepo := mkUser("owner")
	ins(owner, ownerRepo, "completed", "", day(1, 8))
	l.ownerRun = ins(owner, ownerRepo, "failed", "agent_failure", day(1, 10))
	ins(owner, ownerRepo, "completed", "", day(1, 10))
	ins(owner, ownerRepo, "completed", "", day(1, 11))
	ins(owner, ownerRepo, "completed", "", day(1, 12))
	m, mRepo := mkUser("other")
	l.allRun = ins(m, mRepo, "failed", "run_timeout", day(2, 10))
	ins(m, mRepo, "completed", "", day(2, 10))
	ins(m, mRepo, "completed", "", day(2, 11))
	ins(m, mRepo, "completed", "", day(2, 12))
	ins(m, mRepo, "completed", "", day(2, 13))
	l.ownerID, l.allOwner = owner, m
	l.ownerSince, l.allSince = 2, 3
	l.ownerFailedAt, l.allFailedAt = day(1, 10), day(2, 10)
	mustExec(ctx, t, pool, `ANALYZE runs`)
	return l
}

// seedGenericPlanReliability seeds the Reliability generic-plan owner: 300 runs spread over ~37
// days across 24 repos (so the repo joins and the per-repo breakdown have many groups), a
// fifth of them failed with the human-landable origin push_secret_blocked (half of those with an
// available recovery capture of the same user, so the needs_landing EXISTS executes), some
// agent_failure failures for the causes list, and four usage legs per run over 7 models. Its own
// cleanup removes exactly these rows, and runs before the test's snapshot connection is rolled back.
func seedGenericPlanReliability(ctx context.Context, t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	owner, conn := uuid.New(), uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		owner, fmt.Sprintf("genericplan-rel-%s@e2e", owner))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, conn, owner, []byte{0x1})
	t.Cleanup(func() {
		for _, c := range []struct {
			q   string
			arg uuid.UUID
		}{
			{`DELETE FROM recovery_captures WHERE user_id = $1`, owner},
			{`DELETE FROM recovery_custody_holds WHERE user_id = $1`, owner},
			{`DELETE FROM run_usage WHERE run_id IN (SELECT id FROM runs WHERE user_id = $1)`, owner},
			{`DELETE FROM runs WHERE user_id = $1`, owner},
			{`DELETE FROM repos WHERE connection_id = $1`, conn},
			{`DELETE FROM forge_connections WHERE id = $1`, conn},
			{`DELETE FROM users WHERE id = $1`, owner},
		} {
			if _, err := pool.Exec(context.Background(), c.q, c.arg); err != nil {
				t.Errorf("cleanup reliability seed %q: %v", c.q, err)
			}
		}
	})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 SELECT gen_random_uuid(), $1, g, 'g/genericplan-rel-' || lpad(g::text, 2, '0'), 'https://forge.e2e/g/rel-' || g, 'main', true
		 FROM generate_series(1, 24) g`, conn)
	mustExec(ctx, t, pool,
		`WITH rp AS (
		     SELECT id, row_number() OVER (ORDER BY path_with_namespace) - 1 AS n FROM repos WHERE connection_id = $2
		 )
		 INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind,
		                   created_at, finished_at, status_since, fail_origin)
		 SELECT gen_random_uuid(), $1, rp.id, g, 't', 'd',
		        CASE WHEN g % 5 = 0 OR g % 7 = 0 THEN 'failed' ELSE 'completed' END, 'issue',
		        now() - make_interval(hours => g * 3), now() - make_interval(hours => g * 3),
		        now() - make_interval(hours => g * 3),
		        CASE WHEN g % 5 = 0 THEN 'push_secret_blocked' WHEN g % 7 = 0 THEN 'agent_failure' END
		 FROM generate_series(1, 300) g JOIN rp ON rp.n = g % 24`, owner, conn)
	mustExec(ctx, t, pool,
		`INSERT INTO run_usage (run_id, session_id, model, lineage_epoch, input_tokens, cache_read_tokens,
		                        cache_creation_tokens, output_tokens, cost_usd, harness, cost_status, usage_basis)
		 SELECT r.id, 'sess-' || e, 'rel-model-' || ((r.issue_iid + k * 3) % 7), e,
		        100 + e, 10, 5, 50, 0.001, 'claude', 'metered', 'per_leg'
		 FROM runs r
		 CROSS JOIN generate_series(0, 1) AS k
		 CROSS JOIN generate_series(0, 1) AS e
		 WHERE r.user_id = $1`, owner)
	mustExec(ctx, t, pool,
		`WITH h AS (
		     INSERT INTO recovery_custody_holds (id, user_id, run_id, generation, state, original_worker_id, original_worker_identity)
		     SELECT gen_random_uuid(), user_id, id, 1, 'released', gen_random_uuid(), 'ident'
		     FROM runs WHERE user_id = $1 AND fail_origin = 'push_secret_blocked' AND issue_iid % 2 = 0
		     RETURNING id, run_id
		 )
		 INSERT INTO recovery_captures (id, hold_id, run_id, user_id, original_worker_identity, source_sha, idempotency_key, state, expires_at)
		 SELECT gen_random_uuid(), h.id, h.run_id, $1, 'ident', 'aaaa1111', h.id::text, 'available', now() + interval '1 hour'
		 FROM h`, owner)
	var withCapture, repos int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM recovery_captures WHERE user_id = $1),
	                                     (SELECT count(DISTINCT repo_id) FROM runs WHERE user_id = $1)`, owner).Scan(&withCapture, &repos); err != nil {
		t.Fatalf("count reliability seed: %v", err)
	}
	if withCapture == 0 || repos < 20 {
		t.Fatalf("reliability seed has %d captures over %d repos; the EXISTS and repo joins would not execute", withCapture, repos)
	}
	mustExec(ctx, t, pool, `ANALYZE runs`)
	mustExec(ctx, t, pool, `ANALYZE run_usage`)
	return owner
}

// dropBaseSeed deletes exactly the rows newGenericPlanEnv inserted for this env (its named users,
// the noise users tagged with e.userID, their runs, usage, judge review, repos and connections),
// so a test that builds its own env does not grow the shared database. It is registered after
// the env's own cleanups, so it runs first, while the snapshot connection only holds reads.
func (e *genericPlanEnv) dropBaseSeed(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		const users = `(SELECT id FROM users WHERE id = ANY($1) OR email LIKE 'genericplan-noise-%-' || $2 || '@e2e')`
		for _, q := range []string{
			`DELETE FROM run_reviews WHERE user_id IN ` + users,
			`DELETE FROM run_usage WHERE run_id IN (SELECT id FROM runs WHERE user_id IN ` + users + `)`,
			`DELETE FROM runs WHERE user_id IN ` + users,
			`DELETE FROM repos WHERE connection_id IN (SELECT id FROM forge_connections WHERE user_id IN ` + users + `)`,
			`DELETE FROM forge_connections WHERE user_id IN ` + users,
			`DELETE FROM users WHERE id IN ` + users,
		} {
			if _, err := e.pool.Exec(ctx, q, e.seededUsers, e.userID.String()); err != nil {
				t.Errorf("cleanup base seed %q: %v", q, err)
			}
		}
	})
}

// reliabilityArgs renders the EXECUTE arguments of a Reliability statement for a 30d window
// ending at the transaction's now(). The argument order is read from what the generated Go
// method actually passes (a capturing DBTX), never assumed.
func (e *genericPlanEnv) reliabilityArgs(t *testing.T, owner uuid.UUID, breakdown string) string {
	t.Helper()
	var now time.Time
	if err := e.conn.QueryRow(e.ctx, `SELECT now()`, pgx.QueryExecModeSimpleProtocol).Scan(&now); err != nil {
		t.Fatalf("select now(): %v", err)
	}
	b, err := analytics.ComputeBounds(now, time.UTC, analytics.Window30d, nil)
	if err != nil {
		t.Fatalf("ComputeBounds: %v", err)
	}
	ts := func(v time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: v, Valid: true} }
	tss := func(vs []time.Time) []pgtype.Timestamptz {
		out := make([]pgtype.Timestamptz, len(vs))
		for i, v := range vs {
			out[i] = ts(v)
		}
		return out
	}
	var prev pgtype.Timestamptz
	if b.PreviousFrom != nil {
		prev = ts(*b.PreviousFrom)
	}
	capture := &capturingDBTX{}
	cq := store.New(capture)
	landable := []string{"workflow_scope_missing", "push_secret_blocked"}
	if owner != uuid.Nil {
		_, _ = cq.AnalyticsOwnerReliability(e.ctx, store.AnalyticsOwnerReliabilityParams{
			UserID: owner, Now: ts(b.Now), From: ts(b.From), PreviousFrom: prev, FoldFrom: ts(b.FoldFrom),
			BucketStarts: tss(b.BucketStarts), BucketEnds: tss(b.BucketEnds), RollStarts: tss(b.RollStarts), RollEnds: tss(b.RollEnds),
			RollHarnesses: []string{"claude", "codex"}, LandableOrigins: landable,
			Kinds: runkind.Delivery(), Breakdown: breakdown,
		})
	} else {
		_, _ = cq.AnalyticsAllReliability(e.ctx, store.AnalyticsAllReliabilityParams{
			Now: ts(b.Now), From: ts(b.From), PreviousFrom: prev, FoldFrom: ts(b.FoldFrom),
			BucketStarts: tss(b.BucketStarts), BucketEnds: tss(b.BucketEnds), RollStarts: tss(b.RollStarts), RollEnds: tss(b.RollEnds),
			RollHarnesses: []string{"claude", "codex"}, LandableOrigins: landable,
			Kinds: runkind.Delivery(), Breakdown: breakdown,
		})
	}
	lits := make([]string, len(capture.args))
	for i, a := range capture.args {
		lits[i] = sqlLiteral(t, a)
	}
	return strings.Join(lits, ", ")
}

// TestAnalyticsReliabilityGenericPlanLiveDB is the issue #1620 plan-shape check applied to the
// Reliability statements: under force_generic_plan ONE prepared statement per scope is executed
// with breakdown=primary_model (the Primary-model fold reads run_usage: positive work, inside
// the global 4 x count(run_usage) bound and, for the owner, inside the owner's own legs) and
// then with breakdown=harness (zero executed run_usage scans: the parameter gate must keep the
// fold out of the plan). The seed holds usage legs of other users, so an unscoped fold shows.
func TestAnalyticsReliabilityGenericPlanLiveDB(t *testing.T) {
	e := newGenericPlanEnvWith(t, true)
	e.dropBaseSeed(t)
	if e.relOwnLegs == 0 || e.relOwnLegs >= e.spendDenominator {
		t.Fatalf("foreign usage missing: owner legs %d, table %d", e.relOwnLegs, e.spendDenominator)
	}
	cases := []struct {
		name   string
		sql    string
		owner  uuid.UUID
		scoped bool
	}{
		{"AnalyticsOwnerReliability", store.AnalyticsOwnerReliabilitySQL, e.relOwnerID, true},
		{"AnalyticsAllReliability", store.AnalyticsAllReliabilitySQL, uuid.Nil, false},
	}
	for i, c := range cases {
		stmt := fmt.Sprintf("reliability_plan_%d", i)
		e.exec(t, fmt.Sprintf("PREPARE %s AS %s", stmt, c.sql))
		pc := genericPlanCase{name: c.name, sql: c.sql, scoped: c.scoped, spend: true, ownLegs: e.relOwnLegs}

		doc := e.explain(t, c.name+"[primary_model]", stmt, e.reliabilityArgs(t, c.owner, "primary_model"))
		t.Logf("%s[primary_model] generic plan:\n%s", c.name, renderPlan(doc, 0))
		if offenders := e.planOffenders(pc, doc); len(offenders) > 0 {
			t.Errorf("%s[primary_model]: run_usage work exceeds its bound (4 x %d, own legs %d):\n  %s", c.name, e.spendDenominator, e.relOwnLegs, strings.Join(offenders, "\n  "))
		}
		var usageWork, usageSum, slack float64
		for _, n := range collectScans(doc, "run_usage") {
			total, rounded := nodeWork(n)
			usageWork += n.ActualRows * n.ActualLoops
			usageSum += total
			slack += rounded * n.ActualLoops / 2
		}
		if usageWork == 0 {
			t.Errorf("%s[primary_model]: no run_usage row was read; the seed or the window is empty and the bounds prove nothing", c.name)
		}
		if !c.scoped && usageSum > float64(e.spendDenominator)+slack {
			t.Errorf("%s[primary_model]: run_usage scans did %.0f rows of work, more than one fold of the table (%d, slack %.0f)", c.name, usageSum, e.spendDenominator, slack)
		}
		e.reliabilityLoopOffenders(t, c.name+"[primary_model]", doc)

		doc = e.explain(t, c.name+"[harness]", stmt, e.reliabilityArgs(t, c.owner, "harness"))
		t.Logf("%s[harness] generic plan:\n%s", c.name, renderPlan(doc, 0))
		for _, n := range collectScans(doc, "run_usage") {
			if n.ActualLoops > 0 {
				t.Errorf("%s[harness]: %s on run_usage executed (loops %.0f, rows %.0f): the usage fold must not run when the breakdown is not primary_model", c.name, n.NodeType, n.ActualLoops, n.ActualRows)
			}
		}
		e.reliabilityLoopOffenders(t, c.name+"[harness]", doc)
		e.exec(t, "DEALLOCATE "+stmt)
	}
}

// reliabilityLoopOffenders reports a per-cohort-row re-scan: a scan of runs or run_usage, or a
// CTE scan of a run-derived CTE (cohort, run_fold, rfb, the breakdown and count CTEs), that
// looped more than once for more than 4 x count(runs) rows of work. The rolling-range join
// (series x ranges x segments, a few thousand rows whatever the data) reads only the segment
// CTE, so it is deliberately not a run-derived scan.
func (e *genericPlanEnv) reliabilityLoopOffenders(t *testing.T, name string, root *planNode) {
	t.Helper()
	limit := 4 * float64(e.runsTotal)
	runDerived := map[string]bool{"cohort": true, "run_fold": true, "rfb": true, "bd_items": true, "bd_ranked": true, "fan": true, "cnt": true}
	var visit func(n *planNode)
	visit = func(n *planNode) {
		derived := n.RelationName == "runs" || n.RelationName == "run_usage" || runDerived[n.CTEName]
		if work := (n.ActualRows + n.RowsRemovedByFilter) * n.ActualLoops; derived && n.ActualLoops > 1 && work > limit {
			t.Errorf("%s: %s %s%s looped %.0f times for %.0f rows of work (> 4 x count(runs) = %.0f): a per-row re-scan", name, n.NodeType, n.CTEName, n.RelationName, n.ActualLoops, work, limit)
		}
		for i := range n.Plans {
			visit(&n.Plans[i])
		}
	}
	visit(root)
}

// collectScans returns every plan node that scans the named relation.
func collectScans(root *planNode, relation string) []*planNode {
	var out []*planNode
	var visit func(n *planNode)
	visit = func(n *planNode) {
		if n.RelationName == relation {
			out = append(out, n)
		}
		for i := range n.Plans {
			visit(&n.Plans[i])
		}
	}
	visit(root)
	return out
}

// TestAnalyticsLatestFailureGenericPlanLiveDB runs the two latest-failure statements as
// generic prepared plans against a seed with an earlier and a later matching completion per
// scope: the statement must return the exact run and the exact positive completed-since count,
// and the plan must have executed the count's scan of runs (alias r3), not skipped it.
func TestAnalyticsLatestFailureGenericPlanLiveDB(t *testing.T) {
	e := newGenericPlanEnv(t)
	e.dropBaseSeed(t)
	l := e.latest
	cases := []struct {
		name      string
		sql       string
		args      func(t *testing.T) string
		run, user uuid.UUID
		failedAt  time.Time
		since     int64
	}{
		{"AnalyticsOwnerLatestFailure", store.AnalyticsOwnerLatestFailureSQL, func(t *testing.T) string {
			return e.latestArgs(t, l.ownerID)
		}, l.ownerRun, l.ownerID, l.ownerFailedAt, l.ownerSince},
		{"AnalyticsAllLatestFailure", store.AnalyticsAllLatestFailureSQL, func(t *testing.T) string {
			return e.latestArgs(t, uuid.Nil)
		}, l.allRun, l.allOwner, l.allFailedAt, l.allSince},
	}
	for i, c := range cases {
		stmt := fmt.Sprintf("latest_failure_plan_%d", i)
		e.exec(t, fmt.Sprintf("PREPARE %s AS %s", stmt, c.sql))
		args := c.args(t)

		var found bool
		var failedAt time.Time
		var origin string
		var runID, userID uuid.UUID
		var since int64
		if err := e.conn.QueryRow(e.ctx, fmt.Sprintf("EXECUTE %s(%s)", stmt, args), pgx.QueryExecModeSimpleProtocol).
			Scan(&found, &failedAt, &origin, &runID, &userID, &since); err != nil {
			t.Fatalf("%s: EXECUTE: %v", c.name, err)
		}
		if !found || runID != c.run || userID != c.user || !failedAt.Equal(c.failedAt) || since != c.since || since <= 0 {
			t.Errorf("%s: got found=%v run=%s user=%s at=%s since=%d, want run %s user %s at %s since %d",
				c.name, found, runID, userID, failedAt, since, c.run, c.user, c.failedAt, c.since)
		}
		doc := e.explain(t, c.name, stmt, args)
		t.Logf("%s generic plan:\n%s", c.name, renderPlan(doc, 0))
		var executed bool
		var visit func(n *planNode)
		visit = func(n *planNode) {
			if n.RelationName == "runs" && n.Alias == "r3" && n.ActualLoops >= 1 {
				executed = true
			}
			for i := range n.Plans {
				visit(&n.Plans[i])
			}
		}
		visit(doc)
		if !executed {
			t.Errorf("%s: the completed-since count's scan of runs (alias r3) did not execute under the generic plan", c.name)
		}
		e.exec(t, "DEALLOCATE "+stmt)
	}
}

// latestArgs renders the EXECUTE arguments of a latest-failure statement (no repo or harness
// filter, the Delivery kinds), reading the argument order from the generated method.
func (e *genericPlanEnv) latestArgs(t *testing.T, owner uuid.UUID) string {
	t.Helper()
	capture := &capturingDBTX{}
	cq := store.New(capture)
	if owner != uuid.Nil {
		_, _ = cq.AnalyticsOwnerLatestFailure(e.ctx, store.AnalyticsOwnerLatestFailureParams{UserID: owner, Kinds: runkind.Delivery()})
	} else {
		_, _ = cq.AnalyticsAllLatestFailure(e.ctx, store.AnalyticsAllLatestFailureParams{Kinds: runkind.Delivery()})
	}
	lits := make([]string, len(capture.args))
	for i, a := range capture.args {
		lits[i] = sqlLiteral(t, a)
	}
	return strings.Join(lits, ", ")
}
