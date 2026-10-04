package workersvc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"

	"github.com/google/uuid"
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

type StateReportResult struct {
	Run            store.Run           `json:"-"`
	Applied        bool                `json:"-"`
	GateRevision   int64               `json:"-"`
	Reconciliation *LeadReconciliation `json:"-"`
}

// PlanCrossCheckStatusResult keeps the existing active candidate internal.
// Parked responses have no candidate, raw run, findings, or presentation body.
type PlanCrossCheckStatusResult struct {
	CrossCheck     store.CrossCheck    `json:"-"`
	LeadLastSeq    int32               `json:"-"`
	Parked         bool                `json:"-"`
	Verdict        string              `json:"-"`
	ReasonClass    string              `json:"-"`
	Reconciliation *LeadReconciliation `json:"-"`
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
	if lead.UserID != worker.UserID || lead.ClaimGeneration != generation || lead.ClaimReleasedAt.Valid || !lead.PlanCrossCheckRequired {
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
	run, applied, err := s.setState(ctx, worker, runID, req, &result.GateRevision, &result.Reconciliation)
	if err != nil {
		return StateReportResult{Run: run}, err
	}
	result.Run, result.Applied = run, applied
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
