package workersvc

import (
	"context"
	"errors"
	"math"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// taskBranchPrefix is the server-owned namespace every task branch lives in
// (PRD #400 Decision 4). A task's destination is NEVER caller-controlled in v1, so
// a branch in this namespace is safe by construction — it can never be a
// protected/default branch. The create-time assertion below re-checks it anyway
// (defense in depth).
const taskBranchPrefix = "uzi/task/"

// ErrTaskBranchUnsafe is returned when the server-derived task branch fails the
// create-time safety assertion (not in uzi/task/*, or equal to the repo's default
// branch). Because CreateTaskRun mints "uzi/task/<uuid>" itself this can never fire
// in practice, but the check is a load-bearing guardrail (PRD #400 Decision 8) that
// must stay real, so it maps to an error rather than being elided.
var ErrTaskBranchUnsafe = errors.New("task branch is not in the uzi/task/* namespace or resolves to the default branch")

// maxTaskBaseBranchBytes bounds the optional base_branch (the source ref a task
// branches from). A git ref cannot legitimately approach this, so it is a cheap
// dedicated cap on top of the request-body limit rather than a semantic constraint.
const maxTaskBaseBranchBytes = 512

// ErrTaskBaseBranchTooLong is returned when the trimmed base_branch exceeds
// maxTaskBaseBranchBytes (PRD #400) → 400. Mapped in writeStartRunError.
var ErrTaskBaseBranchTooLong = errors.New("base branch is too long")

// CreateTaskRun queues a kind='task' run for a uzi handoff (PRD #400). It is shaped
// like CreatePromptRun, deliberately NOT createRun: a task run is repo-ful but
// ISSUE-LESS (no forge issue, no PRD link), so the normal path's cached-PRD-issue and
// PRD-link gates do not apply. The inline context is snapshotted directly as the run's
// issue_description (inheriting the 256 KiB cap + NUL sanitization), and a derived
// first-line title as its issue_title, so the run view is self-contained.
//
// Unlike every other kind, a task's branch is known AT CREATE: the destination is the
// server-named uzi/task/<run-id>, so the id is minted here first and the branch
// derived from it, then both are handed to the caller-supplied-id insert. auto_approve
// is baked true in the SQL (handoff is the no-plan-gate "just do it" mode) and is not a
// parameter.
//
// Ownership is enforced up front via GetRepoForUser (bypassing createRun drops the
// consent check otherwise), and the #66 default-branch guardrail (guardDefaultBranch)
// gates this PAT-bearing path exactly as the other issue-less creators do. Finally a
// cheap branch-safety assertion (Decision 8) confirms the resolved branch is namespaced
// and is not the repo's default branch — belt-and-suspenders behind the server-named
// branch being safe by construction.
func (s *Service) CreateTaskRun(ctx context.Context, userID, repoID uuid.UUID, inlineContext, baseBranch string, openMR, reviewRequested, thenFixRequested, interactive bool) (store.Run, error) {
	id := uuid.New()
	branch := taskBranchPrefix + id.String()

	// Ownership FIRST, before the input caps below — the same ordering CreatePromptRun's
	// model states. Resolving the repo up front means an over-cap request to a repo the
	// caller does not own returns repo-not-found (404) rather than too-large (422), so a
	// stranger cannot use the cap error to probe which repos exist. This also replicates
	// the consent check createRun would otherwise run (this path bypasses createRun).
	row, err := s.q.GetRepoForUser(ctx, store.GetRepoForUserParams{ID: repoID, UserID: userID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.Run{}, ErrRepoNotFound
		}
		return store.Run{}, err
	}

	// Cap the inline context the same way the seeded/prompt paths cap issue_description,
	// then NUL-strip it (and base_branch) — both are untrusted caller input reused as
	// TEXT columns.
	if len(inlineContext) > MaxIssueDescriptionBytes {
		return store.Run{}, ErrDescriptionTooLarge
	}
	inlineContext, _ = stripNUL(inlineContext)
	baseBranch, _ = stripNUL(baseBranch)
	// A git ref cannot legitimately exceed maxTaskBaseBranchBytes; cap it with a small
	// dedicated bound rather than relying only on the 1 MiB request-body cap. Checked on
	// the trimmed value (pgTextTrimNarg trims again before persisting).
	if len(strings.TrimSpace(baseBranch)) > maxTaskBaseBranchBytes {
		return store.Run{}, ErrTaskBaseBranchTooLong
	}

	// --then-fix IMPLIES --review server-side (issue #403 F5): the CLI applies this
	// (reviewRequested := review || thenFix), but a direct API caller — the second
	// consumer of this API — could set then_fix without review, silently getting no
	// auto-review and thus no fix run. Enforce the invariant at this shared choke point.
	if thenFixRequested {
		reviewRequested = true
	}

	// #66 D1 layer 2: the shared service-layer guardrail, same as the other PAT-bearing
	// inserts. A refused repo never gets a run.
	if err := s.guardDefaultBranch(ctx, row); err != nil {
		return store.Run{}, err
	}
	// Decision 8: the create-time namespace/default-branch assertion. Cannot fail for a
	// server-minted uzi/task/<uuid>, but is a real guardrail, not a comment.
	if !strings.HasPrefix(branch, taskBranchPrefix) || branch == row.DefaultBranch.String {
		return store.Run{}, ErrTaskBranchUnsafe
	}

	// issue #785: a NON-interactive handoff persists the dedicated handoff budget so it is
	// decoupled from the global RUN_TIMEOUT / RUN_MAX_ITERATIONS (see handoffBudget).
	budgetWall, budgetIters := s.handoffBudget(interactive)

	run, err := s.q.CreateTaskRun(ctx, store.CreateTaskRunParams{
		RunID:               id,
		UserID:              userID,
		RepoID:              repoID,
		Branch:              pgconv.TextOrNull(branch),
		BaseBranch:          pgTextTrimNarg(baseBranch),
		OpenMr:              openMR,
		Interactive:         interactive,
		ReviewRequested:     reviewRequested,
		ThenFixRequested:    thenFixRequested,
		IssueTitle:          deriveTaskTitle(inlineContext),
		IssueDescription:    inlineContext,
		BudgetWallSeconds:   budgetWall,
		BudgetMaxIterations: budgetIters,
		// PRD #35: the OWNER's default. A handoff has no per-request wait_on_limit
		// override today, so nil resolves to the user's users.wait_on_limit — the same
		// defaulting every other creation path applies (ci_fix/mr_rework/CreateRun).
		WaitOnLimit: s.resolveWaitOnLimit(ctx, userID, nil),
	})
	if err != nil {
		return store.Run{}, err
	}
	// A task run is created NOT-yet-claimable (dispatched_at NULL); the live status
	// broadcast stays consistent with other kinds, but the worker cannot claim it until
	// DispatchTaskRun stamps the gate (PRD #400 Decision 6).
	s.notify(run.ID, "queued")
	logRunCreated(run)
	return run, nil
}

// handoffBudget computes the dedicated handoff run budget (issue #785) from config:
// HANDOFF_RUN_TIMEOUT wall (LEAST-capped to budgetWallCeilingSeconds, the repo invariant
// that every budget writer caps the wall to the 8h ceiling — see runtime.sql
// SweepRunningTimeout) and HANDOFF_RUN_MAX_ITERATIONS. Both are NULL — so the claim/sweeper
// COALESCE falls back to the global RUN_TIMEOUT / RUN_MAX_ITERATIONS — for an interactive
// handoff (idle-bounded by WorkerTaskIdleTimeout instead) OR a non-positive/out-of-range
// knob. The guards check the COMPUTED integer seconds/iterations (not the raw duration): a
// sub-second HANDOFF_RUN_TIMEOUT truncating to 0, or an iteration cap above int32 max, is
// invalid and left NULL rather than persisting a 0 / negative / wrapped value that trips the
// `NULL OR > 0` CHECK. Used by CreateTaskRun at create; a then-fix run instead COPIES its
// original task's already-computed persisted budget (see CreateThenFixRun), so it is not
// recomputed and cannot drift if config changes between the task and its fix.
func (s *Service) handoffBudget(interactive bool) (budgetWall, budgetIters pgtype.Int4) {
	if interactive {
		return
	}
	if secs := int(s.p.HandoffRunTimeout.Seconds()); secs > 0 {
		budgetWall = pgtype.Int4{Int32: int32(min(secs, budgetWallCeilingSeconds)), Valid: true} //nolint:gosec // G115: min() bounds the value to budgetWallCeilingSeconds, a small constant well within int32
	}
	if s.p.HandoffRunMaxIterations > 0 && s.p.HandoffRunMaxIterations <= math.MaxInt32 {
		budgetIters = pgtype.Int4{Int32: int32(s.p.HandoffRunMaxIterations), Valid: true}
	}
	return
}

// DispatchTaskRun stamps the dispatch gate for a task run (PRD #400 Decision 6),
// making it genuinely claimable. The CLI calls this AFTER it has pushed local HEAD to
// the run's uzi/task/<id> branch — that push is what the ClaimRun gate
// (dispatched_at IS NOT NULL) waits for, closing the claim-before-seed race. The query
// is owner-scoped and guarded on kind='task' AND dispatched_at IS NULL, so a foreign
// run id, a non-task run, and an already-dispatched run all match 0 rows and surface as
// ErrRunNotFound (mapped to 404) rather than re-broadcasting a claimable signal.
func (s *Service) DispatchTaskRun(ctx context.Context, userID, runID uuid.UUID) (store.Run, error) {
	run, err := s.q.DispatchTaskRun(ctx, store.DispatchTaskRunParams{RunID: runID, UserID: userID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.Run{}, ErrRunNotFound
		}
		return store.Run{}, err
	}
	// The run is now claimable; broadcast queued so any live view re-reads it (the status
	// is unchanged, but the run's claimability just changed).
	s.notify(run.ID, "queued")
	return run, nil
}

// ErrTaskReviewAlreadyActive is returned when a review run already exists for a target
// (the uq_one_active_task_review_per_target partial unique index raised 23505). The
// auto-enqueue hook treats it as a no-op ("already being reviewed"); an explicit caller
// can surface it. PRD #400 M4a.
var ErrTaskReviewAlreadyActive = errors.New("a review run is already active for this task")

// CreateTaskReviewRun mints a REVIEW run for a completed task (PRD #400 M4a): a task run
// that IS a review of targetRunID, distinguished by a non-null review_target_run_id. It
// reuses the reviewed task's own branch (the review clones and diffs it against
// baseBranch) and is dispatched immediately (the query stamps dispatched_at = now()) — a
// review needs no CLI seed push because the target branch is already pushed. The worker
// (M4b) routes such a claim to a diff-review executor. A concurrent duplicate trips the
// partial unique index (23505 → ErrTaskReviewAlreadyActive). The id is minted here so the
// server-named uzi/task/<id> title/provenance stays consistent with CreateTaskRun, but the
// review's WORKING branch is the target's, not a fresh one.
func (s *Service) CreateTaskReviewRun(ctx context.Context, userID, repoID, targetRunID uuid.UUID, branch, baseBranch string) (store.Run, error) {
	id := uuid.New()
	run, err := s.q.CreateTaskReviewRun(ctx, store.CreateTaskReviewRunParams{
		RunID:       id,
		UserID:      userID,
		RepoID:      repoID,
		Branch:      pgconv.TextOrNull(branch),
		BaseBranch:  pgTextTrimNarg(baseBranch),
		TargetRunID: pgconv.UUID(targetRunID),
		IssueTitle:  deriveTaskReviewTitle(branch),
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return store.Run{}, ErrTaskReviewAlreadyActive
		}
		return store.Run{}, err
	}
	// The review run is already claimable (dispatched_at stamped in SQL); broadcast queued
	// so any live view re-reads it, exactly like DispatchTaskRun does for a plain handoff.
	s.notify(run.ID, "queued")
	logRunCreated(run)
	return run, nil
}

// ErrThenFixAlreadyActive is returned when a fix run already exists for an original task
// (the uq_one_active_then_fix_per_target partial unique index raised 23505). The
// then-fix auto-enqueue hook treats it as a no-op ("a fix is already active"). PRD #400 M5.
var ErrThenFixAlreadyActive = errors.New("a fix run is already active for this task")

// CreateThenFixRun mints a FIX run for an original task (PRD #400 M5): a NORMAL task run
// (review_target_run_id NULL) that pushes fixes — composed from a diff-review's findings —
// back to the original's own branch. It reuses the original's branch (the user pulls fixes
// there) and is dispatched immediately (the query stamps dispatched_at = now()): the branch
// already exists, so unlike a direct handoff a fix needs no CLI seed push. Because it is a
// plain task, the existing worker RunRunner implements-and-pushes it with no new worker code
// or kind. A concurrent duplicate trips the partial unique index (23505 →
// ErrThenFixAlreadyActive). The id is minted here for the same server-named-provenance
// reason CreateTaskRun/CreateTaskReviewRun mint theirs.
func (s *Service) CreateThenFixRun(ctx context.Context, userID, repoID, originalRunID uuid.UUID, branch, baseBranch, description string, budgetWall, budgetIters pgtype.Int4) (store.Run, error) {
	id := uuid.New()
	// A then-fix INHERITS the original task's persisted budget verbatim (issue #785):
	// budgetWall / budgetIters are the original run's stored budget_wall_seconds /
	// budget_max_iterations. Copying rather than recomputing from config means a change to
	// HANDOFF_RUN_TIMEOUT / HANDOFF_RUN_MAX_ITERATIONS between the original task and its
	// auto-spawned fix cannot alter the fix's budget, and the fix phase of a long
	// non-interactive handoff keeps the same allowance instead of reverting to the global
	// default. NULL (global fallback) exactly when the original's was NULL.
	// issue #403 F4: the composed findings description is server-generated and can exceed
	// MaxIssueDescriptionBytes (up to ~200 findings). Every other creator caps issue_description;
	// mirror that here. TRUNCATE rather than error (as CreateTaskRun does): the caller
	// (maybeEnqueueThenFix) is best-effort and swallows errors, so erroring would silently drop
	// the entire fix — a truncated-but-present fix is the better outcome, and the prompt stays bounded.
	description, _ = stripNUL(description)
	if len(description) > MaxIssueDescriptionBytes {
		const marker = "\n… findings truncated at the description cap …\n"
		keep := MaxIssueDescriptionBytes - len(marker)
		if keep < 0 {
			keep = 0
		}
		description = strings.ToValidUTF8(description[:keep], "") + marker
	}

	run, err := s.q.CreateThenFixRun(ctx, store.CreateThenFixRunParams{
		RunID:               id,
		UserID:              userID,
		RepoID:              repoID,
		Branch:              pgconv.TextOrNull(branch),
		BaseBranch:          pgTextTrimNarg(baseBranch),
		ThenFixOfRunID:      pgconv.UUID(originalRunID),
		IssueTitle:          deriveThenFixTitle(branch),
		IssueDescription:    description,
		BudgetWallSeconds:   budgetWall,
		BudgetMaxIterations: budgetIters,
		// PRD #35: stamp the owner's usage-limit-parking default, same as CreateTaskRun —
		// otherwise a fix run falls to the column DEFAULT false and stops on a limit even
		// when the owner (and the original handoff) opted into parking.
		WaitOnLimit: s.resolveWaitOnLimit(ctx, userID, nil),
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return store.Run{}, ErrThenFixAlreadyActive
		}
		return store.Run{}, err
	}
	// The fix run is already claimable (dispatched_at stamped in SQL); broadcast queued so
	// any live view re-reads it, exactly like CreateTaskReviewRun / DispatchTaskRun.
	s.notify(run.ID, "queued")
	logRunCreated(run)
	return run, nil
}

// deriveThenFixTitle synthesizes the display title for a fix run (its issue_description
// carries the composed findings, but the title stays short). The branch is the original's
// server-named uzi/task/<id>, safe display text.
func deriveThenFixTitle(branch string) string {
	if strings.TrimSpace(branch) == "" {
		return "Fix of handoff task"
	}
	return "Fix of " + branch
}

// deriveTaskReviewTitle synthesizes the display title for a review run (it has no issue
// and its inline context is empty). The branch is the server-named uzi/task/<id>, safe
// display text.
func deriveTaskReviewTitle(branch string) string {
	if strings.TrimSpace(branch) == "" {
		return "Review of handoff task"
	}
	return "Review of " + branch
}

// maxTaskTitleRunes bounds the derived task title. issue_title is display text, not a
// blob; the full context lives in issue_description.
const maxTaskTitleRunes = 120

// deriveTaskTitle picks a short, human-readable title from the inline context: the
// first non-empty line, trimmed and truncated to maxTaskTitleRunes runes. A blank
// context falls back to a stable placeholder so the run view header is never empty.
func deriveTaskTitle(text string) string {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		r := []rune(line)
		if len(r) > maxTaskTitleRunes {
			return string(r[:maxTaskTitleRunes])
		}
		return line
	}
	return "handoff task"
}

// pgTextTrimNarg maps an optional base branch to a nullable pgtype.Text: an empty (or
// pgTextTrimNarg trims whitespace and converts an empty result to NULL; otherwise, it returns the trimmed text as a PostgreSQL value.
func pgTextTrimNarg(s string) pgtype.Text {
	s = strings.TrimSpace(s)
	if s == "" {
		return pgtype.Text{}
	}
	return pgconv.TextOrNull(s)
}
