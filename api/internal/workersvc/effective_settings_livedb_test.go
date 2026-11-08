package workersvc

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/agenttmpl"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// effective_settings_livedb_test.go is the claim-equivalence contract of PredictHarness and
// PredictLead (PRD #3254): for each case the prediction is compared with the literal values the
// case expects AND with what creation and claim assembly actually produced for the same user, so a
// prediction that drifts from the claim, or a claim that drifts from the table, reddens. Skipped
// unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; run via ./e2e/run-store-it.sh.

type effFix struct {
	*poolFix
	workerID uuid.UUID
	token    uuid.UUID
	codex    poolAccount
}

func newEffFix(t *testing.T) *effFix {
	t.Helper()
	pf := newPoolFix(t)
	svc := New(pf.env.q, pf.env.box, testParams())
	svc.SetVault(pf.env.vlt)
	svc.txBeginner = pf.env.pool
	svc.SetForges(originDriftForges{f: &originDriftForge{}})
	svc.p.Autoselect = pf.svc.p.Autoselect
	svc.p.Autoselect.MaxStaleness = 15 * time.Minute
	svc.p.CodexUsagePollInterval = pf.svc.p.CodexUsagePollInterval
	svc.p.UsagePollInterval = 5 * time.Minute
	pf.svc = svc
	resealBotPAT(t, pf.env, pf.userID)
	fx := &effFix{poolFix: pf}
	if err := pf.env.pool.QueryRow(pf.env.ctx, `SELECT id FROM workers WHERE user_id = $1`, pf.userID).Scan(&fx.workerID); err != nil {
		t.Fatalf("read worker: %v", err)
	}
	// Seed the builtin roster first so the shared namespace is identical in every
	// test order, then switch all of it off for this user. A user template whose
	// name exists in the shared namespace is excluded by the prediction and claim
	// queries, so fixtures use names no builtin carries (see template).
	if _, err := store.ReconcileBuiltinTemplates(pf.env.ctx, pf.env.q); err != nil {
		t.Fatalf("reconcile builtins: %v", err)
	}
	// The shared lead roster must not decide: every shared template is off for this user.
	pf.env.exec(`INSERT INTO agent_template_allocations(template_id,user_id,enabled)
		SELECT id,$1,false FROM agent_templates
		ON CONFLICT (template_id,(COALESCE(user_id,'00000000-0000-0000-0000-000000000000'::uuid)))
		DO UPDATE SET enabled=false`, pf.userID)
	return fx
}

// anthropic gives the user an enabled default Anthropic token.
func (fx *effFix) anthropic() {
	fx.t.Helper()
	sealed, err := fx.env.box.Seal([]byte(codexToken("anthropic")))
	if err != nil {
		fx.t.Fatalf("seal anthropic token: %v", err)
	}
	fx.token = uuid.New()
	fx.env.exec(`INSERT INTO user_secrets (id, user_id, kind, label, is_default, ciphertext, sealed_with)
		VALUES ($1, $2, 'anthropic_token', $3, true, $4, 'master')`, fx.token, fx.userID, "eff-claude-"+fx.token.String()[:8], sealed)
}

// codexDefault gives the user a linked default Codex subscription.
func (fx *effFix) codexDefault() {
	fx.t.Helper()
	fx.codex = fx.newAccount("eff-codex")
	fx.makeDefault(fx.codex.alias)
}

func (fx *effFix) lane(column, value string) {
	fx.t.Helper()
	fx.env.exec(`UPDATE users SET `+column+` = NULLIF($2,'') WHERE id = $1`, fx.userID, value)
}

// effLeadName is lead-shaped (agenttmpl.IsLeadName) but not a builtin name, so
// the fixture's user template is not shadowed by the shared builtin "lead".
const effLeadName = "orchestrator"

func (fx *effFix) template(name, claudePin, codexPin string) {
	fx.t.Helper()
	var shared int
	if err := fx.env.pool.QueryRow(fx.env.ctx, `SELECT count(*) FROM agent_templates WHERE scope <> 'user' AND name = $1`, name).Scan(&shared); err != nil {
		fx.t.Fatalf("shared name probe: %v", err)
	}
	if shared != 0 {
		fx.t.Fatalf("fixture template %q collides with a shared template: the resolver and claim path exclude a user template whose name exists in the shared namespace", name)
	}
	id := uuid.New()
	fx.env.exec(`INSERT INTO agent_templates(id,name,description,prompt_body,scope,user_id,model,codex_model)
		VALUES($1,$2,'d','b','user',$3,NULLIF($4,''),NULLIF($5,''))`, id, name, fx.userID, claudePin, codexPin)
	fx.env.exec(`INSERT INTO agent_template_allocations(template_id,user_id,enabled) VALUES($1,$2,true)`, id, fx.userID)
}

func (fx *effFix) create(explicit *Harness, override *RawCredentialOverride) (store.Run, error) {
	fx.t.Helper()
	fx.iid++
	seedEligibleIssue(fx.t, fx.env, fx.repoID, fx.iid)
	return fx.svc.CreateRun(fx.env.ctx, fx.userID, fx.repoID, fx.iid, "eff", nil, nil, false, nil, nil, explicit, override)
}

func (fx *effFix) mustCreate(explicit *Harness) store.Run {
	fx.t.Helper()
	run, err := fx.create(explicit, nil)
	if err != nil {
		fx.t.Fatalf("create: %v", err)
	}
	return mustRun(fx.t, fx.env, run.ID)
}

// claim assembles the claim a worker would receive for run.
func (fx *effFix) claim(run store.Run) *ClaimPayload {
	fx.t.Helper()
	fx.env.exec(`UPDATE runs SET status = 'claimed', worker_id = $2, claim_generation = 1 WHERE id = $1`, run.ID, fx.workerID)
	w := store.Worker{ID: fx.workerID, UserID: fx.userID, ProtocolCapabilities: []string{
		capability.CodexHarnessV1, capability.CodexRuntimeV2, capability.CodexCustomModelV1,
	}}
	payload, err := fx.svc.assembleClaim(fx.env.ctx, w, mustRun(fx.t, fx.env, run.ID))
	if err != nil {
		fx.t.Fatalf("assembleClaim: %v", err)
	}
	return payload
}

func (fx *effFix) predictHarness(explicit *Harness) (HarnessPrediction, error) {
	fx.t.Helper()
	return fx.svc.PredictHarness(fx.env.ctx, fx.userID, explicit, IssueStartOrigin())
}

func (fx *effFix) predictLead(h Harness, review bool) LeadPrediction {
	fx.t.Helper()
	got, err := fx.svc.PredictLead(fx.env.ctx, fx.userID, h, nil, false, review)
	if err != nil {
		fx.t.Fatalf("PredictLead: %v", err)
	}
	return got
}

// leadWant is one case's literal expectation. claimModel and effort are "" when the claim ships
// none; model is "" when the SDK decides.
type leadWant struct {
	harness                 Harness
	model, modelSource      string
	claimModel, claimSource string
	effort, effortSource    string
}

func (w leadWant) check(t *testing.T, lead LeadPrediction, payload *ClaimPayload) {
	t.Helper()
	if lead.Model != w.model || lead.ModelSource != w.modelSource {
		t.Errorf("predicted model = %q (%s), want %q (%s)", lead.Model, lead.ModelSource, w.model, w.modelSource)
	}
	if got := derefOr(lead.ClaimModel, ""); got != w.claimModel || lead.ClaimModelSource != w.claimSource {
		t.Errorf("predicted claim model = %q (%s), want %q (%s)", got, lead.ClaimModelSource, w.claimModel, w.claimSource)
	}
	if got := derefOr(lead.Effort, ""); got != w.effort || lead.EffortSource != w.effortSource {
		t.Errorf("predicted effort = %q (%s), want %q (%s)", got, lead.EffortSource, w.effort, w.effortSource)
	}
	// And the claim itself ships exactly the predicted claim-level values.
	if got := derefOr(payload.Config.DefaultModel, ""); got != w.claimModel {
		t.Errorf("claim default_model = %q, want %q", got, w.claimModel)
	}
	if got := derefOr(payload.Config.DefaultModelSource, ""); got != w.claimSource {
		t.Errorf("claim default_model_source = %q, want %q", got, w.claimSource)
	}
	if got := derefOr(payload.Config.DefaultEffort, ""); got != w.effort {
		t.Errorf("claim default_effort = %q, want %q", got, w.effort)
	}
	if got := derefOr(payload.Config.DefaultEffortSource, ""); got != w.effortSource {
		t.Errorf("claim default_effort_source = %q, want %q", got, w.effortSource)
	}
	// A template-sourced lead model is delivered on the lead agent of the claim roster.
	if w.modelSource == "template" {
		var names []string
		for _, ag := range payload.Agents {
			names = append(names, ag.Name)
		}
		idx, ok := agenttmpl.SelectLead(names)
		if !ok {
			t.Fatalf("claim roster %v carries no lead-shaped agent", names)
		}
		if got := derefOr(payload.Agents[idx].Model, ""); got != w.model {
			t.Errorf("claim lead agent %q model = %q, want the predicted %q", payload.Agents[idx].Name, got, w.model)
		}
	}
}

// TestEffectiveSettingsLeadClaimEquivalenceLiveDB walks the lead rows: harness frozen at create,
// then the model and effort the claim ships, against the prediction and the literal table.
func TestEffectiveSettingsLeadClaimEquivalenceLiveDB(t *testing.T) {
	codex := HarnessCodex
	claude := HarnessClaude
	cases := []struct {
		name     string
		setup    func(fx *effFix)
		explicit *Harness
		want     leadWant
	}{
		{
			name:  "claude only, nothing configured",
			setup: func(fx *effFix) { fx.anthropic() },
			want:  leadWant{harness: HarnessClaude, model: "", modelSource: "builtin", effort: "medium", effortSource: "builtin"},
		},
		{
			name:  "claude only with a lead template",
			setup: func(fx *effFix) { fx.anthropic(); fx.template(effLeadName, "opus", "") },
			want:  leadWant{harness: HarnessClaude, model: "opus", modelSource: "template", effort: "medium", effortSource: "builtin"},
		},
		{
			name: "claude lane wins over the template and ships",
			setup: func(fx *effFix) {
				fx.anthropic()
				fx.template(effLeadName, "opus", "")
				fx.lane("default_claude_model", "sonnet")
			},
			want: leadWant{harness: HarnessClaude, model: "sonnet", modelSource: "user_default", claimModel: "sonnet", claimSource: "user_default",
				effort: "medium", effortSource: "builtin"},
		},
		{
			name:  "legacy codex pin on the claude column is the SDK default",
			setup: func(fx *effFix) { fx.anthropic(); fx.template(effLeadName, "gpt-6-luna", "") },
			want:  leadWant{harness: HarnessClaude, model: "", modelSource: "builtin", effort: "medium", effortSource: "builtin"},
		},
		{
			name: "claude lane holding a codex role id is dropped",
			setup: func(fx *effFix) {
				fx.anthropic()
				fx.template(effLeadName, "opus", "")
				fx.lane("default_claude_model", "gpt-6-luna")
			},
			want: leadWant{harness: HarnessClaude, model: "opus", modelSource: "template", effort: "medium", effortSource: "builtin"},
		},
		{
			name:  "claude explicit effort",
			setup: func(fx *effFix) { fx.anthropic(); fx.lane("default_effort", "high") },
			want:  leadWant{harness: HarnessClaude, model: "", modelSource: "builtin", effort: "high", effortSource: "user_default"},
		},
		{
			name:  "codex only, nothing configured",
			setup: func(fx *effFix) { fx.codexDefault() },
			want:  leadWant{harness: HarnessCodex, model: agenttmpl.DefaultCodexModel, modelSource: "builtin", effort: "medium", effortSource: "builtin"},
		},
		{
			name:  "codex lead pin",
			setup: func(fx *effFix) { fx.codexDefault(); fx.template(effLeadName, "", "gpt-6-sol") },
			want:  leadWant{harness: HarnessCodex, model: "gpt-6-sol", modelSource: "template", effort: "medium", effortSource: "builtin"},
		},
		{
			name: "codex lane",
			setup: func(fx *effFix) {
				fx.codexDefault()
				fx.template(effLeadName, "", "gpt-6-astra")
				fx.lane("default_codex_model", "gpt-6-sol")
			},
			want: leadWant{harness: HarnessCodex, model: "gpt-6-sol", modelSource: "user_default", claimModel: "gpt-6-sol", claimSource: "user_default",
				effort: "medium", effortSource: "builtin"},
		},
		{
			name:  "codex runtime default effort ships none",
			setup: func(fx *effFix) { fx.codexDefault(); fx.lane("default_codex_effort", "runtime") },
			want:  leadWant{harness: HarnessCodex, model: agenttmpl.DefaultCodexModel, modelSource: "builtin", effort: "", effortSource: "user_default"},
		},
		{
			name:     "explicit codex with both usable",
			setup:    func(fx *effFix) { fx.anthropic(); fx.codexDefault() },
			explicit: &codex,
			want:     leadWant{harness: HarnessCodex, model: agenttmpl.DefaultCodexModel, modelSource: "builtin", effort: "medium", effortSource: "builtin"},
		},
		{
			name:     "explicit claude with both usable and a codex lane set",
			setup:    func(fx *effFix) { fx.anthropic(); fx.codexDefault(); fx.lane("default_codex_model", "gpt-6-sol") },
			explicit: &claude,
			want:     leadWant{harness: HarnessClaude, model: "", modelSource: "builtin", effort: "medium", effortSource: "builtin"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fx := newEffFix(t)
			c.setup(fx)
			pred, err := fx.predictHarness(c.explicit)
			if err != nil {
				t.Fatal(err)
			}
			if pred.ChosenAtCreate || pred.Harness != c.want.harness {
				t.Fatalf("predicted harness = %+v, want exact %s", pred, c.want.harness)
			}
			run := fx.mustCreate(c.explicit)
			if run.Harness != string(c.want.harness) {
				t.Fatalf("created run harness = %q, want %q", run.Harness, c.want.harness)
			}
			lead := fx.predictLead(Harness(run.Harness), false)
			c.want.check(t, lead, fx.claim(run))
		})
	}
}

// TestEffectiveSettingsSubagentPinsMatchClaimLiveDB compares the predicted own pins with the
// roster the claim delivers, on both harnesses.
func TestEffectiveSettingsSubagentPinsMatchClaimLiveDB(t *testing.T) {
	for _, h := range []Harness{HarnessClaude, HarnessCodex} {
		t.Run(string(h), func(t *testing.T) {
			fx := newEffFix(t)
			fx.anthropic()
			fx.codexDefault()
			fx.template(effLeadName, "opus", "gpt-6-sol")
			fx.template("eff-coder", "sonnet", "gpt-6-astra")
			fx.template("eff-tester", "", "")
			fx.template("scribe", "haiku", "")
			run := fx.mustCreate(&h)
			lead := fx.predictLead(h, false)
			payload := fx.claim(run)

			want := map[string]string{}
			var names []string
			for _, a := range payload.Agents {
				names = append(names, a.Name)
			}
			leadIdx, ok := agenttmpl.SelectLead(names)
			if !ok {
				t.Fatalf("claim roster %v carries no lead-shaped agent", names)
			}
			wantLead := map[Harness]string{HarnessClaude: "opus", HarnessCodex: "gpt-6-sol"}[h]
			if got := derefOr(payload.Agents[leadIdx].Model, ""); got != wantLead {
				t.Errorf("claim lead agent %q model = %q, want %q", payload.Agents[leadIdx].Name, got, wantLead)
			}
			if lead.Model != wantLead || lead.Subagents == nil || lead.Subagents.LeadValue != wantLead {
				t.Errorf("predicted lead model %q (subagents lead value %+v), want %q as delivered", lead.Model, lead.Subagents, wantLead)
			}
			for i, a := range payload.Agents {
				if i != leadIdx && a.Model != nil {
					want[a.Name] = *a.Model
				}
			}
			got := map[string]string{}
			for _, p := range lead.Subagents.Pinned {
				got[p.Name] = p.Model
			}
			if lead.Subagents.Mode != SubagentModeOwnPins || len(got) != len(want) || len(want) == 0 {
				t.Fatalf("predicted pins %v (%s), claim delivers %v", got, lead.Subagents.Mode, want)
			}
			for name, model := range want {
				if got[name] != model {
					t.Errorf("subagent %s: predicted %q, claim delivers %q", name, got[name], model)
				}
			}
			wantPins := map[Harness]map[string]string{
				HarnessClaude: {"eff-coder": "sonnet", "scribe": "haiku"},
				HarnessCodex:  {"eff-coder": "gpt-6-astra"},
			}[h]
			for name, model := range wantPins {
				if want[name] != model {
					t.Errorf("subagent %s: claim delivers %q, want the template pin %q", name, want[name], model)
				}
			}
		})
	}
}

// TestEffectiveSettingsReviewClaimLiveDB pins the review branch against a real Codex review claim.
func TestEffectiveSettingsReviewClaimLiveDB(t *testing.T) {
	t.Run("codex review claim ships no model, the built-in is predicted", func(t *testing.T) {
		fx := newEffFix(t)
		fx.codexDefault()
		fx.lane("default_codex_model", "custom-codex-preview") // a custom lane must not reach the review
		originalID := seedTaskRun(t, fx.env, fx.userID, fx.repoID, "uzi/task/"+uuid.NewString(), string(HarnessCodex), "completed", true, false)
		fx.svc.maybeEnqueueTaskReview(fx.env.ctx, mustRun(t, fx.env, originalID))
		reviewID, harness := firstTaskRunForReviewTarget(t, fx.env, originalID)
		if harness != string(HarnessCodex) {
			t.Fatalf("review run harness = %q, want codex", harness)
		}
		review := mustRun(t, fx.env, reviewID)
		payload := fx.claim(review)
		if payload.Config.DefaultModel != nil {
			t.Fatalf("a Codex review claim ships default_model %q, want none", *payload.Config.DefaultModel)
		}
		got := fx.predictLead(HarnessCodex, true)
		if got.Model != agenttmpl.CodexTaskReviewModel || got.ModelSource != "builtin" || got.Effort != nil {
			t.Fatalf("review prediction = %+v", got)
		}
	})
}

// ---- harness creation equivalence ----

func (fx *effFix) claudeReading(pct int) {
	fx.t.Helper()
	fx.env.exec(`INSERT INTO anthropic_rate_limits (user_secret_id,user_id,five_hour_pct,seven_day_pct,
            seven_day_resets_at,source,synced_at,enablement_rev)
        SELECT id,user_id,$2,$2,now()+interval '2 days','usage_endpoint',now()-interval '1 minute',enablement_rev
        FROM user_secrets WHERE id=$1
        ON CONFLICT (user_secret_id) DO UPDATE SET five_hour_pct=EXCLUDED.five_hour_pct,
            seven_day_pct=EXCLUDED.seven_day_pct,seven_day_resets_at=EXCLUDED.seven_day_resets_at,
            synced_at=EXCLUDED.synced_at,enablement_rev=EXCLUDED.enablement_rev`, fx.token, pct)
}

func (fx *effFix) quarantineCodex() {
	fx.t.Helper()
	fx.env.exec(`UPDATE codex_provider_account SET coord_state='quarantined' WHERE id=$1`, fx.codex.account)
}

func (fx *effFix) setPolicy(defaultHarness string, overflow bool) {
	fx.t.Helper()
	fx.env.exec(`UPDATE users SET default_harness = NULLIF($2,''), harness_overflow_enabled = $3 WHERE id = $1`, fx.userID, defaultHarness, overflow)
}

// bothUsable is a user with an Anthropic token and a default Codex account, each with a fresh reading.
func (fx *effFix) bothUsable(claudeUsedPct int, codexUtil float64) {
	fx.anthropic()
	fx.codexDefault()
	fx.claudeReading(claudeUsedPct)
	fx.reading(fx.codex, codexUtil)
}

func TestEffectiveSettingsHarnessCreationEquivalenceLiveDB(t *testing.T) {
	// leadsTo checks that a chosen-at-create prediction names the policy, that creation lands on a
	// harness, and that the claim of the created run equals the prediction for THAT harness.
	checkChosen := func(t *testing.T, fx *effFix, policy string, landsOn Harness) {
		t.Helper()
		pred, err := fx.predictHarness(nil)
		if err != nil {
			t.Fatal(err)
		}
		if !pred.ChosenAtCreate || pred.Harness != "" || pred.Policy != policy || pred.Credential != nil {
			t.Fatalf("prediction = %+v, want chosen at create under %q", pred, policy)
		}
		run := fx.mustCreate(nil)
		if run.Harness != string(landsOn) {
			t.Fatalf("created run harness = %q, want %q (decision %v)", run.Harness, landsOn, run.HarnessDecisionReason)
		}
		lead := fx.predictLead(Harness(run.Harness), false)
		payload := fx.claim(run)
		if got := derefOr(payload.Config.DefaultEffort, ""); got != derefOr(lead.Effort, "") {
			t.Errorf("claim default_effort = %q, predicted %q", got, derefOr(lead.Effort, ""))
		}
		if got := derefOr(payload.Config.DefaultModel, ""); got != derefOr(lead.ClaimModel, "") {
			t.Errorf("claim default_model = %q, predicted %q", got, derefOr(lead.ClaimModel, ""))
		}
	}

	t.Run("automatic lands on claude", func(t *testing.T) {
		fx := newEffFix(t)
		fx.bothUsable(10, 95) // claude has headroom, codex is depleted
		checkChosen(t, fx, PredictPolicyAutomaticWeeklyReset, HarnessClaude)
	})
	t.Run("automatic lands on codex", func(t *testing.T) {
		fx := newEffFix(t)
		fx.bothUsable(95, 10) // claude is depleted, codex has headroom
		checkChosen(t, fx, PredictPolicyAutomaticWeeklyReset, HarnessCodex)
	})
	t.Run("overflow lands on codex", func(t *testing.T) {
		fx := newEffFix(t)
		fx.bothUsable(90, 10)
		fx.setPolicy("claude", true)
		checkChosen(t, fx, PredictPolicyOverflow, HarnessCodex)
	})
	t.Run("overflow stays on claude", func(t *testing.T) {
		fx := newEffFix(t)
		fx.bothUsable(0, 10)
		fx.setPolicy("claude", true)
		checkChosen(t, fx, PredictPolicyOverflow, HarnessClaude)
	})
	t.Run("a stored default with overflow off is exact", func(t *testing.T) {
		fx := newEffFix(t)
		fx.bothUsable(90, 10)
		fx.setPolicy("codex", false)
		pred, err := fx.predictHarness(nil)
		if err != nil || pred.ChosenAtCreate || pred.Harness != HarnessCodex || pred.Rule != PredictRuleUserDefault {
			t.Fatalf("prediction = %+v, %v", pred, err)
		}
		if run := fx.mustCreate(nil); run.Harness != "codex" {
			t.Fatalf("created harness = %q", run.Harness)
		}
	})
	t.Run("an explicit harness is exact even with overflow on", func(t *testing.T) {
		fx := newEffFix(t)
		fx.bothUsable(90, 10)
		fx.setPolicy("claude", true)
		claude := HarnessClaude
		pred, err := fx.predictHarness(&claude)
		if err != nil || pred.ChosenAtCreate || pred.Harness != HarnessClaude {
			t.Fatalf("prediction = %+v, %v", pred, err)
		}
		if run := fx.mustCreate(&claude); run.Harness != "claude" {
			t.Fatalf("created harness = %q, want the explicit claude despite the overflow evidence", run.Harness)
		}
	})
	t.Run("a default whose harness is unusable falls back with the note", func(t *testing.T) {
		fx := newEffFix(t)
		fx.anthropic()
		fx.setPolicy("codex", false)
		pred, err := fx.predictHarness(nil)
		if err != nil || pred.Harness != HarnessClaude || !pred.DefaultSkipped || pred.ChosenAtCreate {
			t.Fatalf("prediction = %+v, %v", pred, err)
		}
		if run := fx.mustCreate(nil); run.Harness != "claude" {
			t.Fatalf("created harness = %q", run.Harness)
		}
	})
	// Deviation 7: creation excludes overflow for a Claude-preferred run whose Codex default is
	// structurally unavailable, so the preview must be exact (and creation must not fail).
	t.Run("overflow with a structurally unavailable codex default is exact", func(t *testing.T) {
		fx := newEffFix(t)
		fx.bothUsable(90, 10)
		fx.setPolicy("claude", true)
		fx.quarantineCodex()
		pred, err := fx.predictHarness(nil)
		if err != nil || pred.ChosenAtCreate || pred.Harness != HarnessClaude {
			t.Fatalf("prediction = %+v, %v; want the exact claude harness", pred, err)
		}
		run := fx.mustCreate(nil)
		if run.Harness != "claude" || run.HarnessDecisionReason.String != "excluded" {
			t.Fatalf("created harness=%q reason=%v, want claude excluded", run.Harness, run.HarnessDecisionReason)
		}
	})
}

// TestEffectiveSettingsRefusalsEqualCreateLiveDB compares the sentinel the prediction returns with
// the one creation returns.
func TestEffectiveSettingsRefusalsEqualCreateLiveDB(t *testing.T) {
	codex := HarnessCodex
	claude := HarnessClaude
	cases := []struct {
		name     string
		setup    func(fx *effFix)
		explicit *Harness
		want     error
		not      error
	}{
		{"explicit codex without a codex credential", func(fx *effFix) { fx.anthropic() }, &codex, ErrNoCredentialForHarness, ErrHarnessCredentialDisabled},
		{"explicit claude without a token", func(fx *effFix) { fx.codexDefault() }, &claude, ErrNoCredentialForHarness, ErrHarnessCredentialDisabled},
		{"explicit codex with every credential disabled", func(fx *effFix) {
			fx.anthropic()
			fx.codexDefault()
			fx.env.exec(`UPDATE user_secrets SET disabled_at = now(), is_default = false WHERE user_id = $1 AND kind IN ('codex_auth','openai_api_key')`, fx.userID)
		}, &codex, ErrHarnessCredentialDisabled, nil},
		{"neither harness usable", func(fx *effFix) {}, nil, ErrNoUsableCredential, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fx := newEffFix(t)
			c.setup(fx)
			_, predErr := fx.predictHarness(c.explicit)
			_, createErr := fx.create(c.explicit, nil)
			for label, err := range map[string]error{"prediction": predErr, "create": createErr} {
				if !errors.Is(err, c.want) || (c.not != nil && errors.Is(err, c.not)) {
					t.Errorf("%s error = %v, want %v", label, err, c.want)
				}
			}
		})
	}
}

// TestEffectiveSettingsCodexPoolCredentialLiveDB: in account mode auto the Codex account is picked
// at creation, so the prediction says so and creation binds a pooled alias.
func TestEffectiveSettingsCodexPoolCredentialLiveDB(t *testing.T) {
	fx := newEffFix(t)
	fx.codexDefault()
	fx.pool(fx.codex.alias)
	fx.mode("auto")
	fx.reading(fx.codex, 10)
	codex := HarnessCodex
	pred, err := fx.predictHarness(&codex)
	if err != nil {
		t.Fatal(err)
	}
	want := CredentialPrediction{Kind: CredentialKindCodex, Source: CredentialSourceChosenAtCreate, FixedAt: FixedAtCreate, AuthMode: "subscription"}
	if pred.Credential == nil || *pred.Credential != want {
		t.Fatalf("credential = %+v, want %+v", pred.Credential, want)
	}
	run := fx.mustCreate(&codex)
	if !run.CodexSecretID.Valid || uuid.UUID(run.CodexSecretID.Bytes) != fx.codex.alias {
		t.Fatalf("created run bound %v, want the pooled alias", run.CodexSecretID)
	}
	if got := run.CodexSelectReason; !got.Valid || got.String == string(codexChoiceDefault) {
		t.Fatalf("select reason = %v, want a pool reason", got)
	}
}

// TestEffectiveSettingsOverrideSentinelsEqualCreateLiveDB: the token checks start-preview runs are
// create's own validator, so a disabled, deleted or foreign token is refused with the same
// sentinel by both.
func TestEffectiveSettingsOverrideSentinelsEqualCreateLiveDB(t *testing.T) {
	fx := newEffFix(t)
	fx.anthropic()
	claude := HarnessClaude
	other := newEffFix(t)
	other.anthropic()

	disabled := uuid.New()
	sealed, _ := fx.env.box.Seal([]byte(codexToken("anthropic")))
	fx.env.exec(`INSERT INTO user_secrets (id, user_id, kind, label, ciphertext, sealed_with, disabled_at)
		VALUES ($1,$2,'anthropic_token','eff-disabled',$3,'master',now())`, disabled, fx.userID, sealed)

	cases := []struct {
		name string
		mode string
		id   *uuid.UUID
		want error
	}{
		{"disabled pinned token", "pinned", &disabled, ErrCredentialDisabled},
		{"deleted pinned token", "pinned", effUUID(uuid.New()), ErrCredentialOverrideSecretNotFound},
		{"foreign pinned token", "pinned", &other.token, ErrCredentialOverrideSecretNotFound},
		{"pinned without a token", "pinned", nil, ErrCredentialOverridePinnedNeedsSecret},
		{"owned enabled token", "pinned", &fx.token, nil},
		{"auto", "auto", nil, nil},
		{"default", "default", nil, nil},
		{"inherit", "inherit", nil, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, predErr := fx.svc.ResolveCredentialOverride(fx.env.ctx, fx.userID, "issue", "claude", c.mode, c.id)
			_, createErr := fx.create(&claude, &RawCredentialOverride{Mode: c.mode, SecretID: c.id})
			if !errors.Is(predErr, c.want) || !errors.Is(createErr, c.want) || (c.want == nil && (predErr != nil || createErr != nil)) {
				t.Fatalf("prediction err = %v, create err = %v, want %v", predErr, createErr, c.want)
			}
		})
	}
}

func effUUID(id uuid.UUID) *uuid.UUID { return &id }

// TestEffectiveSettingsJudgeAndSummaryMatchClaimLiveDB compares the judge and summary models the
// effective-settings cells resolve with the fields real claims ship. The summary model rides an
// issue-run claim (summary_model, for the run's harness); the judge model rides only a judge-run
// claim (judge_model). The inputs are the ones handler's TestEffectiveSettingsCellsEqualMySettingsLiveDB
// feeds the cells, and the expected values here are the literals it asserts, so the two tests pin
// the same four resolved models: the Claude judge user value (opus), the Codex judge instance value
// (gpt-6-sol), the Claude summary instance value (sonnet) and the Codex summary user value (gpt-6-luna).
func TestEffectiveSettingsJudgeAndSummaryMatchClaimLiveDB(t *testing.T) {
	codex, claude := HarnessCodex, HarnessClaude
	fx := newEffFix(t)
	fx.anthropic()
	fx.codexDefault()
	fx.svc.SetSettings(fakeSettings{model: "haiku", codexModel: "gpt-6-sol", summaryModel: "sonnet"})
	fx.lane("judge_model", "opus")
	fx.lane("codex_summary_model", "gpt-6-luna")

	for _, c := range []struct {
		harness     Harness
		explicit    *Harness
		wantSummary string
	}{{HarnessClaude, &claude, "sonnet"}, {HarnessCodex, &codex, "gpt-6-luna"}} {
		run := fx.mustCreate(c.explicit)
		if run.Harness != string(c.harness) {
			t.Fatalf("created run harness = %q, want %q", run.Harness, c.harness)
		}
		if got := derefOr(fx.claim(run).SummaryModel, "<nil>"); got != c.wantSummary {
			t.Errorf("%s issue claim summary_model = %q, want %q", c.harness, got, c.wantSummary)
		}
	}

	for i, c := range []struct {
		harness   Harness
		wantJudge string
	}{{HarnessClaude, "opus"}, {HarnessCodex, "gpt-6-sol"}} {
		// One active judge per target, so each judge run reviews its own completed issue run.
		target := uuid.New()
		fx.env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status)
			VALUES ($1, $2, $3, 'issue', $4, 't', 'd', 'completed')`, target, fx.userID, fx.repoID, 900+i)
		id := uuid.New()
		fx.env.exec(`INSERT INTO runs (id, user_id, kind, issue_title, issue_description, status, target_run_id, harness)
			VALUES ($1, $2, 'judge', 't', 'd', 'queued', $3, $4)`, id, fx.userID, target, string(c.harness))
		if c.harness == HarnessCodex {
			if err := fx.svc.FreezeCodexBinding(fx.env.ctx, fx.userID, id, fx.codex.alias, codexAuthModeSubscription); err != nil {
				t.Fatalf("FreezeCodexBinding: %v", err)
			}
		}
		fx.env.exec(`UPDATE runs SET status = 'claimed', worker_id = $2, claim_generation = 1 WHERE id = $1`, id, fx.workerID)
		payload, err := fx.svc.assembleJudgeClaim(fx.env.ctx, store.Worker{ID: fx.workerID, UserID: fx.userID}, mustRun(t, fx.env, id))
		if err != nil {
			t.Fatalf("assembleJudgeClaim %s: %v", c.harness, err)
		}
		if got := derefOr(payload.JudgeModel, "<nil>"); got != c.wantJudge {
			t.Errorf("%s judge claim judge_model = %q, want %q", c.harness, got, c.wantJudge)
		}
	}
}
