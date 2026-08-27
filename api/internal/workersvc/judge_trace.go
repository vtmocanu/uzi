package workersvc

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Judge trace pagination caps (PRD #46 Decision 3, audit L2): the trace endpoint
// enforces the page/size budget server-side so a pathological run can't be replayed
// wholesale in one call.
const (
	judgeTracePageDefault int32 = 200
	judgeTracePageMax     int32 = 500
	judgeInputsCap        int32 = 500
)

// JudgeTraceResult is one page of a reviewed run's trace plus its metadata + steering
// log (PRD #46 Decision 3). The worker paginates messages via `after`; the metadata
// and (small) steering log are returned on every page.
type JudgeTraceResult struct {
	Target   store.Run
	Messages []store.RunMessage
	Inputs   []store.RunUserInput
}

// authorizeJudgeTrace enforces the judge-run-scoped authorization shared by the trace
// and review endpoints (PRD #46 Decision 3, audit H1). The caller's worker must own a
// NON-TERMINAL judge run whose target_run_id is targetID; the reviewed run is then
// loaded and target.user_id == judge.user_id is re-asserted INDEPENDENTLY (the enqueue
// invariant is necessary but not sufficient). Plain user-scoping is explicitly
// rejected — it would let any of a user's workers stream any of their traces at will.
// Every failure returns ErrRunNotFound so the endpoint reveals nothing (no "no judge
// run" vs "owner mismatch" oracle).
func (s *Service) authorizeJudgeTrace(ctx context.Context, wkr store.Worker, targetID uuid.UUID) (judge, target store.Run, err error) {
	judge, err = s.q.GetActiveJudgeRunForWorkerTarget(ctx, store.GetActiveJudgeRunForWorkerTargetParams{
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
	if target.UserID != judge.UserID {
		return store.Run{}, store.Run{}, ErrRunNotFound
	}
	return judge, target, nil
}

// JudgeTrace returns one page of the reviewed run's trace for the worker's judge run
// (PRD #46 Decision 3). Authorization is judge-run-scoped (authorizeJudgeTrace). The
// message page is bounded server-side (judgeTracePageMax) so the whole run can't be
// replayed in a single call; the steering log is capped too.
func (s *Service) JudgeTrace(ctx context.Context, wkr store.Worker, targetID uuid.UUID, after, limit int32) (JudgeTraceResult, error) {
	_, target, err := s.authorizeJudgeTrace(ctx, wkr, targetID)
	if err != nil {
		return JudgeTraceResult{}, err
	}
	if after < 0 {
		after = 0
	}
	if limit <= 0 {
		limit = judgeTracePageDefault
	}
	if limit > judgeTracePageMax {
		limit = judgeTracePageMax
	}
	msgs, err := s.q.ListRunMessagesForWorkerPage(ctx, store.ListRunMessagesForWorkerPageParams{
		RunID:    targetID,
		AfterSeq: after,
		Lim:      limit,
	})
	if err != nil {
		return JudgeTraceResult{}, err
	}
	inputs, err := s.q.ListRunInputsForRun(ctx, store.ListRunInputsForRunParams{
		RunID: targetID,
		Lim:   judgeInputsCap,
	})
	if err != nil {
		return JudgeTraceResult{}, err
	}
	return JudgeTraceResult{Target: target, Messages: msgs, Inputs: inputs}, nil
}
