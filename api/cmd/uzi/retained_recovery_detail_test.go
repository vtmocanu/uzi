package main

import (
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
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
			[]string{"active secret-scan remediation", "Resume cannot succeed unchanged", "uzi run export", "--output <path>", "operator can recover the retained work from worker storage", "Cancel ends the run", "uzi run cancel"},
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

// The board row never offers Resume for an active secret-scan remediation (the server 409s it);
// any other detail keeps "Resume now/Cancel".
func TestAdoptionFailedDetailBoardRow(t *testing.T) {
	m := tuiTestModel(t, &uzicli.FakeClient{}, "")
	m.width = 200
	for _, tc := range []struct {
		detail *string
		want   string
	}{
		{strPtrT(recoveryDetailSecretRemediationActive), "Owner-held · Cancel · detail: enter"},
		{strPtrT(recoveryDetailRemediationProofUnknown), "Resume now/Cancel"},
		{nil, "Resume now/Cancel"},
	} {
		r := retainedRecoveryRun("adoption/held")
		r.RecoveryWaitDetail = tc.detail
		got := stripANSI(m.boardSecondLine(apitypes.RunListItemDTO{RunDTO: r}))
		if !strings.Contains(got, tc.want) {
			t.Errorf("detail %v: board row %q, want %q", tc.detail, got, tc.want)
		}
		if tc.want != "Resume now/Cancel" && strings.Contains(got, "Resume") {
			t.Errorf("active detail board row offers Resume: %q", got)
		}
	}
}

// The steer-queue state suffix never offers Resume for an active secret-scan remediation.
func TestAdoptionFailedDetailSteerState(t *testing.T) {
	in := apitypes.SteerInputDTO{Kind: "follow_up"}
	r := retainedRecoveryRun("adoption/held")
	if got := steerStateForRun(in, r); !strings.Contains(got, "owner Resume/Cancel") {
		t.Fatalf("unclassified steer state = %q, want Resume/Cancel", got)
	}
	r.RecoveryWaitDetail = strPtrT(recoveryDetailSecretRemediationActive)
	if got := steerStateForRun(in, r); strings.Contains(got, "Resume") || !strings.Contains(got, "owner Cancel") {
		t.Fatalf("active steer state = %q, want owner Cancel and no Resume", got)
	}
}
