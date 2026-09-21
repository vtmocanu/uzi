package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #1227 M1 owner-decision errors, mapped to HTTP status codes by the handler.
var (
	// ErrCompletionRevisionConflict → 409: a partial/accept decision raced another decision (or a
	// completion consume) and the run's contract_revision no longer matches the one the caller
	// fenced on, OR the run is not in a revisable state (unfrozen / split-state). The caller must
	// re-read the current revision and decide again; last-write-wins is deliberately refused (D1).
	ErrCompletionRevisionConflict = errors.New("completion decision conflicts with the current contract revision")
	// ErrCompletionDecisionInvalid → 400: the decision failed semantic validation. It is ALWAYS
	// wrapped with a safe, client-showable reason via fmt.Errorf("%w: <msg>", …); the handler uses
	// err.Error() as the client message. The wrapped messages are fixed strings (no user data):
	// "unknown milestone id", "unknown criterion id", "reason required", "milestone already
	// satisfied", etc.
	ErrCompletionDecisionInvalid = errors.New("completion decision invalid")
)

// maxCompletionReasonBytes caps an owner decision's reason at the same 8 KiB the guidance/follow-up
// cap uses (handler.MaxGuidanceBytes). Defined locally to avoid a workersvc→handler import cycle;
// the handler ALSO caps it for shape, and the service re-checks non-empty here (D1: reasons are
// required).
const maxCompletionReasonBytes = 8 * 1024

// CompletionDecisionInput is the owner/admin completion decision (PRD #1227 D1), the service-layer
// shape of the widened {id}/completion/decision body. Decision is one of "continue" (owner-only,
// #1226), "partial" (owner/admin, reduce scope by milestone id) or "accept" (owner/admin, accept
// unmet criteria by criterion id). Keep is the milestone-id set a partial keeps in scope; Criteria
// is the criterion-id set an accept accepts; Reason is required for partial/accept; ContractRevision
// is the revision the caller believes it is deciding against (the optimistic fence).
type CompletionDecisionInput struct {
	Decision         string
	Guidance         string
	Keep             []string
	Criteria         []string
	Reason           string
	ContractRevision int
}

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
	// The continue writes are NON-transactional and byte-identical to #1226: the shared
	// resumeCompletionBlocked helper (also used by the #1227 partial/accept transaction) executes
	// them against s.q, with followupGuidance == auditBody == guidance.
	if err := resumeCompletionBlocked(ctx, s.q, run, userID, guidance, guidance, int32(s.p.RunTimeout.Seconds())); err != nil {
		return store.Run{}, err
	}
	// Re-read owner-scoped so the DTO reflects the resumed (queued) status for the paused case,
	// and the unchanged awaiting_input status for the live case.
	return s.GetRun(ctx, userID, runID)
}

// resumeCompletionBlocked performs the shared resume mechanics for an owner decision on a
// completion-blocked run (PRD #1226 D7 / #1227 M1), against a query surface q (the pool-backed
// s.q for the non-transactional continue path, or a tx-bound *store.Queries for the partial/accept
// transaction). It dispatches on the two completion-blocked states and refuses a run in neither:
//
//   - awaiting_input + completionQuestionOpen(run) + a live open_question_id: the LIVE window. The
//     decision resumes IN PLACE (no status transition — the worker reports running itself). The
//     guidance is delivered as an ANSWER naming the run's OWN open_question_id in the exact
//     {question_id, answers} wire shape parseAnswerBody expects (a follow_up would never resolve
//     steering.awaitAnswer). An empty followupGuidance still sends the continue sentinel
//     (["continue"]) so the await resolves. A completion window without an open_question_id can
//     never resolve, so it is refused like a non-blocked run.
//   - paused + hold_reason='completion_blocked': the lead was reaped. The decision resumes THROUGH
//     queued via ResumePausedRun; the guidance rides as a `follow_up` the resumed claim's
//     pullFollowUp drains, written ONLY when non-empty (an empty guidance is a bare continue). The
//     hold columns are LEFT SET (SetRunRunning clears them on the first running report).
//   - neither → ErrCompletionNotBlocked (the run never asked for a decision).
//
// On BOTH branches it writes the `completion_decision` AUDIT row (Body=auditBody; excluded from
// ConsumeRunInputs so the worker never drains it) and CLEARS the served budget_exhausted steer
// (D3). For the partial/accept path the caller runs this LAST inside the transaction, so a
// not-blocked run rolls back the revision bump and permit invalidation atomically.
func resumeCompletionBlocked(ctx context.Context, q Store, run store.Run, userID uuid.UUID, followupGuidance, auditBody string, globalTimeoutSeconds int32) error {
	live := false
	paused := false
	switch {
	case run.Status == "awaiting_input" && completionQuestionOpen(run):
		if !run.OpenQuestionID.Valid || run.OpenQuestionID.String == "" {
			return ErrCompletionNotBlocked
		}
		live = true
	case run.Status == "paused" && run.HoldReason.Valid && run.HoldReason.String == "completion_blocked":
		paused = true
	default:
		return ErrCompletionNotBlocked
	}

	if live {
		answers := []string{followupGuidance}
		if followupGuidance == "" {
			answers = []string{"continue"}
		}
		encoded, merr := json.Marshal(AnswerBody{QuestionID: run.OpenQuestionID.String, Answers: answers})
		if merr != nil {
			return fmt.Errorf("encode completion continue answer: %w", merr)
		}
		if _, err := q.CreateRunAnswerInput(ctx, store.CreateRunAnswerInputParams{
			RunID: run.ID, Body: pgconv.TextOrNull(string(encoded)), QuestionID: pgconv.TextOrNull(run.OpenQuestionID.String),
		}); err != nil {
			return err
		}
	} else if followupGuidance != "" {
		if _, err := q.CreateRunInput(ctx, store.CreateRunInputParams{
			RunID: run.ID, Kind: "follow_up", Body: pgconv.TextOrNull(followupGuidance),
		}); err != nil {
			return err
		}
	}
	if _, err := q.CreateRunInput(ctx, store.CreateRunInputParams{
		RunID: run.ID, Kind: "completion_decision", Body: pgconv.TextOrNull(auditBody),
	}); err != nil {
		return err
	}
	if _, err := q.ClearCompletionBudgetExhausted(ctx, run.ID); err != nil {
		return err
	}
	if paused {
		// PRD #1497 M1 (D6): ResumePausedRun refuses a completion_blocked hold to the owner-facing /
		// credential resume paths; the completion decision endpoint is its dedicated exception, so it
		// opts in with AllowCompletionBlockedHold. GlobalTimeoutSeconds feeds the budget guard (inert
		// here — a completion_blocked row is never budget_exhausted, so the budget clause passes).
		if _, err := q.ResumePausedRun(ctx, store.ResumePausedRunParams{
			ID:                         run.ID,
			UserID:                     userID,
			AllowCompletionBlockedHold: true,
			GlobalTimeoutSeconds:       globalTimeoutSeconds,
		}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrCompletionNotBlocked
			}
			return err
		}
	}
	return nil
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

// DecideCompletion is the widened owner-scoped completion-decision entry point (PRD #1227 M1). It
// dispatches on req.Decision:
//
//   - "continue": preserved EXACTLY as #1226 — owner-only via ContinueCompletionDecision, no
//     transaction, no contract change.
//   - "partial" / "accept": OWNER-SCOPED (GetRun authorizes the caller, exactly like continue and
//     CreateRunInput), then ONE transaction that, under the run's FOR UPDATE row lock (the mutex,
//     like completeRunWithPermit): re-checks ownership against the locked row, fences on the
//     contract revision (fresh apply / idempotent no-op / conflict), validates the decision against
//     the LOCKED contract, bumps the revision to N+1 with the revised contract, invalidates every
//     prior unconsumed permit, records the audit input + resumes the run, and commits. Every write
//     is atomic: a validation failure or a not-blocked run rolls the whole transaction back.
//
// A partial/accept decision is a WRITE that reduces/accepts scope, so it is strictly owner-only: a
// foreign caller — INCLUDING a read-only admin_ro (uza_) Bearer token, which keeps IsAdmin=true
// through RequireUser — gets ErrRunNotFound (→ 404) and cannot write another user's run. This
// preserves the read-only-ceiling invariant every other admin write in this repo enforces
// cookie-only; the binding spec's "the OWNER records an explicit later decision" is honored here.
func (s *Service) DecideCompletion(ctx context.Context, userID uuid.UUID, runID uuid.UUID, req CompletionDecisionInput) (store.Run, error) {
	switch req.Decision {
	case "continue":
		// Owner-only, byte-identical to #1226 (ContinueCompletionDecision's GetRun is owner-scoped).
		return s.ContinueCompletionDecision(ctx, userID, runID, req.Guidance)
	case "partial", "accept":
		// handled below
	default:
		return store.Run{}, fmt.Errorf("%w: unknown decision", ErrCompletionDecisionInvalid)
	}

	// Authorize the caller OWNER-SCOPED BEFORE opening the transaction: GetRun returns
	// ErrRunNotFound for a foreign/absent run (exactly as an unknown id), so a partial/accept WRITE
	// cannot touch another user's run — a read-only admin_ro (uza_) Bearer token is hidden as 404
	// like any non-owner. The locked row re-checks ownership after the lock so it cannot be raced by
	// an ownership change.
	if _, err := s.GetRun(ctx, userID, runID); err != nil {
		return store.Run{}, err
	}

	// Common validation independent of the locked contract: a non-empty, bounded reason and a
	// positive fenced revision. reason is owner-supplied free text, so NUL-strip it FIRST (a NUL is
	// not whitespace, so it would survive TrimSpace and later raise a Postgres jsonb 22P05/22021 on
	// the contract/audit write, 500ing the decision) — the SAME stripNUL-then-TrimSpace order the
	// sibling completion path uses on the reported head/branch. Then the non-empty + cap checks run
	// on the cleaned value. (The handler also caps the reason for shape; this re-checks non-empty
	// per D1 and is the authoritative gate for the CLI path.)
	cleanReason, _ := stripNUL(req.Reason)
	reason := strings.TrimSpace(cleanReason)
	if reason == "" {
		return store.Run{}, fmt.Errorf("%w: reason required", ErrCompletionDecisionInvalid)
	}
	if len(reason) > maxCompletionReasonBytes {
		return store.Run{}, fmt.Errorf("%w: reason is too long", ErrCompletionDecisionInvalid)
	}
	if req.ContractRevision <= 0 {
		return store.Run{}, fmt.Errorf("%w: contract_revision required", ErrCompletionDecisionInvalid)
	}
	// Thread the TRIMMED reason through the contract write, the audit body and the idempotency
	// comparison so all three agree byte-for-byte.
	dec := req
	dec.Reason = reason

	if s.txBeginner == nil {
		return store.Run{}, fmt.Errorf("completion decision transaction unavailable: no tx beginner wired for run %s", runID)
	}
	tx, err := s.txBeginner.Begin(ctx)
	if err != nil {
		return store.Run{}, err
	}
	// A no-op after a successful Commit; on any early return (conflict, invalid, not-blocked, infra)
	// it undoes the FOR UPDATE lock AND the revision bump / permit invalidation, so a decision that
	// does not fully apply changes nothing.
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := store.New(tx)

	locked, err := qtx.GetRunByIDForUpdate(ctx, runID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.Run{}, ErrRunNotFound
		}
		return store.Run{}, err
	}
	// Re-check ownership under the lock: the caller must own the LOCKED row. A foreign run is hidden
	// as ErrRunNotFound (no existence leak), matching the pre-lock GetRun. This closes the ownership
	// TOCTOU while staying strictly owner-only — no admin bypass, so no admin_ro token can write.
	if locked.UserID != userID {
		return store.Run{}, ErrRunNotFound
	}
	// The revision fence. An unfrozen row (revision NULL) is not revisable → conflict (fail-closed;
	// a retry after the freeze lands succeeds). Otherwise compare-and-set on cur:
	//   cur == req.ContractRevision      → FRESH apply.
	//   cur == req.ContractRevision + 1  → a possible IDEMPOTENT repeat: if the current contract
	//                                       already encodes THIS exact decision at revision cur, it
	//                                       is a retry after response loss — no writes, return 200.
	//   otherwise                        → ErrCompletionRevisionConflict (409). last-write-wins is
	//                                       refused (D1).
	if !locked.ContractRevision.Valid {
		return store.Run{}, ErrCompletionRevisionConflict
	}
	cur := int(locked.ContractRevision.Int32)
	switch {
	case cur == req.ContractRevision:
		// fresh apply — fall through to validation + write.
	case cur == req.ContractRevision+1 && decisionAlreadyEncoded(locked, dec, cur):
		// Idempotent repeat: nothing to write. The deferred Rollback releases the lock; the re-read
		// runs owner-scoped on a separate pool connection (a plain SELECT does not block on the lock).
		return s.GetRun(ctx, userID, runID)
	default:
		return store.Run{}, ErrCompletionRevisionConflict
	}

	// Validate the decision against the LOCKED contract/frozen list.
	frozenMs, _ := DecodeMilestones(locked.MilestonesFrozen)
	if err := validateDecision(locked, dec, frozenMs); err != nil {
		return store.Run{}, err
	}

	newRev := cur + 1
	revised, err := buildRevisedContract(locked.CompletionContract, dec, frozenMs, newRev)
	if err != nil {
		return store.Run{}, err
	}
	// Contract revisions are a small monotone counter (freeze at 1, +1 per owner decision), bounded
	// far below int32: cur round-trips its own int32 column value (locked.ContractRevision.Int32)
	// and newRev = cur+1, so neither conversion can lose bits.
	cur32 := int32(cur)       //nolint:gosec // G115: cur round-trips the int32 contract_revision column value
	newRev32 := int32(newRev) //nolint:gosec // G115: newRev = cur+1, a small monotone revision counter far below int32
	// Compare-and-set the revision under the lock. 0 rows (the guard: interlocked, frozen, at the
	// expected revision) → conflict — a concurrent decision moved the revision first.
	if _, err := qtx.BumpContractRevision(ctx, store.BumpContractRevisionParams{
		NewRevision:        pgtype.Int4{Int32: newRev32, Valid: true},
		CompletionContract: revised,
		RunID:              runID,
		ExpectedRevision:   pgtype.Int4{Int32: cur32, Valid: true},
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.Run{}, ErrCompletionRevisionConflict
		}
		return store.Run{}, err
	}
	// Every prior unconsumed permit is now stale against the new scope.
	if _, err := qtx.InvalidatePriorCompletionPermits(ctx, store.InvalidatePriorCompletionPermitsParams{
		RunID: runID, NewRevision: newRev32,
	}); err != nil {
		return store.Run{}, err
	}
	// Record the audit input and resume the run — the SAME mechanics the continue path uses. The
	// reason rides as the follow_up guidance (paused branch) and the audit body is the decision
	// JSON. A not-blocked run returns ErrCompletionNotBlocked here, rolling the whole transaction
	// back (the bump + invalidation never commit). Resume is scoped to the run's owner
	// (locked.UserID, which now equals userID since the caller must own the locked row); ResumePausedRun
	// fences on user_id.
	auditBody, err := decisionAuditBody(dec, frozenMs, newRev)
	if err != nil {
		return store.Run{}, err
	}
	if err := resumeCompletionBlocked(ctx, qtx, locked, locked.UserID, dec.Reason, auditBody, int32(s.p.RunTimeout.Seconds())); err != nil {
		return store.Run{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return store.Run{}, err
	}
	return s.GetRun(ctx, userID, runID)
}

// validateDecision applies the D1 semantic rules for a partial/accept decision against the LOCKED
// contract and frozen milestone list. Every violation is ErrCompletionDecisionInvalid wrapped with
// a safe, client-showable reason (no user data). The reason is validated by the caller.
func validateDecision(locked store.Run, dec CompletionDecisionInput, frozenMs []Milestone) error {
	prior, err := parseCompletionContract(locked.CompletionContract)
	if err != nil {
		// A frozen run at a valid revision should have a non-null contract; a NULL/corrupt one is a
		// transient split-state, not a client error — refuse as a conflict (retryable), not a 400.
		return ErrCompletionRevisionConflict
	}
	frozen := make(map[string]bool, len(frozenMs))
	for _, m := range frozenMs {
		frozen[m.ID] = true
	}
	currentDeferred := make(map[string]bool)
	if prior.Scope != nil {
		for _, d := range prior.Scope.Out {
			currentDeferred[d.MilestoneID] = true
		}
	}
	// Milestone ids that already carry an owner-accepted criterion (PRD #1227 D3). A partial must not
	// defer such a milestone: the revised contract would then place it in scope.out AND keep its
	// criterion in accepted — contradictory for the M4 DTO. This is symmetric with the accept arm
	// refusing to accept a criterion whose milestone is already deferred.
	acceptedMilestone := make(map[string]bool, len(prior.Accepted))
	for _, a := range prior.Accepted {
		acceptedMilestone[a.MilestoneID] = true
	}

	switch dec.Decision {
	case "partial":
		if len(dec.Keep) == 0 {
			return fmt.Errorf("%w: keep set is empty", ErrCompletionDecisionInvalid)
		}
		seen := make(map[string]bool, len(dec.Keep))
		for _, id := range dec.Keep {
			if seen[id] {
				return fmt.Errorf("%w: duplicate milestone id", ErrCompletionDecisionInvalid)
			}
			seen[id] = true
			if !frozen[id] {
				return fmt.Errorf("%w: unknown milestone id", ErrCompletionDecisionInvalid)
			}
			// A keep id that is CURRENTLY deferred would be a restoration (widening scope), which is
			// not a partial. keep must be a subset of the current in-scope set (frozen − currentOut).
			if currentDeferred[id] {
				return fmt.Errorf("%w: milestone is out of scope", ErrCompletionDecisionInvalid)
			}
		}
		// A milestone being deferred (frozen − keep) must not already have an owner-accepted
		// criterion, or the revised contract would contradict itself (scope.out ∩ accepted).
		for _, m := range frozenMs {
			if !seen[m.ID] && acceptedMilestone[m.ID] {
				return fmt.Errorf("%w: cannot defer an already-accepted milestone", ErrCompletionDecisionInvalid)
			}
		}
		// The decision must STRICTLY add at least one new deferral (a frozen milestone that is
		// neither kept nor already deferred). Otherwise it removes nothing from scope — a no-op.
		newlyDeferred := 0
		for _, m := range frozenMs {
			if !seen[m.ID] && !currentDeferred[m.ID] {
				newlyDeferred++
			}
		}
		if newlyDeferred == 0 {
			return fmt.Errorf("%w: partial removes no milestone from scope", ErrCompletionDecisionInvalid)
		}
		return nil
	case "accept":
		if len(dec.Criteria) == 0 {
			return fmt.Errorf("%w: criteria set is empty", ErrCompletionDecisionInvalid)
		}
		critByID := make(map[string]completionCriterion, len(prior.Criteria))
		for _, cr := range prior.Criteria {
			critByID[cr.ID] = cr
		}
		completed, _ := DecodeMilestoneIDs(locked.MilestonesCompleted)
		completedSet := make(map[string]bool, len(completed))
		for _, id := range completed {
			completedSet[id] = true
		}
		alreadyAccepted := make(map[string]bool, len(prior.Accepted))
		for _, a := range prior.Accepted {
			alreadyAccepted[a.ID] = true
		}
		seen := make(map[string]bool, len(dec.Criteria))
		for _, id := range dec.Criteria {
			if seen[id] {
				return fmt.Errorf("%w: duplicate criterion id", ErrCompletionDecisionInvalid)
			}
			seen[id] = true
			cr, ok := critByID[id]
			if !ok {
				return fmt.Errorf("%w: unknown criterion id", ErrCompletionDecisionInvalid)
			}
			if completedSet[cr.MilestoneID] {
				return fmt.Errorf("%w: milestone already satisfied", ErrCompletionDecisionInvalid)
			}
			if currentDeferred[cr.MilestoneID] {
				return fmt.Errorf("%w: criterion is out of scope", ErrCompletionDecisionInvalid)
			}
			if alreadyAccepted[id] {
				return fmt.Errorf("%w: criterion already accepted", ErrCompletionDecisionInvalid)
			}
		}
		return nil
	}
	return fmt.Errorf("%w: unknown decision", ErrCompletionDecisionInvalid)
}

// decisionAlreadyEncoded reports whether the LOCKED contract (already at revision cur ==
// req.ContractRevision+1) encodes THIS exact decision — the retry-after-response-loss test that
// makes a repeated identical submission idempotent (D1). It compares the effect of the decision, not
// its inputs: a partial matches when scope.in equals the keep set, scope.out's milestone set equals
// (frozen − keep), and the entries deferred AT revision cur carry this reason; an accept matches
// when the accepted entries AT revision cur are exactly the criteria set with this reason.
func decisionAlreadyEncoded(locked store.Run, dec CompletionDecisionInput, cur int) bool {
	c, err := parseCompletionContract(locked.CompletionContract)
	if err != nil {
		return false
	}
	switch dec.Decision {
	case "partial":
		if c.Scope == nil {
			return false
		}
		if !sameStringSet(c.Scope.In, dec.Keep) {
			return false
		}
		frozenMs, _ := DecodeMilestones(locked.MilestonesFrozen)
		keep := make(map[string]bool, len(dec.Keep))
		for _, k := range dec.Keep {
			keep[k] = true
		}
		wantOut := make(map[string]bool)
		for _, m := range frozenMs {
			if !keep[m.ID] {
				wantOut[m.ID] = true
			}
		}
		gotOut := make(map[string]bool, len(c.Scope.Out))
		sawCur := false
		for _, d := range c.Scope.Out {
			gotOut[d.MilestoneID] = true
			if d.Revision == cur {
				sawCur = true
				if d.Reason != dec.Reason {
					return false
				}
			}
		}
		return sawCur && sameBoolSet(wantOut, gotOut)
	case "accept":
		want := make(map[string]bool, len(dec.Criteria))
		for _, id := range dec.Criteria {
			want[id] = true
		}
		got := make(map[string]bool)
		for _, a := range c.Accepted {
			if a.Revision == cur {
				got[a.ID] = true
				if a.Reason != dec.Reason {
					return false
				}
			}
		}
		return len(got) > 0 && sameBoolSet(want, got)
	}
	return false
}

// decisionAuditBody builds the completion_decision audit input body (PRD #1227 M1): the decision
// JSON recorded on the run's input history. For a partial the deferred list is the full
// out-of-scope set (frozen − keep), sorted; for an accept the accepted criterion ids.
func decisionAuditBody(dec CompletionDecisionInput, frozenMs []Milestone, newRev int) (string, error) {
	switch dec.Decision {
	case "partial":
		keep := make(map[string]bool, len(dec.Keep))
		for _, k := range dec.Keep {
			keep[k] = true
		}
		deferred := make([]string, 0, len(frozenMs))
		for _, m := range frozenMs {
			if !keep[m.ID] {
				deferred = append(deferred, m.ID)
			}
		}
		sort.Strings(deferred)
		b, err := json.Marshal(struct {
			Decision string   `json:"decision"`
			Keep     []string `json:"keep"`
			Deferred []string `json:"deferred"`
			Reason   string   `json:"reason"`
			Revision int      `json:"revision"`
		}{Decision: "partial", Keep: dec.Keep, Deferred: deferred, Reason: dec.Reason, Revision: newRev})
		return string(b), err
	case "accept":
		b, err := json.Marshal(struct {
			Decision string   `json:"decision"`
			Criteria []string `json:"criteria"`
			Reason   string   `json:"reason"`
			Revision int      `json:"revision"`
		}{Decision: "accept", Criteria: dec.Criteria, Reason: dec.Reason, Revision: newRev})
		return string(b), err
	}
	return "", fmt.Errorf("decisionAuditBody: unsupported decision %q", dec.Decision)
}

// sameStringSet reports whether two string slices contain the same DISTINCT elements (order- and
// duplicate-insensitive).
func sameStringSet(a, b []string) bool {
	as := make(map[string]bool, len(a))
	for _, x := range a {
		as[x] = true
	}
	bs := make(map[string]bool, len(b))
	for _, x := range b {
		bs[x] = true
	}
	return sameBoolSet(as, bs)
}

// sameBoolSet reports whether two set-valued maps have the same keys.
func sameBoolSet(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}
