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

// TestSyncActiveMRTargetsLiveDB is the issue #4616 M3 acceptance test: the poller-side
// SyncActiveMRTargets, wired to the REAL *workersvc.Service canceller, stops ci_fix runs
// whose bound MR merged, over the same rows and SQL the server runs. Each subtest seeds its
// own repo with a ci_fix that has NO completed source run, NO watch row and NO mr_iid, so
// the only link to its MR is ci_fix_mr_iid: no other watcher can see it.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (e2e/run-store-it.sh).
type seeded struct {
	repoID, runID uuid.UUID
	branch        string
}

func TestSyncActiveMRTargetsLiveDB(t *testing.T) {
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

	workerSvc := workersvc.New(q, nil, workersvc.Params{WorkerHeartbeatStale: time.Hour})
	workerSvc.SetTxBeginner(pool)

	exec := func(t *testing.T, sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	userID, connID := uuid.New(), uuid.New()
	exec(t, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("active-target-%s@e2e", userID))
	exec(t, `INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	         VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})

	const mrIID = int64(5151)
	var projectSeq int64 = 700000
	// seed inserts a repo plus one ci_fix run. bound sets ci_fix_mr_iid; live attaches a
	// worker with a fresh heartbeat (status running); otherwise the run is queued.
	seed := func(t *testing.T, bound, live bool, status string) seeded {
		t.Helper()
		projectSeq++
		repoID, runID := uuid.New(), uuid.New()
		exec(t, `INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		         VALUES ($1, $2, $3, $4, 'https://forge.e2e/g/r', 'main', true)`,
			repoID, connID, projectSeq, fmt.Sprintf("g/r-%s", repoID))
		var worker, iid any
		if live {
			wID := uuid.New()
			exec(t, `INSERT INTO workers (id, user_id, name, token_hash, status, last_heartbeat_at)
			         VALUES ($1, $2, 'w', $3, 'online', now())`, wID, userID, []byte("tok-"+wID.String()))
			worker = wID
		}
		if bound {
			iid = mrIID
		}
		branch := "agent/ci-fix-" + runID.String()
		exec(t, `INSERT INTO runs (id, user_id, repo_id, kind, pipeline_id, pipeline_ref, ci_fix_mr_iid, status, worker_id, issue_title, issue_description)
		         VALUES ($1, $2, $3, 'ci_fix', 99, $4, $5, $6, $7, 't', 'd')`,
			runID, userID, repoID, branch, iid, status, worker)
		return seeded{repoID: repoID, runID: runID, branch: branch}
	}
	read := func(t *testing.T, runID uuid.UUID) (status string, stopKind, stopReason pgtype.Text, gen int64) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT status, stop_kind, stop_reason, claim_generation FROM runs WHERE id = $1`, runID).
			Scan(&status, &stopKind, &stopReason, &gen); err != nil {
			t.Fatalf("read run: %v", err)
		}
		return
	}
	cancelInputs := func(t *testing.T, runID uuid.UUID) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM run_user_inputs WHERE run_id = $1 AND kind = 'cancel'`, runID).Scan(&n); err != nil {
			t.Fatalf("count cancel inputs: %v", err)
		}
		return n
	}
	newSvc := func() *Service {
		svc := New(q, nil, time.Second, nil)
		svc.SetTerminalTargetCanceller(workerSvc)
		return svc
	}
	forgeFor := func(s seeded, state string) *summaryForge {
		return &summaryForge{byIID: map[int64]forge.MergeRequestSummary{mrIID: {
			IID: mrIID, State: state, SourceBranch: s.branch, HeadSHA: activeTestSHA,
		}}}
	}
	pass := func(t *testing.T, svc *Service, s seeded, f forge.CodeHost) {
		t.Helper()
		if err := svc.SyncActiveMRTargets(ctx, s.repoID, 4242, f); err != nil {
			t.Fatalf("SyncActiveMRTargets: %v", err)
		}
	}

	t.Run("bound ci_fix with only ci_fix_mr_iid is cancelled on a first merged observation", func(t *testing.T) {
		s := seed(t, true, false, "queued")
		pass(t, newSvc(), s, forgeFor(s, forge.MRStateMerged))
		status, stopKind, _, _ := read(t, s.runID)
		if status != "cancelled" || !stopKind.Valid || stopKind.String != "cancelled" {
			t.Fatalf("status=%q stop_kind=%+v, want cancelled/cancelled", status, stopKind)
		}
	})

	t.Run("unbound ci_fix is untouched and its MR is never read", func(t *testing.T) {
		s := seed(t, false, false, "queued")
		f := forgeFor(s, forge.MRStateMerged)
		pass(t, newSvc(), s, f)
		status, stopKind, _, _ := read(t, s.runID)
		if status != "queued" || stopKind.Valid || len(f.calls) != 0 {
			t.Fatalf("status=%q stop_kind=%+v forge calls=%v, want queued/NULL/none", status, stopKind, f.calls)
		}
	})

	t.Run("duplicate observations create exactly one cancel input for a live worker", func(t *testing.T) {
		s := seed(t, true, true, "running")
		svc := newSvc()
		f := forgeFor(s, forge.MRStateMerged)
		pass(t, svc, s, f)
		pass(t, svc, s, f)
		_, stopKind, _, _ := read(t, s.runID)
		if !stopKind.Valid || stopKind.String != "cancelled" {
			t.Fatalf("stop_kind = %+v, want cancelled", stopKind)
		}
		if n := cancelInputs(t, s.runID); n != 1 {
			t.Fatalf("cancel inputs = %d, want exactly 1 across two passes", n)
		}
	})

	t.Run("locked then merged cancels on the merged pass", func(t *testing.T) {
		s := seed(t, true, false, "queued")
		svc := newSvc()
		f := forgeFor(s, forge.MRStateLocked)
		pass(t, svc, s, f)
		if status, stopKind, _, _ := read(t, s.runID); status != "queued" || stopKind.Valid {
			t.Fatalf("after locked: status=%q stop_kind=%+v, want untouched", status, stopKind)
		}
		f.byIID[mrIID] = goodFor(s, forge.MRStateMerged)
		pass(t, svc, s, f)
		if status, _, _, _ := read(t, s.runID); status != "cancelled" {
			t.Fatalf("after merged: status=%q, want cancelled", status)
		}
	})

	t.Run("a reclaim between observation and cancel leaves the run alone", func(t *testing.T) {
		s := seed(t, true, true, "running")
		f := forgeFor(s, forge.MRStateMerged)
		// The barrier: the run is reclaimed (claim_generation bumped) after the enumeration
		// row was read but before the cancel runs, so the observation is stale.
		f.hook = func(int64) {
			exec(t, `UPDATE runs SET claim_generation = claim_generation + 1 WHERE id = $1`, s.runID)
		}
		pass(t, newSvc(), s, f)
		status, stopKind, _, gen := read(t, s.runID)
		if gen != 1 || status != "running" || stopKind.Valid || cancelInputs(t, s.runID) != 0 {
			t.Fatalf("status=%q stop_kind=%+v gen=%d inputs=%d, want running/NULL/1/0 (stale observation)", status, stopKind, gen, cancelInputs(t, s.runID))
		}
	})

	t.Run("a paused ci_fix is skipped", func(t *testing.T) {
		s := seed(t, true, false, "paused")
		pass(t, newSvc(), s, forgeFor(s, forge.MRStateMerged))
		status, stopKind, _, _ := read(t, s.runID)
		if status != "paused" || stopKind.Valid {
			t.Fatalf("status=%q stop_kind=%+v, want paused/NULL", status, stopKind)
		}
	})

	t.Run("an operator verdict is not overwritten", func(t *testing.T) {
		s := seed(t, true, true, "running")
		exec(t, `UPDATE runs SET stop_kind = 'cancelled', stop_reason = 'operator said so' WHERE id = $1`, s.runID)
		pass(t, newSvc(), s, forgeFor(s, forge.MRStateMerged))
		_, stopKind, stopReason, _ := read(t, s.runID)
		if !stopKind.Valid || stopKind.String != "cancelled" || stopReason.String != "operator said so" || cancelInputs(t, s.runID) != 0 {
			t.Fatalf("stop_kind=%+v stop_reason=%+v inputs=%d, want the operator verdict intact and no new input", stopKind, stopReason, cancelInputs(t, s.runID))
		}
	})
}

func goodFor(s seeded, state string) forge.MergeRequestSummary {
	return forge.MergeRequestSummary{IID: 5151, State: state, SourceBranch: s.branch, HeadSHA: activeTestSHA}
}
