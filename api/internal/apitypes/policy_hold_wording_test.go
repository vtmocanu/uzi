package apitypes

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

func TestPolicyHoldWordingMatchesFixture(t *testing.T) {
	raw, err := os.ReadFile("../../../fixtures/policy-hold/wording.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx struct {
		Causes       map[string]string `json:"causes"`
		UnknownCause string            `json:"unknown_cause"`
		Modes        map[string]struct {
			Label       string `json:"label"`
			Consequence string `json:"consequence"`
		} `json:"modes"`
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fx.Causes, policyHoldCauseText) {
		t.Errorf("cause table differs from fixture:\nfixture=%v\ngo=%v", fx.Causes, policyHoldCauseText)
	}
	if fx.UnknownCause != policyHoldUnknownCauseText {
		t.Errorf("unknown_cause = %q, fixture %q", policyHoldUnknownCauseText, fx.UnknownCause)
	}
	labels := map[string]string{}
	consequences := map[string]string{}
	for k, m := range fx.Modes {
		labels[k], consequences[k] = m.Label, m.Consequence
	}
	if !reflect.DeepEqual(labels, policyResumeModeLabel) {
		t.Errorf("label table differs from fixture:\nfixture=%v\ngo=%v", labels, policyResumeModeLabel)
	}
	if !reflect.DeepEqual(consequences, policyResumeModeConsequence) {
		t.Errorf("consequence table differs from fixture:\nfixture=%v\ngo=%v", consequences, policyResumeModeConsequence)
	}
	if len(PolicyResumeModes) != len(fx.Modes) {
		t.Errorf("PolicyResumeModes has %d entries, fixture %d", len(PolicyResumeModes), len(fx.Modes))
	}
	for _, m := range PolicyResumeModes {
		if _, ok := fx.Modes[m]; !ok {
			t.Errorf("mode %q missing from fixture", m)
		}
	}
}

func TestPolicyHoldCauseTextUnknown(t *testing.T) {
	novel := "brand_new_cause"
	for _, c := range []*string{nil, &novel} {
		if got := PolicyHoldCauseText(c); got != "held (cause not recorded)" {
			t.Errorf("PolicyHoldCauseText(%v) = %q", c, got)
		}
	}
	refused := "refused"
	if got := PolicyHoldCauseText(&refused); got != policyHoldCauseText["refused"] {
		t.Errorf("refused = %q", got)
	}
	if PolicyResumeModeLabel("nope") != "" || PolicyResumeModeConsequence("nope") != "" {
		t.Error("unknown mode must have no wording")
	}
}
