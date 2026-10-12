package workersvc

import (
	"context"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// M1 records the current offline-mid-roll ladder without changing run health.
func TestRollEvidenceOfflineBaselineReasonNoWorker(t *testing.T) {
	r := runRow("queued")
	r.StatusSince = ago(15 * time.Minute)
	fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}, onlineWorkers: 0, freeSlotWorkers: 0}
	svc := healthSvc(fs, defaultHealthSettings())
	svc.detectRunHealth(context.Background(), t0)
	w := lastWrite(t, fs, r.ID)
	if w.HealthReason.String != reasonNoWorker {
		t.Fatalf("offline ladder reason=%q, want %q", w.HealthReason.String, reasonNoWorker)
	}
	t.Logf("observed offline-mid-roll reason: %s", w.HealthReason.String)
}
