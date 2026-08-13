package workersvc

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"gitlab.example.com/vtmocanu/uzi/api/internal/privcheck"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// fakeGuard is the workersvc test double for RepoGuard (the #66 default-branch
// guardrail, D1 layer 2). It returns a fixed GuardResult and records how many times
// GuardRepo was called, so a test can assert the gate ran and stopped the create
// path before any deeper store write.
type fakeGuard struct {
	res    privcheck.GuardResult
	called int
}

func (g *fakeGuard) GuardRepo(_ context.Context, _ privcheck.GuardInput) privcheck.GuardResult {
	g.called++
	return g.res
}

// blockedGuard returns a guard that refuses with two block findings, plus the
// messages BlockMessages() should surface (the SeverityBlock ones only — the warn
// finding is excluded).
func blockedGuard() (*fakeGuard, []string) {
	return &fakeGuard{res: privcheck.GuardResult{
		Blocked: true,
		Findings: []privcheck.Finding{
			{Code: privcheck.CodeWriteRoleCanPush, Severity: privcheck.SeverityBlock, Message: "the bot can push to the default branch"},
			{Code: privcheck.CodeBotCanMerge, Severity: privcheck.SeverityBlock, Message: "the bot can merge into the default branch"},
			{Code: privcheck.CodeBotRoleAboveWrite, Severity: privcheck.SeverityWarn, Message: "the bot is an admin (warn, not a refusal reason)"},
		},
	}}, []string{"the bot can push to the default branch", "the bot can merge into the default branch"}
}

// aValidRepoRow is a GetRepoForUser row shaped enough for guardDefaultBranch to
// build a GuardInput (the guard itself is faked, so the values only need to be
// present, not real).
func aValidRepoRow() store.GetRepoForUserRow {
	return store.GetRepoForUserRow{ID: uuid.New(), ForgeProjectID: 1, ForgeType: "gitlab", BaseUrl: "https://x", PathWithNamespace: "grp/repo"}
}

func assertGuardrailBlocked(t *testing.T, err error, wantMsgs []string) {
	t.Helper()
	if !errors.Is(err, ErrGuardrailBlocked) {
		t.Fatalf("err = %v, want to wrap ErrGuardrailBlocked", err)
	}
	var g *GuardrailBlockedError
	if !errors.As(err, &g) {
		t.Fatalf("err = %v, want *GuardrailBlockedError", err)
	}
	if len(g.Findings) != len(wantMsgs) {
		t.Fatalf("findings = %v, want %v", g.Findings, wantMsgs)
	}
	for i, m := range wantMsgs {
		if g.Findings[i] != m {
			t.Fatalf("finding[%d] = %q, want %q", i, g.Findings[i], m)
		}
	}
}

// TestCreateRunGuardrailBlocksBeforeInsert asserts the issue-lane create path
// refuses at the service-layer guard (right after GetRepoForUser) and never reaches
// the deeper store writes (PRD #66 M5, D1 layer 2).
func TestCreateRunGuardrailBlocksBeforeInsert(t *testing.T) {
	user, repo := uuid.New(), uuid.New()
	fs := &fakeStore{
		repoRow:         aValidRepoRow(),
		issueByID:       store.Issue{Title: "T", Labels: prdLabels(), HasPrdLink: true},
		createRunResult: store.Run{ID: uuid.New()},
	}
	guard, wantMsgs := blockedGuard()
	svc := New(fs, newBox(t), testParams())
	svc.SetRepoGuard(guard)

	_, err := svc.CreateRun(context.Background(), user, repo, 4, "desc", false, nil, nil)
	assertGuardrailBlocked(t, err, wantMsgs)
	if guard.called != 1 {
		t.Fatalf("guard called %d times, want 1", guard.called)
	}
	if fs.createRunParams != nil {
		t.Fatal("CreateRun store insert must not run once the guardrail blocks")
	}
}

// TestCreateCIFixRunGuardrailBlocksBeforeInsert: the CI-fix create path refuses at
// the shared guard before the branch-exclusion check or the insert.
func TestCreateCIFixRunGuardrailBlocksBeforeInsert(t *testing.T) {
	user, repo := uuid.New(), uuid.New()
	fs := &fakeStore{repoRow: aValidRepoRow(), ciFixRunResult: store.Run{ID: uuid.New()}}
	guard, wantMsgs := blockedGuard()
	svc := New(fs, newBox(t), testParams())
	svc.SetRepoGuard(guard)

	_, err := svc.CreateCIFixRun(context.Background(), user, repo, "feat/x", "t", "d", FailureSnapshot{}, nil)
	assertGuardrailBlocked(t, err, wantMsgs)
	if guard.called != 1 {
		t.Fatalf("guard called %d times, want 1", guard.called)
	}
	if fs.ciFixRunParams != nil {
		t.Fatal("CI-fix store insert must not run once the guardrail blocks")
	}
}

// TestCreateSelfImproveRunGuardrailBlocksBeforeInsert: the self-improve create path
// refuses at the shared guard before its dedicated insert.
func TestCreateSelfImproveRunGuardrailBlocksBeforeInsert(t *testing.T) {
	user, repo := uuid.New(), uuid.New()
	fs := &fakeStore{repoRow: aValidRepoRow(), createRunResult: store.Run{ID: uuid.New()}}
	guard, wantMsgs := blockedGuard()
	svc := New(fs, newBox(t), testParams())
	svc.SetRepoGuard(guard)

	_, err := svc.CreateSelfImproveRun(context.Background(), user, repo, 7, "t", "d")
	assertGuardrailBlocked(t, err, wantMsgs)
	if guard.called != 1 {
		t.Fatalf("guard called %d times, want 1", guard.called)
	}
	if fs.selfImproveParams != nil {
		t.Fatal("self-improve store insert must not run once the guardrail blocks")
	}
}

// TestCreatePromptRunGuardrailBlocksBeforeInsert: the scheduled-prompt create path
// (PRD #241, scheduler-fired unattended with the bot PAT) refuses at the shared guard
// before its dedicated insert, so an un-gated schedule cannot bypass the guardrail.
func TestCreatePromptRunGuardrailBlocksBeforeInsert(t *testing.T) {
	user, repo, sched := uuid.New(), uuid.New(), uuid.New()
	fs := &fakeStore{repoRow: aValidRepoRow(), promptRunResult: store.Run{ID: uuid.New()}}
	guard, wantMsgs := blockedGuard()
	svc := New(fs, newBox(t), testParams())
	svc.SetRepoGuard(guard)

	_, err := svc.CreatePromptRun(context.Background(), user, repo, sched, "t", "p", false, false, nil, false)
	assertGuardrailBlocked(t, err, wantMsgs)
	if guard.called != 1 {
		t.Fatalf("guard called %d times, want 1", guard.called)
	}
	if fs.promptRunParams != nil {
		t.Fatal("prompt store insert must not run once the guardrail blocks")
	}
}

// TestCreatePromptRunNotBlockedGuardProceeds: a clearing guard lets the scheduled-prompt
// path proceed past the gate to its insert.
func TestCreatePromptRunNotBlockedGuardProceeds(t *testing.T) {
	user, repo, sched := uuid.New(), uuid.New(), uuid.New()
	fs := &fakeStore{repoRow: aValidRepoRow(), promptRunResult: store.Run{ID: uuid.New()}}
	guard := &fakeGuard{res: privcheck.GuardResult{Blocked: false}}
	svc := New(fs, newBox(t), testParams())
	svc.SetRepoGuard(guard)

	if _, err := svc.CreatePromptRun(context.Background(), user, repo, sched, "t", "p", false, false, nil, false); err != nil {
		t.Fatalf("CreatePromptRun with a clearing guard: %v", err)
	}
	if guard.called != 1 {
		t.Fatalf("guard called %d times, want 1", guard.called)
	}
	if fs.promptRunParams == nil {
		t.Fatal("prompt store insert must run when the guard clears the repo")
	}
}

// TestCreateRunNotBlockedGuardProceeds: a guard that clears the repo (Blocked=false)
// lets the create path proceed past the gate to the insert.
func TestCreateRunNotBlockedGuardProceeds(t *testing.T) {
	user, repo := uuid.New(), uuid.New()
	fs := &fakeStore{
		repoRow:         aValidRepoRow(),
		issueByID:       store.Issue{Title: "T", Labels: prdLabels(), HasPrdLink: true},
		createRunResult: store.Run{ID: uuid.New()},
	}
	guard := &fakeGuard{res: privcheck.GuardResult{Blocked: false}}
	svc := New(fs, newBox(t), testParams())
	svc.SetRepoGuard(guard)

	if _, err := svc.CreateRun(context.Background(), user, repo, 4, "desc", false, nil, nil); err != nil {
		t.Fatalf("CreateRun with a clearing guard: %v", err)
	}
	if guard.called != 1 {
		t.Fatalf("guard called %d times, want 1", guard.called)
	}
	if fs.createRunParams == nil {
		t.Fatal("CreateRun store insert must run when the guard clears the repo")
	}
}

// TestCreateRunNilGuardNotBlocked: with SetRepoGuard never called (guard == nil) the
// create path is NOT blocked by the guardrail — the service-layer gate is skipped and
// layer 3 (the claim backstop) is the net (PRD #66 M5, RepoGuard doc).
func TestCreateRunNilGuardNotBlocked(t *testing.T) {
	user, repo := uuid.New(), uuid.New()
	fs := &fakeStore{
		repoRow:         aValidRepoRow(),
		issueByID:       store.Issue{Title: "T", Labels: prdLabels(), HasPrdLink: true},
		createRunResult: store.Run{ID: uuid.New()},
	}
	svc := New(fs, newBox(t), testParams()) // SetRepoGuard deliberately not called

	if _, err := svc.CreateRun(context.Background(), user, repo, 4, "desc", false, nil, nil); err != nil {
		t.Fatalf("CreateRun with a nil guard must not be guardrail-blocked: %v", err)
	}
	if fs.createRunParams == nil {
		t.Fatal("CreateRun store insert must run when no guard is wired")
	}
}
