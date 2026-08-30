package forgesvc

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// TestMRReworkCancelOnMRCloseLiveDB is the milestone-M3 acceptance test for issue #853:
// the MR-close watcher (forgesvc.SyncMRStates) aborts an in-flight mr_rework run once its
// MR leaves the opened state (merged/closed), and leaves it untouched on a locked
// transition. It wires the REAL collaborators end to end against a live Postgres —
// forgesvc.Service --SetReworkCanceller--> *workersvc.Service (whose CancelReworkForMR
// resolves the active mr_rework via GetActiveMRReworkRunForMR and either enqueues a cancel
// verdict for a live worker or flips the run server-side). A fake canceller would only
// re-assert the wiring the unit tests already pin (mr_watch_test.go); the point here is
// that the two services agree over the SAME rows through the SAME SQL the server runs.
//
// The four subtests each seed their OWN repo + issue + mr_iid so they are independent, and
// assert on the LIVE DB (runs.status / runs.stop_kind / a run_user_inputs cancel row):
//
//  1. Merged, NO live poller  -> the rework run flips to status='cancelled' (CancelRunServerSide).
//  2. Merged, LIVE poller     -> stop_kind='cancelled' stamped AND a kind='cancel' run_user_inputs
//     row is enqueued (CreateStopVerdictInput); the run stays non-terminal because no real worker
//     acks — the enqueue+stamp IS the observable, not a terminal status.
//  3. Locked (no-op)          -> the rework run is untouched: status unchanged, stop_kind NULL, no
//     cancel input. Locked is transient mid-merge and must NOT trigger a cancel.
//  4. Closed, cancels too     -> the CLOSED arm reaches the cancel (guardedMRMove returns moveSkipped
//     on a closed issue, not moveDeferred), so the rework run flips to status='cancelled'.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; e2e/run-store-it.sh
// (and task gate:api in CI) provide one. `go test ./...` without it SKIPs.
func TestMRReworkCancelOnMRCloseLiveDB(t *testing.T) {
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

	// The REAL canceller wired into the REAL forge sync. WorkerHeartbeatStale is set
	// large so a worker whose last_heartbeat_at = now() reads as a LIVE poller in
	// hasLivePoller (now() - heartbeat < stale). now defaults to time.Now via New.
	workerSvc := workersvc.New(q, nil, workersvc.Params{WorkerHeartbeatStale: time.Hour})
	forgeSvc := New(q, nil, time.Second, nil)
	forgeSvc.SetReworkCanceller(workerSvc)

	exec := func(t *testing.T, sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}

	// One owner is enough; each subtest gets a FRESH repo so scenarios never collide on
	// the (repo_id, mr_iid) partial unique index or the candidate scan.
	userID, connID := uuid.New(), uuid.New()
	exec(t, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("rework-cancel-%s@e2e", userID))
	exec(t, `INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	         VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`,
		connID, userID, []byte{0x1})

	const forgeProjectID = 4242 // fakeForge ignores it; SyncMRStates only forwards it.

	// scenario seeds a fresh repo with:
	//   - an issue cache row (state issueState, labelled Human Review so it is a Lane-A
	//     candidate when open; a closed issue is admitted by Lane B while mr_state='opened');
	//   - the completed ISSUE run (kind='issue', status='completed', mr_iid, mr_state='opened')
	//     — the sole/latest run for its issue_iid, so it IS the watch candidate;
	//   - the active MR_REWORK run (kind='mr_rework', mr_iid=N, status='running', pipeline_ref
	//     set, target_run_id = the issue run, issue_iid NULL so it is never itself a candidate).
	// When live is true it also seeds a worker with a fresh heartbeat and points the rework
	// run's worker_id at it, so hasLivePoller returns true.
	//
	// issueIID and mrIID differ on purpose (an issue number is not its MR number); the watch
	// is keyed on mr_iid, which is what links the issue run and the rework run to one MR.
	type seeded struct {
		repoID      uuid.UUID
		reworkRunID uuid.UUID
		mrIID       int64
	}
	seed := func(t *testing.T, issueIID, mrIID int64, issueState string, live bool) seeded {
		t.Helper()
		repoID := uuid.New()
		// forge_project_id must be UNIQUE per repo under repos_connection_id_forge_project_id_key
		// (one connection can't own two repos for the same forge project); mrIID is distinct per
		// subtest so it serves. This is only the STORED row — SyncMRStates is still called with the
		// const forgeProjectID below, which the fakeForge ignores, so the value here is inert beyond
		// satisfying the constraint.
		exec(t, `INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		         VALUES ($1, $2, $3, $4, 'https://forge.e2e/g/r', 'main', true)`,
			repoID, connID, mrIID, fmt.Sprintf("g/r-%s", repoID))

		exec(t, `INSERT INTO issues (repo_id, forge_issue_iid, title, state, labels, web_url, forge_updated_at, synced_at)
		         VALUES ($1, $2, 'seeded', $3, '["Human Review"]'::jsonb, 'https://x', now(), now())`,
			repoID, issueIID, issueState)

		branch := fmt.Sprintf("agent/issue-%d", issueIID)
		issueRunID := uuid.New()
		exec(t, `INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, branch, mr_iid, mr_state, status)
		         VALUES ($1, $2, $3, 'issue', $4, 't', 'd', $5, $6, 'opened', 'completed')`,
			issueRunID, userID, repoID, issueIID, branch, mrIID)

		var workerID any // NULL unless a live worker is seeded
		if live {
			wID := uuid.New()
			exec(t, `INSERT INTO workers (id, user_id, name, token_hash, status, last_heartbeat_at)
			         VALUES ($1, $2, 'w', $3, 'online', now())`,
				wID, userID, []byte(fmt.Sprintf("tok-%s", wID)))
			workerID = wID
		}

		reworkRunID := uuid.New()
		exec(t, `INSERT INTO runs (id, user_id, repo_id, kind, issue_title, issue_description,
		             pipeline_ref, mr_iid, target_run_id, worker_id, status)
		         VALUES ($1, $2, $3, 'mr_rework', 't', 'd', $4, $5, $6, $7, 'running')`,
			reworkRunID, userID, repoID, branch, mrIID, issueRunID, workerID)

		return seeded{repoID: repoID, reworkRunID: reworkRunID, mrIID: mrIID}
	}

	// readRun reads the live rework-run row back through raw SQL (this is a live-DB test).
	readRun := func(t *testing.T, runID uuid.UUID) (status string, stopKind pgtype.Text) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT status, stop_kind FROM runs WHERE id = $1`, runID).
			Scan(&status, &stopKind); err != nil {
			t.Fatalf("read rework run %s: %v", runID, err)
		}
		return status, stopKind
	}
	countCancelInputs := func(t *testing.T, runID uuid.UUID) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM run_user_inputs WHERE run_id = $1 AND kind = 'cancel'`, runID).
			Scan(&n); err != nil {
			t.Fatalf("count cancel inputs %s: %v", runID, err)
		}
		return n
	}

	t.Run("merged, no live poller -> server-side cancel", func(t *testing.T) {
		s := seed(t, 8531, 85310, "opened", false /* live */)
		f := &fakeForge{mrByIID: map[int64]forge.MergeRequest{s.mrIID: forgeMR(s.mrIID, "merged")}}

		if err := forgeSvc.SyncMRStates(ctx, s.repoID, forgeProjectID, f); err != nil {
			t.Fatalf("SyncMRStates: %v", err)
		}

		status, stopKind := readRun(t, s.reworkRunID)
		if status != "cancelled" {
			t.Fatalf("rework run status = %q, want %q — a merged MR with no live poller must be cancelled server-side", status, "cancelled")
		}
		if !stopKind.Valid || stopKind.String != "cancelled" {
			t.Fatalf("rework run stop_kind = %+v, want 'cancelled'", stopKind)
		}
		if n := countCancelInputs(t, s.reworkRunID); n != 0 {
			t.Fatalf("kind='cancel' run_user_inputs rows = %d, want 0 for a non-live worker (server-side cancel enqueues no verdict)", n)
		}
	})

	t.Run("merged, live poller -> cancel verdict enqueued + stop_kind stamped", func(t *testing.T) {
		s := seed(t, 8532, 85320, "opened", true /* live */)
		f := &fakeForge{mrByIID: map[int64]forge.MergeRequest{s.mrIID: forgeMR(s.mrIID, "merged")}}

		if err := forgeSvc.SyncMRStates(ctx, s.repoID, forgeProjectID, f); err != nil {
			t.Fatalf("SyncMRStates: %v", err)
		}

		status, stopKind := readRun(t, s.reworkRunID)
		// The live-worker path stamps stop_kind + enqueues a cancel; it does NOT itself
		// finalize the run (a real worker would ack the verdict and report). So the run
		// stays non-terminal here — assert the ENQUEUE + STAMP, not a terminal status.
		if !stopKind.Valid || stopKind.String != "cancelled" {
			t.Fatalf("rework run stop_kind = %+v, want 'cancelled' (stamped by CreateStopVerdictInput)", stopKind)
		}
		if n := countCancelInputs(t, s.reworkRunID); n != 1 {
			t.Fatalf("kind='cancel' run_user_inputs rows = %d, want 1 (a live worker must be sent a cancel verdict)", n)
		}
		if status == "cancelled" || status == "completed" || status == "failed" {
			t.Fatalf("rework run status = %q, want a NON-terminal status (no real worker acked the verdict)", status)
		}
	})

	t.Run("locked -> no-op, rework untouched", func(t *testing.T) {
		s := seed(t, 8533, 85330, "opened", false /* live */)
		f := &fakeForge{mrByIID: map[int64]forge.MergeRequest{s.mrIID: forgeMR(s.mrIID, "locked")}}

		if err := forgeSvc.SyncMRStates(ctx, s.repoID, forgeProjectID, f); err != nil {
			t.Fatalf("SyncMRStates: %v", err)
		}

		status, stopKind := readRun(t, s.reworkRunID)
		if status != "running" {
			t.Fatalf("rework run status = %q, want 'running' — a locked transition must NOT cancel the rework", status)
		}
		if stopKind.Valid {
			t.Fatalf("rework run stop_kind = %+v, want NULL — locked must leave the run untouched", stopKind)
		}
		if n := countCancelInputs(t, s.reworkRunID); n != 0 {
			t.Fatalf("kind='cancel' run_user_inputs rows = %d, want 0 — locked must enqueue nothing", n)
		}
	})

	t.Run("closed, no live poller -> server-side cancel via the CLOSED arm", func(t *testing.T) {
		// Closed issue: guardedMRMove short-circuits to moveSkipped (a closed issue is a
		// terminal placement, not a workflow column), so the CLOSED arm PROCEEDS to the
		// cancel instead of deferring. Lane B admits the closed-issue candidate while
		// mr_state='opened'.
		s := seed(t, 8534, 85340, "closed", false /* live */)
		f := &fakeForge{mrByIID: map[int64]forge.MergeRequest{s.mrIID: forgeMR(s.mrIID, "closed")}}

		if err := forgeSvc.SyncMRStates(ctx, s.repoID, forgeProjectID, f); err != nil {
			t.Fatalf("SyncMRStates: %v", err)
		}

		status, stopKind := readRun(t, s.reworkRunID)
		if status != "cancelled" {
			t.Fatalf("rework run status = %q, want 'cancelled' — a closed MR must cancel the rework via the CLOSED arm", status)
		}
		if !stopKind.Valid || stopKind.String != "cancelled" {
			t.Fatalf("rework run stop_kind = %+v, want 'cancelled'", stopKind)
		}
		if n := countCancelInputs(t, s.reworkRunID); n != 0 {
			t.Fatalf("kind='cancel' run_user_inputs rows = %d, want 0 for a non-live worker (server-side cancel enqueues no verdict)", n)
		}
	})

	t.Run("closed, forge move deferred -> cancels anyway (decoupled from board move)", func(t *testing.T) {
		// Regression guard for #853 review finding: an OPEN issue whose card sits in
		// Human Review is a Lane-A candidate, so guardedMRMove ATTEMPTS the move — but
		// updateErr makes AutoMove fail, returning moveDeferred (not moveSkipped). The
		// cancel must still fire: it is keyed on the MR, not gated behind the move, so a
		// confirmed close cancels the in-flight rework even when the board move can't
		// complete. mr_state is left unadvanced (the move retries next tick), which we do
		// not assert here — the point is that the rework is cancelled regardless.
		s := seed(t, 8535, 85350, "opened", false /* live */)
		f := &fakeForge{
			mrByIID:   map[int64]forge.MergeRequest{s.mrIID: forgeMR(s.mrIID, "closed")},
			updateErr: fmt.Errorf("forge unreachable"),
		}

		if err := forgeSvc.SyncMRStates(ctx, s.repoID, forgeProjectID, f); err != nil {
			t.Fatalf("SyncMRStates: %v", err)
		}

		status, stopKind := readRun(t, s.reworkRunID)
		if status != "cancelled" {
			t.Fatalf("rework run status = %q, want 'cancelled' — a confirmed-closed MR must cancel the rework even when the board move defers", status)
		}
		if !stopKind.Valid || stopKind.String != "cancelled" {
			t.Fatalf("rework run stop_kind = %+v, want 'cancelled'", stopKind)
		}
	})
}
