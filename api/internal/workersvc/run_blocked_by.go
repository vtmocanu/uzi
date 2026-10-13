package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/runprogress"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// blockedByQuestionPayload is the slice of the `question` run-message payload the
// hint reads (shape: questionPayload in slacksvc/question.go, which is unexported).
// Every string in it is model-authored and untrusted: only the digit runs
// runprogress.IssueRefs extracts ever leave this file.
type blockedByQuestionPayload struct {
	QuestionID string `json:"question_id"`
	Questions  []struct {
		Question string `json:"question"`
	} `json:"questions"`
}

// MaybeBlockedByRun returns the id of a live run the parked run's open question
// mentions (PRD #2602, "Blocked-by hint"), or nil. It is a hint, not a recorded
// dependency, and is best-effort: the caller logs an error and omits the field.
//
// It answers only for a run in awaiting_input with a repo and an open question id;
// the newest question message must carry that same id and no answer for it may exist
// yet (any submitted answer, applied or not). Candidate runs are scoped to the PARKED
// run's owner, never the viewer (an admin may view another user's run), and to its
// repo. A question mentioning no other issue costs no lookup query.
func (s *Service) MaybeBlockedByRun(ctx context.Context, run store.Run) (*uuid.UUID, error) {
	if run.Status != "awaiting_input" || !run.RepoID.Valid || !run.OpenQuestionID.Valid || run.OpenQuestionID.String == "" {
		return nil, nil
	}
	raw, err := s.q.GetLatestRunQuestion(ctx, run.ID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("latest run question: %w", err)
	}
	var payload blockedByQuestionPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		// A payload we cannot read is no hint, not an error worth a log line per read.
		return nil, nil
	}
	if payload.QuestionID != run.OpenQuestionID.String {
		return nil, nil
	}
	answered, err := s.q.RunQuestionAnswerExists(ctx, store.RunQuestionAnswerExistsParams{
		RunID:      run.ID,
		QuestionID: run.OpenQuestionID,
	})
	if err != nil {
		return nil, fmt.Errorf("question answer exists: %w", err)
	}
	if answered {
		return nil, nil
	}
	texts := make([]string, 0, len(payload.Questions))
	for _, q := range payload.Questions {
		texts = append(texts, q.Question)
	}
	var self int64
	if run.IssueIid.Valid {
		self = run.IssueIid.Int64
	}
	refs := runprogress.IssueRefs(texts, self)
	if len(refs) == 0 {
		return nil, nil
	}
	branches := make([]string, 0, len(refs))
	for _, n := range refs {
		branches = append(branches, agentIssueBranch(n))
	}
	id, err := s.q.GetMaybeBlockingRun(ctx, store.GetMaybeBlockingRunParams{
		Owner:  run.UserID,
		RepoID: uuid.UUID(run.RepoID.Bytes),
		RunID:  run.ID,
		Iids:   refs,
		Refs:   branches,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("maybe blocking run: %w", err)
	}
	return &id, nil
}
