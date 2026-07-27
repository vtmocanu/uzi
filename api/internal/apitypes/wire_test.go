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
	// PRD #111 M1: which Anthropic credential the claim spent. The label is a
	// snapshot and outlives the id, so both keys are always on the wire.
	"anthropic_secret_id", "anthropic_secret_label",
	// PRD #111 M5: the MODE that named it, and the measured headroom of an auto pick
	// (D20). Four keys, three independent nullabilities — the label outlives the id,
	// the reason is present on every claimed run, and the headroom only on an auto
	// pick — so all four are always PRESENT on the wire and a client branches per
	// field rather than on the group.
	"anthropic_select_reason", "anthropic_headroom_pct",
	// PRD #35 usage-limit park. Five keys, always present. wait_on_limit is set from
	// creation on every run; the other four stay null/0 until a first park, so — as
	// with the credential group above — a client branches per field, never on the
	// group. limit_resets_at and retry_not_before are separate keys because they are
	// separate instants: the countdown reads retry_not_before, which is jittered,
	// clamped and pool-aware and is routinely EARLIER than the reported reset.
	"wait_on_limit", "limit_resets_at", "retry_not_before", "limit_wait_count",
	"rate_limit_type",
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
	want := append(append([]string{}, runDTOKeys...), "repo_path", "worker_name", "judge_verdict", "judge_todo_count")
	assertTags(t, "RunListItemDTO(no owner)", RunListItemDTO{}, want...)
	// owner_email present when set.
	email := "u@example.test"
	if got := tagSet(t, RunListItemDTO{OwnerEmail: &email}); !contains(got, "owner_email") {
		t.Fatalf("RunListItemDTO with OwnerEmail set must include owner_email, got %v", got)
	}
}

func TestMessageDTOTags(t *testing.T) {
	// agent_instance/agent_label (PRD #99) are NOT omitempty: like agent, the keys
	// are always on the wire and carry JSON null when absent, so a consumer can
	// read them unconditionally and fall back on null.
	assertTags(t, "MessageDTO", MessageDTO{},
		"seq", "kind", "agent", "agent_instance", "agent_label", "payload", "created_at")
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

func TestJudgeOccurrenceDTOTags(t *testing.T) {
	// filed_issue and set_via are both omitempty: an unfiled (or claimed-but-unsettled)
	// occurrence omits filed_issue rather than shipping a null the consumer special-cases,
	// and a hand-set (or absent) disposition omits set_via, which carries meaning only when
	// present.
	assertTags(t, "JudgeOccurrenceDTO", JudgeOccurrenceDTO{},
		"run_id", "run_title", "review_id", "rec_id", "verdict", "confidence", "bucket")
	assertTags(t, "JudgeOccurrenceDTO(filed)", JudgeOccurrenceDTO{FiledIssue: &JudgeFiledIssueRefDTO{}},
		"run_id", "run_title", "review_id", "rec_id", "verdict", "confidence", "bucket", "filed_issue")
	// The auto-done shape: PRD #98 Decision 6's "done via #IID" needs BOTH, since the label
	// names the issue whose closure produced the done.
	assertTags(t, "JudgeOccurrenceDTO(auto-done)",
		JudgeOccurrenceDTO{SetVia: "issue_close", FiledIssue: &JudgeFiledIssueRefDTO{}},
		"run_id", "run_title", "review_id", "rec_id", "verdict", "confidence", "bucket",
		"filed_issue", "set_via")
}

// A hand-marked done and an issue-close auto-done must be DISTINGUISHABLE on the wire —
// that is the entire reason set_via exists, and until PRD #98 review B3 the column never
// left the store, so every client rendered the two identically. Asserting the two encodings
// DIFFER is the pin; asserting only that an auto-done carries the field would still pass if
// a hand-set one carried it too.
func TestJudgeOccurrenceAutoDoneIsDistinguishableOnTheWire(t *testing.T) {
	handSet, err := json.Marshal(JudgeOccurrenceDTO{Bucket: "done"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	autoDone, err := json.Marshal(JudgeOccurrenceDTO{Bucket: "done", SetVia: "issue_close"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(handSet) == string(autoDone) {
		t.Fatal("a hand-marked done and an issue_close auto-done encode identically — the provenance never reaches a client")
	}
	if strings.Contains(string(handSet), "set_via") {
		t.Errorf("a hand-set disposition must OMIT set_via, got: %s", handSet)
	}
	if !strings.Contains(string(autoDone), `"set_via":"issue_close"`) {
		t.Errorf("an auto-done must carry set_via=issue_close, got: %s", autoDone)
	}
}

func TestJudgeFiledIssueRefDTOTags(t *testing.T) {
	assertTags(t, "JudgeFiledIssueRefDTO", JudgeFiledIssueRefDTO{}, "issue_iid", "issue_url", "filed_at")
}

func TestJudgeRecommendationGroupDTOTags(t *testing.T) {
	assertTags(t, "JudgeRecommendationGroupDTO", JudgeRecommendationGroupDTO{},
		"category", "target", "bucket", "open_count", "run_count", "rationale_preview", "occurrences")
}

func TestJudgeBacklogDTOTags(t *testing.T) {
	assertTags(t, "JudgeBacklogDTO", JudgeBacklogDTO{}, "bucket", "run", "groups", "truncated", "triage")
}

// The bulk fan-out's reply had NO wire pin at all until PRD #98 review BLK-UNDO, which is
// why `settled` could be added without anything noticing the shape moved. `settled` is
// deliberately NOT omitempty: a consumer must be able to distinguish "this call settled
// nothing" from "this server does not send the field", and an omitted key collapses the two.
func TestJudgeDispositionResultDTOTags(t *testing.T) {
	assertTags(t, "JudgeDispositionResultDTO", JudgeDispositionResultDTO{},
		"updated", "settled", "groups", "truncated", "triage")
}

func TestJudgeSettledMemberDTOTags(t *testing.T) {
	assertTags(t, "JudgeSettledMemberDTO", JudgeSettledMemberDTO{}, "run_id", "rec_id")
}

// An empty fan-out must marshal `settled` as [], never null: the client iterates it to build
// its undo set, and a null would make "nothing was settled" an error case at every consumer
// instead of an empty loop. Concretely, `undo: res.settled` → null → UndoToast reading
// `toast.undo.length` → TypeError on any zero-updated dismiss.
//
// READ THIS BEFORE PRUNING EITHER TEST. What follows is a property of encoding/json, NOT of
// the code that builds the response: it marshals a HAND-BUILT struct whose Settled is
// already `[]JudgeSettledMemberDTO{}`, so it cannot fail for the reason its name gives
// (PRD #98 review). Proven: making `settled` a nil slice in the service leaves this test
// GREEN and reddens handler.TestBulkDispositionSettledEmptyWhenNothingMatched with
// `{"settled":null}`.
//
// THE HANDLER TEST IS THE REAL GUARD. This one is kept as the documented statement of the
// wire contract — the place a reader looks for what the field must encode as — and is
// deliberately NOT the thing standing between the codebase and that TypeError. Do not delete
// the handler test on the theory that the wire pin covers it; it is the other way round.
func TestJudgeDispositionResultEmptySettledIsArray(t *testing.T) {
	b, err := json.Marshal(JudgeDispositionResultDTO{Settled: []JudgeSettledMemberDTO{}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"settled":[]`) {
		t.Errorf("want an empty ARRAY for settled, got: %s", b)
	}
	// And the failure mode itself, so the CONTRAST is on the page rather than implied: a nil
	// slice encodes as null, which is exactly what the service must never produce.
	nilB, err := json.Marshal(JudgeDispositionResultDTO{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(nilB), `"settled":null`) {
		t.Errorf("expected a NIL slice to encode as null (the state the service must avoid), got: %s", nilB)
	}
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

func TestSecretDTOTags(t *testing.T) {
	// PRD #104 M2: the token-list element. It MUST NOT carry any value/ciphertext
	// field — the pin is here so a future field addition that leaks the secret trips
	// this test.
	assertTags(t, "SecretDTO", SecretDTO{},
		"id", "kind", "label", "is_default",
		// PRD #111 M2: the auto-selection pool opt-in. A flag the owner set, not a
		// value — it names no credential and reveals nothing about one.
		"auto_eligible",
		"created_at", "updated_at")
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
	// PRD #113: derived upgrade health, computed at read time from `version` against
	// the control-plane release. Derived rather than stored, so nothing in the store
	// layer pins it — this tag set is the only wire contract these two fields have.
	"upgrade_status", "upgrade_detail", "upgrade_target",
	"upgrade_blocking_container", "upgrade_blocking_reason", "upgrade_last_exit_code",
	"last_heartbeat_at", "created_at", "stats_cpu_pct", "stats_mem_bytes",
	"stats_mem_limit_bytes", "stats_source",
	// PRD #104 M3: which Anthropic credential this worker's run-lane claims spend.
	// Both null ⇒ unbound ⇒ the owner's default. The LABEL, never the token value —
	// this DTO is the shape the web UI and the CLI both read.
	"anthropic_secret_id", "anthropic_secret_label",
	// PRD #111 M3: HOW this worker chooses — default | pinned | auto. The server
	// reports the EFFECTIVE mode, so "pinned" always has an id beside it.
	"anthropic_bind_mode",
}

func TestWorkerDTOTags(t *testing.T) {
	assertTags(t, "WorkerDTO", WorkerDTO{}, workerDTOKeys...)
}

func TestAdminWorkerDTOTags(t *testing.T) {
	want := append(append([]string{}, workerDTOKeys...), "owner_email")
	assertTags(t, "AdminWorkerDTO", AdminWorkerDTO{}, want...)
}

// TestAdminCLITokenDTOTags is a security pin, not only a wire pin. The admin
// credential inventory must never carry the token value or its sha256, and this is
// the mechanism that makes adding one a build failure rather than a review catch —
// assertTags demands the key set match EXACTLY, so a stray "token_hash" or "token"
// field fails here even though nothing else in the stack would notice.
func TestAdminCLITokenDTOTags(t *testing.T) {
	assertTags(t, "AdminCLITokenDTO", AdminCLITokenDTO{},
		"id", "user_id", "owner_email", "name", "token_prefix", "scope", "revoked",
		"created_at", "last_used_at", "last_used_ip", "expires_at")
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
		"secret_id", "label", "is_default",
		// PRD #111 M2: the pool opt-in and the SERVER-COMPUTED live eligibility.
		// auto_status is on the wire as a string precisely so no client re-derives
		// it from the windows below (D21).
		"auto_eligible", "auto_status",
		"limits")
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

// RunEventDTO is the /api/ws frame (PRD #112). Both halves matter and the second is
// the load-bearing one: every field except `type` is omitempty, so a pin on the ZERO
// value asserts exactly one key and would sail through a rename of the other eight.
// The populated pin is what actually holds the wire contract — and this type is
// aliased by hub.Event, so it is one shape for the server, the web and the CLI.
func TestRunEventDTOTags(t *testing.T) {
	// Zero value: the signal frames (state/health/input) really do serialize to just
	// a type, which is the property the CLI's seq-less recovery contract rests on.
	assertTags(t, "RunEventDTO(zero)", RunEventDTO{}, "type")

	agent, instance, label := "coder", "toolu_01", "write the tests"
	now := time.Unix(0, 0)
	full := RunEventDTO{
		Type:          "message",
		Seq:           7,
		Kind:          "text",
		Agent:         &agent,
		AgentInstance: &instance,
		AgentLabel:    &label,
		Payload:       json.RawMessage(`{"text":"hi"}`),
		CreatedAt:     &now,
		Status:        "running",
	}
	assertTags(t, "RunEventDTO(full)", full,
		"type", "seq", "kind", "agent", "agent_instance", "agent_label",
		"payload", "created_at", "status")
}
