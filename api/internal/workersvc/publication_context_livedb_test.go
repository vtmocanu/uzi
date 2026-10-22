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
		for _, scenario := range []string{"closing", "non_closing", "missing", "failed", "failed_restricted", "prompt", "self_improve", "wrong_branch", "foreign_source", "cycle", "16_hops", "17_hops", "duplicate_roots", "conflicting_roots", "source_drift", "release", "new_ci_root"} {
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
				if scenario == "prompt" {
					f.e.exec(t, "UPDATE runs SET kind=$2,issue_iid=NULL WHERE id=$1", f.id, scenario)
				} else if scenario == "self_improve" {
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
		for _, restriction := range []string{"reported", "persisted", "unused_ceiling", "applied_cap", "deferral", "bad_deferral", "bad_version"} {
			t.Run(fmt.Sprintf("permit=%t/%s", permit, restriction), func(t *testing.T) {
				f := seedSourcePublication(t, permit)
				switch restriction {
				case "reported":
					f.req.SourcePublication.NonClosing = true
				case "persisted":
					f.e.exec(t, "UPDATE runs SET completion_non_closing_reason='partial' WHERE id=$1", f.id)
				case "unused_ceiling":
					f.e.exec(t, "UPDATE runs SET scope_ceiling=100 WHERE id=$1", f.id)
				case "applied_cap":
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
			})
		}
	}
}
