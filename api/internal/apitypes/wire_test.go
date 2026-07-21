package apitypes

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"
	"time"
)

// These are wire-shape pins (PRD #64 M1 A.3). Each moved DTO gets a golden
// top-level tag-set assertion, proving the leaf extraction is byte-neutral: any
// future rename/add/remove/omitempty change to a field a CLI or SPA decodes fails
// here loudly instead of silently breaking the contract. Coverage of these shapes
// was near-zero before the move, so the pin is authored alongside it.

// tagSet marshals v and returns its sorted top-level JSON object keys.
func tagSet(t *testing.T, v any) []string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %T: %v", v, err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal %T to object: %v (json=%s)", v, err, b)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// assertTags fails unless v's top-level JSON keys are exactly want (order-free).
func assertTags(t *testing.T, name string, v any, want ...string) {
	t.Helper()
	got := tagSet(t, v)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("%s tag set mismatch\n got: %v\nwant: %v", name, got, want)
	}
}

func TestUserDTOTags(t *testing.T) {
	assertTags(t, "UserDTO", UserDTO{},
		"id", "email", "display_name", "is_admin", "is_active",
		"autopilot_enabled", "judge_enabled",
		// PRD #104 M4: which credential this user's retrospectives spend. Both null
		// ⇒ their default. The label, never the token value.
		"judge_anthropic_secret_id", "judge_anthropic_secret_label",
		"created_at", "last_login")
}

func TestRepoAgentTags(t *testing.T) {
	assertTags(t, "RepoAgent", RepoAgent{}, "name", "description")
}

func TestAgentSelectionTags(t *testing.T) {
	assertTags(t, "AgentSelection", AgentSelection{}, "source", "exclusions")
}

// runDTOKeys is the RunDTO tag set with Usage nil (usage is omitempty). Shared by
// the RunDTO pin and the RunListItemDTO embed pin.
var runDTOKeys = []string{
	"id", "repo_id", "forge_type", "kind", "issue_iid", "issue_title", "issue_description",
	"title", "resume_of_run_id", "status", "requeue_count", "iteration_count",
	"auto_approve", "worker_id", "branch", "mr_iid", "mr_web_url", "mr_state", "failure_reason",
	"stop_kind", "health", "health_reason", "health_since", "plan_md",
	"pipeline_ref", "pipeline_web_url", "fix_verdict", "claimed_at", "started_at",
	"finished_at", "created_at", "updated_at", "repo_agents", "agent_source",
	"agent_exclusions", "own_agents",
}

func TestRunDTOTags(t *testing.T) {
	// usage is omitempty: absent when nil.
	assertTags(t, "RunDTO(no usage)", RunDTO{}, runDTOKeys...)
	// present when set.
	got := tagSet(t, RunDTO{Usage: &UsageDTO{}})
	if !contains(got, "usage") {
		t.Fatalf("RunDTO with Usage set must include usage key, got %v", got)
	}
}

func TestRunListItemDTOTags(t *testing.T) {
	// Embeds RunDTO (keys flatten in) plus repo_path + worker_name; owner_email is
	// omitempty (absent when nil).
	want := append(append([]string{}, runDTOKeys...), "repo_path", "worker_name")
	assertTags(t, "RunListItemDTO(no owner)", RunListItemDTO{}, want...)
	// owner_email present when set.
	email := "u@example.test"
	if got := tagSet(t, RunListItemDTO{OwnerEmail: &email}); !contains(got, "owner_email") {
		t.Fatalf("RunListItemDTO with OwnerEmail set must include owner_email, got %v", got)
	}
}

func TestMessageDTOTags(t *testing.T) {
	assertTags(t, "MessageDTO", MessageDTO{}, "seq", "kind", "agent", "payload", "created_at")
}

func TestRunInputTags(t *testing.T) {
	assertTags(t, "RunInputRequest", RunInputRequest{}, "kind", "body", "selection")
	// id + created_at are omitempty (nil on approve/cancel/reject): the zero value is
	// still just server_side (PRD #95 S2).
	assertTags(t, "RunInputResponse", RunInputResponse{}, "server_side")
	// A follow_up write returns the created row: id + created_at appear when set.
	id := int64(7)
	now := time.Unix(0, 0)
	assertTags(t, "RunInputResponse(with row)", RunInputResponse{ID: &id, CreatedAt: &now},
		"server_side", "id", "created_at")
}

func TestSteerInputDTOTags(t *testing.T) {
	assertTags(t, "SteerInputDTO", SteerInputDTO{}, "id", "body", "created_at", "consumed_at")
}

func TestRecommendationDTOTags(t *testing.T) {
	assertTags(t, "RecommendationDTO", RecommendationDTO{},
		"id", "category", "target", "rationale_md", "confidence", "created_at")
}

func TestReviewDTOTags(t *testing.T) {
	assertTags(t, "ReviewDTO", ReviewDTO{},
		"id", "target_run_id", "verdict", "summary_md", "judge_model", "status",
		"created_at", "updated_at", "recommendations", "filed_issues",
		"dispositions", "triage")
}

func TestFiledIssueDTOTags(t *testing.T) {
	assertTags(t, "FiledIssueDTO", FiledIssueDTO{},
		"category", "target", "issue_iid", "issue_url", "filed_at")
}

func TestDispositionDTOTags(t *testing.T) {
	assertTags(t, "DispositionDTO", DispositionDTO{},
		"category", "target", "status", "reason", "set_at", "stale")
}

func TestTriageDTOTags(t *testing.T) {
	assertTags(t, "TriageDTO", TriageDTO{},
		"total", "todo", "filed", "done", "dismissed", "false_positives")
}

func TestIssueDraftDTOTags(t *testing.T) {
	assertTags(t, "IssueDraftDTO", IssueDraftDTO{},
		"default_repo_id", "title", "description", "labels", "provenance", "default_note")
}

// TestReviewNullEnvelope pins the GET /api/runs/{id}/review contract that a
// visible-but-unjudged run returns 200 {"review": null}, and a null review is a
// valid decode — the CLI/SPA must model review as nullable, not treat null as an
// error (PRD #64 M1 A.3).
func TestReviewNullEnvelope(t *testing.T) {
	b, err := json.Marshal(map[string]any{"review": (*ReviewDTO)(nil)})
	if err != nil {
		t.Fatalf("marshal null review envelope: %v", err)
	}
	if string(b) != `{"review":null}` {
		t.Fatalf("null review envelope = %s, want {\"review\":null}", b)
	}
	// A populated review round-trips through the same envelope key.
	b, err = json.Marshal(map[string]any{"review": ReviewDTO{ID: "r1"}})
	if err != nil {
		t.Fatalf("marshal review envelope: %v", err)
	}
	if !strings.HasPrefix(string(b), `{"review":{`) {
		t.Fatalf("populated review envelope = %s, want {\"review\":{...}}", b)
	}
}

func TestRepoDTOTags(t *testing.T) {
	assertTags(t, "RepoDTO", RepoDTO{},
		"id", "connection_id", "forge_project_id", "path_with_namespace", "web_url",
		"default_branch", "enabled", "repo_skills_enabled", "repo_devbox_opt_in", "pipeline")
}

func TestPipelineDTOTags(t *testing.T) {
	assertTags(t, "PipelineDTO", PipelineDTO{},
		"ref", "status", "web_url", "pipeline_id", "synced_at")
}

// workerDTOKeys is the WorkerDTO tag set (no omitempty fields). Shared by the
// WorkerDTO pin and the AdminWorkerDTO embed pin.
var workerDTOKeys = []string{
	"id", "name", "status", "kind", "hosted_size", "docker", "busy", "active_runs",
	"max_concurrent_runs", "template_declared", "template_reported", "version",
	"last_heartbeat_at", "created_at", "stats_cpu_pct", "stats_mem_bytes",
	"stats_mem_limit_bytes", "stats_source",
	// PRD #104 M3: which Anthropic credential this worker's run-lane claims spend.
	// Both null ⇒ unbound ⇒ the owner's default. The LABEL, never the token value —
	// this DTO is the shape the web UI and the CLI both read.
	"anthropic_secret_id", "anthropic_secret_label",
}

func TestWorkerDTOTags(t *testing.T) {
	assertTags(t, "WorkerDTO", WorkerDTO{}, workerDTOKeys...)
}

func TestAdminWorkerDTOTags(t *testing.T) {
	want := append(append([]string{}, workerDTOKeys...), "owner_email")
	assertTags(t, "AdminWorkerDTO", AdminWorkerDTO{}, want...)
}

func TestUsageDTOTags(t *testing.T) {
	assertTags(t, "UsageDTO", UsageDTO{},
		"input_tokens", "cache_read_tokens", "cache_creation_tokens", "output_tokens", "cost_usd")
}

func TestSelfUsageDTOTags(t *testing.T) {
	assertTags(t, "SelfUsageDTO", SelfUsageDTO{}, "lifetime", "last_7_days", "run_count")
}

func TestAdminUserUsageDTOTags(t *testing.T) {
	assertTags(t, "AdminUserUsageDTO", AdminUserUsageDTO{}, "user_id", "email", "usage", "run_count")
}

func TestAdminUsageDTOTags(t *testing.T) {
	assertTags(t, "AdminUsageDTO", AdminUsageDTO{}, "factory", "users", "earliest_run")
}

func TestRateLimitWindowTags(t *testing.T) {
	assertTags(t, "RateLimitWindow", RateLimitWindow{}, "pct", "resets_at")
}

// TestRateLimitDTOTags pins the discriminated union: a status-only value drops
// every ok-only field (omitempty), an "ok" value carries them all.
func TestRateLimitDTOTags(t *testing.T) {
	assertTags(t, "RateLimitDTO(status-only)", RateLimitDTO{Status: "no_token"}, "status")
	stale := false
	full := RateLimitDTO{
		Status:   "ok",
		FiveHour: &RateLimitWindow{},
		SevenDay: &RateLimitWindow{},
		Source:   "api",
		SyncedAt: time.Now().UTC().Format(time.RFC3339),
		Stale:    &stale,
	}
	assertTags(t, "RateLimitDTO(ok)", full,
		"status", "five_hour", "seven_day", "source", "synced_at", "stale")
}

func TestTokenRateLimitDTOTags(t *testing.T) {
	assertTags(t, "TokenRateLimitDTO", TokenRateLimitDTO{},
		"secret_id", "label", "is_default", "limits")
}

func TestAdminRateLimitRowDTOTags(t *testing.T) {
	assertTags(t, "AdminRateLimitRowDTO", AdminRateLimitRowDTO{},
		"id", "email", "name", "vault_locked", "tokens")
}

// TestAgentMemoryWriteRequestTags pins the worker save body: exactly {title, body}
// — no identity fields exist on the wire, so (user_id, repo_id) can only be derived
// server-side from the run claim (PRD #90 B3).
func TestAgentMemoryWriteRequestTags(t *testing.T) {
	assertTags(t, "AgentMemoryWriteRequest", AgentMemoryWriteRequest{}, "title", "body")
}

// TestAgentMemoryDTOTags pins the read shape. repo_id/repo_name/run_id are
// omitempty: the worker per-(user,repo) read omits the redundant repo fields, and
// run_id drops when the writing run is pruned (FK SET NULL). The user-facing
// cross-repo list sets all of them.
func TestAgentMemoryDTOTags(t *testing.T) {
	assertTags(t, "AgentMemoryDTO(worker)", AgentMemoryDTO{}, "id", "title", "body", "created_at")
	full := AgentMemoryDTO{ID: "m1", RepoID: "r1", RepoName: "org/repo", Title: "t", Body: "b", RunID: "run1"}
	assertTags(t, "AgentMemoryDTO(user list)", full,
		"id", "repo_id", "repo_name", "title", "body", "run_id", "created_at")
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
