package workersvc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

const policyMaxSafeGeneration int64 = 9007199254740991

var policyCorrelation = regexp.MustCompile(`^pr-[0-9a-f]{32}-[1-9][0-9]{0,15}$`)
var policyHead = regexp.MustCompile(`^[0-9a-f]{40,64}$`)

// PolicyHoldRequest contains only typed observation and preservation evidence.
// It carries no execution authority or provider response text.
type PolicyHoldRequest struct {
	OutcomeID                string  `json:"outcome_id"`
	SourceGeneration         int64   `json:"source_generation"`
	ClaimGeneration          int64   `json:"claim_generation"`
	Detail                   string  `json:"detail"`
	ContinuationMode         string  `json:"continuation_mode"`
	PolicyTag                *string `json:"tag,omitempty"`
	Phase                    *string `json:"phase,omitempty"`
	CorrelationID            *string `json:"correlation_id,omitempty"`
	SessionID                *string `json:"session_id,omitempty"`
	Head                     *string `json:"head,omitempty"`
	Published                bool    `json:"published"`
	CheckpointContainsLatest *bool   `json:"checkpoint_contains_latest,omitempty"`
}

// UnmarshalJSON keeps the worker wire contract closed even for JSON null, which
// encoding/json otherwise silently maps to zero/nil fields. The handler owns the
// complete 17 KiB body bound before invoking this decoder.
func (r *PolicyHoldRequest) UnmarshalJSON(data []byte) error {
	type wire PolicyHoldRequest
	var decoded wire
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	// Reuse the existing validated-JSON scanner to detect lossy Unicode/NUL
	// decoding. Policy identity must be rejected, never rewritten by sanitation.
	if _, repairs := sanitizePayloadJSON(data); repairs.any() {
		return fmt.Errorf("%w: invalid policy string encoding", ErrInvalidState)
	}
	for key := range fields {
		switch key {
		case "outcome_id", "source_generation", "claim_generation", "detail", "continuation_mode", "tag", "phase", "correlation_id", "session_id", "head", "published", "checkpoint_contains_latest":
		default:
			return fmt.Errorf("%w: unknown policy field", ErrInvalidState)
		}
	}
	if _, ok := fields["published"]; !ok {
		return fmt.Errorf("%w: published required", ErrInvalidState)
	}
	for _, raw := range fields {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return fmt.Errorf("%w: null policy field", ErrInvalidState)
		}
	}
	*r = PolicyHoldRequest(decoded)
	return nil
}

// PolicyHoldReply uses pending for every unresolved delivery, including unknown
// ownership; absence is never evidence of a terminal outcome.
type PolicyHoldReply struct {
	Disposition        string `json:"disposition"`
	OutcomeID          string `json:"outcome_id"`
	SourceGeneration   int64  `json:"source_generation"`
	ObservedStatus     string `json:"observed_status,omitempty"`
	HoldReason         string `json:"hold_reason,omitempty"`
	ObservedGeneration int64  `json:"observed_generation,omitempty"`
	ApplyingGeneration int64  `json:"applying_generation,omitempty"`
	Reason             string `json:"reason,omitempty"`
}

func (r PolicyHoldRequest) Validate() error {
	id, err := uuid.Parse(r.OutcomeID)
	if err != nil || id == uuid.Nil || id.String() != r.OutcomeID {
		return fmt.Errorf("%w: canonical nonzero outcome_id required", ErrInvalidState)
	}
	if r.SourceGeneration <= 0 || r.ClaimGeneration < r.SourceGeneration || r.ClaimGeneration > policyMaxSafeGeneration {
		return fmt.Errorf("%w: invalid policy generations", ErrInvalidState)
	}
	if r.Detail != "refused" && r.Detail != "resume_rejected" {
		return fmt.Errorf("%w: invalid policy detail", ErrInvalidState)
	}
	if r.ContinuationMode != "same_conversation" && r.ContinuationMode != "fresh_from_saved_work" {
		return fmt.Errorf("%w: invalid continuation mode", ErrInvalidState)
	}
	if r.Detail == "refused" && (r.PolicyTag == nil || r.Phase == nil || r.CorrelationID == nil) {
		return fmt.Errorf("%w: refusal metadata required", ErrInvalidState)
	}
	if r.PolicyTag != nil && *r.PolicyTag != "cyberPolicy" && *r.PolicyTag != "misalignmentPolicyViolation" {
		return fmt.Errorf("%w: invalid policy tag", ErrInvalidState)
	}
	if r.Phase != nil && *r.Phase != "planning" && *r.Phase != "implementation" {
		return fmt.Errorf("%w: invalid policy phase", ErrInvalidState)
	}
	if r.CorrelationID != nil {
		v := *r.CorrelationID
		if len(v) < 37 || len(v) > 52 || !policyCorrelation.MatchString(v) {
			return fmt.Errorf("%w: invalid policy correlation", ErrInvalidState)
		}
		n, e := strconv.ParseInt(v[36:], 10, 64)
		if e != nil || n > policyMaxSafeGeneration {
			return fmt.Errorf("%w: invalid policy correlation counter", ErrInvalidState)
		}
	}
	if r.SessionID != nil && (!utf8.ValidString(*r.SessionID) || len(*r.SessionID) > 1024 || strings.ContainsRune(*r.SessionID, 0)) {
		return fmt.Errorf("%w: invalid session_id", ErrInvalidState)
	}
	if r.Head != nil && *r.Head != "" && (!policyHead.MatchString(*r.Head) || (len(*r.Head) != 40 && len(*r.Head) != 64)) {
		return fmt.Errorf("%w: invalid captured head", ErrInvalidState)
	}
	if r.CheckpointContainsLatest != nil && *r.CheckpointContainsLatest && !r.Published {
		return fmt.Errorf("%w: latest checkpoint requires publication", ErrInvalidState)
	}
	if (r.Published || (r.CheckpointContainsLatest != nil && *r.CheckpointContainsLatest)) && (r.Head == nil || *r.Head == "") {
		return fmt.Errorf("%w: published evidence requires a head", ErrInvalidState)
	}
	return nil
}

func activePolicyHold(r store.Run) bool {
	return r.PolicyPreserveRequired || r.PolicyPendingFresh ||
		(r.PolicyOutcomeID.Valid && !r.PolicyResolvedAt.Valid)
}

// ReportPolicyHold owns a worker/run transaction. Reconciliation precedes state
// mutation so lost ACK replay cannot undo an intervening owner action.
func (s *Service) ReportPolicyHold(ctx context.Context, worker store.Worker, runID uuid.UUID, req PolicyHoldRequest) (PolicyHoldReply, error) {
	reply := PolicyHoldReply{Disposition: "pending", OutcomeID: req.OutcomeID, SourceGeneration: req.SourceGeneration}
	if err := req.Validate(); err != nil {
		return reply, err
	}
	pending := func(reason string) (PolicyHoldReply, error) { reply.Reason = reason; return reply, nil }
	if s.txBeginner == nil {
		return reply, fmt.Errorf("policy hold requires transaction support")
	}
	tx, err := s.beginReadCommitted(ctx)
	if err != nil {
		return reply, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := store.New(tx)
	currentWorker, err := q.GetWorkerForUpdate(ctx, worker.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return pending("worker_unavailable")
	}
	if err != nil {
		return reply, err
	}
	if currentWorker.UserID != worker.UserID || !slices.Contains(currentWorker.ProtocolCapabilities, capability.ProviderPolicyHoldV1) {
		return pending("worker_not_capable")
	}
	r, err := q.GetRunByIDForUpdate(ctx, runID)
	if errors.Is(err, pgx.ErrNoRows) {
		return pending("ownership_unconfirmed")
	}
	if err != nil {
		return reply, err
	}
	if r.UserID != currentWorker.UserID || laneMismatch(runID, r.EgressProfileID.Valid, currentWorker) {
		return pending("ownership_unconfirmed")
	}
	id, _ := uuid.Parse(req.OutcomeID)
	currentOwned := r.WorkerID.Valid && uuid.UUID(r.WorkerID.Bytes) == worker.ID
	// Conversion records the applying generation without rewriting the original
	// journal. Either envelope may reconcile this exact outcome; only currentOwned
	// can reach the mutation fences below.
	recordedOwned := r.PolicyApplyingWorkerID.Valid && uuid.UUID(r.PolicyApplyingWorkerID.Bytes) == worker.ID &&
		r.PolicyApplyingGeneration.Valid &&
		(r.PolicyApplyingGeneration.Int64 == req.ClaimGeneration || req.ClaimGeneration == req.SourceGeneration) &&
		r.PolicyOutcomeID.Valid && uuid.UUID(r.PolicyOutcomeID.Bytes) == id &&
		r.PolicySourceGeneration.Valid && r.PolicySourceGeneration.Int64 == req.SourceGeneration
	if !currentOwned && !recordedOwned {
		return pending("ownership_unconfirmed")
	}
	reply.ObservedStatus = r.Status
	reply.HoldReason = r.HoldReason.String
	reply.ObservedGeneration = r.ClaimGeneration
	// Positive recorded identity is reconciliation evidence, never execution authority.
	if r.PolicyOutcomeID.Valid && uuid.UUID(r.PolicyOutcomeID.Bytes) == id {
		if !r.PolicySourceGeneration.Valid || r.PolicySourceGeneration.Int64 != req.SourceGeneration {
			return pending("identity_conflict")
		}
		reply.Disposition = "already_applied"
		return reply, nil
	}
	if !currentOwned {
		return pending("ownership_unconfirmed")
	}
	if slices.Contains([]string{"completed", "failed", "cancelled"}, r.Status) {
		// The locked row is still owned by this authenticated worker/user.
		reply.Disposition = "terminal"
		return reply, nil
	}
	if r.PolicyApplyingGeneration.Valid && r.PolicyApplyingGeneration.Int64 > req.SourceGeneration {
		reply.ApplyingGeneration = r.PolicyApplyingGeneration.Int64
		reply.Disposition = "resolved_newer_policy"
		return reply, nil
	}
	// Unlike beginClaimMutation's reconciliation-only fence this is unconditional.
	if r.ClaimReleasedAt.Valid || r.ClaimGeneration != req.ClaimGeneration {
		return pending("stale_claim")
	}
	if r.StopKind.Valid && r.StopKind.String != "" && r.StopKind.String != "cancelled" {
		return pending("owner_disposition_pending")
	}
	if r.StopKind.Valid && r.StopKind.String == "cancelled" {
		n, e := q.CancelRunByWorker(ctx, store.CancelRunByWorkerParams{ID: runID, WorkerID: pgconv.UUID(worker.ID)})
		if e != nil {
			return reply, e
		}
		if n != 1 {
			return pending("cancel_pending")
		}
		if e = tx.Commit(ctx); e != nil {
			return reply, e
		}
		reply.Disposition = "terminal"
		reply.ObservedStatus = "cancelled"
		return reply, nil
	}
	if !slices.Contains([]string{"issue", "task", "prompt", "self_improve"}, r.Kind) || r.Interactive || r.Harness != "codex" {
		return pending("ineligible_lane")
	}
	if r.Status != "claimed" && r.Status != "running" {
		return pending("ineligible_status")
	}
	if r.PolicyApplyingGeneration.Valid && r.PolicyApplyingGeneration.Int64 >= req.ClaimGeneration {
		return pending("generation_already_used")
	}
	// An older observation may only re-hold under independently current authority;
	// it cannot count as another actual refusal.
	if req.Detail == "refused" && req.SourceGeneration != req.ClaimGeneration {
		return pending("conversion_required")
	}
	actual := req.Detail == "refused"
	held, err := q.ApplyPolicyHold(ctx, store.ApplyPolicyHoldParams{
		ID: runID, UserID: worker.UserID, WorkerID: pgconv.UUID(worker.ID),
		OutcomeID: pgconv.UUID(id), ClaimGeneration: pgconv.Int8Ptr(&req.ClaimGeneration), SourceGeneration: pgconv.Int8Ptr(&req.SourceGeneration),
		ContinuationMode: pgconv.TextPtr(&req.ContinuationMode), ActualRefusal: actual,
		PolicyTag: pgconv.TextPtr(req.PolicyTag), Phase: pgconv.TextPtr(req.Phase), CorrelationID: pgconv.TextPtr(req.CorrelationID),
		SessionID: pgconv.TextPtr(req.SessionID), Head: pgconv.TextPtr(req.Head),
		Published: req.Published, CheckpointContainsLatest: pgconv.BoolPtr(req.CheckpointContainsLatest),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return pending("transition_refused")
	}
	if err != nil {
		return reply, err
	}
	if err = tx.Commit(ctx); err != nil {
		return reply, err
	}
	reply.Disposition = "applied"
	reply.ObservedStatus = held.Status
	reply.HoldReason = held.HoldReason.String
	reply.ObservedGeneration = held.ClaimGeneration
	if s.bcast != nil {
		s.bcast.PublishState(runID, held.Status)
	}
	s.notify(runID, held.Status)
	return reply, nil
}
