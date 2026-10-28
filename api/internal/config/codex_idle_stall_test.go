package config

import (
	"encoding/base64"
	"testing"
)

func TestCodexIdleStallMaxParks(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://uzi:pw@db:5432/uzi?sslmode=disable")
	t.Setenv("JWT_SECRET", "unit-test-jwt-signing-key-not-a-real-secret")
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	t.Setenv("UZI_SECRET_KEY", base64.StdEncoding.EncodeToString(key))
	for _, tc := range []struct {
		value string
		want  int
	}{
		{"", 3}, {"0", 0}, {"3", 3}, {"2147483646", 2147483646}, {"2147483647", 3},
		{"9999999999999999999999", 3}, {"-1", 3}, {"bad", 3}, {"4junk", 3}, {" 4 ", 4},
	} {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv("RUN_CODEX_IDLE_STALL_MAX_PARKS", tc.value)
			cfg, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			if cfg.RunCodexIdleStallMaxParks != tc.want {
				t.Fatalf("got %d want %d", cfg.RunCodexIdleStallMaxParks, tc.want)
			}
		})
	}
}
