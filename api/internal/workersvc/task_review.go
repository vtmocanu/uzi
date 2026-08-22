package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Task-review taxonomy (PRD #400 M4a). These sets mirror the task_reviews /
// task_review_findings CHECK constraints; the handler validates a worker's review POST
// against them at ingest and the DB CHECK is the backstop.
var (
	// TaskReviewStatuses: "complete" is a real reviewer verdict; "failed" is the
	// deterministic fallback written when the reviewer agent call fails — findings still
	// land in the latter case (mirrors the judge's status).
	TaskReviewStatuses = map[string]bool{"complete": true, "failed": true}
	// TaskReviewSeverities is the closed severity enum for a diff-review finding.
	TaskReviewSeverities = map[string]bool{"info": true, "warning": true, "error": true}
)

// Task-review free-text length caps (PRD #400 M4a). Generous for a review/finding, tight
// enough to bound an attacker-suppliable worker POST.
const (
	TaskReviewSummaryMaxBytes   = 8 * 1024
	TaskReviewFileMaxBytes      = 1024
	TaskReviewSymbolMaxBytes    = 512
	TaskReviewRationaleMaxBytes = 4 * 1024
	TaskReviewFindingSummaryMax = 4 * 1024
	TaskReviewMaxFindings       = 200
	// TaskReviewMaxLine bounds the reported line number to a sane non-negative int so a
	// pathological value cannot land in the column. Larger than any real source file.
	TaskReviewMaxLine = 1 << 30
)

// TaskReviewSubmission is a reviewer's header + findings, already VALIDATED and SCRUBBED
// by the handler (enum-checked, length-capped, control-stripped, secret-scrubbed). The
// service persists it; the DB CHECK on status/severity is the backstop.
type TaskReviewSubmission struct {
	Status    string
	SummaryMd string
	Findings  []TaskReviewFinding
}

// TaskReviewFinding is one structured diff-review finding. The json tags match the
// jsonb_to_recordset columns the atomic upsert query destructures.
type TaskReviewFinding struct {
	File        string `json:"file"`
	Symbol      string `json:"symbol"`
	Line        int32  `json:"line"`
	Severity    string `json:"severity"`
	SummaryMd   string `json:"summary_md"`
	RationaleMd string `json:"rationale_md"`
}

// TaskReviewWithFindings is the read payload: the review header row plus its findings, all
// already scrubbed + capped at ingest. nil (not this struct) means "no review yet".
type TaskReviewWithFindings struct {
	Review   store.TaskReview
	Findings []store.TaskReviewFinding
}

// authorizeTaskReviewTarget enforces the review-run-scoped authorization for the review
// POST (PRD #400 M4a, mirrors authorizeJudgeTrace): the caller's worker must own a
// NON-TERMINAL review run whose review_target_run_id is targetID; the reviewed target run
// is then loaded and target.user_id == review.user_id is re-asserted INDEPENDENTLY. Every
// failure returns ErrRunNotFound so the endpoint reveals nothing.
func (s *Service) authorizeTaskReviewTarget(ctx context.Context, wkr store.Worker, targetID uuid.UUID) (review, target store.Run, err error) {
	review, err = s.q.GetActiveTaskReviewRunForWorkerTarget(ctx, store.GetActiveTaskReviewRunForWorkerTargetParams{
		WorkerID:    pgUUID(wkr.ID),
		TargetRunID: pgUUID(targetID),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.Run{}, store.Run{}, ErrRunNotFound
		}
		return store.Run{}, store.Run{}, err
	}
	target, err = s.q.GetRunByID(ctx, targetID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.Run{}, store.Run{}, ErrRunNotFound
		}
		return store.Run{}, store.Run{}, err
	}
	if target.UserID != review.UserID {
		return store.Run{}, store.Run{}, ErrRunNotFound
	}
	return review, target, nil
}

// PostTaskReview persists a reviewer's header + findings for a reviewed task (PRD #400
// M4a) — the worker's write-back at review-run completion. Authorization is
// review-run-scoped (authorizeTaskReviewTarget): the caller's worker must own the active
// review run reviewing targetID, and target/review owner equality is re-asserted. The
// header and its findings are written in ONE atomic CTE with UPSERT (replace) semantics,
// so a re-review overwrites the prior findings rather than 23505-ing (the same shape the
// judge's PostReview uses; workersvc holds no pool for a service-level tx).
func (s *Service) PostTaskReview(ctx context.Context, wkr store.Worker, targetID uuid.UUID, sub TaskReviewSubmission) error {
	review, target, err := s.authorizeTaskReviewTarget(ctx, wkr, targetID)
	if err != nil {
		return err
	}
	findings := sub.Findings
	if findings == nil {
		findings = []TaskReviewFinding{}
	}
	findingsJSON, err := json.Marshal(findings)
	if err != nil {
		return fmt.Errorf("marshal findings: %w", err)
	}
	if _, err := s.q.UpsertTaskReviewWithFindings(ctx, store.UpsertTaskReviewWithFindingsParams{
		TargetRunID: target.ID,
		ReviewRunID: pgUUID(review.ID),
		UserID:      target.UserID,
		Status:      sub.Status,
		SummaryMd:   sub.SummaryMd,
		Findings:    findingsJSON,
	}); err != nil {
		return err
	}
	return nil
}

// GetTaskReviewPanel is the read for the CLI/panel: the review header (nil when the task
// was never reviewed) plus its findings, for a run the caller can see. Visibility reuses
// the owner-or-admin GetRunForViewer rule (mirrors GetRunReviewPanel): a run the viewer
// can't see is ErrRunNotFound, exactly as an unknown id. A visible run that has no review
// yet returns (nil, nil) — a legitimate "no review", distinct from "no such run".
func (s *Service) GetTaskReviewPanel(ctx context.Context, userID uuid.UUID, isAdmin bool, targetRunID uuid.UUID) (*TaskReviewWithFindings, error) {
	if _, err := s.GetRunForViewer(ctx, userID, isAdmin, targetRunID); err != nil {
		return nil, err
	}
	review, err := s.q.GetTaskReviewForTarget(ctx, targetRunID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil // visible run, not reviewed (yet)
	}
	if err != nil {
		return nil, err
	}
	findings, err := s.q.ListTaskReviewFindings(ctx, review.ID)
	if err != nil {
		return nil, err
	}
	return &TaskReviewWithFindings{Review: review, Findings: findings}, nil
}
