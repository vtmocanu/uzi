package workersvc

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forge/forgetest"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type contextPublicationForge struct {
	forgetest.BaseFake
	summary forge.MergeRequestSummary
	barrier func()
}

func (f *contextPublicationForge) GetMergeRequestSummary(_ context.Context, _, _ int64) (forge.MergeRequestSummary, error) {
	if f.barrier != nil {
		f.barrier()
	}
	return f.summary, nil
}

func TestPublicationContextResolverLiveDB(t *testing.T) {
	for _, kind := range []string{"mr_rework", "ci_fix"} {
		for _, scenario := range []string{"closing", "non_closing", "missing", "failed", "failed_restricted", "prompt", "self_improve", "wrong_branch", "foreign_source", "foreign_repo", "cycle", "16_hops", "17_hops", "duplicate_roots", "conflicting_roots", "source_drift", "release", "new_ci_root"} {
			t.Run(kind+"/"+scenario, func(t *testing.T) {
				f := seedSourcePublication(t, false)
				if scenario == "non_closing" {
					f.req.SourcePublication.NonClosing = true
				}
				if scenario != "missing" && scenario != "failed" && scenario != "failed_restricted" && scenario != "prompt" && scenario != "self_improve" && scenario != "cycle" {
					result, err := f.svc.SetStateReportWithReconciliation(f.e.ctx, f.w, f.id, f.req)
					if err != nil || !result.Applied {
						t.Fatalf("source completion: %+v %v", result, err)
					}
				} else {
					f.e.exec(t, "UPDATE runs SET status='failed',branch=$2,mr_iid=7 WHERE id=$1", f.id, *f.req.Branch)
				}
				if scenario == "failed_restricted" {
					f.e.exec(t, "UPDATE runs SET completion_non_closing_reason='partial' WHERE id=$1", f.id)
				}
				switch scenario {
				case "prompt":
					f.e.exec(t, "UPDATE runs SET kind=$2,issue_iid=NULL WHERE id=$1", f.id, scenario)
				case "self_improve":
					f.e.exec(t, "UPDATE runs SET kind=$2 WHERE id=$1", f.id, scenario)
				}
				branch := *f.req.Branch
				seedChild := func(target uuid.UUID, branch string) uuid.UUID {
					id := uuid.New()
					f.e.exec(t, `INSERT INTO runs(id,user_id,repo_id,kind,status,worker_id,claim_generation,pipeline_ref,mr_iid,target_run_id,issue_title,issue_description)
 VALUES($1,$2,$3,'mr_rework','completed',$4,1,$5,7,$6,'rework','')`, id, f.w.UserID, f.e.repoID, f.w.ID, branch, target)
					return id
				}
				target := f.id
				if scenario == "16_hops" || scenario == "17_hops" {
					count := 16
					if scenario == "16_hops" {
						count = 15
					}
					for i := 0; i < count; i++ {
						target = seedChild(target, branch)
					}
				}
				requestID := seedChild(target, branch)
				f.e.exec(t, "UPDATE runs SET status='running' WHERE id=$1", requestID)
				if kind == "ci_fix" {
					f.e.exec(t, "UPDATE runs SET kind='ci_fix',pipeline_id=123,target_run_id=NULL WHERE id=$1", requestID)
				}
				switch scenario {
				case "wrong_branch":
					f.e.exec(t, "UPDATE runs SET branch='other' WHERE id=$1", f.id)
				case "foreign_source":
					other := uuid.New()
					f.e.exec(t, "INSERT INTO users(id,email,password_hash) VALUES($1,$2,'x')", other, other.String()+"@e2e")
					f.e.exec(t, "UPDATE runs SET user_id=$2 WHERE id=$1", f.id, other)
				case "foreign_repo":
					other := uuid.New()
					f.e.exec(t, `INSERT INTO repos(id,connection_id,forge_project_id,path_with_namespace,web_url,default_branch) SELECT $1,connection_id,forge_project_id+1,path_with_namespace||'-other',web_url||'-other',default_branch FROM repos WHERE id=$2`, other, f.e.repoID)
					f.e.exec(t, "UPDATE runs SET repo_id=$2 WHERE id=$1", f.id, other)
				case "cycle":
					f.e.exec(t, "UPDATE runs SET kind='mr_rework',pipeline_ref=$2,target_run_id=$1 WHERE id=$1", f.id, branch)
				case "duplicate_roots":
					seedChild(f.id, branch)
				case "conflicting_roots":
					other := f.e.seedLegacyRunningRun(t, f.w.ID)
					f.e.exec(t, "UPDATE runs SET branch=$2,mr_iid=7 WHERE id=$1", other, branch)
				}
				proof := &contextPublicationForge{summary: forge.MergeRequestSummary{IID: 7, State: forge.MRStateOpened, SourceBranch: branch, HeadSHA: strings.Repeat("b", 40)}}
				switch scenario {
				case "source_drift":
					proof.barrier = func() { f.e.exec(t, "UPDATE runs SET completion_non_closing_reason='partial' WHERE id=$1", f.id) }
				case "release":
					proof.barrier = func() { f.e.exec(t, "UPDATE runs SET claim_released_at=now() WHERE id=$1", requestID) }
				case "new_ci_root":
					proof.barrier = func() {
						other := f.e.seedLegacyRunningRun(t, f.w.ID)
						f.e.exec(t, "UPDATE runs SET branch=$2,mr_iid=7 WHERE id=$1", other, branch)
					}
				}
				f.svc.SetForges(settleUnitBuilder{f: proof})
				// The outgoing head is NEW and must differ from the forge's current head.
				req := PublicationContextRequest{ClaimGeneration: 1, Branch: branch, CandidateHead: strings.Repeat("c", 40)}
				result := f.svc.ResolvePublicationContext(f.e.ctx, f.w, requestID, req)
				want := "unknown"
				switch scenario {
				case "closing", "non_closing", "failed_restricted", "duplicate_roots", "16_hops":
					want = "resolved"
				case "prompt", "self_improve":
					want = "not_applicable"
				case "conflicting_roots", "new_ci_root":
					if kind == "mr_rework" {
						want = "resolved"
					}
				}
				if result.Status != want {
					t.Fatalf("status want %s: %+v", want, result)
				}
				if want == "not_applicable" && (result.Binding == nil || result.Binding.IssueIID != nil) {
					t.Fatalf("not_applicable must carry no source issue: %+v", result.Binding)
				}
				if want == "resolved" {
					intent := "closing"
					if scenario == "non_closing" || scenario == "failed_restricted" {
						intent = "non_closing"
					}
					if result.Binding == nil || result.Binding.Intent != intent || result.Binding.SourceRunID != f.id ||
						result.Binding.CandidateHead != req.CandidateHead || result.Binding.MRIID != 7 {
						t.Fatalf("binding: %+v", result.Binding)
					}
				}
				if scenario == "source_drift" && result.Reason != "authority_changed" {
					t.Fatalf("source barrier: %+v", result)
				}
				if scenario == "release" && result.Reason != "claim_changed" {
					t.Fatalf("release barrier: %+v", result)
				}
			})
		}
	}
}

func TestSourcePublicationRestrictionsLiveDB(t *testing.T) {
	for _, permit := range []bool{false, true} {
		for _, restriction := range []string{"reported", "persisted", "unused_ceiling", "persisted_cap", "deferral", "bad_deferral", "bad_version"} {
			t.Run(fmt.Sprintf("permit=%t/%s", permit, restriction), func(t *testing.T) {
				f := seedSourcePublication(t, permit)
				switch restriction {
				case "reported":
					f.req.SourcePublication.NonClosing = true
				case "persisted":
					f.e.exec(t, "UPDATE runs SET completion_non_closing_reason='partial' WHERE id=$1", f.id)
				case "unused_ceiling":
					f.e.exec(t, "UPDATE runs SET scope_ceiling=100 WHERE id=$1", f.id)
				case "persisted_cap":
					// Existing applied evidence must override a false report even for legacy sources.
					f.e.exec(t, "UPDATE runs SET stop_kind='scope_capped' WHERE id=$1", f.id)
				case "bad_version":
					f.e.exec(t, "UPDATE runs SET completion_contract_version=2 WHERE id=$1", f.id)
				case "deferral", "bad_deferral":
					frozen := frozenJSON(t, "m1", "m2")
					c, err := buildCompletionContract(frozen)
					if err != nil {
						t.Fatal(err)
					}
					var contract completionContract
					if err := json.Unmarshal(c, &contract); err != nil {
						t.Fatal(err)
					}
					contract.Scope = &scopeBlock{In: []string{"m1"}, Out: []deferredEntry{{MilestoneID: "m2", Reason: "owner deferred", Revision: 1}}}
					if restriction == "bad_deferral" {
						contract.Scope.Out[0].MilestoneID = "unknown"
					}
					c, err = json.Marshal(contract)
					if err != nil {
						t.Fatal(err)
					}
					f.e.exec(t, "UPDATE runs SET completion_contract_version=1,contract_revision=1,milestones_frozen=$2,milestones_completed=$3,completion_contract=$4 WHERE id=$1", f.id, frozen, idsJSON(t, "m1"), c)
					if !permit {
						// This scenario is enrolled now: exercise its permit gate too.
						head := f.req.SourcePublication.PublishedHead
						f.req.Head = &head
						p, err := f.svc.RequestCompletionPermit(f.e.ctx, f.w, f.id, CompletionPermitRequest{ContractRevision: 1, Branch: *f.req.Branch, Head: head})
						if restriction == "deferral" && (err != nil || !p.Granted) {
							t.Fatalf("permit: %+v %v", p, err)
						}
					}
				}
				result, err := f.svc.SetStateReportWithReconciliation(f.e.ctx, f.w, f.id, f.req)
				_, b, _ := f.stored(t)
				if restriction == "bad_version" || restriction == "bad_deferral" {
					if err == nil && result.Applied || len(b) > 0 {
						t.Fatalf("invalid authority: %+v %v %s", result, err, b)
					}
					return
				}
				if err != nil || !result.Applied {
					t.Fatalf("completion: %+v %v", result, err)
				}
				var stamp SourcePublicationIntent
				if err := json.Unmarshal(b, &stamp); err != nil {
					t.Fatal(err)
				}
				want := "non_closing"
				if restriction == "unused_ceiling" {
					want = "closing"
				}
				if stamp.Intent != want {
					t.Fatalf("restriction %s: %s", restriction, b)
				}
				assertSourcePublicationRefresh(t, f, want)
			})
		}
	}
}

func assertSourcePublicationRefresh(t *testing.T, f sourcePublicationFixture, want string) {
	t.Helper()
	branch := *f.req.Branch
	proof := &contextPublicationForge{summary: forge.MergeRequestSummary{IID: 7, State: forge.MRStateOpened, SourceBranch: branch, HeadSHA: strings.Repeat("b", 40)}}
	f.svc.SetForges(settleUnitBuilder{f: proof})
	for _, kind := range []string{"mr_rework", "ci_fix"} {
		id := uuid.New()
		f.e.exec(t, `INSERT INTO runs(id,user_id,repo_id,kind,status,worker_id,claim_generation,pipeline_ref,mr_iid,target_run_id,issue_title,issue_description)
 VALUES($1,$2,$3,'mr_rework','running',$4,1,$5,7,$6,'refresh','')`, id, f.w.UserID, f.e.repoID, f.w.ID, branch, f.id)
		if kind == "ci_fix" {
			f.e.exec(t, "UPDATE runs SET kind='ci_fix',pipeline_id=123,target_run_id=NULL WHERE id=$1", id)
		}
		got := f.svc.ResolvePublicationContext(f.e.ctx, f.w, id, PublicationContextRequest{1, branch, strings.Repeat("c", 40)})
		if got.Status != "resolved" || got.Binding == nil || got.Binding.Intent != want || got.Binding.SourceRunID != f.id {
			t.Fatalf("%s refresh lost restriction: %+v", kind, got)
		}
		f.e.exec(t, "UPDATE runs SET status='completed' WHERE id=$1", id)
	}
}

func TestSourcePublicationAppliedRestrictionLiveDB(t *testing.T) {
	for _, permit := range []bool{false, true} {
		t.Run(fmt.Sprint("actual_cap/permit=", permit), func(t *testing.T) {
			f := seedSourcePublication(t, permit)
			frozen := frozenJSON(t, "m1", "m2")
			if permit {
				c, err := buildCompletionContract(frozen)
				if err != nil {
					t.Fatal(err)
				}
				f.e.exec(t, "UPDATE runs SET completion_contract=$2,milestones_frozen=$3,milestones_completed=$4 WHERE id=$1", f.id, c, frozen, idsJSON(t, "m1"))
			} else {
				f.e.exec(t, "UPDATE runs SET milestones_frozen=$2,milestones_completed=$3 WHERE id=$1", f.id, frozen, idsJSON(t, "m1"))
			}
			if _, err := f.svc.SubmitInput(f.e.ctx, f.w.UserID, f.id, "scope", "1", nil); err != nil {
				t.Fatal(err)
			}
			capped := true
			f.req.ScopeCapped = &capped
			if permit {
				f.e.exec(t, "DELETE FROM run_completion_permits WHERE run_id=$1", f.id)
				p, err := f.svc.RequestCompletionPermit(f.e.ctx, f.w, f.id, CompletionPermitRequest{ContractRevision: 1, Branch: *f.req.Branch, Head: *f.req.Head, ScopeCapped: &capped})
				if err != nil || !p.Granted {
					t.Fatalf("capped permit: %+v %v", p, err)
				}
			}
			before, err := f.e.q.GetRunByID(f.e.ctx, f.id)
			if err != nil || before.StopKind.Valid {
				t.Fatalf("cap already applied: %+v %v", before.StopKind, err)
			}
			result, err := f.svc.SetStateReportWithReconciliation(f.e.ctx, f.w, f.id, f.req)
			_, raw, _ := f.stored(t)
			var stamp SourcePublicationIntent
			if err != nil || !result.Applied || result.Run.StopKind.String != "scope_capped" || json.Unmarshal(raw, &stamp) != nil || stamp.Intent != "non_closing" || stamp.ReportedNonClosing {
				t.Fatalf("actual cap stamp: %+v %v %s", result, err, raw)
			}
			assertSourcePublicationRefresh(t, f, "non_closing")
		})
	}
	t.Run("semantic_maintainer_pending", func(t *testing.T) {
		// Semantic completion is enrolled and uses the permit path.
		f := seedSourcePublication(t, true)
		pending := semanticCompletionFixture(t, false)
		f.e.exec(t, "UPDATE runs SET completion_contract=$2,milestones_frozen=$3,milestones_completed='[]' WHERE id=$1", f.id, pending.CompletionContract, pending.MilestonesFrozen)
		f.e.exec(t, "DELETE FROM run_completion_permits WHERE run_id=$1", f.id)
		p, err := f.svc.RequestCompletionPermit(f.e.ctx, f.w, f.id, CompletionPermitRequest{ContractRevision: 1, Branch: *f.req.Branch, Head: *f.req.Head})
		if err != nil || !p.Granted || p.Permit.CompletionPending == nil {
			t.Fatalf("pending permit: %+v %v", p, err)
		}
		result, err := f.svc.SetStateReportWithReconciliation(f.e.ctx, f.w, f.id, f.req)
		_, raw, _ := f.stored(t)
		var stamp SourcePublicationIntent
		if err != nil || !result.Applied || json.Unmarshal(raw, &stamp) != nil || stamp.Intent != "non_closing" || stamp.ReportedNonClosing {
			t.Fatalf("pending stamp: %+v %v %s", result, err, raw)
		}
		assertSourcePublicationRefresh(t, f, "non_closing")
	})
}

func TestPublicationContextManyDescendantsLiveDB(t *testing.T) {
	for defectIndex, defect := range []string{"same_root", "second_root", "cycle", "missing", "conflicting_mr"} {
		t.Run(defect, func(t *testing.T) {
			f := seedSourcePublication(t, false)
			result, err := f.svc.SetStateReportWithReconciliation(f.e.ctx, f.w, f.id, f.req)
			if err != nil || !result.Applied {
				t.Fatalf("source: %+v %v", result, err)
			}
			branch := *f.req.Branch
			for i := 1; i <= 80; i++ {
				id := uuid.MustParse(fmt.Sprintf("%08x-0000-4000-8000-%012d", defectIndex+1, i))
				f.e.exec(t, `INSERT INTO runs(id,user_id,repo_id,kind,status,worker_id,claim_generation,pipeline_ref,mr_iid,target_run_id,issue_title,issue_description)
 VALUES($1,$2,$3,'mr_rework','completed',$4,1,$5,7,$6,'descendant','')`, id, f.w.UserID, f.e.repoID, f.w.ID, branch, f.id)
			}
			late := uuid.MustParse(fmt.Sprintf("fffffff%x-ffff-4fff-8fff-ffffffffffff", defectIndex))
			switch defect {
			case "second_root":
				f.e.exec(t, `INSERT INTO runs(id,user_id,repo_id,kind,status,branch,mr_iid,issue_iid,issue_title,issue_description)
 VALUES($1,$2,$3,'issue','failed',$4,7,99999,'second source','')`, late, f.w.UserID, f.e.repoID, branch)
			case "cycle", "missing", "conflicting_mr":
				f.e.exec(t, `INSERT INTO runs(id,user_id,repo_id,kind,status,pipeline_ref,mr_iid,target_run_id,issue_title,issue_description)
 VALUES($1,$2,$3,'mr_rework','completed',$4,7,$5,'bad descendant','')`, late, f.w.UserID, f.e.repoID, branch, f.id)
				switch defect {
				case "cycle":
					f.e.exec(t, "UPDATE runs SET target_run_id=id WHERE id=$1", late)
				case "missing":
					missing := uuid.New()
					f.e.exec(t, `INSERT INTO runs(id,user_id,repo_id,kind,status,branch,issue_title,issue_description) VALUES($1,$2,$3,'prompt','failed',$4,'unpublished root','')`, missing, f.w.UserID, f.e.repoID, branch)
					f.e.exec(t, "UPDATE runs SET target_run_id=$2 WHERE id=$1", late, missing)
				case "conflicting_mr":
					f.e.exec(t, "UPDATE runs SET mr_iid=8 WHERE id=$1", late)
				}
			}
			requestID := uuid.New()
			f.e.exec(t, `INSERT INTO runs(id,user_id,repo_id,kind,status,worker_id,claim_generation,pipeline_ref,mr_iid,pipeline_id,issue_title,issue_description)
 VALUES($1,$2,$3,'ci_fix','running',$4,1,$5,7,123,'refresh','')`, requestID, f.w.UserID, f.e.repoID, f.w.ID, branch)
			raw, err := f.e.q.PublicationContextSnapshot(f.e.ctx, store.PublicationContextSnapshotParams{RunID: requestID, UserID: f.w.UserID})
			if err != nil {
				t.Fatal(err)
			}
			var snapshot publicationSnapshot
			if err := json.Unmarshal(raw, &snapshot); err != nil {
				t.Fatal(err)
			}
			// Descendant count must not multiply the authority JSON payload.
			wantNodes := 1
			if defect == "second_root" || defect == "missing" {
				wantNodes = 2
			}
			if len(snapshot.Nodes) != wantNodes {
				t.Fatalf("root authority payload=%d want=%d", len(snapshot.Nodes), wantNodes)
			}
			f.svc.SetForges(settleUnitBuilder{f: &contextPublicationForge{summary: forge.MergeRequestSummary{IID: 7, State: forge.MRStateOpened, SourceBranch: branch, HeadSHA: strings.Repeat("b", 40)}}})
			got := f.svc.ResolvePublicationContext(f.e.ctx, f.w, requestID, PublicationContextRequest{1, branch, strings.Repeat("c", 40)})
			if defect == "same_root" {
				if got.Status != "resolved" || got.Binding.SourceRunID != f.id || got.Binding.Intent != "closing" {
					t.Fatalf("shared root: %+v", got)
				}
			} else if got.Status != "unknown" {
				t.Fatalf("late bad association ignored: %+v", got)
			}
		})
	}
}
