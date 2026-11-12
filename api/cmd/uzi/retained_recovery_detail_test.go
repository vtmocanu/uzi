package main

import (
	"strings"
	"testing"
)

// TestAdoptionFailedDetailGuidance covers the closed switch of issue #4177: a known detail picks
// its own guidance, and nil or any other value keeps the generic text and never prints the raw string.
func TestAdoptionFailedDetailGuidance(t *testing.T) {
	const generic = "Resume starts another recovery episode"
	cases := []struct {
		name   string
		detail *string
		want   []string
		deny   []string
	}{
		{"active", strPtrT(recoveryDetailSecretRemediationActive),
			[]string{"active secret-scan remediation", "Resume cannot succeed unchanged", "uzi run export", "--output <file>", "operator can recover the retained work from worker storage", "Cancel ends the run", "uzi run cancel"},
			[]string{generic, "remediation proof could not be verified", "uzi run resume", "success is not assured"}},
		{"unknown proof", strPtrT(recoveryDetailRemediationProofUnknown),
			[]string{"remediation proof could not be verified", "Resume may help", "uzi run resume"},
			[]string{generic, "cannot succeed unchanged"}},
		{"nil", nil, []string{generic}, []string{"cannot succeed unchanged", "proof could not be verified"}},
		{"unrecognised", strPtrT("raw-new-value"), []string{generic}, []string{"raw-new-value", "cannot succeed unchanged"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := retainedRecoveryRun("adoption/held")
			r.RecoveryWaitDetail = tc.detail
			line := recoveryDecisionLine(r)
			tui := strings.Join(recoveryDecisionTUILines(r), "\n")
			flat := strings.Join(strings.Fields(tui), " ")
			for _, w := range tc.want {
				if !strings.Contains(line, w) {
					t.Errorf("line missing %q: %s", w, line)
				}
				if !strings.Contains(flat, w) {
					t.Errorf("TUI lines missing %q: %s", w, tui)
				}
			}
			for _, d := range tc.deny {
				if strings.Contains(line, d) || strings.Contains(flat, d) {
					t.Errorf("unexpected %q in line or TUI lines", d)
				}
			}
		})
	}
}

// A detail on any other cause is ignored by the closed switch.
func TestRecoveryDetailIgnoredOnOtherCauses(t *testing.T) {
	r := retainedRecoveryRun("held")
	d := recoveryDetailSecretRemediationActive
	r.RecoveryWaitDetail = &d
	if strings.Contains(recoveryDecisionLine(r), "secret-scan") {
		t.Fatal("detail leaked into a non-adoption cause")
	}
}
