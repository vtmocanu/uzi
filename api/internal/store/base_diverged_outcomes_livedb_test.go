package store_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

func TestBaseDivergedOutcomesAndBoardLiveDB(t *testing.T) {
	ctx, dsn := standIsolatedDB(t, "base_diverged_outcomes_")
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	q := store.New(pool)
	a, b, c, conn, repo := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{a, b, c} {
		mustExec(ctx, t, pool, "INSERT INTO users(id,email,password_hash) VALUES($1,$2,'x')", id, id.String()+"@example.com")
	}
	mustExec(ctx, t, pool, `INSERT INTO forge_connections(id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext) VALUES($1,$2,'github','https://example.com','bot',1,'x')`, conn, a)
	mustExec(ctx, t, pool, `INSERT INTO repos(id,connection_id,forge_project_id,path_with_namespace,web_url,default_branch) VALUES($1,$2,1,'example/outcomes','https://example.com/outcomes','main')`, repo, conn)
	now := time.Now().UTC().Truncate(time.Microsecond)
	iid := int64(0)
	seed := func(user uuid.UUID, status, kind string, origin any, days int, end time.Time) uuid.UUID {
		t.Helper()
		iid++
		id := uuid.New()
		var rid, issue any = repo, iid
		var target any
		harness, reportOnly := "claude", false
		var wall any
		if kind == "judge" || kind == "cross_check" {
			var lead uuid.UUID
			if err := pool.QueryRow(ctx, `SELECT id FROM runs WHERE user_id=$1 AND kind='issue' ORDER BY id LIMIT 1`, user).Scan(&lead); err != nil {
				t.Fatal(err)
			}
			target = lead
			issue = nil
		}
		if kind == "cross_check" {
			harness, reportOnly, wall = "codex", true, 60
		}
		if kind == "chat" || kind == "judge" {
			rid, issue = nil, nil
		}
		mustExec(ctx, t, pool, `INSERT INTO runs(id,user_id,repo_id,issue_iid,issue_title,issue_description,status,kind,fail_origin,created_at,finished_at,failure_reason,stop_kind,stop_reason,target_run_id,harness,report_only,budget_wall_seconds)
  VALUES($1,$2,$3,$4,'t','d',$5,$6,$7,$8,$9,'private failure','cancelled','private stop',$10,$11,$12,$13)`, id, user, rid, issue, status, kind, origin, now.AddDate(0, 0, -days), end, target, harness, reportOnly, wall)
		return id
	}
	seed(a, "failed", "issue", nil, 10, now.Add(-5*time.Hour))
	latestFailure := seed(a, "failed", "issue", "agent_failure", 1, now.Add(-4*time.Hour))
	seed(a, "completed", "issue", nil, 10, now.Add(-6*time.Hour))
	seed(a, "completed", "issue", nil, 1, now.Add(-3*time.Hour))
	seed(a, "cancelled", "issue", nil, 1, now.Add(-2*time.Hour))
	seed(a, "failed", "issue", "plan_rejected", 1, now.Add(-time.Hour))
	seed(a, "failed", "issue", "base_diverged", 10, now.Add(-30*time.Minute))
	boardRun := seed(a, "failed", "issue", "base_diverged", 0, now)
	boardIID := iid
	seed(a, "failed", "chat", "agent_failure", 0, now.Add(time.Hour))
	seed(b, "failed", "issue", "base_diverged", 10, now.Add(time.Hour))
	seed(b, "failed", "issue", "base_diverged", 0, now.Add(2*time.Hour))
	// The existing policy refusal remains an ordinary, judge-eligible failure.
	policyRun := seed(c, "failed", "issue", "provider_policy_refusal", 0, now.Add(-7*time.Hour))
	// Every excluded kind carries the new origin for every user, including the
	// refusal-only owner, so all three query scopes must exclude all three kinds.
	for _, user := range []uuid.UUID{a, b, c} {
		for _, kind := range []string{"chat", "judge", "cross_check"} {
			seed(user, "failed", kind, "base_diverged", 0, now.Add(3*time.Hour))
		}
	}
	origins := workersvc.AllHumanLandableFailOrigins()
	check := func(label string, got []int64, raw []byte, want []int64, wantOrigins map[string]int64) {
		t.Helper()
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s counts=%v want %v", label, got, want)
		}
		m := decodeOutcomeOrigins(t, label, raw)
		if !reflect.DeepEqual(m, wantOrigins) {
			t.Fatalf("%s origins=%v want %v", label, m, wantOrigins)
		}
		var sum int64
		for _, n := range m {
			sum += n
		}
		if got[0] != got[1]+got[2]+got[3]+got[4]+got[5] || sum != got[5] || got[6] > got[5] {
			t.Fatalf("%s partition: %v origins=%v", label, got, m)
		}
	}
	type expected struct {
		life, week       []int64
		lifeMap, weekMap map[string]int64
		run              uuid.UUID
		completed        int64
		failedAt         time.Time
		failedOrigin     string
	}
	want := map[uuid.UUID]expected{
		a: {[]int64{8, 2, 1, 1, 2, 2, 0}, []int64{5, 1, 1, 1, 1, 1, 0}, map[string]int64{"agent_failure": 1, "unknown": 1}, map[string]int64{"agent_failure": 1}, latestFailure, 1, now.Add(-4 * time.Hour), "agent_failure"},
		b: {[]int64{2, 0, 0, 0, 2, 0, 0}, []int64{1, 0, 0, 0, 1, 0, 0}, map[string]int64{}, map[string]int64{}, uuid.Nil, 0, time.Time{}, ""},
		c: {[]int64{1, 0, 0, 0, 0, 1, 0}, []int64{1, 0, 0, 0, 0, 1, 0}, map[string]int64{"provider_policy_refusal": 1}, map[string]int64{"provider_policy_refusal": 1}, policyRun, 0, now.Add(-7 * time.Hour), "provider_policy_refusal"},
	}
	for user, w := range want {
		s, err := q.SelfRunOutcomes(ctx, store.SelfRunOutcomesParams{UserID: user, LandableOrigins: origins})
		if err != nil {
			t.Fatal(err)
		}
		check("self lifetime", []int64{s.LifetimeFinished, s.LifetimeCompleted, s.LifetimeCancelled, s.LifetimePlanRejected, s.LifetimeBaseDiverged, s.LifetimeFailed, s.LifetimeNeedsLanding}, s.LifetimeFailOrigins, w.life, w.lifeMap)
		check("self week", []int64{s.Last7Finished, s.Last7Completed, s.Last7Cancelled, s.Last7PlanRejected, s.Last7BaseDiverged, s.Last7Failed, s.Last7NeedsLanding}, s.Last7FailOrigins, w.week, w.weekMap)
		if s.LastFailedAt.Valid != (w.run != uuid.Nil) || s.LastFailedRunID != w.run || s.CompletedSinceLastFailure != w.completed {
			t.Fatalf("self recency: %+v", s)
		}
		if s.LastFailedOrigin != w.failedOrigin || (w.run != uuid.Nil && !s.LastFailedAt.Time.Equal(w.failedAt)) {
			t.Fatalf("self exact recency: at=%v origin=%q want at=%v origin=%q", s.LastFailedAt, s.LastFailedOrigin, w.failedAt, w.failedOrigin)
		}
	}
	rows, err := q.AdminRunOutcomesPerUser(ctx, origins)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("per user rows=%d", len(rows))
	}
	for _, s := range rows {
		w := want[s.UserID]
		check("per user lifetime", []int64{s.Finished, s.Completed, s.Cancelled, s.PlanRejected, s.LifetimeBaseDiverged, s.Failed, s.NeedsLanding}, s.FailOrigins, w.life, w.lifeMap)
		check("per user week", []int64{s.Last7Finished, s.Last7Completed, s.Last7Cancelled, s.Last7PlanRejected, s.Last7BaseDiverged, s.Last7Failed, s.Last7NeedsLanding}, s.Last7FailOrigins, w.week, w.weekMap)
		if s.LastFailedAt.Valid != (w.run != uuid.Nil) || s.LastFailedRunID != w.run || s.CompletedSinceLastFailure != w.completed {
			t.Fatalf("per user recency: %+v", s)
		}
		if s.LastFailedOrigin != w.failedOrigin || (w.run != uuid.Nil && !s.LastFailedAt.Time.Equal(w.failedAt)) {
			t.Fatalf("per user exact recency: at=%v origin=%q want at=%v origin=%q", s.LastFailedAt, s.LastFailedOrigin, w.failedAt, w.failedOrigin)
		}
	}
	s, err := q.AdminRunOutcomes(ctx, origins)
	if err != nil {
		t.Fatal(err)
	}
	check("factory lifetime", []int64{s.LifetimeFinished, s.LifetimeCompleted, s.LifetimeCancelled, s.LifetimePlanRejected, s.LifetimeBaseDiverged, s.LifetimeFailed, s.LifetimeNeedsLanding}, s.LifetimeFailOrigins, []int64{11, 2, 1, 1, 4, 3, 0}, map[string]int64{"agent_failure": 1, "unknown": 1, "provider_policy_refusal": 1})
	check("factory week", []int64{s.Last7Finished, s.Last7Completed, s.Last7Cancelled, s.Last7PlanRejected, s.Last7BaseDiverged, s.Last7Failed, s.Last7NeedsLanding}, s.Last7FailOrigins, []int64{7, 1, 1, 1, 2, 2, 0}, map[string]int64{"agent_failure": 1, "provider_policy_refusal": 1})
	if !s.LastFailedAt.Valid || !s.LastFailedAt.Time.Equal(now.Add(-4*time.Hour)) || s.LastFailedRunID != latestFailure || s.LastFailedUserID != a || s.LastFailedOrigin != "agent_failure" || s.CompletedSinceLastFailure != 1 {
		t.Fatalf("factory recency: %+v", s)
	}
	single, err := q.GetLatestRunForIssue(ctx, store.GetLatestRunForIssueParams{RepoID: repo, IssueIid: pgtype.Int8{Int64: boardIID, Valid: true}})
	if err != nil {
		t.Fatal(err)
	}
	if single.ID != boardRun || single.FailOrigin.String != "base_diverged" || !single.FailOrigin.Valid || single.FailureReason.String != "private failure" || single.StopKind.String != "cancelled" || single.StopReason.String != "private stop" {
		t.Fatalf("single projection: %+v", single)
	}
	latest, err := q.ListLatestRunsForRepo(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range latest {
		if r.ID != boardRun {
			continue
		}
		found = true
		if !r.FailOrigin.Valid || r.FailOrigin.String != "base_diverged" || r.FailureReason.String != "private failure" || r.StopKind.String != "cancelled" || r.StopReason.String != "private stop" {
			t.Fatalf("board projection: %+v", r)
		}
	}
	if !found {
		t.Fatal("board run missing")
	}
}
