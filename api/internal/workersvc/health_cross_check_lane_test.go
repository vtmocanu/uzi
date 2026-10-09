package workersvc

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type healthCrossCheckPlacementStore struct {
	*healthFakeStore
	childID uuid.UUID
}

func (f *healthCrossCheckPlacementStore) GetPlanCrossCheckChildForHealth(context.Context, uuid.UUID) (uuid.UUID, error) {
	return f.childID, nil
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
