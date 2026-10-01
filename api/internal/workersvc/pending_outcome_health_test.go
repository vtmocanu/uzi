package workersvc

import (
	"context"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #1994: the outcome-undelivered rung of healthTargetFor.

func TestHealthOutcomeUndeliveredRung(t *testing.T) {
	svc := healthSvc(&healthFakeStore{}, defaultHealthSettings())
	threshold := svc.pendingOutcomeThreshold()
	if threshold != 4*15*time.Second {
		t.Fatalf("threshold = %v, want 4 heartbeat intervals (60s)", threshold)
	}
	th := healthThresholds{}

	target := func(r store.ListActiveRunsForHealthRow) (string, string) {
		return svc.healthTargetFor(context.Background(), t0, r, th)
	}

	t.Run("exactly at the threshold flags", func(t *testing.T) {
		r := runRow("running")
		r.StartedAt = ago(time.Minute)
		r.LastActivityAt = ago(time.Second)
		r.TerminalPendingSince = ago(threshold)
		if h, reason := target(r); h != healthStalled || reason != reasonOutcomeUndelivered {
			t.Fatalf("got %q/%q", h, reason)
		}
	})
	t.Run("just under the threshold does not flag", func(t *testing.T) {
		r := runRow("running")
		r.StartedAt = ago(time.Minute)
		r.LastActivityAt = ago(time.Second)
		r.TerminalPendingSince = ago(threshold - time.Second)
		if _, reason := target(r); reason == reasonOutcomeUndelivered {
			t.Fatal("under the threshold must not flag")
		}
	})
	t.Run("NULL since never flags", func(t *testing.T) {
		r := runRow("running")
		r.StartedAt = ago(time.Minute)
		r.LastActivityAt = ago(time.Second)
		if _, reason := target(r); reason == reasonOutcomeUndelivered {
			t.Fatal("NULL since must not flag")
		}
	})
	t.Run("wins over the approval guard with auto-approve", func(t *testing.T) {
		r := runRow("awaiting_approval")
		r.AutoApprove = true
		r.StatusSince = ago(time.Minute)
		r.TerminalPendingSince = ago(threshold + time.Minute)
		if h, reason := target(r); h != healthStalled || reason != reasonOutcomeUndelivered {
			t.Fatalf("got %q/%q", h, reason)
		}
	})
	t.Run("queued is never flagged", func(t *testing.T) {
		r := runRow("queued")
		r.StatusSince = ago(time.Second)
		r.TerminalPendingSince = ago(threshold + time.Minute)
		if _, reason := target(r); reason == reasonOutcomeUndelivered {
			t.Fatal("queued run must not flag")
		}
	})
	t.Run("non-positive heartbeat interval falls back to 15s", func(t *testing.T) {
		p := testParams()
		p.WorkerHeartbeatInterval = 0
		s := New(&healthFakeStore{}, nil, p)
		if got := s.pendingOutcomeThreshold(); got != 60*time.Second {
			t.Fatalf("fallback threshold = %v, want 60s", got)
		}
	})
}
