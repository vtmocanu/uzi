package workersvc

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// boundedReplayFixture starts with the original worker declaration, not the SQL
// outcome that the worker could only learn after receiving its acknowledgment.
func boundedReplayFixture(t *testing.T, generation int64, reason, verdict string) (codexTestEnv, *Service, store.Worker, uuid.UUID, StateRequest) {
	t.Helper()
	env := setupCodexLiveDB(t)
	userID, workerID, repoID := env.seedCodexInfra(t)
	leadID := env.seedCodexRun(t, userID, workerID, repoID)
	env.exec(`UPDATE runs SET status='running',harness='claude',auto_approve=true,
		plan_source='agent',plan_cross_check_required=true,claim_generation=$2 WHERE id=$1`, leadID, generation)
	plan, size := "Review the original bounded candidate", "s"
	caps, tools, milestones := []string{capability.Docker}, []string{}, []Milestone{}
	presentation := uuid.New()
	req := StateRequest{State: "awaiting_approval", ClaimGeneration: &generation,
		PlanMd: &plan, Milestones: &milestones, RequiredCapabilities: &caps,
		RequiredTools: &tools, SizeClass: &size, PresentationID: &presentation,
		PlanCrossCheckGateReason: &reason, PlanCrossCheckRefusal: "submit_failed"}
	if reason == "candidate_refused" {
		req.PlanCrossCheckRefusal = "candidate_invalid"
	}
	if verdict != "" {
		candidate := PlanCrossCheckCandidate{PlanMd: plan, Milestones: []byte(`[]`),
			RequiredCapabilities: caps, RequiredTools: tools, SizeClass: size, BaseCommit: strings.Repeat("a", 40)}
		digest, err := candidate.Digest()
		if err != nil {
			t.Fatal(err)
		}
		childID := uuid.New()
		env.exec(`INSERT INTO runs (id,user_id,repo_id,kind,target_run_id,harness,report_only,budget_wall_seconds,issue_title,issue_description)
			VALUES ($1,$2,$3,'cross_check',$4,'codex',true,1800,'check','check')`, childID, userID, repoID, leadID)
		var reasonClass any
		if verdict == "failed" {
			reasonClass = "model_error"
		}
		env.exec(`INSERT INTO cross_checks (lead_run_id,stage,round,lead_claim_generation,plan_md,milestones,
			required_capabilities,required_tools,size_class,base_commit,planning_diff,candidate_digest,
			checker_run_id,checker_harness,verdict,reason_class,findings,deadline_at)
			VALUES ($1,'plan',1,$2,$3,$4,$5,'{}','s',$6,'',$7,$8,'codex',$9,$10,
				'{"summary":"Original findings","items":[]}',now()+interval '30 minutes')`,
			leadID, generation, plan, candidate.Milestones, caps, candidate.BaseCommit, digest, childID, verdict, reasonClass)
	}
	svc := New(env.q, env.box, testParams())
	svc.SetTxBeginner(env.pool)
	return env, svc, store.Worker{ID: workerID, UserID: userID}, leadID, req
}

func assertBoundedGateUnchanged(t *testing.T, before, after store.Run) {
	t.Helper()
	// Refusal accounting and status_since may change; approval-bearing state must not.
	fields := func(r store.Run) []any {
		return []any{r.Status, r.AutoApprove, r.PlanCrossCheckRequired, r.PlanMd,
			r.MilestonesCandidate, r.RequiredCapabilities, r.RequiredTools, r.SizeClass,
			r.PlanCrossCheckGateReason, r.GatePresentationID, r.GateRevision,
			r.GatePresentedPayload, r.GatePayloadDigest}
	}
	if !reflect.DeepEqual(fields(before), fields(after)) {
		t.Fatalf("report changed approval state: before=%+v after=%+v", fields(before), fields(after))
	}
}

func TestPlanCrossCheckBoundedOriginalReplayLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name, reason, verdict, want string
		generation                  int64
	}{
		{"pending checker", "checker_failed", "pending", "interrupted", 1},
		{"recorded model error", "checker_failed", "failed", "model_error", 1},
		{"recovered missing row", "checker_failed", "", "interrupted", 2},
		{"recovered candidate refusal", "candidate_refused", "", "interrupted", 2},
		{"ordinary missing row control", "checker_failed", "", "checker_failed", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, svc, worker, leadID, original := boundedReplayFixture(t, tc.generation, tc.reason, tc.verdict)
			initial, initialErr := env.q.GetPlanCrossCheck(env.ctx, leadID)
			report := func(req StateRequest, wantRevision int64, wantReason string) store.Run {
				t.Helper()
				_, applied, revision, err := svc.SetStateReport(env.ctx, worker, leadID, req)
				if err != nil || !applied || revision != wantRevision {
					t.Fatalf("report: applied=%v revision=%d err=%v, want revision %d", applied, revision, err, wantRevision)
				}
				got := mustRun(t, env, leadID)
				if got.Status != "awaiting_approval" || got.AutoApprove ||
					got.GateRevision != wantRevision || got.GatePresentationID.Bytes != [16]byte(*req.PresentationID) ||
					!got.GatePresentationID.Valid || got.PlanCrossCheckGateReason.String != wantReason ||
					got.PlanCrossCheckGateReason.Valid != (wantReason != "") {
					t.Fatalf("wrong human gate: %+v", got)
				}
				return got
			}
			report(original, 1, tc.want)
			settled, settledErr := env.q.GetPlanCrossCheck(env.ctx, leadID)
			if tc.verdict != "" {
				if initialErr != nil || settledErr != nil {
					t.Fatalf("cross-check reads: initial=%v settled=%v", initialErr, settledErr)
				}
				// A pending checker may settle on the first forced gate. Its candidate
				// and findings remain historical evidence, independent of that settlement.
				if initial.ID != settled.ID || initial.PlanMd != settled.PlanMd ||
					!bytes.Equal(initial.CandidateDigest, settled.CandidateDigest) ||
					!bytes.Equal(initial.Findings, settled.Findings) {
					t.Fatal("first settlement changed historical candidate/findings")
				}
			} else if !errors.Is(initialErr, pgx.ErrNoRows) || !errors.Is(settledErr, pgx.ErrNoRows) {
				t.Fatalf("missing-row control: initial=%v settled=%v", initialErr, settledErr)
			}
			// Simulate human requirement overrides after A; a retry must use the
			// presented digest and leave all live overrides intact.
			env.exec(`UPDATE runs SET required_capabilities='{}',required_tools='{go}',size_class='l' WHERE id=$1`, leadID)
			first := mustRun(t, env, leadID)
			retry := report(original, 1, tc.want)
			assertBoundedGateUnchanged(t, first, retry)

			b := original
			planB, idB := "Review a revised human candidate", uuid.New()
			b.PlanMd, b.PresentationID = &planB, &idB
			b.PlanCrossCheckGateReason, b.PlanCrossCheckRefusal = nil, ""
			currentB := report(b, 2, "")
			assertBoundedGateUnchanged(t, currentB, report(b, 2, ""))
			_, applied, err := svc.SetState(env.ctx, worker, leadID, original)
			if applied || err == nil {
				t.Fatalf("historical original A accepted: applied=%v err=%v", applied, err)
			}
			assertBoundedGateUnchanged(t, currentB, mustRun(t, env, leadID))
			after, afterErr := env.q.GetPlanCrossCheck(env.ctx, leadID)
			if tc.verdict != "" {
				if afterErr != nil || !reflect.DeepEqual(settled, after) {
					t.Fatalf("replay/revision changed settled cross-check: err=%v before=%+v after=%+v", afterErr, settled, after)
				}
			} else if !errors.Is(afterErr, pgx.ErrNoRows) {
				t.Fatalf("replay fabricated cross-check: %v", afterErr)
			}
		})
	}
}

func TestPlanCrossCheckBoundedReplayFencesLiveDB(t *testing.T) {
	for _, name := range []string{
		"changed plan", "changed milestones", "changed capabilities", "changed tools", "changed size",
		"unseen ID", "idless", "missing generation", "zero generation", "stale generation",
		"released", "wrong worker", "wrong user", "terminal", "untracked kind", "not required",
		"wrong harness", "automatic parked gate", "invalid submit refusal", "missing submit refusal",
		"invalid candidate refusal", "missing candidate refusal",
	} {
		t.Run(name, func(t *testing.T) {
			env, svc, worker, leadID, req := boundedReplayFixture(t, 2, "checker_failed", "")
			_, applied, err := svc.SetState(env.ctx, worker, leadID, req)
			if err != nil || !applied {
				t.Fatalf("first presentation: applied=%v err=%v", applied, err)
			}
			wantErr := ErrInvalidState
			switch name {
			case "changed plan":
				plan := "Different approval-bearing plan"
				req.PlanMd = &plan
				wantErr = ErrGatePresentationConflict
			case "changed milestones":
				ms := []Milestone{{ID: "M1", Title: "Changed milestone"}}
				req.Milestones = &ms
				wantErr = ErrGatePresentationConflict
			case "changed capabilities":
				// Capabilities union with the presented set: add a distinct valid capability.
				caps := []string{capability.Docker, capability.JVM}
				req.RequiredCapabilities = &caps
				wantErr = ErrGatePresentationConflict
			case "changed tools":
				tools := []string{"go"}
				req.RequiredTools = &tools
				wantErr = ErrGatePresentationConflict
			case "changed size":
				size := "l"
				req.SizeClass = &size
				wantErr = ErrGatePresentationConflict
			case "unseen ID":
				id := uuid.New()
				req.PresentationID = &id
			case "idless":
				req.PresentationID = nil
			case "missing generation":
				req.ClaimGeneration = nil
				wantErr = ErrClaimGenerationRequired
			case "zero generation":
				env.exec(`UPDATE runs SET claim_generation=0 WHERE id=$1`, leadID)
				gen := int64(0)
				req.ClaimGeneration = &gen
			case "stale generation":
				gen := int64(1)
				req.ClaimGeneration = &gen
				wantErr = ErrStaleClaim
			case "released":
				env.exec(`UPDATE runs SET claim_released_at=now() WHERE id=$1`, leadID)
				wantErr = ErrStaleClaim
			case "wrong worker":
				worker.ID = uuid.New()
				wantErr = ErrRunNotOwned
			case "wrong user":
				worker.UserID = uuid.New()
			case "terminal":
				env.exec(`UPDATE runs SET status='completed' WHERE id=$1`, leadID)
			case "untracked kind":
				env.exec(`UPDATE runs SET kind='chat',repo_id=NULL,issue_iid=NULL,branch=NULL WHERE id=$1`, leadID)
			case "not required":
				env.exec(`UPDATE runs SET plan_cross_check_required=false WHERE id=$1`, leadID)
			case "wrong harness":
				env.exec(`UPDATE runs SET harness='codex' WHERE id=$1`, leadID)
			case "automatic parked gate":
				env.exec(`UPDATE runs SET auto_approve=true WHERE id=$1`, leadID)
			case "invalid submit refusal":
				req.PlanCrossCheckRefusal = "anything"
			case "missing submit refusal":
				req.PlanCrossCheckRefusal = ""
			case "invalid candidate refusal":
				reason := "candidate_refused"
				req.PlanCrossCheckGateReason, req.PlanCrossCheckRefusal = &reason, "anything"
			case "missing candidate refusal":
				reason := "candidate_refused"
				req.PlanCrossCheckGateReason, req.PlanCrossCheckRefusal = &reason, ""
			}
			before := mustRun(t, env, leadID)
			_, applied, err = svc.SetState(env.ctx, worker, leadID, req)
			if applied || !errors.Is(err, wantErr) {
				t.Fatalf("invalid replay: applied=%v err=%v, want %v", applied, err, wantErr)
			}
			assertBoundedGateUnchanged(t, before, mustRun(t, env, leadID))
			if _, err := env.q.GetPlanCrossCheck(env.ctx, leadID); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("refused replay fabricated cross-check: %v", err)
			}
		})
	}
}
