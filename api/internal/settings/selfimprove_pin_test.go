package settings_test

import (
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/schedsvc"
	"github.com/vtmocanu/uzi/api/internal/settings"
)

// TestReservedSelfImproveLabelMatchesSchedsvc pins settings' mirrored self-improve label
// to its canonical home. ValidateMerged rejects the self-improve tracker's label as a
// uzi_label using a LITERAL, because settings cannot import schedsvc (schedsvc imports
// settings — an import cycle). This EXTERNAL test package can import both, so it feeds
// the canonical schedsvc.SelfImproveTrackingLabel into ValidateMerged: if that constant
// is ever changed, the mirrored literal would no longer match and this test fails,
// catching the drift the duplicated literal would otherwise hide.
func TestReservedSelfImproveLabelMatchesSchedsvc(t *testing.T) {
	// autopilot/finding set to distinct values so only the self-improve guard can fire.
	err := settings.ValidateMerged(map[string]string{
		settings.KeyUziLabel:       schedsvc.SelfImproveTrackingLabel,
		settings.KeyAutopilotLabel: "autopilot",
		settings.KeyFindingLabel:   "agent-found",
	})
	if err == nil {
		t.Fatalf("ValidateMerged accepted uzi_label=%q (the self-improve tracker label); it must be rejected — settings' mirrored literal has drifted from schedsvc.SelfImproveTrackingLabel", schedsvc.SelfImproveTrackingLabel)
	}
	if !strings.Contains(err.Error(), "self-improve") {
		t.Fatalf("ValidateMerged rejected uzi_label=%q but not for the self-improve reason: %v", schedsvc.SelfImproveTrackingLabel, err)
	}
}
