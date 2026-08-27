package store_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// The ephemeral claim restriction (PRD #529 M3, Decision 4) against a REAL Postgres —
// the routing invariant the fake-store unit tests cannot cover because they never run
// the SQL. An ephemeral worker exists to serve exactly one run and must never take
// foreign work on EITHER claim lane, or the busy-guarded teardown (M4) is blocked by a
// non-owning run. These tests prove, against real queue state:
//   - the RUN lane (ClaimRun) claimant clause: an ephemeral worker claims ONLY its
//     bound run even when a second eligible run of the same user is offered;
//   - the RUN lane peer-preemption guard: an ephemeral peer bound to ANOTHER run is
//     never a deferral target (so it can never make a foreign run unclaimable), while
//     an ephemeral peer bound to the candidate run IS a valid target (the full
//     predicate, not a bare NOT p.ephemeral);
//   - the CHAT lane (ClaimChatRun) flat exclusion: an ephemeral (run-bound) worker
//     never claims a chat;
//   - a NON-ephemeral worker's claim behaviour is unchanged on both lanes.
//
// Reuses fleetFixture (claim_fleet_placement_integration_test.go, same package) and its
// queuedRunWithCaps helper (ephemeral_provision_integration_test.go). Skipped unless
// UZI_TEST_DATABASE_URL points at a throwaway Postgres; e2e/run-store-it.sh provides one.

// ephemeralWorker seeds an ONLINE ephemeral hosted worker bound to boundRun, so it is a
// real row the peer-eligibility subquery reads (heartbeat now, a concrete cap, not
// draining). Mirrors fleetFixture.worker plus the hosted metadata ck_workers_hosted_metadata
// requires and the ephemeral marker/binding.
func ephemeralWorker(fx *fleetFixture, name string, boundRun uuid.UUID, docker bool) uuid.UUID {
	fx.t.Helper()
	id := uuid.New()
	mustExec(fx.ctx, fx.t, fx.pool,
		`INSERT INTO workers (id, user_id, name, token_hash, status, last_heartbeat_at, max_concurrent_runs,
		                      docker_enabled, kind, template_declared, hosted_size, ephemeral, ephemeral_run_id)
		 VALUES ($1, $2, $3, $4, 'online', now(), 2, $5, 'hosted', 'base', 'm', true, $6)`,
		id, fx.userID, name, tokenHash(), docker, boundRun)
	return id
}

// claimEphemeral threads the two PRD #529 M3 params (@is_ephemeral, @ephemeral_run_id)
// through ClaimRun alongside the capability params, so a store test can exercise the
// claimant clause directly. ClaimRun identifies the claimant purely by params, so no
// worker row is strictly required for the claimant — but the peer-guard tests seed real
// peer rows since the spread subquery reads the workers table.
func claimEphemeral(fx *fleetFixture, workerID uuid.UUID, isEph bool, boundRun uuid.UUID, isDocker bool, allow []uuid.UUID, caps []string) (store.Run, error) {
	p := store.ClaimRunParams{
		WorkerID:            pgtype.UUID{Bytes: workerID, Valid: true},
		UserID:              fx.userID,
		AffinityCutoff:      pgtype.Timestamptz{Time: time.Now().Add(-2 * time.Minute), Valid: true},
		SpreadCutoff:        pgtype.Timestamptz{Time: time.Now().Add(-9 * time.Second), Valid: true},
		HeartbeatCutoff:     pgtype.Timestamptz{Time: time.Now().Add(-45 * time.Second), Valid: true},
		IsDockerWorker:      isDocker,
		DockerRepoAllowlist: allow,
		WorkerCaps:          caps,
		CapabilityAware:     true,
		IsEphemeral:         isEph,
	}
	if isEph {
		p.EphemeralRunID = pgtype.UUID{Bytes: boundRun, Valid: true}
	}
	return fx.q.ClaimRun(fx.ctx, p)
}

// TestClaimRunEphemeralWorkerClaimsOnlyBoundRunLiveDB is the M3 success criterion for the
// RUN lane: an ephemeral docker worker bound to run A, offered A AND a second eligible
// docker run B of the same user, claims ONLY A — and a second call is idle because B is
// off-limits (the claimant clause) and A is now claimed. A normal docker worker then
// claims B, proving the restriction is scoped to ephemeral workers.
func TestClaimRunEphemeralWorkerClaimsOnlyBoundRunLiveDB(t *testing.T) {
	fx := newFleetFixture(t)
	runA := queuedRunWithCaps(fx, []string{"docker"}) // E's bound run (oldest)
	runB := queuedRunWithCaps(fx, []string{"docker"}) // a second eligible docker run

	e := ephemeralWorker(fx, "E", runA, true) // docker, bound to A

	// First claim: only A is eligible for E (B.id != ephemeral_run_id), so E claims A.
	c, err := claimEphemeral(fx, e, true, runA, true, []uuid.UUID{fx.repoID}, nil)
	if err != nil {
		t.Fatalf("ephemeral worker must claim its bound run A: %v", err)
	}
	if c.ID != runA {
		t.Fatalf("ephemeral worker claimed %s, want its bound run %s", c.ID, runA)
	}
	// Second claim: A is now claimed and B is off-limits, so E is idle — it must NOT take B.
	if _, err := claimEphemeral(fx, e, true, runA, true, []uuid.UUID{fx.repoID}, nil); err != pgx.ErrNoRows {
		t.Fatalf("ephemeral worker must NOT claim the foreign run B; got %v, want pgx.ErrNoRows", err)
	}

	// A NON-ephemeral docker worker's behaviour is unchanged: it claims B.
	n := fx.worker("N", capOf(2), true)
	cn, err := fx.q.ClaimRun(fx.ctx, store.ClaimRunParams{
		WorkerID:            pgtype.UUID{Bytes: n, Valid: true},
		UserID:              fx.userID,
		AffinityCutoff:      pgtype.Timestamptz{Time: time.Now().Add(-2 * time.Minute), Valid: true},
		SpreadCutoff:        pgtype.Timestamptz{Time: time.Now().Add(-9 * time.Second), Valid: true},
		HeartbeatCutoff:     pgtype.Timestamptz{Time: time.Now().Add(-45 * time.Second), Valid: true},
		IsDockerWorker:      true,
		DockerRepoAllowlist: []uuid.UUID{fx.repoID},
		CapabilityAware:     true,
	})
	if err != nil {
		t.Fatalf("a non-ephemeral docker worker must claim the remaining docker run B: %v", err)
	}
	if cn.ID != runB {
		t.Fatalf("non-ephemeral worker claimed %s, want %s", cn.ID, runB)
	}
}

// TestClaimChatRunEphemeralWorkerNeverClaimsChatLiveDB is the M3 success criterion for the
// CHAT lane: an ephemeral (run-bound) worker offered a queued chat run is idle, while a
// non-ephemeral worker claims the same chat — proving the flat @is_ephemeral exclusion
// fences ONLY the ephemeral worker and the chat is genuinely claimable.
func TestClaimChatRunEphemeralWorkerNeverClaimsChatLiveDB(t *testing.T) {
	fx := newFleetFixture(t)
	chat, err := fx.q.CreateChatRun(fx.ctx, store.CreateChatRunParams{
		RunID: uuid.New(), UserID: fx.userID, IssueTitle: "q", IssueDescription: "q",
		Title: pgtype.Text{String: "q", Valid: true},
	})
	if err != nil {
		t.Fatalf("CreateChatRun: %v", err)
	}

	// An ephemeral worker bound to some run must NOT claim the chat.
	bound := queuedRunWithCaps(fx, []string{"docker"})
	e := ephemeralWorker(fx, "E", bound, true)
	if _, err := fx.q.ClaimChatRun(fx.ctx, store.ClaimChatRunParams{
		WorkerID:       pgtype.UUID{Bytes: e, Valid: true},
		UserID:         fx.userID,
		IsEphemeral:    true,
		AffinityCutoff: pgtype.Timestamptz{Time: time.Now().Add(-2 * time.Minute), Valid: true},
	}); err != pgx.ErrNoRows {
		t.Fatalf("an ephemeral worker must NOT claim a chat run; got %v, want pgx.ErrNoRows", err)
	}

	// A non-ephemeral worker claims the same chat — the exclusion is scoped to ephemeral.
	n := fx.worker("N", capOf(2), false)
	claimed, err := fx.q.ClaimChatRun(fx.ctx, store.ClaimChatRunParams{
		WorkerID:       pgtype.UUID{Bytes: n, Valid: true},
		UserID:         fx.userID,
		IsEphemeral:    false,
		AffinityCutoff: pgtype.Timestamptz{Time: time.Now().Add(-2 * time.Minute), Valid: true},
	})
	if err != nil {
		t.Fatalf("a non-ephemeral worker must claim the chat run: %v", err)
	}
	if claimed.ID != chat.ID || claimed.Kind != "chat" {
		t.Fatalf("non-ephemeral worker claimed %s (kind %q), want the chat run %s", claimed.ID, claimed.Kind, chat.ID)
	}
}

// TestClaimRunPeerGuardExcludesEphemeralPeerLiveDB is the M3 peer-preemption guard. It
// constructs the load conditions so the fleet-spread clause is ACTIVE (a busy claimant W
// that would normally defer to a strictly-less-loaded idle peer), then proves the guard's
// FULL predicate `(NOT p.ephemeral OR p.ephemeral_run_id = r.id)`:
//   - an ephemeral peer bound to ANOTHER run is excluded, so W does NOT defer the foreign
//     run to it (which would make the run unclaimable, since the peer only ever claims its
//     own bound run) and W claims it itself; and
//   - an ephemeral peer bound to the CANDIDATE run IS a valid deferral target, so a busy W
//     correctly defers to it — the reason a bare `AND NOT p.ephemeral` would be wrong.
func TestClaimRunPeerGuardExcludesEphemeralPeerLiveDB(t *testing.T) {
	// Case 1: ephemeral peer bound to a DIFFERENT run — not a deferral target, W claims R.
	t.Run("ephemeral peer bound to another run is excluded", func(t *testing.T) {
		fx := newFleetFixture(t)
		w := fx.worker("W", capOf(2), false) // non-docker claimant
		fx.holdActive(w, 1)                  // W busy (active 1): would defer to a less-loaded eligible peer

		// E's bound run is terminal, so it is not itself a claim candidate; only R is queued.
		other := uuid.New()
		mustExec(fx.ctx, fx.t, fx.pool,
			`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, worker_id)
			 VALUES ($1, $2, $3, $4, 't', 'd', 'completed', NULL)`,
			other, fx.userID, fx.repoID, fx.nextIID())
		ephemeralWorker(fx, "E", other, false) // idle ephemeral peer bound to the terminal run

		r := fx.queuedRun() // the fresh foreign run W must be able to claim
		c, err := fx.claim(w, false, nil)
		if err != nil {
			t.Fatalf("W must claim R: an ephemeral peer bound to another run is not a deferral target; got %v", err)
		}
		if c.ID != r {
			t.Fatalf("W claimed %s, want the foreign run %s (R must not be deferred to the ephemeral peer)", c.ID, r)
		}
	})

	// Case 2: ephemeral peer bound to the CANDIDATE run IS a valid target — W defers, and
	// the bound ephemeral worker then claims its own run. Proves the full predicate.
	t.Run("ephemeral peer bound to the candidate run stays a valid target", func(t *testing.T) {
		fx := newFleetFixture(t)
		w := fx.worker("W", capOf(2), false)
		fx.holdActive(w, 1) // W busy

		r := fx.queuedRun()                     // the candidate run
		e := ephemeralWorker(fx, "E", r, false) // idle ephemeral peer bound to R

		// R is E's own bound run, so E is a valid deferral target: W defers rather than claim.
		if _, err := fx.claim(w, false, nil); err != pgx.ErrNoRows {
			t.Fatalf("W must defer R to the ephemeral peer bound to R; got %v, want pgx.ErrNoRows", err)
		}
		// The bound ephemeral worker then claims its own run (the deferral had a real taker).
		c, err := claimEphemeral(fx, e, true, r, false, nil, nil)
		if err != nil {
			t.Fatalf("the ephemeral worker must claim its own bound run R: %v", err)
		}
		if c.ID != r {
			t.Fatalf("ephemeral worker claimed %s, want its bound run %s", c.ID, r)
		}
	})
}
