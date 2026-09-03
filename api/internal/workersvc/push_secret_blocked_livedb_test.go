package workersvc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestSetStatePushSecretBlockedLiveDB exercises issue #974 M3 end to end against a REAL
// Postgres — the parts the pure-Go unit tests structurally cannot answer:
//
//	(a) migration 00186 applies and its WIDENED runs_fail_origin_check actually ACCEPTS
//	    fail_origin = 'push_secret_blocked' (the twelfth member, a value the old
//	    eleven-member CHECK rejected), and the runs.preserved_patch column (added by 00137,
//	    reused here) round-trips;
//	(b) a worker `failed` state report carrying FailOrigin='push_secret_blocked' + a
//	    multi-line PreservedPatch persists via SetState→SetRunFailed and reads back with
//	    BOTH fields intact (fail_origin verbatim, preserved_patch sanitized-but-structure
//	    preserved).
//
// Non-vacuity guard: a raw UPDATE stamping a bogus origin must FAIL with a CHECK violation
// (SQLSTATE 23514), proving the positive case is green because the constraint ADMITS the
// new member — not because no constraint exists.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (named OUTSIDE the
// uzi- namespace, per the store live-DB harness). A package that prints `ok` with PASS=0 is
// INVALID, not green.
func TestSetStatePushSecretBlockedLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	// Schema at HEAD — HEAD includes 00186_run_push_secret_blocked.sql. If its CHECK
	// re-add did not parse/apply, Migrate fails here, not on a later assert.
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	defer pool.Close()
	q := store.New(pool)
	svc := New(q, nil, Params{})

	userID, connID, repoID := uuid.New(), uuid.New(), uuid.New()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, userID, fmt.Sprintf("psb-%s@e2e", userID))
	exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'github', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, 1, 'g/psb', 'https://forge.e2e/g/psb', 'main', true)`, repoID, connID)

	// The worker must OWN a NON-TERMINAL run for runOwnedByWorker to admit the report and
	// for SetRunFailed's `status NOT IN (terminal)` guard to apply the transition — mirror an
	// existing SetState LiveDB setup: seed the run, then claim it (worker_id set, running).
	wkr := store.Worker{ID: uuid.New(), UserID: userID}
	// token_hash carries the worker UUID bytes so a re-run against a persistent DB never
	// collides on workers_token_hash_key (Migrate does not truncate).
	tokenHash := wkr.ID[:]
	exec(`INSERT INTO workers (id, user_id, name, token_hash) VALUES ($1, $2, 'w', $3)`, wkr.ID, userID, tokenHash)
	runID := uuid.New()
	// kind='task' satisfies runs_kind_shape (repo_id set, issue_iid NULL, branch NOT NULL) —
	// a task run whose finalize push carries a secret is exactly a push_secret_blocked case.
	exec(`INSERT INTO runs (id, user_id, repo_id, kind, branch, base_branch, issue_title, issue_description, status, worker_id)
	      VALUES ($1, $2, $3, 'task', 'agent/issue-974', 'main', 'push blocked by secret in diff', 'ctx', 'running', $4)`,
		runID, userID, repoID, wkr.ID)

	// A synthetic branch diff: interior \n and \t (which SanitizeBounded PRESERVES) and NO
	// leading/trailing whitespace (which it TRIMS), so the expected persisted value is exactly
	// the fixture. It textually contains a secret-SHAPED line as CONTENT — that is the whole
	// point: this is the diff a pre-push scan would flag, preserved verbatim for recovery. It
	// is just a Go string persisted to the throwaway test DB; no file is written and no scan
	// runs here.
	//
	// The `glpat-`+20 token is ASSEMBLED FROM PARTS (.claude/rules/prds.md): a single contiguous
	// `glpat-<20>` literal in this TRACKED source would be rejected by GitHub Push Protection on
	// THIS repo's own branch push — and a `//gitleaks:allow` would NOT save it, because Push
	// Protection ignores that directive (the exact caveat the feature under test is built around).
	// Split across the `+` there is no contiguous literal for either scanner to match, while the
	// assembled runtime value still exercises the preserved_patch round-trip.
	fakeToken := "glpat-" + "fake0000000000000000"
	patch := "diff --git a/deploy/ci.env b/deploy/ci.env\n" +
		"new file mode 100644\n" +
		"--- /dev/null\n" +
		"+++ b/deploy/ci.env\n" +
		"@@ -0,0 +1,3 @@\n" +
		"+# CI credentials block\n" +
		"+\tGITLAB_TOKEN=" + fakeToken + "\n" +
		"+end"
	ptr := func(s string) *string { return &s }

	// ── (b) the worker's typed push-secret-blocked failure persists via the real SetState path. ──
	got, applied, err := svc.SetState(ctx, wkr, runID, StateRequest{
		State:          "failed",
		FailOrigin:     ptr("push_secret_blocked"),
		PreservedPatch: ptr(patch),
	})
	if err != nil {
		t.Fatalf("SetState(failed, push_secret_blocked): %v", err)
	}
	if !applied {
		t.Fatal("SetState reported applied=false; the failed transition did not land")
	}
	if got.Status != "failed" {
		t.Errorf("returned run status = %q, want failed", got.Status)
	}

	// Re-read the authoritative row (SELECT * — carries fail_origin + preserved_patch).
	run, err := q.GetRunByID(ctx, runID)
	if err != nil {
		t.Fatalf("GetRunByID: %v", err)
	}
	if run.Status != "failed" {
		t.Errorf("persisted status = %q, want failed", run.Status)
	}
	// (a)+(b): the widened CHECK ADMITTED the twelfth member and it round-tripped verbatim.
	if !run.FailOrigin.Valid || run.FailOrigin.String != "push_secret_blocked" {
		t.Errorf("fail_origin = {valid=%t %q}, want {true push_secret_blocked}", run.FailOrigin.Valid, run.FailOrigin.String)
	}
	// The nullable column round-trips; \n/\t survive, so the diff's line structure is intact.
	if !run.PreservedPatch.Valid {
		t.Fatal("preserved_patch is NULL after a push_secret_blocked failed report; want the diff")
	}
	if run.PreservedPatch.String != patch {
		t.Errorf("preserved_patch round-trip mismatch:\n got: %q\nwant: %q", run.PreservedPatch.String, patch)
	}

	// ── Non-vacuity / discrimination: the CHECK is REAL. A raw UPDATE to a bogus origin must
	// be rejected 23514, so the positive case above cannot be green against an absent CHECK. ──
	_, rawErr := pool.Exec(ctx, `UPDATE runs SET fail_origin = 'not_a_member' WHERE id = $1`, runID)
	if rawErr == nil {
		t.Fatal("raw UPDATE to fail_origin='not_a_member' SUCCEEDED; the CHECK constraint is NOT enforced — the positive case is vacuous")
	}
	var pgErr *pgconn.PgError
	if !errors.As(rawErr, &pgErr) || pgErr.Code != "23514" {
		t.Fatalf("bogus fail_origin rejected with %v, want a CHECK violation (SQLSTATE 23514)", rawErr)
	}
	if pgErr.ConstraintName != "runs_fail_origin_check" {
		t.Errorf("CHECK violation on constraint %q, want runs_fail_origin_check", pgErr.ConstraintName)
	}
}
