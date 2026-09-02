package runkind

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// runkind_fixture_test.go is the GO HALF of the run-kind registry cross-language
// contract; the vitest half is web/src/lib/runKindContract.test.ts (added in M3).
// Neither reads the other: each folds its own production knowledge and compares
// against the SAME hand-authored fixture, fixtures/run-kinds/registry.json.
//
// 🔴 This fixture sits ABOVE the api/ module, so it contributes nothing to this
// package's test-cache key: a fixture-only edit leaves a bare `go test
// ./internal/runkind/` printing "ok (cached)" over a changed fixture. The gate's
// -count=1 (task test:api / task gate:api) is what makes this test live; a bare
// scoped `go test` without -count=1 will not catch a fixture drift. See the
// fixture's README for the full asymmetry.

// readFixture is a throw-on-unreadable wrapper (mirroring runUsageContract.test.ts's
// read()): a missing/unreadable fixture is a fatal naming the path, never a skip —
// a skipped contract asserts nothing and would look identical to passing.
func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	path := filepath.Join("..", "..", "..", "fixtures", "run-kinds", name)
	b, err := os.ReadFile(path) //nolint:gosec // G304: test reads a fixed repo-relative fixture path
	if err != nil {
		t.Fatalf("fixture unreadable: %s: %v -- this contract asserts nothing without it, "+
			"and skipping would look identical to passing", path, err)
	}
	return b
}

func TestRegistryFixtureMatchesRunkind(t *testing.T) {
	var reg struct {
		Kinds         []string `json:"kinds"`
		JudgeEligible []string `json:"judge_eligible"`
	}
	if err := json.Unmarshal(readFixture(t, "registry.json"), &reg); err != nil {
		t.Fatalf("registry.json is not valid JSON: %v", err)
	}

	// `kinds` deep-equals All(), order included.
	if !reflect.DeepEqual(reg.Kinds, All()) {
		t.Errorf("registry.json kinds = %v; want runkind.All() = %v (order included)", reg.Kinds, All())
	}

	// `judge_eligible` equals { k in All() : JudgeEligible(k) }, in the order they
	// appear in All() (i.e. ["issue", "ci_fix"]).
	var wantEligible []string
	for _, k := range All() {
		if JudgeEligible(k) {
			wantEligible = append(wantEligible, k)
		}
	}
	if !reflect.DeepEqual(reg.JudgeEligible, wantEligible) {
		t.Errorf("registry.json judge_eligible = %v; want %v", reg.JudgeEligible, wantEligible)
	}
}
