package workersvc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// #3150 health pill: the custody limit is the custody_hold_limit setting read ONCE per pass; an
// unreadable setting is reported as such (never a guessed number) and never nudges.

func queuedCustodyRun() store.ListActiveRunsForHealthRow {
	r := runRow("queued")
	r.StatusSince = ago(15 * time.Minute) // past the 10m queued threshold
	return r
}

func TestHealthCustodyLimitComesFromSetting(t *testing.T) {
	for _, tc := range []struct {
		name        string
		limit       int32
		holds       int64
		wantCustody bool
	}{
		{"4 holds at limit 4", 4, 4, true},
		{"3 holds at limit 4", 4, 3, false},
		{"8 holds at limit 12 (the old constant no longer blocks)", 12, 8, false},
		{"12 holds at limit 12", 12, 12, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, b := queuedCustodyRun(), queuedCustodyRun()
			b.ID = a.ID
			b.ID[0]++ // a second queued run: still ONE setting read per pass
			fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{a, b}, custodyHolds: tc.holds}
			svc := healthSvc(fs, defaultHealthSettings())
			reader := &fakeCustodyLimit{n: tc.limit}
			svc.p.CustodyHoldLimit = reader

			svc.detectRunHealth(context.Background(), t0)

			if n := reader.calls.Load(); n != 1 {
				t.Fatalf("setting read %d times in one pass, want 1", n)
			}
			got := lastWrite(t, fs, a.ID).HealthReason.String == reasonCustodyLimit
			if got != tc.wantCustody {
				t.Fatalf("custody reason = %v, want %v", got, tc.wantCustody)
			}
			for _, l := range fs.custodyLimitCalls {
				if l != tc.limit {
					t.Fatalf("custody admission queried with limit %d, want %d", l, tc.limit)
				}
			}
		})
	}
}

func TestHealthCustodyLimitUnavailableReasonNoNudge(t *testing.T) {
	for name, reader := range map[string]CustodyHoldLimitReader{
		"read error": &fakeCustodyLimit{err: errors.New("settings down")},
		"nil reader": nil,
	} {
		t.Run(name, func(t *testing.T) {
			r := queuedCustodyRun()
			fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}, custodyHolds: 100}
			svc := healthSvc(fs, defaultHealthSettings())
			svc.p.CustodyHoldLimit = reader
			bc := &fakeBroadcaster{}
			svc.SetBroadcaster(bc)

			svc.detectRunHealth(context.Background(), t0)

			w := lastWrite(t, fs, r.ID)
			if w.Health != healthWaitingWorker || w.HealthReason.String != reasonCustodyLimitUnavailable {
				t.Fatalf("health = %q reason = %q, want waiting_worker / the unavailable reason", w.Health, w.HealthReason.String)
			}
			if w.HealthNotifiedAt.Valid {
				t.Fatal("an instance-wide setting failure stamped the nudge cooldown")
			}
			if len(bc.healthNudges) != 1 || bc.healthNudges[0] {
				t.Fatalf("broadcast nudge = %v, want [false]", bc.healthNudges)
			}
			if len(fs.custodyCalls) != 0 {
				t.Fatalf("custody admission queried %d times with no limit", len(fs.custodyCalls))
			}
		})
	}
}

// A threshold struct built without the custody read is UNAVAILABLE, not "limit 0".
func TestHealthZeroThresholdsCustodyUnavailable(t *testing.T) {
	r := queuedCustodyRun()
	fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}, custodyHolds: 100}
	svc := healthSvc(fs, defaultHealthSettings())
	if got := svc.queuedReason(context.Background(), t0, r, healthThresholds{}); got != reasonCustodyLimitUnavailable {
		t.Fatalf("queuedReason(healthThresholds{}) = %q, want the unavailable reason", got)
	}
	if _, reason := svc.healthTargetFor(context.Background(), t0, r, healthThresholds{queued: 10 * time.Minute}); reason != reasonCustodyLimitUnavailable {
		t.Fatalf("healthTargetFor reason = %q, want the unavailable reason", reason)
	}
}

// Cross-check children are exempt from custody admission, so neither custody reason applies.
func TestHealthCrossCheckNoCustodyReason(t *testing.T) {
	r := queuedCustodyRun()
	r.Kind = "cross_check"
	fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}, custodyHolds: 100}
	svc := healthSvc(fs, defaultHealthSettings())
	for _, th := range []healthThresholds{{}, thWithLimit(1)} {
		got := svc.queuedReason(context.Background(), t0, r, th)
		if got == reasonCustodyLimit || got == reasonCustodyLimitUnavailable {
			t.Fatalf("cross_check run got custody reason %q (th %+v)", got, th)
		}
	}
	if len(fs.custodyCalls) != 0 {
		t.Fatalf("cross_check run queried custody admission %d times", len(fs.custodyCalls))
	}
}
