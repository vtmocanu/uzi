package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

// TestLoadHistoryRetention pins the strict UZI_HISTORY_RETENTION parser (issue #4695): unset,
// empty and a zero duration are the same "off"; 24h or more is accepted with no maximum; a
// malformed, negative or shorter value makes Load fail naming the key.
func TestLoadHistoryRetention(t *testing.T) {
	cases := []struct {
		name    string
		set     bool
		value   string
		want    time.Duration
		wantErr string
	}{
		{name: "unset is off"},
		{name: "empty is off", set: true, value: ""},
		{name: "zero is off", set: true, value: "0"},
		{name: "0s is off", set: true, value: "0s"},
		{name: "24h", set: true, value: "24h", want: 24 * time.Hour},
		{name: "1440m", set: true, value: "1440m", want: 24 * time.Hour},
		{name: "very long has no maximum", set: true, value: "87600h", want: 87600 * time.Hour},
		{name: "just under 24h", set: true, value: "23h59m59s", wantErr: "shorter than the minimum"},
		{name: "negative", set: true, value: "-1h", wantErr: "negative"},
		{name: "days are not a Go duration", set: true, value: "30d", wantErr: "not a valid duration"},
		{name: "malformed", set: true, value: "abc", wantErr: "not a valid duration"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			autoselectEnv(t)
			if !tc.set {
				// Register the restore of whatever the environment held, then really unset it: an
				// empty value is the "empty is off" case, not "unset".
				t.Setenv("UZI_HISTORY_RETENTION", "")
				if err := os.Unsetenv("UZI_HISTORY_RETENTION"); err != nil {
					t.Fatalf("unset UZI_HISTORY_RETENTION: %v", err)
				}
			} else {
				t.Setenv("UZI_HISTORY_RETENTION", tc.value)
			}
			cfg, err := Load()
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("Load() succeeded with retention %q (HistoryRetention=%s); want an error containing %q", tc.value, cfg.HistoryRetention, tc.wantErr)
				}
				if !strings.Contains(err.Error(), "UZI_HISTORY_RETENTION") || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("Load() error = %v; want it to name UZI_HISTORY_RETENTION and contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load(): %v", err)
			}
			if cfg.HistoryRetention != tc.want {
				t.Fatalf("HistoryRetention = %s, want %s", cfg.HistoryRetention, tc.want)
			}
		})
	}
}
