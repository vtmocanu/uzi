package workersvc

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// ContinueCompletionDecision records the owner/admin CONTINUE decision on a completion-blocked
// run (PRD #1226 M5, D7) and resumes it, returning the re-read run for the DTO. It is the ONE
// owner decision path this child supports (decision="continue"); the handler rejects any other
// decision with a 400 before calling here, and caps guidance at MaxGuidanceBytes.
//
// It reads the run owner-scoped FIRST (GetRun → ErrRunNotFound for a foreign/absent run, which
// the handler maps to 404 before any write), then dispatches on the two states a completion
// interlock can reach:
//
//   - awaiting_input, live completion-question window: the lead executor is still alive, so the
//     decision resumes IN PLACE. There is NO status transition here — the worker reports
//     `running` itself when its steering poll picks up the guidance and it resumes its loop
//     (SetRunRunning then clears the hold on that first accepted running report). The window is
//     identified the SAME way SetRunCompletionHold admits awaiting_input: an INTERLOCKED run
//     (completion_contract_version set) with at least one recorded completion attempt.
//   - paused with hold_reason='completion_blocked': the lead was reaped (or the owner already
//     paused it), so the decision resumes THROUGH queued via ResumePausedRun (which banks the
//     parked wall-clock into budget_paused_seconds). The guidance rides as a `follow_up` the new
//     claim/session picks up. The hold columns are DELIBERATELY LEFT SET here — D7 clears them on
//     the first accepted running report (SetRunRunning), NOT prematurely in the resume — so a
//     resumed run still carries hold_reason/hold_captured_head until it is running again.
//
// Any other status/reason is neither window, so it is a 409 (ErrCompletionNotBlocked): the run
// never asked for a decision.
//
// Two rows are written on the accepted path (writes BEFORE the transition, so a follow_up is
// enqueued before the run leaves paused and the new claim cannot miss it):
//   - a `follow_up` input carrying the guidance — the EXISTING guidance-injection channel the
//     worker's steering poll drains (ConsumeRunInputs includes follow_up). Written only when
//     guidance is non-empty; an empty guidance is a bare continue with nothing to inject.
//   - a `completion_decision` AUDIT row — EXCLUDED from ConsumeRunInputs (like `resume`), so the
//     worker never drains it; it records the decision for the run's input history. Its body is
//     the guidance (or NULL). The decision type is encoded by the kind (only continue this child).
func (s *Service) ContinueCompletionDecision(ctx context.Context, userID, runID uuid.UUID, guidance string) (store.Run, error) {
	run, err := s.GetRun(ctx, userID, runID)
	if err != nil {
		return store.Run{}, err
	}

	paused := false
	switch {
	case run.Status == "awaiting_input" && completionQuestionOpen(run):
		// Live window: no transition — the worker reports running itself on resume.
	case run.Status == "paused" && run.HoldReason.Valid && run.HoldReason.String == "completion_blocked":
		paused = true
	default:
		return store.Run{}, ErrCompletionNotBlocked
	}

	// The guidance-injection follow_up + the completion_decision audit row are written BEFORE the
	// resume transition (paused case) so the follow_up is enqueued while the run is still paused
	// and the new claim cannot race past it. For the live awaiting_input window there is no
	// transition, so ordering is immaterial there.
	if guidance != "" {
		if _, err := s.q.CreateRunInput(ctx, store.CreateRunInputParams{
			RunID: runID, Kind: "follow_up", Body: pgconv.TextOrNull(guidance),
		}); err != nil {
			return store.Run{}, err
		}
	}
	if _, err := s.q.CreateRunInput(ctx, store.CreateRunInputParams{
		RunID: runID, Kind: "completion_decision", Body: pgconv.TextOrNull(guidance),
	}); err != nil {
		return store.Run{}, err
	}

	if paused {
		// paused → queued, banking budget_paused_seconds; owner+status-scoped. The hold columns
		// are NOT touched (SetRunRunning clears them on the first running report). A race that
		// moved the run out of paused between the read and here yields 0 rows → 409.
		if _, err := s.q.ResumePausedRun(ctx, store.ResumePausedRunParams{ID: runID, UserID: userID}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return store.Run{}, ErrCompletionNotBlocked
			}
			return store.Run{}, err
		}
	}

	// Re-read owner-scoped so the DTO reflects the resumed (queued) status for the paused case,
	// and the unchanged awaiting_input status for the live case.
	return s.GetRun(ctx, userID, runID)
}

// completionQuestionOpen reports whether an awaiting_input run is parked on the completion
// interlock's question window rather than an ordinary PRD #88 clarification question. It uses the
// SAME discriminator SetRunCompletionHold admits awaiting_input on: an INTERLOCKED run
// (completion_contract_version set) that has recorded at least one completion attempt. A dedicated
// completion-question marker is a later M5 refinement; until then this interlock+attempt pair is
// the available, correct signal, and it keeps the decision endpoint in lockstep with the hold
// transition's own admit condition.
func completionQuestionOpen(run store.Run) bool {
	return run.CompletionContractVersion.Valid && run.CompletionAttempts > 0
}
