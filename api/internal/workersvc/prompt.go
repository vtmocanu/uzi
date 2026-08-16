package workersvc

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// ErrActivePromptExists is returned when a non-terminal prompt run already exists
// for the schedule (backed by the uq_runs_one_active_prompt_per_schedule partial
// index). The scheduler treats it as "a prior prompt run is still in flight" and
// advances the schedule without firing again (Decision 5-style dedup); it is never
// surfaced to a user (only the scheduler calls CreatePromptRun). Mirrors
// ErrActiveSelfImproveExists.
var ErrActivePromptExists = errors.New("a prompt run is already active for this schedule")

// CreatePromptRun queues a kind='prompt' run for a scheduled prompt fire (PRD #241).
// It is shaped like CreateSelfImproveRun, deliberately NOT createRun: a prompt run is
// repo-ful but ISSUE-LESS (no forge issue, no PRD link), so the normal path's
// cached-PRD-issue and PRD-link gates do not apply. The prompt text is snapshotted
// directly as the run's issue_description and a caller-derived title as its
// issue_title, so the run view is self-contained.
//
// Ownership is still enforced: the prompt path bypasses createRun, where
// GetRepoForUser normally validates that repoID is a repo userID owns, so this
// method replicates that consent check up front (else ownership is silently
// dropped). The #66 default-branch guardrail (D1 layer 2) also gates this path:
// a prompt run is scheduler-fired unattended and receives the bot PAT at claim,
// so an un-gated scheduled prompt against a repo whose bot can reach the default
// branch would bypass the guardrail entirely — hence guardDefaultBranch runs on
// the fetched repo row before the insert, identical to the other PAT-bearing
// run-create paths. auto_approve and wait_on_limit ride straight from the schedule
// the owner configured. A second active run for the schedule is rejected by the
// partial unique index → ErrActivePromptExists.
func (s *Service) CreatePromptRun(ctx context.Context, userID, repoID, scheduleID uuid.UUID, title, prompt string, autoApprove, waitOnLimit bool, model *string, overrideSubagentModel bool) (store.Run, error) {
	row, err := s.q.GetRepoForUser(ctx, store.GetRepoForUserParams{ID: repoID, UserID: userID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.Run{}, ErrRepoNotFound
		}
		return store.Run{}, err
	}
	if err := s.guardDefaultBranch(ctx, row); err != nil {
		return store.Run{}, err
	}
	run, err := s.q.CreatePromptRun(ctx, store.CreatePromptRunParams{
		UserID:           userID,
		RepoID:           repoID,
		ScheduleID:       scheduleID,
		IssueTitle:       title,
		IssueDescription: prompt,
		AutoApprove:      autoApprove,
		WaitOnLimit:      waitOnLimit,
		// PRD #300: the schedule's per-schedule model override, frozen onto this run.
		// nil → NULL → the run inherits the owner's per-user Worker default.
		Model: pgTextPtr(model),
		// PRD #305: the schedule's "apply model also to agents" opt-in, frozen onto this
		// run at fire time (M1 stores only; delivery M3, worker behaviour M4).
		OverrideSubagentModel: overrideSubagentModel,
	})
	if err != nil {
		if isUniqueViolation(err) {
			return store.Run{}, ErrActivePromptExists
		}
		return store.Run{}, err
	}
	// queued keeps the live status broadcast consistent with other run kinds.
	s.notify(run.ID, "queued")
	return run, nil
}
