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
