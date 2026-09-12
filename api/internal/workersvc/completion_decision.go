package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

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
//     `running` itself when its steering poll picks up the ANSWER and it resumes its loop
//     (SetRunRunning then clears the hold on that first accepted running report). The window is
//     identified by the DEDICATED completion-question marker (completion_question_at, set by
//     SetRunAwaitingInput only when the worker's report flagged a COMPLETION question) via
//     completionQuestionOpen — NOT the old interlock+attempts proxy, which also matched an
//     ordinary PRD #88 clarification on an interlocked post-attempt run. The marker distinguishes
//     the question KIND; the open_question_id below distinguishes only its identity. A live
//     completion question ALSO carries an open_question_id (the worker names it at the ask);
//     without one the worker's await (steering.awaitAnswer) can never resolve, so a missing id
//     is not a real completion window and is refused like any non-blocked run.
//   - paused with hold_reason='completion_blocked': the lead was reaped (or the owner already
//     paused it), so the decision resumes THROUGH queued via ResumePausedRun (which banks the
//     parked wall-clock into budget_paused_seconds). The guidance rides as a `follow_up` the new
//     claim/session's pullFollowUp picks up (there is NO live await to resolve). The hold columns
//     are DELIBERATELY LEFT SET here — D7 clears them on the first accepted running report
//     (SetRunRunning), NOT prematurely in the resume — so a resumed run still carries
//     hold_reason/hold_captured_head until it is running again.
//
// Any other status/reason is neither window, so it is a 409 (ErrCompletionNotBlocked): the run
// never asked for a decision.
//
// The guidance-injection channel DIFFERS by branch, because the worker awaits differently:
//   - LIVE awaiting_input window: the worker's completion-question await is
//     steering.awaitAnswer(questionId), which steering.route() resolves ONLY on an `answer`-kind
//     input (parseAnswerBody requires {question_id, answers}); a `follow_up` NEVER resolves it. So
//     the continue is delivered as an ANSWER naming the run's OWN open_question_id, in the exact
//     wire shape parseAnswerBody expects — mirroring submitAnswer's write via CreateRunAnswerInput
//     (kind='answer', the question_id column). The answer is written UNCONDITIONALLY: an empty
//     guidance still sends a non-empty continue sentinel (["continue"]) so the await resolves and
//     the worker gets an unambiguous continue signal.
//   - paused window: there is no live await; the resumed NEW claim's pullFollowUp drains the
//     guidance, so it rides as a `follow_up` (ConsumeRunInputs includes follow_up). Written only
//     when guidance is non-empty; an empty guidance is a bare continue with nothing to inject.
//
// On both branches a `completion_decision` AUDIT row is written — EXCLUDED from ConsumeRunInputs
// (like `resume`), so the worker never drains it; it records the decision for the run's input
// history. Its body is the guidance (or NULL). The decision type is encoded by the kind (only
// continue this child). And on both branches the served budget_exhausted steer is CLEARED
// (ClearCompletionBudgetExhausted) — D3's "a new owner decision clears it": a no-op on the paused
// branch (already NULL from SetRunCompletionHold), and on the live branch the clear that stops the
// resumed worker being immediately re-steered into the hold off a since-consumed ACK.
//
// All writes happen BEFORE the resume transition (paused case) so the follow_up is enqueued while
// the run is still paused and the new claim cannot race past it. For the live awaiting_input
// window there is no transition, so ordering is immaterial there.
func (s *Service) ContinueCompletionDecision(ctx context.Context, userID, runID uuid.UUID, guidance string) (store.Run, error) {
	run, err := s.GetRun(ctx, userID, runID)
	if err != nil {
		return store.Run{}, err
	}

	live := false
	paused := false
	switch {
	case run.Status == "awaiting_input" && completionQuestionOpen(run):
		// Live window: no transition — the worker reports running itself on resume.
		// completionQuestionOpen already proved this is a COMPLETION question (the marker), not an
		// ordinary clarification, so the answer written below cannot resolve the wrong question. The
		// open_question_id is a SEPARATE requirement — it names WHICH question, not its kind: a
		// completion question always names one, and without it the answer cannot resolve the worker's
		// await (steering.awaitAnswer keys on the id), so a missing id is refused like a non-blocked run.
		if !run.OpenQuestionID.Valid || run.OpenQuestionID.String == "" {
			return store.Run{}, ErrCompletionNotBlocked
		}
		live = true
	case run.Status == "paused" && run.HoldReason.Valid && run.HoldReason.String == "completion_blocked":
		paused = true
	default:
		return store.Run{}, ErrCompletionNotBlocked
	}

	if live {
		// Deliver the continue as an ANSWER (the only kind steering.awaitAnswer resolves on),
		// naming the run's OWN open_question_id in the exact {question_id, answers} wire shape
		// parseAnswerBody expects. An empty guidance still sends a continue sentinel so the await
		// resolves. Re-encoded from the AnswerBody struct (the same type submitAnswer marshals).
		answers := []string{guidance}
		if guidance == "" {
			answers = []string{"continue"}
		}
		encoded, merr := json.Marshal(AnswerBody{QuestionID: run.OpenQuestionID.String, Answers: answers})
		if merr != nil {
			return store.Run{}, fmt.Errorf("encode completion continue answer: %w", merr)
		}
		if _, err := s.q.CreateRunAnswerInput(ctx, store.CreateRunAnswerInputParams{
			RunID: runID, Body: pgconv.TextOrNull(string(encoded)), QuestionID: pgconv.TextOrNull(run.OpenQuestionID.String),
		}); err != nil {
			return store.Run{}, err
		}
	} else if guidance != "" {
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

	// D3: a NEW OWNER DECISION clears the served budget_exhausted steer. Unconditional and
	// id-scoped (owner already proven by the GetRun read): a no-op on the paused branch, and on
	// the live branch the clear that stops the resumed worker being re-steered into the hold.
	if _, err := s.q.ClearCompletionBudgetExhausted(ctx, runID); err != nil {
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
// interlock's question window rather than an ordinary PRD #88 clarification question. It keys on
// the DEDICATED completion-question marker (completion_question_at), which SetRunAwaitingInput sets
// ONLY when the worker's report flags a COMPLETION question and BOTH SetRunRunning and
// SetRunCompletionHold clear on resolution. Because the marker is authored solely by a completion
// question report, an ordinary ask_user clarification — even on an interlocked run past a
// completion attempt — no longer matches, so the owner completion-continue can no longer deliver
// its `answer` to (and wrongly resolve) the wrong question. This REPLACES the old
// interlock+completion_attempts proxy, which matched any interlocked post-attempt awaiting_input
// run regardless of what it was actually asking.
func completionQuestionOpen(run store.Run) bool {
	return run.CompletionQuestionAt.Valid
}
