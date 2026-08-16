package config

import (
	"encoding/base64"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/autoselect"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
)

// PRD #111 M6 — the auto-selection knobs, and specifically the ONE default the e2e
// harness can no longer exercise.
//
// 🔴 WHY THIS FILE EXISTS AT ALL. UZI_AUTOSELECT_MAX_STALENESS defaults to
// 3 x UZI_USAGE_POLL_INTERVAL, and the e2e overlay disables the poller — so in that
// stack the default computes to ZERO, nothing is ever fresh, and an `auto` worker can
// only ever fall back. That is correct behaviour (R2), but it makes the happy path
// undrivable there, so M6 pins the knob in the overlay and drives both outcomes from
// synced_at instead. The arithmetic the overlay stopped exercising is pinned here,
// where an arithmetic default belongs and where it can be asserted EXACTLY rather than
// inferred from a run's behaviour.

func autoselectEnv(t *testing.T) {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgres://uzi:pw@db:5432/uzi?sslmode=disable")
	// Low-entropy but valid (non-placeholder, long-enough) signing key; a
	// high-entropy literal would trip the secret scanner on a fresh add.
	t.Setenv("JWT_SECRET", "unit-test-jwt-signing-key-not-a-real-secret")
	varied := make([]byte, secretbox.KeySize)
	for i := range varied {
		varied[i] = byte(i + 1)
	}
	t.Setenv("UZI_SECRET_KEY", base64.StdEncoding.EncodeToString(varied))
}

// TestAutoselectMaxStalenessTracksThePollInterval is R2 as arithmetic. The two rows
// that matter are the last one — a disabled poller yielding a ZERO window, which is
// what makes every token stale and auto degrade to the worker's non-auto binding —
// and the first, which pins the 3x multiplier that D17 chose so the selector and the
// shipped meter agree about the same reading.
//
// D17's residual, stated honestly in config.go and worth repeating where the test
// lives: they agree by MATCHING DEFAULTS, not by sharing one definition, so an
// operator who overrides this re-opens the divergence.
func TestAutoselectMaxStalenessTracksThePollInterval(t *testing.T) {
	for _, tc := range []struct {
		name     string
		interval string
		want     time.Duration
	}{
		{"the shipped default interval", "", 3 * 5 * time.Minute},
		{"an explicit interval", "2m", 6 * time.Minute},
		{"the poller DISABLED", "0", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			autoselectEnv(t)
			if tc.interval != "" {
				t.Setenv("UZI_USAGE_POLL_INTERVAL", tc.interval)
			}
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load(): %v", err)
			}
			if got := cfg.AutoselectMaxStaleness; got != tc.want {
				t.Fatalf("AutoselectMaxStaleness = %v, want %v (3x the poll interval %q)",
					got, tc.want, cfg.UsagePollInterval)
			}
		})
	}
}

// TestAutoselectDisabledPollerMakesEverythingStale closes the loop the previous test
// only opens. A zero window is an arithmetic fact; that it PRODUCES the fallback is a
// behavioural one, and the two are worth joining in one place because the e2e phase no
// longer does it.
//
// It feeds the loaded policy to the real classifier with a reading from ONE NANOSECOND
// ago — the most favourable input a fresh gauge could ever produce — and requires
// stale. Anything weaker (a reading from an hour ago) would pass against a policy that
// simply had a short window rather than a zero one.
func TestAutoselectDisabledPollerMakesEverythingStale(t *testing.T) {
	autoselectEnv(t)
	t.Setenv("UZI_USAGE_POLL_INTERVAL", "0")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	now := time.Now()
	syncedAt := now.Add(-time.Nanosecond)
	five, seven := int16(0), int16(0) // a completely unused token: nothing else can disqualify it
	got := autoselect.Classify(autoselect.Candidate{
		AutoEligible: true,
		HasReading:   true,
		FiveHourPct:  &five,
		SevenDayPct:  &seven,
		SyncedAt:     &syncedAt,
	}, cfg.AutoselectPolicy(), now)
	if got.Status != autoselect.StatusStale {
		t.Fatalf("with the poller disabled a one-nanosecond-old reading of a 100%%-free token "+
			"classified %q, want %q. R2 is that a disabled poller degrades auto to the worker's "+
			"non-auto binding, and this is the arithmetic that does it", got.Status, autoselect.StatusStale)
	}
}

// TestAutoselectPolicyIsOneMapping guards the constructor D21 exists for. The settings
// page and the claim path both call cfg.AutoselectPolicy(), so a field wired to the
// wrong knob would make them agree with each other and disagree with the operator —
// invisible in any test that builds the Policy by hand.
//
// Four DISTINCT values, deliberately: with the defaults, MinHeadroom 15 and
// HeadroomTiePct 5 and InflightPenalty 3 are already distinct, but a future default
// collision would silently make a swapped pair untestable.
func TestAutoselectPolicyIsOneMapping(t *testing.T) {
	autoselectEnv(t)
	t.Setenv("UZI_AUTOSELECT_MIN_HEADROOM", "21")
	t.Setenv("UZI_AUTOSELECT_HEADROOM_TIE_PCT", "22")
	t.Setenv("UZI_AUTOSELECT_INFLIGHT_PENALTY", "23")
	t.Setenv("UZI_AUTOSELECT_MAX_STALENESS", "24m")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	want := autoselect.Policy{
		MinHeadroom: 21, HeadroomTiePct: 22, InflightPenalty: 23, MaxStaleness: 24 * time.Minute,
	}
	if got := cfg.AutoselectPolicy(); got != want {
		t.Fatalf("AutoselectPolicy() = %+v, want %+v — a knob is wired to the wrong field, which "+
			"the settings page and the selector would agree about while both being wrong", got, want)
	}
}
