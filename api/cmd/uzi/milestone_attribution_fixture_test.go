package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// milestone_attribution_fixture_test.go is the GO HALF of a cross-language golden for the
// per-milestone agent attribution join (PRD #1224). The vitest half is
// web/src/lib/milestoneAttribution.fixture.test.ts. Neither reads the other: each folds its
// OWN production join helpers over the SAME fixture (fixtures/milestone-attribution/cases.json),
// so a mismatch NAMES the drifting surface — the one thing the per-surface render tests (M5/M6)
// cannot catch, since they exercise each surface in isolation.
//
// This Go side drives the TUI/CLI helpers effectiveMilestoneAgents + uniqueMilestoneAgentMatch
// (tui_detail_rail.go); the TS side drives effectiveMilestoneAgents + uniqueLiveMatchMilestoneId
// (runBadge.ts). The fixture pins the DECISION-level contract both must agree on, and reddens a
// case under any of these mutations to either surface:
//   - flipping the output order from FROZEN (run.Milestones) to milestones_in_progress order,
//   - dropping the D6 read-time stale re-filter (an entry whose id is no longer in progress),
//   - changing the duplicate-id rule (first-valid-wins, so the first entry's agent decides).
//
// 🔴 fixtures/ sits ABOVE the api/ module, so a fixture-only edit contributes nothing to this
// package's test-cache key: a bare `go test ./cmd/uzi/` can print "ok (cached)" over a changed
// fixture. Run this package with -count=1 (task test:api / task gate:api already do) so the
// contract is live; see the fixture's README for the asymmetry.
//
// D9 (the in-progress MARK renders ALONGSIDE the attribution, not in place of it) is a RENDER
// concern this membership fixture does not touch; it is already pinned by the strict TUI golden
// in tui_milestone_attribution_test.go (TestTUIMilestoneAttributionMultiUniqueMatch keeps the
// " ◕ Beta" mark line beside the attribution now-line), so it is referenced here, not duplicated.

// attribCase is one hand-authored cross-surface attribution case. milestones_agents is nil for the
// JSON `null` case; unique_match_id is a *string so JSON null (0 or 2+ matches) is distinct from a
// matched id — the Go helper spells "no match" as "", which this maps to.
type attribCase struct {
	Name                 string                    `json:"name"`
	Milestones           []string                  `json:"milestones"`
	MilestonesCompleted  []string                  `json:"milestones_completed"`
	MilestonesInProgress []string                  `json:"milestones_in_progress"`
	MilestonesAgents     []apitypes.MilestoneAgent `json:"milestones_agents"`
	ActivityAgent        string                    `json:"activity_agent"`
	Expected             struct {
		EffectiveIDs  []string `json:"effective_ids"`
		UniqueMatchID *string  `json:"unique_match_id"`
	} `json:"expected"`
}

// milestonesFromIDs builds the frozen apitypes.Milestone slice from a bare id list, titling each
// milestone with its own id (the join reads only the id, never the title).
func milestonesFromIDs(ids []string) []apitypes.Milestone {
	ms := make([]apitypes.Milestone, 0, len(ids))
	for _, id := range ids {
		ms = append(ms, apitypes.Milestone{ID: id, Title: id})
	}
	return ms
}

// sortedStrings returns a sorted copy so an unordered map-key slice compares as a SET (the Go
// helper returns a map, so membership — not order — is what the Go half asserts).
func sortedStrings(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func TestMilestoneAttributionCrossSurfaceFixture(t *testing.T) {
	// A missing/unreadable fixture is fatal, never a skip: a skipped contract asserts nothing and
	// would look identical to passing.
	path := filepath.Join("..", "..", "..", "fixtures", "milestone-attribution", "cases.json")
	raw, err := os.ReadFile(path) //nolint:gosec // G304: test reads a fixed repo-relative fixture path
	if err != nil {
		t.Fatalf("fixture unreadable: %s: %v -- this contract asserts nothing without it, and "+
			"skipping would look identical to passing", path, err)
	}
	var cases []attribCase
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatalf("cases.json is not valid JSON: %v", err)
	}
	if len(cases) == 0 {
		t.Fatalf("cases.json decoded to zero cases -- a vacuous fixture asserts nothing")
	}

	for _, c := range cases {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			run := apitypes.RunDTO{
				Milestones:           milestonesFromIDs(c.Milestones),
				MilestonesCompleted:  c.MilestonesCompleted,
				MilestonesInProgress: c.MilestonesInProgress,
				MilestonesAgents:     c.MilestonesAgents,
			}

			// effective membership: the map KEY SET equals expected.effective_ids as a SET.
			eff := effectiveMilestoneAgents(run)
			gotIDs := make([]string, 0, len(eff))
			for id := range eff {
				gotIDs = append(gotIDs, id)
			}
			wantIDs := sortedStrings(c.Expected.EffectiveIDs)
			if got := sortedStrings(gotIDs); !equalStrings(got, wantIDs) {
				t.Errorf("effectiveMilestoneAgents key set = %v; want %v (as a set)", got, wantIDs)
			}

			// unique live match: the Go helper returns "" for null/absent, which the fixture spells
			// as JSON null (0 or 2+ matches).
			wantUnique := ""
			if c.Expected.UniqueMatchID != nil {
				wantUnique = *c.Expected.UniqueMatchID
			}
			if got := uniqueMilestoneAgentMatch(eff, c.ActivityAgent); got != wantUnique {
				t.Errorf("uniqueMilestoneAgentMatch(activity=%q) = %q; want %q", c.ActivityAgent, got, wantUnique)
			}
		})
	}
}

// equalStrings reports whether two same-ordered slices are element-equal (both callers sort first).
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
