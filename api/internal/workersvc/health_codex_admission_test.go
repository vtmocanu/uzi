package workersvc

import (
	"context"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestHealthCodexAdmissionReasonNoNudge(t *testing.T) {
	for _, class := range []string{"rate_limit", "overload"} {
		t.Run(class, func(t *testing.T) {
			r := runRow("queued")
			r.StatusSince = ago(15 * time.Minute)
			r.CodexAdmissionClass = pgconv.TextOrNull(class)
			r.CodexAdmissionEligibleAt = pgconv.Time(t0.Add(time.Minute))
			fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}, onlineWorkers: 1, freeSlotWorkers: 1}
			svc := healthSvc(fs, defaultHealthSettings())
			b := &fakeBroadcaster{}
			svc.SetBroadcaster(b)
			svc.detectRunHealth(context.Background(), t0)
			w := lastWrite(t, fs, r.ID)
			want := apitypes.CodexAdmissionReason(class, r.CodexAdmissionEligibleAt.Time)
			if w.Health != healthWaitingWorker || w.HealthReason.String != want {
				t.Fatalf("health/reason = %s/%s, want %s", w.Health, w.HealthReason.String, want)
			}
			if w.HealthNotifiedAt.Valid || len(b.healthNudges) != 1 || b.healthNudges[0] {
				t.Fatalf("cooldown nudged: notified=%v events=%v", w.HealthNotifiedAt, b.healthNudges)
			}
		})
	}
}

func TestHealthCodexAdmissionPrecedenceAndExpiry(t *testing.T) {
	for _, tc := range []struct {
		name                              string
		maintenance, unavailable, expired bool
		want                              string
	}{
		{"maintenance", true, false, false, reasonMaintenance},
		{"unavailable", false, true, false, reasonCodexAccountUnavailable},
		{"expired", false, false, true, reasonNoWorker},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := runRow("queued")
			r.StatusSince = ago(15 * time.Minute)
			r.IntakePauseHeld = tc.maintenance
			r.CodexAccountGated = tc.unavailable
			r.CodexAdmissionClass = pgconv.TextOrNull("rate_limit")
			r.CodexAdmissionEligibleAt = pgconv.Time(t0.Add(time.Minute))
			if tc.expired {
				r.CodexAdmissionEligibleAt = pgconv.Time(t0)
			}
			fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}, onlineWorkers: 0}
			svc := healthSvc(fs, defaultHealthSettings())
			svc.detectRunHealth(context.Background(), t0)
			if got := lastWrite(t, fs, r.ID).HealthReason.String; got != tc.want {
				t.Fatalf("reason=%q want %q", got, tc.want)
			}
		})
	}
}

func TestHealthCodexAdmissionExpiryRestampsWithoutBurst(t *testing.T) {
	r := runRow("queued")
	r.StatusSince = ago(40 * time.Minute)
	r.Health = healthWaitingWorker
	r.HealthReason = pgconv.TextOrNull(apitypes.CodexAdmissionReason("overload", t0.Add(-time.Minute)))
	r.HealthSince = ago(30 * time.Minute)
	fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}, onlineWorkers: 0}
	svc := healthSvc(fs, defaultHealthSettings())
	b := &fakeBroadcaster{}
	svc.SetBroadcaster(b)
	svc.detectRunHealth(context.Background(), t0)
	w := lastWrite(t, fs, r.ID)
	if w.HealthReason.String != reasonNoWorker || !w.HealthSince.Time.Equal(t0) {
		t.Fatalf("expiry did not clear/restamp: %+v", w)
	}
	if w.HealthNotifiedAt.Valid || len(b.healthNudges) != 1 || b.healthNudges[0] {
		t.Fatalf("expiry burst: %+v %v", w.HealthNotifiedAt, b.healthNudges)
	}
}
