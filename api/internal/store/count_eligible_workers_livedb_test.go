package store_test

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// workerCaps inserts an online worker under the fixture's user with an explicit
// docker_enabled flag, capability set, and optional draining_since — the columns the
// fleetFixture.worker helper does not expose, needed to exercise the capability clause of
// CountOnlineEligibleWorkersForRepo and to prove draining workers are still counted by it
// (issue #512 M2 review correction). A nil caps slice is normalized to the empty array '{}'
// (capabilities is text[] NOT NULL, so a nil slice would otherwise encode as SQL NULL and
// violate the constraint).
func (fx *fleetFixture) workerCaps(name string, docker bool, caps []string, draining bool) uuid.UUID {
	fx.t.Helper()
	id := uuid.New()
	var drainingSince any
	if draining {
		drainingSince = time.Now()
	}
	// capabilities is text[] NOT NULL; a nil Go slice encodes as SQL NULL (pgx) and would
	// violate the constraint, so normalize to the empty array the column default would give.
	if caps == nil {
		caps = []string{}
	}
	mustExec(fx.ctx, fx.t, fx.pool,
		`INSERT INTO workers (id, user_id, name, token_hash, status, last_heartbeat_at, docker_enabled, capabilities, draining_since)
		 VALUES ($1, $2, $3, $4, 'online', now(), $5, $6, $7)`,
		id, fx.userID, name, tokenHash(), docker, caps, drainingSince)
	return id
}

// TestCountOnlineEligibleWorkersForRepoLiveDB pins issue #512 M2's claim-time eligibility
// count against a REAL Postgres — the only proof the new query runs, since a green sqlc
// generate does not execute it (fn_worker_can_claim is resolved at prepare time, not
// generation). It shares the fleet fixture with claim_fleet_placement_integration_test.go:
// fn_worker_can_claim (migration 00113, extended in 00142) short-circuits the FENCE for a
// non-Docker worker, so a non-Docker worker is always fence-eligible while a Docker worker is
// fence-eligible only when the repo is on the allowlist; the CAPABILITY clause additionally
// requires the run's required set to be a subset of the worker's effective caps whenever
// capability_aware is true.
//
// The fleet is built up across sub-cases (each asserts against the composition named in its
// message), so a mis-wired query cannot pass as a silent 0 — the allowlisted and
// capability-aware cases carry explicit non-vacuity (>0) guards.
func TestCountOnlineEligibleWorkersForRepoLiveDB(t *testing.T) {
	fx := newFleetFixture(t)

	count := func(allow []uuid.UUID, req []string, capAware bool) int64 {
		t.Helper()
		n, err := fx.q.CountOnlineEligibleWorkersForRepo(fx.ctx, store.CountOnlineEligibleWorkersForRepoParams{
			UserID:               fx.userID,
			DockerRepoAllowlist:  allow,
			RepoID:               fx.repoID,
			Kind:                 "task",
			RequiredCapabilities: req,
			CapabilityAware:      capAware,
		})
		if err != nil {
			t.Fatalf("CountOnlineEligibleWorkersForRepo: %v", err)
		}
		return n
	}

	// Two Docker workers online, empty allowlist: neither is eligible for the repo (a
	// Docker worker fails fn_worker_can_claim unless the repo is allowlisted).
	fx.worker("d1", nil, true)
	fx.worker("d2", nil, true)
	if n := count([]uuid.UUID{}, nil, false); n != 0 {
		t.Fatalf("two Docker workers + empty allowlist: eligible = %d, want 0", n)
	}

	// Add a non-Docker worker: it short-circuits fn_worker_can_claim and is always
	// eligible, so exactly one of the three qualifies with an empty allowlist.
	fx.worker("n1", nil, false)
	if n := count([]uuid.UUID{}, nil, false); n != 1 {
		t.Fatalf("2 Docker + 1 non-Docker + empty allowlist: eligible = %d, want 1 (only the non-Docker)", n)
	}

	// Allowlist the repo: the two Docker workers become eligible too, and the non-Docker
	// stays eligible, so all three qualify. Explicit non-vacuity guard so a mis-wired
	// query cannot pass as a silent 0.
	n := count([]uuid.UUID{fx.repoID}, nil, false)
	if n <= 0 {
		t.Fatalf("2 Docker + 1 non-Docker + repo allowlisted: eligible = %d, want > 0 (non-vacuity guard)", n)
	}
	if n != 3 {
		t.Fatalf("2 Docker + 1 non-Docker + repo allowlisted: eligible = %d, want 3 (all eligible)", n)
	}

	// --- issue #512 M2: capability-awareness (capability_aware=true) ---
	//
	// Add a Docker worker that ALSO advertises a non-docker capability {jvm}. A run
	// requiring {jvm} with capability_aware=true: the three existing workers all fail the
	// capability clause ({jvm} not a subset of their empty caps), so only this worker can
	// satisfy the requirement — and only when the repo is allowlisted (it is a Docker
	// worker, so the fence still applies).
	fx.workerCaps("dj", true, []string{"jvm"}, false)

	// Fenced: repo NOT allowlisted → the jvm Docker worker fails the fence, and no other
	// worker has {jvm}, so nothing is eligible.
	if n := count([]uuid.UUID{}, []string{"jvm"}, true); n != 0 {
		t.Fatalf("jvm-required + empty allowlist + capability_aware: eligible = %d, want 0 (the jvm worker is a fenced Docker worker)", n)
	}

	// Allowlisted: the jvm Docker worker passes the fence AND satisfies the capability, so
	// exactly it qualifies (the capless workers do not, under capability_aware). Non-vacuity
	// guard so a globally-zero bug cannot pass.
	if n := count([]uuid.UUID{fx.repoID}, []string{"jvm"}, true); n <= 0 {
		t.Fatalf("jvm-required + repo allowlisted + capability_aware: eligible = %d, want > 0 (non-vacuity guard)", n)
	} else if n != 1 {
		t.Fatalf("jvm-required + repo allowlisted + capability_aware: eligible = %d, want 1 (only the jvm worker)", n)
	}

	// --- issue #512 M2 (review correction): draining is NOT excluded here ---
	//
	// This is an ELIGIBILITY count (docker-allowlist + capability fence), not an availability
	// one: it deliberately does NOT filter draining workers. A non-Docker worker is always
	// fence-eligible, so adding one raises the empty-allowlist count by 1 EVEN WHEN it is
	// draining. Excluding draining here would misattribute a transient all-draining fleet
	// (during a worker roll) to the docker allowlist in queuedReason's rung; the draining/busy
	// axis is owned downstream by CountOnlineWorkersWithFreeSlotForUser instead. Measure
	// before/after: the count MUST rise by exactly 1.
	before := count([]uuid.UUID{}, nil, false)
	fx.workerCaps("nd", false, nil, true) // non-Docker, draining
	after := count([]uuid.UUID{}, nil, false)
	if after != before+1 {
		t.Fatalf("a draining non-Docker worker must still count as eligible: eligible went %d -> %d, want +1 (this rung ignores draining; availability is a separate axis)", before, after)
	}
}
