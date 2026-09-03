package workersvc

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/runkind"
)

// checkpoint_branch_contract_test.go is the GO HALF of the checkpoint branch-derivation
// cross-language contract (PRD #1062 M3); the node:test half is
// agent/test/checkpoint-branch-contract.test.ts. Neither reads the other: each folds its
// own production derivation and compares against the SAME hand-authored fixture,
// fixtures/checkpoint-branch/cases.json.
//
// 🔴 This fixture sits ABOVE the api/ module, so it contributes nothing to this package's
// test-cache key: a fixture-only edit leaves a bare `go test ./internal/workersvc/`
// printing "ok (cached)" over a changed fixture. The gate's -count=1 (task test:api /
// task gate:api) is what makes this test live; a bare scoped `go test` without -count=1
// will not catch a fixture drift. See the fixture's README for the full asymmetry.

// checkpointBranchCases is the hand-authored fixture shape.
type checkpointBranchCases struct {
	Eligible []struct {
		Kind     string `json:"kind"`
		RunID    string `json:"run_id"`
		IssueIid int64  `json:"issue_iid"`
		Branch   string `json:"branch"`
	} `json:"eligible"`
	Ineligible []string `json:"ineligible"`
}

// readCheckpointBranchFixture is a throw-on-unreadable wrapper (mirroring the
// run-kinds contract): a missing/unreadable fixture is a fatal naming the path, never a
// skip — a skipped contract asserts nothing and would look identical to passing.
func readCheckpointBranchFixture(t *testing.T) checkpointBranchCases {
	t.Helper()
	path := filepath.Join("..", "..", "..", "fixtures", "checkpoint-branch", "cases.json")
	b, err := os.ReadFile(path) //nolint:gosec // G304: test reads a fixed repo-relative fixture path
	if err != nil {
		t.Fatalf("fixture unreadable: %s: %v -- this contract asserts nothing without it, "+
			"and skipping would look identical to passing", path, err)
	}
	var cases checkpointBranchCases
	if err := json.Unmarshal(b, &cases); err != nil {
		t.Fatalf("fixture %s is not valid JSON: %v", path, err)
	}
	return cases
}

// TestCheckpointBranchContract pins the server-side checkpointBranch derivation against
// the hand-authored cross-language fixture: each eligible case's branch, the exact set of
// eligible kinds (a drift guard), and that every ineligible kind returns ok=false.
func TestCheckpointBranchContract(t *testing.T) {
	cases := readCheckpointBranchFixture(t)

	// A fixed uuid used wherever a case's own run_id is not the thing under test (the
	// eligible-kind drift guard and the ineligible cases).
	fixedID := uuid.MustParse("33333333-3333-4333-8333-333333333333")
	validIid := pgtype.Int8{Int64: 999, Valid: true}

	// Each eligible case: the branch derives exactly as the fixture states.
	fixtureEligibleKinds := map[string]bool{}
	for _, c := range cases.Eligible {
		fixtureEligibleKinds[c.Kind] = true
		got, ok := checkpointBranch(c.Kind, uuid.MustParse(c.RunID), pgtype.Int8{Int64: c.IssueIid, Valid: true})
		if !ok {
			t.Errorf("checkpointBranch(%q, %s, %d) ok=false, want an eligible kind", c.Kind, c.RunID, c.IssueIid)
			continue
		}
		if got != c.Branch {
			t.Errorf("checkpointBranch(%q, %s, %d) = %q, want %q", c.Kind, c.RunID, c.IssueIid, got, c.Branch)
		}
	}

	// Drift guard: the kinds checkpointBranch returns ok=true for MUST equal the
	// fixture's eligible-kind set. Iterate every DB run kind with a valid iid + a fixed
	// uuid; this catches a newly-enabled kind the fixture forgot (and vice versa).
	prodEligibleKinds := map[string]bool{}
	for _, kind := range runkind.All() {
		if _, ok := checkpointBranch(kind, fixedID, validIid); ok {
			prodEligibleKinds[kind] = true
		}
	}
	if !stringSetEqual(fixtureEligibleKinds, prodEligibleKinds) {
		t.Errorf("eligible-kind set drift: fixture=%v, checkpointBranch ok=true for=%v "+
			"(a newly-enabled kind must be added to fixtures/checkpoint-branch/cases.json, "+
			"or a removed one taken out)", sortedKeys(fixtureEligibleKinds), sortedKeys(prodEligibleKinds))
	}

	// Every ineligible kind returns ok=false, even with a valid iid.
	for _, kind := range cases.Ineligible {
		if branch, ok := checkpointBranch(kind, fixedID, validIid); ok {
			t.Errorf("checkpointBranch(%q, ...) ok=true (branch %q), want ok=false for an ineligible kind", kind, branch)
		}
	}

	// The ELIGIBLE issue kind with a MISSING iid must still be unsupported — the
	// `if !issueIid.Valid` guard in checkpointBranch. Without this assertion a regressed
	// guard would hand an iid-less issue run a checkpoint branch (issue #1037 review).
	if branch, ok := checkpointBranch(runkind.Issue, fixedID, pgtype.Int8{}); ok || branch != "" {
		t.Errorf("checkpointBranch(issue, missing iid) = (%q, %t), want (\"\", false)", branch, ok)
	}
}

func stringSetEqual(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

func sortedKeys(m map[string]bool) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
