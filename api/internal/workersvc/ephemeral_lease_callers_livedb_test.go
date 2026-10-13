package workersvc

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// PRD #2006 reviewer finding: the lease interval reached ClaimRun and
// CountOnlineWorkersClaimableForRun only from the leased claimant's own path, so every other caller
// passed SQL NULL and a leased ephemeral worker was invisible to them. These tests go through the
// real Service methods (never a fixture passing the interval itself), the seam the bug hid behind.

// TestClaimSpreadDefersToLeasedEphemeralPeerLiveDB: a busy persistent worker claiming through
// Service.Claim defers a fresh queued run to a LEASED, idle ephemeral peer that may claim it through
// its lease (ClaimRun's spread-peer clause), so the claim is idle. With the lease off the peer is
// not a deferral target and the same worker claims the run, which is the positive control.
func TestClaimSpreadDefersToLeasedEphemeralPeerLiveDB(t *testing.T) {
	e := newLeaseEnv(t)
	peer, _, iid := e.seedLeasedWorker(t)
	e.exec(`UPDATE workers SET max_concurrent_runs = 2 WHERE id = $1`, peer)

	// The claimant: persistent, cap 2, already running one run so that the idle peer is strictly
	// less loaded (a minimum-loaded worker never defers).
	claimant := uuid.New()
	e.exec(`INSERT INTO workers (id, user_id, name, token_hash, status, last_heartbeat_at, max_concurrent_runs, snapshot_register_nonce)
	        VALUES ($1, $2, 'persistent', $3, 'online', now(), 2, 'nonce-A')`, claimant, e.userID, claimant[:])
	busy := uuid.New()
	e.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, worker_id, claim_generation)
	        VALUES ($1, $2, $3, 'issue', $4, 't', 'd', 'running', $5, 1)`, busy, e.userID, e.repoID, nextLeaseIID(), claimant)

	follow := e.seedFollowUp(t, iid, agentIssueBranch(iid))

	on := e.service(2*time.Hour, e.pool)
	if r := awaitClaim(t, e.claimNoSnapshotAsync(on, claimant)); r.err != nil || r.payload != nil {
		t.Fatalf("lease on: claim = (%+v, %v), want idle (deferred to the leased peer)", r.payload, r.err)
	}
	if s := e.runStatusOf(t, follow); s != "queued" {
		t.Fatalf("lease on: follow-up = %q, want still queued", s)
	}

	off := e.service(0, e.pool)
	r := awaitClaim(t, e.claimNoSnapshotAsync(off, claimant))
	if r.err != nil || r.payload == nil || r.payload.RunID != follow.String() {
		t.Fatalf("lease off: claim = (%+v, %v), want the follow-up %s", r.payload, r.err, follow)
	}
}

// TestQueuedReasonCountsLeasedEphemeralWorkerLiveDB: the queued-health resolver's claimable count
// includes a leased ephemeral worker that may claim the run, so a released-worker run is NOT told to
// "restart worker". With the lease off the same state does name the released worker (control).
func TestQueuedReasonCountsLeasedEphemeralWorkerLiveDB(t *testing.T) {
	e := newLeaseEnv(t)
	_, _, iid := e.seedLeasedWorker(t)

	released := uuid.New()
	e.exec(`INSERT INTO workers (id, user_id, name, token_hash, status, last_heartbeat_at, snapshot_register_nonce)
	        VALUES ($1, $2, 'the-released', $3, 'online', now(), 'nonce-A')`, released, e.userID, released[:])
	follow := e.seedFollowUp(t, iid, agentIssueBranch(iid))
	e.exec(`UPDATE runs SET released_worker_id = $2, released_worker_nonce = 'nonce-A', claim_generation = 2,
	               started_at = now() - interval '2 hours', status_since = now() - interval '5 minutes' WHERE id = $1`, follow, released)

	reason := func(svc *Service) string {
		rows, err := svc.q.ListActiveRunsForHealth(e.ctx, nil)
		if err != nil {
			t.Fatalf("ListActiveRunsForHealth: %v", err)
		}
		for _, r := range rows {
			if r.ID == follow {
				return svc.queuedReason(e.ctx, time.Now(), r)
			}
		}
		t.Fatalf("run %s not listed for health", follow)
		return ""
	}
	restart := "waiting for another worker, or restart worker the-released (its previous process was released at the time limit)"

	if got := reason(e.service(0, e.pool)); got != restart {
		t.Fatalf("lease off: queuedReason = %q, want %q", got, restart)
	}
	if got := reason(e.service(2*time.Hour, e.pool)); got == restart {
		t.Fatalf("lease on: queuedReason = %q, the leased worker must count as able to claim", got)
	}
}
