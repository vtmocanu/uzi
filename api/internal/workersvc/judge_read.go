package workersvc

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
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
}

// GetReviewForTarget returns the judge's review of a run for the run-page panel
// (PRD #46 M4, Decision 5/6). Visibility reuses the owner-or-admin GetRunForViewer
// rule (audit M2 / M3 carry-forward): a run the viewer can't see is ErrRunNotFound,
// exactly as an unknown id. A visible run that has NOT been judged yet returns
// (nil, nil) — a legitimate "no review", distinct from "no such run", so the handler
// can render an empty panel rather than a 404.
func (s *Service) GetReviewForTarget(ctx context.Context, userID uuid.UUID, isAdmin bool, targetRunID uuid.UUID) (*ReviewWithRecommendations, error) {
	if _, err := s.GetRunForViewer(ctx, userID, isAdmin, targetRunID); err != nil {
		return nil, err
	}
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
	return &ReviewWithRecommendations{Review: review, Recommendations: recs, FiledIssues: filed}, nil
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
		TargetRunID:      pgUUID(target.ID),
		IssueTitle:       judgeRunTitle(target),
		IssueDescription: "",
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return store.Run{}, ErrJudgeAlreadyActive
		}
		return store.Run{}, err
	}
	return judge, nil
}
