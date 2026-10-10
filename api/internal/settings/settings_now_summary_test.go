package settings

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestNowSummaryEnabledThreeState(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stored string
		absent bool
		want   bool
	}{
		{name: "absent defaults on", absent: true, want: true},
		{name: "true", stored: "true", want: true},
		{name: "false", stored: "false", want: false},
		{name: "malformed falls back to the default on", stored: "yes", want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := &fakeStore{}
			if !tc.absent {
				fs.rows = append(fs.rows, row(KeyNowSummaryEnabled, tc.stored))
			}
			got, err := New(fs, time.Minute).NowSummaryEnabled(context.Background())
			if err != nil || got != tc.want {
				t.Fatalf("NowSummaryEnabled = %v, %v; want %v, nil", got, err, tc.want)
			}
		})
	}
}

func TestNowSummaryEnabledPropagatesStoreError(t *testing.T) {
	fs := &fakeStore{err: errors.New("db down")}
	if _, err := New(fs, time.Minute).NowSummaryEnabled(context.Background()); err == nil {
		t.Fatal("a store read error must reach the caller, which owns the fail-closed decision")
	}
}

func TestNowSummaryKeyWritableAndValidated(t *testing.T) {
	if !Known(KeyNowSummaryEnabled) {
		t.Fatalf("%s should be Known (admin-writable)", KeyNowSummaryEnabled)
	}
	if Defaults[KeyNowSummaryEnabled] != "true" {
		t.Fatalf("default = %q, want true", Defaults[KeyNowSummaryEnabled])
	}
	for _, ok := range []string{"true", "false"} {
		if err := Validate(KeyNowSummaryEnabled, ok); err != nil {
			t.Errorf("Validate(%q) = %v, want nil", ok, err)
		}
	}
	if err := Validate(KeyNowSummaryEnabled, "maybe"); err == nil {
		t.Error("Validate(maybe) = nil, want an error")
	}
}

// An expired cache whose refresh fails must not keep serving a stale "true": the
// instance switch is a strict read, so a failed refresh surfaces as (false, err).
func TestNowSummaryEnabledStrictOnRefreshError(t *testing.T) {
	fs := &fakeStore{rows: []store.AppSetting{row(KeyNowSummaryEnabled, "true")}}
	now := time.Unix(0, 0)
	c := New(fs, time.Minute)
	c.now = func() time.Time { return now }

	if got, err := c.NowSummaryEnabled(context.Background()); err != nil || !got {
		t.Fatalf("prime: got %v, %v; want true, nil", got, err)
	}
	now = now.Add(2 * time.Minute)
	fs.err = errors.New("db down")
	if got, err := c.NowSummaryEnabled(context.Background()); err == nil || got {
		t.Fatalf("expired cache + failed refresh: got %v, %v; want false, non-nil error", got, err)
	}
}

// A within-TTL warm cache is still served without a store read.
func TestNowSummaryEnabledWarmCacheWithinTTL(t *testing.T) {
	fs := &fakeStore{rows: []store.AppSetting{row(KeyNowSummaryEnabled, "false")}}
	c := New(fs, time.Minute)
	if got, err := c.NowSummaryEnabled(context.Background()); err != nil || got {
		t.Fatalf("prime: got %v, %v; want false, nil", got, err)
	}
	fs.err = errors.New("db down")
	if got, err := c.NowSummaryEnabled(context.Background()); err != nil || got {
		t.Fatalf("warm: got %v, %v; want false, nil", got, err)
	}
}
