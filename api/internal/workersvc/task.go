package workersvc

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

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
func (s *Service) CreateTaskRun(ctx context.Context, userID, repoID uuid.UUID, context, baseBranch string, openMR bool) (store.Run, error) {
	id := uuid.New()
	branch := taskBranchPrefix + id.String()

	// Cap the inline context the same way the seeded/prompt paths cap issue_description,
	// then NUL-strip it (and base_branch) — both are untrusted caller input reused as
	// TEXT columns.
	if len(context) > MaxIssueDescriptionBytes {
		return store.Run{}, ErrDescriptionTooLarge
	}
	context, _ = stripNUL(context)
	baseBranch, _ = stripNUL(baseBranch)

	row, err := s.q.GetRepoForUser(ctx, store.GetRepoForUserParams{ID: repoID, UserID: userID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.Run{}, ErrRepoNotFound
		}
		return store.Run{}, err
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

	run, err := s.q.CreateTaskRun(ctx, store.CreateTaskRunParams{
		RunID:            id,
		UserID:           userID,
		RepoID:           repoID,
		Branch:           pgText(branch),
		BaseBranch:       pgTextTrimNarg(baseBranch),
		OpenMr:           openMR,
		IssueTitle:       deriveTaskTitle(context),
		IssueDescription: context,
	})
	if err != nil {
		return store.Run{}, err
	}
	// queued keeps the live status broadcast consistent with other run kinds.
	s.notify(run.ID, "queued")
	return run, nil
}

// maxTaskTitleRunes bounds the derived task title. issue_title is display text, not a
// blob; the full context lives in issue_description.
const maxTaskTitleRunes = 120

// deriveTaskTitle picks a short, human-readable title from the inline context: the
// first non-empty line, trimmed and truncated to maxTaskTitleRunes runes. A blank
// context falls back to a stable placeholder so the run view header is never empty.
func deriveTaskTitle(context string) string {
	for _, line := range strings.Split(context, "\n") {
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
// whitespace-only) value is NULL, so an unset --base does not persist a blank string.
func pgTextTrimNarg(s string) pgtype.Text {
	s = strings.TrimSpace(s)
	if s == "" {
		return pgtype.Text{}
	}
	return pgText(s)
}
