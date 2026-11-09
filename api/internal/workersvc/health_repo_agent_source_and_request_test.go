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

// Issue #4005: a run needing both repo_agent_source_v1 and agent_source_request_v1 is claimable
// only by ONE worker carrying both, so health must diagnose the pair against one worker.
type agentSourcePairHealthStore struct {
	*healthFakeStore
	repo, request, both int64
	bothCalls           int
}

func (s *agentSourcePairHealthStore) CountOnlineWorkersSatisfyingRepoAgentSource(context.Context, uuid.UUID) (int64, error) {
	return s.repo, nil
}

func (s *agentSourcePairHealthStore) CountOnlineWorkersSatisfyingAgentSourceRequest(context.Context, uuid.UUID) (int64, error) {
	return s.request, nil
}

func (s *agentSourcePairHealthStore) CountOnlineWorkersSatisfyingRepoAgentSourceAndRequest(context.Context, uuid.UUID) (int64, error) {
	s.bothCalls++
	return s.both, nil
}

func agentSourcePairHealth(needRepo bool, requested string, repo, request, both int64) (*agentSourcePairHealthStore, *Service, store.ListActiveRunsForHealthRow) {
	r := queuedRunPastThreshold()
	r.RepoAgentSourceRequired = needRepo
	r.AgentSourceRequested = pgtype.Text{String: requested, Valid: requested != ""}
	fs := &agentSourcePairHealthStore{healthFakeStore: &healthFakeStore{
		active:          []store.ListActiveRunsForHealthRow{r},
		onlineWorkers:   2,
		freeSlotWorkers: 2,
	}, repo: repo, request: request, both: both}
	return fs, healthSvc(fs, defaultHealthSettings()), r
}

func TestHealthIssue4005SplitFleetNamesOneWorkerPair(t *testing.T) {
	fs, svc, r := agentSourcePairHealth(true, "repo", 1, 1, 0)
	svc.detectRunHealth(context.Background(), t0)
	w := lastWrite(t, fs.healthFakeStore, r.ID)
	if w.HealthReason.String != reasonNoRepoAgentSourceAndRequestCapableWorker {
		t.Fatalf("reason = %q, want %q", w.HealthReason.String, reasonNoRepoAgentSourceAndRequestCapableWorker)
	}
	for _, c := range []string{capability.RepoAgentSourceV1, capability.AgentSourceRequestV1} {
		if !strings.Contains(w.HealthReason.String, c) {
			t.Fatalf("reason %q must name %s", w.HealthReason.String, c)
		}
	}
}

func TestHealthIssue4005OneWorkerWithBothFallsThrough(t *testing.T) {
	fs, svc, r := agentSourcePairHealth(true, "own", 1, 1, 1)
	svc.detectRunHealth(context.Background(), t0)
	if w := lastWrite(t, fs.healthFakeStore, r.ID); w.HealthReason.String != reasonWaitingWorker {
		t.Fatalf("reason = %q, want %q", w.HealthReason.String, reasonWaitingWorker)
	}
	if fs.bothCalls != 1 {
		t.Fatalf("intersection lookups = %d, want 1", fs.bothCalls)
	}
}

func TestHealthIssue4005SingleRequirementSkipsPairLookup(t *testing.T) {
	for _, tc := range []struct {
		name     string
		needRepo bool
		req      string
	}{{"request only", false, "repo"}, {"repo only", true, ""}} {
		t.Run(tc.name, func(t *testing.T) {
			fs, svc, r := agentSourcePairHealth(tc.needRepo, tc.req, 1, 1, 0)
			svc.detectRunHealth(context.Background(), t0)
			if w := lastWrite(t, fs.healthFakeStore, r.ID); w.HealthReason.String != reasonWaitingWorker {
				t.Fatalf("reason = %q, want %q", w.HealthReason.String, reasonWaitingWorker)
			}
			if fs.bothCalls != 0 {
				t.Fatalf("issued %d intersection lookups for a run needing one capability", fs.bothCalls)
			}
		})
	}
}

func TestHealthIssue4005MissingCapabilityKeepsSpecificReason(t *testing.T) {
	fs, svc, r := agentSourcePairHealth(true, "repo", 0, 1, 0)
	svc.detectRunHealth(context.Background(), t0)
	if w := lastWrite(t, fs.healthFakeStore, r.ID); w.HealthReason.String != reasonNoRepoAgentSourceCapableWorker {
		t.Fatalf("reason = %q, want %q", w.HealthReason.String, reasonNoRepoAgentSourceCapableWorker)
	}
	fs, svc, r = agentSourcePairHealth(true, "repo", 1, 0, 0)
	svc.detectRunHealth(context.Background(), t0)
	if w := lastWrite(t, fs.healthFakeStore, r.ID); w.HealthReason.String != reasonNoAgentSourceRequestCapableWorker {
		t.Fatalf("reason = %q, want %q", w.HealthReason.String, reasonNoAgentSourceRequestCapableWorker)
	}
}
