package workersvc

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type repoAgentSourceHealthStore struct {
	*healthFakeStore
	capable int64
	calls   []uuid.UUID
}

func (f *repoAgentSourceHealthStore) CountOnlineWorkersSatisfyingRepoAgentSource(_ context.Context, userID uuid.UUID) (int64, error) {
	f.calls = append(f.calls, userID)
	return f.capable, nil
}

func repoAgentSourceHealth(pinned bool, capable int64) (*repoAgentSourceHealthStore, *Service, store.ListActiveRunsForHealthRow) {
	r := queuedRunPastThreshold()
	if pinned {
		r.RepoAgentSourceCommit = pgtype.Text{String: "1111111111111111111111111111111111111111", Valid: true}
	}
	fs := &repoAgentSourceHealthStore{
		healthFakeStore: &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}, onlineWorkers: 1, freeSlotWorkers: 1},
		capable:         capable,
	}
	return fs, healthSvc(fs, defaultHealthSettings()), r
}

func TestHealthQueuedPinnedRunWithoutCapableWorkerNamesCapability(t *testing.T) {
	fs, svc, r := repoAgentSourceHealth(true, 0)
	svc.detectRunHealth(context.Background(), t0)
	w := lastWrite(t, fs.healthFakeStore, r.ID)
	if w.HealthReason.String != reasonNoRepoAgentSourceCapableWorker || !strings.Contains(w.HealthReason.String, capability.RepoAgentSourceV1) {
		t.Fatalf("reason = %q, want %q", w.HealthReason.String, reasonNoRepoAgentSourceCapableWorker)
	}
}

func TestHealthQueuedPinnedRunWithCapableWorkerFallsThrough(t *testing.T) {
	fs, svc, r := repoAgentSourceHealth(true, 1)
	svc.detectRunHealth(context.Background(), t0)
	if w := lastWrite(t, fs.healthFakeStore, r.ID); w.HealthReason.String != reasonWaitingWorker {
		t.Fatalf("reason = %q, want %q", w.HealthReason.String, reasonWaitingWorker)
	}
}

func TestHealthQueuedUnpinnedRunSkipsRepoAgentSourceLookup(t *testing.T) {
	fs, svc, _ := repoAgentSourceHealth(false, 0)
	svc.detectRunHealth(context.Background(), t0)
	if len(fs.calls) != 0 {
		t.Fatalf("issued %d repo agent source lookups for an unpinned run", len(fs.calls))
	}
}

func TestHealthQueuedStampedUnpinnedRunWithoutCapableWorkerNamesCapability(t *testing.T) {
	fs, svc, r := repoAgentSourceHealth(false, 0)
	r.RepoAgentSourceRequired = true
	fs.active = []store.ListActiveRunsForHealthRow{r}
	svc.detectRunHealth(context.Background(), t0)
	if w := lastWrite(t, fs.healthFakeStore, r.ID); w.HealthReason.String != reasonNoRepoAgentSourceCapableWorker {
		t.Fatalf("reason = %q, want %q", w.HealthReason.String, reasonNoRepoAgentSourceCapableWorker)
	}
}
