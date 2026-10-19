package workersvc

import (
	"bytes"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

func TestSemanticGateRawPlanDisplayLiveDB(t *testing.T) {
	f := newSemanticRevisionLive(t)
	// The fixture starts running: no status bypass is needed for this candidate.
	f.structural.PlanMd = "implement\x00 café 😀 behavior"
	f.sources = append(f.sources, apitypes.SemanticSourceCapture{ID: "linked-prd", Kind: "prd", Provenance: "repo:prd", Completeness: "complete", Bytes: []byte("café\x00 😀 requirement")})
	prd := f.sources[len(f.sources)-1]
	f.proposal.Criteria[0].Citations = append(f.proposal.Criteria[0].Citations, apitypes.SemanticCitation{SourceID: prd.ID, Hash: semanticHash(prd.Bytes), Start: 0, End: len(prd.Bytes)})
	f.submit(t)
	rawCandidate := f.candidate
	before := semanticRevisionSnapshot(t, f)
	f.submit(t)
	if !sameSemanticJSON(rawCandidate, f.candidate) || !bytes.Equal(before, semanticRevisionSnapshot(t, f)) {
		t.Fatal("identical raw retry wrote a revision or remapped identity")
	}
	publishGateCoverage(t, f)
	req := gateReportRequest(t, f)
	gate := presentSemanticGate(t, f, req)
	if !sameSemanticJSON(gate, presentSemanticGate(t, f, req)) {
		t.Fatal("identical report changed presentation")
	}
	assertRaw := func(g *apitypes.SemanticGateDTO, plan string) {
		t.Helper()
		if string(g.Plan) != plan || g.Presentation.Candidate.PlanHash != semanticHash([]byte(plan)) || !sameSemanticJSON(g.Presentation.Candidate, f.candidate) {
			t.Fatal("owner DTO lost raw plan or criterion identity")
		}
		found := false
		for _, source := range g.Sources {
			if source.ID == prd.ID {
				found = true
				if !bytes.Equal(source.Bytes, prd.Bytes) {
					t.Fatal("owner DTO lost raw PRD bytes")
				}
			}
		}
		if !found {
			t.Fatal("linked PRD absent")
		}
		run, err := f.e.q.GetRunByID(f.e.ctx, f.run)
		if err != nil {
			t.Fatal(err)
		}
		snapshot, ok, err := decodeGatePresentedPayload(run.GatePresentedPayload)
		projected, _ := stripNUL(plan)
		if err != nil || !ok || !run.PlanMd.Valid || run.PlanMd.String != projected || snapshot.PlanMd == nil || *snapshot.PlanMd != projected {
			t.Fatal("PG text/presentation did not retain only display projection")
		}
	}
	assertRaw(gate, f.structural.PlanMd)

	// A bound, ACKed human receipt admits the visible-equivalent fresh candidate.
	inputID := submitSemanticRevision(t, f, gate, "remove the interior NUL from the raw plan", true)
	f.structural.PlanMd = "implement café 😀 behavior"
	f.submit(t)
	if f.candidate.PlanHash == rawCandidate.PlanHash || f.candidate.Digest == rawCandidate.Digest || f.candidate.Criteria[0].ID == rawCandidate.Criteria[0].ID || f.candidate.PlanFieldsDigest != rawCandidate.PlanFieldsDigest {
		t.Fatal("visible-equivalent raw revision lost separate approval identity")
	}
	req = gateReportRequest(t, f)
	req.RevisionInputID = &inputID
	req.ExpectedGateRevision = &gate.Approval.PresentationRevision
	next := presentSemanticGate(t, f, req)
	if sameSemanticJSON(gate.Approval, next.Approval) {
		t.Fatal("raw revision reused approval")
	}
	if _, err := semanticGateApprove(f, f.e.userID, gate, nil); err == nil {
		t.Fatal("old raw approval accepted")
	}
	assertRaw(next, f.structural.PlanMd)

	// Exercise exact freeze and replay on an interior-NUL candidate too.
	inputID = submitSemanticRevision(t, f, next, "restore exact raw plan", true)
	f.structural.PlanMd = "implement\x00 café 😀 behavior"
	f.submit(t)
	req = gateReportRequest(t, f)
	req.RevisionInputID = &inputID
	req.ExpectedGateRevision = &next.Approval.PresentationRevision
	final := presentSemanticGate(t, f, req)
	assertRaw(final, f.structural.PlanMd)
	if f.candidate.PlanHash != rawCandidate.PlanHash || f.candidate.Digest == rawCandidate.Digest || f.candidate.Criteria[0].ID == rawCandidate.Criteria[0].ID {
		t.Fatal("restored raw plan resurrected historical candidate identity")
	}
	if _, err := semanticGateApprove(f, f.e.userID, gate, nil); err == nil {
		t.Fatal("restored raw plan resurrected original approval")
	}
	first, err := semanticGateApprove(f, f.e.userID, final, nil)
	if err != nil {
		t.Fatal(err)
	}
	before = semanticRevisionSnapshot(t, f)
	replay, err := semanticGateApprove(f, f.e.userID, final, nil)
	if err != nil || first.ID != replay.ID || !first.CreatedAt.Equal(replay.CreatedAt) || !bytes.Equal(before, semanticRevisionSnapshot(t, f)) {
		t.Fatalf("freeze receipt replay changed identity/state: %v", err)
	}
	run, err := f.e.q.GetRunByID(f.e.ctx, f.run)
	if err != nil {
		t.Fatal(err)
	}
	contract, err := parseCompletionContract(run.CompletionContract)
	if err != nil || contract.PlanHash != f.candidate.PlanHash || !sameSemanticJSON(contract.SemanticPresentation, &final.Presentation) || !sameSemanticJSON(contract.SemanticApproval, &final.Approval) || contract.Criteria[0].ID != f.candidate.Criteria[0].ID {
		t.Fatalf("freeze lost raw identity: %v", err)
	}
}

// This compatibility fixture seeds the previous dormant writer's encoding.
// It does not rewrite an existing production enrollment or claim.
func TestSemanticGateLegacyRawMappingLiveDB(t *testing.T) {
	f := newSemanticRevisionLive(t)
	f.structural.PlanMd = "legacy\x00 raw plan"
	f.submit(t)
	current := f.candidate
	planning, err := f.e.q.GetSemanticPlanning(f.e.ctx, f.run)
	if err != nil || !planning.Fingerprint.Valid {
		t.Fatalf("current fingerprint missing: %v", err)
	}
	oldFields := gatePresentedPayload{PlanMd: &f.structural.PlanMd, Milestones: f.structural.Milestones,
		RequiredCapabilities: f.structural.RequiredCapabilities, RequiredTools: f.structural.RequiredTools,
		SizeClass: nonEmptyPtr(f.structural.SizeClass)}
	canonical, err := oldFields.canonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	legacy := current
	legacy.PlanFieldsDigest = semanticHash(canonical)
	legacy.Digest = ""
	identity := semanticJSON(t, struct {
		Fingerprint string
		Candidate   apitypes.SemanticCandidate
	}{planning.Fingerprint.String, legacy})
	legacy.Digest = semanticHash(append([]byte("uzi:semantic_v1:candidate\x00"), identity...))
	if legacy.PlanFieldsDigest == current.PlanFieldsDigest || legacy.Digest == current.Digest {
		t.Fatal("legacy fixture did not retain the old raw-text encoding")
	}
	raw := semanticJSON(t, legacy)
	f.e.exec(t, "UPDATE semantic_planning SET candidate=$2 WHERE run_id=$1", f.run, raw)
	f.e.exec(t, "UPDATE semantic_candidate_history SET candidate=$2 WHERE run_id=$1 AND revision=$3", f.run, raw, legacy.Revision)
	run, err := f.e.q.GetRunByID(f.e.ctx, f.run)
	if err != nil {
		t.Fatal(err)
	}
	presentation, _, err := loadSemanticGate(f.e.ctx, f.e.q, run)
	if err != nil || presentation == nil || !sameSemanticJSON(presentation.Candidate, legacy) {
		t.Fatalf("legacy mapping fixture is invalid: %v", err)
	}
	before := semanticRevisionSnapshot(t, f)
	f.submit(t)
	if !sameSemanticJSON(f.candidate, legacy) || !bytes.Equal(before, semanticRevisionSnapshot(t, f)) {
		t.Fatal("identical retry backfilled the persisted legacy mapping")
	}
	_, applied, _, err := f.svc.SetStateReport(f.e.ctx, semanticRevisionWorker(t, f), f.run, gateReportRequest(t, f))
	if err == nil || applied || !bytes.Equal(before, semanticRevisionSnapshot(t, f)) {
		t.Fatalf("legacy mapping was silently reinterpreted: applied=%v err=%v", applied, err)
	}
	f.structural.PlanMd += " revised"
	f.submit(t)
	if f.candidate.Revision != legacy.Revision+1 || f.candidate.Digest == legacy.Digest || f.candidate.Criteria[0].ID == legacy.Criteria[0].ID {
		t.Fatal("fresh proposal restored historical identities")
	}
	presentSemanticGate(t, f, gateReportRequest(t, f))
}

func TestSemanticGateDisplayEmptyAdmissionLiveDB(t *testing.T) {
	f := newSemanticRevisionLive(t)
	before := semanticRevisionSnapshot(t, f)
	for _, plan := range []string{"\x00", " \x00\n\t"} {
		f.structural.PlanMd = plan
		if _, err := f.svc.SubmitSemanticCandidate(f.e.ctx, f.worker, f.run, 7, f.structural, semanticJSON(t, f.proposal), f.sources); err == nil {
			t.Fatal("display-empty plan admitted")
		}
		if !bytes.Equal(before, semanticRevisionSnapshot(t, f)) {
			t.Fatal("invalid plan changed previously persisted data")
		}
	}
}
