package workersvc

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// ErrActiveSelfImproveExists is returned when a non-terminal self_improve run
// already exists (backed by the uq_runs_one_active_self_improve partial index). The
// engine treats it as "a cycle is still in flight" and skips the tick (Decision 9);
// it is never surfaced to a user (only the engine calls CreateSelfImproveRun).
var ErrActiveSelfImproveExists = errors.New("a self-improvement run is already active")

// CreateSelfImproveRun queues the autonomous self-improvement run against uzi's
// own repo (PRD #46 Decision 10). It is shaped like CreateCIFixRun, deliberately
// NOT createRun: the normal path requires the issue to be in the poller cache and
// to carry a PRD link, and a just-filed tracking issue satisfies neither (review
// B2). The engine has already resolved the tracking issue and composed the planning
// material; this method validates that repoID is a repo the enabling admin owns,
// then inserts an issue-shaped, auto_approve, kind='self_improve' run snapshotting
// title/description directly (issue_description carries the accumulated improve_uzi
// backlog, which the worker frames as untrusted data). A second active run is
// rejected by the partial unique index → ErrActiveSelfImproveExists.
func (s *Service) CreateSelfImproveRun(ctx context.Context, userID, repoID uuid.UUID, issueIID int64, title, description string) (store.Run, error) {
	row, err := s.q.GetRepoForUser(ctx, store.GetRepoForUserParams{ID: repoID, UserID: userID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.Run{}, ErrRepoNotFound
		}
		return store.Run{}, err
	}
	// #66 D1 layer 2: the shared service-layer guardrail. The self-improve engine tick
	// has no user in the loop, so a refusal here just skips the cycle (logged upstream),
	// which is the desired outcome — never a run against an unguarded default branch.
	if err := s.guardDefaultBranch(ctx, row); err != nil {
		return store.Run{}, err
	}
	run, err := s.q.CreateSelfImproveRun(ctx, store.CreateSelfImproveRunParams{
		UserID:           userID,
		RepoID:           repoID,
		IssueIid:         pgtype.Int8{Int64: issueIID, Valid: true},
		IssueTitle:       title,
		IssueDescription: description,
		// PRD #35: the OWNER's default. An engine tick has no user in the loop either.
		WaitOnLimit: s.resolveWaitOnLimit(ctx, userID, nil),
	})
	if err != nil {
		if isUniqueViolation(err) {
			return store.Run{}, ErrActiveSelfImproveExists
		}
		return store.Run{}, err
	}
	// queued drives the tracking issue's board move via runlifecycle (accepted side
	// effect, review N1) and keeps the live status broadcast consistent with other
	// issue-shaped runs.
	s.notify(run.ID, "queued")
	return run, nil
}
