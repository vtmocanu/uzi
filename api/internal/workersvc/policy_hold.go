package workersvc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PolicyHoldFeature negotiates the completed policy-hold owner contract.
const PolicyHoldFeature = "provider_policy_hold"

// PolicyResumeGuidanceMaxBytes bounds decoded owner guidance.
const PolicyResumeGuidanceMaxBytes = 16000

type PolicyHoldRequest struct {
	OutcomeID                string  `json:"outcome_id"`
	SourceGeneration         int64   `json:"source_generation"`
	ClaimGeneration          int64   `json:"claim_generation"`
	Detail                   string  `json:"detail"`
	Tag                      *string `json:"tag,omitempty"`
	Phase                    *string `json:"phase,omitempty"`
	CorrelationID            *string `json:"correlation_id,omitempty"`
	SessionID                *string `json:"session_id,omitempty"`
	Head                     string  `json:"head"`
	Published                bool    `json:"published"`
	CheckpointContainsLatest *bool   `json:"checkpoint_contains_latest,omitempty"`
	ExpectedGateRevision     *int64  `json:"expected_gate_revision,omitempty"`
}

type PolicyHoldDisposition string

const (
	PolicyHoldApplied                          PolicyHoldDisposition = "applied"
	PolicyHoldAlreadyApplied                   PolicyHoldDisposition = "already_applied"
	PolicyHoldResolvedNewer                    PolicyHoldDisposition = "resolved_newer_policy"
	PolicyHoldTerminal                         PolicyHoldDisposition = "terminal"
	PolicyHoldPending                          PolicyHoldDisposition = "pending"
	PolicyHoldRecorded                         PolicyHoldDisposition = "recorded"
	PolicyHoldUnauthenticatedHistoricalOutcome PolicyHoldDisposition = "unauthenticated_historical_outcome"
)

var policyCorrelation = regexp.MustCompile(`^pr-[0-9a-f]{32}-[1-9][0-9]{0,15}$`)

func ValidPolicyCorrelation(v string) bool {
	if !policyCorrelation.MatchString(v) {
		return false
	}
	n, err := strconv.ParseUint(v[36:], 10, 64)
	return err == nil && n <= 9007199254740991
}

func (r PolicyHoldRequest) validate() error {
	id, err := uuid.Parse(r.OutcomeID)
	if err != nil || id == uuid.Nil || id.String() != r.OutcomeID ||
		r.SourceGeneration <= 0 || r.ClaimGeneration <= 0 || r.SourceGeneration > r.ClaimGeneration ||
		(r.Detail != "refused" && r.Detail != "resume_rejected") {
		return ErrInvalidState
	}
	if r.Detail == "refused" && (r.Tag == nil || r.Phase == nil || r.CorrelationID == nil) {
		return ErrInvalidState
	}
	if r.Tag != nil && *r.Tag != "cyberPolicy" && *r.Tag != "misalignmentPolicyViolation" {
		return ErrInvalidState
	}
	if r.Phase != nil && *r.Phase != "planning" && *r.Phase != "implementation" {
		return ErrInvalidState
	}
	if r.CorrelationID != nil && !ValidPolicyCorrelation(*r.CorrelationID) {
		return ErrInvalidState
	}
	if r.ExpectedGateRevision != nil && *r.ExpectedGateRevision <= 0 {
		return ErrInvalidState
	}
	if r.Head != "" {
		if len(r.Head) != 40 && len(r.Head) != 64 {
			return ErrInvalidState
		}
		for _, c := range r.Head {
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
				return ErrInvalidState
			}
		}
	}
	if r.Published && r.Head == "" {
		return ErrInvalidState
	}
	if r.SessionID != nil && (strings.TrimSpace(*r.SessionID) == "" || len(*r.SessionID) > 512 || strings.ContainsAny(*r.SessionID, "\x00\r\n")) {
		return ErrInvalidState
	}
	return nil
}

func policyEligible(r store.Run) bool {
	return r.Harness == "codex" && !r.Interactive && slices.Contains([]string{"issue", "task", "prompt", "self_improve"}, r.Kind)
}

// ReportPolicyHold authorizes current delivery by its claim, or historical delivery
// by its original custody, before reconciling immutable outcome evidence.
func (s *Service) ReportPolicyHold(ctx context.Context, w store.Worker, id uuid.UUID, req PolicyHoldRequest) (store.Run, PolicyHoldDisposition, error) {
	if err := req.validate(); err != nil {
		return store.Run{}, PolicyHoldPending, err
	}
	if !s.p.PolicyHoldNegotiated || !s.p.PolicyHoldSnapshotEnabled || s.p.TerminalPendingLease <= 0 ||
		!slices.Contains(w.ProtocolCapabilities, capability.ProviderPolicyHoldV1) ||
		!slices.Contains(w.ProtocolCapabilities, capability.CredentialSwitchV1) {
		return store.Run{}, PolicyHoldPending, ErrInvalidState
	}
	if s.txBeginner == nil {
		return store.Run{}, PolicyHoldPending, fmt.Errorf("policy hold requires transaction")
	}
	tx, err := s.beginReadCommitted(ctx)
	if err != nil {
		return store.Run{}, PolicyHoldPending, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := store.New(tx)
	if _, err = q.GetWorkerForUpdate(ctx, w.ID); err != nil {
		return store.Run{}, PolicyHoldPending, err
	}
	r, err := q.GetRunByIDForUpdate(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.Run{}, PolicyHoldPending, ErrRunNotOwned
	}
	if err != nil {
		return r, PolicyHoldPending, err
	}
	if r.UserID != w.UserID {
		return store.Run{}, PolicyHoldPending, ErrRunNotOwned
	}
	if !policyEligible(r) {
		return r, PolicyHoldPending, ErrInvalidState
	}
	if req.ClaimGeneration > r.ClaimGeneration {
		return store.Run{}, PolicyHoldPending, ErrRunNotOwned
	}
	owned := r.WorkerID.Valid && r.WorkerID.Bytes == w.ID && r.ClaimGeneration == req.ClaimGeneration && !r.ClaimReleasedAt.Valid
	budgetPark := r.WorkerID.Valid && r.WorkerID.Bytes == w.ID &&
		r.ClaimGeneration == req.ClaimGeneration && req.SourceGeneration == r.ClaimGeneration &&
		r.ClaimReleasedAt.Valid && r.ReleasedWorkerID.Valid && r.ReleasedWorkerID.Bytes == w.ID &&
		r.Status == "paused" && r.HoldReason.Valid && r.HoldReason.String == "budget_exhausted"
	if !owned && !budgetPark {
		// ClaimRun clears released_worker_*; only original custody authenticates
		// an old delivery after an owner choice or a subsequent claim.
		_, err := q.LockPolicyHistoricalCustody(ctx, store.LockPolicyHistoricalCustodyParams{
			RunID: id, Generation: req.SourceGeneration, OriginalWorkerID: w.ID, UserID: w.UserID,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			slog.Warn("policy historical outcome unauthenticated", "run", id,
				"source_generation", req.SourceGeneration, "current_generation", r.ClaimGeneration,
				"reason", "custody_missing_mismatched_or_discarded")
			return r, PolicyHoldUnauthenticatedHistoricalOutcome, nil
		}
		if err != nil {
			return r, PolicyHoldPending, err
		}
	}
	if r.Status == "cancelled" || r.Status == "failed" || r.Status == "completed" {
		return r, PolicyHoldTerminal, nil
	}
	if owned && validateReconciledGeneration(r, &req.ClaimGeneration) != nil {
		return r, PolicyHoldPending, nil
	}
	outcome, _ := uuid.Parse(req.OutcomeID)
	if r.PolicyLastSourceGeneration.Valid && r.PolicyLastSourceGeneration.Int64 > req.SourceGeneration {
		return r, PolicyHoldResolvedNewer, nil
	}
	if r.PolicyLastOutcomeID.Valid && r.PolicyLastOutcomeID.Bytes == outcome {
		return r, PolicyHoldAlreadyApplied, nil
	}
	if owned && r.StopKind.Valid && r.StopKind.String != "" {
		_, err := q.CancelRunByWorker(ctx, store.CancelRunByWorkerParams{ID: id, WorkerID: pgconv.UUID(w.ID)})
		if err != nil {
			return r, PolicyHoldPending, err
		}
		cancelled, err := q.GetRunByIDForUpdate(ctx, id)
		if err != nil {
			return r, PolicyHoldPending, err
		}
		if err := tx.Commit(ctx); err != nil {
			return r, PolicyHoldPending, err
		}
		return cancelled, PolicyHoldTerminal, err
	}
	// A bounded last-id slot cannot establish whether another outcome from the same
	// original source was applied. Keep that evidence blocking rather than count twice.
	if r.PolicyLastSourceGeneration.Valid && r.PolicyLastSourceGeneration.Int64 == req.SourceGeneration {
		return r, PolicyHoldPending, nil
	}
	if req.Detail == "resume_rejected" && r.PolicyRefusalCount == 0 {
		return r, PolicyHoldPending, ErrInvalidState
	}
	provenance := "unverified"
	if req.Head != "" {
		provenance = "verified"
	}
	if !owned {
		recorded, err := q.RecordPolicyHoldMetadata(ctx, store.RecordPolicyHoldMetadataParams{
			ID: id, Detail: req.Detail, OutcomeID: pgconv.UUID(outcome),
			SourceGeneration: pgtype.Int8{Int64: req.SourceGeneration, Valid: true},
			Tag:              policyText(req.Tag), Phase: policyText(req.Phase), CorrelationID: policyText(req.CorrelationID),
			RecordPreservation: budgetPark, Provenance: policyText(&provenance),
			Head: policyTextNonempty(req.Head), CheckpointContainsLatest: policyBool(req.CheckpointContainsLatest),
			Published: pgtype.Bool{Bool: req.Published, Valid: true}, SessionID: policyText(req.SessionID),
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return r, PolicyHoldPending, nil
		}
		if err != nil {
			return r, PolicyHoldPending, err
		}
		if err := tx.Commit(ctx); err != nil {
			return r, PolicyHoldPending, err
		}
		return recorded, PolicyHoldRecorded, nil
	}
	held, err := q.SetRunPolicyHold(ctx, store.SetRunPolicyHoldParams{
		ID: id, WorkerID: pgconv.UUID(w.ID), ClaimGeneration: req.ClaimGeneration,
		OutcomeID: pgconv.UUID(outcome), SourceGeneration: pgtype.Int8{Int64: req.SourceGeneration, Valid: true}, Detail: policyText(&req.Detail),
		Tag: policyText(req.Tag), Phase: policyText(req.Phase), CorrelationID: policyText(req.CorrelationID),
		SessionID: policyText(req.SessionID), Head: policyTextNonempty(req.Head), Provenance: pgtype.Text{String: provenance, Valid: true},
		Published: pgtype.Bool{Bool: req.Published, Valid: true}, CheckpointContainsLatest: policyBool(req.CheckpointContainsLatest),
		ExpectedGateRevision: policyInt(req.ExpectedGateRevision),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return r, PolicyHoldPending, nil
	}
	if err != nil {
		return r, PolicyHoldPending, err
	}
	if err := tx.Commit(ctx); err != nil {
		return r, PolicyHoldPending, err
	}
	return held, PolicyHoldApplied, nil
}

func policyText(v *string) pgtype.Text {
	if v == nil {
		return pgtype.Text{}
	}
	return pgtype.Text{String: *v, Valid: true}
}
func policyTextNonempty(v string) pgtype.Text {
	if v == "" {
		return pgtype.Text{}
	}
	return policyText(&v)
}
func policyBool(v *bool) pgtype.Bool {
	if v == nil {
		return pgtype.Bool{}
	}
	return pgtype.Bool{Bool: *v, Valid: true}
}
func policyInt(v *int64) pgtype.Int8 {
	if v == nil {
		return pgtype.Int8{}
	}
	return pgtype.Int8{Int64: *v, Valid: true}
}

// PolicyHoldActionToken binds an owner action to the presented outcome and claim.
func PolicyHoldActionToken(r store.Run) string {
	if !r.PolicyLastOutcomeID.Valid {
		return ""
	}
	return fmt.Sprintf("\"policy-%s-%d\"", uuid.UUID(r.PolicyLastOutcomeID.Bytes), r.ClaimGeneration)
}

type PolicyResumeRequest struct {
	Mode               string `json:"mode,omitempty"`
	Guidance           string `json:"guidance,omitempty"`
	completionDecision bool
}

func (s *Service) ResumePolicyHold(ctx context.Context, owner, id uuid.UUID, token string, req PolicyResumeRequest) (store.Run, error) {
	if req.Mode != "" && req.Mode != "preserve" && req.Mode != "fresh" || len(req.Guidance) > PolicyResumeGuidanceMaxBytes || !utf8.ValidString(req.Guidance) || strings.ContainsRune(req.Guidance, 0) {
		return store.Run{}, ErrInvalidState
	}
	if s.txBeginner == nil {
		return store.Run{}, fmt.Errorf("policy resume requires transaction")
	}
	tx, err := s.beginReadCommitted(ctx)
	if err != nil {
		return store.Run{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := store.New(tx)
	r, err := q.GetRunByIDForUpdate(ctx, id)
	if err != nil {
		return r, err
	}
	if r.UserID != owner {
		return store.Run{}, ErrRunNotOwned
	}
	r, err = s.resumePolicyHoldLocked(ctx, q, r, token, req)
	if err != nil {
		return r, err
	}
	if err := tx.Commit(ctx); err != nil {
		return r, err
	}
	return r, nil
}

// PolicyOwnerConstrained remains true when a later hold replaces the policy reason.
func PolicyOwnerConstrained(r store.Run) bool {
	return !terminalStatuses[r.Status] && (r.PolicyRefusalCount > 0 || r.HoldReason.String == "provider_policy_refusal")
}

// PolicyOwnerResumable excludes holds with a separate owner decision mechanism.
func PolicyOwnerResumable(r store.Run) bool {
	return PolicyOwnerConstrained(r) && r.Status == "paused" && r.ClaimGeneration < math.MaxInt64 &&
		r.HoldReason.String != "completion_blocked" && r.HoldReason.String != "credential_disabled"
}

// PolicyOwnerBudgetAvailable uses the frozen pause instant, as ResumePausedRun does.
func PolicyOwnerBudgetAvailable(r store.Run, timeout time.Duration) bool {
	if !r.StartedAt.Valid {
		return true
	}
	wall := int64(timeout / time.Second)
	if r.BudgetWallSeconds.Valid {
		wall = int64(r.BudgetWallSeconds.Int32)
	}
	used := int64(math.Round(r.StatusSince.Time.Sub(r.StartedAt.Time).Seconds()))
	if used < 0 {
		used = 0
	}
	return wall+int64(r.BudgetExtensionSeconds)+int64(r.BudgetFinalizeSeconds)-(used-int64(r.BudgetPausedSeconds)) > 0
}

// PolicyOwnerCanStop is shared by the locked writer and the owner DTO.
func PolicyOwnerCanStop(r store.Run, timeout time.Duration) bool {
	if !PolicyOwnerResumable(r) || (r.HoldReason.String == "budget_exhausted" && !PolicyOwnerBudgetAvailable(r, timeout)) || r.Kind != "issue" || r.Interactive {
		return false
	}
	frozen, err := DecodeMilestones(r.MilestonesFrozen)
	if err != nil || len(frozen) == 0 {
		return false
	}
	completed, err := DecodeMilestoneIDs(r.MilestonesCompleted)
	return err == nil && len(completed) > 0 && len(completed) <= len(frozen)
}

// resumePolicyHoldLocked requires the caller's transaction and run lock. Stop and
// credential reassignment reuse it without starting a nested transaction.
func (s *Service) resumePolicyHoldLocked(ctx context.Context, q *store.Queries, r store.Run, token string, req PolicyResumeRequest) (store.Run, error) {
	if r.Status != "paused" || !PolicyOwnerConstrained(r) || token == "" || token != PolicyHoldActionToken(r) {
		return r, fmt.Errorf("%w: policy action changed; upgrade your client and refetch the presented action", ErrStaleClaim)
	}
	if r.HoldReason.String == "budget_exhausted" && !PolicyOwnerBudgetAvailable(r, s.p.RunTimeout) {
		return r, fmt.Errorf("%w: this run is out of time; extend it before resuming", ErrWallParkStopUnavailable)
	}
	canResume := PolicyOwnerResumable(r) || (req.completionDecision && r.HoldReason.String == "completion_blocked")
	if !canResume {
		return r, ErrInvalidState
	}
	fresh := req.Mode == "fresh"
	if r.ClaimGeneration == math.MaxInt64 {
		return r, ErrInvalidState
	}
	if strings.TrimSpace(req.Guidance) != "" {
		if _, err := q.CreateRunInput(ctx, store.CreateRunInputParams{RunID: r.ID, Kind: "follow_up", Body: policyText(&req.Guidance)}); err != nil {
			return r, err
		}
	}
	audit := "policy hold: preserve conversation"
	if fresh {
		audit = fmt.Sprintf("policy hold: fresh conversation; prior session_id=%q", r.SessionID.String)
	}
	if _, err := q.CreateRunInput(ctx, store.CreateRunInputParams{RunID: r.ID, Kind: "resume", Body: policyTextNonempty(audit)}); err != nil {
		return r, err
	}
	return q.ResumePolicyHold(ctx, store.ResumePolicyHoldParams{
		ID: r.ID, UserID: r.UserID, OutcomeID: r.PolicyLastOutcomeID, ClaimGeneration: r.ClaimGeneration,
		AllowCompletionBlockedHold: req.completionDecision,
		Fresh:                      fresh, GlobalTimeoutSeconds: int32(s.p.RunTimeout.Seconds()),
	})
}
