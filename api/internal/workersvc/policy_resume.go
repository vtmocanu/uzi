package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Owner resume modes for an unresolved compact policy hold.
const (
	PolicyResumePreserve            = "preserve"
	PolicyResumeFreshFromSavedWork  = "fresh_from_saved_work"
	PolicyResumeFreshOneClaim       = "fresh_one_claim"
	policyResumeGuidanceMaxBytes    = 16000
	policyResumeAuditPrefixTemplate = `{"policy_mode":%q,"episode_id":%q`
)

var (
	// ErrPolicyResumeConflict is a safe refusal: stale, foreign-episode, mode-mismatched or
	// otherwise ineligible requests change nothing.
	ErrPolicyResumeConflict = errors.New("policy hold resume conflict")
	// ErrPolicyFreshUnavailable reports that the optional one-claim Fresh action is not shipped.
	ErrPolicyFreshUnavailable = errors.New("fresh_one_claim is unavailable on this server")
)

// PolicyResumeRequest is the decoded owner request. ExpectedEpisodeID is the outcome id of the
// exact applied hold the owner saw.
type PolicyResumeRequest struct {
	Mode              string
	ExpectedEpisodeID uuid.UUID
	Guidance          string
}

// Validate checks the shape only; eligibility is decided under the run lock.
func (r PolicyResumeRequest) Validate() error {
	switch r.Mode {
	case PolicyResumePreserve, PolicyResumeFreshFromSavedWork, PolicyResumeFreshOneClaim:
	default:
		return fmt.Errorf("%w: invalid policy resume mode", ErrInvalidState)
	}
	if r.ExpectedEpisodeID == uuid.Nil {
		return fmt.Errorf("%w: expected_episode_id required", ErrInvalidState)
	}
	if len(r.Guidance) > policyResumeGuidanceMaxBytes || !utf8.ValidString(r.Guidance) || strings.ContainsRune(r.Guidance, 0) {
		return fmt.Errorf("%w: invalid guidance", ErrInvalidState)
	}
	return nil
}

type policyResumeAudit struct {
	PolicyMode     string  `json:"policy_mode"`
	EpisodeID      string  `json:"episode_id"`
	PriorSessionID *string `json:"prior_session_id"`
}

// PolicyResumeResult carries the run as the owner should see it. Replay is true when the request
// repeated an already-recorded resume (a lost response) and changed nothing.
type PolicyResumeResult struct {
	Run    store.Run
	Replay bool
}

// ResumePolicyHold resolves one exact applied policy episode and queues the run in a single
// read-committed transaction that locks only the run row (never a worker after the run). The
// UPDATE repeats every eligibility predicate, so a stale, foreign or mode-mismatched request
// matches zero rows and commits nothing: no session, barrier, guidance, audit or queue change.
func (s *Service) ResumePolicyHold(ctx context.Context, userID, runID uuid.UUID, req PolicyResumeRequest) (PolicyResumeResult, error) {
	if err := req.Validate(); err != nil {
		return PolicyResumeResult{}, err
	}
	if req.Mode == PolicyResumeFreshOneClaim {
		return PolicyResumeResult{}, ErrPolicyFreshUnavailable
	}
	if s.txBeginner == nil {
		return PolicyResumeResult{}, fmt.Errorf("policy resume requires transaction support")
	}
	tx, err := s.beginReadCommitted(ctx)
	if err != nil {
		return PolicyResumeResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := store.New(tx)
	prior, err := q.GetRunByIDForUpdate(ctx, runID)
	if errors.Is(err, pgx.ErrNoRows) {
		return PolicyResumeResult{}, ErrRunNotFound
	}
	if err != nil {
		return PolicyResumeResult{}, err
	}
	if prior.UserID != userID {
		return PolicyResumeResult{}, ErrRunNotFound
	}
	resumed, err := q.ResumePolicyHold(ctx, store.ResumePolicyHoldParams{
		ID: runID, UserID: userID, Mode: req.Mode, ExpectedEpisodeID: pgconv.UUID(req.ExpectedEpisodeID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		if prior.PolicyOutcomeID.Valid && uuid.UUID(prior.PolicyOutcomeID.Bytes) == req.ExpectedEpisodeID && prior.PolicyResolvedAt.Valid {
			seen, aerr := q.PolicyResumeAuditExists(ctx, store.PolicyResumeAuditExistsParams{
				RunID: runID, BodyPrefix: fmt.Sprintf(policyResumeAuditPrefixTemplate, req.Mode, req.ExpectedEpisodeID.String()),
			})
			if aerr != nil {
				return PolicyResumeResult{}, aerr
			}
			if seen {
				return PolicyResumeResult{Run: prior, Replay: true}, nil
			}
		}
		return PolicyResumeResult{}, ErrPolicyResumeConflict
	}
	if err != nil {
		return PolicyResumeResult{}, err
	}
	if req.Guidance != "" {
		if _, err := q.CreateRunInput(ctx, store.CreateRunInputParams{
			RunID: runID, Kind: "follow_up", Body: pgtype.Text{String: req.Guidance, Valid: true},
		}); err != nil {
			return PolicyResumeResult{}, err
		}
	}
	audit := policyResumeAudit{PolicyMode: req.Mode, EpisodeID: req.ExpectedEpisodeID.String()}
	if prior.SessionID.Valid && prior.SessionID.String != "" {
		sid := prior.SessionID.String
		audit.PriorSessionID = &sid
	}
	body, err := json.Marshal(audit)
	if err != nil {
		return PolicyResumeResult{}, err
	}
	if _, err := q.CreateRunInput(ctx, store.CreateRunInputParams{
		RunID: runID, Kind: "resume", Body: pgtype.Text{String: string(body), Valid: true},
	}); err != nil {
		return PolicyResumeResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return PolicyResumeResult{}, err
	}
	if s.bcast != nil {
		s.bcast.PublishState(runID, resumed.Status)
	}
	s.notify(runID, resumed.Status)
	return PolicyResumeResult{Run: resumed}, nil
}

var policyRecoveryHead = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)

// PolicyRecoveryVerifiedRequest is the worker's per-generation verification record for the
// required recovery head. It carries no execution authority and never changes the head itself.
type PolicyRecoveryVerifiedRequest struct {
	ClaimGeneration int64  `json:"claim_generation"`
	RequiredHead    string `json:"required_head"`
}

// Validate bounds the request to a positive safe generation and a 40/64-hex head.
func (r PolicyRecoveryVerifiedRequest) Validate() error {
	if r.ClaimGeneration <= 0 || r.ClaimGeneration > policyMaxSafeGeneration {
		return fmt.Errorf("%w: invalid claim_generation", ErrInvalidState)
	}
	if !policyRecoveryHead.MatchString(r.RequiredHead) {
		return fmt.Errorf("%w: invalid required_head", ErrInvalidState)
	}
	return nil
}

// PolicyRecoveryVerifiedReply: recorded is authoritative, pending means ownership or generation
// could not be confirmed (retry later), refused is an authoritative refusal for this request.
type PolicyRecoveryVerifiedReply struct {
	Disposition string `json:"disposition"`
	Reason      string `json:"reason,omitempty"`
}

// RecordPolicyRecoveryVerified stores the verification record under the worker then run locks.
// It never touches policy_required_head: only a later same-generation published report can
// supersede it, and a lost reply that still committed changes nothing about that rule.
func (s *Service) RecordPolicyRecoveryVerified(ctx context.Context, worker store.Worker, runID uuid.UUID, req PolicyRecoveryVerifiedRequest) (PolicyRecoveryVerifiedReply, error) {
	if err := req.Validate(); err != nil {
		return PolicyRecoveryVerifiedReply{}, err
	}
	pending := func(reason string) (PolicyRecoveryVerifiedReply, error) {
		return PolicyRecoveryVerifiedReply{Disposition: "pending", Reason: reason}, nil
	}
	refused := func(reason string) (PolicyRecoveryVerifiedReply, error) {
		return PolicyRecoveryVerifiedReply{Disposition: "refused", Reason: reason}, nil
	}
	if s.txBeginner == nil {
		return PolicyRecoveryVerifiedReply{}, fmt.Errorf("policy recovery verification requires transaction support")
	}
	tx, err := s.beginReadCommitted(ctx)
	if err != nil {
		return PolicyRecoveryVerifiedReply{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := store.New(tx)
	current, err := q.GetWorkerForUpdate(ctx, worker.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return pending("worker_unavailable")
	}
	if err != nil {
		return PolicyRecoveryVerifiedReply{}, err
	}
	if current.UserID != worker.UserID || !slices.Contains(current.ProtocolCapabilities, capability.ProviderPolicyHoldV1) {
		return pending("worker_not_capable")
	}
	r, err := q.GetRunByIDForUpdate(ctx, runID)
	if errors.Is(err, pgx.ErrNoRows) {
		return pending("ownership_unconfirmed")
	}
	if err != nil {
		return PolicyRecoveryVerifiedReply{}, err
	}
	if r.UserID != current.UserID || laneMismatch(runID, r.EgressProfileID.Valid, current) ||
		!r.WorkerID.Valid || uuid.UUID(r.WorkerID.Bytes) != worker.ID {
		return pending("ownership_unconfirmed")
	}
	if r.ClaimReleasedAt.Valid {
		return pending("claim_released")
	}
	if r.ClaimGeneration != req.ClaimGeneration {
		return pending("stale_claim")
	}
	if r.Status != "claimed" && r.Status != "running" {
		return pending("ineligible_status")
	}
	if !r.PolicyPreserveRequired {
		return refused("not_preserving")
	}
	if !r.PolicyRequiredHead.Valid || r.PolicyRequiredHead.String != req.RequiredHead {
		return refused("head_mismatch")
	}
	if _, err := q.RecordPolicyRecoveryVerified(ctx, store.RecordPolicyRecoveryVerifiedParams{
		ID: runID, UserID: worker.UserID, WorkerID: pgconv.UUID(worker.ID),
		ClaimGeneration: pgconv.Int8Ptr(&req.ClaimGeneration), RequiredHead: pgconv.TextPtr(&req.RequiredHead),
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return pending("transition_refused")
		}
		return PolicyRecoveryVerifiedReply{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return PolicyRecoveryVerifiedReply{}, err
	}
	return PolicyRecoveryVerifiedReply{Disposition: "recorded"}, nil
}
