package workersvc

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

func ownerHeldRun() store.ListActiveRunsForHealthRow {
	r := runRow("queued")
	r.StatusSince = ago(time.Minute)
	r.UpdatedAt = ago(30 * time.Minute)
	r.OwnerHoldPinnable = true
	r.StaleRequeueWorkerName = "w1"
	return r
}

func TestHealthOwnerHoldReasonStableNoNudge(t *testing.T) {
	for _, name := range []string{"w1", ""} {
		t.Run(name, func(t *testing.T) {
			r := ownerHeldRun()
			r.StaleRequeueWorkerName = name
			fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}
			svc, b := pinSvc(fs, defaultHealthSettings())
			svc.p.WorkerStaleRequeueGrace = 0
			svc.p.WorkerAffinityCeiling = time.Hour
			svc.detectRunHealth(context.Background(), t0)
			w := lastWrite(t, fs, r.ID)
			who := "its previous worker"
			if name != "" {
				who += " " + name
			}
			want := "waiting until 2026-07-12T12:30:00Z for " + who + " to finish draining or maintenance; another worker may take it after that"
			if w.Health != healthWaitingWorker || w.HealthReason.String != want {
				t.Fatalf("health=(%q,%q), want waiting_worker/%q", w.Health, w.HealthReason.String, want)
			}
			if w.HealthNotifiedAt.Valid || len(b.healthNudges) != 1 || b.healthNudges[0] {
				t.Fatalf("unexpected nudge: stamp=%v nudges=%v", w.HealthNotifiedAt, b.healthNudges)
			}
			r.Health, r.HealthReason, r.HealthSince = w.Health, w.HealthReason, w.HealthSince
			fs.active = []store.ListActiveRunsForHealthRow{r}
			if n := svc.detectRunHealth(context.Background(), t0.Add(time.Minute)); n != 0 {
				t.Fatalf("stable tick changed %d rows", n)
			}
		})
	}
}

func TestHealthOwnerHoldBoundaryAndControls(t *testing.T) {
	for _, tc := range []struct {
		name   string
		now    time.Time
		mutate func(*store.ListActiveRunsForHealthRow)
		held   bool
	}{
		{"before", t0.Add(-time.Nanosecond), func(*store.ListActiveRunsForHealthRow) {}, true},
		{"equality", t0, func(*store.ListActiveRunsForHealthRow) {}, true},
		{"after", t0.Add(time.Nanosecond), func(*store.ListActiveRunsForHealthRow) {}, false},
		{"NULL timestamp", t0, func(r *store.ListActiveRunsForHealthRow) { r.UpdatedAt = pgtype.Timestamptz{} }, false},
		{"missing owner or cleared flags", t0, func(r *store.ListActiveRunsForHealthRow) { r.OwnerHoldPinnable = false }, false},
		{"cross_check projection exempt", t0, func(r *store.ListActiveRunsForHealthRow) { r.Kind = "cross_check"; r.OwnerHoldPinnable = false }, false},
		{"claimed", t0, func(r *store.ListActiveRunsForHealthRow) { r.Status = "claimed" }, false},
		{"running", t0, func(r *store.ListActiveRunsForHealthRow) {
			r.Status = "running"
			r.StartedAt = ago(time.Minute)
			r.LastActivityAt = ago(time.Second)
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := ownerHeldRun()
			r.UpdatedAt = ago(time.Hour)
			tc.mutate(&r)
			svc, _ := pinSvc(&healthFakeStore{}, defaultHealthSettings())
			svc.p.WorkerAffinityCeiling = time.Hour
			_, reason := svc.healthTargetFor(context.Background(), tc.now, r, healthThresholds{queued: 10 * time.Minute, custodyLimit: testCustodyLimit, custodyLimitOK: true})
			if got := isStaleRequeuePinReason(reason); got != tc.held {
				t.Fatalf("reason=%q held=%v want %v", reason, got, tc.held)
			}
		})
	}
}

func TestHealthOwnerHoldDisabledAndPrecedence(t *testing.T) {
	for _, block := range []string{"disabled", "vault", "custody", "Codex", "isolated"} {
		for _, age := range []time.Duration{time.Minute, 15 * time.Minute} {
			t.Run(block+"/"+age.String(), func(t *testing.T) {
				r := ownerHeldRun()
				r.StatusSince = ago(age)
				fs := &healthFakeStore{}
				svc, _ := pinSvc(fs, defaultHealthSettings())
				th := healthThresholds{queued: 10 * time.Minute, custodyLimit: testCustodyLimit, custodyLimitOK: true}
				want := ""
				switch block {
				case "disabled":
					th.queued = 0
				case "vault":
					svc.SetVault(newMemoryVault(newBox(t), newMemVaultStore()))
					want = reasonVaultLocked
				case "custody":
					fs.custodyHolds = int64(testCustodyLimit)
					want = reasonCustodyLimit
				case "Codex":
					r.CodexAccountGated = true
					want = reasonCodexAccountUnavailable
				case "isolated":
					r.EgressProfileID = pgtype.UUID{Bytes: uuid.New(), Valid: true}
					want = reasonWaitingIsolatedLane
				}
				if age < th.queued {
					want = ""
				}
				flag, reason := svc.healthTargetFor(context.Background(), t0, r, th)
				expectedFlag := healthWaitingWorker
				if want == "" {
					expectedFlag = healthOK
				}
				if reason != want || flag != expectedFlag {
					t.Fatalf("health=(%q,%q) want (%q,%q)", flag, reason, expectedFlag, want)
				}
			})
		}
	}
}

func TestHealthOwnerHoldOverlapUsesCeilingBeyondShortStaleGrace(t *testing.T) {
	r := ownerHeldRun()
	r.StaleRequeuePinnable = true
	r.StatusSince = ago(5 * time.Minute)
	fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}
	svc, _ := pinSvc(fs, defaultHealthSettings())
	svc.p.WorkerAffinityCeiling = time.Hour
	svc.p.WorkerStaleRequeueGrace = 10 * time.Minute
	for _, now := range []time.Time{t0, t0.Add(6 * time.Minute)} {
		_, reason := svc.healthTargetFor(context.Background(), now, r, healthThresholds{queued: 10 * time.Minute, custodyLimit: testCustodyLimit, custodyLimitOK: true})
		want := "waiting until 2026-07-12T12:30:00Z for its previous worker w1 to finish draining or maintenance; another worker may take it after that"
		if reason != want {
			t.Fatalf("at %s reason=%q want %q", now, reason, want)
		}
	}
}

func TestHealthOwnerHoldLifecycle(t *testing.T) {
	for _, mode := range []string{"expiry", "claim", "stale only"} {
		t.Run(mode, func(t *testing.T) {
			r := ownerHeldRun()
			r.StatusSince = ago(15 * time.Minute)
			r.Health = healthWaitingWorker
			r.HealthReason = pgtype.Text{String: ownerHoldReason(t0, "w1"), Valid: true}
			r.HealthSince = ago(10 * time.Minute)
			r.UpdatedAt = ago(time.Hour)
			now := t0.Add(time.Nanosecond)
			want := reasonNoWorker
			wantNudge := true
			wantSince := now
			switch mode {
			case "claim":
				r.Status = "running"
				r.StartedAt = ago(time.Minute)
				r.LastActivityAt = ago(time.Second)
				want = ""
				wantNudge = false
			case "stale only":
				r.OwnerHoldPinnable = false
				r.StaleRequeuePinnable = true
				r.UpdatedAt = ago(time.Minute)
				want = "waiting until 2026-07-12T12:05:00Z for its previous worker w1 to return; another worker may take it after that"
				wantSince = r.HealthSince.Time
				wantNudge = false
			}
			fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}
			svc, b := pinSvc(fs, defaultHealthSettings())
			svc.p.WorkerAffinityCeiling = time.Hour
			svc.detectRunHealth(context.Background(), now)
			w := lastWrite(t, fs, r.ID)
			if w.HealthReason.String != want {
				t.Fatalf("reason=%q want %q", w.HealthReason.String, want)
			}
			if mode == "claim" {
				if w.Health != healthOK || w.HealthSince.Valid {
					t.Fatalf("claim did not clear health: %v", w)
				}
			} else if !w.HealthSince.Time.Equal(wantSince) {
				t.Fatalf("since=%v want %v", w.HealthSince, wantSince)
			}
			if len(b.healthNudges) != 1 || b.healthNudges[0] != wantNudge {
				t.Fatalf("nudges=%v want %v", b.healthNudges, wantNudge)
			}
			if wantNudge && (!w.HealthNotifiedAt.Valid || !w.HealthNotifiedAt.Time.Equal(now)) {
				t.Fatal("genuine wait missing notification stamp")
			}
			if !wantNudge && w.HealthNotifiedAt.Valid {
				t.Fatal("expected wait stamped notification")
			}
		})
	}
}
