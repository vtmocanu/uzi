package workersvc

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #3248: a claim assembled for a Codex-harness ISSUE run carries the summary model
// of the Codex lane (user lane > instance Codex value > built-in), end to end through
// assembleClaim with a real Codex binding. The hermetic TestClaimSummaryModelLanes
// covers the resolver alone; this proves the claim assembly passes the run's harness.
func TestCodexClaimCarriesSummaryModelLiveDB(t *testing.T) {
	f := newJudgeLaneFixture(t) // a Codex-capable subscription alias, an Anthropic token and a bot PAT
	env := f.env
	claim := func(t *testing.T, userCodex any, instance map[string]string) string {
		t.Helper()
		env.exec(`UPDATE users SET summary_model = 'opus', codex_summary_model = $2 WHERE id = $1`, f.userID, userCodex)
		env.exec(`DELETE FROM app_settings WHERE key IN ('summary_model', 'codex_summary_model')`)
		for k, v := range instance {
			env.exec(`INSERT INTO app_settings (key, value) VALUES ($1, $2)`, k, v)
		}
		t.Cleanup(func() {
			env.exec(`DELETE FROM app_settings WHERE key IN ('summary_model', 'codex_summary_model')`)
		})
		runID := uuid.New()
		env.exec(`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, kind, status, worker_id, harness)
		          VALUES ($1, $2, $3, $4, 't', 'd', 'issue', 'claimed', $5, 'codex')`,
			runID, f.userID, f.repoID, int64(runID.ID()), f.workerID)
		svc := New(env.q, env.box, testParams())
		svc.SetVault(env.vlt)
		svc.SetSettings(settings.New(env.q, time.Minute))
		if err := svc.FreezeCodexBinding(env.ctx, f.userID, runID, f.aliasID, codexAuthModeSubscription); err != nil {
			t.Fatalf("FreezeCodexBinding: %v", err)
		}
		payload, err := svc.assembleClaim(env.ctx, store.Worker{ID: f.workerID, UserID: f.userID}, mustRun(t, env, runID))
		if err != nil {
			t.Fatalf("assembleClaim (codex issue): %v", err)
		}
		if payload.SummaryModel == nil {
			t.Fatal("Codex issue claim carries no SummaryModel")
		}
		return *payload.SummaryModel
	}

	t.Run("owner's Codex lane wins over the Claude lane and the instance value", func(t *testing.T) {
		got := claim(t, "gpt-6-astra", map[string]string{settings.KeyCodexSummaryModel: "gpt-6-sol", settings.KeySummaryModel: "sonnet"})
		if got != "gpt-6-astra" {
			t.Fatalf("SummaryModel = %q, want gpt-6-astra", got)
		}
	})
	t.Run("no user value and no instance value resolves the Codex built-in", func(t *testing.T) {
		got := claim(t, nil, nil)
		if got != "gpt-6-luna" {
			t.Fatalf("SummaryModel = %q, want gpt-6-luna", got)
		}
	})
}
