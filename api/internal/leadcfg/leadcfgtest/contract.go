// Package leadcfgtest loads fixtures/lead-config/contract.json and fixtures/run-kinds/registry.json for
// the tests of the lead-config seam (PRD #3244), so every consumer reads the one fixture the same way and
// adds cases to the file, never a second matrix. It is imported by tests only.
package leadcfgtest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// Vocabulary is the contract's closed sets.
type Vocabulary struct {
	Harnesses        []string `json:"harnesses"`
	Phases           []string `json:"phases"`
	ModelSources     []string `json:"model_sources"`
	EffortSources    []string `json:"effort_sources"`
	EffortModes      []string `json:"effort_modes"`
	ModelEvidence    []string `json:"model_evidence"`
	EffortEvidence   []string `json:"effort_evidence"`
	RequestedEfforts []string `json:"requested_efforts"`
	ObservedEfforts  []string `json:"observed_efforts"`
	LeadLaneKinds    []string `json:"lead_lane_kinds"`
}

// Labels is the contract's run-level label table; {H} is the harness name.
type Labels struct {
	Model               map[string]string `json:"model"`
	ModelSDKDefault     string            `json:"model_sdk_default"`
	Effort              map[string]string `json:"effort"`
	EffortModes         map[string]string `json:"effort_modes"`
	Evidence            map[string]string `json:"evidence"`
	NotObserved         string            `json:"not_observed"`
	EffortNotSent       string            `json:"effort_not_sent"`
	ObservedEffortOther string            `json:"observed_effort_other"`
}

// DisplayExpect is the expected presentation: the chip text ("" = no chip) and the tooltip lines.
type DisplayExpect struct {
	Chip  string   `json:"chip"`
	Lines []string `json:"lines"`
}

// DisplayCase is one display_cases entry. Input is the neutral LeadConfigViewInput as raw JSON so each
// presenter decodes it into its own type.
type DisplayCase struct {
	ID     string          `json:"id"`
	Input  json.RawMessage `json:"input"`
	Expect DisplayExpect   `json:"expect"`
}

// Contract is fixtures/lead-config/contract.json. RequestCases stay raw: each case is
// {id, harness, phase, config, templateModel?, providerDefaultModel?, expect}.
type Contract struct {
	Vocabulary   Vocabulary        `json:"vocabulary"`
	FixedAt      map[string]string `json:"fixed_at"`
	Labels       Labels            `json:"labels"`
	RequestCases []json.RawMessage `json:"request_cases"`
	DisplayCases []DisplayCase     `json:"display_cases"`
}

// LoadContract reads the contract, failing the test (never skipping) when it is unreadable.
func LoadContract(t testing.TB) Contract {
	t.Helper()
	var c Contract
	readFixture(t, "lead-config/contract.json", &c)
	return c
}

// LaneCase is one row of the lead-lane matrix: a run kind crossed with a review target and an egress
// binding, and whether the run has a lead agent. Want is derived from the contract's lead_lane_kinds,
// never from leadcfg.LeadLane, so it is an independent oracle.
type LaneCase struct {
	Kind         string
	ReviewTarget bool
	EgressBound  bool
	Want         bool
}

// LaneMatrix crosses every kind of fixtures/run-kinds/registry.json with review target and egress
// binding. Later units reuse it to assert that claim keys, overlay presence and the SQL fence agree with
// leadcfg.LeadLane for every case.
func LaneMatrix(t testing.TB) []LaneCase {
	t.Helper()
	var reg struct {
		Kinds []string `json:"kinds"`
	}
	readFixture(t, "run-kinds/registry.json", &reg)
	if len(reg.Kinds) == 0 {
		t.Fatal("run-kinds registry lists no kinds")
	}
	lead := map[string]bool{}
	for _, k := range LoadContract(t).Vocabulary.LeadLaneKinds {
		lead[k] = true
	}
	var out []LaneCase
	for _, k := range reg.Kinds {
		for _, review := range []bool{false, true} {
			for _, egress := range []bool{false, true} {
				out = append(out, LaneCase{Kind: k, ReviewTarget: review, EgressBound: egress,
					Want: lead[k] && !review && !egress})
			}
		}
	}
	return out
}

func readFixture(t testing.TB, rel string, into any) {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the fixtures directory")
	}
	// api/internal/leadcfg/leadcfgtest -> repository root.
	path := filepath.Join(filepath.Dir(here), "..", "..", "..", "..", "fixtures", filepath.FromSlash(rel))
	b, err := os.ReadFile(path) //nolint:gosec // G304: fixed in-repo fixture path
	if err != nil {
		t.Fatalf("fixture unreadable: %v -- this contract asserts nothing without it", err)
	}
	if err := json.Unmarshal(b, into); err != nil {
		t.Fatalf("fixture %s: %v", rel, err)
	}
}
