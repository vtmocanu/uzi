package handler

import (
	"testing"

	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// TestLeadEffortSettingCellDefaultsFollowTheLeadTemplate covers the cell both effective-settings
// views (including the schedule preview, effective_settings_schedule.go) build from a prediction:
// DefaultEffort/DefaultSource say what clearing the preference resolves to.
func TestLeadEffortSettingCellDefaultsFollowTheLeadTemplate(t *testing.T) {
	str := func(s string) *string { return &s }
	for _, tc := range []struct {
		name         string
		lead         workersvc.LeadPrediction
		wantDefault  string
		wantDefSrc   string
		wantResolved string
		wantSource   string
	}{
		{"unset preference with a template", workersvc.LeadPrediction{Effort: str("high"), EffortSource: "template", TemplateEffort: str("high")},
			"high", "template", "high", "template"},
		{"explicit preference keeps its source but Default is the template", workersvc.LeadPrediction{UserEffort: str("low"), Effort: str("low"), EffortSource: "user_default", TemplateEffort: str("high")},
			"high", "template", "low", "user_default"},
		{"no template stays builtin", workersvc.LeadPrediction{Effort: str("medium"), EffortSource: "builtin"},
			"medium", "builtin", "medium", "builtin"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := leadEffortSettingCell("claude", tc.lead)
			if c.DefaultEffort != tc.wantDefault || c.DefaultSource != tc.wantDefSrc {
				t.Errorf("default = %q (%s), want %q (%s)", c.DefaultEffort, c.DefaultSource, tc.wantDefault, tc.wantDefSrc)
			}
			if got := ptrOrEmpty(c.ResolvedEffort); got != tc.wantResolved || c.Source != tc.wantSource {
				t.Errorf("resolved = %q (%s), want %q (%s)", got, c.Source, tc.wantResolved, tc.wantSource)
			}
		})
	}
}

func ptrOrEmpty(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func TestPlanningEffortCellFollowsLeadTemplate(t *testing.T) {
	for _, harness := range []workersvc.Harness{workersvc.HarnessClaude, workersvc.HarnessCodex} {
		t.Run(string(harness), func(t *testing.T) {
			level := "low"
			lead := workersvc.LeadPrediction{Harness: harness, Effort: &level, EffortSource: "template", TemplateEffort: &level}
			inherited := planningEffortCell(lead, nil)
			if inherited.Harness != string(harness) || ptrOrEmpty(inherited.ResolvedEffort) != level || inherited.Source != "template" || inherited.DefaultEffort != level {
				t.Fatalf("planning inheritance lost the lead template: %+v", inherited)
			}
			planning := "high"
			pinned := planningEffortCell(lead, &planning)
			if pinned.Harness != string(harness) || ptrOrEmpty(pinned.ResolvedEffort) != planning || pinned.Source != "user_planning_default" || pinned.DefaultEffort != level {
				t.Fatalf("planning pin must win while its default follows the lead: %+v", pinned)
			}
		})
	}
}
