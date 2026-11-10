package workersvc

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

const IterationBudgetExhausted = "iteration_budget_exhausted"

var ErrIterationParkEvidence = errors.New("invalid iteration park evidence")
var ErrIterationParkAuthority = errors.New("iteration park requires transaction authority")

// IterationParkEvidence describes this invocation's allowance, not the run's
// lifetime high-water iteration counter. Published and CheckpointContainsLatest are the
// publication evidence that can supersede a required policy recovery head (#3607 M2a).
type IterationParkEvidence struct {
	ClaimGeneration          int64
	IterationCount           int32
	MaxIterations            int32
	Head                     string
	Published                bool
	CheckpointContainsLatest *bool
}

func (e IterationParkEvidence) Validate() error {
	if e.ClaimGeneration <= 0 || e.IterationCount <= 0 || e.MaxIterations <= 0 ||
		e.IterationCount < e.MaxIterations || int64(e.IterationCount) > int64(e.MaxIterations)+1 ||
		len(e.Head) > 128 || !utf8.ValidString(e.Head) {
		return ErrIterationParkEvidence
	}
	return nil
}

// ReportIterationPark serializes admission, transition and input settlement with
// first attempts and owner controls. A refusal never commits mutations.
func (s *Service) ReportIterationPark(ctx context.Context, w store.Worker, id uuid.UUID, e IterationParkEvidence) (store.Run, bool, string, error) {
	if err := e.Validate(); err != nil {
		return store.Run{}, false, "", err
	}
	if s.txBeginner == nil {
		return store.Run{}, false, "", ErrIterationParkAuthority
	}
	locked, run, tx, err := s.beginClaimMutation(ctx, w, id, &e.ClaimGeneration)
	if err != nil {
		// Reconciled runs enforce the generation in beginClaimMutation itself.
		if errors.Is(err, ErrStaleClaim) {
			run, err = s.runOwnedByWorker(ctx, id, w)
			if err == nil {
				return run, false, iterationClaimRefusal(run, e.ClaimGeneration), nil
			}
		}
		return store.Run{}, false, "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if reason := iterationClaimRefusal(run, e.ClaimGeneration); reason != "" {
		return run, false, reason, nil
	}
	// The policy episode is an independent continuation fence, including a
	// replay whose iteration marker survived another lifecycle transition.
	if unresolvedPolicyEpisode(run) {
		return run, false, "pending_control", nil
	}
	// A replay must not certify zero-attempt eligibility after history changed.
	if run.CompletionAttempts != 0 {
		return run, false, "prior_attempt", nil
	}
	q := store.New(tx)
	head, _ := stripNUL(e.Head)
	captured := pgconv.TextOrNull(strings.TrimSpace(head))
	// Replay precedes mutable configured-default equality. It never changes
	// status_since, budgets or an existing captured head.
	if run.Status == "paused" && run.HoldReason.String == IterationBudgetExhausted {
		if captured.Valid && !run.HoldCapturedHead.Valid {
			if _, err := q.RecordIterationParkCapturedHead(ctx, store.RecordIterationParkCapturedHeadParams{
				ID: id, WorkerID: pgconv.UUID(w.ID), ClaimGeneration: e.ClaimGeneration, Head: captured,
			}); err != nil {
				return run, false, "", err
			}
			run.HoldCapturedHead = captured
		}
		if err := tx.Commit(ctx); err != nil {
			return run, false, "", err
		}
		return run, true, "", nil
	}
	if run.Status != "running" {
		return run, false, "not_running", nil
	}
	eligible := false
	switch run.Kind {
	case "issue", "mr_rework", "ci_fix", "task", "prompt", "self_improve":
		eligible = true
	}
	if !eligible || run.Interactive || run.ReviewTargetRunID.Valid || run.EgressProfileID.Valid {
		return run, false, "ineligible_lane", nil
	}
	if int64(e.MaxIterations) != int64(coalesceInt(run.BudgetMaxIterations, s.p.RunMaxIterations)) {
		return run, false, "allowance_mismatch", nil
	}
	pending, err := q.IterationParkHasPendingControl(ctx, id)
	if err != nil {
		return run, false, "", err
	}
	if pending || run.StopKind.String != "" {
		return run, false, "pending_control", nil
	}
	// Capture has already stopped this invocation. A concurrent credential choice
	// applies on owner Resume/reclaim, so keep both its stamp and override. Only
	// credential application may clear the stamp; the park does not apply a token.
	// Pass the server fallback to the write when it applies. A frozen allowance
	// makes it irrelevant; do not narrow an unused, potentially wider config value.
	globalMaxIterations := e.MaxIterations
	if !run.BudgetMaxIterations.Valid {
		configured := s.p.RunMaxIterations
		if configured <= 0 || configured > 2147483647 {
			return run, false, "allowance_mismatch", nil
		}
		globalMaxIterations = int32(configured)
	}
	n, err := q.SetRunIterationPark(ctx, store.SetRunIterationParkParams{
		ID: id, WorkerID: pgconv.UUID(w.ID), ClaimGeneration: e.ClaimGeneration,
		HoldCapturedHead: captured, GlobalMaxIterations: globalMaxIterations,
		Published: e.Published, CheckpointContainsLatest: pgconv.BoolPtr(e.CheckpointContainsLatest),
		MaxIterations: e.MaxIterations, IterationCount: e.IterationCount,
	})
	if err != nil {
		return run, false, "", err
	}
	if n != 1 {
		return run, false, "pending_control", nil
	}
	run, err = q.GetRunByID(ctx, id)
	if err != nil {
		return run, false, "", err
	}
	if v, ok := locked.setCheckpointDurability(ctx, w, run, "paused", e.CheckpointContainsLatest); ok {
		run.CheckpointContainsLatest = v
	}
	if err := tx.Commit(ctx); err != nil {
		return run, false, "", err
	}
	if s.bcast != nil {
		s.bcast.PublishState(id, run.Status)
	}
	s.notify(id, run.Status)
	return run, true, "", nil
}

func iterationClaimRefusal(run store.Run, generation int64) string {
	if run.ClaimGeneration != generation {
		return "stale_claim"
	}
	if run.ClaimReleasedAt.Valid {
		return "released_claim"
	}
	return ""
}
