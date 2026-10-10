package settings

import (
	"context"
	"errors"
	"testing"
	"time"
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
