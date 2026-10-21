package workersvc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// semanticHealthStore answers the optional GetRunSemanticHealth lookup the queued-reason rung
// reads for PRD #1230 semantic enrollment.
type semanticHealthStore struct {
	*healthFakeStore
	row store.GetRunSemanticHealthRow
	err error
}

func (s *semanticHealthStore) GetRunSemanticHealth(context.Context, store.GetRunSemanticHealthParams) (store.GetRunSemanticHealthRow, error) {
	return s.row, s.err
}

// A queued semantic checker or semantic lead with no capable worker names the specific missing
// capability; a structural run, a capable fleet and an unreadable lookup keep the generic reason.
func TestHealthSemanticQueuedReasons(t *testing.T) {
	for _, tc := range []struct {
		name string
		row  store.GetRunSemanticHealthRow
		err  error
		want string
	}{
		{"semantic checker, no capable worker", store.GetRunSemanticHealthRow{PlanCheckerEnrolled: true}, nil, reasonNoSemanticPlanCheckerCapableWorker},
		{"semantic checker, capable worker online", store.GetRunSemanticHealthRow{PlanCheckerEnrolled: true, PlanCheckerCapable: 1}, nil, reasonWaitingWorker},
		{"semantic lead, no capable worker", store.GetRunSemanticHealthRow{LeadEnrolled: true}, nil, reasonNoSemanticPublicationCapableWorker},
		{"semantic lead, capable worker online", store.GetRunSemanticHealthRow{LeadEnrolled: true, PublicationCapable: 2}, nil, reasonWaitingWorker},
		{"structural run is never named", store.GetRunSemanticHealthRow{}, nil, reasonWaitingWorker},
		{"lookup failure falls through", store.GetRunSemanticHealthRow{LeadEnrolled: true}, errors.New("boom"), reasonWaitingWorker},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := runRow("queued")
			r.StatusSince = ago(15 * time.Minute)
			fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}, onlineWorkers: 1, freeSlotWorkers: 1}
			svc := healthSvc(fs, defaultHealthSettings())
			svc.q = &semanticHealthStore{healthFakeStore: fs, row: tc.row, err: tc.err}
			svc.detectRunHealth(context.Background(), t0)
			got := lastWrite(t, fs, r.ID)
			if got.Health != healthWaitingWorker || got.HealthReason.String != tc.want {
				t.Fatalf("health=%s reason=%q want=%q", got.Health, got.HealthReason.String, tc.want)
			}
		})
	}
}
