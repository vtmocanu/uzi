package workersvc

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// wireContractFixture is the single shared golden file both sides validate
// against, so the Go producer and the TS worker consumer can never drift into
// two lenient fakes (the PRD #4 M1+M2 lesson). The Go side (this test) pins that
// the server MARSHALS exactly this shape; the worker side
// (agent/test/claim-skills-contract.test.ts) pins that it PARSES exactly this
// shape from the same file. Regenerate with `UPDATE_GOLDEN=1 go test`.
const wireContractFixture = "testdata/claim_skills_wire.json"

// sampleClaimPayloadWithSkills is a representative, fully-populated claim showing
// every PRD #16 wire addition: repo.skills_enabled, the skills union, the
// skills_dropped log, per-template ClaimAgent.skills, and the config caps. Values
// are fixed (no random uuids) so the golden file is stable. Secrets are obvious
// non-credentials so scanners never flag the fixture.
func sampleClaimPayloadWithSkills() ClaimPayload {
	strptr := func(s string) *string { return &s }
	i64ptr := func(v int64) *int64 { return &v }
	return ClaimPayload{
		RunID:            "11111111-1111-1111-1111-111111111111",
		Kind:             RunKindIssue,
		IssueIID:         i64ptr(42),
		IssueTitle:       "Extend the pipeline",
		IssueDescription: "PRD: add a job",
		Status:           "claimed",
		Branch:           strptr("agent/issue-42"),
		SessionID:        strptr("sess-abc"),
		LastSeq:          7,
		IterationCount:   1,
		RequeueCount:     0,
		PlanMd:           strptr("# Plan\n"),
		AutoApprove:      true, // PRD #19 autopilot; part of the same claim shape
		Repo: ClaimRepo{
			ID:            "22222222-2222-2222-2222-222222222222",
			URL:           "https://gitlab.example.com/g/p",
			CloneURL:      "https://gitlab.example.com/g/p.git",
			DefaultBranch: strptr("main"),
			SkillsEnabled: true,
		},
		Secrets: ClaimSecrets{
			ForgeUsername:       "uzi-bot",
			ForgePAT:            "FORGE-PAT-PLACEHOLDER",
			AnthropicOAuthToken: "ANTHROPIC-OAUTH-PLACEHOLDER",
		},
		Agents: []ClaimAgent{
			{
				Name:        "coder",
				Description: "writes code",
				PromptBody:  "you code",
				Tools:       []string{"Read", "Edit"},
				Model:       strptr("opus"),
				Skills:      []string{"ci-cd-norms", "team-kb"},
			},
			{
				Name:        "reviewer",
				Description: "reviews",
				PromptBody:  "you review",
				Tools:       nil, // inherit-all
				Model:       nil, // inherit
				Skills:      []string{},
			},
		},
		Skills: []ClaimSkill{
			{Name: "ci-cd-norms", Description: "how CI/CD works at example.", Body: "# example CI/CD\n"},
			{Name: "team-kb", Description: "the team knowledge base.", Body: "# Team KB\n"},
		},
		SkillsDropped: []ClaimSkillDrop{
			{Name: "ci-cd-norms", Reason: DropShadowed},
		},
		Config: ClaimConfig{
			RunTimeoutSeconds:  7200,
			IdleTimeoutSeconds: 600,
			MaxIterations:      5,
			DefaultModel:       strptr("sonnet"),
			SkillMaxBytes:      65536,
			SkillsMaxPerRun:    32,
		},
	}
}

func TestClaimSkillsWireContract(t *testing.T) {
	got, err := json.MarshalIndent(sampleClaimPayloadWithSkills(), "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got = append(got, '\n')

	path := filepath.FromSlash(wireContractFixture)
	if os.Getenv("UPDATE_GOLDEN") != "" {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("update golden: %v", err)
		}
		t.Logf("wrote %s", path)
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (run `UPDATE_GOLDEN=1 go test` to create it): %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("claim wire shape drifted from %s.\nThe server output changed; if intended, regenerate with UPDATE_GOLDEN=1 and update the worker-side contract test to match.\n--- got ---\n%s", wireContractFixture, got)
	}
}

// TestClaimSkillsWireContractRoundTrips proves the golden file itself is
// canonical: unmarshaling it and re-marshaling reproduces it byte-for-byte, so a
// hand-edit that broke the shape (or key order) would fail here too.
func TestClaimSkillsWireContractRoundTrips(t *testing.T) {
	raw, err := os.ReadFile(filepath.FromSlash(wireContractFixture))
	if err != nil {
		t.Skipf("golden not present: %v", err)
	}
	var p ClaimPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("unmarshal golden: %v", err)
	}
	re, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	re = append(re, '\n')
	if string(re) != string(raw) {
		t.Errorf("golden file is not canonical for ClaimPayload; regenerate with UPDATE_GOLDEN=1")
	}
}
