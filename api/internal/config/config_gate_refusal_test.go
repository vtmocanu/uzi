package config

import (
	"encoding/base64"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/secretbox"
)

// TestLoadRunGateRefusalMax pins the PRD #1795 M1 plan-gate refusal cap: unset gives the
// default 3, an explicit positive value is honoured, and 0 is a legal "unlimited" (the off
// switch RUN_FORGE_UNREACHABLE_MAX_PARKS=0 also has), which an ordinary >0 parser would reject.
func TestLoadRunGateRefusalMax(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://uzi:pw@db:5432/uzi?sslmode=disable")
	t.Setenv("JWT_SECRET", "unit-test-jwt-signing-key-not-a-real-secret")
	varied := make([]byte, secretbox.KeySize)
	for i := range varied {
		varied[i] = byte(i + 1)
	}
	t.Setenv("UZI_SECRET_KEY", base64.StdEncoding.EncodeToString(varied))

	for _, c := range []struct {
		env  string
		set  bool
		want int
	}{
		{want: 3},
		{env: "5", set: true, want: 5},
		{env: "0", set: true, want: 0},
	} {
		if c.set {
			t.Setenv("RUN_GATE_REFUSAL_MAX", c.env)
		}
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load() with RUN_GATE_REFUSAL_MAX=%q: %v", c.env, err)
		}
		if cfg.RunGateRefusalMax != c.want {
			t.Errorf("RunGateRefusalMax with RUN_GATE_REFUSAL_MAX=%q (set=%v) = %d, want %d", c.env, c.set, cfg.RunGateRefusalMax, c.want)
		}
	}
}
