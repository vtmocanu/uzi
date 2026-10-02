package settings

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestDecisionsMemoEnabled(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stored string
		absent bool
		want   bool
	}{
		{name: "absent defaults dark", absent: true, want: false},
		{name: "true", stored: "true", want: true},
		{name: "false", stored: "false", want: false},
		{name: "malformed falls back to dark", stored: "yes", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := &fakeStore{}
			if !tc.absent {
				fs.rows = append(fs.rows, row(KeyDecisionsMemoEnabled, tc.stored))
			}
			got, err := New(fs, time.Minute).DecisionsMemoEnabled(context.Background())
			if err != nil || got != tc.want {
				t.Fatalf("DecisionsMemoEnabled = %v, %v; want %v, nil", got, err, tc.want)
			}
		})
	}
}

func TestDecisionsMemoEnabledPropagatesStoreError(t *testing.T) {
	fs := &fakeStore{err: errors.New("db down")}
	if _, err := New(fs, time.Minute).DecisionsMemoEnabled(context.Background()); err == nil {
		t.Fatal("a store read error must reach the caller, which owns the fail-closed decision")
	}
}

func TestDecisionsMemoKeyWritableAndValidated(t *testing.T) {
	if !Known(KeyDecisionsMemoEnabled) {
		t.Fatalf("%s should be Known (admin-writable)", KeyDecisionsMemoEnabled)
	}
	if Defaults[KeyDecisionsMemoEnabled] != "false" {
		t.Fatalf("default = %q, want false", Defaults[KeyDecisionsMemoEnabled])
	}
	for _, ok := range []string{"true", "false"} {
		if err := Validate(KeyDecisionsMemoEnabled, ok); err != nil {
			t.Errorf("Validate(%q) = %v, want nil", ok, err)
		}
	}
	if err := Validate(KeyDecisionsMemoEnabled, "yes"); err == nil {
		t.Error("Validate(yes) = nil, want an error")
	}
}
