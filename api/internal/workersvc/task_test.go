package workersvc

import (
	"context"
	"errors"
	"strings"
	"testing"

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

	_, err := svc.CreateTaskRun(context.Background(), user, repo, "do the thing", "", false, false)
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

	_, err := svc.CreateTaskRun(context.Background(), user, repo, "Fix the flaky test\nmore detail here", "develop", true, false)
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
	if _, err := svc.CreateTaskRun(context.Background(), user, repo, "clean\x00text", "   ", false, false); err != nil {
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
	if _, err := svc2.CreateTaskRun(context.Background(), user, repo, big, "", false, false); !errors.Is(err, ErrDescriptionTooLarge) {
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
	if _, err := svc.CreateTaskRun(context.Background(), uuid.New(), uuid.New(), "x", "", false, false); !errors.Is(err, ErrRepoNotFound) {
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
	if _, err := svc.CreateTaskRun(context.Background(), uuid.New(), uuid.New(), "do it", long, false, false); !errors.Is(err, ErrTaskBaseBranchTooLong) {
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
	if _, err := svc.CreateTaskRun(context.Background(), uuid.New(), uuid.New(), big, "", false, false); !errors.Is(err, ErrRepoNotFound) {
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
