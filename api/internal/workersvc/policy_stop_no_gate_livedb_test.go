package workersvc

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/settings"
)

// Public issue creation supplies birth coverage. Only admission/ownership and
// cross-check configuration are bridged; plan, progress and hold use service APIs.
// These Go carriers exercise API transactions, not worker execution or ACKs.
func policyStopNoGateFixture(t *testing.T, seeded, checked, full bool) (codexTestEnv, *gateFix, uuid.UUID) {
	t.Helper()
	e, f, old, candidate := acceptanceUnpublishedFixture(t)
	f.svc.p.PolicyStopNegotiated = true
	resetInterlockSetting(t, e)
	f.svc.SetCompletionInterlockSettings(settings.New(e.q, time.Minute))
	repo := uuid.UUID(mustRun(t, e, old).RepoID.Bytes)
	seedEligibleIssue(t, e, repo, 9471)
	var seed *SeededPlan
	if seeded {
		seed = &SeededPlan{PlanMD: "# Persisted owner implementation plan"}
	}
	r, err := f.svc.CreateRun(e.ctx, f.userID, repo, 9471, "ordinary no-gate Stop", nil, nil, false, seed, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	e.exec("UPDATE runs SET worker_id=$2,status='claimed',claim_generation=1,auto_approve=$4,plan_cross_check_required=$3 WHERE id=$1", r.ID, f.wkr.ID, checked, !seeded)
	ms := []Milestone{{ID: "m1", Title: "one"}, {ID: "m2", Title: "two"}}
	req := StateRequest{State: "running", ClaimGeneration: i64(1), Milestones: &ms, SessionID: strPtr("no-gate-session")}
	if !seeded {
		candidate.PlanMd = "# Persisted agent implementation plan"
		candidate.Milestones = mustStopJSON(t, ms)
		req.PlanMd = &candidate.PlanMd
		if checked {
			cc, err := f.svc.SubmitPlanCrossCheck(e.ctx, f.wkr, r.ID, 1, candidate, 1)
			if err != nil {
				t.Fatal(err)
			}
			autopilotDecide(t, e, f, cc, "approve")
			digest, err := candidate.Digest()
			if err != nil {
				t.Fatal(err)
			}
			req.CandidateDigest = hex.EncodeToString(digest)
			req.RequiredCapabilities = &candidate.RequiredCapabilities
			req.RequiredTools = &candidate.RequiredTools
			req.SizeClass = &candidate.SizeClass
		}
	}
	autopilotRevisionReport(t, f, r.ID, req)
	completed := []string{"m1"}
	if full {
		completed = append(completed, "m2")
	}
	progress := autopilotRevisionReport(t, f, r.ID, StateRequest{State: "running", ClaimGeneration: i64(1), MilestonesCompleted: &completed})
	frozen, err := DecodeMilestones(progress.MilestonesFrozen)
	if err != nil || len(frozen) != 2 {
		t.Fatalf("public running report froze %d milestones: %v", len(frozen), err)
	}
	if seeded {
		// Public seeded creation deliberately stays legacy with the setting ON.
		if progress.CompletionContractVersion.Valid || progress.ContractRevision.Valid || len(progress.CompletionContract) != 0 {
			t.Fatal("seeded running report manufactured an interlocked contract")
		}
	} else if !progress.CompletionContractVersion.Valid || !progress.ContractRevision.Valid || len(progress.CompletionContract) == 0 {
		t.Fatalf("public unseeded contract not frozen: version=%+v revision=%+v bytes=%d", progress.CompletionContractVersion, progress.ContractRevision, len(progress.CompletionContract))
	}
	hold := policyRequest()
	hold.Phase = strPtr("implementation")
	held, disposition, err := f.svc.ReportPolicyHold(e.ctx, f.wkr, r.ID, hold)
	if err != nil || disposition != PolicyHoldApplied || held.PolicyLastPhase.String != "implementation" {
		t.Fatalf("implementation hold disposition=%s phase=%s err=%v", disposition, held.PolicyLastPhase.String, err)
	}
	assertAutopilotApproval(t, e, r.ID, true)
	return e, f, r.ID
}

func policyStopNoGateReview(t *testing.T, e codexTestEnv, f *gateFix, run uuid.UUID) apitypes.PolicyStopReview {
	t.Helper()
	before := policyOwnerStopSnapshot(t, e, run)
	review, err := f.svc.PolicyStopReviewForOwner(e.ctx, f.userID, run, PolicyHoldActionToken(mustRun(t, e, run)))
	if err != nil {
		t.Fatal(err)
	}
	if policyOwnerStopSnapshot(t, e, run) != before {
		t.Fatal("review mutated persisted state")
	}
	if review.Manifest.Gate != nil || review.Manifest.SourceContext.Gate != nil || len(review.Manifest.HumanFeedback) != 0 {
		t.Fatal("ordinary review fabricated gate or revision feedback")
	}
	raw := mustStopJSON(t, review.Manifest)
	if !bytes.Contains(raw, []byte("\"gate\":null")) || bytes.Contains(raw, []byte("\"presentation_id\"")) || bytes.Contains(raw, []byte("\"payload_digest\"")) {
		t.Fatalf("fabricated gate fields: %s", raw)
	}
	return review
}

func TestPolicyStopNoGateApprovedLiveDB(t *testing.T) {
	for _, mode := range []string{"seeded_partial", "seeded_full", "agent_partial", "agent_full", "approve_witness"} {
		t.Run(mode, func(t *testing.T) {
			seeded := mode == "seeded_partial" || mode == "seeded_full"
			full := mode == "seeded_full" || mode == "agent_full"
			e, f, run := policyStopNoGateFixture(t, seeded, mode == "approve_witness", full)
			review := policyStopNoGateReview(t, e, f, run)
			if mode == "approve_witness" {
				cc := review.Manifest.CrossCheckFeedback
				if len(cc) != 1 || cc[0].Verdict != "approve" || cc[0].Supersede {
					t.Fatalf("missing independent APPROVE witness: %+v", cc)
				}
			}
			before := mustRun(t, e, run)
			var scopesBefore int
			if err := e.pool.QueryRow(e.ctx, "SELECT count(*) FROM run_user_inputs WHERE run_id=$1 AND kind='scope'", run).Scan(&scopesBefore); err != nil {
				t.Fatal(err)
			}
			res, err := f.svc.SubmitInputWithOptions(e.ctx, f.userID, run, "stop", "finalize completed work", nil, SubmitInputOptions{ActionIfMatch: review.StopIfMatch})
			want := 1
			if full {
				want = 2
			}
			if err != nil || !res.Resumed || res.PolicyStop == nil || res.ScopeCeiling == nil || *res.ScopeCeiling != want {
				t.Fatalf("confirmation=%+v err=%v", res, err)
			}
			after := mustRun(t, e, run)
			if after.Status != "queued" || !after.ScopeCeiling.Valid || int(after.ScopeCeiling.Int32) != want ||
				len(after.PolicyStopResolution) == 0 || after.SessionID != before.SessionID ||
				after.PolicyFreshClaimGeneration != before.PolicyFreshClaimGeneration {
				t.Fatal("Stop did not persist the preserving queue transition and exact ceiling")
			}
			var scopesAfter int
			if err := e.pool.QueryRow(e.ctx, "SELECT count(*) FROM run_user_inputs WHERE run_id=$1 AND kind='scope'", run).Scan(&scopesAfter); err != nil {
				t.Fatal(err)
			}
			if scopesAfter != scopesBefore+1 {
				t.Fatal("Stop did not create exactly one scope input")
			}
			if after.GateRevision != before.GateRevision || after.GatePresentationID != before.GatePresentationID ||
				!bytes.Equal(after.GatePayloadDigest, before.GatePayloadDigest) || !bytes.Equal(after.GatePresentedPayload, before.GatePresentedPayload) {
				t.Fatal("Stop fabricated or changed a gate")
			}
			if after.PlanMd != before.PlanMd || after.PlanSource != before.PlanSource ||
				!bytes.Equal(after.PolicyRevisionSource, before.PolicyRevisionSource) ||
				after.PolicyCandidateCrossCheckID != before.PolicyCandidateCrossCheckID ||
				!bytes.Equal(after.MilestonesFrozen, before.MilestonesFrozen) ||
				!bytes.Equal(after.MilestonesCompleted, before.MilestonesCompleted) ||
				!bytes.Equal(after.CompletionContract, before.CompletionContract) ||
				after.CompletionContractVersion != before.CompletionContractVersion || after.ContractRevision != before.ContractRevision {
				t.Fatal("Stop changed approved plan/source/selection/frozen execution contract")
			}
			assertAutopilotApproval(t, e, run, true)
			// Accepted replay authenticates immutable consent before current eligibility.
			e.exec("UPDATE runs SET status='failed',plan_md=NULL,gate_revision=9 WHERE id=$1", run)
			snapshot := policyOwnerStopSnapshot(t, e, run)
			retry, err := f.svc.SubmitInputWithOptions(e.ctx, f.userID, run, "stop", "", nil, SubmitInputOptions{ActionIfMatch: review.StopIfMatch})
			if err != nil || retry.PolicyStop == nil || retry.PolicyStop.Disposition != "already_accepted" {
				t.Fatalf("exact receipt retry=%+v err=%v", retry, err)
			}
			if policyOwnerStopSnapshot(t, e, run) != snapshot {
				t.Fatal("accepted retry wrote after mutable terminal fixture change")
			}
		})
	}
}

func TestPolicyStopNoGateRefusalsLiveDB(t *testing.T) {
	for _, mode := range []string{"blank_plan", "missing_plan", "flags_only", "unknown_source", "malformed_v1", "unknown_v1", "partial_gate", "bad_selection", "required_selection", "pending_revise", "primary", "bad_witness", "future_witness", "settled_witness", "projection"} {
		t.Run(mode, func(t *testing.T) {
			checked := mode == "bad_witness" || mode == "future_witness" || mode == "settled_witness" || mode == "projection"
			e, f, run := policyStopNoGateFixture(t, false, checked, false)
			review := policyStopNoGateReview(t, e, f, run)
			switch mode {
			case "blank_plan":
				e.exec("UPDATE runs SET plan_md='  ' WHERE id=$1", run)
			case "missing_plan":
				e.exec("UPDATE runs SET plan_md=NULL WHERE id=$1", run)
			case "flags_only":
				e.exec("UPDATE runs SET auto_approve=false WHERE id=$1", run)
			case "unknown_source":
				e.exec("UPDATE runs SET policy_revision_source=NULL WHERE id=$1", run)
			case "malformed_v1":
				e.exec("UPDATE runs SET policy_revision_source='{}' WHERE id=$1", run)
			case "unknown_v1":
				e.exec("UPDATE runs SET policy_revision_source=jsonb_set(policy_revision_source,'{version}','99') WHERE id=$1", run)
			case "partial_gate":
				e.exec("UPDATE runs SET gate_payload_digest=$2 WHERE id=$1", run, []byte{1})
			case "bad_selection":
				e.exec("UPDATE runs SET policy_candidate_cross_check_id=$2 WHERE id=$1", run, uuid.New())
			case "required_selection":
				e.exec("UPDATE runs SET plan_cross_check_required=true WHERE id=$1", run)
			case "primary":
				var input int64
				if err := e.pool.QueryRow(e.ctx, "INSERT INTO run_user_inputs(run_id,kind,body) VALUES($1,'revise_plan','primary fixture') RETURNING id", run).Scan(&input); err != nil {
					t.Fatal(err)
				}
				source := acceptanceSource(t, e, run)
				id := stopDecimal(input)
				source.PrimaryRevision = &apitypes.PolicyRevisionRef{Source: "run_user_input", InputID: &id}
				e.exec("UPDATE runs SET policy_revision_source=$2 WHERE id=$1", run, mustStopJSON(t, source))
			case "pending_revise":
				// Applicable unbound feedback with frozen progress remains refused.
				// Full pending-revision Stop waiver is intentionally still open.
				e.exec("INSERT INTO run_user_inputs(run_id,kind,body) VALUES($1,'revise_plan','pending revision')", run)
			case "bad_witness":
				e.exec("UPDATE cross_checks SET candidate_digest=$2 WHERE lead_run_id=$1", run, bytes.Repeat([]byte{1}, 32))
			case "future_witness":
				e.exec("UPDATE cross_checks SET lead_claim_generation=2 WHERE lead_run_id=$1", run)
			case "settled_witness":
				var scope int64
				if err := e.pool.QueryRow(e.ctx, "INSERT INTO run_user_inputs(run_id,kind,scope_ceiling,body) VALUES($1,'scope',1,'settlement fixture') RETURNING id", run).Scan(&scope); err != nil {
					t.Fatal(err)
				}
				// Settlement is legal only for revise; preserve that database shape.
				e.exec("UPDATE cross_checks SET verdict='revise' WHERE lead_run_id=$1", run)
				e.exec("UPDATE cross_checks SET revision_disposition='superseded_by_stop',revision_settled_at=now(),revision_superseded_by_stop_input_id=$2 WHERE lead_run_id=$1", run, scope)
			case "projection":
				e.exec("UPDATE runs SET plan_md='different persisted plan' WHERE id=$1", run)
			}
			before := policyOwnerStopSnapshot(t, e, run)
			_, err := f.svc.PolicyStopReviewForOwner(e.ctx, f.userID, run, PolicyHoldActionToken(mustRun(t, e, run)))
			requirePolicyOwnerStopConflict(t, err, "policy_stop_review_unverifiable")
			if policyOwnerStopSnapshot(t, e, run) != before {
				t.Fatal("refused preview mutated state")
			}
			_, err = f.svc.SubmitInputWithOptions(e.ctx, f.userID, run, "stop", "", nil, SubmitInputOptions{ActionIfMatch: review.StopIfMatch})
			requirePolicyOwnerStopConflict(t, err, "policy_stop_review_unverifiable")
			if policyOwnerStopSnapshot(t, e, run) != before {
				t.Fatal("refused confirmation mutated state")
			}
		})
	}
}

func TestPolicyStopNoGateStaleConsentLiveDB(t *testing.T) {
	for _, mode := range []string{"completed", "contract", "ceiling"} {
		t.Run(mode, func(t *testing.T) {
			e, f, run := policyStopNoGateFixture(t, false, false, false)
			review := policyStopNoGateReview(t, e, f, run)
			switch mode {
			case "completed":
				e.exec("UPDATE runs SET milestones_completed=$2 WHERE id=$1", run, `["m1","m2"]`)
			case "contract":
				r := mustRun(t, e, run)
				var c map[string]any
				if err := json.Unmarshal(r.CompletionContract, &c); err != nil {
					t.Fatal(err)
				}
				c["revision"] = r.ContractRevision.Int32 + 1
				e.exec("UPDATE runs SET completion_contract=$2,contract_revision=contract_revision+1 WHERE id=$1", run, mustStopJSON(t, c))
			case "ceiling":
				e.exec("UPDATE runs SET scope_ceiling=2 WHERE id=$1", run)
			}
			before := policyOwnerStopSnapshot(t, e, run)
			_, err := f.svc.SubmitInputWithOptions(e.ctx, f.userID, run, "stop", "", nil, SubmitInputOptions{ActionIfMatch: review.StopIfMatch})
			requirePolicyOwnerStopConflict(t, err, "policy_stop_token_stale")
			if policyOwnerStopSnapshot(t, e, run) != before {
				t.Fatal("stale consent mutated state")
			}
			next := policyStopNoGateReview(t, e, f, run)
			if next.StopIfMatch == review.StopIfMatch {
				t.Fatal("mutable execution change did not bind new consent")
			}
		})
	}
}
