package workersvc

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestHealthPhaseModelsCrossCheckSameWorker pins that a plan-cross-check-required Claude lead run
// needing claude_phase_models_v1 is servable only when ONE worker advertises both the cross-check
// capability and claude_phase_models_v1; a split fleet is reported as having no capable worker.
func TestHealthPhaseModelsCrossCheckSameWorker(t *testing.T) {
	crossOnly := store.ListWorkersByUserRow{Status: "online", ProtocolCapabilities: []string{capability.CrossCheckV1}}
	phaseOnly := store.ListWorkersByUserRow{Status: "online", ProtocolCapabilities: []string{capability.ClaudePhaseModelsV1}}
	both := store.ListWorkersByUserRow{Status: "online", ProtocolCapabilities: []string{capability.CrossCheckV1, capability.ClaudePhaseModelsV1}}
	for _, tc := range []struct {
		name    string
		workers []store.ListWorkersByUserRow
		want    string
	}{
		{"split fleet", []store.ListWorkersByUserRow{crossOnly, phaseOnly}, reasonNoCrossCheckCapableWorker},
		{"one worker has both", []store.ListWorkersByUserRow{crossOnly, both}, reasonWaitingWorker},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := runRow("queued")
			r.Kind = "issue"
			r.StatusSince = ago(15 * time.Minute)
			r.PhaseModelsRequired = true
			fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}, onlineWorkers: 1, freeSlotWorkers: 1,
				crossCheckRun:     store.Run{ID: r.ID, Kind: "issue", Harness: "claude", PlanCrossCheckRequired: true},
				crossCheckWorkers: tc.workers}
			svc := healthSvc(fs, defaultHealthSettings())
			// The count rung sees a phase-models-capable worker (B), so only the per-worker
			// conjunction in the cross-check loop can catch the split.
			svc.q = &phaseHealthStore{healthFakeStore: fs, count: 1}
			svc.detectRunHealth(context.Background(), t0)
			got := lastWrite(t, fs, r.ID)
			if got.HealthReason.String != tc.want {
				t.Fatalf("reason = %q, want %q", got.HealthReason.String, tc.want)
			}
		})
	}
}

// phaseHealthStore answers CountOnlineWorkersSatisfyingPhaseModels from a canned count.
type phaseHealthStore struct {
	*healthFakeStore
	count int64
	calls int
}

func (f *phaseHealthStore) CountOnlineWorkersSatisfyingPhaseModels(context.Context, uuid.UUID) (int64, error) {
	f.calls++
	return f.count, nil
}

// TestHealthQueuedReasonNoPhaseModelsCapableWorker pins PRD #3252's queued-reason rung: a queued
// Claude run that needs claude_phase_models_v1 is labelled when the owner has no online worker
// advertising it, and only then.
func TestHealthQueuedReasonNoPhaseModelsCapableWorker(t *testing.T) {
	for _, tc := range []struct {
		name     string
		required bool
		count    int64
		want     bool
		wantCall bool
	}{
		{"required, no capable worker", true, 0, true, true},
		{"required, a capable worker is online", true, 1, false, true},
		{"not required, no capable worker", false, 0, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := runRow("queued")
			r.Kind = "issue"
			r.StatusSince = ago(15 * time.Minute)
			r.PhaseModelsRequired = tc.required
			fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}, onlineWorkers: 1, freeSlotWorkers: 1}
			q := &phaseHealthStore{healthFakeStore: fs, count: tc.count}
			svc := healthSvc(fs, defaultHealthSettings())
			svc.q = q
			svc.detectRunHealth(context.Background(), t0)
			got := lastWrite(t, fs, r.ID).HealthReason.String
			if (got == reasonNoPhaseModelsCapableWorker) != tc.want {
				t.Fatalf("reason = %q, want the phase-models reason = %v", got, tc.want)
			}
			if (q.calls > 0) != tc.wantCall {
				t.Fatalf("satisfier count called %d times, want a call = %v", q.calls, tc.wantCall)
			}
		})
	}
	if reasonNoPhaseModelsCapableWorker != "no worker supporting separate planning settings is online" {
		t.Fatalf("reason text drifted: %q", reasonNoPhaseModelsCapableWorker)
	}
}
