package recovery

import (
	"testing"
	"time"
)

func TestRequestDeadline(t *testing.T) {
	for _, tc := range []struct {
		name             string
		configured, want time.Duration
	}{
		{"default", 0, 120 * time.Second},
		{"negative", -time.Second, 120 * time.Second},
		{"configured", 5 * time.Second, 5 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := New(nil, nil, nil, Limits{RequestDeadline: tc.configured}, nil)
			if got := svc.RequestDeadline(); got != tc.want {
				t.Fatalf("RequestDeadline() = %s, want %s", got, tc.want)
			}
		})
	}
}
