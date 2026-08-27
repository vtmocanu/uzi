package store_test

import (
	"testing"

	"github.com/google/uuid"
)

// The effective-worker-caps docker fold, single-sourced into fn_effective_worker_caps
// (migration 00151, issue #512 M5), against a REAL Postgres. A green sqlc generate does
// not execute either SQL function (both resolve at prepare time, not generation), so this
// is the only proof that (a) fn_effective_worker_caps folds `docker` into a worker's caps
// exactly as the four old inline copies did, and (b) the CREATE OR REPLACE of
// fn_worker_can_claim preserved its claim predicate while delegating the fold.
//
// Reuses newFleetFixture (which Skips unless UZI_TEST_DATABASE_URL points at a throwaway
// Postgres, via e2e/run-store-it.sh) purely for its migrated pool — these functions are
// pure, so no fleet seeding is needed.

// TestEffectiveWorkerCapsFoldLiveDB pins the fold truth table, including NULL caps and the
// non-dedup case. Both arguments are bound directly in SQL (NULL::text[] for the NULL case,
// ARRAY[...] literals otherwise) so no nil Go slice is passed for a text[] argument.
func TestEffectiveWorkerCapsFoldLiveDB(t *testing.T) {
	fx := newFleetFixture(t)

	fold := func(capsExpr string, isDocker bool) []string {
		t.Helper()
		var got []string
		err := fx.pool.QueryRow(fx.ctx,
			`SELECT fn_effective_worker_caps(`+capsExpr+`, $1)`, isDocker).Scan(&got)
		if err != nil {
			t.Fatalf("fn_effective_worker_caps(%s, %v): %v", capsExpr, isDocker, err)
		}
		return got
	}

	cases := []struct {
		name     string
		capsExpr string
		isDocker bool
		want     []string
	}{
		{"null-no-docker", `NULL::text[]`, false, []string{}},
		{"null-docker", `NULL::text[]`, true, []string{"docker"}},
		{"empty-no-docker", `ARRAY[]::text[]`, false, []string{}},
		{"empty-docker", `ARRAY[]::text[]`, true, []string{"docker"}},
		{"jvm-no-docker", `ARRAY['jvm']::text[]`, false, []string{"jvm"}},
		{"jvm-docker", `ARRAY['jvm']::text[]`, true, []string{"jvm", "docker"}},
		// Non-dedup: a worker already carrying `docker` yields two, matching the Go/TS folds.
		{"docker-docker", `ARRAY['docker']::text[]`, true, []string{"docker", "docker"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := fold(tc.capsExpr, tc.isDocker)
			if !equalStrings(got, tc.want) {
				t.Errorf("fn_effective_worker_caps(%s, %v) = %v, want %v", tc.capsExpr, tc.isDocker, got, tc.want)
			}
		})
	}
}

// TestWorkerCanClaimEquivalenceLiveDB pins a representative matrix of the claim predicate
// after the CREATE OR REPLACE (issue #512 M5) delegated its fold to fn_effective_worker_caps
// — proving the allowlist clause and the capability subset clause still evaluate as they did
// in migration 00142. Includes a positive AND a negative control.
func TestWorkerCanClaimEquivalenceLiveDB(t *testing.T) {
	fx := newFleetFixture(t)
	repo := uuid.New()

	// canClaim invokes fn_worker_can_claim with the 7-arg signature. repoID is passed as a
	// *uuid.UUID so a nil pointer binds SQL NULL (the repo-less judge case); allowlist,
	// worker_caps and required are non-nil slices so none encodes as NULL.
	canClaim := func(isDocker bool, allowlist []uuid.UUID, repoID *uuid.UUID, kind string, workerCaps, required []string, capAware bool) bool {
		t.Helper()
		var got bool
		err := fx.pool.QueryRow(fx.ctx,
			`SELECT fn_worker_can_claim($1, $2, $3, $4, $5, $6, $7)`,
			isDocker, allowlist, repoID, kind, workerCaps, required, capAware).Scan(&got)
		if err != nil {
			t.Fatalf("fn_worker_can_claim: %v", err)
		}
		return got
	}

	empty := []string{}
	noAllow := []uuid.UUID{}
	allow := []uuid.UUID{repo}

	// Positive control: a non-docker worker with capability-awareness off short-circuits the
	// fence (NOT is_docker) and the capability clause (NOT capability_aware) → claimable.
	if !canClaim(false, noAllow, &repo, "task", empty, empty, false) {
		t.Errorf("non-docker worker, capability_aware off: want claimable")
	}

	// Negative control: a docker worker on a NON-allowlisted repo fails the fence → barred.
	if canClaim(true, noAllow, &repo, "task", empty, empty, false) {
		t.Errorf("docker worker, empty allowlist, non-judge repo: want barred")
	}

	// Docker worker on an ALLOWLISTED repo passes the fence.
	if !canClaim(true, allow, &repo, "task", empty, empty, false) {
		t.Errorf("docker worker, repo allowlisted: want claimable")
	}

	// Judge exemption: a docker worker with a repo-less judge run is exempt from the fence
	// even with an empty allowlist.
	if !canClaim(true, noAllow, nil, "judge", empty, empty, false) {
		t.Errorf("docker worker, repo-less judge run: want claimable (fence exempt)")
	}

	// Capability clause, match: capability_aware on, required {jvm} ⊆ worker {jvm} → claimable.
	if !canClaim(false, noAllow, &repo, "task", []string{"jvm"}, []string{"jvm"}, true) {
		t.Errorf("non-docker worker with jvm, {jvm} required, capability_aware: want claimable")
	}

	// Capability clause, mismatch: required {jvm} not a subset of an empty worker set → barred.
	if canClaim(false, noAllow, &repo, "task", empty, []string{"jvm"}, true) {
		t.Errorf("non-docker worker without jvm, {jvm} required, capability_aware: want barred")
	}

	// The fold in action: a docker worker (is_docker=true) with NO stored caps satisfies a
	// {docker} requirement because fn_effective_worker_caps folds `docker` into its effective
	// set — and it is on the allowlist so the fence also passes.
	if !canClaim(true, allow, &repo, "task", empty, []string{"docker"}, true) {
		t.Errorf("docker worker (allowlisted), {docker} required, capability_aware: want claimable (fold satisfies it)")
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
