package workersvc

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestHealthAccountCooldown(t *testing.T) {
	r := runRow("running")
	r.AccountCooldownClass = pgconv.TextOrNull("rate_limit")
	r.AccountCooldownEligibleAt = pgconv.Time(t0.Add(time.Minute))
	r.LastActivityAt = ago(time.Hour)
	fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}
	svc := healthSvc(fs, defaultHealthSettings())
	b := &fakeBroadcaster{}
	svc.SetBroadcaster(b)
	svc.detectRunHealth(context.Background(), t0)
	w := lastWrite(t, fs, r.ID)
	if w.Health != healthWaitingAccountCooldown || w.HealthReason.String != apitypes.AccountCooldownReason || w.HealthNotifiedAt.Valid {
		t.Fatalf("cooldown=%+v", w)
	}
	if len(b.healthNudges) != 1 || b.healthNudges[0] {
		t.Fatalf("entry nudges=%v", b.healthNudges)
	}
	// Identical polls, refreshed leases and extended policy leave reason, writes and WS unchanged.
	r.Health, r.HealthReason, r.HealthSince = w.Health, w.HealthReason, w.HealthSince
	for _, extra := range []time.Duration{0, time.Minute, time.Hour} {
		r.AccountCooldownEligibleAt = pgconv.Time(t0.Add(time.Minute + extra))
		fs.active[0] = r
		before := len(b.healths)
		if n := svc.detectRunHealth(context.Background(), t0); n != 0 || len(b.healths) != before || len(fs.writes) != 1 {
			t.Fatal("unchanged cooldown wrote or broadcast")
		}
	}
	// Expiry clears the expected wait without notifying.
	r.AccountCooldownEligibleAt = pgconv.Time(t0)
	r.LastActivityAt = pgconv.Time(t0)
	fs.active[0] = r
	svc.detectRunHealth(context.Background(), t0)
	w = lastWrite(t, fs, r.ID)
	if w.Health != healthOK || w.HealthNotifiedAt.Valid || b.healthNudges[len(b.healthNudges)-1] {
		t.Fatalf("exit=%+v", w)
	}
	// A later ordinary fault starts a normal episode.
	r.Health, r.HealthReason = w.Health, w.HealthReason
	r.LastActivityAt = ago(time.Hour)
	fs.active[0] = r
	svc.detectRunHealth(context.Background(), t0)
	if !b.healthNudges[len(b.healthNudges)-1] {
		t.Fatal("later fault failed to notify")
	}
}

func TestHealthAccountCooldownPrecedence(t *testing.T) {
	for _, status := range []string{"claimed", "running", "awaiting_approval"} {
		r := runRow(status)
		r.AccountCooldownClass = pgconv.TextOrNull("overload")
		r.AccountCooldownEligibleAt = pgconv.Time(t0.Add(time.Minute))
		fs := &healthFakeStore{}
		svc := healthSvc(fs, defaultHealthSettings())
		flag, reason := svc.healthTargetFor(context.Background(), t0, r, svc.healthThresholds(context.Background()))
		if flag != healthWaitingAccountCooldown || reason != apitypes.AccountCooldownReason {
			t.Fatalf("%s=%s/%s", status, flag, reason)
		}
	}
	for _, elapsed := range []time.Duration{95 * time.Second, 101 * time.Second} {
		r := runRow("running")
		r.Kind = "issue"
		r.StartedAt = ago(elapsed)
		r.BudgetWallSeconds = pgtype.Int4{Int32: 100, Valid: true}
		r.AccountCooldownClass = pgconv.TextOrNull("overload")
		r.AccountCooldownEligibleAt = pgconv.Time(t0.Add(time.Minute))
		svc := healthSvc(&healthFakeStore{}, defaultHealthSettings())
		flag, reason := svc.healthTargetFor(context.Background(), t0, r, svc.healthThresholds(context.Background()))
		if flag != healthSlow || reason != reasonNearTimeout {
			t.Fatalf("wall elapsed=%s: %s/%s", elapsed, flag, reason)
		}
	}
}
