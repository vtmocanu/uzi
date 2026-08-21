package store_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestCountOnlineEligibleWorkersForRepoLiveDB pins PRD #361's eligibility count against a
// REAL Postgres — the only proof the new query runs, since a green sqlc generate does not
// execute it (fn_worker_can_claim is resolved at prepare time, not generation). It shares
// the fleet fixture with claim_fleet_placement_integration_test.go: fn_worker_can_claim
// (migration 00113) short-circuits NOT is_docker, so a non-Docker worker is always
// eligible while a Docker worker is eligible only when the repo is on the allowlist.
//
// The fleet is built up across sub-cases (each asserts against the composition named in
// its message), so a mis-wired query cannot pass as a silent 0 — the final allowlisted
// case has an explicit non-vacuity guard.
func TestCountOnlineEligibleWorkersForRepoLiveDB(t *testing.T) {
	fx := newFleetFixture(t)

	count := func(allow []uuid.UUID) int64 {
		t.Helper()
		n, err := fx.q.CountOnlineEligibleWorkersForRepo(fx.ctx, store.CountOnlineEligibleWorkersForRepoParams{
			UserID:              fx.userID,
			DockerRepoAllowlist: allow,
			RepoID:              fx.repoID,
			Kind:                "task",
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
	if n := count([]uuid.UUID{}); n != 0 {
		t.Fatalf("two Docker workers + empty allowlist: eligible = %d, want 0", n)
	}

	// Add a non-Docker worker: it short-circuits fn_worker_can_claim and is always
	// eligible, so exactly one of the three qualifies with an empty allowlist.
	fx.worker("n1", nil, false)
	if n := count([]uuid.UUID{}); n != 1 {
		t.Fatalf("2 Docker + 1 non-Docker + empty allowlist: eligible = %d, want 1 (only the non-Docker)", n)
	}

	// Allowlist the repo: the two Docker workers become eligible too, and the non-Docker
	// stays eligible, so all three qualify. Explicit non-vacuity guard so a mis-wired
	// query cannot pass as a silent 0.
	n := count([]uuid.UUID{fx.repoID})
	if n <= 0 {
		t.Fatalf("2 Docker + 1 non-Docker + repo allowlisted: eligible = %d, want > 0 (non-vacuity guard)", n)
	}
	if n != 3 {
		t.Fatalf("2 Docker + 1 non-Docker + repo allowlisted: eligible = %d, want 3 (all eligible)", n)
	}
}
