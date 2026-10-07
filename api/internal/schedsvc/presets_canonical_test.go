package schedsvc

import (
	"encoding/json"
	"os"
	"testing"
)

func TestSchedulePresetsCanonical(t *testing.T) {
	raw, err := os.ReadFile("../../../fixtures/schedule-presets/canonical.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Cron, Preset string
		Hour, Minute int
		OK           bool
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.Cron, func(t *testing.T) {
			p, h, m, ok := CronToPreset(tc.Cron)
			if p != tc.Preset || h != tc.Hour || m != tc.Minute || ok != tc.OK {
				t.Fatalf("got %s,%d,%d,%v want %+v", p, h, m, ok, tc)
			}
			if tc.Preset == PresetEveryNMinutes {
				expr, err := EveryNMinutesCron(tc.Hour)
				if err != nil || expr != tc.Cron {
					t.Fatalf("render=%q err=%v", expr, err)
				}
			}
		})
	}
	for _, n := range []int{-1, 0, 7, 8, 9, 40, 60} {
		if _, err := EveryNMinutesCron(n); err == nil {
			t.Fatalf("accepted %d", n)
		}
	}
	if _, err := PresetToCron(PresetEveryNMinutes, 0, 0); err == nil {
		t.Fatal("time of day accepted minute interval")
	}
}
