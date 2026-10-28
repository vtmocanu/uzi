package workersvc

import (
	"testing"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestClaimCodexRoleLaneUnchangedLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	f := newSubscriptionFixture(t, env)
	resealBotPAT(t, env, f.userID)
	svc := New(env.q, env.box, testParams())
	svc.SetVault(env.vlt)
	run := mustRun(t, env, f.runID)
	w := store.Worker{ID: f.workerID, UserID: f.userID, ProtocolCapabilities: []string{capability.CodexHarnessV1, capability.CodexRuntimeV2, capability.CodexCustomModelV1}}
	// Luna is deliberately included: the Codex lane accepts it as a custom root under
	// the existing curated capability rule. The Claude lane must not affect this claim.
	models := append(codexRoleFixture(t), "custom-codex-preview", "")
	for _, model := range models {
		t.Run("lane/"+model, func(t *testing.T) {
			var lane any
			if model != "" {
				lane = model
			}
			env.exec("UPDATE users SET default_claude_model='gpt-6-luna', default_codex_model=$2 WHERE id=$1", f.userID, lane)
			pl, err := svc.assembleClaim(env.ctx, w, run)
			if err != nil {
				t.Fatal(err)
			}
			if pl == nil {
				t.Fatal("missing claim")
			}
			if (pl.Config.DefaultModel == nil) != (model == "") || (pl.Config.DefaultModel != nil && *pl.Config.DefaultModel != model) {
				t.Fatalf("Codex model = %v, want %q", pl.Config.DefaultModel, model)
			}
			lanes, err := env.q.GetUserHarnessModelDefaults(env.ctx, f.userID)
			if err != nil {
				t.Fatal(err)
			}
			if lanes.DefaultClaudeModel.String != "gpt-6-luna" || lanes.DefaultCodexModel.Valid != (model != "") || lanes.DefaultCodexModel.String != model {
				t.Fatalf("stored lanes changed: %+v", lanes)
			}
			if n := env.countHarnessModelFallbackNotes(t, f.runID); n != 0 {
				t.Fatalf("unexpected fallback notes: %d", n)
			}
		})
	}
}
