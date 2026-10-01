package workersvc

import (
	"testing"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestAssembleCodexClaimEffortLaneLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	f := newSubscriptionFixture(t, env)
	resealBotPAT(t, env, f.userID)
	svc := New(env.q, env.box, testParams())
	run := mustRun(t, env, f.runID)
	worker := store.Worker{ID: f.workerID, UserID: f.userID, ProtocolCapabilities: []string{capability.CodexHarnessV1, capability.CodexRuntimeV2, capability.CodexCustomModelV1}}
	for _, c := range []struct {
		shared string
		codex  any
		want   string
	}{
		{"max", "low", "low"}, {"low", nil, "medium"}, {"high", " \t ", "medium"}, {"low", "xhigh", "xhigh"},
	} {
		env.exec(`UPDATE users SET default_effort=$2,default_codex_effort=$3 WHERE id=$1`, f.userID, c.shared, c.codex)
		payload, err := svc.assembleClaim(env.ctx, worker, run)
		if err != nil {
			t.Fatal(err)
		}
		if payload.Config.DefaultEffort == nil || *payload.Config.DefaultEffort != c.want {
			t.Fatalf("shared=%q codex=%v: got=%v want=%q", c.shared, c.codex, payload.Config.DefaultEffort, c.want)
		}
	}
}

func TestAssembleCodexJudgeEffortLaneLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, workerID, repoID := env.seedCodexInfra(t)
	aliasID := env.seedLinkedSubscription(t, userID, "codex-effort", codexToken("access"), codexToken("refresh"))
	target := env.seedCodexRun(t, userID, workerID, repoID)
	judge := env.seedCodexJudgeRun(t, userID, workerID, target)
	svc := New(env.q, env.box, testParams())
	if err := svc.FreezeCodexBinding(env.ctx, userID, judge, aliasID, codexAuthModeSubscription); err != nil {
		t.Fatal(err)
	}
	run := mustRun(t, env, judge)
	for _, c := range []struct {
		codex any
		want  string
	}{{"low", "low"}, {nil, "medium"}, {" \t ", "medium"}} {
		env.exec(`UPDATE users SET default_effort='max',default_codex_effort=$2 WHERE id=$1`, userID, c.codex)
		payload, err := svc.assembleClaim(env.ctx, store.Worker{ID: workerID, UserID: userID}, run)
		if err != nil {
			t.Fatal(err)
		}
		if payload.Config.DefaultEffort == nil || *payload.Config.DefaultEffort != c.want {
			t.Fatalf("Codex judge effort got=%v want=%q", payload.Config.DefaultEffort, c.want)
		}
	}
}
