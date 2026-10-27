package workersvc

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Identity-only fixture: no accepted receipt, source authentication, or Stop
// permission is manufactured. The semantic candidate is compiled and finalized,
// unlike semanticCompletionFixture's deliberately synthetic digest.
func stopFrozenFixture(t *testing.T, profile string) (store.Run, apitypes.PolicyStopExecution, *policyIdentityReader) {
	t.Helper()
	structural, proposal, sources := semanticFixture()
	structural.Milestones = []byte(`[{"id":"m1","title":"Behavior"},{"id":"m2","title":"Follow up"}]`)
	r := interlockedRun(worker())
	r.Kind = "issue"
	r.PlanMd = pgtype.Text{String: structural.PlanMd, Valid: true}
	r.MilestonesFrozen = structural.Milestones
	r.MilestonesCompleted = idsJSON(t, "m1")
	r.ScopeCeiling = pgtype.Int4{Int32: 1, Valid: true}
	reader := &policyIdentityReader{planningErr: pgx.ErrNoRows}
	var err error
	switch profile {
	case "legacy":
		r.CompletionContractVersion, r.ContractRevision = pgtype.Int4{}, pgtype.Int4{}
	case "structural":
		r.CompletionContract, err = buildCompletionContract(r.MilestonesFrozen)
	case "semantic":
		maintainer := proposal.Criteria[0]
		maintainer.Kind = "maintainer"
		maintainer.Maintainer = &apitypes.SemanticMaintainer{Reason: "owner deployment", Conditions: []string{"available"}, VerificationSteps: []string{"observe rollout"}}
		proposal.Criteria = append(proposal.Criteria, maintainer)
		candidate, _, compileErr := compileSemanticProposal(structural, semanticJSON(t, proposal), sources)
		if compileErr != nil {
			t.Fatal(compileErr)
		}
		candidate.Revision = 1
		for i := range candidate.Criteria {
			candidate.Criteria[i].ID = uuid.NewString()
		}
		digest, digestErr := semanticCheckerDigest(structural, candidate)
		if digestErr != nil {
			t.Fatal(digestErr)
		}
		candidate.Digest = hex.EncodeToString(digest)
		r.GateRevision, r.GatePayloadDigest = 1, []byte{1}
		presentation := semanticGateProjection(&semanticCoverageTx{candidate: candidate}, r)
		approval := semanticApprovalFor(r, presentation)
		r.CompletionContract, err = semanticGateContract(&presentation, &approval)
		reader.planningErr = nil
		reader.planning.Enrollment = semanticJSON(t, apitypes.SemanticEnrollment{Profile: "semantic"})
		reader.history = map[string][]byte{candidate.Digest: semanticJSON(t, candidate)}
	default:
		t.Fatal("unknown profile")
	}
	if err != nil {
		t.Fatal(err)
	}
	return r, stopFrozenSnapshot(t, r), reader
}

func stopFrozenSnapshot(t *testing.T, r store.Run) apitypes.PolicyStopExecution {
	t.Helper()
	frozen, err := DecodeMilestones(r.MilestonesFrozen)
	if err != nil {
		t.Fatal(err)
	}
	completed, err := DecodeMilestoneIDs(r.MilestonesCompleted)
	if err != nil {
		t.Fatal(err)
	}
	e := apitypes.PolicyStopExecution{Contract: append([]byte(nil), r.CompletionContract...), Frozen: frozen, CompletedIDs: completed, ProposedCeiling: r.ScopeCeiling.Int32}
	if r.CompletionContractVersion.Valid {
		v := r.CompletionContractVersion.Int32
		e.ContractVersion = &v
	}
	if r.ContractRevision.Valid {
		v := stopDecimal(int64(r.ContractRevision.Int32))
		e.ContractRevision = &v
	}
	return e
}

func stopFrozenDifferentCandidate(t *testing.T, revision int64) apitypes.SemanticCandidate {
	t.Helper()
	structural, proposal, sources := semanticFixture()
	structural.PlanMd = "different unapproved B"
	structural.Milestones = []byte(`[{"id":"b1","title":"Different gate"}]`)
	proposal.Criteria[0].MilestoneID = "b1"
	candidate, _, err := compileSemanticProposal(structural, semanticJSON(t, proposal), sources)
	if err != nil {
		t.Fatal(err)
	}
	candidate.Revision = revision
	candidate.Criteria[0].ID = uuid.NewString()
	digest, err := semanticCheckerDigest(structural, candidate)
	if err != nil {
		t.Fatal(err)
	}
	candidate.Digest = hex.EncodeToString(digest)
	return candidate
}

func TestPolicyStopFrozenExecutionIdentity(t *testing.T) {
	for _, profile := range []string{"legacy", "structural", "semantic"} {
		for _, name := range []string{
			"subset", "full", "completed_reordered", "saved_old_ceiling", "frozen_order", "frozen_title", "frozen_id", "saved_title",
			"invalid_title", "duplicate_frozen", "empty_frozen", "malformed_frozen", "duplicate_completed", "saved_duplicate_completed", "foreign_completed", "saved_foreign_completed", "empty_completed", "different_completed",
			"ceiling_missing", "ceiling_drift", "proposed_zero", "proposed_drift", "version_nullable", "revision_nullable", "revision_overflow", "saved_revision_drift", "version_drift", "contract_drift", "canonical_keys", "planning_error", "bad_enrollment", "nil_reader", "typed_nil_reader",
		} {
			t.Run(profile+"/"+name, func(t *testing.T) {
				r, e, q := stopFrozenFixture(t, profile)
				want := name == "subset" || name == "full" || name == "completed_reordered" || name == "saved_old_ceiling" || name == "canonical_keys"
				var wantErr error
				var reader policyRevisionIdentityReader = q
				switch name {
				case "full", "completed_reordered":
					r.MilestonesCompleted = idsJSON(t, "m1", "m2")
					r.ScopeCeiling.Int32 = 2
					e = stopFrozenSnapshot(t, r)
					if name == "completed_reordered" {
						e.CompletedIDs = []string{"m2", "m1"}
					}
					if profile != "legacy" {
						if eligible, verifiable := scopeCapEligibility(r); eligible || !verifiable {
							t.Fatalf("full-set control: eligible=%v verifiable=%v", eligible, verifiable)
						}
					}
				case "saved_old_ceiling":
					old := int32(49)
					e.CurrentCeiling = &old
				case "frozen_order":
					r.MilestonesFrozen = semanticJSON(t, []Milestone{e.Frozen[1], e.Frozen[0]})
				case "frozen_title":
					ms := append([]Milestone(nil), e.Frozen...)
					ms[0].Title += " "
					r.MilestonesFrozen = semanticJSON(t, ms)
				case "frozen_id":
					ms := append([]Milestone(nil), e.Frozen...)
					ms[1].ID = "m3"
					r.MilestonesFrozen = semanticJSON(t, ms)
				case "saved_title":
					e.Frozen[0].Title = "Different"
				case "invalid_title":
					e.Frozen[0].Title = " "
					r.MilestonesFrozen = semanticJSON(t, e.Frozen)
				case "duplicate_frozen":
					e.Frozen[1] = e.Frozen[0]
					r.MilestonesFrozen = semanticJSON(t, e.Frozen)
				case "empty_frozen":
					r.MilestonesFrozen = []byte("[]")
				case "malformed_frozen":
					r.MilestonesFrozen = []byte("{")
				case "duplicate_completed":
					r.MilestonesCompleted = idsJSON(t, "m1", "m1")
				case "saved_duplicate_completed":
					e.CompletedIDs = []string{"m1", "m1"}
				case "foreign_completed":
					r.MilestonesCompleted = idsJSON(t, "foreign")
				case "saved_foreign_completed":
					e.CompletedIDs = []string{"foreign"}
				case "empty_completed":
					r.MilestonesCompleted = []byte("[]")
					e.CompletedIDs = nil
				case "different_completed":
					e.CompletedIDs = []string{"m2"}
				case "ceiling_missing":
					r.ScopeCeiling.Valid = false
				case "ceiling_drift":
					r.ScopeCeiling.Int32 = 2
				case "proposed_zero":
					e.ProposedCeiling = 0
				case "proposed_drift":
					e.ProposedCeiling = 2
				case "version_nullable":
					if e.ContractVersion == nil {
						v := int32(1)
						e.ContractVersion = &v
					} else {
						e.ContractVersion = nil
					}
				case "revision_nullable":
					if e.ContractRevision == nil {
						v := stopDecimal(1)
						e.ContractRevision = &v
					} else {
						e.ContractRevision = nil
					}
				case "revision_overflow":
					v := apitypes.PolicyStopDecimal("2147483648")
					e.ContractRevision = &v
				case "saved_revision_drift":
					v := stopDecimal(2)
					e.ContractRevision = &v
				case "version_drift":
					v := int32(2)
					e.ContractVersion = &v
				case "contract_drift":
					e.Contract = []byte(`{"changed":true}`)
				case "canonical_keys":
					if profile == "legacy" {
						e.Contract = []byte(" null ")
					} else {
						var fields map[string]json.RawMessage
						if err := json.Unmarshal(e.Contract, &fields); err != nil {
							t.Fatal(err)
						}
						// Whitespace and object ordering are presentation, not contract identity.
						e.Contract = append([]byte(" \n"), semanticJSON(t, fields)...)
					}
				case "planning_error":
					wantErr = errors.New("enrollment storage failure")
					q.planningErr = wantErr
				case "bad_enrollment":
					q.planningErr = nil
					q.planning.Enrollment = []byte(`{"profile":"semantic","unknown":true}`)
				case "nil_reader":
					reader = nil
				case "typed_nil_reader":
					var absent *policyIdentityReader
					reader = absent
				}
				before := semanticJSON(t, r)
				target, valid, err := policyStopFrozenExecutionTarget(context.Background(), reader, r, e, false)
				if valid != want || !errors.Is(err, wantErr) {
					t.Fatalf("valid=%v err=%v; want %v %v", valid, err, want, wantErr)
				}
				if want && profile == "semantic" {
					c, err := parseCompletionContract(r.CompletionContract)
					if err != nil || target == nil || !sameSemanticJSON(*target, c.SemanticPresentation.Candidate) {
						t.Fatal("lost frozen semantic identity")
					}
					if !reflect.DeepEqual(q.lookups, []store.GetSemanticCandidateByDigestParams{{RunID: r.ID, Column2: target.Digest}}) {
						t.Fatalf("history reads: %+v", q.lookups)
					}
				} else if target != nil {
					t.Fatal("unexpected semantic target")
				}
				if !reflect.DeepEqual(before, semanticJSON(t, r)) {
					t.Fatal("identity check mutated row")
				}
			})
		}
	}
}

func TestPolicyStopFrozenExecutionSemanticHistory(t *testing.T) {
	for _, name := range []string{"gated_B_mutable_C", "gate_zero", "gate_zero_drift", "missing_history", "malformed_history", "mismatched_history", "history_error", "pending_revision_two", "unenrolled_history", "strict_unknown_contract", "strict_duplicate_contract", "broken_partition", "missing_authorization", "semantic_null_contract"} {
		t.Run(name, func(t *testing.T) {
			r, e, q := stopFrozenFixture(t, "semantic")
			c, err := parseCompletionContract(r.CompletionContract)
			if err != nil {
				t.Fatal(err)
			}
			a := c.SemanticPresentation.Candidate
			noGate := name == "gate_zero" || name == "gate_zero_drift"
			want := name == "gated_B_mutable_C" || name == "gate_zero" || name == "pending_revision_two" || name == "unenrolled_history"
			var wantErr error
			switch name {
			case "gated_B_mutable_C":
				// Current unapproved gate B has different plan AND definitions; mutable C
				// is intentionally unusable and cannot substitute for frozen history A.
				r.PlanMd.String = "different unapproved B"
				r.MilestonesCandidate = []byte(`[{"id":"b1","title":"Different gate"}]`)
				b := stopFrozenDifferentCandidate(t, 2)
				p := semanticGateProjection(&semanticCoverageTx{candidate: b}, r)
				r.GatePresentedPayload = semanticJSON(t, gatePresentedPayload{PlanMd: &r.PlanMd.String, Milestones: r.MilestonesCandidate, Semantic: &p})
				q.history[b.Digest] = semanticJSON(t, b)
				q.planning.Candidate = []byte(`{"digest":"mutable-C"}`)
				q.planning.Revision = 99
			case "unenrolled_history":
				q.planningErr = pgx.ErrNoRows
			case "gate_zero_drift":
				r.PlanMd.String += " drift"
			case "missing_history":
				delete(q.history, a.Digest)
			case "malformed_history":
				q.history[a.Digest] = []byte("{")
			case "mismatched_history":
				changed := a
				changed.Criteria = append([]apitypes.SemanticCriterion(nil), a.Criteria...)
				changed.Criteria[0].DoneWhen = "different"
				q.history[a.Digest] = semanticJSON(t, changed)
			case "history_error":
				wantErr = errors.New("history storage failure")
				q.historyErr = wantErr
			case "pending_revision_two":
				c.Revision = 2
				r.ContractRevision.Int32 = 2
				r.CompletionContract = semanticJSON(t, c)
				e = stopFrozenSnapshot(t, r)
				pending, err := CompletionPending(r)
				if err != nil || pending == nil || len(pending.Criteria) == 0 || pending.Criteria[0].Status != "unverified" {
					t.Fatalf("pending control: %+v %v", pending, err)
				}
			case "strict_unknown_contract":
				r.CompletionContract = append([]byte(`{"unknown":true,`), r.CompletionContract[1:]...)
				e = stopFrozenSnapshot(t, r)
			case "strict_duplicate_contract":
				r.CompletionContract = append([]byte(`{"profile":"semantic",`), r.CompletionContract[1:]...)
				e = stopFrozenSnapshot(t, r)
			case "broken_partition":
				r.CompletionContract = append([]byte(`{"scope":{"in":["foreign"],"out":[]},`), r.CompletionContract[1:]...)
				e = stopFrozenSnapshot(t, r)
			case "missing_authorization":
				c.MaintainerAuthorizations = nil
				r.CompletionContract = semanticJSON(t, c)
				e = stopFrozenSnapshot(t, r)
			case "semantic_null_contract":
				r.CompletionContract = nil
				r.ContractRevision = pgtype.Int4{}
				r.CompletionContractVersion = pgtype.Int4{}
				e = stopFrozenSnapshot(t, r)
			}
			target, valid, err := policyStopFrozenExecutionTarget(context.Background(), q, r, e, noGate)
			if valid != want || !errors.Is(err, wantErr) {
				t.Fatalf("valid=%v err=%v; want %v %v", valid, err, want, wantErr)
			}
			if want && (target == nil || target.Digest != a.Digest) {
				t.Fatal("selected mutable candidate")
			}
			for _, lookup := range q.lookups {
				if lookup.RunID != r.ID || lookup.Column2 != a.Digest {
					t.Fatalf("wrong immutable lookup: %+v", lookup)
				}
			}
			if name == "gated_B_mutable_C" && (policyRevisionPlanApproved(context.Background(), q, r, false) || string(q.planning.Candidate) != `{"digest":"mutable-C"}`) {
				t.Fatal("adopted mutable candidate")
			}
		})
	}
}

func TestPolicyStopFrozenExecutionStructuralContamination(t *testing.T) {
	for _, profile := range []string{"legacy", "structural"} {
		for _, name := range []string{"enrolled", "reserved_null", "nonnull_legacy", "invalid_revision", "partial_tuple", "number_identity"} {
			t.Run(profile+"/"+name, func(t *testing.T) {
				r, _, q := stopFrozenFixture(t, profile)
				switch name {
				case "enrolled":
					q.planningErr = nil
					q.planning.Enrollment = []byte(`{"profile":"semantic"}`)
				case "reserved_null":
					if profile == "legacy" {
						r.CompletionContract = []byte(`{"semantic_presentation":null}`)
					} else {
						r.CompletionContract = append([]byte(`{"semantic_presentation":null,`), r.CompletionContract[1:]...)
					}
				case "nonnull_legacy":
					r.CompletionContract = []byte(`{"profile":"structural"}`)
				case "invalid_revision":
					r.ContractRevision = pgtype.Int4{Int32: 0, Valid: true}
				case "partial_tuple":
					r.CompletionContractVersion.Valid = !r.CompletionContractVersion.Valid
				case "number_identity":
					if profile == "structural" {
						r.CompletionContract = append([]byte(`{"n":9007199254740993,`), r.CompletionContract[1:]...)
					} else {
						r.CompletionContract = []byte(`{"n":9007199254740993}`)
					}
				}
				e := stopFrozenSnapshot(t, r)
				if name == "number_identity" {
					if profile == "structural" {
						raw, err := buildCompletionContract(r.MilestonesFrozen)
						if err != nil {
							t.Fatal(err)
						}
						e.Contract = append([]byte(`{"n":9007199254740992,`), raw[1:]...)
					} else {
						e.Contract = []byte(`{"n":9007199254740992}`)
					}
				}
				if _, valid, err := policyStopFrozenExecutionTarget(context.Background(), q, r, e, false); valid || err != nil {
					t.Fatalf("contamination accepted=%v err=%v", valid, err)
				}
			})
		}
	}
}
