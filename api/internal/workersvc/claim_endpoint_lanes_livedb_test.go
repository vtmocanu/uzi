package workersvc

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestClaimJudgeEndpointOnlyLiveDB: an instance bound to a custom endpoint and holding
// NO Anthropic token must still be able to claim a JUDGE run.
//
// The endpoint IS the judge's credential (PRD #229 D10), so the subscription token is
// legitimately absent. Before this, assembleJudgeClaim opened the judged credential
// unconditionally and the run failed at claim on an endpoint-only instance. That is
// worse than it sounds: judge is the lane that REVIEWS completed runs, so such an
// instance could plan and implement but never review its own work.
func TestClaimJudgeEndpointOnlyLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, workerID, repoID := env.seedCodexInfra(t)
	// Deliberately NO anthropic_token row: that is the shape under test.
	runID := env.seedCodexRun(t, userID, workerID, repoID)
	// runs_kind_shape: a judge run carries no repo/issue/branch and points at a target.
	// The target is inserted directly rather than through seedCodexRun, which would
	// collide on uq_runs_one_active_per_issue (both rows would be issue 1 on one repo).
	targetID := uuid.New()
	env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, worker_id)
		VALUES ($1, $2, $3, 'issue', 2, 'target', 'd', 'completed', $4)`, targetID, userID, repoID, workerID)
	env.exec(`UPDATE runs SET kind = 'judge', repo_id = NULL, issue_iid = NULL, branch = NULL,
		target_run_id = $2, status = 'queued', worker_id = NULL WHERE id = $1`, runID, targetID)

	svc := New(env.q, env.box, testParams())
	svc.SetVault(env.vlt)
	svc.SetTxBeginner(env.pool)
	svc.SetAnthropicEndpoint("https://gateway.example", "sk-gateway", "gateway-model-1")

	payload, err := svc.ClaimReported(env.ctx, store.Worker{ID: workerID, UserID: userID}, nil, MemoryAuthorityReport{})
	if err != nil {
		t.Fatalf("claiming a judge run on an endpoint-only instance: %v", err)
	}
	if payload == nil {
		t.Fatal("expected a judge claim payload")
	}
	if payload.RunID != runID.String() {
		t.Fatalf("claimed run = %s, want %s", payload.RunID, runID)
	}
	if payload.Secrets.AnthropicEndpoint == nil {
		t.Fatal("judge claim carries no endpoint; the judge would have no credential at all")
	}
	if got := payload.Secrets.AnthropicEndpoint.Model; got != "gateway-model-1" {
		t.Fatalf("judge claim endpoint model = %q, want gateway-model-1", got)
	}
	if got := payload.Secrets.AnthropicOAuthToken; got != "" {
		t.Fatalf("judge claim carried a subscription token alongside the endpoint: %q", got)
	}
	// The exclusion must be visible on the WIRE, not only in the struct: the field has no
	// omitempty, so a bound claim emits the key with an empty string. A test that only
	// reads the struct cannot tell that apart from "the helper was skipped", because on a
	// bound run the token is never opened and is empty either way.
	raw, merr := json.Marshal(payload.Secrets)
	if merr != nil {
		t.Fatalf("marshal secrets: %v", merr)
	}
	var wire map[string]any
	if uerr := json.Unmarshal(raw, &wire); uerr != nil {
		t.Fatalf("decode secrets: %v", uerr)
	}
	if _, present := wire["anthropic_oauth_token"]; !present {
		t.Fatal("bound judge claim omitted anthropic_oauth_token; the exclusion must ride the wire as an empty string")
	}
	if ep, ok := wire["anthropic_endpoint"].(map[string]any); !ok || ep["api_key"] != "sk-gateway" {
		t.Fatalf("bound judge claim did not carry the endpoint api_key on the wire: %v", wire["anthropic_endpoint"])
	}
}

// TestClaimJobEndpointOnlyLiveDB is the same shape for the job lane. A job is repo-less
// like a judge and forks its own assembly even earlier, so it needs its own proof rather
// than an assumption that fixing one fixed the other.
func TestClaimJobEndpointOnlyLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, workerID, repoID := env.seedCodexInfra(t)
	runID := env.seedCodexRun(t, userID, workerID, repoID)
	// runs_kind_shape: a job run carries no repo/issue/branch; it needs a job_type.
	env.exec(`UPDATE runs SET kind = 'job', repo_id = NULL, issue_iid = NULL, branch = NULL,
		job_type = 'research', status = 'queued', worker_id = NULL WHERE id = $1`, runID)
	// ClaimRun's job clause admits only a worker advertising job_runner_v1, so the seeded
	// worker must carry it or the claim is idle for a reason unrelated to the endpoint.
	env.exec(`UPDATE workers SET protocol_capabilities = ARRAY['job_runner_v1'] WHERE id = $1`, workerID)

	svc := New(env.q, env.box, testParams())
	svc.SetVault(env.vlt)
	svc.SetTxBeginner(env.pool)
	svc.SetAnthropicEndpoint("https://gateway.example", "sk-gateway", "gateway-model-1")

	payload, err := svc.ClaimReported(env.ctx, store.Worker{ID: workerID, UserID: userID}, nil, MemoryAuthorityReport{})
	if err != nil {
		t.Fatalf("claiming a job run on an endpoint-only instance: %v", err)
	}
	if payload == nil {
		t.Fatal("expected a job claim payload")
	}
	if payload.RunID != runID.String() {
		t.Fatalf("claimed run = %s, want %s", payload.RunID, runID)
	}
	if payload.Secrets.AnthropicEndpoint == nil {
		t.Fatal("job claim carries no endpoint; the job would have no credential at all")
	}
	if got := payload.Secrets.AnthropicOAuthToken; got != "" {
		t.Fatalf("job claim carried a subscription token alongside the endpoint: %q", got)
	}
	// Same wire-level check as the judge lane: the field has no omitempty, so a bound claim
	// must emit anthropic_oauth_token as an empty string rather than omitting it, and must
	// carry the endpoint's key. Reading the struct alone cannot distinguish the two.
	raw, merr := json.Marshal(payload.Secrets)
	if merr != nil {
		t.Fatalf("marshal secrets: %v", merr)
	}
	var wire map[string]any
	if uerr := json.Unmarshal(raw, &wire); uerr != nil {
		t.Fatalf("decode secrets: %v", uerr)
	}
	if _, present := wire["anthropic_oauth_token"]; !present {
		t.Fatal("bound job claim omitted anthropic_oauth_token; the exclusion must ride the wire as an empty string")
	}
	if ep, ok := wire["anthropic_endpoint"].(map[string]any); !ok || ep["api_key"] != "sk-gateway" {
		t.Fatalf("bound job claim did not carry the endpoint api_key on the wire: %v", wire["anthropic_endpoint"])
	}
}
