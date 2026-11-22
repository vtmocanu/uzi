package store_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestFailureRecencyLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	// Roll back the fixture so this test can share the integration runner's DB.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := store.New(tx)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := tx.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	owner, clean, empty, conn, repo := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{owner, clean, empty} {
		exec(`INSERT INTO users (id, email, password_hash) VALUES ($1,$2,'x')`, id, fmt.Sprintf("recency-%s@example.com", id))
	}
	exec(`INSERT INTO forge_connections (id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext) VALUES ($1,$2,'github','https://example.com','bot',1,'x')`, conn, owner)
	exec(`INSERT INTO repos (id,connection_id,forge_project_id,path_with_namespace,web_url,default_branch) VALUES ($1,$2,1,'example/recency','https://example.com/recency','main')`, repo, conn)
	at := time.Date(2100, 1, 2, 3, 4, 5, 0, time.UTC) // later than other integration fixtures
	iid := 0
	seed := func(id, user uuid.UUID, status, kind string, origin any, end time.Time, missing bool, target any) {
		t.Helper()
		iid++
		var rid, issue, finished any = repo, iid, end
		if kind == "chat" || kind == "judge" {
			rid, issue = nil, nil
		}
		if missing {
			finished = nil
		}
		exec(`INSERT INTO runs (id,user_id,repo_id,issue_iid,issue_title,issue_description,status,kind,fail_origin,finished_at,status_since,target_run_id) VALUES ($1,$2,$3,$4,'t','d',$5,$6,$7,$8,$9,$10)`, id, user, rid, issue, status, kind, origin, finished, end, target)
	}
	// These two failures tie at `at` but are older than the last failure (at+3h, below), so
	// they only count toward failed/finished totals. No run_usage row is ever inserted.
	lower := uuid.MustParse("00000000-0000-4000-8000-000000002399")
	winner := uuid.MustParse("ffffffff-ffff-4fff-8fff-ffffffff2399")
	seed(lower, owner, "failed", "issue", "agent_failure", at, false, nil)
	seed(winner, owner, "failed", "issue", nil, at, true, nil)
	seed(uuid.New(), owner, "completed", "issue", nil, at.Add(time.Hour), true, nil)
	seed(uuid.New(), owner, "completed", "issue", nil, at.Add(-time.Hour), false, nil)
	// Completed at exactly the last failure's end time (at+3h): strict greater-than, so it
	// does not count as completed since the last failure.
	seed(uuid.New(), owner, "completed", "issue", nil, at.Add(3*time.Hour), false, nil)
	seed(uuid.New(), clean, "completed", "issue", nil, at.Add(4*time.Hour), false, nil)
	for _, tc := range []struct {
		status, kind string
		origin       any
		target       any
		end          time.Duration
	}{
		// Controls end after the last failure (at+3h), so each would win if not excluded.
		{"cancelled", "issue", nil, nil, 5 * time.Hour}, {"failed", "issue", "plan_rejected", nil, 5 * time.Hour},
		{"failed", "issue", "base_diverged", nil, 5 * time.Hour},
		// Chat is the only kind outside the outcome population (issue #4577): neither chat row
		// may become the last failure or count as completed after it.
		{"failed", "chat", "agent_failure", nil, 8 * time.Hour}, {"completed", "chat", nil, nil, 8 * time.Hour},
		// A completed judge after the last failure counts.
		{"completed", "judge", nil, lower, 6 * time.Hour},
	} {
		seed(uuid.New(), owner, tc.status, tc.kind, tc.origin, at.Add(tc.end), false, tc.target)
	}
	// Judge and cross_check children count as outcomes (issue #4577). The failed judge and
	// failed cross_check child tie at the last failure's end time (at+3h); the higher UUID (the judge, whose
	// missing finished_at and NULL origin exercise both fallbacks) wins the tie.
	judgeWinner := uuid.MustParse("ffffffff-ffff-4fff-8fff-ffffffff2398")
	seed(judgeWinner, owner, "failed", "judge", nil, at.Add(3*time.Hour), true, lower)
	for _, c := range []struct {
		id     uuid.UUID
		status string
		end    time.Duration
	}{
		{uuid.MustParse("00000000-0000-4000-8000-000000002398"), "failed", 3 * time.Hour},
		{uuid.New(), "completed", 7 * time.Hour},
	} {
		exec(`INSERT INTO runs (id,user_id,repo_id,issue_title,issue_description,status,kind,fail_origin,finished_at,status_since,target_run_id,harness,report_only,budget_wall_seconds) VALUES ($1,$2,$3,'t','d',$4,'cross_check',CASE WHEN $4 = 'failed' THEN 'agent_failure' END,$5,$5,$6,'codex',true,600)`,
			c.id, owner, repo, c.status, at.Add(c.end), lower)
	}
	self, err := q.SelfRunOutcomes(ctx, store.SelfRunOutcomesParams{UserID: owner})
	if err != nil {
		t.Fatal(err)
	}
	if !self.LastFailedAt.Valid || !self.LastFailedAt.Time.Equal(at.Add(3*time.Hour)) || self.LastFailedRunID != judgeWinner || self.LastFailedOrigin != "unknown" || self.CompletedSinceLastFailure != 2 {
		t.Fatalf("self failure recency = %+v, want tied historical judge failure %s, unknown, 2 completed (judge + cross_check)", self, judgeWinner)
	}
	for _, user := range []uuid.UUID{clean, empty} {
		row, err := q.SelfRunOutcomes(ctx, store.SelfRunOutcomesParams{UserID: user})
		if err != nil {
			t.Fatal(err)
		}
		if row.LastFailedAt.Valid {
			t.Fatalf("user %s has invented failure: %+v", user, row)
		}
	}
	factory, err := q.AdminRunOutcomes(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !factory.LastFailedAt.Valid || !factory.LastFailedAt.Time.Equal(at.Add(3*time.Hour)) || factory.LastFailedRunID != judgeWinner || factory.LastFailedUserID != owner || factory.LastFailedOrigin != "unknown" || factory.CompletedSinceLastFailure != 3 {
		t.Fatalf("factory failure recency = %+v, want owner %s and 3 completed (owner judge + cross_check, clean user's)", factory, owner)
	}
	users, err := q.AdminRunOutcomesPerUser(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	latest := time.Time{}
	found := false
	for _, row := range users {
		if row.LastFailedAt.Valid && row.LastFailedAt.Time.After(latest) {
			latest = row.LastFailedAt.Time
		}
		if row.UserID == owner {
			found = true
			if row.LastFailedRunID != judgeWinner || row.LastFailedOrigin != "unknown" || row.CompletedSinceLastFailure != 2 || row.Failed != self.LifetimeFailed || row.Finished != self.LifetimeFinished {
				t.Fatalf("per-user recency/counts = %+v, want self recency/counts", row)
			}
		}
		if row.UserID == clean && row.LastFailedAt.Valid {
			t.Fatal("clean user's per-user row has a failure")
		}
	}
	if !found || !factory.LastFailedAt.Time.Equal(latest) {
		t.Fatal("static fixture factory/per-user timestamps disagree or owner missing")
	}
	// A failure with no later completed runs yields zero, not an SQL NULL scan error.
	seed(uuid.New(), clean, "failed", "issue", "worker_lost", at.Add(4*time.Hour), false, nil)
	zero, err := q.SelfRunOutcomes(ctx, store.SelfRunOutcomesParams{UserID: clean})
	if err != nil {
		t.Fatal(err)
	}
	if zero.LastFailedOrigin != "worker_lost" || zero.CompletedSinceLastFailure != 0 {
		t.Fatalf("new infrastructure failure = %+v", zero)
	}
}
