package workersvc

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// MaxDecisionsMemoBytes bounds one run decisions memo body (issue #2083). It mirrors the CHECK on
// run_decision_memos.body (octet_length 1..8192) and is applied to the SANITIZED body, so the
// stored byte count is the one the cap measured.
const MaxDecisionsMemoBytes = 8192

var (
	// ErrDecisionsMemoEmpty is a body that is empty or whitespace-only after sanitizing → 400.
	ErrDecisionsMemoEmpty = errors.New("decisions memo body must be non-empty")
	// ErrDecisionsMemoTooLarge is a sanitized body over MaxDecisionsMemoBytes → 400.
	ErrDecisionsMemoTooLarge = errors.New("decisions memo body too large")
	// ErrDecisionsMemoClaimNotCurrent is a fenced write or read the run's current state refuses:
	// the generation is not the run's, the claim was released, or (a write only) the run is no
	// longer running or its kind carries no memo → 409 claim_not_current. A read by a run whose
	// kind carries no memo answers a nil memo, not this error.
	ErrDecisionsMemoClaimNotCurrent = errors.New("decisions memo claim is not current")
)

// DecisionsMemo is the resolved prior memo for an mr_rework run.
type DecisionsMemo struct {
	Format      int
	Body        string
	SourceRunID uuid.UUID
}

// SaveDecisionsMemo stores the run's decisions memo (issue #2083), returning the stored byte count.
// The authoritative fence is the single UpsertRunDecisionMemoFenced statement, which re-checks
// worker, generation, released claim, status, kind and repo under the run row lock; the
// runOwnedByWorker read ahead of it only separates "not this worker's run" (ErrRunNotOwned → 404,
// including the isolated-lane purpose check) from "this worker's run, claim not current" (0 rows →
// ErrDecisionsMemoClaimNotCurrent → 409). The body is never logged.
func (s *Service) SaveDecisionsMemo(ctx context.Context, wkr store.Worker, runID uuid.UUID, claimGeneration int64, body string) (int, error) {
	if _, err := s.runOwnedByWorker(ctx, runID, wkr); err != nil {
		return 0, err
	}
	// Strip control characters before the cap, exactly like SaveMemory: the memo is replayed into
	// a later lead's prompt, so an injected ANSI escape or control byte must not survive the store.
	body = sanitizeMemoryField(body, true)
	if strings.TrimSpace(body) == "" {
		return 0, ErrDecisionsMemoEmpty
	}
	if len(body) > MaxDecisionsMemoBytes {
		return 0, ErrDecisionsMemoTooLarge
	}
	rows, err := s.q.UpsertRunDecisionMemoFenced(ctx, store.UpsertRunDecisionMemoFencedParams{
		RunID:           runID,
		WorkerID:        pgconv.UUID(wkr.ID),
		ClaimGeneration: claimGeneration,
		Body:            body,
	})
	if err != nil {
		return 0, err
	}
	if rows == 0 {
		return 0, ErrDecisionsMemoClaimNotCurrent
	}
	return len(body), nil
}

// ResolveDecisionsMemo returns the prior memo an mr_rework run should start from, or nil when there
// is none (issue #2083). The caller must hold the run's current claim: the run is read through
// runOwnedByWorker (ErrRunNotOwned → 404) and must match claimGeneration with its claim unreleased
// (else ErrDecisionsMemoClaimNotCurrent), so a stale flight never reads. Only mr_rework runs with a
// repo, pipeline_ref and mr_iid resolve; the lineage is (owner, repo, branch = pipeline_ref,
// mr_iid), taken from the claimed run and never from the caller.
func (s *Service) ResolveDecisionsMemo(ctx context.Context, wkr store.Worker, runID uuid.UUID, claimGeneration int64) (*DecisionsMemo, error) {
	run, err := s.runOwnedByWorker(ctx, runID, wkr)
	if err != nil {
		return nil, err
	}
	if run.ClaimGeneration != claimGeneration || run.ClaimReleasedAt.Valid {
		return nil, ErrDecisionsMemoClaimNotCurrent
	}
	if run.Kind != "mr_rework" || !run.RepoID.Valid || !run.PipelineRef.Valid || run.PipelineRef.String == "" || !run.MrIid.Valid {
		return nil, nil
	}
	row, err := s.q.GetLatestDecisionMemoForLineage(ctx, store.GetLatestDecisionMemoForLineageParams{
		UserID:    run.UserID,
		RepoID:    run.RepoID,
		Branch:    pgtype.Text{String: run.PipelineRef.String, Valid: true},
		MrIid:     run.MrIid,
		SelfRunID: run.ID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &DecisionsMemo{Format: int(row.FormatVersion), Body: row.Body, SourceRunID: row.RunID}, nil
}
