package workersvc

import (
	"errors"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/agenttmpl"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// effective_settings_run_override_livedb_test.go extends the claim-equivalence contract of
// effective_settings_livedb_test.go to the start-time model and effort (PRD #3250 section 8): what
// PredictRunOverrideLead promises equals what the created run's claim ships, a model the claim would
// park is refused by the prediction and never replaced, and a model choice leaves the creation-time
// harness policies out (D22). Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres;
// run via ./e2e/run-store-it.sh.

func (fx *effFix) createWithStartOptions(explicit *Harness, model, effort *string) (store.Run, error) {
	fx.t.Helper()
	fx.iid++
	seedEligibleIssue(fx.t, fx.env, fx.repoID, fx.iid)
	run, err := fx.svc.CreateRun(fx.env.ctx, fx.userID, fx.repoID, fx.iid, "eff", nil, nil, false, nil, nil, explicit, nil, model, effort)
	if err != nil {
		return store.Run{}, err
	}
	return mustRun(fx.t, fx.env, run.ID), nil
}

func TestEffectiveSettingsRunOverrideClaimEquivalenceLiveDB(t *testing.T) {
	codex := HarnessCodex
	cases := []struct {
		name          string
		setup         func(fx *effFix)
		explicit      *Harness
		model, effort *string
		want          leadWant
	}{
		{
			name:  "claude model and effort",
			setup: func(fx *effFix) { fx.anthropic() },
			model: strp("sonnet"), effort: strp("xhigh"),
			want: leadWant{harness: HarnessClaude, model: "sonnet", modelSource: "run_override", claimModel: "sonnet", claimSource: "run_override",
				effort: "xhigh", effortSource: "run_override"},
		},
		{
			name:     "codex model and effort",
			setup:    func(fx *effFix) { fx.anthropic(); fx.codexDefault() },
			explicit: &codex,
			model:    strp("gpt-6-sol"), effort: strp("max"),
			want: leadWant{harness: HarnessCodex, model: "gpt-6-sol", modelSource: "run_override", claimModel: "gpt-6-sol", claimSource: "run_override",
				effort: "max", effortSource: "run_override"},
		},
		{
			name: "a run model beats the owner's lane",
			setup: func(fx *effFix) {
				fx.anthropic()
				fx.lane("default_claude_model", "haiku")
				fx.lane("default_effort", "low")
			},
			model: strp("opus"), effort: strp("high"),
			want: leadWant{harness: HarnessClaude, model: "opus", modelSource: "run_override", claimModel: "opus", claimSource: "run_override",
				effort: "high", effortSource: "run_override"},
		},
		{
			name:   "claude effort only keeps the template model",
			setup:  func(fx *effFix) { fx.anthropic(); fx.template(effLeadName, "opus", "") },
			effort: strp("low"),
			want:   leadWant{harness: HarnessClaude, model: "opus", modelSource: "template", effort: "low", effortSource: "run_override"},
		},
		{
			// PRD #3252: a frozen run model wins in both phases; the planning effort still applies to plan.
			name: "a run model is the plan model too, the planning effort still splits",
			setup: func(fx *effFix) {
				fx.anthropic()
				fx.lane("planning_claude_model", "haiku")
				fx.lane("planning_claude_effort", "high")
			},
			model: strp("sonnet"),
			want: leadWant{harness: HarnessClaude, model: "sonnet", modelSource: "run_override", claimModel: "sonnet", claimSource: "run_override",
				effort: "medium", effortSource: "builtin",
				plan: &ClaimPhaseSettings{Model: strp("sonnet"), Effort: strp("high"),
					ModelSource: "run_override", EffortSource: "user_planning_default"}},
		},
		{
			name: "a run effort is the plan effort too, the planning model still splits",
			setup: func(fx *effFix) {
				fx.anthropic()
				fx.lane("planning_claude_model", "opus")
				fx.lane("planning_claude_effort", "max")
			},
			effort: strp("low"),
			want: leadWant{harness: HarnessClaude, model: "", modelSource: "builtin", effort: "low", effortSource: "run_override",
				plan: &ClaimPhaseSettings{Model: strp("opus"), Effort: strp("low"),
					ModelSource: "user_planning_default", EffortSource: "run_override"}},
		},
		{
			name: "a run model and effort leave nothing for the planning lane",
			setup: func(fx *effFix) {
				fx.anthropic()
				fx.lane("planning_claude_model", "haiku")
				fx.lane("planning_claude_effort", "high")
			},
			model: strp("sonnet"), effort: strp("xhigh"),
			want: leadWant{harness: HarnessClaude, model: "sonnet", modelSource: "run_override", claimModel: "sonnet", claimSource: "run_override",
				effort: "xhigh", effortSource: "run_override"},
		},
		{
			name:     "codex effort only keeps the built-in model",
			setup:    func(fx *effFix) { fx.codexDefault() },
			explicit: &codex,
			effort:   strp("medium"),
			want:     leadWant{harness: HarnessCodex, model: agenttmpl.DefaultCodexModel, modelSource: "builtin", effort: "medium", effortSource: "run_override"},
		},
		{
			name:     "an effort beats the owner's runtime-default choice",
			setup:    func(fx *effFix) { fx.codexDefault(); fx.lane("default_codex_effort", "runtime") },
			explicit: &codex,
			effort:   strp("high"),
			want:     leadWant{harness: HarnessCodex, model: agenttmpl.DefaultCodexModel, modelSource: "builtin", effort: "high", effortSource: "run_override"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fx := newEffFix(t)
			c.setup(fx)
			run, err := fx.createWithStartOptions(c.explicit, c.model, c.effort)
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			if run.Harness != string(c.want.harness) {
				t.Fatalf("created run harness = %q, want %q", run.Harness, c.want.harness)
			}
			lead, err := fx.svc.PredictRunOverrideLead(fx.env.ctx, fx.userID, Harness(run.Harness), c.model, c.effort)
			if err != nil {
				t.Fatalf("PredictRunOverrideLead: %v", err)
			}
			if lead.Unhonorable != nil {
				t.Fatalf("prediction refused a choice create accepted: %+v", lead.Unhonorable)
			}
			c.want.check(t, lead, fx.claim(run))
		})
	}
}

// TestEffectiveSettingsRunOverrideUnhonorableIsNeverSubstitutedLiveDB: a run_override model the
// harness cannot run is refused by the prediction exactly where the claim parks the run, and neither
// swaps in the lane, template or built-in.
func TestEffectiveSettingsRunOverrideUnhonorableIsNeverSubstitutedLiveDB(t *testing.T) {
	codex := HarnessCodex
	for _, tc := range []struct {
		name     string
		explicit *Harness
		model    string
	}{
		{"claude run holding a codex role id", nil, "gpt-6-sol"},
		{"codex run holding a claude alias", &codex, "opus"},
		{"codex run holding an unknown id", &codex, "gpt-foo"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newEffFix(t)
			fx.anthropic()
			fx.codexDefault()
			fx.lane("default_claude_model", "sonnet")
			fx.lane("default_codex_model", "gpt-6-luna")
			// A run that was honorable when created and is not any more: the row is seeded directly.
			run := fx.mustCreate(tc.explicit)
			setRunStartOptions(fx.env, run.ID, tc.model, "run_override", nil, nil)

			lead, err := fx.svc.PredictRunOverrideLead(fx.env.ctx, fx.userID, Harness(run.Harness), strp(tc.model), nil)
			if err != nil {
				t.Fatalf("PredictRunOverrideLead: %v", err)
			}
			u := lead.Unhonorable
			if u == nil || u.Setting != "model" || u.Value != tc.model || u.Harness != Harness(run.Harness) {
				t.Fatalf("prediction = %+v, want an unhonorable model %q on %s", lead, tc.model, run.Harness)
			}
			if lead.Model != "" || lead.ClaimModel != nil || lead.ModelSource != "" {
				t.Errorf("a refused model was substituted: model=%q claim=%v source=%q", lead.Model, lead.ClaimModel, lead.ModelSource)
			}

			fx.env.exec(`UPDATE runs SET status = 'claimed', worker_id = $2, claim_generation = 1 WHERE id = $1`, run.ID, fx.workerID)
			w := store.Worker{ID: fx.workerID, UserID: fx.userID, ProtocolCapabilities: []string{
				capability.CodexHarnessV1, capability.CodexRuntimeV2, capability.CodexCustomModelV1,
			}}
			if _, err := fx.svc.assembleClaim(fx.env.ctx, w, mustRun(t, fx.env, run.ID)); !errors.Is(err, errRunOverrideUnavailable) {
				t.Fatalf("assembleClaim = %v, want errRunOverrideUnavailable (the claim parks, never substitutes)", err)
			}
		})
	}
}

// TestEffectiveSettingsModelOptsOutOfAutomaticHarnessLiveDB pins D22 (user decision 2026-11-14): a
// model choice makes the creation origin ineligible for the automatic and overflow harness
// policies, so the prediction names exactly the harness create freezes; effort alone leaves the
// policy in force. There is deliberately no case for an undecided harness whose alternatives differ
// in model compatibility: a model request is never undecided, so that case cannot be built.
func TestEffectiveSettingsModelOptsOutOfAutomaticHarnessLiveDB(t *testing.T) {
	fx := newEffFix(t)
	fx.bothUsable(95, 10) // limit readings that choose Codex when the policy is allowed to

	control, err := fx.predictHarness(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !control.ChosenAtCreate || control.Policy != PredictPolicyAutomaticWeeklyReset {
		t.Fatalf("control prediction = %+v, want chosen at create under the automatic policy", control)
	}

	// The premise, under the same readings: a create without a model is steered to Codex by the
	// automatic policy, so the opus create below choosing Claude is the opt-out and not the default.
	noModel, err := fx.createWithStartOptions(nil, nil, nil)
	if err != nil {
		t.Fatalf("create without a model: %v", err)
	}
	if noModel.Harness != string(HarnessCodex) {
		t.Fatalf("create without a model froze %q, want codex under these readings", noModel.Harness)
	}

	model := "opus" // compatible with the D11 harness, Claude
	origin := IssueStartOrigin()
	origin.Model = &model
	pred, err := fx.svc.PredictHarness(fx.env.ctx, fx.userID, nil, origin)
	if err != nil {
		t.Fatal(err)
	}
	if pred.ChosenAtCreate || pred.Harness != HarnessClaude || pred.Credential == nil {
		t.Fatalf("prediction with a model = %+v, want exactly claude", pred)
	}

	created, err := fx.createWithStartOptions(nil, &model, nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if Harness(created.Harness) != pred.Harness || !created.Model.Valid || created.Model.String != model {
		t.Fatalf("create froze %q with model %v, preview promised %q with %q", created.Harness, created.Model, pred.Harness, model)
	}

	// Effort alone is not an opt-out: the create is still steered to Codex.
	effort := "high"
	effortOnly, err := fx.createWithStartOptions(nil, nil, &effort)
	if err != nil {
		t.Fatalf("create with effort only: %v", err)
	}
	if effortOnly.Harness != string(HarnessCodex) {
		t.Fatalf("create with effort only froze %q, want codex", effortOnly.Harness)
	}
}
