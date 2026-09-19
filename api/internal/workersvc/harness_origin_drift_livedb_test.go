package workersvc

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/forge"
)

// harness_origin_drift_livedb_test.go is the PRD #1429 M2 acceptance inventory (D4): every
// rewired production run-creation origin must resolve and freeze the SAME harness a
// Codex-only or Claude-only user would get under D11 — by the actual PRODUCTION CALL PATH
// (workersvc.Service methods the handlers/scheduler/poller call), never by inserting through
// the store directly. It reuses the codexTestEnv fixtures. Skipped unless
// UZI_TEST_DATABASE_URL points at a throwaway Postgres; run via ./e2e/run-store-it.sh. Every
// name ends LiveDB so the store-IT sweep selects it.

// seedCodexOnlyOriginUser seeds a user+repo whose ONLY usable harness is Codex (an
// openai_api_key default; no Anthropic token).
func seedCodexOnlyOriginUser(t *testing.T, env codexTestEnv) (userID, repoID uuid.UUID) {
	t.Helper()
	userID, _, repoID = env.seedCodexInfra(t)
	alias := env.seedStaticAPIKey(t, userID, "codex-key-"+uuid.NewString(), codexToken("sk"))
	makeCodexDefault(t, env, alias)
	return userID, repoID
}

// seedClaudeOnlyOriginUser seeds a user+repo whose ONLY usable harness is Claude.
func seedClaudeOnlyOriginUser(t *testing.T, env codexTestEnv) (userID, repoID uuid.UUID) {
	t.Helper()
	userID, _, repoID = env.seedCodexInfra(t)
	seedAnthropicToken(t, env, userID)
	return userID, repoID
}

// seedEligibleIssue inserts a cached, uzi-labelled issue row so the run-eligibility gate
// (PRD #764/#767) admits it — the same jsonb the board renders from.
func seedEligibleIssue(t *testing.T, env codexTestEnv, repoID uuid.UUID, iid int64) {
	t.Helper()
	env.exec(`INSERT INTO issues (id, repo_id, forge_issue_iid, title, state, labels, web_url, has_prd_link, forge_updated_at, synced_at)
	          VALUES ($1, $2, $3, 'origin-drift issue', 'opened', '["uzi"]', 'https://forge.e2e/i', false, now(), now())`,
		uuid.New(), repoID, iid)
}

// seedPromptScheduleRow inserts the minimal run_schedules row a prompt-origin run's
// runs.schedule_id FK needs, returning its id.
func seedPromptScheduleRow(t *testing.T, env codexTestEnv, userID, repoID uuid.UUID) uuid.UUID {
	t.Helper()
	schedID := uuid.New()
	env.exec(`INSERT INTO run_schedules (id, user_id, repo_id, target, prompt, timing, run_at, timezone)
	          VALUES ($1, $2, $3, 'prompt', 'do the origin-drift thing', 'once', now(), 'UTC')`,
		schedID, userID, repoID)
	return schedID
}

// originDriftForge is the minimal forge.Forge fake StartRunForUser needs: GetIssue (its
// Description rides the run) and ListIssueComments (the best-effort D6 snapshot fetch), both
// overridden so the embedded nil forge.Forge is never actually called.
type originDriftForge struct {
	forge.Forge
}

func (f *originDriftForge) GetIssue(_ context.Context, _ int64, iid int64) (forge.Issue, error) {
	return forge.Issue{IID: iid, Title: "origin-drift issue", Description: "d", State: "opened"}, nil
}

func (f *originDriftForge) ListIssueComments(_ context.Context, _ int64, _ int64) ([]forge.IssueComment, error) {
	return nil, nil
}

// originDriftForges is the ForgeBuilder seam: every connection resolves to the one fake.
type originDriftForges struct{ f forge.Forge }

func (b originDriftForges) ForgeForConnection(_ string, _ string, _ []byte) (forge.Forge, error) {
	return b.f, nil
}

// repoPathFor reconstructs the path_with_namespace seedCodexInfra stamps, so a chat
// start_run test (which is keyed by path, not id) can resolve back to the same repo.
func repoPathFor(repoID uuid.UUID) string { return "g/" + repoID.String() }

// assertRunHarness re-reads runID and fails unless its committed harness matches want — the
// freeze (when Codex) runs AFTER the INSERT's own RETURNING, so the post-commit row is the
// only trustworthy read (mirrors create_run_atomic_livedb_test.go).
func assertRunHarness(t *testing.T, env codexTestEnv, runID uuid.UUID, want Harness) {
	t.Helper()
	got := mustRun(t, env, runID)
	if got.Harness != string(want) {
		t.Fatalf("run %s harness = %q, want %q", runID, got.Harness, want)
	}
	if want == HarnessCodex {
		if !got.CodexSecretID.Valid {
			t.Fatalf("run %s harness=codex but codex_secret_id is not frozen", runID)
		}
		if !got.CodexMaterialRevision.Valid {
			t.Fatalf("run %s harness=codex but codex_material_revision is not frozen", runID)
		}
	}
}

// runOriginDriftOrigins exercises every rewired production origin against userID/repoID/svc
// and asserts each lands on want. iidBase seeds a distinct issue-iid/ref range per call so two
// invocations against the same user/repo (e.g. Codex-only then explicit-pin) never collide.
func runOriginDriftOrigins(t *testing.T, env codexTestEnv, svc *Service, userID, repoID uuid.UUID, want Harness, iidBase int64) {
	t.Helper()

	t.Run("manual issue create (Service.CreateRun)", func(t *testing.T) {
		iid := iidBase + 1
		seedEligibleIssue(t, env, repoID, iid)
		run, err := svc.CreateRun(env.ctx, userID, repoID, iid, "desc", nil, nil, false, nil, nil, nil)
		if err != nil {
			t.Fatalf("CreateRun: %v", err)
		}
		assertRunHarness(t, env, run.ID, want)
	})

	t.Run("chat start_run (Service.StartRunForUserByPath)", func(t *testing.T) {
		iid := iidBase + 2
		seedEligibleIssue(t, env, repoID, iid)
		run, err := svc.StartRunForUserByPath(env.ctx, userID, repoPathFor(repoID), iid, nil, nil, nil)
		if err != nil {
			t.Fatalf("StartRunForUserByPath: %v", err)
		}
		assertRunHarness(t, env, run.ID, want)
	})

	t.Run("scheduled issue (Service.CreateScheduledRun)", func(t *testing.T) {
		iid := iidBase + 3
		seedEligibleIssue(t, env, repoID, iid)
		run, err := svc.CreateScheduledRun(env.ctx, userID, repoID, iid, "desc", nil, nil, nil, false, nil, nil, nil)
		if err != nil {
			t.Fatalf("CreateScheduledRun: %v", err)
		}
		assertRunHarness(t, env, run.ID, want)
	})

	t.Run("scheduled autopilot (Service.CreateScheduledAutopilotRun)", func(t *testing.T) {
		iid := iidBase + 4
		seedEligibleIssue(t, env, repoID, iid)
		waitOnLimit := false
		run, err := svc.CreateScheduledAutopilotRun(env.ctx, userID, repoID, iid, "desc", &waitOnLimit, nil, nil, false, nil, nil)
		if err != nil {
			t.Fatalf("CreateScheduledAutopilotRun: %v", err)
		}
		assertRunHarness(t, env, run.ID, want)
	})

	// Fix 2 (review): the label-poller's autopilot origin, distinct from the scheduled
	// autopilot case above — CreateAutopilotRun is its OWN seam (the poller's, kept
	// deliberately untouched from the scheduler's CreateScheduledAutopilotRun so a widened
	// scheduler seam can never change label-driven autopilot behaviour), and until now it had
	// no origin-drift coverage of its own alongside the other rewired origins in this table.
	t.Run("label-poller autopilot (Service.CreateAutopilotRun)", func(t *testing.T) {
		iid := iidBase + 6
		seedEligibleIssue(t, env, repoID, iid)
		run, err := svc.CreateAutopilotRun(env.ctx, userID, repoID, iid, "desc")
		if err != nil {
			t.Fatalf("CreateAutopilotRun: %v", err)
		}
		assertRunHarness(t, env, run.ID, want)
	})

	t.Run("prompt (Service.CreatePromptRun)", func(t *testing.T) {
		schedID := seedPromptScheduleRow(t, env, userID, repoID)
		run, err := svc.CreatePromptRun(env.ctx, userID, repoID, schedID, "t", "p", false, false, nil, nil, false, nil, nil)
		if err != nil {
			t.Fatalf("CreatePromptRun: %v", err)
		}
		assertRunHarness(t, env, run.ID, want)
	})

	t.Run("self-improve (Service.CreateSelfImproveRun)", func(t *testing.T) {
		iid := iidBase + 5
		run, err := svc.CreateSelfImproveRun(env.ctx, userID, repoID, iid, "t", "d", nil, nil, false, nil)
		if err != nil {
			t.Fatalf("CreateSelfImproveRun: %v", err)
		}
		assertRunHarness(t, env, run.ID, want)
	})

	t.Run("CI-fix (Service.CreateAutoCIFixRun)", func(t *testing.T) {
		ref := "origin-drift-cifix-" + uuid.NewString()
		run, err := svc.CreateAutoCIFixRun(env.ctx, userID, repoID, ref, "Fix CI", "desc", sampleSnapshot(), nil)
		if err != nil {
			t.Fatalf("CreateAutoCIFixRun: %v", err)
		}
		assertRunHarness(t, env, run.ID, want)
	})

	t.Run("task handoff (Service.CreateTaskRun)", func(t *testing.T) {
		run, err := svc.CreateTaskRun(env.ctx, userID, repoID, "inline context", "", false, false, false, false)
		if err != nil {
			t.Fatalf("CreateTaskRun: %v", err)
		}
		assertRunHarness(t, env, run.ID, want)
	})
}

// TestOriginDriftCodexOnlyLiveDB (D4): for a Codex-only user, every rewired origin produces a
// run with harness='codex' AND a frozen codex_secret_id, by the production call path.
func TestOriginDriftCodexOnlyLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, repoID := seedCodexOnlyOriginUser(t, env)
	svc := atomicSvc(env)
	svc.SetForges(originDriftForges{f: &originDriftForge{}})

	runOriginDriftOrigins(t, env, svc, userID, repoID, HarnessCodex, 90000)
}

// TestOriginDriftClaudeOnlyLiveDB (D4): for a Claude-only user, every rewired origin produces
// a run with harness='claude', by the production call path.
func TestOriginDriftClaudeOnlyLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, repoID := seedClaudeOnlyOriginUser(t, env)
	svc := atomicSvc(env)
	svc.SetForges(originDriftForges{f: &originDriftForge{}})

	runOriginDriftOrigins(t, env, svc, userID, repoID, HarnessClaude, 91000)
}

// TestOriginDriftExplicitPinHonoredLiveDB (D2/D4): a user with BOTH harnesses usable (D11's
// implicit tiebreak would choose Claude) still gets Codex on the manual create path when the
// request explicitly pins it — an explicit selection is never overridden by the implicit
// default.
func TestOriginDriftExplicitPinHonoredLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, _, repoID := env.seedCodexInfra(t)
	seedAnthropicToken(t, env, userID)
	alias := env.seedStaticAPIKey(t, userID, "codex-key-"+uuid.NewString(), codexToken("sk"))
	makeCodexDefault(t, env, alias)

	svc := atomicSvc(env)
	const iid = 92001
	seedEligibleIssue(t, env, repoID, iid)
	codex := HarnessCodex
	run, err := svc.CreateRun(env.ctx, userID, repoID, iid, "desc", nil, nil, false, nil, &codex, nil)
	if err != nil {
		t.Fatalf("CreateRun with explicit codex pin: %v", err)
	}
	assertRunHarness(t, env, run.ID, HarnessCodex)
}

// TestOriginDriftChatConversationStaysClaudeLiveDB (D4): the two Chat CONVERSATION inserts —
// CreateChatRun and CreateChatContinueRun — remain LITERAL Claude exceptions even for a
// Codex-only user; the Chat conversation executor has no Codex implementation.
func TestOriginDriftChatConversationStaysClaudeLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, repoID := seedCodexOnlyOriginUser(t, env)
	_ = repoID
	svc := New(env.q, env.box, testParams())

	run, err := svc.CreateChatRun(env.ctx, userID, "hello from a codex-only user")
	if err != nil {
		t.Fatalf("CreateChatRun: %v", err)
	}
	assertRunHarness(t, env, run.ID, HarnessClaude)

	// End it, then continue it — CreateChatContinueRun must ALSO stay literal Claude.
	env.exec(`UPDATE runs SET status = 'completed' WHERE id = $1`, run.ID)
	cont, err := svc.ContinueChat(env.ctx, userID, run.ID)
	if err != nil {
		t.Fatalf("ContinueChat: %v", err)
	}
	assertRunHarness(t, env, cont.ID, HarnessClaude)
}
