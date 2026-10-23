package workersvc

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
)

func malformedPublicationContract(t *testing.T, raw []byte, defect string) []byte {
	t.Helper()
	var c map[string]any
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	cr := c["criteria"].([]any)[0].(map[string]any)
	switch defect {
	case "acceptance":
		c["accepted"] = []any{map[string]any{"id": "m1.c1", "milestone_id": "foreign", "text": cr["text"], "reason": "", "revision": 99}}
	case "acceptance_text":
		c["accepted"] = []any{map[string]any{"id": "m1.c1", "milestone_id": "m1", "text": "changed", "reason": "owner", "revision": 1}}
	case "criterion_text":
		cr["text"] = "changed"
	case "audit_missing":
		delete(cr, "audit")
	case "audit_value":
		cr["audit"] = map[string]any{}
	case "findings_null":
		cr["finding_ids"] = nil
	case "findings_nonempty":
		cr["finding_ids"] = []string{"foreign"}
	}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSourcePublicationStructuralAuthority(t *testing.T) {
	w := worker()
	run := interlockedRun(w)
	run.MilestonesFrozen = frozenJSON(t, "m1")
	run.MilestonesCompleted = idsJSON(t, "m1")
	raw, err := buildCompletionContract(run.MilestonesFrozen)
	if err != nil {
		t.Fatal(err)
	}
	repo, requestID := uuid.New(), uuid.New()
	branch, mr, iid := "agent/issue-1", int64(7), int64(1)
	version, revision := int32(1), int32(1)
	stamp, err := json.Marshal(SourcePublicationIntent{1, 1, branch, mr, strings.Repeat("a", 40), false, "closing"})
	if err != nil {
		t.Fatal(err)
	}
	req := PublicationContextRequest{1, branch, strings.Repeat("b", 40)}
	source := publicationAuthority{ID: run.ID, UserID: w.UserID, RepoID: &repo, Kind: "issue", Status: "completed", Branch: &branch, MRIID: &mr, IssueIID: &iid, ContractVersion: &version, ContractRevision: &revision, MilestonesFrozen: run.MilestonesFrozen, MilestonesCompleted: run.MilestonesCompleted, Stamp: stamp}
	snapshot := publicationSnapshot{ConnectionID: &repo, ProjectID: 1, Request: publicationAuthority{ID: requestID, UserID: w.UserID, RepoID: &repo, WorkerID: &w.ID, Kind: "ci_fix", Status: "running", ClaimGeneration: 1, PipelineRef: &branch}}
	for _, defect := range []string{"valid", "acceptance", "acceptance_text", "criterion_text", "audit_missing", "audit_value", "findings_null", "findings_nonempty"} {
		t.Run(defect, func(t *testing.T) {
			c := raw
			if defect != "valid" {
				c = malformedPublicationContract(t, raw, defect)
			}
			run.CompletionContract = c
			run.CompletionContractVersion = pgconv.Int4Ptr32(&version)
			run.ContractRevision = pgconv.Int4Ptr32(&revision)
			_, err := sourcePublicationRestriction(run, false)
			source.Contract = c
			snapshot.Nodes = []publicationNode{{Run: source, Seed: source.ID}}
			got := resolvePublicationSnapshot(snapshot, w, requestID, req)
			if defect == "valid" {
				if err != nil || got.Status != "resolved" || got.Binding.Intent != "closing" {
					t.Fatalf("positive control: %v %+v", err, got)
				}
			} else if err == nil || got.Status != "unknown" {
				t.Fatalf("malformed authority closed: %v %+v", err, got)
			}
		})
	}
}

func TestSourcePublicationStructuralAdmissionLiveDB(t *testing.T) {
	for _, defect := range []string{"acceptance", "acceptance_text", "criterion_text", "audit_missing", "audit_value", "findings_null", "findings_nonempty"} {
		t.Run(defect, func(t *testing.T) {
			f := seedSourcePublication(t, true)
			run, err := f.e.q.GetRunByID(f.e.ctx, f.id)
			if err != nil {
				t.Fatal(err)
			}
			f.e.exec(t, "UPDATE runs SET completion_contract=$2 WHERE id=$1", f.id, malformedPublicationContract(t, run.CompletionContract, defect))
			result, err := f.svc.SetStateReportWithReconciliation(f.e.ctx, f.w, f.id, f.req)
			status, stamp, spent := f.stored(t)
			if (err == nil && result.Applied) || status != "running" || len(stamp) != 0 || spent {
				t.Fatalf("invalid terminal admission: %+v %v %s %s spent=%t", result, err, status, stamp, spent)
			}
		})
	}
}
