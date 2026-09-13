package workersvc

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// findAdminGroup locates a (category, target) coordinate in an admin backlog's groups.
func findAdminGroup(groups []apitypes.JudgeAdminGroupDTO, category, target string) (int, bool) {
	for i, g := range groups {
		if g.Category == category && g.Target == target {
			return i, true
		}
	}
	return 0, false
}

// TestAdminJudgeAggregateLiveDB is PRD #1184 M1's admin "All users" aggregate read against real
// Postgres. It pins what a fake store cannot: the ListJudgeRecommendationRowsAll SQL has NO user
// predicate (so two DIFFERENT owners' recommendations on the SAME coordinate dedup into one
// group with user_count 2), its projection hides attribution (no owner, run id/title, review/rec
// id, filed iid/url reaches the response), and the LIMIT cap binds in SQL so a >cap backlog is
// truncated.
//
// The store-IT runner shares ONE database across the whole LiveDB set, so the ALL-users query
// sees every other test's rows too. Every counting assertion is therefore scoped to a coordinate
// whose target carries a fresh uuid — no other test can collide with it — and the group is looked
// up by that target rather than by position.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; run via
// ./e2e/run-store-it.sh. A package that prints `ok` with PASS=0 is INVALID, not green.
func TestAdminJudgeAggregateLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh for live-DB coverage")
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
	svc := New(q, newBox(t), testParams())

	mustExec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}

	// A fresh owner with its own forge connection + repo; every key is a fresh uuid since the DB
	// is shared. Returns the owner id.
	iid := int64(0)
	seedOwner := func(tag string) (userID, repoID uuid.UUID) {
		userID, connID, repoID := uuid.New(), uuid.New(), uuid.New()
		mustExec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
			userID, fmt.Sprintf("adminjudge-%s-%s@e2e", tag, userID))
		mustExec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
			 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
		mustExec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
			 VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, repoID, connID)
		return userID, repoID
	}

	// judgedRun creates a completed run owned by userID/repoID, its review, and one recommendation
	// per coordinate. Returns the run id and the review id.
	judgedRun := func(userID, repoID uuid.UUID, title, verdict string, coords ...[2]string) (uuid.UUID, uuid.UUID) {
		runID, reviewID := uuid.New(), uuid.New()
		iid++
		mustExec(`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status)
			 VALUES ($1, $2, $3, $4, $5, 'd', 'completed')`, runID, userID, repoID, iid, title)
		mustExec(`INSERT INTO run_reviews (id, target_run_id, user_id, verdict) VALUES ($1, $2, $3, $4)`,
			reviewID, runID, userID, verdict)
		for _, c := range coords {
			mustExec(`INSERT INTO review_recommendations (review_id, category, target, rationale_md)
				 VALUES ($1, $2, $3, 'because')`, reviewID, c[0], c[1])
		}
		return runID, reviewID
	}

	// Coordinate targets carry a fresh uuid so the shared DB cannot collide.
	suffix := uuid.NewString()
	docsTarget := "docs-" + suffix
	doneTarget := "done-" + suffix
	docs := [2]string{"improve_uzi", docsTarget}
	done := [2]string{"improve_agent", doneTarget}

	ownerA, repoA := seedOwner("a")
	ownerB, repoB := seedOwner("b")

	// ownerA hits `docs` in TWO runs; ownerB hits it in ONE — so the group spans 3 runs across 2
	// users, all open (no disposition). "run A1"/"A2" are the run TITLES that must NEVER surface.
	runA1, _ := judgedRun(ownerA, repoA, "secret run A1", "issues", docs)
	runA2, _ := judgedRun(ownerA, repoA, "secret run A2", "ok", docs)
	runB1, reviewB1 := judgedRun(ownerB, repoB, "secret run B1", "issues", docs)

	// A separate coordinate under ownerA that is DONE, so its group rolls up to `done`.
	runDone, reviewDone := judgedRun(ownerA, repoA, "secret done run", "ok", done)
	mustExec(`INSERT INTO recommendation_dispositions
		   (review_id, category, target, status, rationale_hash, set_by_user_id)
		 VALUES ($1, $2, $3, 'done', 'h', $4)`, reviewDone, done[0], done[1], ownerA)

	// ---- 1. the aggregate: cross-user dedup, counts, rollup ----------------------------------
	backlog, err := svc.AdminJudgeRecommendationBacklog(ctx, "all", nil)
	if err != nil {
		t.Fatalf("AdminJudgeRecommendationBacklog: %v", err)
	}
	di, ok := findAdminGroup(backlog.Groups, "improve_uzi", docsTarget)
	if !ok {
		t.Fatalf("no (improve_uzi, %s) group in the all-users backlog", docsTarget)
	}
	dg := backlog.Groups[di]
	if dg.UserCount != 2 {
		t.Errorf("docs user_count = %d, want 2 (ownerA + ownerB share the coordinate; the query has NO user predicate)", dg.UserCount)
	}
	if dg.RunCount != 3 {
		t.Errorf("docs run_count = %d, want 3 (two of ownerA's runs + one of ownerB's)", dg.RunCount)
	}
	if dg.OpenCount != 3 || dg.Bucket != "todo" {
		t.Errorf("docs open_count/bucket = %d/%q, want 3/todo (all three occurrences undisposed)", dg.OpenCount, dg.Bucket)
	}
	if len(dg.Occurrences) != 3 {
		t.Errorf("docs occurrences = %d, want 3", len(dg.Occurrences))
	}

	gi, ok := findAdminGroup(backlog.Groups, "improve_agent", doneTarget)
	if !ok {
		t.Fatalf("no (improve_agent, %s) group in the all-users backlog", doneTarget)
	}
	if backlog.Groups[gi].Bucket != "done" || backlog.Groups[gi].OpenCount != 0 {
		t.Errorf("done group bucket/open = %q/%d, want done/0", backlog.Groups[gi].Bucket, backlog.Groups[gi].OpenCount)
	}

	// ---- 2. attribution hidden: no seeded identifier appears in the serialized response ------
	// Scan the FULL backlog JSON for every owner, run and review id, and the run TITLES — a leak
	// riding a permitted field's value is exactly what the tag test cannot see.
	blob, err := json.Marshal(backlog)
	if err != nil {
		t.Fatalf("marshal backlog: %v", err)
	}
	body := string(blob)
	for _, id := range []uuid.UUID{ownerA, ownerB, runA1, runA2, runB1, runDone, reviewB1, reviewDone} {
		if strings.Contains(body, id.String()) {
			t.Errorf("the admin aggregate leaked an identifier %s — attribution must be hidden at the SQL + Go layers", id)
		}
	}
	for _, title := range []string{"secret run A1", "secret run A2", "secret run B1", "secret done run"} {
		if strings.Contains(body, title) {
			t.Errorf("the admin aggregate leaked a run title %q — run_title must not be projected", title)
		}
	}

	// ---- 3. the bucket filter and category filter narrow the aggregate -----------------------
	todoOnly, err := svc.AdminJudgeRecommendationBacklog(ctx, "todo", nil)
	if err != nil {
		t.Fatalf("todo backlog: %v", err)
	}
	if _, ok := findAdminGroup(todoOnly.Groups, "improve_agent", doneTarget); ok {
		t.Errorf("the done group must be filtered out of the ?bucket=todo view")
	}
	if _, ok := findAdminGroup(todoOnly.Groups, "improve_uzi", docsTarget); !ok {
		t.Errorf("the open docs group must appear in the ?bucket=todo view")
	}

	// ---- 4. the LIMIT cap binds in SQL: seed >cap recommendations, expect Truncated ----------
	// One review with cap+1 distinct-target recommendations guarantees the all-users query
	// returns cap+1 rows, so Truncated flips true (this DB already holds the rows above too).
	ownerC, repoC := seedOwner("c")
	_, reviewC := judgedRun(ownerC, repoC, "bulk run", "ok")
	mustExec(`INSERT INTO review_recommendations (review_id, category, target, rationale_md)
		 SELECT $1, 'improve_uzi', 'bulk-' || g::text || '-' || $2, 'because'
		 FROM generate_series(1, $3::int) g`, reviewC, suffix, JudgeBacklogMaxRows+1)

	capped, err := svc.AdminJudgeRecommendationBacklog(ctx, "all", nil)
	if err != nil {
		t.Fatalf("capped backlog: %v", err)
	}
	if !capped.Truncated {
		t.Errorf("Truncated = false after seeding %d recommendations, want true — the SQL LIMIT cap must bind", JudgeBacklogMaxRows+1)
	}

	// ---- 5. the all-users triage strip counts across users -----------------------------------
	// Only assert the SHAPE is reachable and non-empty rather than exact totals (the shared DB is
	// polluted by other tests); exactness is covered deterministically by the handler test.
	triage, err := svc.AdminJudgeTriageStats(ctx)
	if err != nil {
		t.Fatalf("AdminJudgeTriageStats: %v", err)
	}
	if triage.Total <= 0 {
		t.Errorf("all-users triage total = %d, want > 0", triage.Total)
	}
}
