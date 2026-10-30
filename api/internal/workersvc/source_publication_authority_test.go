package workersvc

import (
	"encoding/json"
	"reflect"
	"sort"
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
			got, _ := resolvePublicationSnapshot(snapshot, w, requestID, req)
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

func publicationKeys(t *testing.T, v any) []string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func TestSourcePublicationUnstampedCompatibility(t *testing.T) {
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
	head := strings.Repeat("a", 40)
	candidate := strings.Repeat("b", 40)
	mkStamp := func(s SourcePublicationIntent) json.RawMessage {
		b, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	req := PublicationContextRequest{1, branch, candidate}
	base := func() publicationAuthority {
		return publicationAuthority{ID: uuid.New(), UserID: w.UserID, RepoID: &repo, Kind: "issue", Status: "completed", Branch: &branch, MRIID: &mr, IssueIID: &iid,
			ContractVersion: &version, ContractRevision: &revision, Contract: raw, MilestonesFrozen: run.MilestonesFrozen, MilestonesCompleted: run.MilestonesCompleted}
	}
	reason := "partial"
	legacy := func(a *publicationAuthority) {
		a.Contract, a.ContractVersion, a.ContractRevision, a.MilestonesFrozen, a.MilestonesCompleted = nil, nil, nil, nil, nil
	}
	type unstampedCase struct {
		name      string
		mutate    func(*publicationAuthority)
		status    string
		reason    string
		intent    string
		fallback  bool
		wantStamp bool
	}
	cases := []unstampedCase{
		{name: "completed_nil_stamp", status: "resolved", intent: "non_closing", fallback: true},
		{name: "completed_null_stamp", mutate: func(a *publicationAuthority) { a.Stamp = json.RawMessage("null") }, status: "resolved", intent: "non_closing", fallback: true},
		{name: "completed_legacy_no_contract", mutate: legacy, status: "resolved", intent: "non_closing", fallback: true},
		{name: "affirmative_restriction_no_stamp", mutate: func(a *publicationAuthority) { a.NonClosingReason = &reason }, status: "resolved", intent: "non_closing"},
		{name: "failed_unstamped", mutate: func(a *publicationAuthority) { a.Status = "failed" }, status: "unknown", reason: "publication_unproven"},
		{name: "running_unstamped", mutate: func(a *publicationAuthority) { a.Status = "running" }, status: "unknown", reason: "publication_unproven"},
		{name: "cancelled_unstamped", mutate: func(a *publicationAuthority) { a.Status = "cancelled" }, status: "unknown", reason: "publication_unproven"},
		{name: "failed_legacy_unstamped", mutate: func(a *publicationAuthority) { legacy(a); a.Status = "failed" }, status: "unknown", reason: "publication_unproven"},
		{name: "report_only", mutate: func(a *publicationAuthority) { a.ReportOnly = true }, status: "unknown", reason: "source_unknown"},
		{name: "stamped_closing", mutate: func(a *publicationAuthority) {
			a.Stamp = mkStamp(SourcePublicationIntent{1, 1, branch, mr, head, false, "closing"})
		}, status: "resolved", intent: "closing", wantStamp: true},
		{name: "stamped_non_closing", mutate: func(a *publicationAuthority) {
			a.Stamp = mkStamp(SourcePublicationIntent{1, 1, branch, mr, head, false, "non_closing"})
		}, status: "resolved", intent: "non_closing", wantStamp: true},
		{name: "stamp_wrong_branch", mutate: func(a *publicationAuthority) {
			a.Stamp = mkStamp(SourcePublicationIntent{1, 1, "agent/issue-2", mr, head, false, "closing"})
		}, status: "unknown", reason: "authority_unknown"},
		{name: "stamp_wrong_mr", mutate: func(a *publicationAuthority) {
			a.Stamp = mkStamp(SourcePublicationIntent{1, 1, branch, 8, head, false, "closing"})
		}, status: "unknown", reason: "authority_unknown"},
		{name: "stamp_version_2", mutate: func(a *publicationAuthority) {
			a.Stamp = mkStamp(SourcePublicationIntent{2, 1, branch, mr, head, false, "closing"})
		}, status: "unknown", reason: "authority_unknown"},
		{name: "stamp_unknown_field", mutate: func(a *publicationAuthority) {
			a.Stamp = json.RawMessage(`{"version":1,"claim_generation":1,"branch":"agent/issue-1","mr_iid":7,"published_head":"` + head + `","reported_non_closing":false,"intent":"closing","extra":true}`)
		}, status: "unknown", reason: "authority_unknown"},
		{name: "stamped_but_failed", mutate: func(a *publicationAuthority) {
			a.Status = "failed"
			a.Stamp = mkStamp(SourcePublicationIntent{1, 1, branch, mr, head, false, "closing"})
		}, status: "unknown", reason: "authority_unknown"},
	}
	for _, defect := range []string{"acceptance", "acceptance_text", "criterion_text", "audit_missing", "audit_value", "findings_null", "findings_nonempty"} {
		bad := malformedPublicationContract(t, raw, defect)
		cases = append(cases,
			unstampedCase{name: "guard_invalid_contract_" + defect, mutate: func(a *publicationAuthority) { a.Contract = bad }, status: "unknown", reason: "authority_unknown"},
			unstampedCase{name: "guard_invalid_contract_restricted_" + defect, mutate: func(a *publicationAuthority) { a.Contract = bad; a.NonClosingReason = &reason }, status: "resolved", intent: "non_closing"},
		)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			source := base()
			if tc.mutate != nil {
				tc.mutate(&source)
			}
			snapshot := publicationSnapshot{ConnectionID: &repo, ProjectID: 1,
				Request: publicationAuthority{ID: requestID, UserID: w.UserID, RepoID: &repo, WorkerID: &w.ID, Kind: "ci_fix", Status: "running", ClaimGeneration: 1, PipelineRef: &branch},
				Nodes:   []publicationNode{{Run: source, Seed: source.ID}}}
			got, fallback := resolvePublicationSnapshot(snapshot, w, requestID, req)
			if got.Status != tc.status || got.Reason != tc.reason || fallback != tc.fallback {
				t.Fatalf("want %s/%s fallback=%t, got %+v fallback=%t", tc.status, tc.reason, tc.fallback, got, fallback)
			}
			if tc.status != "resolved" {
				if got.Binding != nil {
					t.Fatalf("unexpected binding: %+v", got.Binding)
				}
				return
			}
			b := got.Binding
			if b == nil || b.Intent != tc.intent || (b.SourcePublication != nil) != tc.wantStamp {
				t.Fatalf("binding: %+v", b)
			}
			if b.RunID != requestID || b.ClaimGeneration != 1 || b.Branch != branch || b.CandidateHead != candidate || b.MRIID != mr ||
				b.SourceRunID != source.ID || b.IssueIID == nil || *b.IssueIID != iid {
				t.Fatalf("binding identity: %+v", b)
			}
			if tc.fallback || tc.name == "affirmative_restriction_no_stamp" {
				if got := publicationKeys(t, got); !reflect.DeepEqual(got, []string{"binding", "status", "version"}) {
					t.Fatalf("top-level keys: %v", got)
				}
				if got := publicationKeys(t, b); !reflect.DeepEqual(got, []string{"branch", "candidate_head", "claim_generation", "intent", "issue_iid", "mr_iid", "run_id", "source_publication", "source_run_id"}) {
					t.Fatalf("binding keys: %v", got)
				}
				var m map[string]any
				enc, _ := json.Marshal(b)
				if err := json.Unmarshal(enc, &m); err != nil {
					t.Fatal(err)
				}
				if v, ok := m["source_publication"]; !ok || v != nil {
					t.Fatalf("source_publication must serialize null: %v", m)
				}
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
