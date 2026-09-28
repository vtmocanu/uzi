package handler

import (
	"bytes"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// PRD #1795 M3 (D4): a new worker sends presentation_id / adopt_gate_revision on its
// awaiting_approval report ONLY when the api advertised the gate_revision_v1 register feature,
// because every api decodes /state strictly (httpx.DecodeJSON, DisallowUnknownFields): an older
// api that meets an unknown field answers 400 and the run can never gate.
//
// fixtures/worker-state-request/ holds the exact bodies the worker sends in both cases, pinned
// on the agent side by agent/test/runner-gate-revision-reclaim.test.ts (which captures the real
// runner's report). This test feeds them through the REAL strict decoder:
//   - the non-negotiated body decodes into stateRequestMinusGateRevision, the current
//     StateRequest minus the #1795 fields (the field set an api WITHOUT #1795 declares, plus
//     any field added since), so an older api accepts it;
//   - the negotiated body fails that same decode on "presentation_id", proving the send gate is
//     what keeps an older api working;
//   - both decode into the current StateRequest.
//
// Run with -count=1 after a fixture-only edit (fixtures/ is outside this package's cache key).
const workerStateFixtureDir = "../../../fixtures/worker-state-request"

// stateRequestMinusGateRevision mirrors the CURRENT workersvc.StateRequest minus the two #1795
// fields: every other field in order, with the same type and JSON tag. It tracks fields added
// after #1795 by design (TestWorkerStateDecodeMirrorInLockstep fails until they are mirrored
// here), so it is not a frozen snapshot of the pre-#1795 struct.
type stateRequestMinusGateRevision struct {
	State                    string                     `json:"status"`
	ClaimGeneration          *int64                     `json:"claim_generation"`
	MessagesThroughSeq       *int64                     `json:"messages_through_seq"`
	PlanMd                   *string                    `json:"plan_md"`
	Branch                   *string                    `json:"branch"`
	MrIID                    *int64                     `json:"mr_iid"`
	Head                     *string                    `json:"head"`
	MrWebURL                 *string                    `json:"mr_web_url"`
	FailureReason            *string                    `json:"failure_reason"`
	IterationCount           int32                      `json:"iteration_count"`
	SessionID                *string                    `json:"session_id"`
	FixVerdict               *string                    `json:"fix_verdict"`
	PrdDonePath              *string                    `json:"prd_done_path"`
	ReportOnly               *bool                      `json:"report_only"`
	ReportMd                 *string                    `json:"report_md"`
	Proposal                 *workersvc.ProposalPayload `json:"proposal"`
	ScopeCapped              *bool                      `json:"scope_capped"`
	BranchMoved              *bool                      `json:"branch_moved"`
	RepoAgents               *[]workersvc.RepoAgent     `json:"repo_agents"`
	PlanChangedFiles         *[]string                  `json:"plan_changed_files"`
	Milestones               *[]workersvc.Milestone     `json:"milestones"`
	MilestonesCompleted      *[]string                  `json:"milestones_completed"`
	MilestonesInProgress     *[]string                  `json:"milestones_in_progress"`
	MilestonesAgents         *[]apitypes.MilestoneAgent `json:"milestones_agents"`
	SeededFromDefault        *bool                      `json:"seeded_from_default"`
	AgentSelection           *workersvc.AgentSelection  `json:"agent_selection"`
	OpenQuestionID           *string                    `json:"open_question_id"`
	CompletionQuestion       bool                       `json:"completion_question"`
	OpenFollowupID           *int64                     `json:"open_followup_id"`
	LimitResetsAt            *int64                     `json:"limit_resets_at"`
	RateLimitType            *string                    `json:"rate_limit_type"`
	FailOrigin               *string                    `json:"fail_origin"`
	PreservedPatch           *string                    `json:"preserved_patch"`
	RequiredCapabilities     *[]string                  `json:"required_capabilities"`
	RequiredTools            *[]string                  `json:"required_tools"`
	SizeClass                *string                    `json:"size_class"`
	RecoveryCause            *string                    `json:"recovery_cause"`
	DiskParkPreventive       *bool                      `json:"disk_park_preventive"`
	CheckpointContainsLatest *bool                      `json:"checkpoint_contains_latest"`
}

// gateRevisionStateFields are the StateRequest fields PRD #1795 added (the only difference
// between the mirror above and the current struct).
var gateRevisionStateFields = map[string]bool{"PresentationID": true, "AdoptGateRevision": true}

func TestWorkerStateDecodeMirrorInLockstep(t *testing.T) {
	cur := reflect.TypeOf(workersvc.StateRequest{})
	mirror := reflect.TypeOf(stateRequestMinusGateRevision{})
	var want []reflect.StructField
	seen := map[string]bool{}
	for i := 0; i < cur.NumField(); i++ {
		f := cur.Field(i)
		if gateRevisionStateFields[f.Name] {
			seen[f.Name] = true
			continue
		}
		want = append(want, f)
	}
	for name := range gateRevisionStateFields {
		if !seen[name] {
			t.Fatalf("workersvc.StateRequest no longer has %s; update gateRevisionStateFields", name)
		}
	}
	if mirror.NumField() != len(want) {
		t.Fatalf("stateRequestMinusGateRevision has %d fields, StateRequest minus the #1795 fields has %d: keep them in lockstep", mirror.NumField(), len(want))
	}
	for i, f := range want {
		m := mirror.Field(i)
		if m.Name != f.Name || m.Type != f.Type || m.Tag != f.Tag {
			t.Errorf("field %d: mirror %s %v `%s`, StateRequest %s %v `%s`", i, m.Name, m.Type, m.Tag, f.Name, f.Type, f.Tag)
		}
	}
}

// decodeStateFixture reads a fixture and decodes it through httpx.DecodeJSON exactly as the
// /state handler does, from a real *http.Request.
func decodeStateFixture(t *testing.T, name string, dst any) error {
	t.Helper()
	// os.Root confines the read to the fixture directory: a name cannot climb out of it.
	root, err := os.OpenRoot(workerStateFixtureDir)
	if err != nil {
		t.Fatalf("open fixture dir %s: %v", workerStateFixtureDir, err)
	}
	defer func() { _ = root.Close() }()
	body, err := root.ReadFile(name)
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	r := httptest.NewRequest("POST", "/api/worker/runs/00000000-0000-0000-0000-000000000001/state", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	return httpx.DecodeJSON(r, dst)
}

func TestWorkerStateDecodeWithoutGateRevisionFeature(t *testing.T) {
	var old stateRequestMinusGateRevision
	if err := decodeStateFixture(t, "awaiting_approval.json", &old); err != nil {
		t.Fatalf("an api without #1795 must accept the non-negotiated awaiting_approval report: %v", err)
	}
	if old.State != "awaiting_approval" || old.PlanMd == nil || old.ClaimGeneration == nil {
		t.Fatalf("decoded an unexpected report: %+v", old)
	}
	var cur workersvc.StateRequest
	if err := decodeStateFixture(t, "awaiting_approval.json", &cur); err != nil {
		t.Fatalf("the current api must accept it: %v", err)
	}
	if cur.PresentationID != nil || cur.AdoptGateRevision != nil {
		t.Fatalf("the non-negotiated report carried a presentation field: %+v", cur)
	}
}

func TestWorkerStateDecodeWithGateRevisionFeature(t *testing.T) {
	var old stateRequestMinusGateRevision
	err := decodeStateFixture(t, "awaiting_approval.gate_revision_v1.json", &old)
	if err == nil || !strings.Contains(err.Error(), `unknown field "presentation_id"`) {
		t.Fatalf("an api without #1795 must reject the negotiated report on presentation_id, got %v", err)
	}
	var cur workersvc.StateRequest
	if err := decodeStateFixture(t, "awaiting_approval.gate_revision_v1.json", &cur); err != nil {
		t.Fatalf("the current api must accept the negotiated report: %v", err)
	}
	if cur.PresentationID == nil || *cur.PresentationID == uuid.Nil {
		t.Fatalf("the negotiated report carried no presentation id: %+v", cur)
	}
}
