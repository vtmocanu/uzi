package workersvc

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func semanticPolicyGateFixture(t *testing.T) (store.Run, apitypes.PolicyRevisionSourceV1, *policyIdentityReader, store.CrossCheck) {
	t.Helper()
	cc, c, reader := policyIdentityFixture(t)
	presentation := semanticGateProjection(&semanticCoverageTx{candidate: c}, store.Run{ClaimGeneration: 1})
	payload := gatePresentedPayload{PlanMd: textPtr(cc.PlanMd), Milestones: cc.Milestones, Semantic: &presentation}
	raw, digest, err := payload.digest()
	if err != nil {
		t.Fatal(err)
	}
	r := store.Run{ID: cc.LeadRunID, Kind: "issue", Harness: "codex", ClaimGeneration: 1, GateRevision: 1,
		GatePresentationID: pgtype.UUID{Bytes: uuid.New(), Valid: true}, GatePresentedPayload: raw, GatePayloadDigest: digest,
		PlanMd: cc.PlanMd, MilestonesCandidate: cc.Milestones}
	source := apitypes.PolicyRevisionSourceV1{Version: 1, Gate: &apitypes.PolicyStopGateContext{
		Revision: "1", PresentationID: uuid.UUID(r.GatePresentationID.Bytes).String(), PayloadDigest: hex.EncodeToString(digest)}, AssociatedCrossChecks: []apitypes.PolicyRevisionRef{}}
	r.PolicyRevisionSource = semanticJSON(t, source)
	return r, source, reader, cc
}

func TestPolicySemanticSourceGate(t *testing.T) {
	for _, name := range []string{"exact", "latest_changed", "missing_semantic", "missing_history", "malformed_history", "wrong_binding", "wrong_display", "wrong_source", "changed_digest", "bad_candidate_digest", "storage_error", "structural", "changed_requirements"} {
		t.Run(name, func(t *testing.T) {
			r, source, reader, _ := semanticPolicyGateFixture(t)
			payload, _, _ := decodeGatePresentedPayload(r.GatePresentedPayload)
			want := name == "exact" || name == "latest_changed" || name == "structural"
			var wantErr error
			switch name {
			case "latest_changed":
				reader.planning.Candidate = []byte(`{"digest":"unrelated"}`)
			case "missing_semantic":
				payload.Semantic = nil
			case "missing_history":
				reader.history = nil
			case "malformed_history":
				reader.history[payload.Semantic.Candidate.Digest] = []byte("{")
			case "wrong_binding":
				payload.Semantic.Binding.CandidateDigest = "wrong"
			case "wrong_display":
				*payload.PlanMd += "changed"
				r.PlanMd.String = *payload.PlanMd
			case "wrong_source":
				source.Gate.PresentationID = uuid.NewString()
			case "changed_digest":
				r.GatePayloadDigest = bytes.Repeat([]byte{1}, 32)
			case "bad_candidate_digest":
				payload.Semantic.Candidate.Digest = "not-a-canonical-digest"
				payload.Semantic.Binding = SemanticCoverageBinding(payload.Semantic.Candidate)
				reader.history[payload.Semantic.Candidate.Digest] = semanticJSON(t, payload.Semantic.Candidate)
			case "storage_error":
				wantErr = errors.New("history unavailable")
				reader.historyErr = wantErr
			case "structural":
				payload.Semantic = nil
				reader.planningErr = errors.New("should not query")
				// A legacy reader has no semantic authority and retains structural behavior.
			case "changed_requirements":
				r.RequiredCapabilities = []string{"docker"}
			}
			if name != "changed_digest" && name != "wrong_source" {
				var err error
				r.GatePresentedPayload, r.GatePayloadDigest, err = payload.digest()
				if err != nil {
					t.Fatal(err)
				}
				source.Gate.PayloadDigest = hex.EncodeToString(r.GatePayloadDigest)
			}
			var q any = reader
			if name == "structural" {
				q = nil
			}
			before := append([]byte{}, r.GatePresentedPayload...)
			got, err := policyRevisionGateMatches(context.Background(), q, r, source)
			if got != want || !errors.Is(err, wantErr) {
				t.Fatalf("match=%v err=%v want=%v err=%v", got, err, want, wantErr)
			}
			if !bytes.Equal(before, r.GatePresentedPayload) {
				t.Fatal("authentication rewrote snapshot")
			}
		})
	}
}

func TestPolicySemanticSourceApplicability(t *testing.T) {
	r, _, reader, cc := semanticPolicyGateFixture(t)
	payload, _, _ := decodeGatePresentedPayload(r.GatePresentedPayload)
	a := payload.Semantic.Candidate
	b := a
	b.Revision++
	digest, digestErr := semanticCheckerDigest(PlanCrossCheckCandidate{PlanMd: cc.PlanMd.String, Milestones: cc.Milestones, RequiredCapabilities: cc.RequiredCapabilities, RequiredTools: cc.RequiredTools, SizeClass: cc.SizeClass.String, BaseCommit: cc.BaseCommit.String, PlanningDiff: cc.PlanningDiff.String}, b)
	if digestErr != nil {
		t.Fatal(digestErr)
	}
	b.Digest = hex.EncodeToString(digest)
	reader.history[b.Digest] = semanticJSON(t, b)
	reader.planning.Candidate = semanticJSON(t, b)
	valid, err := policyRevisionIdentityValid(context.Background(), reader, cc)
	if err != nil || !valid {
		t.Fatalf("historical A: %v %v", valid, err)
	}
	for _, test := range []struct {
		name   string
		target *apitypes.SemanticCandidate
		want   bool
	}{
		{"exact_A", &a, true}, {"same_text_B", &b, false}, {"missing", nil, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := policyRevisionIdentityApplicable(context.Background(), reader, r, cc, test.target, false)
			if err != nil || got != test.want {
				t.Fatalf("applicable=%v err=%v", got, err)
			}
		})
	}
}

func TestPolicySemanticSourceAcceptedEmptyInventory(t *testing.T) {
	r, _, reader, _ := semanticPolicyGateFixture(t)
	payload, _, _ := decodeGatePresentedPayload(r.GatePresentedPayload)
	r.GateRevision = 0
	r.GatePresentationID = pgtype.UUID{}
	r.GatePayloadDigest, r.GatePresentedPayload = nil, nil
	r.MilestonesFrozen = r.MilestonesCandidate
	r.PlanSource = planSourceSeeded
	r.PolicyRefusalCount = 1
	r.CompletionContractVersion = pgtype.Int4{Int32: 1, Valid: true}
	r.ContractRevision = pgtype.Int4{Int32: 1, Valid: true}
	var err error
	r.CompletionContract, err = semanticGateContract(payload.Semantic, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.PolicyRevisionSource = semanticJSON(t, apitypes.PolicyRevisionSourceV1{Version: 1, AssociatedCrossChecks: []apitypes.PolicyRevisionRef{}})
	q := &policyIdentitySemanticApprovalReader{&policyIdentityApprovalReader{}, reader}
	if !policyRevisionPlanApproved(context.Background(), q, r, true) {
		t.Fatal("valid accepted empty inventory refused")
	}
	for _, tc := range []struct {
		name   string
		change func(*store.Run)
	}{
		{"changed_plan", func(row *store.Run) { row.PlanMd.String += " different accepted plan" }},
		{"changed_frozen_definition", func(row *store.Run) { row.MilestonesFrozen = []byte(`[{"id":"m1","title":"Different execution"}]`) }},
		{"changed_contract_revision", func(row *store.Run) { row.ContractRevision.Int32++ }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := r
			tc.change(&changed)
			if policyRevisionPlanApproved(context.Background(), q, changed, true) {
				t.Fatal("changed execution authorized by a different accepted semantic contract")
			}
		})
	}
}
