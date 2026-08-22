package workersvc

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
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
// skills_dropped log, per-template ClaimAgent.skills, and the config caps — plus,
// since PRD #35, the usage-limit park fields. Values are fixed (no random uuids) so
// the golden file is stable. Secrets are obvious non-credentials so scanners never
// flag the fixture.
//
// The booleans are deliberately set NON-DEFAULT (true), exactly as AutoApprove
// already was.
//
// The reason is NOT the one this comment used to give — that "a golden carrying every
// flag at its zero value would still pass against a producer that dropped the field
// entirely". That is false, and measured false: TestClaimSkillsWireContract compares
// json.MarshalIndent byte-for-byte against the file and NONE of these tags carries
// omitempty, so a dropped field omits the KEY and the bytes differ whatever the value
// was. Demonstrated by deleting `"wait_on_limit"` from the golden and watching the
// test go red — a golden at false and a golden at true both catch it.
//
// The real reason, which is weaker but still sufficient: a non-default value is what
// distinguishes "wired" from "present and always zero" for a future producer built
// from the REAL claim path rather than from this hand-built fixture. A fixture full
// of zero values agrees with a server that computes them all wrong in the same
// direction.
func sampleClaimPayloadWithSkills() ClaimPayload {
	strptr := func(s string) *string { return &s }
	i64ptr := func(v int64) *int64 { return &v }
	return ClaimPayload{
		RunID:            "11111111-1111-1111-1111-111111111111",
		Kind:             RunKindIssue,
		IssueIID:         i64ptr(42),
		IssueTitle:       "Extend the pipeline",
		IssueDescription: "PRD: add a job",
		// PRD #381: the bounded, bot-filtered snapshot of the issue's HUMAN comments
		// rides the claim so the worker sees the discussion, not just the description.
		// NON-EMPTY on purpose (Truncated:true, one real comment) — an empty/absent
		// snapshot here would agree with a producer that dropped the field, the same
		// "wired vs present-and-nil" reason the values above carry non-default. The
		// timestamp is FIXED so the golden stays byte-stable.
		IssueComments: &IssueCommentsSnapshot{
			Comments: []IssueCommentSnapshot{
				{
					AuthorUsername:    "carol",
					AuthorForgeUserID: 42,
					CreatedAt:         time.Date(2026, 7, 4, 9, 0, 0, 0, time.UTC),
					Body:              "please guard on Valid",
				},
			},
			Truncated: true,
		},
		Status:         "claimed",
		Branch:         strptr("agent/issue-42"),
		SessionID:      strptr("sess-abc"),
		LastSeq:        7,
		IterationCount: 1,
		RequeueCount:   0,
		PlanMd:         strptr("# Plan\n"),
		AutoApprove:    true, // PRD #19 autopilot; part of the same claim shape
		// PRD #400 M2: the task-run MR gate + source ref ride every claim. Both are
		// NON-DEFAULT here (open_mr true, base_branch non-nil) for the same "wired vs
		// present-and-zero" reason the flags above carry non-default — even though they
		// are only meaningful for kind='task'. open_mr has no omitempty (always present,
		// like auto_approve); base_branch is a pointer (nil ⇒ absent).
		OpenMr: true,
		// PRD #517 M1: interactive rides every claim (a plain bool, no omitempty, always
		// present). Non-default (true) here for the same "wired vs present-and-zero" reason
		// as open_mr above, even though it is only meaningful for kind='task'.
		Interactive: true,
		BaseBranch:  strptr("develop"),
		// PRD #400 M4a: review_target_run_id rides every claim (a *string, no omitempty, so
		// always present — null for a non-review run). This sample is NOT a review run, so it
		// is left nil/null here; the non-null delivery for an actual review run is pinned in
		// TestClaimCarriesReviewTargetRunID (service_test.go), not by mutating this shared
		// golden into a semantically-incoherent review-of-itself.
		WaitOnLimit:  true, // PRD #35 Decision 7: the run's usage-limit opt-in
		PlanApproved: true,
		// PRD #209: plan_source rides every claim (NOT NULL, no omitempty). "seeded"
		// here makes this a coherent seeded-run claim — approved, no session — and is a
		// NON-DEFAULT value for the same "wired vs present-and-zero" reason the booleans
		// above carry true. The default 'agent' would agree with a producer that dropped
		// the field's write.
		PlanSource: planSourceSeeded,
		// PRD #209 M4: the staleness guard rides every seeded claim. A NON-NULL planned
		// commit and require_base_match:true make this a coherent guarded seeded claim and,
		// like the values above, are non-default so a producer that dropped either write is
		// caught. planned_base_commit is omitempty (nil ⇒ absent); require_base_match is
		// always present (NOT NULL, no omitempty).
		PlannedBaseCommit: strptr("abc123def4567890abc123def4567890abc12345"),
		RequireBaseMatch:  true,
		// PRD #35: the persisted selection rides the claim so a resumed, already-approved
		// run keeps the exclusions the same human verdict carried. NON-EMPTY on purpose —
		// an empty exclusions list here would agree with a producer that dropped them.
		AgentSelection: &AgentSelection{Source: "repo", Exclusions: []string{"reviewer"}}, // PRD #35 Decision 6b: lets a resume skip the plan gate
		Repo: ClaimRepo{
			ID:              "22222222-2222-2222-2222-222222222222",
			URL:             "https://gitlab.example.com/g/p",
			CloneURL:        "https://gitlab.example.com/g/p.git",
			DefaultBranch:   strptr("main"),
			SkillsEnabled:   true,
			ClaudemdEnabled: true,     // PRD #246: repo owner opted into advisory CLAUDE.md; rides the claim byte-for-byte (no omitempty)
			ForgeType:       "gitlab", // PRD #65 R8: emitted on every claim, "gitlab" for a GitLab connection
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
				Skills:      []string{"team-runbook", "team-kb"},
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
			{Name: "team-runbook", Description: "the team runbook.", Body: "# Team Runbook\n"},
			{Name: "team-kb", Description: "the team knowledge base.", Body: "# Team KB\n"},
		},
		SkillsDropped: []ClaimSkillDrop{
			{Name: "team-runbook", Reason: DropShadowed},
		},
		Config: ClaimConfig{
			RunTimeoutSeconds:  7200,
			IdleTimeoutSeconds: 600,
			MaxIterations:      5,
			PlanMaxRevisions:   3, // PRD #41 plan-revision cap
			// PRD #88 clarification bounds. Pinned to the shipped defaults rather
			// than left zero: the golden IS the wire contract, and a recorded 0
			// would read as "this run may ask no questions and its answer deadline
			// is already past" to anyone implementing against it.
			QuestionMax:            5,
			QuestionTimeoutSeconds: 86400,
			DefaultModel:           strptr("sonnet"),
			SkillMaxBytes:          65536,
			SkillsMaxPerRun:        32,
			ToolPackages:           []string{"kubectl@1.31", "jq"}, // PRD #18 M3 tier-1 list
			RepoDevboxOptIn:        false,                          // M5 wires the toggle; false until then
			// PRD #123 M1b: the Decision 6 denylist base names ride the claim so the worker
			// filters tier-2 (repo devbox.json) packages by base name. A small NON-EMPTY
			// subset here (the real list is the full denylist) pins the wire key + shape
			// across the language boundary; the TS contract test deep-equals this value.
			DeniedToolPackages: []string{"glab", "vault"},
			// PRD #305 M3: OverrideSubagentModel is deliberately LEFT at its zero value
			// here, unlike the other flags in this fixture. It carries omitempty, so an off
			// run omits the key and its claim is byte-identical to today's wire — the exact
			// property this shared golden must keep for the un-upgraded worker (and the
			// TS-side contract test that parses this same file). The flag-on delivery and
			// the flag-off omit-from-the-wire are proven in the dedicated service_test.go
			// tests (TestClaimDeliversOverrideSubagentModelWhenFrozenOn /
			// TestClaimOmitsOverrideSubagentModelWhenFrozenOff), not by mutating this golden.
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

// TestClaimRepoCarriesForgeType pins the first of PRD #65 R8's two wire changes:
// forge_type is emitted on every claim (forge_connections.forge_type is NOT NULL, so
// the server always has a value) as "gitlab" for a GitLab connection. It is additive
// — an old worker ignores the unknown key and keeps working (proven on the parse side
// by agent/test/claim-skills-contract.test.ts, which reads this same golden).
func TestClaimRepoCarriesForgeType(t *testing.T) {
	b, err := json.Marshal(sampleClaimPayloadWithSkills().Repo)
	if err != nil {
		t.Fatalf("marshal repo: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal repo: %v", err)
	}
	if got, ok := m["forge_type"]; !ok || got != "gitlab" {
		t.Errorf("claim repo must carry forge_type=\"gitlab\" (R8); got %v (present=%v)", got, ok)
	}
}

// TestClaimCarriesTaskMrFields pins PRD #400 M2's two additions: open_mr and
// base_branch ride every claim. open_mr has no omitempty (always present, like
// auto_approve); base_branch is a pointer that is present when non-nil. Both are
// meaningful only for a task run, but the wire always carries the key so a worker
// never has to guess. Additive — an old worker ignores both keys.
func TestClaimCarriesTaskMrFields(t *testing.T) {
	b, err := json.Marshal(sampleClaimPayloadWithSkills())
	if err != nil {
		t.Fatalf("marshal claim: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal claim: %v", err)
	}
	got, ok := m["open_mr"]
	if !ok {
		t.Error("claim must carry open_mr (PRD #400 M2); key absent")
	} else if got != true {
		t.Errorf("open_mr = %v, want true", got)
	}
	base, ok := m["base_branch"]
	if !ok {
		t.Error("claim must carry base_branch (PRD #400 M2); key absent")
	} else if base != "develop" {
		t.Errorf("base_branch = %v, want develop", base)
	}
}

// TestClaimCarriesReviewTargetRunID pins PRD #400 M4a's claim addition: review_target_run_id
// rides every claim (a *string, no omitempty) — null for a non-review run, and the reviewed
// task's id for a review run, which is how the worker (M4b) routes a claim to its diff-review
// executor. The sample (a non-review run) carries the key as null; a review-run payload carries
// the target id as a string. Additive — an old worker ignores the key.
func TestClaimCarriesReviewTargetRunID(t *testing.T) {
	// Non-review run: key present, null.
	b, err := json.Marshal(sampleClaimPayloadWithSkills())
	if err != nil {
		t.Fatalf("marshal claim: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal claim: %v", err)
	}
	if got, ok := m["review_target_run_id"]; !ok {
		t.Error("claim must carry review_target_run_id (PRD #400 M4a); key absent")
	} else if got != nil {
		t.Errorf("review_target_run_id = %v, want null for a non-review run", got)
	}

	// Review run: the reviewed task's id rides the claim as a string.
	const targetID = "33333333-3333-3333-3333-333333333333"
	rev := sampleClaimPayloadWithSkills()
	rev.ReviewTargetRunID = strPtr(targetID)
	b2, err := json.Marshal(rev)
	if err != nil {
		t.Fatalf("marshal review claim: %v", err)
	}
	var m2 map[string]any
	if err := json.Unmarshal(b2, &m2); err != nil {
		t.Fatalf("unmarshal review claim: %v", err)
	}
	if got := m2["review_target_run_id"]; got != targetID {
		t.Errorf("review_target_run_id = %v, want %s", got, targetID)
	}
}

// TestCompletionMrWebURLWireContract pins the second of R8's wire changes: mr_web_url
// on the completion payload is additive + optional. An OLD worker (which never sends
// the key) must still complete a GitLab run — its StateRequest decodes with a nil
// MrWebURL, which textParam turns into a NULL mr_web_url so the web falls back to the
// legacy forgeUrls.ts reconstruction. A NEW worker sends the URL and it lands verbatim.
func TestCompletionMrWebURLWireContract(t *testing.T) {
	// Old worker: no mr_web_url key at all (D8/R8 pre-feature shape).
	const oldWorker = `{"status":"completed","branch":"agent/issue-7","mr_iid":42}`
	var oldReq StateRequest
	if err := json.Unmarshal([]byte(oldWorker), &oldReq); err != nil {
		t.Fatalf("old-worker completion unmarshal: %v", err)
	}
	if oldReq.MrWebURL != nil {
		t.Errorf("old worker: expected nil MrWebURL, got %q", *oldReq.MrWebURL)
	}
	if p := textParam(oldReq.MrWebURL); p.Valid {
		t.Errorf("old worker: expected NULL mr_web_url, got %q", p.String)
	}

	// New worker: carries the forge-reported URL.
	const url = "https://gitlab.example.com/g/p/-/merge_requests/42"
	newWorker := `{"status":"completed","branch":"agent/issue-7","mr_iid":42,"mr_web_url":"` + url + `"}`
	var newReq StateRequest
	if err := json.Unmarshal([]byte(newWorker), &newReq); err != nil {
		t.Fatalf("new-worker completion unmarshal: %v", err)
	}
	if newReq.MrWebURL == nil || *newReq.MrWebURL != url {
		t.Errorf("new worker: expected MrWebURL %q, got %v", url, newReq.MrWebURL)
	}
	if p := textParam(newReq.MrWebURL); !p.Valid || p.String != url {
		t.Errorf("new worker: expected mr_web_url %q, got valid=%v %q", url, p.Valid, p.String)
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
