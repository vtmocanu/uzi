package workersvc

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
)

type healthCrossCheckPlacementStore struct {
	*healthFakeStore
	childID uuid.UUID
}

func (f *healthCrossCheckPlacementStore) GetPlanCrossCheckChildForHealth(context.Context, uuid.UUID) (uuid.UUID, error) {
	return f.childID, nil
}

func TestHealthQueuedCheckerCustodyExemption(t *testing.T) {
	for _, n := range []int64{8, 9} {
		for _, tc := range []struct {
			name   string
			counts store.CountOnlineWorkersClaimableForRunRow
			gated  bool
			want   string
		}{
			{"waiting", store.CountOnlineWorkersClaimableForRunRow{StrictEligible: 1, Claimable: 1}, false, reasonPlanCrossCheckWaiting},
			{"busy", store.CountOnlineWorkersClaimableForRunRow{StrictEligible: 1}, false, reasonCrossCheckSlotsBusy},
			{"no worker", store.CountOnlineWorkersClaimableForRunRow{}, false, reasonNoWorker},
			{"credential gated", store.CountOnlineWorkersClaimableForRunRow{StrictEligible: 1, Claimable: 1}, true, reasonCodexAccountUnavailable},
		} {
			t.Run(fmt.Sprintf("%d/%s", n, tc.name), func(t *testing.T) {
				r := runRow("queued")
				r.Kind = "cross_check"
				r.StatusSince = ago(15 * time.Minute)
				r.CodexAccountGated = tc.gated
				fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}, custodyHolds: n, eligibility: tc.counts}
				svc := healthSvc(fs, defaultHealthSettings())
				svc.detectRunHealth(context.Background(), t0)
				w := lastWrite(t, fs, r.ID)
				if w.Health != healthWaitingWorker || w.HealthReason.String != tc.want {
					t.Errorf("queued checker health=%s/%s want waiting_worker/%s", w.Health, w.HealthReason.String, tc.want)
				}
				if len(fs.custodyCalls) != 0 {
					t.Errorf("hold-free queued checker made %d custody lookups", len(fs.custodyCalls))
				}
				r.Kind = "issue"
				fs.custodyCalls = nil
				if got := svc.queuedReason(context.Background(), t0, r, thWithLimit(testCustodyLimit)); got != reasonCustodyLimit || len(fs.custodyCalls) != 1 {
					t.Fatalf("ordinary code health=%s custody lookups=%d", got, len(fs.custodyCalls))
				}
			})
		}
	}
}

func TestHealthQueuedCheckerVaultPrecedesPlacement(t *testing.T) {
	r := runRow("queued")
	r.Kind = "cross_check"
	r.StatusSince = ago(15 * time.Minute)
	fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}, custodyHolds: 9}
	svc := healthSvc(fs, defaultHealthSettings())
	svc.SetVault(vault.New(newBox(t), newMemVaultStore()))
	svc.detectRunHealth(context.Background(), t0)
	if got := lastWrite(t, fs, r.ID).HealthReason.String; got != reasonVaultLocked {
		t.Fatalf("locked checker reason=%s", got)
	}
	if len(fs.custodyCalls) != 0 || len(fs.eligibilityCalls) != 0 {
		t.Fatal("locked checker queried custody or placement")
	}
}

func TestHealthCrossCheckPlacementDoesNotNudgeRunningLead(t *testing.T) {
	for _, tc := range []struct {
		name   string
		counts store.CountOnlineWorkersClaimableForRunRow
		reason string
	}{
		{"slot busy", store.CountOnlineWorkersClaimableForRunRow{StrictEligible: 1}, reasonCrossCheckSlotsBusy},
		{"no eligible checker", store.CountOnlineWorkersClaimableForRunRow{}, reasonNoCrossCheckWorker},
		{"checker available", store.CountOnlineWorkersClaimableForRunRow{StrictEligible: 1, Claimable: 1}, reasonPlanCrossCheckWaiting},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := stalledRunRow()
			r.Kind = runkind.Issue
			fs, svc, b := nudgeSvc(t, r, defaultHealthSettings())
			fs.liveCrossCheck = map[uuid.UUID]bool{r.ID: true}
			fs.eligibility = tc.counts
			childID := uuid.New()
			svc.q = &healthCrossCheckPlacementStore{healthFakeStore: fs, childID: childID}
			if changed := svc.detectRunHealth(context.Background(), t0); changed != 1 {
				t.Fatalf("changed=%d want 1", changed)
			}
			w := lastWrite(t, fs, r.ID)
			if w.Health != healthWaitingWorker || w.HealthReason.String != tc.reason {
				t.Fatalf("health=%s/%s want %s/%s", w.Health, w.HealthReason.String, healthWaitingWorker, tc.reason)
			}
			if w.HealthNotifiedAt.Valid || len(b.healthNudges) != 1 || b.healthNudges[0] {
				t.Fatalf("running lead waiting on child nudged: write=%+v nudges=%v", w, b.healthNudges)
			}
			if len(fs.eligibilityCalls) != 1 || fs.eligibilityCalls[0].RunID != childID {
				t.Fatalf("eligibility calls=%v want child %s", fs.eligibilityCalls, childID)
			}
		})
	}
}
