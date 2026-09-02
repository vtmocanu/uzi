package config

import (
	"testing"
	"time"
)

// TestLoadMRReviewQuietPeriod covers the MR_REVIEW_QUIET_PERIOD knob (PRD #966
// D6): the default when unset, a valid override, and the malformed fall-back to
// the default (parseNonNegDuration's default-on-error shape).
func TestLoadMRReviewQuietPeriod(t *testing.T) {
	t.Run("default when unset", func(t *testing.T) {
		slackBaseEnv(t)
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load(): %v", err)
		}
		if cfg.MRReviewQuietPeriod != 3*time.Minute {
			t.Errorf("MRReviewQuietPeriod = %v, want %v", cfg.MRReviewQuietPeriod, 3*time.Minute)
		}
	})

	t.Run("valid override", func(t *testing.T) {
		slackBaseEnv(t)
		t.Setenv("MR_REVIEW_QUIET_PERIOD", "2s")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load(): %v", err)
		}
		if cfg.MRReviewQuietPeriod != 2*time.Second {
			t.Errorf("MRReviewQuietPeriod = %v, want %v", cfg.MRReviewQuietPeriod, 2*time.Second)
		}
	})

	t.Run("malformed falls back to default", func(t *testing.T) {
		slackBaseEnv(t)
		t.Setenv("MR_REVIEW_QUIET_PERIOD", "not-a-duration")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load(): %v", err)
		}
		if cfg.MRReviewQuietPeriod != 3*time.Minute {
			t.Errorf("MRReviewQuietPeriod = %v, want %v (malformed → default)", cfg.MRReviewQuietPeriod, 3*time.Minute)
		}
	})
}
