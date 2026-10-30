package runpresentation_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/leadcfg"
	"github.com/vtmocanu/uzi/api/internal/leadcfg/leadcfgtest"
	"github.com/vtmocanu/uzi/api/internal/runpresentation"
)

func TestLeadConfigViewDisplayCases(t *testing.T) {
	c := leadcfgtest.LoadContract(t)
	if len(c.DisplayCases) == 0 {
		t.Fatal("contract has no display_cases")
	}
	visited := map[string]bool{}
	for _, dc := range c.DisplayCases {
		visited[dc.ID] = false
		t.Run(dc.ID, func(t *testing.T) {
			dec := json.NewDecoder(strings.NewReader(string(dc.Input)))
			dec.DisallowUnknownFields()
			var in runpresentation.LeadConfigViewInput
			if err := dec.Decode(&in); err != nil {
				t.Fatalf("decode input: %v", err)
			}
			got := runpresentation.LeadConfigView(in)
			visited[dc.ID] = true
			if got.Chip != dc.Expect.Chip {
				t.Errorf("chip = %q, want %q", got.Chip, dc.Expect.Chip)
			}
			if len(got.Lines) != len(dc.Expect.Lines) || (len(got.Lines) > 0 && !reflect.DeepEqual(got.Lines, dc.Expect.Lines)) {
				t.Errorf("lines = %q, want %q", got.Lines, dc.Expect.Lines)
			}
		})
	}
	for id, ok := range visited {
		if !ok {
			t.Errorf("display case %q was not visited", id)
		}
	}
}

func TestLeadConfigLabelTableMatchesContract(t *testing.T) {
	c := leadcfgtest.LoadContract(t)
	l := c.Labels
	for _, h := range []string{"claude", "codex"} {
		name := map[string]string{"claude": "Claude", "codex": "Codex"}[h]
		for tok, want := range l.Model {
			if got := leadcfg.SourceLabel("model", tok, h); got != strings.ReplaceAll(want, "{H}", name) {
				t.Errorf("model %s/%s = %q, want %q", h, tok, got, want)
			}
		}
		for tok, want := range l.Effort {
			if got := leadcfg.SourceLabel("effort", tok, h); got != strings.ReplaceAll(want, "{H}", name) {
				t.Errorf("effort %s/%s = %q, want %q", h, tok, got, want)
			}
		}
		for tok, want := range l.EffortModes {
			if got := leadcfg.SourceLabel("effort", tok, h); got != want {
				t.Errorf("effort mode %s = %q, want %q", tok, got, want)
			}
		}
	}
	for tok, want := range l.Evidence {
		if got := leadcfg.EvidenceLabel(tok); got != want {
			t.Errorf("evidence %s = %q, want %q", tok, got, want)
		}
	}
	for _, p := range []struct{ name, got, want string }{
		{"model_sdk_default", leadcfg.SDKDefaultModelLabel, l.ModelSDKDefault},
		{"not_observed", leadcfg.NotObservedLabel, l.NotObserved},
		{"effort_not_sent", leadcfg.EffortNotSentLabel, l.EffortNotSent},
		{"observed_effort_other", leadcfg.ObservedEffortOtherLabel, l.ObservedEffortOther},
	} {
		if p.got != p.want {
			t.Errorf("%s = %q, want %q", p.name, p.got, p.want)
		}
	}
}

func TestLeadConfigViewInputFromRun(t *testing.T) {
	model := "gpt-6-astra"
	run := apitypes.RunDTO{Status: "queued", Harness: "codex", Model: &model, LeadConfig: &apitypes.RunLeadConfigDTO{Applicable: true, Current: true}}
	got := runpresentation.LeadConfigViewInputFromRun(run)
	if got.Status != "queued" || got.Harness != "codex" || got.PreClaim.Model != model || got.PreClaim.ModelSource != "schedule_pin" || got.LeadConfig != run.LeadConfig {
		t.Fatalf("unexpected input: %+v", got)
	}
	if got := runpresentation.LeadConfigViewInputFromRun(apitypes.RunDTO{}); got.PreClaim.Model != "" {
		t.Fatalf("nil model must stay empty, got %q", got.PreClaim.Model)
	}
}

func strp(s string) *string { return &s }

// The observed side (rule 4/5) has no fixture case until M2; these pin it meanwhile.
func TestLeadConfigViewObservedSide(t *testing.T) {
	row := func(mut func(*apitypes.LeadPhaseConfigDTO)) runpresentation.LeadConfigViewInput {
		p := apitypes.LeadPhaseConfigDTO{
			Phase: "implement", PhaseEntry: 1, Seq: 1, Harness: "claude",
			RequestedModel: strp("opus"), RequestedModelSource: "user_default",
			RequestedEffort: strp("xhigh"), RequestedEffortMode: "explicit", RequestedEffortSource: "user_default",
		}
		mut(&p)
		return runpresentation.LeadConfigViewInput{Status: "running", Harness: "claude",
			LeadConfig: &apitypes.RunLeadConfigDTO{Applicable: true, Current: true, Phases: []apitypes.LeadPhaseConfigDTO{p}}}
	}
	cases := []struct {
		name string
		in   runpresentation.LeadConfigViewInput
		chip string
		want []string // tooltip lines 3 and 4 (runtime, effort)
	}{
		{"mismatch from runtime report", row(func(p *apitypes.LeadPhaseConfigDTO) {
			p.ObservedModel, p.ObservedModelEvidence = strp("opus-4"), strp("claude_init")
			p.ObservedEffort, p.ObservedEffortEvidence = strp("high"), strp("claude_init")
		}), "lead: opus · high (requested xhigh)", []string{"runtime: opus-4 (Claude SDK init)", "effort: high (Claude SDK init)"}},
		{"codex_sent mismatch keeps requested", row(func(p *apitypes.LeadPhaseConfigDTO) {
			p.ObservedEffort, p.ObservedEffortEvidence = strp("high"), strp("codex_sent")
		}), "lead: opus · xhigh", []string{"runtime: not reported yet", "effort: high (sent to Codex)"}},
		{"other is never a level", row(func(p *apitypes.LeadPhaseConfigDTO) {
			p.ObservedEffort, p.ObservedEffortEvidence = strp("other"), strp("claude_init")
		}), "lead: opus · xhigh", []string{"runtime: not reported yet", "effort: an unrecognized level (Claude SDK init)"}},
		{"claude not sent", row(func(p *apitypes.LeadPhaseConfigDTO) {
			p.ObservedEffortEvidence = strp("claude_init")
		}), "lead: opus · xhigh", []string{"runtime: not reported yet", "effort: not sent"}},
		{"unknown evidence", row(func(p *apitypes.LeadPhaseConfigDTO) {
			p.ObservedModelEvidence, p.ObservedEffortEvidence = strp("unknown"), strp("unknown")
		}), "lead: opus · xhigh", []string{"runtime: not reported by the runtime", "effort: not reported by the runtime"}},
		{"runtime default with observation", row(func(p *apitypes.LeadPhaseConfigDTO) {
			p.RequestedEffort, p.RequestedEffortMode = nil, "runtime_default"
			p.ObservedEffort, p.ObservedEffortEvidence = strp("medium"), strp("codex_runtime_default")
		}), "lead: opus · runtime default: medium", []string{"runtime: not reported yet", "effort: medium (Codex thread config)"}},
		{"runtime default with other observation", row(func(p *apitypes.LeadPhaseConfigDTO) {
			p.RequestedEffort, p.RequestedEffortMode = nil, "runtime_default"
			p.ObservedEffort, p.ObservedEffortEvidence = strp("other"), strp("codex_runtime_default")
		}), "lead: opus · runtime default", []string{"runtime: not reported yet", "effort: an unrecognized level (Codex thread config)"}},
		{"builtin with no model uses the observed model", row(func(p *apitypes.LeadPhaseConfigDTO) {
			p.RequestedModel, p.RequestedModelSource = nil, "builtin"
			p.ObservedModel, p.ObservedModelEvidence = strp("sonnet-5"), strp("claude_init")
		}), "lead: sonnet-5 · xhigh", []string{"runtime: sonnet-5 (Claude SDK init)", "effort: not reported yet"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := runpresentation.LeadConfigView(tc.in)
			if got.Chip != tc.chip {
				t.Errorf("chip = %q, want %q", got.Chip, tc.chip)
			}
			if len(got.Lines) < 5 || !reflect.DeepEqual(got.Lines[2:4], tc.want) {
				t.Errorf("lines = %q, want runtime/effort %q", got.Lines, tc.want)
			}
		})
	}
}
