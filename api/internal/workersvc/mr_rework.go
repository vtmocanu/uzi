package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// RunKindMRRework is the runs.kind for an MR review-watcher rework run (PRD #700 M3),
// mirroring the runs_kind_check domain. It is the sibling of RunKindCIFix: a
// poller-detected, fully-automatic run that folds review-comment fixes onto an
// existing agent MR branch.
const RunKindMRRework = "mr_rework"

// ErrActiveMRReworkExists is returned when an mr_rework run already exists for the MR
// (backed by the uq_runs_one_active_mr_rework partial index on (repo_id, mr_iid)).
// The detector swallows it and retries next tick; a handler would map it to 409.
var ErrActiveMRReworkExists = errors.New("an active MR-rework run already exists for this merge request")

// CreateAutoMRReworkRun queues an mr_rework run for a completed run's MR that gained
// new review comments on a green pipeline (PRD #700 M3, the sibling of
// CreateAutoCIFixRun). It is always AUTOMATIC — there is no manual mr_rework trigger
// (Decision 13) — so it always sets auto_approve=true (the worker resolves its own
// plan gate, Decision 1).
//
// ref is the agent/issue-N branch (also the pipeline_ref branch-guard key, written AT
// INSERT so the cross-kind guard below is never create-time-NULL — Decision 6). mrIID
// is the MR the rework folds onto; sourceRunID is the completed run whose MR is
// watched (stored as target_run_id, mirroring judge). snapshot is the MR review
// snapshot the DETECTOR already built and filtered — it rides this create path
// explicitly (Decision 8), NOT via CreateRun's issue-comment fetch — and may be nil
// (stored NULL).
//
// Two guards run before the insert:
//   - the create-time CROSS-KIND branch guard (CountActiveBranchRunsForRef counts
//     active ci_fix OR mr_rework runs on the same pipeline_ref) → ErrBranchInUse, so a
//     ci_fix and an mr_rework never share one worktree (they fire on opposite CI
//     states, so this only bites a genuine race);
//   - the one-active-mr_rework-per-MR unique index (a second rework on the same MR →
//     ErrActiveMRReworkExists).
//
// The detector swallows both and retries next tick, exactly as ci-autofix does.
func (s *Service) CreateAutoMRReworkRun(ctx context.Context, userID, repoID uuid.UUID, ref string, mrIID int64, sourceRunID uuid.UUID, title, description string, snapshot *ReviewCommentsSnapshot) (store.Run, error) {
	// Repo-ownership / existence check, mirroring createCIFixRun so an unknown repo is
	// a clean ErrRepoNotFound rather than an FK error at INSERT.
	if _, err := s.q.GetRepoForUser(ctx, store.GetRepoForUserParams{ID: repoID, UserID: userID}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.Run{}, ErrRepoNotFound
		}
		return store.Run{}, err
	}

	// Cross-kind create-time branch guard (Decision 6). pipeline_ref is populated AT
	// INSERT for both ci_fix and mr_rework, so this count sees a freshly-created
	// sibling — unlike the legacy runs.branch count, which is NULL for a run's whole
	// active life.
	active, err := s.q.CountActiveBranchRunsForRef(ctx, store.CountActiveBranchRunsForRefParams{
		RepoID:      repoID,
		PipelineRef: pgtype.Text{String: ref, Valid: true},
	})
	if err != nil {
		return store.Run{}, err
	}
	if active > 0 {
		return store.Run{}, ErrBranchInUse
	}

	// The MR review snapshot rides the create path explicitly (Decision 8). A nil
	// snapshot stores NULL (sqlc.narg): the detector only proceeds with a non-nil
	// snapshot, but the marshal stays nil-safe so a future caller cannot ship a
	// half-populated column.
	var reviewJSON []byte
	if snapshot != nil {
		b, err := json.Marshal(snapshot)
		if err != nil {
			return store.Run{}, fmt.Errorf("marshal review snapshot: %w", err)
		}
		reviewJSON = b
	}

	run, err := s.q.CreateAutoMRReworkRun(ctx, store.CreateAutoMRReworkRunParams{
		UserID:           userID,
		RepoID:           repoID,
		IssueTitle:       title,
		IssueDescription: description,
		PipelineRef:      pgtype.Text{String: ref, Valid: true},
		MrIid:            pgtype.Int8{Int64: mrIID, Valid: true},
		TargetRunID:      pgtype.UUID{Bytes: sourceRunID, Valid: true},
		ReviewComments:   reviewJSON,
		// PRD #35: the OWNER's default. An mr_rework run is created by the poller with
		// no user in the loop, so there is no per-run request to honour.
		WaitOnLimit: s.resolveWaitOnLimit(ctx, userID, nil),
	})
	if err != nil {
		if isUniqueViolation(err) {
			return store.Run{}, ErrActiveMRReworkExists
		}
		return store.Run{}, err
	}
	// queued drives no board-column move for an mr_rework run (issue_iid is NULL, so
	// notifyOnce skips it), but firing the hook keeps the live status broadcast
	// consistent with issue runs.
	s.notify(run.ID, "queued")
	return run, nil
}
