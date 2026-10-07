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
	// The higher UUID wins the end-time tie; its missing timestamp and origin
	// exercise both historical fallbacks. No run_usage row is ever inserted.
	lower := uuid.MustParse("00000000-0000-4000-8000-000000002399")
	winner := uuid.MustParse("ffffffff-ffff-4fff-8fff-ffffffff2399")
	seed(lower, owner, "failed", "issue", "agent_failure", at, false, nil)
	seed(winner, owner, "failed", "issue", nil, at, true, nil)
	seed(uuid.New(), owner, "completed", "issue", nil, at.Add(time.Hour), true, nil)
	seed(uuid.New(), owner, "completed", "issue", nil, at.Add(-time.Hour), false, nil)
	seed(uuid.New(), owner, "completed", "issue", nil, at, false, nil) // strict greater-than
	seed(uuid.New(), clean, "completed", "issue", nil, at.Add(2*time.Hour), false, nil)
	for _, tc := range []struct {
		status, kind string
		origin       any
		target       any
	}{
		{"cancelled", "issue", nil, nil}, {"failed", "issue", "plan_rejected", nil},
		{"failed", "chat", "agent_failure", nil}, {"failed", "judge", "agent_failure", lower},
		{"completed", "chat", nil, nil}, {"completed", "judge", nil, lower},
	} {
		seed(uuid.New(), owner, tc.status, tc.kind, tc.origin, at.Add(3*time.Hour), false, tc.target)
	}
	// Plan cross-check children are excluded from the outcome totals, so a later
	// failed or completed child must neither replace the failure nor count after it.
	for _, status := range []string{"failed", "completed"} {
		exec(`INSERT INTO runs (id,user_id,repo_id,issue_title,issue_description,status,kind,fail_origin,finished_at,status_since,target_run_id,harness,report_only,budget_wall_seconds) VALUES ($1,$2,$3,'t','d',$4,'cross_check',CASE WHEN $4 = 'failed' THEN 'agent_failure' END,$5,$5,$6,'codex',true,600)`,
			uuid.New(), owner, repo, status, at.Add(3*time.Hour), lower)
	}
	self, err := q.SelfRunOutcomes(ctx, store.SelfRunOutcomesParams{UserID: owner})
	if err != nil {
		t.Fatal(err)
	}
	if !self.LastFailedAt.Valid || !self.LastFailedAt.Time.Equal(at) || self.LastFailedRunID != winner || self.LastFailedOrigin != "unknown" || self.CompletedSinceLastFailure != 1 {
		t.Fatalf("self failure recency = %+v, want tied historical failure %s at %s, unknown, 1 completed", self, winner, at)
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
	if !factory.LastFailedAt.Valid || !factory.LastFailedAt.Time.Equal(at) || factory.LastFailedRunID != winner || factory.LastFailedUserID != owner || factory.LastFailedOrigin != "unknown" || factory.CompletedSinceLastFailure != 2 {
		t.Fatalf("factory failure recency = %+v, want owner %s and 2 completed", factory, owner)
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
			if row.LastFailedRunID != winner || row.LastFailedOrigin != "unknown" || row.CompletedSinceLastFailure != 1 || row.Failed != self.LifetimeFailed || row.Finished != self.LifetimeFinished {
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
