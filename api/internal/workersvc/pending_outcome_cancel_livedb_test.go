package workersvc

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// This file is the live-DB half of PRD #1391 Run B M3d (part A, D13): the owner-resolved
// blocked-outcome cancel. It exercises against a REAL Postgres the three pieces that touch
// worker_active_runs / workers.pending_overflow_until and therefore cannot be trusted to a
// fake store: hasLivePoller's terminal_pending detection, the atomic owner-scoped
// CancelRunServerSideWithPendingOutcome predicate, and the SubmitInput confirmation gate.
// Skipped unless UZI_TEST_DATABASE_URL is set (setupCodexLiveDB skips).

// setWorkerHeartbeatFresh stamps the worker's heartbeat at now() so hasLivePoller reads it as a
// live poller for its runs UNLESS a pending-outcome lease says otherwise (the discriminator under
// test — without this the NULL heartbeat alone would make every run read non-live and hide the
// lease's own effect).
func setWorkerHeartbeatFresh(t *testing.T, env codexTestEnv, workerID uuid.UUID) {
	t.Helper()
	env.exec(`UPDATE workers SET last_heartbeat_at = now() WHERE id = $1`, workerID)
}

// setRunGeneration pins a run's claim_generation so a lease can be seeded at the exact same
// generation (the D11/D13 fence) or a deliberately different one.
func setRunGeneration(t *testing.T, env codexTestEnv, runID uuid.UUID, gen int64) {
	t.Helper()
	env.exec(`UPDATE runs SET claim_generation = $2 WHERE id = $1`, runID, gen)
}

// seedTerminalPendingLease inserts one worker_active_runs row marking runID as terminal_pending on
// its worker at the given generation, leased until `until`. This is the row that keeps a run's
// outcome protected (D11) and, positively, makes RunHasPendingOutcomeLease / the cancel fire (D13).
func seedTerminalPendingLease(t *testing.T, env codexTestEnv, workerID, runID uuid.UUID, gen int64, until time.Time) {
	t.Helper()
	env.exec(`INSERT INTO worker_active_runs
	            (worker_id, run_id, claim_generation, phase, terminal_pending, terminal_pending_until, snapshot_epoch, reported_at)
	          VALUES ($1, $2, $3, 'running', true, $4, 1, now())`,
		workerID, runID, gen, until)
}

// seedQueuedRun inserts one issue run in 'queued' status with NO worker (the plainest no-live-poller
// run with no pending outcome), for the "today's path is unaffected" assertion.
func seedQueuedRun(t *testing.T, env codexTestEnv, o reevalOwner, issueIID int64) uuid.UUID {
	t.Helper()
	id := uuid.New()
	env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description,
	             status, status_since, anthropic_secret_id, budget_paused_seconds)
	          VALUES ($1, $2, $3, 'issue', $4, 't', 'd', 'queued', now(), $5, 0)`,
		id, o.userID, o.repoID, issueIID, o.altTok)
	return id
}

// TestHasLivePollerPendingOutcomeLeaseLiveDB pins item 1: a run whose owning worker holds an
// unexpired terminal_pending lease at the run's CURRENT generation (or whose owner is under an
// unexpired pending_overflow) has NO live poller for itself, even though that worker's heartbeat
// is fresh. Deleting the RunHasPendingOutcomeLease branch from hasLivePoller reddens the lease and
// overflow sub-tests (they would read live=true off the fresh heartbeat).
func TestHasLivePollerPendingOutcomeLeaseLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	svc := New(env.q, env.box, testParams())
	o := seedReevalOwner(t, env, BindModeAuto, false)
	setWorkerHeartbeatFresh(t, env, o.workerID)

	live := func(id uuid.UUID) bool {
		run, err := svc.GetRun(env.ctx, o.userID, id)
		if err != nil {
			t.Fatalf("GetRun %s: %v", id, err)
		}
		l, err := svc.hasLivePoller(env.ctx, run)
		if err != nil {
			t.Fatalf("hasLivePoller %s: %v", id, err)
		}
		return l
	}

	t.Run("running run with a fresh worker and no lease is live", func(t *testing.T) {
		id := seedRunningRun(t, env, o, 7001, 0)
		setRunGeneration(t, env, id, 5)
		if !live(id) {
			t.Fatal("a running run under a fresh heartbeat with no pending outcome must be live")
		}
	})

	t.Run("terminal_pending lease at the current generation is NOT live", func(t *testing.T) {
		id := seedRunningRun(t, env, o, 7002, 0)
		setRunGeneration(t, env, id, 5)
		seedTerminalPendingLease(t, env, o.workerID, id, 5, time.Now().Add(time.Hour))
		if live(id) {
			t.Fatal("a run with an unexpired terminal_pending lease at its generation has no live poller")
		}
	})

	t.Run("a lease at a DIFFERENT generation does not suppress the poller", func(t *testing.T) {
		id := seedRunningRun(t, env, o, 7003, 0)
		setRunGeneration(t, env, id, 5)
		seedTerminalPendingLease(t, env, o.workerID, id, 4, time.Now().Add(time.Hour)) // gen 4 != run's 5
		if !live(id) {
			t.Fatal("a lease at a stale generation must not be read as this run's pending outcome")
		}
	})

	t.Run("an EXPIRED lease does not suppress the poller", func(t *testing.T) {
		id := seedRunningRun(t, env, o, 7004, 0)
		setRunGeneration(t, env, id, 5)
		seedTerminalPendingLease(t, env, o.workerID, id, 5, time.Now().Add(-time.Minute)) // already expired
		if !live(id) {
			t.Fatal("an expired terminal_pending lease must not suppress the poller")
		}
	})

	t.Run("pending_overflow on the owning worker (run unlisted) is NOT live", func(t *testing.T) {
		id := seedRunningRun(t, env, o, 7005, 0)
		setRunGeneration(t, env, id, 5)
		// No worker_active_runs row for this run at all: the worker-level closure stands in.
		env.exec(`UPDATE workers SET pending_overflow_until = now() + interval '1 hour' WHERE id = $1`, o.workerID)
		defer env.exec(`UPDATE workers SET pending_overflow_until = NULL WHERE id = $1`, o.workerID)
		if live(id) {
			t.Fatal("a run whose owner is under an unexpired pending_overflow has no live poller")
		}
	})
}

// TestCancelRunServerSideWithPendingOutcomeLiveDB pins item 3: the atomic owner-scoped cancel
// terminalises exactly a run with a pending-outcome lease (or under pending_overflow) at the
// current generation, and matches 0 rows for a different generation, an expired lease, a foreign
// user, or a run with no pending outcome. Dropping the pending-outcome predicate from the query
// would let it cancel the no-outcome run (reddening that sub-test); it is a single row-locking
// UPDATE, so there is no Go-side read-then-write here to race.
func TestCancelRunServerSideWithPendingOutcomeLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	o := seedReevalOwner(t, env, BindModeAuto, false)
	q := env.q

	cancel := func(id, userID uuid.UUID) int64 {
		rows, err := q.CancelRunServerSideWithPendingOutcome(env.ctx, store.CancelRunServerSideWithPendingOutcomeParams{
			ID: id, UserID: userID,
		})
		if err != nil {
			t.Fatalf("CancelRunServerSideWithPendingOutcome %s: %v", id, err)
		}
		return rows
	}

	t.Run("terminalises a leased run at the current generation", func(t *testing.T) {
		id := seedRunningRun(t, env, o, 7101, 0)
		setRunGeneration(t, env, id, 3)
		seedTerminalPendingLease(t, env, o.workerID, id, 3, time.Now().Add(time.Hour))
		if got := cancel(id, o.userID); got != 1 {
			t.Fatalf("rows = %d, want 1 (a leased run at its generation cancels)", got)
		}
		if s := statusOf(t, env, id); s != "cancelled" {
			t.Fatalf("status = %q, want cancelled", s)
		}
	})

	t.Run("cancels an unlisted run under pending_overflow", func(t *testing.T) {
		id := seedRunningRun(t, env, o, 7102, 0)
		setRunGeneration(t, env, id, 3)
		env.exec(`UPDATE workers SET pending_overflow_until = now() + interval '1 hour' WHERE id = $1`, o.workerID)
		defer env.exec(`UPDATE workers SET pending_overflow_until = NULL WHERE id = $1`, o.workerID)
		if got := cancel(id, o.userID); got != 1 {
			t.Fatalf("rows = %d, want 1 (an unlisted run under overflow cancels)", got)
		}
		if s := statusOf(t, env, id); s != "cancelled" {
			t.Fatalf("status = %q, want cancelled", s)
		}
	})

	t.Run("a DIFFERENT generation does not cancel", func(t *testing.T) {
		id := seedRunningRun(t, env, o, 7103, 0)
		setRunGeneration(t, env, id, 3)
		seedTerminalPendingLease(t, env, o.workerID, id, 2, time.Now().Add(time.Hour)) // gen 2 != 3
		if got := cancel(id, o.userID); got != 0 {
			t.Fatalf("rows = %d, want 0 (a stale-generation lease is not this run's outcome)", got)
		}
		if s := statusOf(t, env, id); s != "running" {
			t.Fatalf("status = %q, want running (must not cancel)", s)
		}
	})

	t.Run("an EXPIRED lease does not cancel", func(t *testing.T) {
		id := seedRunningRun(t, env, o, 7104, 0)
		setRunGeneration(t, env, id, 3)
		seedTerminalPendingLease(t, env, o.workerID, id, 3, time.Now().Add(-time.Minute))
		if got := cancel(id, o.userID); got != 0 {
			t.Fatalf("rows = %d, want 0 (an expired lease is not a live pending outcome)", got)
		}
	})

	t.Run("a FOREIGN user does not cancel", func(t *testing.T) {
		id := seedRunningRun(t, env, o, 7105, 0)
		setRunGeneration(t, env, id, 3)
		seedTerminalPendingLease(t, env, o.workerID, id, 3, time.Now().Add(time.Hour))
		if got := cancel(id, uuid.New()); got != 0 {
			t.Fatalf("rows = %d, want 0 (owner-scoped: a non-owner cancels nothing)", got)
		}
		if s := statusOf(t, env, id); s != "running" {
			t.Fatalf("status = %q, want running (a foreign cancel must not apply)", s)
		}
	})

	t.Run("a run with NO pending outcome does not cancel through this query", func(t *testing.T) {
		id := seedRunningRun(t, env, o, 7106, 0)
		setRunGeneration(t, env, id, 3)
		// No lease, no overflow.
		if got := cancel(id, o.userID); got != 0 {
			t.Fatalf("rows = %d, want 0 (the pending-outcome predicate must gate this query)", got)
		}
	})
}

// TestSubmitInputPendingOutcomeCancelLiveDB pins items 1/3/4 through SubmitInput: a cancel of a
// pending-outcome run WITHOUT the discard bit is ErrOutcomePendingConfirmationRequired; WITH it the
// run is cancelled server-side; a foreign (e.g. admin_ro) caller resolves 404, never a cancel; and
// a cancel of a run with NO pending outcome takes today's server-side path unchanged.
func TestSubmitInputPendingOutcomeCancelLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	svc := New(env.q, env.box, testParams())
	svc.SetTxBeginner(env.pool) // the discard path runs a FOR UPDATE-locked transaction
	o := seedReevalOwner(t, env, BindModeAuto, false)
	setWorkerHeartbeatFresh(t, env, o.workerID) // fresh, so only the lease makes the run non-live

	seedPending := func(issueIID int64) uuid.UUID {
		id := seedRunningRun(t, env, o, issueIID, 0)
		setRunGeneration(t, env, id, 4)
		seedTerminalPendingLease(t, env, o.workerID, id, 4, time.Now().Add(time.Hour))
		return id
	}

	t.Run("cancel without the discard bit is a confirmation-required refusal", func(t *testing.T) {
		id := seedPending(7201)
		_, err := svc.SubmitInputWithOptions(env.ctx, o.userID, id, "cancel", "", nil, SubmitInputOptions{})
		if !errors.Is(err, ErrOutcomePendingConfirmationRequired) {
			t.Fatalf("err = %v, want ErrOutcomePendingConfirmationRequired", err)
		}
		if s := statusOf(t, env, id); s != "running" {
			t.Fatalf("status = %q, want running (a refused cancel must not transition)", s)
		}
	})

	t.Run("cancel WITH the discard bit cancels server-side", func(t *testing.T) {
		id := seedPending(7202)
		res, err := svc.SubmitInputWithOptions(env.ctx, o.userID, id, "cancel", "operator discard", nil, SubmitInputOptions{DiscardPendingOutcome: true})
		if err != nil {
			t.Fatalf("discard cancel err = %v, want nil", err)
		}
		if !res.ServerSide {
			t.Fatal("a discard cancel of a no-live-poller run must apply server-side")
		}
		if s := statusOf(t, env, id); s != "cancelled" {
			t.Fatalf("status = %q, want cancelled", s)
		}
	})

	t.Run("a foreign (admin_ro) caller gets 404, never a cancel", func(t *testing.T) {
		id := seedPending(7203)
		_, err := svc.SubmitInputWithOptions(env.ctx, uuid.New(), id, "cancel", "", nil, SubmitInputOptions{DiscardPendingOutcome: true})
		if !errors.Is(err, ErrRunNotFound) {
			t.Fatalf("err = %v, want ErrRunNotFound (owner-only; a non-owner cancels nothing)", err)
		}
		if s := statusOf(t, env, id); s != "running" {
			t.Fatalf("status = %q, want running (a foreign discard must not apply)", s)
		}
	})

	t.Run("a cancel of a run with NO pending outcome takes today's path", func(t *testing.T) {
		id := seedQueuedRun(t, env, o, 7204) // queued, no worker → no live poller, no pending outcome
		res, err := svc.SubmitInputWithOptions(env.ctx, o.userID, id, "cancel", "", nil, SubmitInputOptions{})
		if err != nil {
			t.Fatalf("plain cancel err = %v, want nil (no confirmation gate for a non-pending run)", err)
		}
		if !res.ServerSide {
			t.Fatal("a no-poller cancel of a queued run must apply server-side")
		}
		if s := statusOf(t, env, id); s != "cancelled" {
			t.Fatalf("status = %q, want cancelled", s)
		}
	})
}
