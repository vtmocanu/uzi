package workersvc

import (
	"context"
	"encoding/hex"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type policyIdentityReader struct {
	planning    store.GetSemanticPlanningRow
	planningErr error
	history     map[string][]byte
	historyErr  error
	lookups     []store.GetSemanticCandidateByDigestParams
	runIDs      []uuid.UUID
}

func (r *policyIdentityReader) GetSemanticPlanning(_ context.Context, id uuid.UUID) (store.GetSemanticPlanningRow, error) {
	r.runIDs = append(r.runIDs, id)
	return r.planning, r.planningErr
}

func (r *policyIdentityReader) GetSemanticCandidateByDigest(_ context.Context, p store.GetSemanticCandidateByDigestParams) ([]byte, error) {
	r.lookups = append(r.lookups, p)
	if r.historyErr != nil {
		return nil, r.historyErr
	}
	raw, ok := r.history[p.Column2]
	if !ok {
		return nil, pgx.ErrNoRows
	}
	return raw, nil
}

func policyIdentityFixture(t *testing.T) (store.CrossCheck, apitypes.SemanticCandidate, *policyIdentityReader) {
	t.Helper()
	c, proposal, sources := semanticFixture()
	immutable, _, err := compileSemanticProposal(c, semanticJSON(t, proposal), sources)
	if err != nil {
		t.Fatal(err)
	}
	immutable.Revision = 1
	immutable.Criteria[0].ID = uuid.NewString()
	digest, err := semanticCheckerDigest(c, immutable)
	if err != nil {
		t.Fatal(err)
	}
	immutable.Digest = hex.EncodeToString(digest)
	cc := store.CrossCheck{
		ID: uuid.New(), LeadRunID: uuid.New(), Stage: "plan", Round: 1, LeadClaimGeneration: 1,
		PlanMd: pgtype.Text{String: c.PlanMd, Valid: true}, Milestones: c.Milestones,
		RequiredCapabilities: c.RequiredCapabilities, RequiredTools: c.RequiredTools,
		SizeClass:       pgtype.Text{String: c.SizeClass, Valid: true},
		BaseCommit:      pgtype.Text{String: c.BaseCommit, Valid: true},
		PlanningDiff:    pgtype.Text{String: c.PlanningDiff, Valid: true},
		CandidateDigest: digest, Verdict: "approve",
		DecidedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}
	reader := &policyIdentityReader{
		planning: store.GetSemanticPlanningRow{Enrollment: semanticJSON(t, apitypes.SemanticEnrollment{Profile: "semantic"})},
		history:  map[string][]byte{immutable.Digest: semanticJSON(t, immutable)},
	}
	return cc, immutable, reader
}

// This transaction fake exercises the existing source witness seam through
// sqlc's read interface, including error propagation back to its caller.
type policyIdentityTx struct {
	pgx.Tx
	t      *testing.T
	cc     store.CrossCheck
	reader *policyIdentityReader
}

type policyIdentityRow struct {
	value any
	err   error
}

func (r policyIdentityRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	v := reflect.ValueOf(r.value)
	if v.Kind() != reflect.Struct {
		reflect.ValueOf(dest[0]).Elem().Set(v)
		return nil
	}
	if len(dest) != v.NumField() {
		return errors.New("unexpected scan shape")
	}
	for i := range dest {
		reflect.ValueOf(dest[i]).Elem().Set(v.Field(i))
	}
	return nil
}

func (tx *policyIdentityTx) QueryRow(ctx context.Context, query string, args ...any) pgx.Row {
	switch {
	case strings.HasPrefix(query, "-- name: GetPolicyStopCrossCheck :one"):
		if args[0] != tx.cc.ID {
			tx.t.Fatal("cross-check lookup changed identity")
		}
		return policyIdentityRow{value: tx.cc}
	case strings.HasPrefix(query, "-- name: GetSemanticPlanning :one"):
		row, err := tx.reader.GetSemanticPlanning(ctx, args[0].(uuid.UUID))
		return policyIdentityRow{value: row, err: err}
	case strings.HasPrefix(query, "-- name: GetSemanticCandidateByDigest :one"):
		raw, err := tx.reader.GetSemanticCandidateByDigest(ctx, store.GetSemanticCandidateByDigestParams{
			RunID: args[0].(uuid.UUID), Column2: args[1].(string),
		})
		return policyIdentityRow{value: raw, err: err}
	default:
		tx.t.Fatal("unexpected transaction query")
		return policyIdentityRow{err: errors.New("unexpected transaction query")}
	}
}

func TestPolicyRevisionIdentityWitness(t *testing.T) {
	dbErr := errors.New("storage unavailable")
	for _, name := range []string{
		"structural", "semantic", "historical_distinct_from_latest", "missing_history", "mismatched_history",
		"changed_plan", "changed_milestones", "changed_capabilities", "changed_tools", "changed_size", "changed_base", "changed_diff",
		"malformed_structure", "malformed_enrollment", "wrong_profile", "malformed_history", "invalid_history_version",
		"invalid_history_revision", "changed_criterion", "structural_digest_with_semantic_enrollment",
		"semantic_digest_without_enrollment", "planning_database_error", "history_database_error",
	} {
		t.Run(name, func(t *testing.T) {
			cc, immutable, reader := policyIdentityFixture(t)
			want := false
			var wantErr error
			switch name {
			case "structural":
				c, _, _ := semanticFixture()
				var err error
				cc.CandidateDigest, err = c.Digest()
				if err != nil {
					t.Fatal(err)
				}
				reader.planningErr = pgx.ErrNoRows
				want = true
			case "semantic":
				want = true
			case "historical_distinct_from_latest":
				latest := immutable
				latest.Revision++
				digest, err := semanticCheckerDigest(PlanCrossCheckCandidate{PlanMd: cc.PlanMd.String, Milestones: cc.Milestones}, latest)
				if err != nil {
					t.Fatal(err)
				}
				latest.Digest = hex.EncodeToString(digest)
				reader.planning.Candidate = semanticJSON(t, latest)
				reader.history[latest.Digest] = semanticJSON(t, latest)
				want = true
			case "missing_history":
				reader.planning.Candidate = reader.history[immutable.Digest]
				delete(reader.history, immutable.Digest)
			case "mismatched_history":
				immutable.Digest = hex.EncodeToString(make([]byte, 32))
				reader.history[hex.EncodeToString(cc.CandidateDigest)] = semanticJSON(t, immutable)
			case "changed_plan":
				cc.PlanMd.String += "changed"
			case "changed_milestones":
				cc.Milestones = []byte(`[{"id":"m2","title":"Changed"}]`)
			case "changed_capabilities":
				cc.RequiredCapabilities = []string{"network"}
			case "changed_tools":
				cc.RequiredTools = []string{"git"}
			case "changed_size":
				cc.SizeClass.String = "l"
			case "changed_base":
				cc.BaseCommit.String = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			case "changed_diff":
				cc.PlanningDiff.String = "changed"
			case "malformed_structure":
				cc.Milestones = []byte("{")
			case "malformed_enrollment":
				reader.planning.Enrollment = []byte(`{"profile":"semantic","unknown":true}`)
			case "wrong_profile":
				reader.planning.Enrollment = []byte(`{"profile":"structural"}`)
			case "malformed_history":
				reader.history[immutable.Digest] = []byte("{")
			case "invalid_history_version":
				immutable.CandidateIdentityVersion = "unknown"
				reader.history[immutable.Digest] = semanticJSON(t, immutable)
			case "invalid_history_revision":
				immutable.Revision = 0
				reader.history[immutable.Digest] = semanticJSON(t, immutable)
			case "changed_criterion":
				immutable.Criteria[0].DoneWhen += "changed"
				reader.history[immutable.Digest] = semanticJSON(t, immutable)
			case "structural_digest_with_semantic_enrollment":
				c, _, _ := semanticFixture()
				cc.CandidateDigest, _ = c.Digest()
				reader.history[hex.EncodeToString(cc.CandidateDigest)] = semanticJSON(t, immutable)
			case "semantic_digest_without_enrollment":
				reader.planningErr = pgx.ErrNoRows
			case "planning_database_error":
				reader.planningErr, wantErr = dbErr, dbErr
			case "history_database_error":
				reader.historyErr, wantErr = dbErr, dbErr
			}
			id := cc.ID.String()
			tx := &policyIdentityTx{t: t, cc: cc, reader: reader}
			got, err := policyRevisionWitness(context.Background(), tx,
				store.Run{ID: cc.LeadRunID, ClaimGeneration: 1}, store.Worker{},
				apitypes.PolicyRevisionRef{Source: "plan_cross_check", CrossCheckID: &id}, false, false)
			if got != want || !errors.Is(err, wantErr) {
				t.Fatalf("valid=%v err=%v; want valid=%v err=%v", got, err, want, wantErr)
			}
			if len(reader.runIDs) != 1 || reader.runIDs[0] != cc.LeadRunID {
				t.Fatal("enrollment not read by witness lead")
			}
			if reader.planningErr != nil || name == "malformed_enrollment" || name == "wrong_profile" {
				if len(reader.lookups) != 0 {
					t.Fatal("unexpected history read")
				}
			} else if len(reader.lookups) != 1 || reader.lookups[0].RunID != cc.LeadRunID || reader.lookups[0].Column2 != hex.EncodeToString(cc.CandidateDigest) {
				t.Fatal("history lookup must use exact witness identity once")
			}
		})
	}
}

// Deliberately lacks semantic reads, like legacy approval Store fakes.
type policyIdentityApprovalReader struct{ cc store.CrossCheck }

func (r *policyIdentityApprovalReader) ListPolicyStopRevisionInputs(context.Context, uuid.UUID) ([]store.RunUserInput, error) {
	return nil, nil
}
func (r *policyIdentityApprovalReader) GetPolicyStopCrossCheck(_ context.Context, id uuid.UUID) (store.CrossCheck, error) {
	if id != r.cc.ID {
		return store.CrossCheck{}, pgx.ErrNoRows
	}
	return r.cc, nil
}
func (*policyIdentityApprovalReader) HasPolicyCurrentGateApproval(context.Context, store.HasPolicyCurrentGateApprovalParams) (bool, error) {
	return true, nil
}

type policyIdentitySemanticApprovalReader struct {
	*policyIdentityApprovalReader
	*policyIdentityReader
}

func TestPolicyRevisionIdentityApproval(t *testing.T) {
	for _, name := range []string{"legacy_structural", "legacy_semantic_refused", "semantic_history", "missing_history", "database_error"} {
		t.Run(name, func(t *testing.T) {
			cc, candidate, identity := policyIdentityFixture(t)
			want := name == "legacy_structural" || name == "semantic_history"
			if name == "legacy_structural" {
				c, _, _ := semanticFixture()
				cc.CandidateDigest, _ = c.Digest()
			}
			if name == "missing_history" {
				identity.history = nil
			}
			if name == "database_error" {
				identity.historyErr = errors.New("storage unavailable")
			}
			legacy := &policyIdentityApprovalReader{cc: cc}
			var reader any = legacy
			if name != "legacy_structural" && name != "legacy_semantic_refused" {
				reader = &policyIdentitySemanticApprovalReader{legacy, identity}
			}
			id := cc.ID.String()
			r := store.Run{
				ID: cc.LeadRunID, Kind: "issue", Harness: "codex", PolicyRefusalCount: 1, ClaimGeneration: 1,
				PlanMd: cc.PlanMd, PlanSource: planSourceSeeded, MilestonesFrozen: cc.Milestones,
				PolicyCandidateCrossCheckID: pgtype.UUID{Bytes: cc.ID, Valid: true},
				PolicyRevisionSource: semanticJSON(t, apitypes.PolicyRevisionSourceV1{
					Version: 1, AssociatedCrossChecks: []apitypes.PolicyRevisionRef{{Source: "plan_cross_check", CrossCheckID: &id}},
				}),
			}
			if name != "legacy_structural" {
				presentation := semanticGateProjection(&semanticCoverageTx{candidate: candidate}, r)
				var err error
				r.CompletionContract, err = semanticGateContract(&presentation, nil)
				if err != nil {
					t.Fatal(err)
				}
				r.CompletionContractVersion = pgtype.Int4{Int32: 1, Valid: true}
				r.ContractRevision = pgtype.Int4{Int32: 1, Valid: true}
			}
			if got := policyRevisionPlanApproved(context.Background(), reader, r, true); got != want {
				t.Fatalf("approved=%v; want %v", got, want)
			}
		})
	}
}
