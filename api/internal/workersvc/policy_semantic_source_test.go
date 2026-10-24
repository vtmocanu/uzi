package workersvc

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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

type policySemanticLegacyApprovalReader struct {
	*policyIdentityReader
	hasRevision    bool
	revisionErr    error
	revisionRunIDs []uuid.UUID
}

func (r *policySemanticLegacyApprovalReader) HasPolicyRevisionHistory(_ context.Context, id uuid.UUID) (bool, error) {
	r.revisionRunIDs = append(r.revisionRunIDs, id)
	return r.hasRevision, r.revisionErr
}

func TestPolicySemanticSourceLegacyApproval(t *testing.T) {
	for _, tc := range []struct {
		name string
		want bool
	}{
		{"accepted_A", true},
		{"frozen_A_current_planning_B", true},
		{"missing_contract", false},
		{"corrupt_contract", false},
		{"changed_plan", false},
		{"changed_frozen_definition", false},
		{"changed_contract_revision", false},
		{"missing_immutable_history", false},
		{"corrupt_immutable_history", false},
		{"immutable_history_read_error", false},
		{"planning_read_error", false},
		{"recorded_revision", false},
		{"revision_history_query_error", false},
		{"legacy_unapproved", false},
		{"ordinary_approved", true},
		{"ordinary_unapproved", false},
		{"structural_no_revision", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _, identity, cc := semanticPolicyGateFixture(t)
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
			r.PolicyRevisionSource = nil
			q := &policySemanticLegacyApprovalReader{policyIdentityReader: identity}
			legacyApproved := true
			switch tc.name {
			case "frozen_A_current_planning_B":
				b := payload.Semantic.Candidate
				b.Revision++
				digest, err := semanticCheckerDigest(PlanCrossCheckCandidate{PlanMd: cc.PlanMd.String, Milestones: cc.Milestones, RequiredCapabilities: cc.RequiredCapabilities, RequiredTools: cc.RequiredTools, SizeClass: cc.SizeClass.String, BaseCommit: cc.BaseCommit.String, PlanningDiff: cc.PlanningDiff.String}, b)
				if err != nil {
					t.Fatal(err)
				}
				b.Digest = hex.EncodeToString(digest)
				identity.planning.Candidate = semanticJSON(t, b)
				identity.planning.Revision = b.Revision
				identity.history[b.Digest] = semanticJSON(t, b)
				current, valid, err := policyRevisionCurrentTarget(context.Background(), q, r)
				if err != nil || !valid || current == nil || current.Digest != b.Digest {
					t.Fatalf("current planning B is not valid: valid=%v err=%v", valid, err)
				}
			case "missing_contract":
				r.CompletionContract = nil
			case "corrupt_contract":
				r.CompletionContract = []byte("{")
			case "changed_plan":
				r.PlanMd.String += " different accepted plan"
			case "changed_frozen_definition":
				r.MilestonesFrozen = []byte(`[{"id":"m1","title":"Different execution"}]`)
			case "changed_contract_revision":
				r.ContractRevision.Int32++
			case "missing_immutable_history":
				identity.history = nil
			case "corrupt_immutable_history":
				identity.history[payload.Semantic.Candidate.Digest] = []byte("{")
			case "immutable_history_read_error":
				identity.historyErr = errors.New("immutable history unavailable")
			case "planning_read_error":
				identity.planningErr = errors.New("planning unavailable")
			case "recorded_revision":
				q.hasRevision = true
			case "revision_history_query_error":
				q.revisionErr = errors.New("revision history unavailable")
			case "legacy_unapproved":
				legacyApproved = false
			case "ordinary_approved", "ordinary_unapproved":
				r.PolicyRefusalCount = 0
				r.CompletionContract = nil
				legacyApproved = tc.name == "ordinary_approved"
			case "structural_no_revision":
				identity.planningErr = pgx.ErrNoRows
				r.CompletionContract = nil
				r.CompletionContractVersion, r.ContractRevision = pgtype.Int4{}, pgtype.Int4{}
			}
			if len(r.PolicyRevisionSource) != 0 || r.GateRevision != 0 || r.GatePresentationID.Valid {
				t.Fatal("fixture must retain SQL-NULL source and no published gate")
			}
			if got := policyRevisionPlanApproved(context.Background(), q, r, legacyApproved); got != tc.want {
				t.Fatalf("approved=%v; want %v", got, tc.want)
			}
			if r.PolicyRefusalCount > 0 && legacyApproved {
				if len(q.revisionRunIDs) != 1 || q.revisionRunIDs[0] != r.ID {
					t.Fatal("NULL-source fallback must query revision history for this run once")
				}
			} else if len(q.revisionRunIDs) != 0 {
				t.Fatal("ordinary or unapproved run queried revision history")
			}
		})
	}
}

func TestPolicySemanticSourceAcceptedEmptyInventory(t *testing.T) {
	r, _, reader, cc := semanticPolicyGateFixture(t)
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
	t.Run("frozen_A_current_planning_B", func(t *testing.T) {
		b := payload.Semantic.Candidate
		b.Revision++
		digest, err := semanticCheckerDigest(PlanCrossCheckCandidate{PlanMd: cc.PlanMd.String, Milestones: cc.Milestones, RequiredCapabilities: cc.RequiredCapabilities, RequiredTools: cc.RequiredTools, SizeClass: cc.SizeClass.String, BaseCommit: cc.BaseCommit.String, PlanningDiff: cc.PlanningDiff.String}, b)
		if err != nil {
			t.Fatal(err)
		}
		b.Digest = hex.EncodeToString(digest)
		changedReader := *reader
		changedReader.planning.Candidate = semanticJSON(t, b)
		changedReader.planning.Revision = b.Revision
		changedReader.history = map[string][]byte{
			payload.Semantic.Candidate.Digest: semanticJSON(t, payload.Semantic.Candidate),
			b.Digest:                          semanticJSON(t, b),
		}
		current, valid, err := policyRevisionCurrentTarget(context.Background(), &changedReader, r)
		if err != nil || !valid || current == nil || current.Digest != b.Digest {
			t.Fatalf("current planning B is not valid: valid=%v err=%v", valid, err)
		}
		target, valid, err := policyRevisionAcceptedTarget(context.Background(), &changedReader, r)
		if err != nil || !valid || target == nil || target.Digest != payload.Semantic.Candidate.Digest {
			t.Fatalf("accepted target replaced by mutable planning: valid=%v err=%v", valid, err)
		}
		changedQ := &policyIdentitySemanticApprovalReader{&policyIdentityApprovalReader{}, &changedReader}
		if !policyRevisionPlanApproved(context.Background(), changedQ, r, true) {
			t.Fatal("accepted A refused after current planning changed to B")
		}
	})
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
