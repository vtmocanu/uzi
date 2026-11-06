package workersvc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// LeadReconciliation is a bounded snapshot captured under the owning lead lock.
// CurrentPlanSHA256 hashes current plan_md bytes, not a new stored candidate pin.
// GatePayloadDigest identifies the immutable presentation without exposing its body.
type LeadReconciliation struct {
	LeadLastSeq           int32      `json:"lead_last_seq"`
	ClaimGeneration       int64      `json:"claim_generation"`
	PlanCrossCheckSettled bool       `json:"plan_cross_check_settled"`
	GatePresentationID    *uuid.UUID `json:"gate_presentation_id,omitempty"`
	GateRevision          int64      `json:"gate_revision"`
	GatePayloadDigest     string     `json:"gate_payload_digest,omitempty"`
	CurrentPlanSHA256     string     `json:"current_plan_sha256"`
}

// Registration evidence ACK values (issue #3859): "recorded" when RecordRunRegistrationEvidence
// affected the run, "refused" when it matched nothing (wrong nonce or generation, released claim,
// another worker's run, or a status outside claimed/running). Empty when the request carried none.
const (
	RegistrationEvidenceRecorded = "recorded"
	RegistrationEvidenceRefused  = "refused"
)

// RegistrationEvidence is the nested wire object of StateRequest.RegistrationEvidence.
// RegistrationEpoch is a pointer so an absent epoch is a 400 rather than a silent zero.
type RegistrationEvidence struct {
	RegisterNonce     string `json:"register_nonce"`
	ClaimGeneration   int64  `json:"claim_generation"`
	RegistrationEpoch *int64 `json:"registration_epoch"`
}

// ErrInvalidRegistrationEvidence is the 400 for a malformed registration_evidence object or one
// sent on a non-running report. Nothing was written.
var ErrInvalidRegistrationEvidence = errors.New("invalid registration_evidence")

func validateRegistrationEvidence(req StateRequest) error {
	ev := req.RegistrationEvidence
	if ev == nil {
		return nil
	}
	switch {
	case req.State != "running":
		return fmt.Errorf("%w: only valid on a running report", ErrInvalidRegistrationEvidence)
	case ev.RegisterNonce == "":
		return fmt.Errorf("%w: register_nonce is required", ErrInvalidRegistrationEvidence)
	case ev.ClaimGeneration < 1:
		return fmt.Errorf("%w: claim_generation must be at least 1", ErrInvalidRegistrationEvidence)
	case ev.RegistrationEpoch == nil || *ev.RegistrationEpoch < 0:
		return fmt.Errorf("%w: registration_epoch must be present and non-negative", ErrInvalidRegistrationEvidence)
	}
	return nil
}

// recordRegistrationEvidence runs the independent evidence write and returns the ACK value.
func (s *Service) recordRegistrationEvidence(ctx context.Context, worker store.Worker, runID uuid.UUID, req StateRequest) (string, error) {
	ev := req.RegistrationEvidence
	if ev == nil {
		return "", nil
	}
	rows, err := s.q.RecordRunRegistrationEvidence(ctx, store.RecordRunRegistrationEvidenceParams{
		ID: runID, WorkerID: pgconv.UUID(worker.ID),
		Generation:        pgtype.Int8{Int64: ev.ClaimGeneration, Valid: true},
		RegisterNonce:     pgtype.Text{String: ev.RegisterNonce, Valid: true},
		RegistrationEpoch: pgtype.Int8{Int64: *ev.RegistrationEpoch, Valid: true},
	})
	if err != nil {
		return "", err
	}
	if rows == 1 {
		return RegistrationEvidenceRecorded, nil
	}
	return RegistrationEvidenceRefused, nil
}

type StateReportResult struct {
	RegistrationEvidence        string                                `json:"-"`
	RevisionInputID             *int64                                `json:"-"`
	SupersededRevisionInputIDs  []int64                               `json:"-"`
	CompletedPublicationReceipt *apitypes.CompletedPublicationReceipt `json:"-"`
	CompletedPublicationReason  string                                `json:"-"`
	Run                         store.Run                             `json:"-"`
	Applied                     bool                                  `json:"-"`
	GateRevision                int64                                 `json:"-"`
	Reconciliation              *LeadReconciliation                   `json:"-"`
}

// PlanCrossCheckStatusResult keeps the existing active candidate internal.
// Parked responses have no candidate, raw run, findings, or presentation body.
type PlanCrossCheckStatusResult struct {
	Candidate      *PlanCrossCheckCandidate `json:"-"`
	CrossCheck     store.CrossCheck         `json:"-"`
	LeadLastSeq    int32                    `json:"-"`
	Parked         bool                     `json:"-"`
	Verdict        string                   `json:"-"`
	ReasonClass    string                   `json:"-"`
	Reconciliation *LeadReconciliation      `json:"-"`
}

type reconciliationReader interface {
	GetRunOwnedByWorkerForUpdate(context.Context, store.GetRunOwnedByWorkerForUpdateParams) (store.Run, error)
	HasPendingPlanCrossCheck(context.Context, uuid.UUID) (bool, error)
}

// captureLeadReconciliation must use the transaction already holding the lead lock.
// Pending includes expired and historical generations: absence, not liveness, proves settlement.
func captureLeadReconciliation(ctx context.Context, q reconciliationReader, worker store.Worker, leadID uuid.UUID, generation int64) (*LeadReconciliation, error) {
	lead, err := q.GetRunOwnedByWorkerForUpdate(ctx, store.GetRunOwnedByWorkerForUpdateParams{ID: leadID, WorkerID: pgconv.UUID(worker.ID)})
	if err != nil {
		return nil, err
	}
	if lead.UserID != worker.UserID || lead.ClaimGeneration != generation || lead.ClaimReleasedAt.Valid || (!lead.PlanCrossCheckRequired && !lead.PlanCrossCheckGated) {
		return nil, ErrCrossCheckRefused
	}
	pending, err := q.HasPendingPlanCrossCheck(ctx, leadID)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(lead.PlanMd.String))
	proof := &LeadReconciliation{
		LeadLastSeq: lead.LastSeq, ClaimGeneration: lead.ClaimGeneration,
		PlanCrossCheckSettled: !pending, GateRevision: lead.GateRevision,
		GatePayloadDigest: hex.EncodeToString(lead.GatePayloadDigest),
		CurrentPlanSHA256: hex.EncodeToString(sum[:]),
	}
	if lead.GatePresentationID.Valid {
		id := uuid.UUID(lead.GatePresentationID.Bytes)
		proof.GatePresentationID = &id
	}
	return proof, nil
}

// SetStateReportWithReconciliation exposes proof only after the state transaction commits.
func (s *Service) SetStateReportWithReconciliation(ctx context.Context, worker store.Worker, runID uuid.UUID, req StateRequest) (StateReportResult, error) {
	var result StateReportResult
	if err := validateRegistrationEvidence(req); err != nil {
		return result, err
	}
	if err := validateReportOnlyShape(req); err != nil {
		return result, err
	}
	if err := validateCompletedPublicationReport(worker, req); err != nil {
		return result, err
	}
	if err := validateSourcePublicationRequest(req); err != nil {
		return result, err
	}
	if replay, err := s.replaySourcePublication(ctx, worker, runID, req); replay != nil || err != nil {
		if replay != nil {
			return *replay, err
		}
		return result, err
	}
	if replay, err := s.replayCompletedPublication(ctx, worker, runID, req); replay != nil || err != nil {
		if replay != nil {
			return *replay, err
		}
		return result, err
	}
	// Issue #3859: the witness is written first and does not depend on the status write below.
	evidence, err := s.recordRegistrationEvidence(ctx, worker, runID, req)
	if err != nil {
		return result, err
	}
	result.RegistrationEvidence = evidence
	run, applied, err := s.setState(ctx, worker, runID, req, &result.GateRevision, &result.Reconciliation)
	if err != nil {
		return StateReportResult{Run: run, RegistrationEvidence: evidence}, err
	}
	result.Run, result.Applied = run, applied
	if applied && result.GateRevision > 0 && req.RevisionInputID != nil {
		result.RevisionInputID = req.RevisionInputID
		result.SupersededRevisionInputIDs = req.SupersededRevisionInputIDs
	}
	if req.State == "completed" && applied && completedPublicationCapable(worker, req) {
		result.CompletedPublicationReceipt, result.CompletedPublicationReason = s.verifyCompletedPublication(ctx, worker, runID, req)
	}
	return result, nil
}

func (s *Service) PlanCrossCheckStatusWithReconciliation(ctx context.Context, worker store.Worker, leadID uuid.UUID, generation int64, round int32) (PlanCrossCheckStatusResult, error) {
	var result PlanCrossCheckStatusResult
	cc, seq, err := s.planCrossCheckStatus(ctx, worker, leadID, generation, round, &result)
	result.LeadLastSeq = seq
	if !result.Parked {
		result.CrossCheck = cc
	}
	result.Verdict, result.ReasonClass = cc.Verdict, cc.ReasonClass.String
	return result, err
}
