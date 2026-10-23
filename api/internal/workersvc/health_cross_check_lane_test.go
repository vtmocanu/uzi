package workersvc

import (
	"context"
	"encoding/json"
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

func TestHealthQueuedManualCordonNudges(t *testing.T) {
	r := runRow("queued")
	r.Kind = runkind.Issue
	r.StatusSince = ago(15 * time.Minute)
	fs, svc, b := nudgeSvc(t, r, defaultHealthSettings())
	actor, worker := uuid.New(), uuid.New()
	fs.eligibility = store.CountOnlineWorkersClaimableForRunRow{
		ManualSuitable: 1, ManualCordoned: 1,
		ManualContext: mustJSON(t, map[string]any{
			"actor_id": actor, "worker_id": worker, "at": t0,
			"actor_label": "queue operator", "reason": "maintenance",
		}),
	}
	if changed := svc.detectRunHealth(context.Background(), t0); changed != 1 {
		t.Fatalf("changed=%d want 1", changed)
	}
	w := lastWrite(t, fs, r.ID)
	want := fmt.Sprintf("waiting for workers cordoned (manual): queue operator (%s) at %s: maintenance", actor, t0.Format(time.RFC3339))
	if w.Health != healthWaitingWorker || w.HealthReason.String != want {
		t.Fatalf("queued health=%s/%s want waiting_worker/%s", w.Health, w.HealthReason.String, want)
	}
	if !w.HealthNotifiedAt.Valid || !w.HealthNotifiedAt.Time.Equal(t0) || len(b.healthNudges) != 1 || !b.healthNudges[0] {
		t.Fatalf("queued manual episode did not nudge: write=%+v nudges=%v", w, b.healthNudges)
	}
}

func TestHealthCrossCheckPlacementDoesNotNudgeRunningLead(t *testing.T) {
	for _, tc := range []struct {
		name   string
		counts store.CountOnlineWorkersClaimableForRunRow
		reason string
	}{
		{"manual before no eligible checker", store.CountOnlineWorkersClaimableForRunRow{ManualSuitable: 1, ManualCordoned: 1}, "manual"},
		{"manual before busy slots", store.CountOnlineWorkersClaimableForRunRow{ManualSuitable: 1, ManualCordoned: 1, StrictEligible: 1}, "manual"},
		{"manual with suitable peer", store.CountOnlineWorkersClaimableForRunRow{ManualSuitable: 2, ManualCordoned: 1, StrictEligible: 1}, reasonCrossCheckSlotsBusy},
		{"manual own continuation", store.CountOnlineWorkersClaimableForRunRow{ManualSuitable: 1, ManualCordoned: 1, StrictEligible: 1, Claimable: 1, AssignedClaimable: 1}, reasonPlanCrossCheckWaiting},
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
			actor, worker := uuid.New(), uuid.New()
			evidence, err := json.Marshal(map[string]any{"actor_id": actor, "worker_id": worker, "at": t0, "actor_label": "parent operator", "reason": "checker maintenance"})
			if err != nil {
				t.Fatal(err)
			}
			fs.eligibility.ManualContext = evidence
			want := tc.reason
			if want == "manual" {
				want = fmt.Sprintf("waiting for workers cordoned (manual): parent operator (%s) at %s: checker maintenance", actor, t0.Format(time.RFC3339))
			}
			childID := uuid.New()
			svc.q = &healthCrossCheckPlacementStore{healthFakeStore: fs, childID: childID}
			if changed := svc.detectRunHealth(context.Background(), t0); changed != 1 {
				t.Fatalf("changed=%d want 1", changed)
			}
			w := lastWrite(t, fs, r.ID)
			if w.Health != healthWaitingWorker || w.HealthReason.String != want {
				t.Fatalf("health=%s/%s want %s/%s", w.Health, w.HealthReason.String, healthWaitingWorker, want)
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
