package workersvc

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// job_revoke_sweep_livedb_test.go covers the PRD #1908 M6 product-revoke sweep
// (CancelRevokedProductJobs) against a live Postgres. Skipped unless UZI_TEST_DATABASE_URL points
// at a throwaway database; run via ./e2e/run-store-it.sh.

// seedOriginJob seeds a raw job run plus its job_origins row for the given product/token (both
// nil records a uzc_ caller).
func (e jobEnv) seedOriginJob(t *testing.T, userID uuid.UUID, status string, workerID *uuid.UUID, product, token *uuid.UUID) uuid.UUID {
	t.Helper()
	id := e.seedRawJob(t, userID, status, workerID, 10*time.Second, 3600)
	e.exec(`INSERT INTO job_origins (run_id, product_id, product_token_id) VALUES ($1, $2, $3)`, id, product, token)
	return id
}

func (e jobEnv) pendingCancels(t *testing.T, runID uuid.UUID) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(e.ctx,
		`SELECT count(*) FROM run_user_inputs WHERE run_id = $1 AND kind = 'cancel' AND consumed_at IS NULL`, runID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestCancelRevokedProductJobsLiveDB(t *testing.T) {
	triggers := []struct {
		name string
		// fire withdraws the authorization of the owner's product token / product / account.
		fire func(e jobEnv, owner, product, token uuid.UUID)
	}{
		{"single token revoke", func(e jobEnv, owner, _, token uuid.UUID) {
			if n, err := e.q.RevokeProductToken(e.ctx, store.RevokeProductTokenParams{ID: token, UserID: owner}); err != nil || n != 1 {
				t.Fatalf("RevokeProductToken = %d, %v; want 1, nil", n, err)
			}
		}},
		{"admin token revoke", func(e jobEnv, _, _, token uuid.UUID) {
			if n, err := e.q.AdminRevokeProductToken(e.ctx, token); err != nil || n != 1 {
				t.Fatalf("AdminRevokeProductToken = %d, %v; want 1, nil", n, err)
			}
		}},
		{"revoke all", func(e jobEnv, owner, _, _ uuid.UUID) {
			if err := e.q.RevokeAllProductTokens(e.ctx, owner); err != nil {
				t.Fatal(err)
			}
		}},
		{"product disabled", func(e jobEnv, _, product, _ uuid.UUID) {
			if _, err := e.q.UpdateProduct(e.ctx, store.UpdateProductParams{ID: product, Enabled: pgtype.Bool{Bool: false, Valid: true}}); err != nil {
				t.Fatal(err)
			}
		}},
		// SoftDeleteProduct sets deleted_at and clears enabled together, and the
		// products_deleted_is_disabled CHECK forbids a deleted-but-enabled row, so the query's
		// deleted_at clause is belt-and-braces and cannot be exercised on its own.
		{"product deleted", func(e jobEnv, _, product, _ uuid.UUID) {
			if n, err := e.q.SoftDeleteProduct(e.ctx, product); err != nil || n != 1 {
				t.Fatalf("SoftDeleteProduct = %d, %v; want 1, nil", n, err)
			}
		}},
		{"owner deactivated", func(e jobEnv, owner, _, _ uuid.UUID) {
			if _, err := e.q.SetUserActive(e.ctx, store.SetUserActiveParams{ID: owner, IsActive: false}); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range triggers {
		t.Run(tc.name, func(t *testing.T) {
			e := setupJobLiveDB(t, 0)
			owner := e.seedJobUser(t)
			product, token := e.seedProduct(t, owner, []string{"research"})
			w := e.seedWorkerRow(t, owner, false, nil, jobCap)
			queued := e.seedOriginJob(t, owner, "queued", nil, &product, &token)
			running := e.seedOriginJob(t, owner, "running", &w, &product, &token)

			// A bystander of another user and product must never be selected.
			other := e.seedJobUser(t)
			otherProduct, otherToken := e.seedProduct(t, other, []string{"research"})
			bystander := e.seedOriginJob(t, other, "queued", nil, &otherProduct, &otherToken)

			// Nothing is withdrawn yet: the pass changes nothing.
			if n, err := e.svc.CancelRevokedProductJobs(e.ctx); err != nil || n != 0 {
				t.Fatalf("pass with nothing revoked = %d, %v; want 0, nil", n, err)
			}
			if e.status(t, queued) != "queued" || e.status(t, running) != "running" {
				t.Fatal("a pass with nothing revoked must not touch the jobs")
			}
			if e.pendingCancels(t, queued) != 0 || e.pendingCancels(t, running) != 0 {
				t.Fatal("no cancel may be pending before the trigger fires")
			}

			tc.fire(e, owner, product, token)
			// Two jobs of the owner are selected. (owner deactivated also selects none of the
			// bystander's, who is another user.)
			if n, err := e.svc.CancelRevokedProductJobs(e.ctx); err != nil || n != 2 {
				t.Fatalf("pass after %s = %d, %v; want 2, nil", tc.name, n, err)
			}
			if s := e.status(t, queued); s != "cancelled" {
				t.Fatalf("queued job status = %q, want cancelled server-side", s)
			}
			// A running job is cancelled by an input its runner polls, not flipped here.
			if s := e.status(t, running); s != "running" {
				t.Fatalf("running job status = %q, want running until its runner honours the cancel", s)
			}
			if n := e.pendingCancels(t, running); n != 1 {
				t.Fatalf("running job pending cancel inputs = %d, want 1", n)
			}
			if s := e.status(t, bystander); s != "queued" {
				t.Fatalf("another user's job status = %q, want queued", s)
			}

			// Re-running is a no-op: the queued job is terminal, the running one already
			// has its cancel in flight.
			n, err := e.svc.CancelRevokedProductJobs(e.ctx)
			if err != nil {
				t.Fatal(err)
			}
			if n != 0 {
				t.Fatalf("second pass cancelled %d, want 0", n)
			}
			if got := e.pendingCancels(t, running); got != 1 {
				t.Fatalf("running job pending cancel inputs after re-run = %d, want still 1", got)
			}

			// Once the runner consumes the cancel and the run ends, the pass leaves it alone.
			e.exec(`UPDATE run_user_inputs SET consumed_at = now() WHERE run_id = $1`, running)
			e.exec(`UPDATE runs SET status = 'cancelled', finished_at = now() WHERE id = $1`, running)
			if n, err := e.svc.CancelRevokedProductJobs(e.ctx); err != nil || n != 0 {
				t.Fatalf("pass over a finished job = %d, %v; want 0, nil", n, err)
			}
			// The selection itself excludes a terminal job (the service would also refuse it,
			// so the query is asserted directly).
			rows, err := e.q.ListRevokedProductJobs(e.ctx, 1000)
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range rows {
				if r.ID == running || r.ID == queued {
					t.Fatalf("terminal job %s is still selected by the sweep query", r.ID)
				}
			}
		})
	}
}

// A job created with a uzc_ token has no product origin: token and product revokes cannot select
// it, and only its owner's deactivation cancels it.
func TestCancelRevokedProductJobsCLIJobLiveDB(t *testing.T) {
	e := setupJobLiveDB(t, 0)
	owner := e.seedJobUser(t)
	product, token := e.seedProduct(t, owner, []string{"research"})
	w := e.seedWorkerRow(t, owner, false, nil, jobCap)
	queued := e.seedOriginJob(t, owner, "queued", nil, nil, nil)
	running := e.seedOriginJob(t, owner, "running", &w, nil, nil)

	e.exec(`UPDATE product_tokens SET revoked = true WHERE id = $1`, token)
	e.exec(`UPDATE products SET enabled = false WHERE id = $1`, product)
	if err := e.q.RevokeAllProductTokens(e.ctx, owner); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.CancelRevokedProductJobs(e.ctx); err != nil {
		t.Fatal(err)
	}
	if e.status(t, queued) != "queued" || e.status(t, running) != "running" || e.pendingCancels(t, running) != 0 {
		t.Fatal("a uzc_-created job must be untouched by product revokes")
	}

	e.exec(`UPDATE users SET is_active = false WHERE id = $1`, owner)
	if _, err := e.svc.CancelRevokedProductJobs(e.ctx); err != nil {
		t.Fatal(err)
	}
	if s := e.status(t, queued); s != "cancelled" {
		t.Fatalf("queued uzc_ job of a deactivated owner = %q, want cancelled", s)
	}
	if e.pendingCancels(t, running) != 1 {
		t.Fatal("running uzc_ job of a deactivated owner must get a cancel input")
	}
}

// Ordinary expiry only stops new API calls: a job authorized while its token was valid runs on.
func TestCancelRevokedProductJobsExpiredTokenLiveDB(t *testing.T) {
	e := setupJobLiveDB(t, 0)
	owner := e.seedJobUser(t)
	product, token := e.seedProduct(t, owner, []string{"research"})
	w := e.seedWorkerRow(t, owner, false, nil, jobCap)
	queued := e.seedOriginJob(t, owner, "queued", nil, &product, &token)
	running := e.seedOriginJob(t, owner, "running", &w, &product, &token)
	e.exec(`UPDATE product_tokens SET expires_at = now() - interval '1 hour' WHERE id = $1`, token)

	if _, err := e.svc.CancelRevokedProductJobs(e.ctx); err != nil {
		t.Fatal(err)
	}
	if e.status(t, queued) != "queued" || e.status(t, running) != "running" || e.pendingCancels(t, running) != 0 {
		t.Fatal("an expired but unrevoked token must leave its jobs alone")
	}
}

// A job with an unconsumed cancel input is skipped: its cancel is already in flight.
func TestCancelRevokedProductJobsPendingCancelSkippedLiveDB(t *testing.T) {
	e := setupJobLiveDB(t, 0)
	owner := e.seedJobUser(t)
	product, token := e.seedProduct(t, owner, []string{"research"})
	w := e.seedWorkerRow(t, owner, false, nil, jobCap)
	running := e.seedOriginJob(t, owner, "running", &w, &product, &token)
	e.exec(`INSERT INTO run_user_inputs (run_id, kind) VALUES ($1, 'cancel')`, running)
	e.exec(`UPDATE product_tokens SET revoked = true WHERE id = $1`, token)

	n, err := e.svc.CancelRevokedProductJobs(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 || e.pendingCancels(t, running) != 1 {
		t.Fatalf("pass cancelled %d, pending = %d; want 0 and the single existing cancel", n, e.pendingCancels(t, running))
	}
}

// The stale-worker requeue (RequeueRunsOfStaleWorkers) returns a running job to queued with its
// unconsumed cancel input still pending. A queued job has no poller to consume it, so the sweep
// must still select it and take the server-side queued cancel.
func TestCancelRevokedProductJobsRequeuedWithPendingCancelLiveDB(t *testing.T) {
	e := setupJobLiveDB(t, 0)
	owner := e.seedJobUser(t)
	product, token := e.seedProduct(t, owner, []string{"research"})
	w := e.seedWorkerRow(t, owner, false, nil, jobCap)
	running := e.seedOriginJob(t, owner, "running", &w, &product, &token)

	if n, err := e.q.RevokeProductToken(e.ctx, store.RevokeProductTokenParams{ID: token, UserID: owner}); err != nil || n != 1 {
		t.Fatalf("RevokeProductToken = %d, %v; want 1, nil", n, err)
	}
	if n, err := e.svc.CancelRevokedProductJobs(e.ctx); err != nil || n != 1 {
		t.Fatalf("first pass = %d, %v; want 1, nil", n, err)
	}
	if e.status(t, running) != "running" || e.pendingCancels(t, running) != 1 {
		t.Fatal("running job must hold exactly one pending cancel and stay running")
	}

	// The worker dies and the stale-worker requeue returns the job to queued, its cancel input
	// still unconsumed.
	e.exec(`UPDATE runs SET status = 'queued', worker_id = NULL, claimed_at = NULL, started_at = NULL WHERE id = $1`, running)

	if n, err := e.svc.CancelRevokedProductJobs(e.ctx); err != nil || n != 1 {
		t.Fatalf("pass over the requeued job = %d, %v; want 1, nil", n, err)
	}
	if s := e.status(t, running); s != "cancelled" {
		t.Fatalf("requeued job with a pending cancel status = %q, want cancelled server-side", s)
	}
}
