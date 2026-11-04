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

func TestMaterializeRequestedSelection(t *testing.T) {
	roster := []byte(`[{"name":"site-plan-check","description":"d"}]`)
	none := []byte(`[]`)
	requested := func(v string) pgtype.Text { return pgtype.Text{String: v, Valid: v != ""} }
	explicit := &AgentSelection{Source: AgentSourceRepo, Exclusions: []string{"site-plan-check"}}
	for _, tc := range []struct {
		name      string
		requested string
		agents    []byte
		sel       *AgentSelection
		want      string
	}{
		{"unset leaves a bare approval bare", "", roster, nil, ""},
		{"unset with no roster stays bare", "", nil, nil, ""},
		{"own requested", "own", roster, nil, AgentSourceOwn},
		{"repo requested with a roster", "repo", roster, nil, AgentSourceRepo},
		{"repo requested with an empty roster degrades", "repo", none, nil, AgentSourceOwn},
		{"repo requested with no reported roster degrades", "repo", nil, nil, AgentSourceOwn},
		{"own requested without a roster", "own", nil, nil, AgentSourceOwn},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := store.Run{AgentSourceRequested: requested(tc.requested), RepoAgents: tc.agents}
			got := materializeRequestedSelection(run, tc.sel)
			if tc.want == "" {
				if got != nil {
					t.Fatalf("got %+v, want no selection", *got)
				}
				return
			}
			if got == nil || got.Source != tc.want || got.Exclusions == nil || len(got.Exclusions) != 0 {
				t.Fatalf("got %+v, want {Source:%s Exclusions:[]}", got, tc.want)
			}
		})
	}
	t.Run("an explicit selection wins", func(t *testing.T) {
		for _, req := range []string{"", "own", "repo"} {
			run := store.Run{AgentSourceRequested: requested(req), RepoAgents: roster}
			if got := materializeRequestedSelection(run, explicit); got != explicit {
				t.Fatalf("requested %q: got %+v, want the explicit selection unchanged", req, got)
			}
		}
	})
}

type agentSourceHealthStore struct {
	*healthFakeStore
	capable int64
	calls   []uuid.UUID
}

func (s *agentSourceHealthStore) CountOnlineWorkersSatisfyingAgentSourceRequest(_ context.Context, userID uuid.UUID) (int64, error) {
	s.calls = append(s.calls, userID)
	return s.capable, nil
}

func agentSourceHealthSvc(requested string, capable int64) (*agentSourceHealthStore, *Service, store.ListActiveRunsForHealthRow) {
	r := queuedRunPastThreshold()
	r.AgentSourceRequested = pgtype.Text{String: requested, Valid: requested != ""}
	fs := &agentSourceHealthStore{healthFakeStore: &healthFakeStore{
		active:          []store.ListActiveRunsForHealthRow{r},
		onlineWorkers:   1,
		freeSlotWorkers: 1,
	}, capable: capable}
	return fs, healthSvc(fs, defaultHealthSettings()), r
}

func TestHealthQueuedRequestedRunNamesMissingWorkerProtocol(t *testing.T) {
	fs, svc, r := agentSourceHealthSvc("own", 0)
	svc.detectRunHealth(context.Background(), t0)
	w := lastWrite(t, fs.healthFakeStore, r.ID)
	if w.HealthReason.String != reasonNoAgentSourceRequestCapableWorker {
		t.Fatalf("reason = %q, want %q", w.HealthReason.String, reasonNoAgentSourceRequestCapableWorker)
	}
	if !strings.Contains(w.HealthReason.String, capability.AgentSourceRequestV1) {
		t.Fatalf("reason %q must name %s", w.HealthReason.String, capability.AgentSourceRequestV1)
	}
	if len(fs.calls) != 1 || fs.calls[0] != r.UserID {
		t.Fatalf("lookups = %v, want one for %s", fs.calls, r.UserID)
	}
}

func TestHealthQueuedRequestedRunWithCapableWorkerFallsThrough(t *testing.T) {
	fs, svc, r := agentSourceHealthSvc("repo", 1)
	svc.detectRunHealth(context.Background(), t0)
	if w := lastWrite(t, fs.healthFakeStore, r.ID); w.HealthReason.String != reasonWaitingWorker {
		t.Fatalf("reason = %q, want %q", w.HealthReason.String, reasonWaitingWorker)
	}
}

func TestHealthQueuedUnsetRunNeverAgentSourceReason(t *testing.T) {
	fs, svc, r := agentSourceHealthSvc("", 0)
	svc.detectRunHealth(context.Background(), t0)
	if w := lastWrite(t, fs.healthFakeStore, r.ID); w.HealthReason.String == reasonNoAgentSourceRequestCapableWorker {
		t.Fatal("an unset run must not report the agent-source protocol reason")
	}
	if len(fs.calls) != 0 {
		t.Fatalf("issued %d lookups for an unset run", len(fs.calls))
	}
}
