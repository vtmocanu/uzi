package store_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// TestPipelineStatusesLiveDB exercises PRD #6's pipeline-status cache SQL against
// a REAL Postgres — the parts the fake-store unit tests cannot cover: the
// migration itself, the latest-per-ref upsert, the watched-run-ref selection
// (window + non-terminal-vs-terminal-with-MR + DISTINCT-ON-newest + cap), the
// default-branch-only projection, the per-card most-recent-run join, and the
// reconcile eviction.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; the e2e
// runner (e2e/run-store-it.sh) provides one. `go test ./...` without it SKIPs.
func TestPipelineStatusesLiveDB(t *testing.T) {
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

	// Fresh ids isolate this run from leftover rows (repo-scoped queries + unique
	// (repo_id, ref) / (repo_id, forge_issue_iid)).
	userID, connID, repoID := uuid.New(), uuid.New(), uuid.New()
	mustExec(ctx, t, pool,
		`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("pipe-%s@e2e", userID))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`,
		connID, userID, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`,
		repoID, connID)

	now := time.Now()
	// run inserts one run. mrIID<=0 → NULL mr_iid; terminal runs get finished_at =
	// now + finishedOffset (negative = in the past); createdAtOffset orders runs.
	run := func(iid int64, branch, status string, mrIID int64, finishedOffset, createdOffset time.Duration) {
		var mr any
		if mrIID > 0 {
			mr = mrIID
		}
		var finished any
		if status == "completed" || status == "failed" || status == "cancelled" {
			finished = now.Add(finishedOffset)
		}
		mustExec(ctx, t, pool,
			`INSERT INTO runs (user_id, repo_id, issue_iid, issue_title, issue_description, status, branch, mr_iid, finished_at, created_at)
			 VALUES ($1, $2, $3, 't', 'd', $4, $5, $6, $7, $8)`,
			userID, repoID, iid, status, branch, mr, finished, now.Add(createdOffset))
	}

	// ── watched-ref selection fixtures ──
	run(1, "agent/issue-1", "running", 0, 0, -5*time.Minute)                      // non-terminal → watched
	run(2, "agent/issue-2", "completed", 5, -time.Hour, -6*time.Minute)           // terminal+MR, in window → watched (mr 5)
	run(3, "agent/issue-3", "completed", 0, -time.Hour, -7*time.Minute)           // terminal, NO MR → NOT watched
	run(4, "agent/issue-4", "completed", 9, -30*24*time.Hour, -8*time.Minute)     // terminal+MR but OUTSIDE window → NOT watched
	// issue-5 has two runs on the same branch: an older completed+MR, a newer
	// running. DISTINCT ON must collapse to the newest (running, no MR) → watched.
	run(5, "agent/issue-5", "completed", 7, -time.Hour, -20*time.Minute)
	run(5, "agent/issue-5", "running", 0, 0, -1*time.Minute)
	// A queued run with no branch must never be a watched ref.
	run(6, "", "queued", 0, 0, -2*time.Minute)

	window := 14 * 24 * time.Hour
	watched, err := q.ListWatchedRunRefsForRepo(ctx, store.ListWatchedRunRefsForRepoParams{
		RepoID:        repoID,
		FinishedAfter: pgtype.Timestamptz{Time: now.Add(-window), Valid: true},
		MaxRefs:       20,
	})
	if err != nil {
		t.Fatalf("ListWatchedRunRefsForRepo: %v", err)
	}
	gotRefs := map[string]pgtype.Int8{}
	for _, w := range watched {
		gotRefs[w.Branch.String] = w.MrIid
	}
	wantWatched := []string{"agent/issue-1", "agent/issue-2", "agent/issue-5"}
	for _, w := range wantWatched {
		if _, ok := gotRefs[w]; !ok {
			t.Errorf("expected %q to be watched, got %v", w, keys(gotRefs))
		}
	}
	for _, notW := range []string{"agent/issue-3", "agent/issue-4", ""} {
		if _, ok := gotRefs[notW]; ok {
			t.Errorf("%q must NOT be watched (terminal-no-MR / out-of-window / blank)", notW)
		}
	}
	// issue-2's watched ref carries its MR iid; issue-5's newest run has no MR.
	if mr := gotRefs["agent/issue-2"]; !mr.Valid || mr.Int64 != 5 {
		t.Errorf("agent/issue-2 must carry mr_iid=5, got %+v", mr)
	}
	if mr := gotRefs["agent/issue-5"]; mr.Valid {
		t.Errorf("agent/issue-5's newest run has no MR, but got mr_iid=%d", mr.Int64)
	}

	// ── cap ── MaxRefs=1 returns exactly one (the newest branch).
	capped, err := q.ListWatchedRunRefsForRepo(ctx, store.ListWatchedRunRefsForRepoParams{
		RepoID:        repoID,
		FinishedAfter: pgtype.Timestamptz{Time: now.Add(-window), Valid: true},
		MaxRefs:       1,
	})
	if err != nil {
		t.Fatalf("ListWatchedRunRefsForRepo(cap=1): %v", err)
	}
	if len(capped) != 1 {
		t.Fatalf("cap=1 must return one ref, got %d", len(capped))
	}

	// ── upsert (insert then update-on-conflict = latest-per-ref) ──
	upsert := func(ref, status string, pipelineID int64) {
		if _, err := q.UpsertPipelineStatus(ctx, store.UpsertPipelineStatusParams{
			RepoID: repoID, Ref: ref, PipelineID: pipelineID, Sha: "sha",
			Status: status, WebUrl: "https://gl/p",
		}); err != nil {
			t.Fatalf("UpsertPipelineStatus(%s): %v", ref, err)
		}
	}
	upsert("main", "running", 100)
	upsert("main", "failed", 101) // same ref → overwrites, no second row
	upsert("agent/issue-1", "failed", 200)
	upsert("agent/issue-2", "success", 300)

	got, err := q.GetPipelineStatusByRef(ctx, store.GetPipelineStatusByRefParams{RepoID: repoID, Ref: "main"})
	if err != nil {
		t.Fatalf("GetPipelineStatusByRef(main): %v", err)
	}
	if got.PipelineID != 101 || got.Status != "failed" {
		t.Fatalf("upsert must keep the latest per ref, got pipeline=%d status=%s", got.PipelineID, got.Status)
	}

	// ── default-branch-only projection: 'main' is returned, run branches are not ──
	defBranch, err := q.ListDefaultBranchPipelineStatuses(ctx, []uuid.UUID{repoID})
	if err != nil {
		t.Fatalf("ListDefaultBranchPipelineStatuses: %v", err)
	}
	if len(defBranch) != 1 || defBranch[0].PipelineID != 101 {
		t.Fatalf("default-branch projection must return only the main row, got %+v", defBranch)
	}

	// ── per-card: each issue's MOST RECENT run's branch pipeline ──
	cardRows, err := q.ListRunPipelineStatusesForRepo(ctx, repoID)
	if err != nil {
		t.Fatalf("ListRunPipelineStatusesForRepo: %v", err)
	}
	byIssue := map[int64]store.ListRunPipelineStatusesForRepoRow{}
	for _, r := range cardRows {
		byIssue[r.IssueIid] = r
	}
	// issue-1's newest run is agent/issue-1, which has a cached pipeline → present.
	if byIssue[1].PipelineID != 200 {
		t.Errorf("card for issue-1 must carry agent/issue-1's pipeline 200, got %+v", byIssue[1])
	}
	// issue-5's newest run branch (agent/issue-5) has NO cached pipeline → no card row.
	if _, ok := byIssue[5]; ok {
		t.Errorf("issue-5's newest run branch has no cached pipeline, so it must carry no badge")
	}
	// issue-6's newest run has a blank branch → no card row.
	if _, ok := byIssue[6]; ok {
		t.Errorf("issue-6's newest run has no branch, so it must carry no badge")
	}

	// ── reconcile eviction: keep only 'main', drop the run-branch rows ──
	if _, err := q.DeletePipelineStatusesNotIn(ctx, store.DeletePipelineStatusesNotInParams{
		RepoID: repoID, KeepRefs: []string{"main"},
	}); err != nil {
		t.Fatalf("DeletePipelineStatusesNotIn: %v", err)
	}
	if _, err := q.GetPipelineStatusByRef(ctx, store.GetPipelineStatusByRefParams{RepoID: repoID, Ref: "agent/issue-1"}); err == nil {
		t.Errorf("eviction should have dropped agent/issue-1")
	}
	if _, err := q.GetPipelineStatusByRef(ctx, store.GetPipelineStatusByRefParams{RepoID: repoID, Ref: "main"}); err != nil {
		t.Errorf("eviction must keep the still-watched main row, got %v", err)
	}
}

func keys(m map[string]pgtype.Int8) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
