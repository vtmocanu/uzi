package settings

import (
	"context"
	"testing"
	"time"
)

func TestWorkerHibernationEnabledDefaultsOff(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stored string
		absent bool
		want   bool
	}{
		{name: "absent defaults off", absent: true, want: false},
		{name: "true", stored: "true", want: true},
		{name: "false", stored: "false", want: false},
		{name: "malformed falls back to off", stored: "yes", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := &fakeStore{}
			if !tc.absent {
				fs.rows = append(fs.rows, row(KeyWorkerHibernationEnabled, tc.stored))
			}
			got, err := New(fs, time.Minute).WorkerHibernationEnabled(context.Background())
			if err != nil || got != tc.want {
				t.Fatalf("WorkerHibernationEnabled = %v, %v; want %v, nil", got, err, tc.want)
			}
		})
	}
}

func TestWorkerHibernationKeyWritableAndValidated(t *testing.T) {
	if KeyWorkerHibernationEnabled != "worker_hibernation_enabled" {
		t.Fatalf("key literal = %q", KeyWorkerHibernationEnabled)
	}
	if !Known(KeyWorkerHibernationEnabled) {
		t.Fatalf("%s should be Known (admin-writable)", KeyWorkerHibernationEnabled)
	}
	if Defaults[KeyWorkerHibernationEnabled] != "false" {
		t.Fatalf("default = %q, want false", Defaults[KeyWorkerHibernationEnabled])
	}
	for _, ok := range []string{"true", "false"} {
		if err := Validate(KeyWorkerHibernationEnabled, ok); err != nil {
			t.Errorf("Validate(%q) = %v, want nil", ok, err)
		}
	}
	if err := Validate(KeyWorkerHibernationEnabled, "maybe"); err == nil {
		t.Error("Validate(maybe) = nil, want an error")
	}
}
