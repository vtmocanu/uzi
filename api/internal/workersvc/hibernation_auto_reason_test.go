package workersvc

import (
	"testing"
	"time"
)

func TestAutoHibernationReasonFormat(t *testing.T) {
	cases := []struct {
		after time.Duration
		want  string
	}{
		{14 * 24 * time.Hour, "automatic: no run activity for 14d"},
		{36 * time.Hour, "automatic: no run activity for 36h"},
		{90 * time.Minute, "automatic: no run activity for 1h30m0s"},
	}
	for _, c := range cases {
		if got := autoHibernationReason(c.after); got != c.want {
			t.Errorf("autoHibernationReason(%v) = %q, want %q", c.after, got, c.want)
		}
	}
}
