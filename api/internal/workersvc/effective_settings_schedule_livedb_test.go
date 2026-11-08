package workersvc

import (
	"testing"

	"github.com/vtmocanu/uzi/api/internal/agenttmpl"
)

// effective_settings_schedule_livedb_test.go pins the schedule half of PredictLead (PRD #3254): a
// schedule's model pin is frozen onto the run at fire (CreateScheduledRun) and its subagent toggle
// rides the run, so the claim the worker receives must ship exactly what PredictLead predicts for
// the same pin and toggle. The handler test next to the route compares the route's whole body with
// a real save and a real fire; this one compares the lead values with the real claim. Skipped
// unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; run via ./e2e/run-store-it.sh.

func TestEffectiveSettingsSchedulePinClaimEquivalenceLiveDB(t *testing.T) {
	codex, claude := HarnessCodex, HarnessClaude
	for _, c := range []struct {
		name     string
		explicit *Harness
		pin      string
		toggle   bool
		want     leadWant
		ignored  bool
	}{
		{"claude pin on claude", &claude, "sonnet", false,
			leadWant{harness: HarnessClaude, model: "sonnet", modelSource: "schedule_pin", claimModel: "sonnet", claimSource: "schedule_pin", effort: "medium", effortSource: "builtin"}, false},
		{"claude pin on codex is ignored for the lane", &codex, "sonnet", true,
			leadWant{harness: HarnessCodex, model: "gpt-6-sol", modelSource: "user_default", claimModel: "gpt-6-sol", claimSource: "user_default", effort: "medium", effortSource: "builtin"}, true},
		{"custom pin on codex is ignored for the lane", &codex, "my-custom-model", false,
			leadWant{harness: HarnessCodex, model: "gpt-6-sol", modelSource: "user_default", claimModel: "gpt-6-sol", claimSource: "user_default", effort: "medium", effortSource: "builtin"}, true},
		{"codex pin on codex", &codex, "gpt-6-astra", true,
			leadWant{harness: HarnessCodex, model: "gpt-6-astra", modelSource: "schedule_pin", claimModel: "gpt-6-astra", claimSource: "schedule_pin", effort: "medium", effortSource: "builtin"}, false},
		{"codex role pin on claude is ignored", &claude, "gpt-6-luna", false,
			leadWant{harness: HarnessClaude, model: "opus", modelSource: "template", effort: "medium", effortSource: "builtin"}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			fx := newEffFix(t)
			fx.anthropic()
			fx.codexDefault()
			fx.template(effLeadName, "opus", "gpt-6-astra")
			fx.lane("default_codex_model", "gpt-6-sol")
			fx.iid++
			seedEligibleIssue(t, fx.env, fx.repoID, fx.iid)
			pin := c.pin
			created, err := fx.svc.CreateScheduledRun(fx.env.ctx, fx.userID, fx.repoID, fx.iid, "eff", nil, nil, &pin, c.toggle, nil, nil, c.explicit)
			if err != nil {
				t.Fatalf("CreateScheduledRun: %v", err)
			}
			run := mustRun(t, fx.env, created.ID)
			if run.Harness != string(c.want.harness) {
				t.Fatalf("fired run harness = %q, want %q", run.Harness, c.want.harness)
			}
			lead, err := fx.svc.PredictLead(fx.env.ctx, fx.userID, Harness(run.Harness), &pin, c.toggle, false)
			if err != nil {
				t.Fatal(err)
			}
			payload := fx.claim(run)
			c.want.check(t, lead, payload)
			if c.ignored != (lead.DroppedRunModel != nil) {
				t.Errorf("predicted ignored pin = %v, want %v", lead.DroppedRunModel, c.ignored)
			}
			if payload.Config.OverrideSubagentModel != c.toggle {
				t.Errorf("claim override_subagent_model = %v, want the schedule's %v", payload.Config.OverrideSubagentModel, c.toggle)
			}
		})
	}
}

// TestEffectiveSettingsScheduleSubagentModesLiveDB: the toggle off leaves each subagent its own pin,
// which the claim delivers; on, the claim delivers the same roster plus the flag the executors apply
// (Codex clears every pin; Claude applies it only with a lead model), as PredictLead reports.
func TestEffectiveSettingsScheduleSubagentModesLiveDB(t *testing.T) {
	for _, h := range []Harness{HarnessClaude, HarnessCodex} {
		for _, toggle := range []bool{false, true} {
			name := string(h) + " toggle off"
			if toggle {
				name = string(h) + " toggle on"
			}
			t.Run(name, func(t *testing.T) {
				fx := newEffFix(t)
				fx.anthropic()
				fx.codexDefault()
				fx.template(effLeadName, "opus", "gpt-6-sol")
				fx.template("eff-coder", "sonnet", "gpt-6-astra")
				fx.template("eff-tester", "", "")
				fx.iid++
				seedEligibleIssue(t, fx.env, fx.repoID, fx.iid)
				created, err := fx.svc.CreateScheduledRun(fx.env.ctx, fx.userID, fx.repoID, fx.iid, "eff", nil, nil, nil, toggle, nil, nil, &h)
				if err != nil {
					t.Fatalf("CreateScheduledRun: %v", err)
				}
				run := mustRun(t, fx.env, created.ID)
				lead, err := fx.svc.PredictLead(fx.env.ctx, fx.userID, h, nil, toggle, false)
				if err != nil {
					t.Fatal(err)
				}
				payload := fx.claim(run)
				if payload.Config.OverrideSubagentModel != toggle {
					t.Fatalf("claim override_subagent_model = %v, want %v", payload.Config.OverrideSubagentModel, toggle)
				}
				if toggle {
					if lead.Subagents.Mode != SubagentModeFollowLead || lead.Subagents.LeadValue != claimedLeadModel(payload) {
						t.Errorf("predicted subagents = %+v, want every subagent on the lead", lead.Subagents)
					}
					return
				}
				names := make([]string, 0, len(payload.Agents))
				for _, a := range payload.Agents {
					names = append(names, a.Name)
				}
				leadIdx, _ := agenttmpl.SelectLead(names)
				want := map[string]string{}
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
			})
		}
	}
}

// claimedLeadModel is the model the claim gives the lead: the shipped default_model, else the lead
// agent's own pin for the run's harness.
func claimedLeadModel(p *ClaimPayload) string {
	if p.Config.DefaultModel != nil {
		return *p.Config.DefaultModel
	}
	names := make([]string, 0, len(p.Agents))
	for _, a := range p.Agents {
		names = append(names, a.Name)
	}
	if i, ok := agenttmpl.SelectLead(names); ok && p.Agents[i].Model != nil {
		return *p.Agents[i].Model
	}
	return ""
}
