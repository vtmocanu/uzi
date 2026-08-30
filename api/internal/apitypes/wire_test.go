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
		"autopilot_enabled", "judge_enabled", "ci_autofix_enabled",
		// PRD #649: the per-user opt-in to ephemeral worker auto-provisioning.
		"ephemeral_workers_enabled",
		// PRD #35 Decision 7: the per-user DEFAULT for the usage-limit park. A default,
		// not a policy — every run carries its own, stamped at creation.
		"wait_on_limit",
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
	// PRD #764 M2: server-computed PRD presence for the runs view, always on the wire
	// (bool, false for issue-less run kinds whose description carries no prds link).
	"has_prd_link",
	"title", "resume_of_run_id", "status", "requeue_count", "iteration_count",
	// issue #321: server-computed planning-phase display flag, always on the wire (bool,
	// false for every non-planning and pre-feature run). Derived, not stored.
	"is_planning",
	// PRD #122 M1: the FROZEN milestone list, always on the wire (nil ⇒ null ⇒ a run
	// with no milestones, which is every pre-feature run).
	"auto_approve", "milestones",
	// PRD #122 M3: the PRE-APPROVAL candidate list (nil ⇒ null), always on the wire; the
	// web renders it only at the plan gate so the human approves the proposed breakdown.
	"milestones_candidate",
	// PRD #122 M2: live progress (id arrays, nil ⇒ null) and the effective per-run
	// budget (nil ⇒ null ⇒ the global default). All four always present; a client
	// branches per field. budget_* are load-bearing on the state-ack, not just display.
	"milestones_completed", "milestones_in_progress", "budget_max_iterations", "budget_wall_seconds",
	// PRD #634 M2: the operator scope ceiling (nil ⇒ null ⇒ unbounded), always on the wire;
	// like budget_* it is load-bearing on the state-ack, not just display — the worker honors
	// it at the loop top.
	"scope_ceiling",
	"worker_id", "branch",
	// PRD #400 (uzi handoff): the task/handoff columns, meaningful only on a
	// kind='task' run. base_branch is null on every non-task run; open_mr is false by
	// default and on every non-task run. Always on the wire.
	"base_branch", "open_mr",
	// PRD #517 M1: interactive marks a long-lived task run; false by default and on every
	// non-task run. Always on the wire, like open_mr above.
	"interactive",
	// PRD #400 Decision 6: when a task run's dispatch gate was stamped (null on every
	// non-task run and on a task run not yet dispatched). Always on the wire.
	"dispatched_at",
	"mr_iid", "mr_web_url", "mr_state", "failure_reason",
	// issue #403 F1/F6: branch-wide `uzi handoff rm` preconditions, server-computed on the
	// GetRun detail read for a task run (false elsewhere). Always on the wire (bool).
	"branch_has_active_run", "branch_has_open_mr",
	// PRD #411: the forge issue's web URL for the run's clickable #<iid> link. Null for
	// issue-less runs and when the issue is no longer cached. Always on the wire.
	"issue_web_url",
	"stop_kind", "stop_reason", "health", "health_reason", "health_since", "plan_md",
	// PRD #209: plan_md's provenance ("agent"|"seeded"), NOT NULL so always on the wire.
	"plan_source",
	// PRD #362 M1: plain-English run summaries. All three null until the worker posts
	// them (and null forever on any generation failure); summary_deltas is
	// tolerated-on-read (a malformed stored value arrives as null). Always on the wire.
	"summary_intent", "summary_plan", "summary_deltas",
	"pipeline_ref", "pipeline_web_url", "fix_verdict",
	// issue #279: a completed run that opened NO merge request (report-only/evidence
	// completion) and its persisted findings summary. report_only is NOT NULL so always
	// on the wire; report_md is null unless report_only.
	"report_only", "report_md",
	// PRD #377 M1: the agent's diff preserved on a workflow_scope_missing failed run
	// (the branch touched .github/workflows/** the bot PAT cannot push). Nullable, set
	// only on that failed path, but the key is always on the wire.
	"preserved_patch",
	// PRD-link reconciliation (read-only): the path the run declared it archived a PRD
	// to, and when that patch lifecycle settled (null while pending). Both always on the
	// wire — prd_done_path null for a run that moved no PRD, prd_patch_settled_at null
	// until the patch edge is consumed.
	"prd_done_path", "prd_patch_settled_at",
	"claimed_at", "started_at",
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
	// PRD #841 M1: the per-run MR-rework override, tri-state *bool (null = inherit the
	// owner default, resolved live). NOT omitempty — always on the wire, so the web can
	// tell "no per-run opinion" (null) from an explicit true/false and render the
	// effective inherited value.
	"mr_rework_enabled",
	// PRD #300: the per-schedule model a schedule froze onto the run at fire time (nil ⇒
	// null ⇒ the run inherited the owner's per-user default). Always on the wire.
	"model",
	// PRD #305: the frozen "apply model also to agents" flag (bool NOT NULL ⇒ always
	// present; false is the default for a run that did not opt in).
	"override_subagent_model",
	// PRD #320 D8: the DISPLAY priority class ({normal, background, expedited,
	// restored}), computed by fn_run_priority_class. NOT omitempty — always a real
	// value on the wire, so it is asserted on the zero value here.
	"priority",
	// PRD #84: the run's inferred/hinted scheduling requirements, surfaced RAW for the
	// web/CLI readiness/mismatch display. required_capabilities (M2 hint ∪ M4 inference)
	// and required_tools (M4 4b, display-only) are non-nil slices ([] over null); size_class
	// (M4 4b) is the NOT NULL DEFAULT '' string. All three always on the wire, and — via the
	// RunListItemDTO embed pin — on both the list and detail reads.
	"required_capabilities", "required_tools", "size_class",
	// PRD #212: the git-status list surfaced at the approval gate, non-nil ([] over null)
	// via capsOrEmpty like required_tools. On both list and detail via the RunListItemDTO embed.
	"plan_changed_files",
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
	// is_revising (issue #750) is on RunListItemDTO only, deliberately NOT on RunDTO —
	// so it is listed here, not in runDTOKeys.
	want := append(append([]string{}, runDTOKeys...), "repo_path", "worker_name", "judge_verdict", "judge_todo_count", "is_revising")
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
	// PRD #84 M4 4c: override_capabilities is a plain bool (not omitempty), so it is always
	// on the wire; meaningful only with approve_plan, default false.
	assertTags(t, "RunInputRequest", RunInputRequest{}, "kind", "body", "selection", "override_capabilities")
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
	assertTags(t, "SteerInputDTO", SteerInputDTO{}, "id", "kind", "body", "created_at", "consumed_at", "disposition")
}

func TestRecommendationDTOTags(t *testing.T) {
	assertTags(t, "RecommendationDTO", RecommendationDTO{},
		"id", "category", "target", "rationale_md", "confidence", "created_at")
}

func TestReviewDTOTags(t *testing.T) {
	// judge_run is omitempty (PRD #69 M6): a review with no judge-run detail omits it
	// rather than shipping a null the consumer special-cases, so the zero value carries
	// the pre-#69 key set exactly.
	assertTags(t, "ReviewDTO", ReviewDTO{},
		"id", "target_run_id", "verdict", "summary_md", "judge_model", "status",
		"created_at", "updated_at", "recommendations", "filed_issues",
		"dispositions", "triage")
	// With a judge run attached, the nested strip surfaces under judge_run.
	assertTags(t, "ReviewDTO(judge_run)", ReviewDTO{JudgeRun: &JudgeRunDTO{}},
		"id", "target_run_id", "verdict", "summary_md", "judge_model", "status",
		"created_at", "updated_at", "recommendations", "filed_issues",
		"dispositions", "triage", "judge_run")
}

func TestTaskReviewDTOTags(t *testing.T) {
	// PRD #400 M4a: review_run_id is a *string present-with-null (nil when the review run
	// was deleted), so it is NOT omitempty — every key is always on the wire.
	assertTags(t, "TaskReviewDTO", TaskReviewDTO{},
		"target_run_id", "review_run_id", "status", "summary_md", "findings", "created_at")
}

func TestTaskReviewFindingDTOTags(t *testing.T) {
	assertTags(t, "TaskReviewFindingDTO", TaskReviewFindingDTO{},
		"file", "symbol", "line", "severity", "summary", "rationale")
}

func TestJudgeRunDTOTags(t *testing.T) {
	// claimed_at/started_at/finished_at and usage are NOT omitempty: the timings are
	// present-with-null for a pre-feature judge (started_at null), and usage is null when
	// the judge posted no result frame — an absent strip is a null usage, read explicitly.
	assertTags(t, "JudgeRunDTO", JudgeRunDTO{},
		"judge_run_id", "claimed_at", "started_at", "finished_at", "usage")
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

// TestJudgeCategoryStatsDTOTags pins the single top-level key `counts_by_bucket` (PRD #270).
// A NEW DTO gets its own tag test rather than widening TriageDTO, keeping the six-bucket
// triage contract untouched. The nested map shape (bucket → category → count) is not a
// top-level key, so this pins the wire envelope the web reads as
// `counts_by_bucket[tab][cat] ?? 0`.
func TestJudgeCategoryStatsDTOTags(t *testing.T) {
	assertTags(t, "JudgeCategoryStatsDTO", JudgeCategoryStatsDTO{}, "counts_by_bucket")
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

func TestPendingJudgeDTOTags(t *testing.T) {
	assertTags(t, "PendingJudgeDTO", PendingJudgeDTO{}, "state", "enqueued_at")
}

// TestIncidentalFindingDTOTags pins the PRD #333 M4 backlog-row shape. finding_id,
// filed_issue_iid, filed_issue_url and resolved_at are all omitempty: a display-only
// coordinate whose evidence was cascaded away carries no finding_id, and an OPEN coordinate
// carries neither a filed iid, a filed url, nor a resolved_at. The zero-value pin asserts the
// always-present key set (mirroring how runDTOKeys excludes the omitempty Usage), and the
// populated pin asserts the four optional keys surface when set.
func TestIncidentalFindingDTOTags(t *testing.T) {
	assertTags(t, "IncidentalFindingDTO", IncidentalFindingDTO{},
		"location", "repo_id", "repo_path", "status", "last_title", "seen_in_runs")
	id := "f1"
	iid := int64(7)
	now := time.Unix(0, 0)
	full := IncidentalFindingDTO{FindingID: &id, FiledIssueIID: &iid, FiledIssueURL: "https://forge.example/g/a/-/issues/7", ResolvedAt: &now}
	assertTags(t, "IncidentalFindingDTO(full)", full,
		"finding_id", "location", "repo_id", "repo_path", "status", "last_title",
		"seen_in_runs", "filed_issue_iid", "filed_issue_url", "resolved_at")
}

func TestIncidentalFindingBacklogDTOTags(t *testing.T) {
	assertTags(t, "IncidentalFindingBacklogDTO", IncidentalFindingBacklogDTO{},
		"bucket", "repo", "run", "open_count", "findings")
}

func TestIncidentalFindingIssueDraftDTOTags(t *testing.T) {
	assertTags(t, "IncidentalFindingIssueDraftDTO", IncidentalFindingIssueDraftDTO{},
		"title", "description", "location", "labels", "provenance")
}

// TestIncidentalFindingFileResultDTOTags pins the PRD #333 M5/M6 file-response shape. warning
// is omitempty (absent on a clean file, present on created-with-warning), so the zero-value pin
// asserts only `issue` and the populated pin surfaces `warning`.
func TestIncidentalFindingFileResultDTOTags(t *testing.T) {
	assertTags(t, "IncidentalFindingFiledIssueDTO", IncidentalFindingFiledIssueDTO{},
		"iid", "web_url", "title")
	assertTags(t, "IncidentalFindingFileResultDTO", IncidentalFindingFileResultDTO{}, "issue")
	assertTags(t, "IncidentalFindingFileResultDTO(warn)",
		IncidentalFindingFileResultDTO{Warning: "x"}, "issue", "warning")
}

// TestReviewNullEnvelope pins the GET /api/runs/{id}/review contract that a
// visible-but-unjudged run returns 200 with BOTH envelope keys present and null, and
// that a null on either is a valid decode — the CLI/SPA must model review AND
// pending_judge as nullable, not treat null as an error (PRD #64 M1 A.3, widened by
// PRD #119 M1).
//
// The two keys are independently nullable and every combination is legal: (null, null)
// is a never-judged run, (null, set) is one whose auto-judge is still in flight, (set,
// set) is a re-judge in flight, (set, null) is a settled verdict. That is why
// pending_judge is a sibling key rather than a ReviewDTO field — the state it most
// needs to describe is exactly the one where review is null.
//
// The literals below are what json.Marshal actually emits: a map's keys are sorted, so
// pending_judge precedes review regardless of the order they are written here.
func TestReviewNullEnvelope(t *testing.T) {
	b, err := json.Marshal(map[string]any{"review": (*ReviewDTO)(nil), "pending_judge": (*PendingJudgeDTO)(nil)})
	if err != nil {
		t.Fatalf("marshal null review envelope: %v", err)
	}
	if string(b) != `{"pending_judge":null,"review":null}` {
		t.Fatalf("null review envelope = %s, want {\"pending_judge\":null,\"review\":null}", b)
	}
	// A populated review round-trips through the same envelope key, with pending_judge
	// still present and null (the settled-verdict case).
	b, err = json.Marshal(map[string]any{"review": ReviewDTO{ID: "r1"}, "pending_judge": (*PendingJudgeDTO)(nil)})
	if err != nil {
		t.Fatalf("marshal review envelope: %v", err)
	}
	if !strings.Contains(string(b), `"review":{`) || !strings.Contains(string(b), `"pending_judge":null`) {
		t.Fatalf("populated review envelope = %s, want a populated review and a null pending_judge", b)
	}
	// The unjudged-but-pending case: review null, pending_judge populated. This is the
	// shape the run page reads to say "Judge scheduled" instead of "not judged yet".
	b, err = json.Marshal(map[string]any{
		"review":        (*ReviewDTO)(nil),
		"pending_judge": PendingJudgeDTO{State: "scheduled"},
	})
	if err != nil {
		t.Fatalf("marshal pending envelope: %v", err)
	}
	if !strings.Contains(string(b), `"review":null`) || !strings.Contains(string(b), `"state":"scheduled"`) {
		t.Fatalf("pending envelope = %s, want a null review and a populated pending_judge", b)
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
		"default_branch", "enabled", "repo_skills_enabled", "repo_claudemd_enabled", "repo_devbox_opt_in",
		// PRD #686 D1: the per-repo self-improve dogfooding capability (folds the
		// improve_uzi backlog + selects the uzi worker directive). Default false.
		"repo_fold_improve_uzi_backlog",
		// PRD #84 M2: the static per-repo capability hint routed onto each run.
		"required_capabilities", "pipeline",
		// PRD #66 M8 (D8): admin per-repo guardrail override metadata, null when no
		// override is active. Display-only surfacing for M9's badge.
		"guardrail_override",
		// PRD #66 M9 (D8): the server-computed "would a run be refused now" bool the
		// web reads for the badge STATE (override already applied, single Go rule).
		"guardrail_blocked",
		// PRD #361 M1: the computed, caller-scoped "is this repo on the global Docker-
		// worker allowlist" bool. Never the list itself.
		"docker_allowlisted",
		// PRD #361 M3: the computed, caller-scoped "is a run on this repo actually
		// blocked by the Docker-allowlist gap right now" bool, from eligibility directly.
		"docker_blocked")
}

func TestGuardrailOverrideDTOTags(t *testing.T) {
	assertTags(t, "GuardrailOverrideDTO", GuardrailOverrideDTO{},
		"reason", "by", "at")
}

func TestPipelineDTOTags(t *testing.T) {
	assertTags(t, "PipelineDTO", PipelineDTO{},
		"ref", "status", "web_url", "pipeline_id", "synced_at")
}

// workerDTOKeys is the WorkerDTO tag set (no omitempty fields). Shared by the
// WorkerDTO pin and the AdminWorkerDTO embed pin.
var workerDTOKeys = []string{
	"id", "name", "status", "kind", "hosted_size", "docker",
	// PRD #529: marks an auto-provisioned, run-bound throwaway hosted worker; display-only,
	// always false for an external or a standing hosted worker.
	"ephemeral",
	"busy", "active_runs",
	// PRD #84 M1: the server-authoritative capability set (Filter-ed union of the
	// worker's self-report and its template-derived caps, v1 vocabulary {docker, jvm}).
	// Read-only display for the workers UI.
	"capabilities",
	"max_concurrent_runs", "template_declared", "template_reported", "version",
	// PRD #113: derived upgrade health, computed at read time from `version` against
	// the control-plane release. Derived rather than stored, so nothing in the store
	// layer pins it — this tag set is the only wire contract these two fields have.
	"upgrade_status", "upgrade_detail", "upgrade_target",
	"upgrade_blocking_container", "upgrade_blocking_reason", "upgrade_last_exit_code",
	"last_heartbeat_at",
	// PRD #251: api-owned anchor of when the worker became online; null when offline or
	// never online. Uptime is derived client-side as now − online_since; display-only.
	"online_since",
	// PRD #422/#496: when a hosted worker was cordoned/began draining (finishes in-flight
	// runs, claims nothing new); null for a worker that will claim normally. Hosted-only.
	"draining_since",
	"created_at", "stats_cpu_pct", "stats_mem_bytes",
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

// TestUserSettingsDTOTags pins the CLI's decoding mirror of GET /api/me/settings
// against the handler's own userSettingsDTO — a divergence in either type's tag
// set fails here rather than silently dropping a field on decode.
func TestUserSettingsDTOTags(t *testing.T) {
	assertTags(t, "UserSettingsDTO", UserSettingsDTO{},
		"default_model", "default_effort", "judge_model", "summary_model", "theme", "sidebar_token_ids",
		// PRD #700 M5: the per-user MR-review-watcher opt-in (default ON; null clears to default).
		"mr_rework_enabled")
}

// TestAgentMemoryWriteRequestTags pins the worker save body: {title, body} plus the
// writer-declared provenance {basis, evidence} (PRD #266). No identity fields exist
// on the wire, so (user_id, repo_id) can only be derived server-side from the run
// claim (PRD #90 B3). basis/evidence are not omitempty — the worker always sends the
// keys, and a bad/empty basis never fails the write (it is normalized on read).
func TestAgentMemoryWriteRequestTags(t *testing.T) {
	assertTags(t, "AgentMemoryWriteRequest", AgentMemoryWriteRequest{}, "title", "body", "basis", "evidence")
}

// TestAgentMemoryDTOTags pins the read shape. repo_id/repo_name/run_id are
// omitempty: the worker per-(user,repo) read omits the redundant repo fields, and
// run_id drops when the writing run is pruned (FK SET NULL). The user-facing
// cross-repo list sets all of them. basis (PRD #266) is ALWAYS present — the read
// mapper defaults an unknown/NULL value to "inferred", so it is never omitempty;
// evidence is omitempty and drops when empty.
func TestAgentMemoryDTOTags(t *testing.T) {
	assertTags(t, "AgentMemoryDTO(worker)", AgentMemoryDTO{}, "id", "title", "body", "basis", "created_at")
	full := AgentMemoryDTO{ID: "m1", RepoID: "r1", RepoName: "org/repo", Title: "t", Body: "b", RunID: "run1", Basis: "observed", Evidence: "cmd: go test"}
	assertTags(t, "AgentMemoryDTO(user list)", full,
		"id", "repo_id", "repo_name", "title", "body", "run_id", "basis", "evidence", "created_at")
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

// BuildInfoDTO is the unauthenticated GET /api/version response (PRD #175). Both
// halves matter and for opposite reasons.
//
// The DEGRADED pin is the one with teeth: an un-stamped build — every laptop, every
// compose stack, every MR validation image — is the common case, and the contract is
// that its unknown fields are ABSENT rather than empty. A typed decode cannot tell
// those apart, so nothing else in the tree pins it.
//
// The `version` key in both is older than this type and outlives any reshape of it:
// web/src/pages/WorkersSettings.tsx feeds it to PRD #113's worker upgrade
// classification, so renaming or nesting it is a coordinated change across the SPA
// and a feature that gates fleet upgrades.
func TestBuildInfoDTOTags(t *testing.T) {
	assertTags(t, "BuildInfoDTO(unstamped)", BuildInfoDTO{}, "version", "founded")

	commits, uptime := 2060, int64(5400)
	prdsDone, prdsOpen := 80, 32
	full := BuildInfoDTO{
		Version:       "0.11.12",
		Founded:       "2026-07-03",
		BuiltAt:       "2026-07-28T09:15:00Z",
		Commit:        "366a282d52095312f54b99698b241ac872e20284",
		Commits:       &commits,
		PrdsDone:      &prdsDone,
		PrdsOpen:      &prdsOpen,
		UptimeSeconds: &uptime,
	}
	assertTags(t, "BuildInfoDTO(stamped)", full,
		"version", "founded", "built_at", "commit", "commits", "prds_done", "prds_open", "uptime_seconds")

	// uptime_seconds is a pointer so that a genuine 0 (a process in its first second)
	// stays on the wire. omitempty on a bare int64 would drop it and make it
	// indistinguishable from the zero-startedAt "unknown" case.
	zero := int64(0)
	assertTags(t, "BuildInfoDTO(uptime 0)", BuildInfoDTO{UptimeSeconds: &zero},
		"version", "founded", "uptime_seconds")
}

func TestGuardrailImpactDTOTags(t *testing.T) {
	assertTags(t, "GuardrailImpactDTO", GuardrailImpactDTO{},
		"checked_at", "enabled_repo_count", "blocked_count", "unevaluable_count", "repos")
	assertTags(t, "GuardrailImpactRepoDTO", GuardrailImpactRepoDTO{},
		"repo_id", "path", "user_id", "connection_id", "blocked", "unevaluable")
}

func TestAdminBlockedReposDTOTags(t *testing.T) {
	assertTags(t, "AdminBlockedReposDTO", AdminBlockedReposDTO{}, "repos", "checks_unknown")
	// guardrail_override is null when no override is active but the key is always on
	// the wire (a blocked-not-overridden repo carries it as null); privilege_status /
	// privilege_checked_at are pointers, present-as-null when the connection was never
	// checked, so all keys are asserted on the zero value.
	assertTags(t, "BlockedRepoDTO", BlockedRepoDTO{},
		"id", "path", "owner_id", "owner_email", "forge_type", "blocked",
		"block_messages", "guardrail_override", "privilege_status", "privilege_checked_at")
}
