package workersvc

import (
	"bytes"
	"reflect"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func newSemanticRevisionLive(t *testing.T) *coverageLiveFixture {
	t.Helper()
	f := newSemanticGateLive(t)
	f.svc.p.PlanMaxRevisions = 10
	if _, err := f.e.q.RegisterWorker(f.e.ctx, store.RegisterWorkerParams{
		ID: f.worker, ProtocolCapabilities: []string{capability.InputReceiptsV1, capability.GateRevisionV1},
	}); err != nil {
		t.Fatal(err)
	}
	return f
}

func semanticRevisionWorker(t *testing.T, f *coverageLiveFixture) store.Worker {
	t.Helper()
	worker, err := f.e.q.GetWorkerByID(f.e.ctx, f.worker)
	if err != nil {
		t.Fatal(err)
	}
	return worker
}

func submitSemanticRevision(t *testing.T, f *coverageLiveFixture, gate *apitypes.SemanticGateDTO, body string, ack bool) int64 {
	t.Helper()
	input, err := f.svc.SubmitInputWithOptions(f.e.ctx, f.e.userID, f.run, "revise_plan", body, nil, SubmitInputOptions{ExpectedGateRevision: &gate.Approval.PresentationRevision})
	if err != nil || input.ID <= 0 {
		t.Fatalf("owner revision: %+v %v", input, err)
	}
	if ack {
		receipt, err := f.svc.AckInputs(f.e.ctx, semanticRevisionWorker(t, f), f.run, 7, []int64{input.ID})
		if err != nil || !receipt.Active {
			t.Fatalf("ACK: %+v %v", receipt, err)
		}
	}
	return input.ID
}

func assertSemanticRevisionAwaiting(t *testing.T, f *coverageLiveFixture) {
	t.Helper()
	run, err := f.e.q.GetRunByID(f.e.ctx, f.run)
	if err != nil || run.Status != "awaiting_approval" {
		t.Fatalf("revision changed execution state: %s %v", run.Status, err)
	}
}

func semanticRevisionSnapshot(t *testing.T, f *coverageLiveFixture) []byte {
	t.Helper()
	var raw []byte
	err := f.e.pool.QueryRow(f.e.ctx, `SELECT jsonb_build_object(
 'run',to_jsonb(r)||jsonb_build_object('_xmin',r.xmin::text,'_ctid',r.ctid::text),
 'inputs',(SELECT jsonb_agg(to_jsonb(i)||jsonb_build_object('_xmin',i.xmin::text,'_ctid',i.ctid::text) ORDER BY id) FROM run_user_inputs i WHERE run_id=r.id),
 'planning',(SELECT to_jsonb(p)||jsonb_build_object('_xmin',p.xmin::text,'_ctid',p.ctid::text) FROM semantic_planning p WHERE run_id=r.id),
 'history',(SELECT jsonb_agg(to_jsonb(h)||jsonb_build_object('_xmin',h.xmin::text,'_ctid',h.ctid::text) ORDER BY revision) FROM semantic_candidate_history h WHERE run_id=r.id),
 'snapshots',(SELECT jsonb_agg(to_jsonb(s)||jsonb_build_object('_xmin',s.xmin::text,'_ctid',s.ctid::text) ORDER BY hash) FROM semantic_snapshots s WHERE run_id=r.id),
 'sources',(SELECT jsonb_agg(to_jsonb(s)||jsonb_build_object('_xmin',s.xmin::text,'_ctid',s.ctid::text) ORDER BY identity_hash) FROM semantic_source_snapshots s WHERE run_id=r.id))
 FROM runs r WHERE id=$1`, f.run).Scan(&raw)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestSemanticRevisionFlowLiveDB(t *testing.T) {
	for _, withReport := range []bool{false, true} {
		t.Run(map[bool]string{false: "visible unavailable", true: "own report"}[withReport], func(t *testing.T) {
			f := newSemanticRevisionLive(t)
			historicalReport := publishGateCoverage(t, f)
			original := f.candidate
			gateA := presentSemanticGate(t, f, gateReportRequest(t, f))
			inputID := submitSemanticRevision(t, f, gateA, "make the criterion independently observable", true)
			assertSemanticRevisionAwaiting(t, f)
			f.proposal.Criteria[0].DoneWhen += " and independently observable"
			f.submit(t)
			if f.candidate.Revision != original.Revision+1 || f.candidate.Digest == original.Digest || f.candidate.Criteria[0].ID == original.Criteria[0].ID || f.candidate.PlanHash != original.PlanHash {
				t.Fatal("same-plan criteria revision did not get fresh identity")
			}
			compiled := f.candidate
			f.submit(t)
			if !reflect.DeepEqual(compiled, f.candidate) {
				t.Fatal("compile lost-ACK replay changed UUIDs/revision")
			}
			payload, err := f.svc.SemanticCandidatePayload(f.e.ctx, f.worker, f.run, 7, f.candidate.Digest)
			if err != nil || !reflect.DeepEqual(payload.Candidate, f.candidate) || string(payload.Plan) != f.structural.PlanMd {
				t.Fatalf("read revised candidate: %v", err)
			}
			if err := f.svc.PublishSemanticCoverage(f.e.ctx, f.worker, f.run, 7, historicalReport); err == nil {
				t.Fatal("historical report reused")
			}
			status := f.status(t)
			if status.Report != nil || status.Outcome != "unavailable" || status.ChargedTokens != 20 || status.Reviews != 1 {
				t.Fatalf("revision reset/reused accounting: %+v", status)
			}
			reserveRaw := semanticJSON(t, apitypes.SemanticCoverageReserveRequest{RequestID: uuid.NewString(), SemanticCoverageBinding: SemanticCoverageBinding(f.candidate), Supported: true})
			reservation, err := f.svc.ReserveSemanticCoverage(f.e.ctx, f.worker, f.run, 7, reserveRaw)
			if err != nil || reservation.Outcome != "reserved" || reservation.Reviews != 2 || reservation.ChargedTokens != 40020 {
				t.Fatalf("revision reservation: %+v %v", reservation, err)
			}
			replay, err := f.svc.ReserveSemanticCoverage(f.e.ctx, f.worker, f.run, 7, reserveRaw)
			if err != nil || replay.Outcome != "replay" || replay.InvocationID != reservation.InvocationID || replay.Candidate != nil || !replay.Deadline.Equal(reservation.Deadline) {
				t.Fatalf("reservation lost-ACK: %+v %v", replay, err)
			}
			used := 30
			settlement := f.settle(t, reservation, "success", &used)
			var publication []byte
			if withReport {
				report := apitypes.SemanticCoverageReport{SemanticCoverageBinding: SemanticCoverageBinding(f.candidate), Requirements: []apitypes.SemanticCoverageResult{{RequirementID: "r1", Status: "represented", Explanation: "revised behavior", Citations: f.candidate.Criteria[0].Citations, CriterionIDs: []string{f.candidate.Criteria[0].ID}}}, Criteria: []apitypes.SemanticCoverageAtomicity{{CriterionID: f.candidate.Criteria[0].ID, Atomic: true, Explanation: "one behavior"}}}
				publication = semanticJSON(t, apitypes.SemanticCoveragePublishRequest{ReviewID: reservation.ReviewID, InvocationID: reservation.InvocationID, Report: report})
				if err := f.svc.PublishSemanticCoverage(f.e.ctx, f.worker, f.run, 7, publication); err != nil {
					t.Fatal(err)
				}
			}
			req := gateReportRequest(t, f)
			req.RevisionInputID = &inputID
			req.ExpectedGateRevision = &gateA.Approval.PresentationRevision
			gateB := presentSemanticGate(t, f, req)
			if gateB.Approval.PresentationRevision != gateA.Approval.PresentationRevision+1 || gateB.Presentation.Coverage.ChargedTokens != 50 || gateB.Presentation.Coverage.Reviews != 2 {
				t.Fatal("final publication lost cumulative budget")
			}
			if withReport {
				if gateB.Presentation.Coverage.Report == nil || gateB.Presentation.InvocationID != reservation.InvocationID {
					t.Fatal("missing own bounded report")
				}
			} else if gateB.Presentation.Coverage.Report != nil || gateB.Presentation.Coverage.Outcome != "unavailable" {
				t.Fatal("missing visible unavailable result")
			}
			var applied bool
			if err := f.e.pool.QueryRow(f.e.ctx, "SELECT applied_at IS NOT NULL FROM run_user_inputs WHERE id=$1", inputID).Scan(&applied); err != nil || !applied {
				t.Fatalf("publication failed to settle receipt: %v", err)
			}
			before := semanticRevisionSnapshot(t, f)
			presentSemanticGate(t, f, req)
			f.submit(t) // APPLIED now: exact fingerprint still reads current identities.
			if !reflect.DeepEqual(compiled, f.candidate) {
				t.Fatal("APPLIED compile replay changed identity")
			}
			replay, err = f.svc.ReserveSemanticCoverage(f.e.ctx, f.worker, f.run, 7, reserveRaw)
			if err != nil || replay.Outcome != "replay" || replay.InvocationID != reservation.InvocationID || replay.Candidate != nil {
				t.Fatalf("APPLIED reservation replay: %+v %v", replay, err)
			}
			if err := f.svc.SettleSemanticCoverage(f.e.ctx, f.worker, f.run, 7, settlement); err != nil {
				t.Fatal(err)
			}
			if publication != nil {
				if err := f.svc.PublishSemanticCoverage(f.e.ctx, f.worker, f.run, 7, publication); err != nil {
					t.Fatal(err)
				}
			}
			if !bytes.Equal(before, semanticRevisionSnapshot(t, f)) {
				t.Fatal("lost-ACK replay changed persisted values or row versions")
			}
			f.proposal.Criteria[0].DoneWhen += " unauthorized"
			if _, err := f.svc.SubmitSemanticCandidate(f.e.ctx, f.worker, f.run, 7, f.structural, semanticJSON(t, f.proposal), f.sources); err == nil {
				t.Fatal("APPLIED receipt granted new candidate")
			}
			freshReserve := semanticJSON(t, apitypes.SemanticCoverageReserveRequest{RequestID: uuid.NewString(), SemanticCoverageBinding: SemanticCoverageBinding(compiled), Supported: true})
			if _, err := f.svc.ReserveSemanticCoverage(f.e.ctx, f.worker, f.run, 7, freshReserve); err == nil {
				t.Fatal("APPLIED receipt granted new invocation")
			}
			if !withReport {
				report := apitypes.SemanticCoverageReport{SemanticCoverageBinding: SemanticCoverageBinding(compiled), Requirements: []apitypes.SemanticCoverageResult{{RequirementID: "r1", Status: "represented", Explanation: "late report", Citations: compiled.Criteria[0].Citations, CriterionIDs: []string{compiled.Criteria[0].ID}}}, Criteria: []apitypes.SemanticCoverageAtomicity{{CriterionID: compiled.Criteria[0].ID, Atomic: true, Explanation: "one behavior"}}}
				if err := f.svc.PublishSemanticCoverage(f.e.ctx, f.worker, f.run, 7, semanticJSON(t, apitypes.SemanticCoveragePublishRequest{ReviewID: reservation.ReviewID, InvocationID: reservation.InvocationID, Report: report})); err == nil {
					t.Fatal("APPLIED receipt granted fresh report")
				}
			}
			if _, err := semanticGateApprove(f, f.e.userID, gateA, nil); err == nil {
				t.Fatal("old owner approval accepted")
			}
			if !bytes.Equal(before, semanticRevisionSnapshot(t, f)) {
				t.Fatal("unauthorized mutation changed persisted values or row versions")
			}
			if _, err := semanticGateApprove(f, f.e.userID, gateB, nil); err != nil {
				t.Fatal(err)
			}
			assertSemanticRevisionAwaiting(t, f)
		})
	}
}

func TestSemanticRevisionTransferredReceiptLiveDB(t *testing.T) {
	t.Helper()
	f := newSemanticRevisionLive(t)
	gate := presentSemanticGate(t, f, gateReportRequest(t, f))
	first := submitSemanticRevision(t, f, gate, "revise first", false)
	// A later queued input does not make the first receipt ineligible.
	submitSemanticRevision(t, f, gate, "revise later", false)
	previousWorker := f.e.seedWorker(t, nil)
	f.e.exec(t, "UPDATE run_user_inputs SET consumed_at=now(),consumed_worker_id=$2,consumed_claim_generation=6 WHERE id=$1", first, previousWorker)
	receipt, err := f.svc.AckInputs(f.e.ctx, semanticRevisionWorker(t, f), f.run, 7, []int64{first})
	if err != nil || !receipt.Active {
		t.Fatalf("transfer unapplied receipt: %+v %v", receipt, err)
	}
	f.proposal.Criteria[0].DoneWhen += " after transfer"
	f.submit(t)
	assertSemanticRevisionAwaiting(t, f)
	req := gateReportRequest(t, f)
	req.RevisionInputID, req.ExpectedGateRevision = &first, &gate.Approval.PresentationRevision
	presentSemanticGate(t, f, req)
}

func TestSemanticRevisionFreshRefusalLiveDB(t *testing.T) {
	for _, mode := range []string{"no receipt", "unconsumed", "empty", "unicode empty", "applied", "unbound", "old gate", "wrong worker", "wrong generation", "released", "superseded"} {
		t.Run(mode, func(t *testing.T) {
			f := newSemanticRevisionLive(t)
			gate := presentSemanticGate(t, f, gateReportRequest(t, f))
			if mode != "no receipt" {
				id := submitSemanticRevision(t, f, gate, "revise behavior", mode != "unconsumed")
				switch mode {
				case "empty":
					f.e.exec(t, "UPDATE run_user_inputs SET body=E' \\t\\n\\r' WHERE id=$1", id)
				case "unicode empty":
					f.e.exec(t, "UPDATE run_user_inputs SET body=$2 WHERE id=$1", id, "\u00A0\u2003\u2028\uFEFF")
				case "applied":
					_, err := f.svc.ApplyInputs(f.e.ctx, semanticRevisionWorker(t, f), f.run, 7, []int64{id})
					if err != nil {
						t.Fatal(err)
					}
				case "unbound":
					f.e.exec(t, "UPDATE run_user_inputs SET gate_binding='unbound',gate_revision=NULL WHERE id=$1", id)
				case "old gate":
					f.e.exec(t, "UPDATE run_user_inputs SET gate_revision=gate_revision+1 WHERE id=$1", id)
				case "wrong worker":
					other := f.e.seedWorker(t, nil)
					f.e.exec(t, "UPDATE run_user_inputs SET consumed_worker_id=$2 WHERE id=$1", id, other)
				case "wrong generation":
					f.e.exec(t, "UPDATE run_user_inputs SET consumed_claim_generation=6 WHERE id=$1", id)
				case "released":
					f.e.exec(t, "UPDATE runs SET claim_released_at=now() WHERE id=$1", f.run)
				case "superseded":
					f.e.exec(t, "UPDATE run_user_inputs SET disposition='superseded' WHERE id=$1", id)
				}
			}
			assertSemanticRevisionAwaiting(t, f)
			before := semanticRevisionSnapshot(t, f)
			f.proposal.Criteria[0].Text += " changed"
			if _, err := f.svc.SubmitSemanticCandidate(f.e.ctx, f.worker, f.run, 7, f.structural, semanticJSON(t, f.proposal), f.sources); err == nil {
				t.Fatal("unauthorized compile")
			}
			raw := semanticJSON(t, apitypes.SemanticCoverageReserveRequest{RequestID: uuid.NewString(), SemanticCoverageBinding: SemanticCoverageBinding(f.candidate), Supported: true})
			if _, err := f.svc.ReserveSemanticCoverage(f.e.ctx, f.worker, f.run, 7, raw); err == nil {
				t.Fatal("unauthorized reservation")
			}
			if !bytes.Equal(before, semanticRevisionSnapshot(t, f)) {
				t.Fatal("refusal changed persisted semantic/run/receipt values or row versions")
			}
		})
	}
}
