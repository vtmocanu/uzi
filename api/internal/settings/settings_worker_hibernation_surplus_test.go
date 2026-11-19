package settings

import (
	"context"
	"testing"
	"time"
)

func TestWorkerHibernationSurplusDefaultsOff(t *testing.T) {
	if KeyWorkerHibernationSurplus != "worker_hibernation_surplus" || !Known(KeyWorkerHibernationSurplus) ||
		Defaults[KeyWorkerHibernationSurplus] != "false" {
		t.Fatalf("surplus key: literal=%q known=%v default=%q", KeyWorkerHibernationSurplus,
			Known(KeyWorkerHibernationSurplus), Defaults[KeyWorkerHibernationSurplus])
	}
	if err := Validate(KeyWorkerHibernationSurplus, "maybe"); err == nil {
		t.Error("Validate(maybe) = nil, want an error")
	}
	if err := Validate(KeyWorkerHibernationSurplus, "true"); err != nil {
		t.Errorf("Validate(true) = %v", err)
	}
	for _, tc := range []struct {
		name   string
		stored string
		absent bool
		want   bool
	}{
		{name: "absent defaults off", absent: true, want: false},
		{name: "true", stored: "true", want: true},
		{name: "malformed falls back to off", stored: "yes", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := &fakeStore{}
			if !tc.absent {
				fs.rows = append(fs.rows, row(KeyWorkerHibernationSurplus, tc.stored))
			}
			got, err := New(fs, time.Minute).WorkerHibernationSurplus(context.Background())
			if err != nil || got != tc.want {
				t.Fatalf("WorkerHibernationSurplus = %v, %v; want %v, nil", got, err, tc.want)
			}
		})
	}
}

func TestWorkerHibernationSurplusIdleAfterBoundsAndFallback(t *testing.T) {
	if KeyWorkerHibernationSurplusIdleAfter != "worker_hibernation_surplus_idle_after" ||
		!Known(KeyWorkerHibernationSurplusIdleAfter) ||
		Defaults[KeyWorkerHibernationSurplusIdleAfter] != "2h" {
		t.Fatalf("surplus idle-after key: literal=%q known=%v default=%q", KeyWorkerHibernationSurplusIdleAfter,
			Known(KeyWorkerHibernationSurplusIdleAfter), Defaults[KeyWorkerHibernationSurplusIdleAfter])
	}
	const badRange = "must be between 15m and 336h"
	const badForm = `must be a duration like "2h"`
	for _, tc := range []struct {
		value string
		want  string // "" = accepted
	}{
		{"14m59s", badRange},
		{"15m", ""},
		{"2h", ""},
		{"336h", ""},
		{"336h1s", badRange},
		{"2d", badForm},
		{"", badForm},
		{"junk", badForm},
	} {
		err := Validate(KeyWorkerHibernationSurplusIdleAfter, tc.value)
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
		{name: "absent defaults to 2h", absent: true, want: 2 * time.Hour},
		{name: "stored value", stored: "45m", want: 45 * time.Minute},
		{name: "junk falls back", stored: "soon", want: 2 * time.Hour},
		{name: "below range falls back", stored: "14m59s", want: 2 * time.Hour},
		{name: "above range falls back", stored: "336h1s", want: 2 * time.Hour},
		{name: "bounds kept", stored: "336h", want: 336 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := &fakeStore{}
			if !tc.absent {
				fs.rows = append(fs.rows, row(KeyWorkerHibernationSurplusIdleAfter, tc.stored))
			}
			got, err := New(fs, time.Minute).WorkerHibernationSurplusIdleAfter(context.Background())
			if err != nil || got != tc.want {
				t.Fatalf("WorkerHibernationSurplusIdleAfter = %v, %v; want %v, nil", got, err, tc.want)
			}
		})
	}
}
