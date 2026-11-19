package forgesvc

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #4173 M4: the two MR-watch lanes stay disjoint and a linked repo's issue runs
// move to the board-free lane. An unlinked repo's runs match exactly as before.
func TestMRWatchLanesForLinkedAndUnlinkedReposLiveDB(t *testing.T) {
	ctx := context.Background()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	q := store.New(pool)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}

	user, forgeConn, itConn := uuid.New(), uuid.New(), uuid.New()
	linkedRepo, plainRepo := uuid.New(), uuid.New()
	exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, user, fmt.Sprintf("lanes-%s@e2e", user))
	exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'gitlab', $3, 'bot', 1, $4)`, forgeConn, user, "https://"+uuid.NewString()+".forge.e2e", []byte{1})
	exec(`INSERT INTO issue_tracker_connections (id, user_id, kind, base_url, account_id, account_login, token_ciphertext)
	      VALUES ($1, $2, 'youtrack', $3, '1-1', 'bot', $4)`, itConn, user, "https://"+uuid.NewString()+".yt.e2e", []byte{1})
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled,
	        issue_tracker_connection_id, issue_tracker_project_id, issue_tracker_project_short, eligibility_tag_id, eligibility_tag_name, issue_tracker_linked_at)
	      VALUES ($1, $2, 1, $3, 'https://forge.e2e/g/l', 'main', true, $4, '0-1', 'UZI', '5-1', 'uzi', now())`,
		linkedRepo, forgeConn, "g/linked-"+linkedRepo.String(), itConn)
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, 2, $3, 'https://forge.e2e/g/p', 'main', true)`, plainRepo, forgeConn, "g/plain-"+plainRepo.String())

	// Each repo gets the same two runs: a completed issue run with an opened MR (the
	// board lane's candidate on an unlinked repo) and a prompt run (the board-free
	// lane's, always).
	runs := map[uuid.UUID]map[string]uuid.UUID{}
	for _, repo := range []uuid.UUID{linkedRepo, plainRepo} {
		runs[repo] = map[string]uuid.UUID{"issue": uuid.New(), "prompt": uuid.New()}
		exec(`INSERT INTO issues (repo_id, forge_issue_iid, title, state, labels, web_url, forge_updated_at, synced_at)
		      VALUES ($1, 7, 't', 'opened', '["Human Review"]', 'https://forge.e2e/i/7', now(), now())`, repo)
		ins := `INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, branch, mr_iid, mr_state, status)
		        VALUES ($1, $2, $3, $4, $5, 't', 'd', $6, $7, 'opened', 'completed')`
		exec(ins, runs[repo]["issue"], user, repo, "issue", 7, "agent/issue-7-"+repo.String(), 13)
		exec(ins, runs[repo]["prompt"], user, repo, "prompt", nil, "uzi/prompt-"+repo.String(), 21)
	}

	boardLane := func(repo uuid.UUID) map[uuid.UUID]bool {
		t.Helper()
		rows, err := q.ListMRWatchCandidates(ctx, repo)
		if err != nil {
			t.Fatal(err)
		}
		got := map[uuid.UUID]bool{}
		for _, r := range rows {
			got[r.ID] = true
		}
		return got
	}
	freeLane := func(repo uuid.UUID) map[uuid.UUID]bool {
		t.Helper()
		rows, err := q.ListBoardFreeMRStateWatchCandidates(ctx, repo)
		if err != nil {
			t.Fatal(err)
		}
		got := map[uuid.UUID]bool{}
		for _, r := range rows {
			got[r.ID] = true
		}
		return got
	}

	t.Run("unlinked repo: lanes exactly as before", func(t *testing.T) {
		b, f := boardLane(plainRepo), freeLane(plainRepo)
		r := runs[plainRepo]
		if len(b) != 1 || !b[r["issue"]] {
			t.Errorf("board lane = %v, want only the issue run", b)
		}
		if len(f) != 1 || !f[r["prompt"]] {
			t.Errorf("board-free lane = %v, want the prompt run only", f)
		}
	})

	t.Run("linked repo: the issue run moves to the board-free lane", func(t *testing.T) {
		b, f := boardLane(linkedRepo), freeLane(linkedRepo)
		r := runs[linkedRepo]
		if len(b) != 0 {
			t.Errorf("board lane = %v, want none for a linked repo", b)
		}
		if len(f) != 2 || !f[r["issue"]] || !f[r["prompt"]] {
			t.Errorf("board-free lane = %v, want both runs", f)
		}
		for _, id := range r {
			if b[id] && f[id] {
				t.Errorf("run %v is in both lanes", id)
			}
		}
	})
	// A closed MR (the reviewer closed it; it may be reopened): the board lane watches it on an
	// unlinked repo (Lane A reopen edge); the board-free lane must do the same on a linked repo
	// and still not watch it on an unlinked one.
	exec(`UPDATE runs SET mr_state = 'closed' WHERE id = ANY($1)`,
		[]uuid.UUID{runs[linkedRepo]["issue"], runs[plainRepo]["issue"]})

	t.Run("closed MR: linked repo keeps the reopen watch in the board-free lane", func(t *testing.T) {
		r := runs[linkedRepo]
		if f := freeLane(linkedRepo); !f[r["issue"]] {
			t.Errorf("board-free lane = %v, want the closed issue run (reopen edge)", f)
		}
		if b := boardLane(linkedRepo); len(b) != 0 {
			t.Errorf("board lane = %v, want none for a linked repo", b)
		}
	})

	t.Run("closed MR: unlinked repo's board-free set is unchanged", func(t *testing.T) {
		r := runs[plainRepo]
		f := freeLane(plainRepo)
		if len(f) != 1 || !f[r["prompt"]] {
			t.Errorf("board-free lane = %v, want the prompt run only", f)
		}
		if b := boardLane(plainRepo); !b[r["issue"]] {
			t.Errorf("board lane = %v, want the closed issue run (Lane A reopen)", b)
		}
	})

	t.Run("closed MR: a superseded run or a closed issue is not watched", func(t *testing.T) {
		r := runs[linkedRepo]
		newer := uuid.New()
		exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, branch, status)
		      VALUES ($1, $2, $3, 'issue', 7, 't', 'd', $4, 'running')`, newer, user, linkedRepo, "agent/issue-7-rework-"+linkedRepo.String())
		if f := freeLane(linkedRepo); f[r["issue"]] {
			t.Errorf("board-free lane = %v, want the superseded closed run dropped", f)
		}
		exec(`DELETE FROM runs WHERE id = $1`, newer)
		exec(`UPDATE issues SET state = 'closed' WHERE repo_id = $1`, linkedRepo)
		if f := freeLane(linkedRepo); f[r["issue"]] {
			t.Errorf("board-free lane = %v, want a closed issue's closed MR dropped", f)
		}
	})

	// The recorder end to end: a reopen is recorded back to 'opened'.
	t.Run("closed then reopened MR is recorded opened", func(t *testing.T) {
		exec(`UPDATE issues SET state = 'opened' WHERE repo_id = $1`, linkedRepo)
		svc := newTestService(q)
		f := &fakeForge{mr: forgeMR(13, forge.MRStateOpened)}
		if err := svc.SyncBoardFreeMRStates(ctx, linkedRepo, 1, f); err != nil {
			t.Fatal(err)
		}
		var state string
		if err := pool.QueryRow(ctx, `SELECT mr_state FROM runs WHERE id = $1`, runs[linkedRepo]["issue"]).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state != "opened" {
			t.Errorf("mr_state = %q, want opened", state)
		}
	})

	// A closed MR whose issue left the cache (moved, untagged, deleted) must not be polled
	// forever: the reopen watch needs a cached OPEN issue, as the board lane's Lane A does.
	t.Run("closed MR: an evicted issue is not watched", func(t *testing.T) {
		exec(`UPDATE runs SET mr_state = 'closed' WHERE id = $1`, runs[linkedRepo]["issue"])
		exec(`DELETE FROM issues WHERE repo_id = $1 AND forge_issue_iid = 7`, linkedRepo)
		if f := freeLane(linkedRepo); f[runs[linkedRepo]["issue"]] {
			t.Errorf("board-free lane = %v, want an evicted issue's closed MR dropped", f)
		}
	})

	// Live rows (opened/locked/NULL mr_state) sort ahead of closed reopen-watch rows, so a
	// burst of closed rows can never push a live run past the LIMIT.
	t.Run("live rows are ordered ahead of closed rows", func(t *testing.T) {
		exec(`INSERT INTO issues (repo_id, forge_issue_iid, title, state, labels, web_url, forge_updated_at, synced_at)
		      VALUES ($1, 7, 't', 'opened', '[]', 'https://forge.e2e/i/7', now(), now())`, linkedRepo)
		exec(`UPDATE runs SET mr_state = 'closed', created_at = now() + interval '1 hour' WHERE id = $1`, runs[linkedRepo]["issue"])
		exec(`UPDATE runs SET mr_state = NULL, created_at = now() - interval '1 hour' WHERE id = $1`, runs[linkedRepo]["prompt"])
		rows, err := q.ListBoardFreeMRStateWatchCandidates(ctx, linkedRepo)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 2 || rows[0].ID != runs[linkedRepo]["prompt"] || rows[1].ID != runs[linkedRepo]["issue"] {
			t.Errorf("order = %v, want the older live (NULL mr_state) run before the newer closed run", rows)
		}
	})

	t.Run("live rows keep the created_at order on an unlinked repo", func(t *testing.T) {
		// A never-recorded (NULL mr_state) run does not outrank a newer opened one: among
		// live rows the order is created_at DESC exactly as before the linked reopen watch.
		older := uuid.New()
		exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, branch, mr_iid, mr_state, status, created_at)
		      VALUES ($1, $2, $3, 'prompt', NULL, 't', 'd', $4, 22, NULL, 'completed', now() - interval '1 hour')`,
			older, user, plainRepo, "uzi/prompt-older-"+plainRepo.String())
		exec(`UPDATE runs SET created_at = now() WHERE id = $1`, runs[plainRepo]["prompt"])
		rows, err := q.ListBoardFreeMRStateWatchCandidates(ctx, plainRepo)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 2 || rows[0].ID != runs[plainRepo]["prompt"] || rows[1].ID != older {
			t.Errorf("order = %v, want the newer opened run before the older NULL one", rows)
		}
	})
}
