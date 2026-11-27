package config

import (
	"encoding/base64"
	"testing"
	"time"
)

func TestAnalyticsSnapshotInterval(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	base := func(t *testing.T) {
		t.Setenv("DATABASE_URL", "postgres://uzi:pw@db:5432/uzi?sslmode=disable")
		t.Setenv("JWT_SECRET", "unit-test-jwt-signing-key-not-a-real-secret")
		t.Setenv("UZI_SECRET_KEY", base64.StdEncoding.EncodeToString(key))
	}
	t.Run("absent", func(t *testing.T) {
		base(t)
		cfg, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.AnalyticsSnapshotInterval != 4*time.Hour {
			t.Fatalf("got %v want 4h", cfg.AnalyticsSnapshotInterval)
		}
	})
	for _, tc := range []struct {
		value string
		want  time.Duration
	}{
		{"0", 0}, {"0s", 0}, {"1h", time.Hour}, {"24h", 24 * time.Hour}, {"2562047h", 2562047 * time.Hour},
	} {
		t.Run("ok_"+tc.value, func(t *testing.T) {
			base(t)
			t.Setenv("UZI_ANALYTICS_SNAPSHOT_INTERVAL", tc.value)
			cfg, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			if cfg.AnalyticsSnapshotInterval != tc.want {
				t.Fatalf("got %v want %v", cfg.AnalyticsSnapshotInterval, tc.want)
			}
		})
	}
	for _, value := range []string{"", " ", "59m", "-1h", "abc", "1", "1h30"} {
		t.Run("bad_"+value, func(t *testing.T) {
			base(t)
			t.Setenv("UZI_ANALYTICS_SNAPSHOT_INTERVAL", value)
			_, err := Load()
			if err == nil || err.Error() != "UZI_ANALYTICS_SNAPSHOT_INTERVAL must be 0 (off) or a Go duration of at least 1h" {
				t.Fatalf("err = %v", err)
			}
		})
	}
}
