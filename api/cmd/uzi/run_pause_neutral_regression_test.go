package main

import (
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

func TestOrdinaryPauseWithoutEvidenceNeutral(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	// An ordinary pause with no hold or message evidence cannot identify its origin.
	r := apitypes.RunDTO{ID: "r1", Kind: "prompt", Status: statusPaused, UpdatedAt: now.Add(-5 * time.Minute)}
	for _, tc := range []struct {
		name string
		got  string
	}{
		{"pausedHoldNotice", pausedHoldNotice(r)},
		{"pausedLine", pausedLine(r, now, 100)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := strings.ToLower(tc.got)
			if !strings.Contains(got, "paused") {
				t.Errorf("%s = %q, want paused", tc.name, tc.got)
			}
			for _, unsupported := range []string{"owner", "by you"} {
				if strings.Contains(got, unsupported) {
					t.Errorf("%s = %q, must not attribute pause to %q without evidence", tc.name, tc.got, unsupported)
				}
			}
		})
	}
}
