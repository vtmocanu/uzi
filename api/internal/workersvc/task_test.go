package workersvc

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/privcheck"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestCreateTaskRunGuardrailBlocksBeforeInsert: the task/handoff create path (PRD
// #400, a PAT-bearing insert like the other issue-less creators) refuses at the
// shared #66 guard before its dedicated insert.
func TestCreateTaskRunGuardrailBlocksBeforeInsert(t *testing.T) {
	user, repo := uuid.New(), uuid.New()
	fs := &fakeStore{repoRow: aValidRepoRow(), taskRunResult: store.Run{ID: uuid.New()}}
	guard, wantMsgs := blockedGuard()
	svc := New(fs, newBox(t), testParams())
	svc.SetRepoGuard(guard)

	_, err := svc.CreateTaskRun(context.Background(), user, repo, "do the thing", "", false, false, false, false)
	assertGuardrailBlocked(t, err, wantMsgs)
	if guard.called != 1 {
		t.Fatalf("guard called %d times, want 1", guard.called)
	}
	if fs.taskRunParams != nil {
		t.Fatal("task store insert must not run once the guardrail blocks")
	}
}

// TestCreateTaskRunMintsNamespacedBranch: a clearing guard lets the path proceed, and
// the insert carries a server-named uzi/task/<run-id> branch (matching the returned
// run id), auto_approve baked true by the SQL, a derived title, and the passed
// base_branch/open_mr.
func TestCreateTaskRunMintsNamespacedBranch(t *testing.T) {
	user, repo := uuid.New(), uuid.New()
	fs := &fakeStore{repoRow: aValidRepoRow()}
	guard := &fakeGuard{res: privcheck.GuardResult{Blocked: false}}
	svc := New(fs, newBox(t), testParams())
	svc.SetRepoGuard(guard)

	_, err := svc.CreateTaskRun(context.Background(), user, repo, "Fix the flaky test\nmore detail here", "develop", true, false, true, true)
	if err != nil {
		t.Fatalf("CreateTaskRun with a clearing guard: %v", err)
	}
	if fs.taskRunParams == nil {
		t.Fatal("task store insert must run when the guard clears the repo")
	}
	p := fs.taskRunParams
	wantBranch := taskBranchPrefix + p.RunID.String()
	if !p.Branch.Valid || p.Branch.String != wantBranch {
		t.Errorf("branch = %v, want the server-named %q", p.Branch, wantBranch)
	}
	if !strings.HasPrefix(p.Branch.String, taskBranchPrefix) {
		t.Errorf("branch %q is not in the uzi/task/* namespace", p.Branch.String)
	}
	if p.RepoID != repo {
		t.Errorf("repo_id = %v, want %v", p.RepoID, repo)
	}
	if p.IssueTitle != "Fix the flaky test" {
		t.Errorf("issue_title = %q, want the first non-empty line", p.IssueTitle)
	}
	if !p.BaseBranch.Valid || p.BaseBranch.String != "develop" {
		t.Errorf("base_branch = %v, want develop", p.BaseBranch)
	}
	if !p.OpenMr {
		t.Error("open_mr = false, want true when passed")
	}
	if !p.Interactive {
		t.Error("interactive = false, want true when passed (PRD #517 M1) — omitting it in the store params literal silently yields false with a green build")
	}
	if !p.ThenFixRequested {
		t.Error("then_fix_requested = false, want true when --then-fix was passed (PRD #400 M5)")
	}
}

// TestCreateTaskRunPersistsHandoffBudget: issue #785. A non-interactive handoff persists
// the dedicated HANDOFF_RUN_TIMEOUT / HANDOFF_RUN_MAX_ITERATIONS budget onto the run
// (budget_wall_seconds / budget_max_iterations), LEAST-capped to the 8h wall ceiling; an
// interactive handoff persists both as NULL so the claim/sweeper COALESCE falls back to the
// global default.
func TestCreateTaskRunPersistsHandoffBudget(t *testing.T) {
	newSvc := func(fs *fakeStore, p Params) *Service {
		svc := New(fs, newBox(t), p)
		svc.SetRepoGuard(&fakeGuard{res: privcheck.GuardResult{Blocked: false}})
		return svc
	}

	// Non-interactive: the dedicated 4h/10 budget lands on the run.
	t.Run("non-interactive persists budget", func(t *testing.T) {
		p := testParams()
		p.HandoffRunTimeout = 4 * time.Hour
		p.HandoffRunMaxIterations = 10
		fs := &fakeStore{repoRow: aValidRepoRow()}
		if _, err := newSvc(fs, p).CreateTaskRun(context.Background(), uuid.New(), uuid.New(), "do the thing", "", false, false, false, false); err != nil {
			t.Fatalf("CreateTaskRun: %v", err)
		}
		got := fs.taskRunParams
		if got == nil {
			t.Fatal("insert did not run")
		}
		if !got.BudgetWallSeconds.Valid || got.BudgetWallSeconds.Int32 != 14400 {
			t.Errorf("budget_wall_seconds = %v, want valid 14400", got.BudgetWallSeconds)
		}
		if !got.BudgetMaxIterations.Valid || got.BudgetMaxIterations.Int32 != 10 {
			t.Errorf("budget_max_iterations = %v, want valid 10", got.BudgetMaxIterations)
		}
	})

	// Interactive: both NULL, so the global default applies via COALESCE.
	t.Run("interactive persists NULL", func(t *testing.T) {
		p := testParams()
		p.HandoffRunTimeout = 4 * time.Hour
		p.HandoffRunMaxIterations = 10
		fs := &fakeStore{repoRow: aValidRepoRow()}
		if _, err := newSvc(fs, p).CreateTaskRun(context.Background(), uuid.New(), uuid.New(), "do the thing", "", false, false, false, true); err != nil {
			t.Fatalf("CreateTaskRun: %v", err)
		}
		got := fs.taskRunParams
		if got == nil {
			t.Fatal("insert did not run")
		}
		if got.BudgetWallSeconds.Valid {
			t.Errorf("budget_wall_seconds = %v, want NULL for an interactive handoff", got.BudgetWallSeconds)
		}
		if got.BudgetMaxIterations.Valid {
			t.Errorf("budget_max_iterations = %v, want NULL for an interactive handoff", got.BudgetMaxIterations)
		}
	})

	// A wall above the 8h ceiling is LEAST-capped to budgetWallCeilingSeconds (28800).
	t.Run("non-interactive clamps to the wall ceiling", func(t *testing.T) {
		p := testParams()
		p.HandoffRunTimeout = 12 * time.Hour
		p.HandoffRunMaxIterations = 10
		fs := &fakeStore{repoRow: aValidRepoRow()}
		if _, err := newSvc(fs, p).CreateTaskRun(context.Background(), uuid.New(), uuid.New(), "do the thing", "", false, false, false, false); err != nil {
			t.Fatalf("CreateTaskRun: %v", err)
		}
		got := fs.taskRunParams
		if got == nil {
			t.Fatal("insert did not run")
		}
		if !got.BudgetWallSeconds.Valid || got.BudgetWallSeconds.Int32 != budgetWallCeilingSeconds {
			t.Errorf("budget_wall_seconds = %v, want valid %d (ceiling)", got.BudgetWallSeconds, budgetWallCeilingSeconds)
		}
	})

	// A sub-second HANDOFF_RUN_TIMEOUT truncates to 0 integer seconds; the guard is on the
	// COMPUTED seconds, so it persists NULL (global fallback) rather than a 0 that would
	// trip the CHECK (budget_wall_seconds > 0) and fail every non-interactive handoff.
	t.Run("sub-second wall truncates to NULL", func(t *testing.T) {
		p := testParams()
		p.HandoffRunTimeout = 500 * time.Millisecond
		p.HandoffRunMaxIterations = 10
		fs := &fakeStore{repoRow: aValidRepoRow()}
		if _, err := newSvc(fs, p).CreateTaskRun(context.Background(), uuid.New(), uuid.New(), "do the thing", "", false, false, false, false); err != nil {
			t.Fatalf("CreateTaskRun: %v", err)
		}
		got := fs.taskRunParams
		if got == nil {
			t.Fatal("insert did not run")
		}
		if got.BudgetWallSeconds.Valid {
			t.Errorf("budget_wall_seconds = %v, want NULL for a sub-second timeout", got.BudgetWallSeconds)
		}
	})

	// An iteration cap ABOVE int32 max would narrow to a negative/wrong-positive int32; the
	// guard bounds it, so it persists NULL (global fallback) rather than tripping the CHECK
	// (budget_max_iterations > 0) or capping at a bogus value.
	t.Run("iteration cap above int32 max truncates to NULL", func(t *testing.T) {
		p := testParams()
		p.HandoffRunTimeout = 4 * time.Hour
		p.HandoffRunMaxIterations = int(math.MaxInt32) + 1
		fs := &fakeStore{repoRow: aValidRepoRow()}
		if _, err := newSvc(fs, p).CreateTaskRun(context.Background(), uuid.New(), uuid.New(), "do the thing", "", false, false, false, false); err != nil {
			t.Fatalf("CreateTaskRun: %v", err)
		}
		got := fs.taskRunParams
		if got == nil {
			t.Fatal("insert did not run")
		}
		if got.BudgetMaxIterations.Valid {
			t.Errorf("budget_max_iterations = %v, want NULL for an above-int32-max cap", got.BudgetMaxIterations)
		}
	})

	// The int32 max boundary is in-range and persists as-is.
	t.Run("iteration cap at int32 max persists", func(t *testing.T) {
		p := testParams()
		p.HandoffRunTimeout = 4 * time.Hour
		p.HandoffRunMaxIterations = math.MaxInt32
		fs := &fakeStore{repoRow: aValidRepoRow()}
		if _, err := newSvc(fs, p).CreateTaskRun(context.Background(), uuid.New(), uuid.New(), "do the thing", "", false, false, false, false); err != nil {
			t.Fatalf("CreateTaskRun: %v", err)
		}
		got := fs.taskRunParams
		if got == nil {
			t.Fatal("insert did not run")
		}
		if !got.BudgetMaxIterations.Valid || got.BudgetMaxIterations.Int32 != math.MaxInt32 {
			t.Errorf("budget_max_iterations = %v, want valid %d", got.BudgetMaxIterations, int32(math.MaxInt32))
		}
	})
}

// A then-fix run inherits the SAME dedicated handoff budget as its original task (issue
// #785): the fix phase of a long non-interactive handoff must not silently revert to the
// global RUN_TIMEOUT / RUN_MAX_ITERATIONS. CreateThenFixRun is always non-interactive
// (--interactive --then-fix is rejected at the CLI), so it always persists the budget.
func TestCreateThenFixRunPersistsHandoffBudget(t *testing.T) {
	p := testParams()
	p.HandoffRunTimeout = 4 * time.Hour
	p.HandoffRunMaxIterations = 10
	fs := &fakeStore{}
	svc := New(fs, newBox(t), p)
	if _, err := svc.CreateThenFixRun(context.Background(), uuid.New(), uuid.New(), uuid.New(), "uzi/task/abc", "main", "fix the findings"); err != nil {
		t.Fatalf("CreateThenFixRun: %v", err)
	}
	got := fs.thenFixRunParams
	if got == nil {
		t.Fatal("insert did not run")
	}
	if !got.BudgetWallSeconds.Valid || got.BudgetWallSeconds.Int32 != 14400 {
		t.Errorf("budget_wall_seconds = %v, want valid 14400 (same as the original task)", got.BudgetWallSeconds)
	}
	if !got.BudgetMaxIterations.Valid || got.BudgetMaxIterations.Int32 != 10 {
		t.Errorf("budget_max_iterations = %v, want valid 10 (same as the original task)", got.BudgetMaxIterations)
	}
}

// TestCreateTaskRunSanitizesAndCaps: NUL bytes are stripped from both context and
// base_branch, an empty base_branch persists as NULL, and an over-cap context is
// rejected with ErrDescriptionTooLarge before any insert.
func TestCreateTaskRunSanitizesAndCaps(t *testing.T) {
	user, repo := uuid.New(), uuid.New()

	// NUL strip + empty base_branch → NULL.
	fs := &fakeStore{repoRow: aValidRepoRow()}
	svc := New(fs, newBox(t), testParams())
	svc.SetRepoGuard(&fakeGuard{res: privcheck.GuardResult{Blocked: false}})
	if _, err := svc.CreateTaskRun(context.Background(), user, repo, "clean\x00text", "   ", false, false, false, false); err != nil {
		t.Fatalf("CreateTaskRun: %v", err)
	}
	if fs.taskRunParams == nil {
		t.Fatal("insert did not run")
	}
	if strings.Contains(fs.taskRunParams.IssueDescription, "\x00") {
		t.Errorf("issue_description %q still contains a NUL byte", fs.taskRunParams.IssueDescription)
	}
	if fs.taskRunParams.BaseBranch.Valid {
		t.Errorf("base_branch = %v, want NULL for a whitespace-only value", fs.taskRunParams.BaseBranch)
	}

	// Over-cap context rejected before the insert.
	fs2 := &fakeStore{repoRow: aValidRepoRow()}
	svc2 := New(fs2, newBox(t), testParams())
	svc2.SetRepoGuard(&fakeGuard{res: privcheck.GuardResult{Blocked: false}})
	big := strings.Repeat("a", MaxIssueDescriptionBytes+1)
	if _, err := svc2.CreateTaskRun(context.Background(), user, repo, big, "", false, false, false, false); !errors.Is(err, ErrDescriptionTooLarge) {
		t.Fatalf("err = %v, want ErrDescriptionTooLarge", err)
	}
	if fs2.taskRunParams != nil {
		t.Fatal("insert must not run when the context is over the cap")
	}
}

// TestCreateTaskRunRepoNotOwned: an unknown/unowned repo maps pgx.ErrNoRows →
// ErrRepoNotFound (the consent check createRun would otherwise run).
func TestCreateTaskRunRepoNotOwned(t *testing.T) {
	fs := &fakeStore{repoErr: pgx.ErrNoRows}
	svc := New(fs, newBox(t), testParams())
	svc.SetRepoGuard(&fakeGuard{res: privcheck.GuardResult{Blocked: false}})
	if _, err := svc.CreateTaskRun(context.Background(), uuid.New(), uuid.New(), "x", "", false, false, false, false); !errors.Is(err, ErrRepoNotFound) {
		t.Fatalf("err = %v, want ErrRepoNotFound", err)
	}
}

// TestCreateTaskRunBaseBranchTooLong: a base_branch over the dedicated cap is rejected
// with ErrTaskBaseBranchTooLong before any insert.
func TestCreateTaskRunBaseBranchTooLong(t *testing.T) {
	fs := &fakeStore{repoRow: aValidRepoRow()}
	svc := New(fs, newBox(t), testParams())
	svc.SetRepoGuard(&fakeGuard{res: privcheck.GuardResult{Blocked: false}})
	long := strings.Repeat("b", maxTaskBaseBranchBytes+1)
	if _, err := svc.CreateTaskRun(context.Background(), uuid.New(), uuid.New(), "do it", long, false, false, false, false); !errors.Is(err, ErrTaskBaseBranchTooLong) {
		t.Fatalf("err = %v, want ErrTaskBaseBranchTooLong", err)
	}
	if fs.taskRunParams != nil {
		t.Fatal("insert must not run when base_branch is over the cap")
	}
}

// TestCreateTaskRunRepoNotOwnedBeforeCap: an over-cap request to a repo the caller does
// NOT own returns repo-not-found (ownership resolved first), not too-large — the
// ordering nit that mirrors CreatePromptRun.
func TestCreateTaskRunRepoNotOwnedBeforeCap(t *testing.T) {
	fs := &fakeStore{repoErr: pgx.ErrNoRows}
	svc := New(fs, newBox(t), testParams())
	svc.SetRepoGuard(&fakeGuard{res: privcheck.GuardResult{Blocked: false}})
	big := strings.Repeat("a", MaxIssueDescriptionBytes+1)
	if _, err := svc.CreateTaskRun(context.Background(), uuid.New(), uuid.New(), big, "", false, false, false, false); !errors.Is(err, ErrRepoNotFound) {
		t.Fatalf("err = %v, want ErrRepoNotFound (ownership resolved before the cap)", err)
	}
}

// TestDispatchTaskRunSuccess: a stamped run is returned and the dispatch query saw the
// caller's (run, user) pair.
func TestDispatchTaskRunSuccess(t *testing.T) {
	user, run := uuid.New(), uuid.New()
	fs := &fakeStore{dispatchTaskResult: store.Run{ID: run}}
	svc := New(fs, newBox(t), testParams())
	got, err := svc.DispatchTaskRun(context.Background(), user, run)
	if err != nil {
		t.Fatalf("DispatchTaskRun: %v", err)
	}
	if got.ID != run {
		t.Errorf("returned run %v, want %v", got.ID, run)
	}
	if fs.dispatchTaskParams == nil || fs.dispatchTaskParams.RunID != run || fs.dispatchTaskParams.UserID != user {
		t.Errorf("dispatch params = %+v, want run=%v user=%v", fs.dispatchTaskParams, run, user)
	}
}

// TestDispatchTaskRunIdempotencyGuard: the query's ownership+idempotency guard matches
// 0 rows (pgx.ErrNoRows) for a foreign/non-task/already-dispatched run, which the
// service maps to ErrRunNotFound — so a SECOND dispatch is a 404, not a re-broadcast.
func TestDispatchTaskRunIdempotencyGuard(t *testing.T) {
	fs := &fakeStore{dispatchTaskErr: pgx.ErrNoRows}
	svc := New(fs, newBox(t), testParams())
	if _, err := svc.DispatchTaskRun(context.Background(), uuid.New(), uuid.New()); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("err = %v, want ErrRunNotFound for a 0-row dispatch", err)
	}
}

// TestDeriveTaskTitle covers the first-non-empty-line pick, rune truncation, and the
// blank-context fallback.
func TestDeriveTaskTitle(t *testing.T) {
	if got := deriveTaskTitle("\n\n  first line  \nsecond"); got != "first line" {
		t.Errorf("deriveTaskTitle = %q, want %q", got, "first line")
	}
	if got := deriveTaskTitle("   \n\t"); got != "handoff task" {
		t.Errorf("blank context = %q, want the fallback", got)
	}
	long := strings.Repeat("x", maxTaskTitleRunes+50)
	if got := deriveTaskTitle(long); len([]rune(got)) != maxTaskTitleRunes {
		t.Errorf("truncated title has %d runes, want %d", len([]rune(got)), maxTaskTitleRunes)
	}
	// Multi-byte runes are counted as runes, not bytes.
	multi := strings.Repeat("é", maxTaskTitleRunes+10)
	if got := deriveTaskTitle(multi); len([]rune(got)) != maxTaskTitleRunes {
		t.Errorf("multi-byte truncated title has %d runes, want %d", len([]rune(got)), maxTaskTitleRunes)
	}
}
