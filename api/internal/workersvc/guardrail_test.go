package workersvc

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/privcheck"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// fakeGuard is the workersvc test double for RepoGuard (the #66 default-branch
// guardrail, D1 layer 2). It returns a fixed GuardResult and records how many times
// GuardRepo was called, so a test can assert the gate ran and stopped the create
// path before any deeper store write.
type fakeGuard struct {
	res       privcheck.GuardResult
	called    int
	lastInput privcheck.GuardInput // the most recent GuardInput, for asserting Overridden threading (M8)
}

func (g *fakeGuard) GuardRepo(_ context.Context, in privcheck.GuardInput) privcheck.GuardResult {
	g.called++
	g.lastInput = in
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
		issueByID:       store.Issue{Title: "T", Labels: uziLabels(), HasPrdLink: true},
		createRunResult: store.Run{ID: uuid.New()},
	}
	guard, wantMsgs := blockedGuard()
	svc := New(fs, newBox(t), testParams())
	svc.SetRepoGuard(guard)

	_, err := svc.CreateRun(context.Background(), user, repo, 4, "desc", nil, nil, false, nil)
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

	_, err := svc.CreateSelfImproveRun(context.Background(), user, repo, 7, "t", "d", nil, false)
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

	_, err := svc.CreatePromptRun(context.Background(), user, repo, sched, "t", "p", false, false, nil, nil, false)
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

	if _, err := svc.CreatePromptRun(context.Background(), user, repo, sched, "t", "p", false, false, nil, nil, false); err != nil {
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
		issueByID:       store.Issue{Title: "T", Labels: uziLabels(), HasPrdLink: true},
		createRunResult: store.Run{ID: uuid.New()},
	}
	guard := &fakeGuard{res: privcheck.GuardResult{Blocked: false}}
	svc := New(fs, newBox(t), testParams())
	svc.SetRepoGuard(guard)

	if _, err := svc.CreateRun(context.Background(), user, repo, 4, "desc", nil, nil, false, nil); err != nil {
		t.Fatalf("CreateRun with a clearing guard: %v", err)
	}
	if guard.called != 1 {
		t.Fatalf("guard called %d times, want 1", guard.called)
	}
	if fs.createRunParams == nil {
		t.Fatal("CreateRun store insert must run when the guard clears the repo")
	}
}

// TestClaimGuardrailBlocksAtClaim is the D1 layer 3 backstop (PRD #66 M6): a queued
// PAT-bearing run whose repo the bot can now reach is refused AT CLAIM — the run is
// marked failed with the guardrail reason, the failed notify fires, and the worker
// gets NO payload (idle), never a 500. This is the Success Criterion "a run queued
// while main was protected, and claimed after protection was removed, fails at claim
// rather than pushing."
func TestClaimGuardrailBlocksAtClaim(t *testing.T) {
	f := newClaimFixture(t)
	guard, _ := blockedGuard()
	f.svc.SetRepoGuard(guard)

	payload, err := f.svc.Claim(context.Background(), store.Worker{ID: uuid.New(), UserID: f.owner})
	if err != nil {
		t.Fatalf("Claim must report idle (nil error) for a guardrail-blocked run, got %v", err)
	}
	if payload != nil {
		t.Fatalf("a guardrail-blocked claim must return NO payload, got %+v", payload)
	}
	if guard.called != 1 {
		t.Fatalf("guard called %d times, want 1", guard.called)
	}
	if f.fs.markedFailed == nil {
		t.Fatal("a guardrail-blocked claim must mark the run failed")
	}
	if f.fs.markedFailed.ID != f.runID {
		t.Fatalf("failed run id = %v, want %v", f.fs.markedFailed.ID, f.runID)
	}
	if reason := f.fs.markedFailed.FailureReason.String; !strings.Contains(reason, "default-branch guardrail") {
		t.Fatalf("failure reason = %q, want it to name the guardrail", reason)
	}
	// The block finding messages ride the reason (safe to store — no secret bytes),
	// and never the PAT ciphertext.
	if reason := f.fs.markedFailed.FailureReason.String; !strings.Contains(reason, "the bot can push to the default branch") {
		t.Fatalf("failure reason = %q, want the block finding message", reason)
	}
	// A blocked claim aborts before openAnthropic, so no Anthropic credential is
	// recorded on the run — proof the claim short-circuited (the guard runs before
	// the bot-PAT box.Open too, but this assertion gates the downstream record, not
	// box.Open directly).
	if len(f.fs.recordedCreds) != 0 {
		t.Fatalf("a blocked claim must record no credential, got %d", len(f.fs.recordedCreds))
	}
}

// TestClaimGuardrailNotBlockedProceeds: a clearing guard (Blocked=false) lets the
// claim proceed to a full payload, and the guard was consulted exactly once.
func TestClaimGuardrailNotBlockedProceeds(t *testing.T) {
	f := newClaimFixture(t)
	guard := &fakeGuard{res: privcheck.GuardResult{Blocked: false}}
	f.svc.SetRepoGuard(guard)

	payload, err := f.svc.Claim(context.Background(), store.Worker{ID: uuid.New(), UserID: f.owner})
	if err != nil {
		t.Fatalf("Claim with a clearing guard: %v", err)
	}
	if payload == nil {
		t.Fatal("expected a payload when the guard clears the repo, got idle")
	}
	if guard.called != 1 {
		t.Fatalf("guard called %d times, want 1", guard.called)
	}
	if f.fs.markedFailed != nil {
		t.Fatalf("a cleared claim must not fail the run, got %+v", f.fs.markedFailed)
	}
}

// TestClaimJudgeSkipsGuard: a judge run forks before GetRunClaimContext and carries
// no PAT (PRD #66 out-of-scope), so the claim backstop must never invoke the guard
// for it — guard.called stays 0.
func TestClaimJudgeSkipsGuard(t *testing.T) {
	box := newBox(t)
	sealedTok, _ := box.Seal([]byte("anthropic-judge-token-abcdef1234567890"))
	uid, target := uuid.New(), uuid.New()
	fs := &fakeStore{claimRun: judgeRun(uid, target), anthropic: sealedTok}
	svc := New(fs, box, testParams())
	guard := &fakeGuard{res: privcheck.GuardResult{Blocked: true}}
	svc.SetRepoGuard(guard)

	payload, err := svc.Claim(context.Background(), store.Worker{ID: uuid.New(), UserID: uid})
	if err != nil {
		t.Fatalf("Claim (judge): %v", err)
	}
	if payload == nil || payload.Kind != runkind.Judge {
		t.Fatalf("expected a judge payload, got %+v", payload)
	}
	if guard.called != 0 {
		t.Fatalf("the guard must never run for a judge claim, called %d times", guard.called)
	}
}

// TestClaimNilGuardBackstopSkips: with no guard wired (SetRepoGuard never called), the
// claim backstop is a no-op and the claim proceeds — the same nil-safety as layer 2.
func TestClaimNilGuardBackstopSkips(t *testing.T) {
	f := newClaimFixture(t) // SetRepoGuard deliberately not called

	payload, err := f.svc.Claim(context.Background(), store.Worker{ID: uuid.New(), UserID: f.owner})
	if err != nil {
		t.Fatalf("Claim with a nil guard: %v", err)
	}
	if payload == nil {
		t.Fatal("a nil guard must not block the claim, got idle")
	}
	if f.fs.markedFailed != nil {
		t.Fatalf("a nil guard must not fail the run, got %+v", f.fs.markedFailed)
	}
}

// TestCreateRunThreadsOverriddenFromRow (PRD #66 M8): when GetRepoForUser reports a
// non-NULL guardrail_override_reason, the service-layer gate passes Overridden=true to
// GuardRepo so the shared evaluator can downgrade the waivable findings. A NULL reason
// passes Overridden=false. The gate only flips the input bool — it never itself waives
// protection_unreadable (that stays the evaluator's job, preserved by construction).
func TestCreateRunThreadsOverriddenFromRow(t *testing.T) {
	for _, tc := range []struct {
		name      string
		reason    pgtype.Text
		wantOverr bool
	}{
		{"override active", pgtype.Text{String: "admin accepted the risk", Valid: true}, true},
		{"no override", pgtype.Text{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			user, repo := uuid.New(), uuid.New()
			row := aValidRepoRow()
			row.GuardrailOverrideReason = tc.reason
			fs := &fakeStore{
				repoRow:         row,
				issueByID:       store.Issue{Title: "T", Labels: uziLabels(), HasPrdLink: true},
				createRunResult: store.Run{ID: uuid.New()},
			}
			guard := &fakeGuard{res: privcheck.GuardResult{Blocked: false}}
			svc := New(fs, newBox(t), testParams())
			svc.SetRepoGuard(guard)

			if _, err := svc.CreateRun(context.Background(), user, repo, 4, "desc", nil, nil, false, nil); err != nil {
				t.Fatalf("CreateRun: %v", err)
			}
			if guard.lastInput.Overridden != tc.wantOverr {
				t.Fatalf("GuardInput.Overridden = %v, want %v", guard.lastInput.Overridden, tc.wantOverr)
			}
		})
	}
}

// TestClaimThreadsOverriddenFromContext (PRD #66 M8): the claim backstop reads the
// override off GetRunClaimContext's guardrail_override_reason and threads it into the
// GuardInput. A cleared guard is used so the claim proceeds; the assertion is purely
// that the bool crossed over correctly.
func TestClaimThreadsOverriddenFromContext(t *testing.T) {
	for _, tc := range []struct {
		name      string
		reason    pgtype.Text
		wantOverr bool
	}{
		{"override active", pgtype.Text{String: "admin accepted the risk", Valid: true}, true},
		{"no override", pgtype.Text{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newClaimFixture(t)
			f.fs.claimCtx.GuardrailOverrideReason = tc.reason
			guard := &fakeGuard{res: privcheck.GuardResult{Blocked: false}}
			f.svc.SetRepoGuard(guard)

			if _, err := f.svc.Claim(context.Background(), store.Worker{ID: uuid.New(), UserID: f.owner}); err != nil {
				t.Fatalf("Claim: %v", err)
			}
			if guard.lastInput.Overridden != tc.wantOverr {
				t.Fatalf("GuardInput.Overridden = %v, want %v", guard.lastInput.Overridden, tc.wantOverr)
			}
		})
	}
}

// TestCreateRunNilGuardNotBlocked: with SetRepoGuard never called (guard == nil) the
// create path is NOT blocked by the guardrail — the service-layer gate is skipped and
// layer 3 (the claim backstop) is the net (PRD #66 M5, RepoGuard doc).
func TestCreateRunNilGuardNotBlocked(t *testing.T) {
	user, repo := uuid.New(), uuid.New()
	fs := &fakeStore{
		repoRow:         aValidRepoRow(),
		issueByID:       store.Issue{Title: "T", Labels: uziLabels(), HasPrdLink: true},
		createRunResult: store.Run{ID: uuid.New()},
	}
	svc := New(fs, newBox(t), testParams()) // SetRepoGuard deliberately not called

	if _, err := svc.CreateRun(context.Background(), user, repo, 4, "desc", nil, nil, false, nil); err != nil {
		t.Fatalf("CreateRun with a nil guard must not be guardrail-blocked: %v", err)
	}
	if fs.createRunParams == nil {
		t.Fatal("CreateRun store insert must run when no guard is wired")
	}
}
