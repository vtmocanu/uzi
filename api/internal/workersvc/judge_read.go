package workersvc

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Judge surfacing errors (PRD #46 M4). Separate from the enqueue funnel's silent
// gates: the re-run action is an explicit user request, so a blocked re-run returns a
// typed error the handler maps to a specific status, rather than being swallowed.
var (
	// ErrRunNotJudgeable — the target isn't a completed/failed run of an eligible
	// kind, so there is nothing a judge could review (a queued/running/cancelled run,
	// or a judge/self_improve/chat run).
	ErrRunNotJudgeable = errors.New("run cannot be judged")
	// ErrJudgeDisabled — the global judge_enabled kill-switch is off. A manual re-run
	// respects the instance-wide switch just like the automatic funnel does.
	ErrJudgeDisabled = errors.New("run judging is disabled")
	// ErrNoAnthropicToken — the run owner has no Anthropic token, so there is nothing
	// to spend on a judge.
	ErrNoAnthropicToken = errors.New("no anthropic token to run the judge")
	// ErrJudgeAlreadyActive — a judge run is already queued/running for this target
	// (the one-active-judge-per-target unique index). Re-running is a no-op, surfaced
	// so the UI can say "already re-judging" instead of silently doing nothing.
	ErrJudgeAlreadyActive = errors.New("a judge run is already active for this run")
	// ErrNotRunOwner — the caller can SEE the run (they are an admin) but does not
	// OWN it. A judge spends the owner's token, so only the owner may re-run it; an
	// admin gets a clear 403 rather than a misleading not-found (audit H3).
	ErrNotRunOwner = errors.New("only the run owner can run the judge")
)

// ReviewWithRecommendations is the run-page review payload: the verdict row plus its
// structured recommendations, all already scrubbed + capped at ingest (M3), and the
// settled recommendation→forge-issue links for the review (PRD #68) so the panel renders
// a filed row (and a stale-filed flag) instead of the File-issue button without a second
// fetch.
type ReviewWithRecommendations struct {
	Review          store.RunReview
	Recommendations []store.ReviewRecommendation
	FiledIssues     []store.RecommendationFiledIssue
	// Dispositions are the user's triage verdicts on this review's coordinates (PRD #94),
	// keyed like the filed links so they survive a re-judge; the DTO matches them to the
	// current recommendations by (category, target) and flags stale via the rationale hash.
	Dispositions []store.RecommendationDisposition
	// JudgeRun is the judge run's own timing + token/cost usage (PRD #69 M6, Decision 10):
	// the claim/start/finish stamps and the run_usage_totals rollup for the judge run.
	// nil only when there is no judge run row for the review's judge_run_id (not expected
	// while a review exists); its usage columns are NULL for a pre-feature judge that
	// posted no result frame, which the DTO renders as an absent strip.
	JudgeRun *store.GetJudgeRunUsageForTargetRow
}

// PendingJudge is the ACTIVE judge run for a target (PRD #119 M1) — the fact that a
// verdict is already coming, which the run page cannot otherwise tell apart from "never
// judged" (both are review == nil). Status is the RAW runs.status of the judge run
// ('queued'/'claimed'/'running'/… — whatever the index's active set admits); it is
// normalized to a display value at the DTO boundary (handler.pendingJudgeState), never
// here, so the service keeps reporting what the database says and only one layer owns
// the presentation vocabulary. EnqueuedAt is the judge run's created_at.
type PendingJudge struct {
	Status     string
	EnqueuedAt time.Time
}

// GetReviewForTarget returns the judge's review of a run
// (PRD #46 M4, Decision 5/6). Visibility reuses the owner-or-admin GetRunForViewer
// rule (audit M2 / M3 carry-forward): a run the viewer can't see is ErrRunNotFound,
// exactly as an unknown id. A visible run that has NOT been judged yet returns
// (nil, nil) — a legitimate "no review", distinct from "no such run", so the handler
// can render an empty panel rather than a 404.
//
// This is the REVIEW-ONLY read, used by the callers that want a verdict and nothing
// else (the issue-draft/file flows and the disposition write path). The run page uses
// GetRunReviewPanel instead — see the note there for why the signature stayed narrow.
func (s *Service) GetReviewForTarget(ctx context.Context, userID uuid.UUID, isAdmin bool, targetRunID uuid.UUID) (*ReviewWithRecommendations, error) {
	if _, err := s.GetRunForViewer(ctx, userID, isAdmin, targetRunID); err != nil {
		return nil, err
	}
	return s.reviewForTarget(ctx, targetRunID)
}

// GetRunReviewPanel is the run page's whole review payload (PRD #119 M1): the verdict
// (nil when unjudged) AND the active judge run (nil when none). The two are INDEPENDENT
// — an unjudged run with an auto-judge in flight is (nil, pending); a re-judge of an
// already-judged run is (review, pending); a settled run is (review, nil); a run nobody
// ever judged is (nil, nil).
//
// It is its own method rather than a widened GetReviewForTarget deliberately.
// GetReviewForTarget HAD four callers; the panel moved off it onto this method, leaving
// three (issue-draft, issue-file, the disposition write path), and all three want a
// verdict to act on and have no use for a pending judge. Widening the signature instead
// would have made every one of them carry and discard a value, and would have put a
// second query on write paths that do not need it. The panel is the only caller that
// needs both, so the panel gets the method that fetches both.
//
// The visibility gate runs ONCE, before either read: an invisible run is ErrRunNotFound
// with no pending-judge query issued at all, so this route can never be used to probe
// whether some other user's run is being judged.
func (s *Service) GetRunReviewPanel(ctx context.Context, userID uuid.UUID, isAdmin bool, targetRunID uuid.UUID) (*ReviewWithRecommendations, *PendingJudge, error) {
	if _, err := s.GetRunForViewer(ctx, userID, isAdmin, targetRunID); err != nil {
		return nil, nil, err
	}
	review, err := s.reviewForTarget(ctx, targetRunID)
	if err != nil {
		return nil, nil, err
	}
	// The judge run's own timing + usage strip (PRD #69 M6) is a PANEL-only concern, so it
	// is fetched here rather than in the shared reviewForTarget. Only when a review exists
	// (a judged run): an unjudged run has no judge_run_id to look up.
	if review != nil {
		judgeRun, jerr := s.judgeRunUsageForTarget(ctx, targetRunID)
		if jerr != nil {
			return nil, nil, jerr
		}
		review.JudgeRun = judgeRun
	}
	pending, err := s.pendingJudgeForTarget(ctx, targetRunID)
	if err != nil {
		return nil, nil, err
	}
	return review, pending, nil
}

// pendingJudgeForTarget reads the active judge run for a target, or (nil, nil) when
// there is none. Callers MUST have already applied the GetRunForViewer visibility gate —
// this is a plain by-target lookup, exactly like GetRunReviewForTarget, and carries no
// scoping of its own.
func (s *Service) pendingJudgeForTarget(ctx context.Context, targetRunID uuid.UUID) (*PendingJudge, error) {
	row, err := s.q.GetActiveJudgeRunForTarget(ctx, pgconv.UUID(targetRunID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil // no judge in flight: the "Run judge" button is legitimately live
	}
	if err != nil {
		return nil, err
	}
	return &PendingJudge{Status: row.Status, EnqueuedAt: row.CreatedAt.Time}, nil
}

// reviewForTarget is the UNGATED body of the review read: it assumes the caller has
// already established that the viewer may see the target run. Split out of
// GetReviewForTarget so GetRunReviewPanel can reuse it behind ONE visibility check
// instead of running GetRunForViewer twice. The (nil, nil) "visible but not judged"
// contract lives here and is what both callers return.
func (s *Service) reviewForTarget(ctx context.Context, targetRunID uuid.UUID) (*ReviewWithRecommendations, error) {
	review, err := s.q.GetRunReviewForTarget(ctx, targetRunID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil // visible run, not judged (yet)
	}
	if err != nil {
		return nil, err
	}
	recs, err := s.q.ListRecommendationsForReview(ctx, review.ID)
	if err != nil {
		return nil, err
	}
	// The filed-issue links (PRD #68): keyed by the review, they survive a re-judge's
	// recommendation delete-reinsert, so the panel matches them to recommendations by
	// (category, target) and renders the filed state.
	filed, err := s.q.ListFiledIssuesForReview(ctx, review.ID)
	if err != nil {
		return nil, err
	}
	// The dispositions (PRD #94): also keyed by the review, they survive the re-judge
	// delete-reinsert, so the DTO matches them to recommendations by (category, target) and
	// renders the triage chip + the (hash-based) stale flag.
	dispositions, err := s.q.ListDispositionsForReview(ctx, review.ID)
	if err != nil {
		return nil, err
	}
	return &ReviewWithRecommendations{Review: review, Recommendations: recs, FiledIssues: filed, Dispositions: dispositions}, nil
}

// judgeRunUsageForTarget reads the judge run's own timing + usage for the review panel
// (PRD #69 M6, Decision 10). A review always has a judge_run_id, so this returns a row;
// its usage columns are NULL for a pre-feature judge (no result frame → no run_usage
// row). pgx.ErrNoRows would mean the judge run row itself is gone — treated as "no
// judge-run detail" (nil) rather than failing the panel read. It is fetched ONLY by the
// panel path (not the shared reviewForTarget), so the disposition/issue-draft/issue-file
// callers of GetReviewForTarget do not pay for a query they never render.
func (s *Service) judgeRunUsageForTarget(ctx context.Context, targetRunID uuid.UUID) (*store.GetJudgeRunUsageForTargetRow, error) {
	jr, err := s.q.GetJudgeRunUsageForTarget(ctx, targetRunID)
	switch {
	case err == nil:
		return &jr, nil
	case errors.Is(err, pgx.ErrNoRows):
		return nil, nil
	default:
		return nil, err
	}
}

// RerunJudge enqueues a fresh judge run for a terminal run at the OWNER's explicit
// request (PRD #46 Decision 8 — the "re-run judge" action). Spend is owner-scoped
// (audit H3): the judge spends the run owner's Anthropic token, so only the owner may
// trigger it. A non-owner non-admin can't even see the run (ErrRunNotFound); an admin
// can see it but is refused with ErrNotRunOwner — an admin cannot redirect autonomous
// spend onto another user.
//
// Gates mirror the automatic funnel except the per-user opt-in: the explicit click on
// one's own run IS the consent (the opt-in flag gates the automatic path, not a
// deliberate one-off). Still enforced: eligible terminal status + kind, the global
// kill-switch, and an owner token to spend. The one-active-judge-per-target unique
// index dedupes a double-click (23505 → ErrJudgeAlreadyActive); a prior review is
// replaced by the new judge's PostReview UPSERT.
func (s *Service) RerunJudge(ctx context.Context, userID uuid.UUID, isAdmin bool, targetRunID uuid.UUID) (store.Run, error) {
	target, err := s.GetRunForViewer(ctx, userID, isAdmin, targetRunID)
	if err != nil {
		return store.Run{}, err // ErrRunNotFound when not even visible
	}
	if target.UserID != userID {
		return store.Run{}, ErrNotRunOwner // visible to an admin, but not theirs to spend
	}
	if target.Status != "completed" && target.Status != "failed" {
		return store.Run{}, ErrRunNotJudgeable
	}
	if !judgeEligibleKinds[target.Kind] {
		return store.Run{}, ErrRunNotJudgeable
	}
	if s.settings == nil {
		return store.Run{}, ErrJudgeDisabled
	}
	enabled, err := s.settings.JudgeEnabled(ctx)
	if err != nil {
		return store.Run{}, err
	}
	if !enabled {
		return store.Run{}, ErrJudgeDisabled
	}
	if _, err := s.q.GetUserSecretCiphertext(ctx, store.GetUserSecretCiphertextParams{
		UserID: target.UserID,
		Kind:   store.KindAnthropicToken,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.Run{}, ErrNoAnthropicToken
		}
		return store.Run{}, err
	}
	judge, err := s.q.CreateJudgeRun(ctx, store.CreateJudgeRunParams{
		UserID:           target.UserID,
		TargetRunID:      pgconv.UUID(target.ID),
		IssueTitle:       judgeRunTitle(target),
		IssueDescription: "",
		TriggerSource:    "judge_rerun",
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return store.Run{}, ErrJudgeAlreadyActive
		}
		return store.Run{}, err
	}
	logRunCreated(judge)
	return judge, nil
}
