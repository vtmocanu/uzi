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
// A single atomic guard runs AS the insert: CreateAutoMRReworkRun is an
// INSERT … WHERE NOT EXISTS whose predicate matches an active ci_fix run on the same
// pipeline_ref (Decision 6, the create-time CROSS-KIND branch guard — narrowed to the
// cross-kind case only), so a ci_fix and an mr_rework never share one worktree (they
// fire on opposite CI states, so this only bites a genuine race). It reports the branch
// is occupied by the other kind two ways:
//   - a committed ci_fix → zero rows inserted → pgx.ErrNoRows → ErrBranchInUse (the
//     sequential path);
//   - a concurrent-window ci_fix the INSERT's snapshot could not see → the durable
//     uq_runs_one_active_branch_ref spanning index arbitrates and the losing insert
//     raises 23505 on it → ErrBranchInUse.
//
// A second active rework on the SAME MR is a distinct constraint: because the predicate
// above no longer matches mr_rework, a same-MR duplicate proceeds past WHERE NOT EXISTS
// and reaches the uq_runs_one_active_mr_rework unique index (23505 →
// ErrActiveMRReworkExists) — previously that path was shadowed by the broader predicate,
// which returned ErrBranchInUse for a same-MR duplicate.
//
// The detector swallows all of these and retries next tick, exactly as ci-autofix does.
func (s *Service) CreateAutoMRReworkRun(ctx context.Context, userID, repoID uuid.UUID, ref string, mrIID int64, sourceRunID uuid.UUID, title, description string, snapshot *ReviewCommentsSnapshot) (store.Run, error) {
	// Repo-ownership / existence check, mirroring createCIFixRun so an unknown repo is
	// a clean ErrRepoNotFound rather than an FK error at INSERT.
	if _, err := s.q.GetRepoForUser(ctx, store.GetRepoForUserParams{ID: repoID, UserID: userID}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.Run{}, ErrRepoNotFound
		}
		return store.Run{}, err
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
		if errors.Is(err, pgx.ErrNoRows) {
			// WHERE NOT EXISTS matched an active cross-kind ci_fix sibling on this pipeline_ref:
			// the branch is occupied. (Sequential path — a committed sibling is visible.)
			return store.Run{}, ErrBranchInUse
		}
		if uniqueViolationOn(err, "uq_runs_one_active_branch_ref") {
			// Concurrent-window race: the durable cross-kind spanning index arbitrated and
			// this insert lost. A ci_fix (or another mr_rework) holds the branch → ErrBranchInUse.
			return store.Run{}, ErrBranchInUse
		}
		if isUniqueViolation(err) {
			// uq_runs_one_active_mr_rework: a second active rework on the same MR. A same-MR
			// duplicate actually trips BOTH partial indexes at once (same MR ⇒ same
			// pipeline_ref), so this branch is reached only because Postgres reports the
			// violation on uq_runs_one_active_mr_rework FIRST — it is created before
			// uq_runs_one_active_branch_ref in migration 00167 (lower OID), so the
			// uq_runs_one_active_branch_ref check above does not match. That ordering is
			// pinned by TestCreateAutoMRReworkRunSameMRDuplicateIsActiveExistsLiveDB; a
			// migration that recreated the two indexes in reverse order would regress this.
			return store.Run{}, ErrActiveMRReworkExists
		}
		return store.Run{}, err
	}
	// queued drives no board-column move for an mr_rework run (issue_iid is NULL, so
	// notifyOnce skips it), but firing the hook keeps the live status broadcast
	// consistent with issue runs.
	s.notify(run.ID, "queued")
	logRunCreated(run)
	return run, nil
}
