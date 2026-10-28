package workersvc

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #3245: a judge claim resolves its model per harness lane: user lane > instance
// value for the harness > built-in, and a stored value invalid for its lane is skipped
// with at most one harness_model_fallback note.

// judgeLaneFixture seeds one user with a Codex-capable and an Anthropic credential and
// returns helpers to claim a fresh judge run of either harness.
type judgeLaneFixture struct {
	env      codexTestEnv
	userID   uuid.UUID
	workerID uuid.UUID
	aliasID  uuid.UUID
	repoID   uuid.UUID
}

func newJudgeLaneFixture(t *testing.T) judgeLaneFixture {
	t.Helper()
	env := setupCodexLiveDB(t)
	userID, workerID, repoID := env.seedCodexInfra(t)
	aliasID := env.seedLinkedSubscription(t, userID, "judge-lanes-"+uuid.NewString(), codexToken("access"), codexToken("refresh"))
	seedDefaultAnthropicToken(t, env, userID)
	resealBotPAT(t, env, userID)
	return judgeLaneFixture{env: env, userID: userID, workerID: workerID, aliasID: aliasID, repoID: repoID}
}

// claim assembles a fresh judge run of harness against the given user lanes and
// instance rows, returning the claimed model ("" when nil) and the fallback-note count.
func (f judgeLaneFixture) claim(t *testing.T, harness string, userClaude, userCodex any, instance map[string]string, enforceAll bool) (string, int) {
	t.Helper()
	env := f.env
	env.exec(`UPDATE users SET judge_model = $2, codex_judge_model = $3, judge_enabled = $4 WHERE id = $1`,
		f.userID, userClaude, userCodex, !enforceAll)
	env.exec(`DELETE FROM app_settings WHERE key IN ('judge_model', 'codex_judge_model', 'judge_enforce_all')`)
	for k, v := range instance {
		env.exec(`INSERT INTO app_settings (key, value) VALUES ($1, $2)`, k, v)
	}
	if enforceAll {
		env.exec(`INSERT INTO app_settings (key, value) VALUES ('judge_enforce_all', 'true')`)
	}
	t.Cleanup(func() {
		env.exec(`DELETE FROM app_settings WHERE key IN ('judge_model', 'codex_judge_model', 'judge_enforce_all')`)
	})

	// One active judge per target is enforced, so every claim reviews its own run.
	target := uuid.New()
	env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, worker_id)
	          VALUES ($1, $2, $3, 'issue', $4, 't', 'd', 'completed', $5)`,
		target, f.userID, f.repoID, int64(target.ID()), f.workerID)
	judgeID := uuid.New()
	env.exec(`INSERT INTO runs (id, user_id, kind, target_run_id, issue_title, issue_description, status, worker_id, trigger_source, harness)
	          VALUES ($1, $2, 'judge', $3, 'Judge of run', '', 'claimed', $4, 'judge', $5)`,
		judgeID, f.userID, target, f.workerID, harness)
	svc := New(env.q, env.box, testParams())
	svc.SetVault(env.vlt)
	svc.SetSettings(settings.New(env.q, time.Minute))
	if harness == "codex" {
		if err := svc.FreezeCodexBinding(env.ctx, f.userID, judgeID, f.aliasID, codexAuthModeSubscription); err != nil {
			t.Fatalf("FreezeCodexBinding: %v", err)
		}
	}
	payload, err := svc.assembleClaim(env.ctx, store.Worker{ID: f.workerID, UserID: f.userID}, mustRun(t, env, judgeID))
	if err != nil {
		t.Fatalf("assembleClaim (%s judge): %v", harness, err)
	}
	model := ""
	if payload.JudgeModel != nil {
		model = *payload.JudgeModel
	}
	return model, env.countHarnessModelFallbackNotes(t, judgeID)
}

func TestAssembleJudgeClaimCodexLaneLiveDB(t *testing.T) {
	f := newJudgeLaneFixture(t)
	instCodex := func(v string) map[string]string { return map[string]string{settings.KeyCodexJudgeModel: v} }
	for _, tc := range []struct {
		name       string
		userClaude any
		userCodex  any
		instance   map[string]string
		enforceAll bool
		wantModel  string
		wantNotes  int
	}{
		{name: "default is the built-in with no note", wantModel: "gpt-6.1-sol"},
		{name: "user Codex lane wins over the instance value", userCodex: "gpt-6-astra", instance: instCodex("gpt-6-sol"), wantModel: "gpt-6-astra"},
		{name: "instance Codex value is used", instance: instCodex("gpt-6-sol"), wantModel: "gpt-6-sol"},
		{name: "instance reset row falls to the built-in", instance: instCodex(""), wantModel: "gpt-6.1-sol"},
		{name: "the Claude user lane is ignored on a Codex judge", userClaude: "haiku", wantModel: "gpt-6.1-sol"},
		{name: "a stored Claude alias in the Codex lane gives one note", userCodex: "opus", wantModel: "gpt-6.1-sol", wantNotes: 1},
	} {
		for _, enforced := range []bool{false, true} {
			name := tc.name
			if enforced {
				name += " (enforce-all, opt-in off)"
			}
			t.Run(name, func(t *testing.T) {
				model, notes := f.claim(t, "codex", tc.userClaude, tc.userCodex, tc.instance, enforced)
				if model != tc.wantModel || notes != tc.wantNotes {
					t.Fatalf("judge_model = %q with %d notes, want %q with %d", model, notes, tc.wantModel, tc.wantNotes)
				}
			})
		}
	}
}

func TestAssembleJudgeClaimClaudeLaneLiveDB(t *testing.T) {
	f := newJudgeLaneFixture(t)
	instClaude := func(v string) map[string]string { return map[string]string{settings.KeyJudgeModel: v} }
	for _, tc := range []struct {
		name       string
		userClaude any
		userCodex  any
		instance   map[string]string
		wantModel  string
		wantNotes  int
	}{
		{name: "default is the built-in opus with no note", wantModel: "opus"},
		{name: "user Claude lane wins", userClaude: "haiku", instance: instClaude("sonnet"), wantModel: "haiku"},
		{name: "a valid instance alias gives no note", instance: instClaude("sonnet"), wantModel: "sonnet"},
		{name: "a curated Codex id in the instance Claude key gives one note and opus", instance: instClaude("gpt-6-sol"), wantModel: "opus", wantNotes: 1},
		{name: "a legacy user Codex id falls to the instance value with one note", userClaude: "gpt-6-astra", instance: instClaude("sonnet"), wantModel: "sonnet", wantNotes: 1},
		{name: "two invalid tiers still give a single note", userClaude: "gpt-6-astra", instance: instClaude("gpt-6-sol"), wantModel: "opus", wantNotes: 1},
		{name: "the Codex user lane is ignored on a Claude judge", userCodex: "gpt-6-astra", wantModel: "opus"},
	} {
		for _, enforced := range []bool{false, true} {
			name := tc.name
			if enforced {
				name += " (enforce-all, opt-in off)"
			}
			t.Run(name, func(t *testing.T) {
				model, notes := f.claim(t, "claude", tc.userClaude, tc.userCodex, tc.instance, enforced)
				if model != tc.wantModel || notes != tc.wantNotes {
					t.Fatalf("judge_model = %q with %d notes, want %q with %d", model, notes, tc.wantModel, tc.wantNotes)
				}
			})
		}
	}
}
