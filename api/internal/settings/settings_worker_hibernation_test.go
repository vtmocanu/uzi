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

func TestWorkerHibernationAutoDefaultsOff(t *testing.T) {
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
				fs.rows = append(fs.rows, row(KeyWorkerHibernationAuto, tc.stored))
			}
			got, err := New(fs, time.Minute).WorkerHibernationAuto(context.Background())
			if err != nil || got != tc.want {
				t.Fatalf("WorkerHibernationAuto = %v, %v; want %v, nil", got, err, tc.want)
			}
		})
	}
	if KeyWorkerHibernationAuto != "worker_hibernation_auto" || !Known(KeyWorkerHibernationAuto) ||
		Defaults[KeyWorkerHibernationAuto] != "false" {
		t.Fatalf("auto key: literal=%q known=%v default=%q", KeyWorkerHibernationAuto, Known(KeyWorkerHibernationAuto), Defaults[KeyWorkerHibernationAuto])
	}
	if err := Validate(KeyWorkerHibernationAuto, "maybe"); err == nil {
		t.Error("Validate(maybe) = nil, want an error")
	}
	if err := Validate(KeyWorkerHibernationAuto, "true"); err != nil {
		t.Errorf("Validate(true) = %v", err)
	}
}

func TestWorkerHibernationIdleAfterBoundsAndFallback(t *testing.T) {
	if KeyWorkerHibernationIdleAfter != "worker_hibernation_idle_after" || !Known(KeyWorkerHibernationIdleAfter) ||
		Defaults[KeyWorkerHibernationIdleAfter] != "336h" {
		t.Fatalf("idle-after key: literal=%q known=%v default=%q", KeyWorkerHibernationIdleAfter,
			Known(KeyWorkerHibernationIdleAfter), Defaults[KeyWorkerHibernationIdleAfter])
	}
	for _, tc := range []struct {
		value string
		want  string // "" = accepted
	}{
		{"59m", "must be between 1h and 2160h"},
		{"1h", ""},
		{"336h", ""},
		{"2160h", ""},
		{"2161h", "must be between 1h and 2160h"},
		{"14d", `must be a duration like "336h"`},
		{"", `must be a duration like "336h"`},
		{"junk", `must be a duration like "336h"`},
	} {
		err := Validate(KeyWorkerHibernationIdleAfter, tc.value)
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("Validate(%q) = %v, want nil", tc.value, err)
		case tc.want != "" && (err == nil || err.Error() != tc.want):
			t.Errorf("Validate(%q) = %v, want %q", tc.value, err, tc.want)
		}
	}
	for _, tc := range []struct {
		name   string
		stored string
		absent bool
		want   time.Duration
	}{
		{name: "absent defaults to 14 days", absent: true, want: 336 * time.Hour},
		{name: "stored value", stored: "48h", want: 48 * time.Hour},
		{name: "junk falls back", stored: "soon", want: 336 * time.Hour},
		{name: "below range falls back", stored: "59m", want: 336 * time.Hour},
		{name: "above range falls back", stored: "2161h", want: 336 * time.Hour},
		{name: "upper bound kept", stored: "2160h", want: 2160 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := &fakeStore{}
			if !tc.absent {
				fs.rows = append(fs.rows, row(KeyWorkerHibernationIdleAfter, tc.stored))
			}
			got, err := New(fs, time.Minute).WorkerHibernationIdleAfter(context.Background())
			if err != nil || got != tc.want {
				t.Fatalf("WorkerHibernationIdleAfter = %v, %v; want %v, nil", got, err, tc.want)
			}
		})
	}
}
