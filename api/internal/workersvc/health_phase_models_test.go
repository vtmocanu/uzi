package workersvc

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

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

// phaseHealthStore captures the phase/custom count inputs; unrelated Codex rungs default to available.
type phaseHealthStore struct {
	*healthFakeStore
	count        int64
	calls        int
	phaseParams  []store.CountOnlineWorkersSatisfyingPhaseModelsParams
	customParams []store.CountOnlineWorkersSatisfyingCustomCodexParams
	customCount  *int64
}

func (f *phaseHealthStore) CountOnlineWorkersSatisfyingPhaseModels(_ context.Context, arg store.CountOnlineWorkersSatisfyingPhaseModelsParams) (int64, error) {
	f.calls++
	f.phaseParams = append(f.phaseParams, arg)
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

func (f *phaseHealthStore) CountOnlineWorkersSatisfyingCodexHarness(context.Context, uuid.UUID) (int64, error) {
	return 1, nil
}

func (f *phaseHealthStore) CountOnlineWorkersSatisfyingCodexRuntime(context.Context, uuid.UUID) (int64, error) {
	return 1, nil
}

func (f *phaseHealthStore) CountOnlineWorkersSatisfyingCustomCodex(_ context.Context, arg store.CountOnlineWorkersSatisfyingCustomCodexParams) (int64, error) {
	f.customParams = append(f.customParams, arg)
	if f.customCount != nil {
		return *f.customCount, nil
	}
	return 1, nil
}

func TestHealthCodexPlanningCountParameters(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		custom                  bool
		phaseCount, customCount int64
		want                    string
	}{
		{"phase unavailable", false, 0, 1, reasonNoPhaseModelsCapableWorker},
		{"phase available", false, 1, 1, reasonWaitingWorker},
		{"custom and phase combined unavailable", true, 1, 0, reasonNoCustomCodexCapableWorker},
		{"custom and phase combined available", true, 1, 1, reasonWaitingWorker},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := runRow("queued")
			r.Kind, r.Harness = "issue", "codex"
			r.StatusSince = ago(15 * time.Minute)
			r.PhaseModelsRequired, r.CodexCustomRoot = true, tc.custom
			fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}, onlineWorkers: 1, freeSlotWorkers: 1}
			q := &phaseHealthStore{healthFakeStore: fs, count: tc.phaseCount, customCount: &tc.customCount}
			svc := healthSvc(fs, defaultHealthSettings())
			svc.q = q
			svc.detectRunHealth(context.Background(), t0)
			got := lastWrite(t, fs, r.ID)
			if got.Health != healthWaitingWorker || got.HealthReason.String != tc.want {
				t.Fatalf("health=%s reason=%q want %q", got.Health, got.HealthReason.String, tc.want)
			}
			if tc.custom {
				if len(q.customParams) != 1 || q.customParams[0].UserID != r.UserID || !q.customParams[0].PhaseModelsRequired {
					t.Fatalf("custom count params=%v, want owner with PhaseModelsRequired=true", q.customParams)
				}
			} else if len(q.customParams) != 0 {
				t.Fatalf("non-custom run issued custom count: %v", q.customParams)
			}
			if tc.custom && tc.customCount == 0 {
				if q.calls != 0 {
					t.Fatalf("custom failure should short circuit phase rung: calls=%d", q.calls)
				}
			} else if len(q.phaseParams) != 1 || q.phaseParams[0].Harness != "codex" || q.phaseParams[0].UserID != r.UserID {
				t.Fatalf("phase params=%v, want owner with Harness=codex", q.phaseParams)
			}
		})
	}
	if reasonNoCustomCodexCapableWorker != "no worker supporting custom Codex models is online" {
		t.Fatalf("custom reason text drifted: %q", reasonNoCustomCodexCapableWorker)
	}
}

func TestHealthCodexPlanningCrossCheckSameWorker(t *testing.T) {
	worker := func(caps ...string) store.ListWorkersByUserRow {
		return store.ListWorkersByUserRow{Status: "online", ProtocolCapabilities: codexPlanningCaps(caps...)}
	}
	all := worker(capability.CrossCheckV1, capability.CodexPhaseModelsV1, capability.CodexCustomModelV1)
	for _, tc := range []struct {
		name    string
		custom  bool
		workers []store.ListWorkersByUserRow
		want    string
	}{
		{"Codex phase accepted", false, []store.ListWorkersByUserRow{worker(capability.CrossCheckV1, capability.CodexPhaseModelsV1)}, reasonWaitingWorker},
		{"Claude phase rejected", false, []store.ListWorkersByUserRow{worker(capability.CrossCheckV1, capability.ClaudePhaseModelsV1)}, reasonNoCrossCheckCapableWorker},
		{"phase cross-check split", false, []store.ListWorkersByUserRow{worker(capability.CrossCheckV1), worker(capability.CodexPhaseModelsV1)}, reasonNoCrossCheckCapableWorker},
		{"three way split", true, []store.ListWorkersByUserRow{worker(capability.CrossCheckV1), worker(capability.CodexPhaseModelsV1), worker(capability.CodexCustomModelV1)}, reasonNoCrossCheckCapableWorker},
		{"custom phase together without cross-check", true, []store.ListWorkersByUserRow{worker(capability.CodexPhaseModelsV1, capability.CodexCustomModelV1), worker(capability.CrossCheckV1)}, reasonNoCrossCheckCapableWorker},
		{"cross-check custom with Claude phase", true, []store.ListWorkersByUserRow{worker(capability.CrossCheckV1, capability.CodexCustomModelV1, capability.ClaudePhaseModelsV1)}, reasonNoCrossCheckCapableWorker},
		{"all on one worker", true, []store.ListWorkersByUserRow{all}, reasonWaitingWorker},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := runRow("queued")
			r.Kind, r.Harness = "issue", "codex"
			r.StatusSince = ago(15 * time.Minute)
			r.PhaseModelsRequired, r.CodexCustomRoot = true, tc.custom
			fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}, onlineWorkers: 1, freeSlotWorkers: 1,
				crossCheckRun:     store.Run{ID: r.ID, Kind: "issue", Harness: "codex", PlanCrossCheckRequired: true},
				crossCheckWorkers: tc.workers}
			// Both count rungs succeed, forcing the detector to inspect each worker.
			q := &phaseHealthStore{healthFakeStore: fs, count: 1}
			svc := healthSvc(fs, defaultHealthSettings())
			svc.q = q
			svc.detectRunHealth(context.Background(), t0)

			got := lastWrite(t, fs, r.ID)
			if got.HealthReason.String != tc.want {
				t.Fatalf("reason=%q want %q", got.HealthReason.String, tc.want)
			}
			if len(fs.crossCheckWorkerCalls) != 1 {
				t.Fatalf("worker fleet reads=%v, want one", fs.crossCheckWorkerCalls)
			}
		})
	}
}

func TestHealthCodexPlanningDockerProbeSameWorker(t *testing.T) {
	for _, tc := range []struct {
		name       string
		workers    [][]string
		wantDocker bool
	}{
		{"Codex phase", [][]string{{capability.CrossCheckV1, capability.CodexPhaseModelsV1, capability.CodexCustomModelV1}}, true},
		{"Claude phase", [][]string{{capability.CrossCheckV1, capability.ClaudePhaseModelsV1, capability.CodexCustomModelV1}}, false},
		{"split fleet", [][]string{{capability.CrossCheckV1}, {capability.CodexPhaseModelsV1}, {capability.CodexCustomModelV1}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := runRow("queued")
			r.Kind, r.Harness = "issue", "codex"
			r.StatusSince = ago(time.Minute) // young enough to use the gated Docker probe
			r.RepoID = pgtype.UUID{Bytes: uuid.New(), Valid: true}
			r.PhaseModelsRequired, r.CodexCustomRoot = true, true
			workers := make([]store.ListWorkersByUserRow, 0, len(tc.workers))
			for _, caps := range tc.workers {
				workers = append(workers, store.ListWorkersByUserRow{Status: "online", ProtocolCapabilities: codexPlanningCaps(caps...)})
			}
			fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}, onlineWorkers: 1,
				crossCheckRun:     store.Run{ID: r.ID, Kind: "issue", Harness: "codex", PlanCrossCheckRequired: true},
				crossCheckWorkers: workers}
			q := &phaseHealthStore{healthFakeStore: fs, count: 1}
			svc := healthSvc(fs, defaultHealthSettings())
			svc.q = q
			svc.SetDockerAllowlist(fakeAllowlistReader{})
			svc.detectRunHealth(context.Background(), t0)
			if tc.wantDocker {
				got := lastWrite(t, fs, r.ID)
				if got.Health != healthWaitingWorker || got.HealthReason.String != reasonRepoNotDockerAllowedMember {
					t.Fatalf("health=%s reason=%q, want Docker allowlist reason", got.Health, got.HealthReason.String)
				}
			} else {
				for _, got := range fs.writes {
					if got.ID == r.ID && got.Health != healthOK {
						t.Fatalf("incapable fleet reached Docker reason: health=%s reason=%q", got.Health, got.HealthReason.String)
					}
				}
			}
			if len(q.phaseParams) != 1 || q.phaseParams[0].Harness != "codex" ||
				len(q.customParams) != 1 || !q.customParams[0].PhaseModelsRequired || len(fs.crossCheckWorkerCalls) != 1 {
				t.Fatalf("probe did not check Codex planning conjunction: phase=%v custom=%v fleet=%v", q.phaseParams, q.customParams, fs.crossCheckWorkerCalls)
			}
		})
	}
}
