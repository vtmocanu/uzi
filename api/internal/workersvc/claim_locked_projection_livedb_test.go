package workersvc

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// This entire file overlays preunit 1782 without new production helpers or new
// fake fields. Every change commits through afterAssembly before the production
// owned-claim lock. Seeded source/contracts/audit rows are projection fixtures,
// not evidence of source production, adoption, reconciliation or Stop authority.
func TestClaimLockedProjectionRaceLiveDB(t *testing.T) {
	e := setupCodexLiveDB(t)
	for _, mode := range []string{"group", "clearing", "historical", "autopilot", "seeded", "unknown_source", "empty_candidate", "empty_frozen", "open_question", "sessionless", "commit", "final_size"} {
		t.Run(mode, func(t *testing.T) {
			f := newCodexClaimFix(t, e, false)
			w := f.claimant(t, true)
			t.Cleanup(func() { dropCustodyHolds(t, e, f.userID) })
			// The assembly starts with a different, approved plan and frozen list.
			e.exec(`UPDATE runs SET plan_md='assembly plan',plan_source='seeded',auto_approve=true,
 session_id='assembly-session',milestones_frozen='[{"id":"old","title":"Assembly work"}]'
 WHERE id=$1`, f.runID)
			hooked := false
			var presentation uuid.UUID
			var planAt time.Time
			var planSeq int64
			f.svc.claimHooks = &claimTestHooks{afterAssembly: func(_ context.Context, assembled store.Run, p *ClaimPayload, err error) {
				if err != nil || p == nil || assembled.ID != f.runID {
					t.Fatalf("assembly: %v", err)
				}
				if p.PlanMd == nil || *p.PlanMd != "assembly plan" || !p.PlanApproved {
					t.Fatal("fixture did not assemble old approved plan")
				}
				hooked = true
				// Plant obsolete values too, so clearing is observable even for fields the
				// original fixture did not use.
				p.ResumeGateRevision = 99
				oldID := uuid.New()
				p.ResumeGatePresentationID = &oldID
				p.ResumeGatePresented = &GatePresentedRequirements{RequiredTools: []string{"obsolete"}}
				p.ResumePlanSeq = 99
				p.ResumePlanAt = new(time.Time)
				p.OpenQuestionID = new(string)
				p.AgentSelection = &AgentSelection{Source: "own", Exclusions: []string{"obsolete"}}
				p.Config.CompletionContractVersion = new(int)
				p.Config.ContractRevision = new(int)
				p.Config.CompletionScope = &CompletionScopeConfig{}
				p.Config.NonClosing = &CompletionNonClosing{Reason: "obsolete"}
				p.PlanCrossCheckGateReason = new(string)
				// Main final row group, deliberately different from assembly.
				e.exec(`UPDATE runs SET plan_md='locked candidate',plan_source='agent',auto_approve=false,
 session_id='locked-session',open_question_id=NULL,
 milestones_candidate='[{"id":"candidate","title":"Candidate work"}]',
 milestones_frozen='[{"id":"frozen","title":"Frozen work"}]',
 agent_source='own',agent_exclusions='["excluded"]',
 gate_revision=4,gate_presentation_id=$2,required_tools='{live-tool}',
 completion_contract_version=1,contract_revision=2,
 completion_contract='{"profile":"structural","revision":2,"criteria":[{"id":"frozen.c1","milestone_id":"frozen","text":"Frozen work","audit":null,"finding_ids":[]}],"scope":{"in":[],"out":[{"milestone_id":"frozen","reason":"owner deferred","revision":2}]}}',
 completion_non_closing_reason='partial',budget_wall_seconds=100,
 budget_extension_seconds=20,budget_finalize_seconds=30,budget_max_iterations=9,
 plan_cross_check_gate_reason='revise'
 WHERE id=$1`, f.runID, uuid.New())
				row := mustRun(t, e, f.runID)
				presentation = uuid.UUID(row.GatePresentationID.Bytes)
				snap := gatePayloadFromRow(row)
				snap.RequiredTools = []string{"snapshot-tool"}
				raw, digest, derr := snap.digest()
				if derr != nil {
					t.Fatal(derr)
				}
				e.exec("UPDATE runs SET gate_presented_payload=$2,gate_payload_digest=$3 WHERE id=$1", f.runID, raw, digest)
				// A later unmatched plan must not move the persisted plan's presentation time.
				planAt = time.Date(2026, 10, 27, 1, 2, 3, 0, time.UTC)
				planSeq = int64(mustRun(t, e, f.runID).LastSeq) + 2
				e.exec(`INSERT INTO run_messages(run_id,seq,kind,payload,created_at)
 VALUES($1,$2,'plan','{"plan_md":"locked candidate"}',$3),
 ($1,$4,'plan','{"plan_md":"unpersisted plan"}',$5)`,
					f.runID, planSeq-1, planAt, planSeq, planAt.Add(time.Minute))
				switch mode {
				case "group":
					// Insert an audit predecessor after assembly; the final reader must suppress it.
					var input int64
					if err := e.pool.QueryRow(e.ctx, "INSERT INTO run_user_inputs(run_id,kind,body) VALUES($1,'revise_plan','fixture') RETURNING id", f.runID).Scan(&input); err != nil {
						t.Fatal(err)
					}
					e.exec(`INSERT INTO run_milestone_reconciliations
 (id,run_id,owner_id,gate_revision,presentation_id,gate_digest,reset_state_digest,worker_id,claim_generation,reason,consent,before_state,after_state,applied_input_id,stop_cutoff)
 VALUES($1,$2,$3,4,$4,$5,'fixture',$6,$7,'fixture','{}','{"session_id":"locked-session"}','{}',$8,0)`,
						uuid.New(), f.runID, f.userID, presentation, digest, w.ID, assembled.ClaimGeneration, input)
				case "clearing":
					e.exec(`UPDATE runs SET plan_md=NULL,plan_source='agent',session_id=NULL,
 milestones_candidate=NULL,milestones_frozen=NULL,agent_source=NULL,agent_exclusions=NULL,
 gate_revision=0,gate_presentation_id=NULL,gate_presented_payload=NULL,gate_payload_digest=NULL,
 completion_contract_version=NULL,contract_revision=NULL,completion_contract=NULL,
 completion_non_closing_reason=NULL,plan_cross_check_gate_reason=NULL WHERE id=$1`, f.runID)
				case "historical":
					// Ordinary applied approval is intentionally from an older gate.
					e.exec(`INSERT INTO run_user_inputs(run_id,kind,gate_binding,gate_revision,consumed_at,applied_at)
 VALUES($1,'approve_plan','bound',1,now(),now())`, f.runID)
				case "autopilot":
					e.exec("UPDATE runs SET auto_approve=true WHERE id=$1", f.runID)
				case "seeded":
					e.exec("UPDATE runs SET plan_source='seeded' WHERE id=$1", f.runID)
				case "unknown_source":
					e.exec("UPDATE runs SET auto_approve=true,policy_refusal_count=1,policy_revision_source=NULL WHERE id=$1", f.runID)
				case "empty_candidate":
					e.exec("UPDATE runs SET milestones_candidate='[]' WHERE id=$1", f.runID)
				case "empty_frozen":
					e.exec("UPDATE runs SET milestones_frozen='[]',completion_contract=NULL,contract_revision=NULL WHERE id=$1", f.runID)
				case "open_question":
					e.exec("UPDATE runs SET open_question_id='locked-question' WHERE id=$1", f.runID)
				case "sessionless":
					e.exec("UPDATE runs SET session_id=NULL WHERE id=$1", f.runID)
				case "commit":
					f.svc.SetTxBeginner(&revisionGateBeginner{TxBeginner: e.pool, point: "commit"})
				case "final_size":
					encoded, merr := json.Marshal(p)
					if merr != nil {
						t.Fatal(merr)
					}
					f.svc.claimSizeLimit = len(encoded) + 1024
					e.exec("UPDATE runs SET plan_md=$2 WHERE id=$1", f.runID, strings.Repeat("locked", 10000))
				}
			}}
			p, err := f.svc.Claim(e.ctx, w, nil)
			if !hooked {
				t.Fatalf("hook not reached: %v", err)
			}
			if mode == "commit" {
				if err == nil || p != nil {
					t.Fatalf("commit failure delivered: %v", err)
				}
				return
			}
			if mode == "final_size" {
				row := mustRun(t, e, f.runID)
				if err != nil || p != nil || row.Status != "failed" || row.FailOrigin.String != "provisioning_failed" {
					t.Fatalf("final size: payload=%v status=%s err=%v", p, row.Status, err)
				}
				return
			}
			if err != nil || p == nil {
				t.Fatalf("Claim: %v", err)
			}
			if mode == "clearing" {
				if p.PlanMd != nil || p.PlanApproved || p.AutoApprove || p.SessionID != nil || p.ResumePhase != "" ||
					p.ResumeGateRevision != 0 || p.ResumeGatePresentationID != nil || p.ResumeGatePresented != nil ||
					p.ResumePlanSeq != 0 || p.ResumePlanAt != nil || p.OpenQuestionID != nil || p.AgentSelection != nil ||
					p.Milestones != nil || p.ExecutionMilestones != nil || p.PlanCrossCheckGateReason != nil ||
					p.Config.CompletionContractVersion != nil || p.Config.ContractRevision != nil ||
					p.Config.CompletionScope != nil || p.Config.CompletionPending != nil || p.Config.NonClosing != nil {
					t.Fatalf("stale assembly fields survived clearing: %+v", p)
				}
				return
			}
			approved := mode == "historical" || mode == "autopilot" || mode == "seeded"
			if p.PlanApproved != approved || p.PlanMd == nil || *p.PlanMd != "locked candidate" || p.AutoApprove != (mode == "autopilot" || mode == "unknown_source") {
				t.Fatalf("locked approval/plan not projected: approved=%v auto=%v plan=%v", p.PlanApproved, p.AutoApprove, p.PlanMd)
			}
			source := "agent"
			if mode == "seeded" {
				source = "seeded"
			}
			if p.PlanSource != source {
				t.Fatalf("source=%q", p.PlanSource)
			}
			phase := "awaiting_approval"
			if approved {
				phase = "implementing"
			}
			if mode == "open_question" {
				phase = "awaiting_input"
			}
			if mode == "sessionless" {
				phase = ""
			}
			if p.ResumePhase != phase {
				t.Fatalf("phase=%q want=%q", p.ResumePhase, phase)
			}
			if phase == "awaiting_approval" {
				if p.ResumeGateRevision != 4 || p.ResumeGatePresentationID == nil || *p.ResumeGatePresentationID != presentation ||
					p.ResumeGatePresented == nil || !reflect.DeepEqual(p.ResumeGatePresented.RequiredTools, []string{"snapshot-tool"}) || p.ResumePlanSeq != planSeq {
					t.Fatalf("locked gate snapshot/seq lost: revision=%d seq=%d requirements=%+v", p.ResumeGateRevision, p.ResumePlanSeq, p.ResumeGatePresented)
				}
			} else if p.ResumeGateRevision != 0 || p.ResumeGatePresentationID != nil || p.ResumeGatePresented != nil || p.ResumePlanSeq != 0 {
				t.Fatal("obsolete gate fields survived phase change")
			}
			if !approved {
				if p.ResumePlanAt == nil || !p.ResumePlanAt.Equal(planAt) {
					t.Fatalf("wrong persisted plan time: %v", p.ResumePlanAt)
				}
			} else if p.ResumePlanAt != nil {
				t.Fatal("approved plan retained old time")
			}
			if mode == "group" || mode == "sessionless" {
				if p.SessionID != nil {
					t.Fatal("obsolete session escaped")
				}
			} else if p.SessionID == nil || *p.SessionID != "locked-session" {
				t.Fatal("locked session missing")
			}
			if mode == "open_question" {
				if p.OpenQuestionID == nil || *p.OpenQuestionID != "locked-question" {
					t.Fatal("question missing")
				}
			} else if p.OpenQuestionID != nil {
				t.Fatal("stale question survived")
			}
			if p.ExecutionMilestones == nil {
				t.Fatal("frozen execution pointer missing")
			}
			if mode == "empty_frozen" {
				if *p.ExecutionMilestones == nil || len(*p.ExecutionMilestones) != 0 {
					t.Fatal("[] frozen lost")
				}
			} else if !reflect.DeepEqual(*p.ExecutionMilestones, []Milestone{{ID: "frozen", Title: "Frozen work"}}) {
				t.Fatal("execution did not use locked frozen list")
			}
			if mode == "empty_candidate" {
				if p.Milestones == nil || len(p.Milestones) != 0 {
					t.Fatal("[] candidate lost")
				}
			} else {
				want := "frozen"
				if phase == "awaiting_approval" {
					want = "candidate"
				}
				if mode != "empty_frozen" && (len(p.Milestones) != 1 || p.Milestones[0].ID != want) {
					t.Fatalf("presentation milestones=%+v", p.Milestones)
				}
			}
			if p.AgentSelection == nil || p.AgentSelection.Source != "own" || !reflect.DeepEqual(p.AgentSelection.Exclusions, []string{"excluded"}) ||
				p.Config.RunTimeoutSeconds != 150 || p.Config.MaxIterations != 9 ||
				p.PlanCrossCheckGateReason == nil || *p.PlanCrossCheckGateReason != "revise" {
				t.Fatal("locked selection/budget/gate reason lost")
			}
			if mode != "empty_frozen" && (p.Config.ContractRevision == nil || *p.Config.ContractRevision != 2 ||
				p.Config.CompletionContractVersion == nil || *p.Config.CompletionContractVersion != 1 ||
				p.Config.CompletionScope == nil || len(p.Config.CompletionScope.Deferred) != 1 ||
				p.Config.CompletionScope.Deferred[0].Title != "Frozen work" || p.Config.NonClosing == nil || p.Config.NonClosing.Reason != "partial") {
				t.Fatalf("locked contract lost: %+v", p.Config)
			}
			// Projection leaves both candidate and frozen definitions persisted.
			row := mustRun(t, e, f.runID)
			storedFrozen, decodeErr := DecodeMilestones(row.MilestonesFrozen)
			wantFrozen := []Milestone{{ID: "frozen", Title: "Frozen work"}}
			if mode == "empty_frozen" {
				wantFrozen = []Milestone{}
			}
			if decodeErr != nil || !reflect.DeepEqual(storedFrozen, wantFrozen) {
				t.Fatal("projection changed persisted execution definitions")
			}
		})
	}
}

// The contract is built with the existing semantic fixture; this test supplies no
// new approval or reconciliation operation. It observes the frozen projection.
func TestClaimLockedProjectionSemanticContractLiveDB(t *testing.T) {
	e := setupCodexLiveDB(t)
	f := newCodexClaimFix(t, e, false)
	w := f.claimant(t, true)
	t.Cleanup(func() { dropCustodyHolds(t, e, f.userID) })
	frozen := semanticCompletionFixture(t, false)
	contract, err := parseCompletionContract(frozen.CompletionContract)
	if err != nil {
		t.Fatal(err)
	}
	criterion := contract.Criteria[0]
	hooked := false
	f.svc.claimHooks = &claimTestHooks{afterAssembly: func(_ context.Context, run store.Run, p *ClaimPayload, err error) {
		if err != nil || p == nil {
			t.Fatalf("assembly: %v", err)
		}
		if p.Config.CompletionPending != nil {
			t.Fatal("fixture already had pending work")
		}
		hooked = true
		e.exec(`UPDATE runs SET completion_contract_version=1, contract_revision=1,
 completion_contract=$2, milestones_frozen=$3 WHERE id=$1`, run.ID, frozen.CompletionContract, frozen.MilestonesFrozen)
	}}
	p, err := f.svc.Claim(e.ctx, w, nil)
	if !hooked || err != nil || p == nil {
		t.Fatalf("Claim: hooked=%v err=%v", hooked, err)
	}
	pending := p.Config.CompletionPending
	if p.Config.CompletionContractVersion == nil || *p.Config.CompletionContractVersion != 1 ||
		p.Config.ContractRevision == nil || *p.Config.ContractRevision != 1 || pending == nil ||
		pending.ContractRevision != 1 || len(pending.Criteria) != 1 ||
		pending.Criteria[0].CriterionID != criterion.ID || pending.Criteria[0].MilestoneID != criterion.MilestoneID ||
		pending.Criteria[0].Status != "unverified" || pending.Criteria[0].AuthorizationRevision != 1 {
		t.Fatalf("locked semantic contract pending projection missing: %+v", pending)
	}
	if p.ExecutionMilestones == nil || len(*p.ExecutionMilestones) != 1 || (*p.ExecutionMilestones)[0].ID != "m1" {
		t.Fatal("semantic frozen execution missing")
	}
}
