package forgesvc

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestSyncBoardFreeMRStatesRecordsLiveDB exercises the board-free MR-state
// recorder (forgesvc.SyncBoardFreeMRStates, PRD #908 M3) end to end against a REAL Postgres:
// the store WRITE actually persists runs.mr_state, and the run self-evicts from
// ListBoardFreeMRStateWatchCandidates once its MR reaches a terminal state. A fake forge
// scripts the observed MR state and a fake ReworkCanceller records the merge-edge abort — the
// wiring around them (SetReworkCanceller → cancelReworkOnClosedMR) is the same the poller runs.
//
//   - NULL → 'opened' bootstrap: the first observation records without acting (no cancel), and
//     the run remains a watch candidate ('opened' is transient).
//   - 'opened' → 'merged' terminal: the recorder cancels the (would-be) in-flight rework EXACTLY
//     once and records 'merged'; the run then drops out of the candidate set (self-eviction).
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; e2e/run-store-it.sh
// (and task gate:api in CI) provide one. `go test ./...` without it SKIPs.
func TestSyncBoardFreeMRStatesRecordsLiveDB(t *testing.T) {
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

	svc := New(q, nil, time.Second, nil)
	canceller := &fakeReworkCanceller{}
	svc.SetReworkCanceller(canceller)

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}

	userID, connID, repoID := uuid.New(), uuid.New(), uuid.New()
	exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("sched-rec-%s@e2e", userID))
	exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, 1, 'g/schedrec', 'https://forge.e2e/g/schedrec', 'main', true)`, repoID, connID)

	const forgeProjectID = 4242 // fakeForge ignores it; the recorder only forwards it.
	const mrIID int64 = 9200
	branch := "uzi/prompt-" + uuid.New().String()

	// A completed prompt run (issue_iid NULL) with an mr_iid and mr_state NULL — the
	// pre-recorder bootstrap state, so it is a board-free watch candidate.
	runID := uuid.New()
	exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, branch, mr_iid, status)
	      VALUES ($1, $2, $3, 'prompt', NULL, 't', 'd', $4, $5, 'completed')`,
		runID, userID, repoID, branch, mrIID)

	readState := func() pgtype.Text {
		t.Helper()
		var s pgtype.Text
		if err := pool.QueryRow(ctx, `SELECT mr_state FROM runs WHERE id = $1`, runID).Scan(&s); err != nil {
			t.Fatalf("read mr_state: %v", err)
		}
		return s
	}
	candidateIDs := func() []uuid.UUID {
		t.Helper()
		cands, err := q.ListBoardFreeMRStateWatchCandidates(ctx, repoID)
		if err != nil {
			t.Fatalf("ListBoardFreeMRStateWatchCandidates: %v", err)
		}
		ids := make([]uuid.UUID, len(cands))
		for i, c := range cands {
			ids[i] = c.ID
		}
		return ids
	}

	// Step 1: forge reports the MR is opened. The NULL→opened bootstrap records without acting.
	if err := svc.SyncBoardFreeMRStates(ctx, repoID, forgeProjectID, &fakeForge{mr: forgeMR(mrIID, "opened")}); err != nil {
		t.Fatalf("SyncBoardFreeMRStates (opened): %v", err)
	}
	if s := readState(); !s.Valid || s.String != "opened" {
		t.Fatalf("after opened sync, runs.mr_state = %+v, want 'opened' persisted", s)
	}
	if n := len(canceller.calls); n != 0 {
		t.Fatalf("bootstrap opened must NOT cancel a rework; canceller calls = %d", n)
	}
	if ids := candidateIDs(); len(ids) != 1 || ids[0] != runID {
		t.Fatalf("run must still be a watch candidate while opened; got %v", ids)
	}

	// Step 2: forge advances to merged. The opened→merged edge cancels the in-flight rework
	// exactly once and records the terminal state, after which the run self-evicts.
	if err := svc.SyncBoardFreeMRStates(ctx, repoID, forgeProjectID, &fakeForge{mr: forgeMR(mrIID, "merged")}); err != nil {
		t.Fatalf("SyncBoardFreeMRStates (merged): %v", err)
	}
	if s := readState(); !s.Valid || s.String != "merged" {
		t.Fatalf("after merged sync, runs.mr_state = %+v, want 'merged' persisted", s)
	}
	if len(canceller.calls) != 1 || canceller.calls[0] != mrIID {
		t.Fatalf("merge edge must cancel the rework exactly once for mr %d; got %v", mrIID, canceller.calls)
	}
	if ids := candidateIDs(); len(ids) != 0 {
		t.Fatalf("a merged (terminal) MR must self-evict from the watch set; got %v", ids)
	}
}

// TestSyncBoardFreeMRStatesRecordsMrReworkLiveDB is the issue #1253 regression: the widened
// board-free lane must cover an ISSUE-LESS mr_rework helper run (issue_iid NULL), not only
// the prompt/self_improve scheduled lanes it originally served (PRD #908). The board-COUPLED
// ListMRWatchCandidates cannot own such a run — its issues JOIN drops issue_iid IS NULL rows —
// so a bug there would silently strand every issue-less mr_rework MR at mr_state NULL. This
// test seeds a completed mr_rework run whose MR is still transient (mr_state NULL) and drives
// one merged observation through SyncBoardFreeMRStates end to end against a REAL Postgres: the
// run must record mr_state='merged' and then self-evict from ListBoardFreeMRStateWatchCandidates.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; e2e/run-store-it.sh
// (and task gate:api in CI) provide one. `go test ./...` without it SKIPs.
func TestSyncBoardFreeMRStatesRecordsMrReworkLiveDB(t *testing.T) {
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

	svc := New(q, nil, time.Second, nil)
	canceller := &fakeReworkCanceller{}
	svc.SetReworkCanceller(canceller)

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}

	userID, connID, repoID := uuid.New(), uuid.New(), uuid.New()
	exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("mrrework-rec-%s@e2e", userID))
	exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, 1, 'g/mrrework', 'https://forge.e2e/g/mrrework', 'main', true)`, repoID, connID)

	const forgeProjectID = 4242 // fakeForge ignores it; the recorder only forwards it.
	const mrIID int64 = 9300
	branch := "agent/issue-" + uuid.New().String()

	// A completed source run whose id satisfies the mr_rework target_run_id FK (00058
	// REFERENCES runs). It carries NO mr_iid, so it is NOT itself a board-free candidate —
	// leaving the mr_rework run under test as the sole candidate.
	srcRunID := uuid.New()
	exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, branch, status)
	      VALUES ($1, $2, $3, 'prompt', NULL, 't', 'd', $4, 'completed')`,
		srcRunID, userID, repoID, "uzi/prompt-"+uuid.New().String())

	// The issue-LESS mr_rework run under test: issue_iid NULL, mr_iid set, mr_state NULL (the
	// pre-recorder bootstrap state). The 00167 runs_kind_shape CHECK requires pipeline_ref +
	// mr_iid + target_run_id for kind='mr_rework' and lets issue_iid be NULL — which is exactly
	// why the board-coupled lane (its issues JOIN) cannot own it and the widened board-free
	// lane must.
	runID := uuid.New()
	exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, branch, pipeline_ref, target_run_id, mr_iid, status)
	      VALUES ($1, $2, $3, 'mr_rework', NULL, 't', 'd', $4, $4, $5, $6, 'completed')`,
		runID, userID, repoID, branch, srcRunID, mrIID)

	candidateIDs := func() []uuid.UUID {
		t.Helper()
		cands, err := q.ListBoardFreeMRStateWatchCandidates(ctx, repoID)
		if err != nil {
			t.Fatalf("ListBoardFreeMRStateWatchCandidates: %v", err)
		}
		ids := make([]uuid.UUID, len(cands))
		for i, c := range cands {
			ids[i] = c.ID
		}
		return ids
	}
	readState := func() pgtype.Text {
		t.Helper()
		var s pgtype.Text
		if err := pool.QueryRow(ctx, `SELECT mr_state FROM runs WHERE id = $1`, runID).Scan(&s); err != nil {
			t.Fatalf("read mr_state: %v", err)
		}
		return s
	}

	// Before the sync the issue-less mr_rework run is a live board-free watch candidate.
	if ids := candidateIDs(); len(ids) != 1 || ids[0] != runID {
		t.Fatalf("issue-less mr_rework run must be a board-free watch candidate while mr_state is NULL; got %v", ids)
	}

	// The forge reports the MR merged. The NULL→merged bootstrap records the terminal state
	// (no rework was ever gated in for a NULL mr_state, so nothing to cancel), after which the
	// run self-evicts from the candidate set.
	if err := svc.SyncBoardFreeMRStates(ctx, repoID, forgeProjectID, &fakeForge{mr: forgeMR(mrIID, "merged")}); err != nil {
		t.Fatalf("SyncBoardFreeMRStates (merged): %v", err)
	}
	if s := readState(); !s.Valid || s.String != "merged" {
		t.Fatalf("after merged sync, issue-less mr_rework runs.mr_state = %+v, want 'merged' persisted", s)
	}
	if ids := candidateIDs(); len(ids) != 0 {
		t.Fatalf("a merged (terminal) issue-less mr_rework MR must self-evict from the board-free watch set; got %v", ids)
	}
}
