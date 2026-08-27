package store_test

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

// TestRunCompletionSupersedesTimeoutLiveDB pins the issue #329 terminal-seam
// precedence against a REAL Postgres — the SQL WHERE guard and COALESCE writes the
// fake-store unit tests cannot exercise. It covers:
//
//   - SetRunCompleted SUPERSEDES a run the sweeper failed with fail_origin=
//     'run_timeout' (the core fix), and clears the stale failure classification;
//   - SetRunCompleted does NOT override a 'cancelled' run nor a 'failed' run with a
//     non-run_timeout fail_origin;
//   - ReconcileRunMR fills mr_iid/mr_web_url/branch only when NULL (COALESCE
//     non-clobber), and is scoped by worker_id;
//   - ListCIAutofixCandidateRefs excludes a cancelled newest run (issue #329
//     regression guard), falling through to an older completed run or to no candidate.
//
// Every subtest asserts on a concrete post-condition — rows affected AND a re-read
// column value — so a degenerate no-op (0 rows, unchanged row) FAILS rather than
// passing as "no error" (CLAUDE.md positive-control mandate).
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; mirrors the
// other *_integration_test.go in this package.
func TestRunCompletionSupersedesTimeoutLiveDB(t *testing.T) {
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

	// Owner opted INTO ci-autofix WITH an anthropic token, so the autofix subtest's
	// eligibility gates (owner opt-in + token) pass and only the cancelled-exclusion
	// is under test. Harmless for the supersede/reconcile subtests.
	userID, connID, repoID := uuid.New(), uuid.New(), uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash, ci_autofix_enabled) VALUES ($1, $2, 'x', true)`,
		userID, fmt.Sprintf("supersede-%s@e2e", userID))
	mustExec(ctx, t, pool, `INSERT INTO user_secrets (user_id, kind, ciphertext, label) VALUES ($1, 'anthropic_token', $2, 'default')`,
		userID, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, repoID, connID)

	// A unique token_hash: the store IT runner shares one DB across every LiveDB test,
	// and workers.token_hash is UNIQUE — a fixed literal would collide.
	wkr, err := q.CreateWorker(ctx, store.CreateWorkerParams{
		UserID: userID, Name: "laptop", TokenHash: append([]byte("supersede-"), userID[:]...),
		AnthropicBindMode: "default",
	})
	if err != nil {
		t.Fatalf("CreateWorker: %v", err)
	}
	workerID := pgtype.UUID{Bytes: wkr.ID, Valid: true}

	// read fetches the fields the seam precedence and reconcile touch. Nullable columns
	// are read into pgtype so "IS NULL" is distinguishable from a zero value.
	type runRow struct {
		status        string
		mrIID         pgtype.Int8
		mrWebURL      pgtype.Text
		branch        pgtype.Text
		failOrigin    pgtype.Text
		failureReason pgtype.Text
	}
	read := func(id uuid.UUID) runRow {
		t.Helper()
		var r runRow
		if err := pool.QueryRow(ctx,
			`SELECT status, mr_iid, mr_web_url, branch, fail_origin, failure_reason FROM runs WHERE id = $1`, id).
			Scan(&r.status, &r.mrIID, &r.mrWebURL, &r.branch, &r.failOrigin, &r.failureReason); err != nil {
			t.Fatalf("read run %s: %v", id, err)
		}
		return r
	}

	// ── Supersede: a worker completion overrides a run_timeout failure ──────────
	t.Run("Supersede", func(t *testing.T) {
		runID := uuid.New()
		// running, started long ago so the sweep's wall (60s) has elapsed.
		mustExec(ctx, t, pool,
			`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, worker_id, started_at)
			 VALUES ($1, $2, $3, 'issue', 1001, 't', 'd', 'running', $4, now() - interval '10 years')`,
			runID, userID, repoID, wkr.ID)

		swept, err := q.SweepRunningTimeout(ctx, store.SweepRunningTimeoutParams{
			FailureReason:        pgtype.Text{String: "run exceeded RUN_TIMEOUT", Valid: true},
			Now:                  pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true},
			GlobalTimeoutSeconds: 60,
		})
		if err != nil {
			t.Fatalf("SweepRunningTimeout: %v", err)
		}
		found := false
		for _, s := range swept {
			if s.ID == runID {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("sweep did not return our run %s; got %+v", runID, swept)
		}
		if r := read(runID); r.status != "failed" || r.failOrigin.String != "run_timeout" {
			t.Fatalf("after sweep: status=%q fail_origin=%q, want failed/run_timeout", r.status, r.failOrigin.String)
		}

		// The still-owning worker reports completion carrying the MR it opened.
		rows, err := q.SetRunCompleted(ctx, store.SetRunCompletedParams{
			Branch:   pgtype.Text{String: "agent/issue-1001", Valid: true},
			MrIid:    pgtype.Int8{Int64: 77, Valid: true},
			MrWebUrl: pgtype.Text{String: "https://forge.e2e/g/r/-/merge_requests/77", Valid: true},
			ID:       runID, WorkerID: workerID,
		})
		if err != nil {
			t.Fatalf("SetRunCompleted: %v", err)
		}
		if rows != 1 {
			t.Fatalf("SetRunCompleted rows = %d, want 1 (the completion must supersede run_timeout)", rows)
		}
		r := read(runID)
		if r.status != "completed" {
			t.Fatalf("status = %q, want completed", r.status)
		}
		if !r.mrIID.Valid || r.mrIID.Int64 != 77 {
			t.Fatalf("mr_iid = %+v, want 77", r.mrIID)
		}
		if r.mrWebURL.String != "https://forge.e2e/g/r/-/merge_requests/77" {
			t.Fatalf("mr_web_url = %q, want the reported URL", r.mrWebURL.String)
		}
		if r.branch.String != "agent/issue-1001" {
			t.Fatalf("branch = %q, want agent/issue-1001", r.branch.String)
		}
		// The stale timeout classification MUST be cleared.
		if r.failOrigin.Valid {
			t.Fatalf("fail_origin = %q, want NULL (stale run_timeout must be cleared)", r.failOrigin.String)
		}
		if r.failureReason.Valid {
			t.Fatalf("failure_reason = %q, want NULL (stale timeout reason must be cleared)", r.failureReason.String)
		}
	})

	// ── Cancelled is NOT superseded ────────────────────────────────────────────
	t.Run("CancelledNotSuperseded", func(t *testing.T) {
		runID := uuid.New()
		mustExec(ctx, t, pool,
			`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, worker_id)
			 VALUES ($1, $2, $3, 'issue', 1002, 't', 'd', 'cancelled', $4)`,
			runID, userID, repoID, wkr.ID)

		rows, err := q.SetRunCompleted(ctx, store.SetRunCompletedParams{
			Branch:   pgtype.Text{String: "agent/issue-1002", Valid: true},
			MrIid:    pgtype.Int8{Int64: 78, Valid: true},
			MrWebUrl: pgtype.Text{String: "https://forge.e2e/mr/78", Valid: true},
			ID:       runID, WorkerID: workerID,
		})
		if err != nil {
			t.Fatalf("SetRunCompleted: %v", err)
		}
		if rows != 0 {
			t.Fatalf("SetRunCompleted rows = %d, want 0 (a cancelled run must NOT be superseded)", rows)
		}
		r := read(runID)
		if r.status != "cancelled" {
			t.Fatalf("status = %q, want cancelled (unchanged)", r.status)
		}
		if r.mrIID.Valid {
			t.Fatalf("mr_iid = %+v, want NULL (the no-op completion must not write the MR)", r.mrIID)
		}
	})

	// ── A non-run_timeout failed run is NOT superseded ─────────────────────────
	t.Run("AgentFailureNotSuperseded", func(t *testing.T) {
		runID := uuid.New()
		mustExec(ctx, t, pool,
			`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, worker_id, fail_origin, failure_reason)
			 VALUES ($1, $2, $3, 'issue', 1003, 't', 'd', 'failed', $4, 'agent_failure', 'agent died')`,
			runID, userID, repoID, wkr.ID)

		rows, err := q.SetRunCompleted(ctx, store.SetRunCompletedParams{
			Branch: pgtype.Text{String: "agent/issue-1003", Valid: true},
			MrIid:  pgtype.Int8{Int64: 79, Valid: true},
			ID:     runID, WorkerID: workerID,
		})
		if err != nil {
			t.Fatalf("SetRunCompleted: %v", err)
		}
		if rows != 0 {
			t.Fatalf("SetRunCompleted rows = %d, want 0 (a non-run_timeout failure must NOT be superseded)", rows)
		}
		r := read(runID)
		if r.status != "failed" || r.failOrigin.String != "agent_failure" {
			t.Fatalf("after no-op: status=%q fail_origin=%q, want failed/agent_failure (unchanged)", r.status, r.failOrigin.String)
		}
	})

	// ── ReconcileRunMR: fills when NULL, never clobbers, worker-scoped ──────────
	t.Run("ReconcileFillsWhenNull", func(t *testing.T) {
		runID := uuid.New()
		// A cancelled run with NO recorded MR + the correct worker_id: reconcile heals it.
		mustExec(ctx, t, pool,
			`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, worker_id)
			 VALUES ($1, $2, $3, 'issue', 1004, 't', 'd', 'cancelled', $4)`,
			runID, userID, repoID, wkr.ID)

		n, err := q.ReconcileRunMR(ctx, store.ReconcileRunMRParams{
			MrIid:    pgtype.Int8{Int64: 88, Valid: true},
			MrWebUrl: pgtype.Text{String: "https://forge.e2e/mr/88", Valid: true},
			Branch:   pgtype.Text{String: "agent/issue-1004", Valid: true},
			ID:       runID, WorkerID: workerID,
		})
		if err != nil {
			t.Fatalf("ReconcileRunMR: %v", err)
		}
		if n != 1 {
			t.Fatalf("ReconcileRunMR rows = %d, want 1", n)
		}
		r := read(runID)
		if !r.mrIID.Valid || r.mrIID.Int64 != 88 {
			t.Fatalf("mr_iid = %+v, want 88 (filled from NULL)", r.mrIID)
		}
		if r.mrWebURL.String != "https://forge.e2e/mr/88" || r.branch.String != "agent/issue-1004" {
			t.Fatalf("mr_web_url/branch = %q/%q, want the reconciled values", r.mrWebURL.String, r.branch.String)
		}
		// Status is untouched (status-agnostic): still cancelled.
		if r.status != "cancelled" {
			t.Fatalf("status = %q, want cancelled (reconcile must be status-agnostic)", r.status)
		}
	})

	t.Run("ReconcileNeverClobbers", func(t *testing.T) {
		runID := uuid.New()
		// A run that already recorded mr_iid=42.
		mustExec(ctx, t, pool,
			`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, worker_id, mr_iid, mr_web_url, branch)
			 VALUES ($1, $2, $3, 'issue', 1005, 't', 'd', 'completed', $4, 42, 'https://forge.e2e/mr/42', 'agent/issue-1005')`,
			runID, userID, repoID, wkr.ID)

		n, err := q.ReconcileRunMR(ctx, store.ReconcileRunMRParams{
			MrIid:    pgtype.Int8{Int64: 99, Valid: true},
			MrWebUrl: pgtype.Text{String: "https://forge.e2e/mr/99", Valid: true},
			Branch:   pgtype.Text{String: "agent/issue-OTHER", Valid: true},
			ID:       runID, WorkerID: workerID,
		})
		if err != nil {
			t.Fatalf("ReconcileRunMR: %v", err)
		}
		// A row matched (COALESCE keeps the old value), so rowcount is 1 but the value
		// is unchanged — this is the non-clobber contract.
		if n != 1 {
			t.Fatalf("ReconcileRunMR rows = %d, want 1 (a row matched)", n)
		}
		r := read(runID)
		if !r.mrIID.Valid || r.mrIID.Int64 != 42 {
			t.Fatalf("mr_iid = %+v, want 42 unchanged (COALESCE must not clobber)", r.mrIID)
		}
		if r.mrWebURL.String != "https://forge.e2e/mr/42" || r.branch.String != "agent/issue-1005" {
			t.Fatalf("mr_web_url/branch = %q/%q, want the ORIGINAL values (non-clobber)", r.mrWebURL.String, r.branch.String)
		}
	})

	t.Run("ReconcileWorkerScoped", func(t *testing.T) {
		runID := uuid.New()
		mustExec(ctx, t, pool,
			`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, worker_id)
			 VALUES ($1, $2, $3, 'issue', 1006, 't', 'd', 'cancelled', $4)`,
			runID, userID, repoID, wkr.ID)

		foreign := pgtype.UUID{Bytes: uuid.New(), Valid: true}
		n, err := q.ReconcileRunMR(ctx, store.ReconcileRunMRParams{
			MrIid:    pgtype.Int8{Int64: 100, Valid: true},
			MrWebUrl: pgtype.Text{String: "https://forge.e2e/mr/100", Valid: true},
			Branch:   pgtype.Text{String: "agent/issue-1006", Valid: true},
			ID:       runID, WorkerID: foreign,
		})
		if err != nil {
			t.Fatalf("ReconcileRunMR: %v", err)
		}
		if n != 0 {
			t.Fatalf("ReconcileRunMR rows = %d, want 0 (a foreign worker_id must match nothing)", n)
		}
		if r := read(runID); r.mrIID.Valid {
			t.Fatalf("mr_iid = %+v, want NULL (a foreign-scoped reconcile must not write)", r.mrIID)
		}
	})

	// ── Autofix excludes cancelled (issue #329 regression guard) ───────────────
	t.Run("AutofixExcludesCancelled", func(t *testing.T) {
		// insertRun inserts an issue run on a branch with an explicit created_at offset
		// and (optionally) an mr_iid.
		insertRun := func(status, branch string, issueIID int64, mrIID *int64, createdOffset string) {
			var mr any
			if mrIID != nil {
				mr = *mrIID
			}
			mustExec(ctx, t, pool,
				`INSERT INTO runs (user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, branch, mr_iid, created_at)
				 VALUES ($1, $2, 'issue', $3, 't', 'd', $4, $5, $6, now() - $7::interval)`,
				userID, repoID, issueIID, status, branch, mr, createdOffset)
		}
		insertPipeline := func(ref string, pipelineID int64) {
			mustExec(ctx, t, pool,
				`INSERT INTO pipeline_statuses (repo_id, ref, pipeline_id, sha, status, web_url, synced_at)
				 VALUES ($1, $2, $3, 'deadbeef', 'failed', 'https://forge.e2e/p', now())`,
				repoID, ref, pipelineID)
		}
		iid := func(n int64) *int64 { return &n }

		// Branch A: newest run is CANCELLED (carries mr_iid, as a post-#329 reconciled
		// cancel would), older run is COMPLETED with a different mr_iid. Cancelled must be
		// excluded, so DISTINCT ON falls through to the older completed run.
		insertRun("completed", "agent/issue-cancelA", 2001, iid(201), "2 hours")
		insertRun("cancelled", "agent/issue-cancelA", 2001, iid(202), "1 hour")
		insertPipeline("agent/issue-cancelA", 9101)

		// Branch B: the ONLY run is cancelled → no candidate at all.
		insertRun("cancelled", "agent/issue-cancelB", 2002, iid(203), "1 hour")
		insertPipeline("agent/issue-cancelB", 9102)

		cands, err := q.ListCIAutofixCandidateRefs(ctx, repoID)
		if err != nil {
			t.Fatalf("ListCIAutofixCandidateRefs: %v", err)
		}
		byRef := map[string]pgtype.Int8{}
		for _, c := range cands {
			byRef[c.Ref.String] = c.MrIid
		}

		// Branch A: present, and represented by the OLDER completed run (mr_iid=201),
		// NOT the cancelled newest (202).
		mrA, okA := byRef["agent/issue-cancelA"]
		if !okA {
			t.Fatalf("branch agent/issue-cancelA absent; the older completed run should still be a candidate: %+v", cands)
		}
		if !mrA.Valid || mrA.Int64 != 201 {
			t.Fatalf("agent/issue-cancelA candidate mr_iid = %+v, want 201 (the completed run, NOT the cancelled 202)", mrA)
		}

		// Branch B: no candidate — the cancelled newest (and only) run is excluded.
		if _, okB := byRef["agent/issue-cancelB"]; okB {
			t.Fatalf("branch agent/issue-cancelB must yield NO candidate (its only run is cancelled): %+v", cands)
		}
	})
}
