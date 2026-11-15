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
	// PRD #3250 DC-P1 to DC-P8 stay in the contract.
	for _, prefix := range []string{"dcp1-", "dcp2-", "dcp3-", "dcp4-", "dcp5-", "dcp6-", "dcp7-", "dcp8-"} {
		found := false
		for id := range visited {
			found = found || strings.HasPrefix(id, prefix)
		}
		if !found {
			t.Errorf("no display case with prefix %q", prefix)
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

func TestLeadConfigViewInputFromRunRunStartChoice(t *testing.T) {
	str := func(s string) *string { return &s }
	got := runpresentation.LeadConfigViewInputFromRun(apitypes.RunDTO{
		Model: str("opus"), ModelSource: str("run_override"), Effort: str("xhigh"), EffortSource: str("run_override"),
	})
	want := runpresentation.LeadPreClaim{Model: "opus", ModelSource: "run_override", Effort: "xhigh", EffortSource: "run_override"}
	if got.PreClaim != want {
		t.Fatalf("PreClaim = %+v, want %+v", got.PreClaim, want)
	}
	// DC-P8: a model with a NULL source reads as schedule_pin. The claim resolver honours an effort only
	// from run_override, so an effort with a NULL source is not shown pre-claim.
	got = runpresentation.LeadConfigViewInputFromRun(apitypes.RunDTO{Model: str("opus"), Effort: str("high")})
	if got.PreClaim.ModelSource != "schedule_pin" || got.PreClaim.Effort != "" || got.PreClaim.EffortSource != "" {
		t.Fatalf("NULL sources = %+v", got.PreClaim)
	}
	got = runpresentation.LeadConfigViewInputFromRun(apitypes.RunDTO{HoldReason: str("run_override_unavailable")})
	if got.HoldReason != "run_override_unavailable" {
		t.Fatalf("HoldReason = %q", got.HoldReason)
	}
	if got := runpresentation.LeadConfigViewInputFromRun(apitypes.RunDTO{}); got.PreClaim != (runpresentation.LeadPreClaim{}) {
		t.Fatalf("no choice must leave PreClaim empty, got %+v", got.PreClaim)
	}
}
