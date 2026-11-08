package workersvc

import (
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/agenttmpl"
	"github.com/vtmocanu/uzi/api/internal/leadcfg/leadcfgtest"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
)

// Request cases of fixtures/lead-config/contract.json the current server claim cannot produce,
// each with the reason. A case is either listed here or carries a `server` block; never both.
// Phase never matters to the server today (no planning default is resolved), so the plan-phase
// cases are not listed for their phase.
const (
	reasonEffortlessBuiltin = "config ships default_effort_source builtin without default_effort; the server always ships the builtin level, only the owner's `runtime` choice ships no level (and then the source is user_default)"
	reasonEmptyConfig       = "config is empty; the server always ships default_effort_source on a lead-lane claim"
	reasonEffortlessRoster  = reasonEffortlessBuiltin + "; the roster selection it exercises is the worker's (agenttmpl.SelectLead covers the server side)"
)

var nonProducibleRequestCases = map[string]string{
	"claude-effort-absent":                                 reasonEffortlessBuiltin,
	"codex-effort-absent":                                  reasonEffortlessBuiltin,
	"claude-no-claim-fields":                               reasonEmptyConfig,
	"codex-no-claim-fields-plan-phase":                     reasonEmptyConfig,
	"codex-template-claude-alias-ignored":                  reasonEmptyConfig,
	"codex-template-only-astra":                            reasonEffortlessRoster,
	"codex-template-only-luna-unpinned-subagent":           reasonEffortlessRoster,
	"codex-config-schedule-pin-beats-template":             reasonEffortlessBuiltin,
	"claude-orchestrator-only":                             reasonEffortlessRoster,
	"codex-orchestrator-only":                              reasonEffortlessRoster,
	"claude-first-lead-null-pin-later-orchestrator-pinned": reasonEffortlessRoster,
	"codex-first-orchestrator-null-pin-later-lead-pinned":  reasonEffortlessRoster,
	"codex-template-root-preserves-subagent-pin":           reasonEffortlessRoster + "; override_subagent_model is not a lead-settings field",
	"codex-template-root-overrides-subagent-pin":           reasonEffortlessRoster + "; override_subagent_model is not a lead-settings field",
}

// TestLeadRequestCasesServerPrediction pins the server-side lead prediction to the one request_cases
// matrix the worker readers also iterate. For every case with a `server` block it checks that
//
//   - resolveLeadSettings reproduces the case's claim `config` model and effort fields, and
//   - the model the worker then sends (the claim model, else agenttmpl.ResolveWorkerDefaultModel
//     over the lead template's stored pins; the lane is already folded by resolveLeadSettings)
//     and its source equal the case's `expect`.
//
// The contract's source vocabulary is the claim's own (user_default, schedule_pin) plus the worker's
// template and builtin, which are exactly ResolveWorkerDefaultModel's tokens. The Claude SDK default
// is `expect.model` absent with source builtin; the Codex provider default is providerDefaultModel,
// which must equal agenttmpl.DefaultCodexModel.
func TestLeadRequestCasesServerPrediction(t *testing.T) {
	cases := leadcfgtest.RequestCases(t)
	if len(cases) == 0 {
		t.Fatal("contract has no request cases")
	}
	ids := map[string]bool{}
	withServer := 0
	for _, c := range cases {
		ids[c.ID] = true
		if reason, listed := nonProducibleRequestCases[c.ID]; listed {
			if c.Server != nil {
				t.Errorf("%s is listed as non-producible (%s) but has a server block", c.ID, reason)
			}
			continue
		}
		if c.Server == nil {
			t.Errorf("%s has no server block and is not listed in nonProducibleRequestCases", c.ID)
			continue
		}
		withServer++
		t.Run(c.ID, func(t *testing.T) {
			s := c.Server
			text := func(p *string) pgtype.Text { return pgconv.TextOrNull(derefOr(p, "")) }
			if s.RunModel != nil && *s.RunModel == "" || s.Lane != nil && *s.Lane == "" {
				t.Fatal("a server block must use null, not an empty string, for an unset input")
			}
			harness := Harness(c.Harness)
			got := resolveLeadSettings(LeadSettingsInput{
				Harness: harness, RunModel: text(s.RunModel), LaneModel: text(s.Lane), UserEffort: text(s.UserEffort),
			})

			wantStr := func(key string) string {
				v, _ := c.Config[key].(string)
				return v
			}
			if got.Model.Valid != (wantStr("default_model") != "") || (got.Model.Valid && got.Model.String != wantStr("default_model")) {
				t.Errorf("default_model = %+v, case config has %q", got.Model, wantStr("default_model"))
			}
			if string(got.ModelSource) != wantStr("default_model_source") {
				t.Errorf("default_model_source = %q, case config has %q", got.ModelSource, wantStr("default_model_source"))
			}
			if gotEffort := derefOr(got.Effort, ""); gotEffort != wantStr("default_effort") {
				t.Errorf("default_effort = %q, case config has %q", gotEffort, wantStr("default_effort"))
			}
			if string(got.EffortSource) != wantStr("default_effort_source") {
				t.Errorf("default_effort_source = %q, case config has %q", got.EffortSource, wantStr("default_effort_source"))
			}

			model, source := "", string(got.ModelSource)
			if got.Model.Valid {
				model = got.Model.String
			} else {
				// Lane and schedule pin are already folded into got.Model, so the worker falls to the
				// lead template and then its built-in.
				model, source = agenttmpl.ResolveWorkerDefaultModel(c.Harness, nil, s.TemplateClaude, s.TemplateCodex)
			}
			wantModel := derefOr(c.Expect.Model, "")
			if c.Harness == "codex" && model == agenttmpl.DefaultCodexModel && c.ProviderDefaultModel != nil &&
				*c.ProviderDefaultModel != agenttmpl.DefaultCodexModel {
				t.Errorf("providerDefaultModel %q differs from agenttmpl.DefaultCodexModel %q", *c.ProviderDefaultModel, agenttmpl.DefaultCodexModel)
			}
			if model != wantModel {
				t.Errorf("predicted model = %q, expect.model = %q", model, wantModel)
			}
			if source != c.Expect.ModelSource {
				t.Errorf("predicted model source = %q, expect.modelSource = %q", source, c.Expect.ModelSource)
			}
		})
	}
	if withServer == 0 {
		t.Fatal("no request case carries a server block")
	}
	for id := range nonProducibleRequestCases {
		if !ids[id] {
			t.Errorf("nonProducibleRequestCases names %q, which is not a request case", id)
		}
	}
}
