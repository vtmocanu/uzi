package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forge/forgetest"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type publicationForge struct {
	forgetest.BaseFake
	t                               *testing.T
	projectID, mrIID                int64
	expectedBranch                  string
	branch, head, summaryHead       string
	ancestry                        forge.Ancestry
	summaryErr, headErr, compareErr error
	compareCalls                    int
	afterCompare                    func()
}

func (f *publicationForge) GetMergeRequestSummary(_ context.Context, projectID, mrIID int64) (forge.MergeRequestSummary, error) {
	f.t.Helper()
	if projectID != f.projectID || mrIID != f.mrIID {
		f.t.Fatalf("summary arguments: project=%d MR=%d want project=%d MR=%d", projectID, mrIID, f.projectID, f.mrIID)
	}
	return forge.MergeRequestSummary{SourceBranch: f.branch, HeadSHA: f.summaryHead}, f.summaryErr
}
func (f *publicationForge) BranchHead(_ context.Context, projectID int64, branch string) (string, error) {
	f.t.Helper()
	if projectID != f.projectID || branch != f.expectedBranch {
		f.t.Fatalf("branch arguments: project=%d branch=%q want project=%d branch=%q", projectID, branch, f.projectID, f.expectedBranch)
	}
	return f.head, f.headErr
}
func (f *publicationForge) CompareAncestry(_ context.Context, projectID int64, head, candidate string) (forge.Ancestry, error) {
	f.t.Helper()
	if projectID != f.projectID {
		f.t.Fatalf("comparison project=%d want own project=%d", projectID, f.projectID)
	}
	f.compareCalls++
	if head != f.head || candidate != strings.Repeat("a", 40) {
		return forge.AncestryUnknown, errors.New("wrong repository head comparison")
	}
	if f.afterCompare != nil {
		f.afterCompare()
	}
	return f.ancestry, f.compareErr
}

// Drives the public state-report seam, real completion SQL, forge comparison,
// atomic release, and exact receipt replay. Each subtest has independent rows.
func TestCompletedPublicationLiveDB(t *testing.T) {
	cases := []struct {
		name, kind, reason string
		change             func(*publicationForge)
		drift              string
		reportedBranch     string
		retry              bool
	}{
		{name: "self_improve_equal", kind: "self_improve", reportedBranch: "agent/poisoned"},
		{name: "self_improve_ancestor", kind: "self_improve", reportedBranch: "agent/poisoned", change: func(f *publicationForge) { f.head = strings.Repeat("b", 40); f.summaryHead = f.head }},
		{name: "self_improve_retry", kind: "self_improve", reportedBranch: "agent/poisoned", retry: true},
		{name: "self_improve_missing_mr", kind: "self_improve", reason: "mr_missing", change: func(f *publicationForge) { f.summaryErr = forge.ErrMergeRequestNotFound }},
		{name: "self_improve_missing_branch", kind: "self_improve", reason: "branch_missing", change: func(f *publicationForge) { f.headErr = forge.ErrRefNotFound }},
		{name: "self_improve_tracking_issue_branch", kind: "self_improve", reason: "branch_mismatch", change: func(f *publicationForge) { f.branch = "agent/issue-1" }},
		{name: "self_improve_head_mismatch", kind: "self_improve", reason: "head_mismatch", change: func(f *publicationForge) { f.summaryHead = strings.Repeat("c", 40) }},
		{name: "self_improve_not_ancestor", kind: "self_improve", reason: "not_ancestor", change: func(f *publicationForge) { f.ancestry = forge.AncestryNotAncestor }},
		{name: "self_improve_error", kind: "self_improve", reason: "ancestry_unknown", change: func(f *publicationForge) { f.compareErr = errors.New("forge unavailable") }},
		{name: "self_improve_timeout", kind: "self_improve", reason: "forge_timeout", change: func(f *publicationForge) { f.compareErr = context.DeadlineExceeded }},
		{name: "issue_equal", kind: "issue"},
		{name: "issue_reported_branch_replay", kind: "issue", reportedBranch: "agent/old"},
		{name: "issue_reported_branch_retry", kind: "issue", reportedBranch: "agent/old", retry: true},
		{name: "issue_ancestor", kind: "issue", change: func(f *publicationForge) { f.head = strings.Repeat("b", 40); f.summaryHead = f.head }},
		{name: "rework_ancestor", kind: "mr_rework"},
		{name: "unrelated", reason: "not_ancestor", change: func(f *publicationForge) { f.ancestry = forge.AncestryNotAncestor }},
		{name: "unknown", reason: "ancestry_unknown", change: func(f *publicationForge) { f.ancestry = forge.AncestryUnknown }},
		{name: "forge_error", reason: "ancestry_unknown", change: func(f *publicationForge) { f.compareErr = errors.New("untrusted forge error") }},
		{name: "timeout", reason: "forge_timeout", change: func(f *publicationForge) { f.compareErr = context.DeadlineExceeded }},
		{name: "missing_mr", reason: "mr_missing", change: func(f *publicationForge) { f.summaryErr = forge.ErrMergeRequestNotFound }},
		{name: "missing_branch", reason: "branch_missing", change: func(f *publicationForge) { f.headErr = forge.ErrRefNotFound }},
		{name: "poisoned_prior_worker_branch", reason: "branch_mismatch", change: func(f *publicationForge) { f.branch = "agent/issue-poisoned" }},
		{name: "fork_shaped_mismatch", kind: "mr_rework", reason: "branch_mismatch", change: func(f *publicationForge) { f.branch = "fork:agent/issue-1" }},
		{name: "matching_own_repo_copy", kind: "mr_rework"},
		{name: "head_mismatch", reason: "head_mismatch", change: func(f *publicationForge) { f.summaryHead = strings.Repeat("c", 40) }},
		{name: "malformed_head", reason: "head_mismatch", change: func(f *publicationForge) { f.head = "BAD" }},
		{name: "control_branch", reason: "branch_mismatch", change: func(f *publicationForge) { f.branch = "agent/issue-1\u202e" }},
		{name: "project_drift", reason: "identity_changed", drift: "UPDATE repos SET forge_project_id=forge_project_id+1 WHERE id=$1"},
		{name: "base_url_drift", reason: "identity_changed", drift: "UPDATE forge_connections SET base_url='https://changed.e2e' WHERE id=(SELECT connection_id FROM repos WHERE id=$1)"},
		{name: "mr_drift", reason: "identity_changed", drift: "UPDATE runs SET mr_iid=99 WHERE repo_id=$1 AND status='completed'"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Use a separate repository per case so configuration drift has no sibling effects.
			e := setupInterlockLiveDB(t)
			wid := e.seedWorker(t, []string{capability.RecoveryCompletedPublicationV1})
			run := e.seedLegacyRunningRun(t, wid)
			gen := int64(1)
			mr := int64(7)
			final := strings.Repeat("a", 40)
			branch := "agent/issue-1"
			if tc.kind == "mr_rework" {
				branch = "agent/rework"
				e.exec(t, "UPDATE runs SET kind='mr_rework',pipeline_ref=$2,mr_iid=7,target_run_id=$1 WHERE id=$1", run, branch)
			}
			if tc.kind == "self_improve" {
				branch = selfImproveBranch(run)
				e.exec(t, "UPDATE runs SET kind='self_improve',branch='agent/prior-poison',pipeline_ref='agent/pipeline-poison' WHERE id=$1", run)
			}
			e.exec(t, "UPDATE runs SET claim_generation=1 WHERE id=$1", run)
			hold := uuid.New()
			e.exec(t, `INSERT INTO recovery_custody_holds(id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,live_worker_id,live_run_id,inventory_guarded)
 VALUES($1,$2,$3,$4,1,'open',$5,'ident',$5,$4,true)`, hold, e.userID, e.repoID, run, wid)
			sibling := mhOpenHold(t, e, e.seedLegacyRunningRun(t, wid), 1, wid)
			old := mhOpenHold(t, e, run, 0, wid)
			successor := mhOpenHold(t, e, run, 2, wid)
			f := &publicationForge{t: t, projectID: 1, mrIID: mr, expectedBranch: branch, branch: branch, head: final, summaryHead: final, ancestry: forge.AncestryAncestor}
			if tc.change != nil {
				tc.change(f)
			}
			if tc.drift != "" {
				f.afterCompare = func() { e.exec(t, tc.drift, e.repoID) }
			}
			svc := e.permitService(t)
			svc.SetForges(settleUnitBuilder{f: f})
			w := store.Worker{ID: wid, UserID: e.userID, ProtocolCapabilities: []string{capability.RecoveryCompletedPublicationV1}}
			reportedBranch := branch
			if tc.reportedBranch != "" {
				reportedBranch = tc.reportedBranch
			}
			req := StateRequest{State: "completed", ClaimGeneration: &gen, CompletionFinalHead: &final, Branch: &reportedBranch, MrIID: &mr}
			if tc.retry {
				f.compareErr = errors.New("transient forge refusal")
			}
			result, err := svc.SetStateReportWithReconciliation(e.ctx, w, run, req)
			if err != nil || !result.Applied || result.Run.Status != "completed" {
				t.Fatalf("completion committed: %+v %v", result, err)
			}
			if tc.retry {
				if result.CompletedPublicationReceipt != nil || result.CompletedPublicationReason != "ancestry_unknown" || f.compareCalls != 1 {
					t.Fatalf("transient refusal must retain: %+v calls=%d", result, f.compareCalls)
				}
				retained, err := e.q.GetCompletedPublicationHold(e.ctx, store.GetCompletedPublicationHoldParams{RunID: run, UserID: e.userID, WorkerID: wid, Generation: gen})
				if err != nil || retained.State != "open" {
					t.Fatalf("transient refusal hold: %s %v", retained.State, err)
				}
				f.compareErr = nil
				result, err = svc.SetStateReportWithReconciliation(e.ctx, w, run, req)
				if err != nil || !result.Applied || result.CompletedPublicationReceipt == nil || f.compareCalls != 2 {
					t.Fatalf("exact duplicate retry with API branch: %+v %v calls=%d", result, err, f.compareCalls)
				}
			}
			if result.CompletedPublicationReason != tc.reason {
				t.Fatalf("refusal class=%q want %q", result.CompletedPublicationReason, tc.reason)
			}
			stored, err := e.q.GetCompletedPublicationHold(e.ctx, store.GetCompletedPublicationHoldParams{RunID: run, UserID: e.userID, WorkerID: wid, Generation: gen})
			if err != nil {
				t.Fatal(err)
			}
			if tc.reason != "" {
				if stored.State != "open" || result.CompletedPublicationReceipt != nil {
					t.Fatalf("refusal must retain exact hold: %+v", result)
				}
			} else {
				wantCalls := 1
				if tc.retry {
					wantCalls = 2
				}
				if stored.State != "released" || result.CompletedPublicationReceipt == nil || f.compareCalls != wantCalls {
					t.Fatalf("proof must release exact hold after comparison: %+v calls=%d", result, f.compareCalls)
				}
				if result.CompletedPublicationReceipt.Branch != branch {
					t.Fatalf("receipt branch=%q want frozen API branch=%q", result.CompletedPublicationReceipt.Branch, branch)
				}
				want, _ := json.Marshal(result.CompletedPublicationReceipt)
				var receipt apitypes.CompletedPublicationReceipt
				if err := json.Unmarshal(stored.CompletedPublicationReceipt, &receipt); err != nil {
					t.Fatal(err)
				}
				got, _ := json.Marshal(receipt)
				if string(got) != string(want) {
					t.Fatal("stored receipt differs from ACK")
				}
				svc.SetForges(nil)
				// The recorded receipt remains replayable after mutable generation/config drift.
				e.exec(t, "UPDATE runs SET claim_generation=2,claim_released_at=now() WHERE id=$1", run)
				replay, err := svc.SetStateReportWithReconciliation(e.ctx, w, run, req)
				if err != nil || !replay.Applied || replay.CompletedPublicationReceipt == nil {
					t.Fatalf("lost ACK replay unavailable forge: %+v %v", replay, err)
				}
				got, _ = json.Marshal(replay.CompletedPublicationReceipt)
				if string(got) != string(want) {
					t.Fatal("replay must return immutable receipt")
				}
				n, err := e.q.RecordCompletedPublicationRefusal(e.ctx, store.RecordCompletedPublicationRefusalParams{HoldID: hold, RunID: run, UserID: e.userID, WorkerID: wid, Generation: gen, Identity: stored.CompletionIdentity, Reason: pgconv.Text("ancestry_unknown")})
				if err != nil || n != 0 {
					t.Fatalf("delayed refusal overwrote receipt: %d %v", n, err)
				}
			}
			for _, id := range []uuid.UUID{old, successor, sibling} {
				if state, _ := e.fpHoldEvidence(t, id); state != "open" {
					t.Fatal("older/successor hold released")
				}
			}
		})
	}
}
