package workersvc

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #3247: a stored `runtime` preference yields claims with no default_effort key for the
// ordinary lanes of each harness. Skipped unless UZI_TEST_DATABASE_URL points at a throwaway
// Postgres (./e2e/run-store-it.sh).

func TestRuntimeEffortCodexIssueClaimLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	f := newSubscriptionFixture(t, env)
	resealBotPAT(t, env, f.userID)
	svc := New(env.q, env.box, testParams())
	svc.SetVault(env.vlt)
	run := mustRun(t, env, f.runID)
	worker := store.Worker{ID: f.workerID, UserID: f.userID, ProtocolCapabilities: []string{capability.CodexHarnessV1, capability.CodexRuntimeV2, capability.CodexCustomModelV1}}
	// The Claude column must not influence the Codex claim, and NULL still rides medium.
	env.exec(`UPDATE users SET default_effort='max',default_codex_effort=NULL WHERE id=$1`, f.userID)
	payload, err := svc.assembleClaim(env.ctx, worker, run)
	if err != nil || payload.Config.DefaultEffort == nil || *payload.Config.DefaultEffort != "medium" {
		t.Fatalf("NULL baseline: %v %v", payload, err)
	}
	env.exec(`UPDATE users SET default_effort='max',default_codex_effort='runtime' WHERE id=$1`, f.userID)
	payload, err = svc.assembleClaim(env.ctx, worker, run)
	if err != nil {
		t.Fatal(err)
	}
	requireNoDefaultEffortKey(t, "codex issue", payload.Config)
}

func TestRuntimeEffortCodexJudgeClaimLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, workerID, repoID := env.seedCodexInfra(t)
	aliasID := env.seedLinkedSubscription(t, userID, "codex-runtime", codexToken("access"), codexToken("refresh"))
	target := env.seedCodexRun(t, userID, workerID, repoID)
	judge := env.seedCodexJudgeRun(t, userID, workerID, target)
	svc := New(env.q, env.box, testParams())
	svc.SetVault(env.vlt)
	if err := svc.FreezeCodexBinding(env.ctx, userID, judge, aliasID, codexAuthModeSubscription); err != nil {
		t.Fatal(err)
	}
	run := mustRun(t, env, judge)
	env.exec(`UPDATE users SET default_effort='max',default_codex_effort='runtime' WHERE id=$1`, userID)
	payload, err := svc.assembleClaim(env.ctx, store.Worker{ID: workerID, UserID: userID}, run)
	if err != nil {
		t.Fatal(err)
	}
	requireNoDefaultEffortKey(t, "codex judge", payload.Config)
}

func TestRuntimeEffortClaudeIssueAndIsolatedClaimsLiveDB(t *testing.T) {
	t.Run("issue", func(t *testing.T) {
		f := newIsoFix(t)
		f.env.exec(`UPDATE users SET default_effort='runtime',default_codex_effort='max' WHERE id=$1`, f.userID)
		run := f.queuedRun(t, 401, false)
		p := f.claim(t, run)
		if p.IsolatedFetch != nil {
			t.Fatalf("expected an ordinary issue claim, got isolated: %+v", p.IsolatedFetch)
		}
		requireNoDefaultEffortKey(t, "claude issue", p.Config)
	})
	t.Run("isolated", func(t *testing.T) {
		f := newIsoFix(t)
		f.env.exec(`UPDATE users SET default_effort='runtime',default_codex_effort='max' WHERE id=$1`, f.userID)
		run := f.queuedRun(t, 402, true)
		p := f.claimAs(t, f.laneWorker(t, run), run)
		if p.IsolatedFetch == nil {
			t.Fatal("expected an isolated claim")
		}
		requireNoDefaultEffortKey(t, "claude isolated", p.Config)
	})
	t.Run("baseline medium", func(t *testing.T) {
		f := newIsoFix(t)
		run := f.queuedRun(t, 403, false)
		p := f.claim(t, run)
		if p.Config.DefaultEffort == nil || *p.Config.DefaultEffort != "medium" {
			t.Fatalf("NULL preference: DefaultEffort = %v, want medium", p.Config.DefaultEffort)
		}
	})
}

func TestRuntimeEffortJobClaimLiveDB(t *testing.T) {
	e := setupJobLiveDB(t, 0)
	u := e.seedJobUser(t)
	e.makeTokenDefault(t, u)
	e.exec(`UPDATE users SET default_effort='runtime' WHERE id=$1`, u)
	v, err := e.svc.CreateJobRun(e.ctx, jobReq(cliCaller(u)))
	if err != nil {
		t.Fatal(err)
	}
	capID := e.seedWorkerRow(t, u, false, nil, jobCap, jobFilesCap, capability.RecoveryArchiveV1)
	capable := store.Worker{ID: capID, UserID: u, Name: "capable", Status: "online", ProtocolCapabilities: []string{capability.RecoveryArchiveV1, jobCap, jobFilesCap}}
	pl, err := e.svc.Claim(e.ctx, capable, nil)
	if err != nil || pl == nil || pl.RunID != v.ID.String() {
		t.Fatalf("Claim = %+v, %v", pl, err)
	}
	requireNoDefaultEffortKey(t, "job", pl.Config)
}

// Preservation (PRD #3602 + #3247): an unpinned Plan/Code checker of either family is
// delivered and recorded at high / cross-check default whatever the ordinary preference
// holds, and an explicit pin still wins.
func TestCheckerEffortIndependentOfOrdinaryPreferenceLiveDB(t *testing.T) {
	type pref struct {
		name          string
		claude, codex any
	}
	prefs := []pref{{"runtime", "runtime", "runtime"}, {"null", nil, nil}, {"low", "low", "low"}}
	pins := []struct {
		name   string
		effort any
		want   string
		source string
	}{{"unpinned", nil, "high", "cross-check default"}, {"pin-low", "low", "low", "pin"}}

	setPrefs := func(env codexTestEnv, userID uuid.UUID, p pref) {
		env.exec(`UPDATE users SET default_effort=$2,default_codex_effort=$3 WHERE id=$1`, userID, p.claude, p.codex)
	}
	check := func(t *testing.T, p *ClaimPayload, recEffort, recSource string, want, source string) {
		t.Helper()
		if p == nil || p.Config.DefaultEffort == nil || p.CrossCheck == nil || p.CrossCheck.EffortSource == nil {
			t.Fatal("checker claim lacks effort or provenance")
		}
		if *p.Config.DefaultEffort != want || *p.CrossCheck.EffortSource != source {
			t.Fatalf("delivered %s/%s, want %s/%s", *p.Config.DefaultEffort, *p.CrossCheck.EffortSource, want, source)
		}
		if recEffort != want || recSource != source {
			t.Fatalf("recorded %s/%s, want %s/%s", recEffort, recSource, want, source)
		}
	}
	for _, pr := range prefs {
		for _, pin := range pins {
			name := pr.name + "/" + pin.name
			t.Run("plan-claude/"+name, func(t *testing.T) {
				fx := newClaudeCheckerFix(t, BindModePinned)
				fx.env.exec(`UPDATE workers SET protocol_capabilities=$2 WHERE id=$1`, fx.workerID,
					[]string{capability.CrossCheckV1, capability.CrossCheckCodexLeadV1, capability.CrossCheckPinsV1})
				setPrefs(fx.env, fx.userID, pr)
				if pin.effort != nil {
					fx.env.exec(`INSERT INTO user_cross_check_pins (user_id,stage,harness,effort) VALUES ($1,'plan','claude',$2)`, fx.userID, pin.effort)
				}
				p := fx.claim(t)
				var eff, src string
				if err := fx.env.pool.QueryRow(fx.env.ctx, `SELECT checker_effort,checker_effort_source FROM cross_checks WHERE lead_run_id=$1`, fx.lead).Scan(&eff, &src); err != nil {
					t.Fatal(err)
				}
				check(t, p, eff, src, pin.want, pin.source)
			})
			t.Run("plan-codex/"+name, func(t *testing.T) {
				f := newCrossCheckContentionFixture(t)
				f.env.exec(`UPDATE workers SET protocol_capabilities=$2 WHERE id=$1`, f.workerID, checkerPinCaps(true, true))
				setPrefs(f.env, f.userID, pr)
				if pin.effort != nil {
					setCheckerPin(f, "codex", nil, pin.effort)
				}
				p := f.claim(t, false, "", uuid.Nil)
				cc, err := f.env.q.GetPlanCrossCheck(f.env.ctx, f.lead)
				if err != nil {
					t.Fatal(err)
				}
				check(t, p, cc.CheckerEffort.String, cc.CheckerEffortSource.String, pin.want, pin.source)
			})
			t.Run("code-claude/"+name, func(t *testing.T) {
				f, _ := newClaudeCodeCheckerFix(t)
				setPrefs(f.env, f.userID, pr)
				if pin.effort != nil {
					f.env.exec(`INSERT INTO user_cross_check_pins(user_id,stage,harness,effort) VALUES($1,'code','claude',$2)`, f.userID, pin.effort)
				}
				p := laneClaim(t, f, "cross_check", nil)
				cc, err := f.env.q.GetCodeCrossCheck(f.env.ctx, f.lead)
				if err != nil {
					t.Fatal(err)
				}
				check(t, p, cc.CheckerEffort.String, cc.CheckerEffortSource.String, pin.want, pin.source)
			})
			t.Run("code-codex/"+name, func(t *testing.T) {
				f := codeFixture(t)
				f.env.exec("UPDATE workers SET protocol_capabilities=array_append(protocol_capabilities,$2) WHERE id=$1", f.workerID, capability.CrossCheckPinsV1)
				setPrefs(f.env, f.userID, pr)
				if pin.effort != nil {
					f.env.exec(`INSERT INTO user_cross_check_pins(user_id,stage,harness,effort) VALUES($1,'code','codex',$2)`, f.userID, pin.effort)
				}
				cc, err := f.svc.SubmitCodeCrossCheck(f.env.ctx, wkrRow(t, f.env, f.workerID), f.lead, 1, strings.Repeat("a", 40), strings.Repeat("b", 40))
				if err != nil {
					t.Fatal(err)
				}
				f.runID = uuid.UUID(cc.CheckerRunID.Bytes)
				p := laneClaim(t, f, "cross_check", nil)
				rec, err := f.env.q.GetCodeCrossCheck(f.env.ctx, f.lead)
				if err != nil {
					t.Fatal(err)
				}
				check(t, p, rec.CheckerEffort.String, rec.CheckerEffortSource.String, pin.want, pin.source)
			})
		}
	}
}
