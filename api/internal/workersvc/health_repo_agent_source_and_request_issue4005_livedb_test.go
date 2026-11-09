package workersvc

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4005 LiveDB coverage: ClaimRun applies repo_agent_source_v1 and agent_source_request_v1
// to ONE worker, so the queued-reason resolver must too. Drives the real Service.queuedReason and
// ClaimRun over a throwaway Postgres (skipped unless UZI_TEST_DATABASE_URL). Reuses the
// interlockLiveDB harness; the run carries no completion contract, so the completion rung stays out.

const (
	issue4005RepoCap    = "repo_agent_source_v1"
	issue4005RequestCap = "agent_source_request_v1"
)

// seedAgentSourceQueuedRun seeds a queued issue run that needs repo agent sources (needRepo) and/or
// a requested agent source (requested non-empty), and returns the matching health row.
func (e interlockLiveDB) seedAgentSourceQueuedRun(t *testing.T, needRepo bool, requested string) (uuid.UUID, store.ListActiveRunsForHealthRow) {
	t.Helper()
	runID := e.seedQueuedRun(t, nil, nil)
	var req any
	if requested != "" {
		req = requested
	}
	// runs_agent_source_requested_pair_check: the basis is set exactly when the request is.
	e.exec(t, `UPDATE runs SET repo_agent_source_required = $2, agent_source_requested = $3::text,
	           agent_source_requested_basis = CASE WHEN $3::text IS NULL THEN NULL ELSE 'repo_default' END
	           WHERE id = $1`, runID, needRepo, req)
	return runID, store.ListActiveRunsForHealthRow{
		ID:                      runID,
		UserID:                  e.userID,
		Status:                  "queued",
		Health:                  healthOK,
		RepoAgentSourceRequired: needRepo,
		AgentSourceRequested:    pgtype.Text{String: requested, Valid: requested != ""},
	}
}

func TestQueuedReasonIssue4005SplitFleetNamesOneWorkerPairLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	repoOnly := e.seedWorker(t, []string{issue4005RepoCap})
	requestOnly := e.seedWorker(t, []string{issue4005RequestCap})
	_, row := e.seedAgentSourceQueuedRun(t, true, "repo")

	got := svc.queuedReason(e.ctx, time.Now(), row, thWithLimit(testCustodyLimit))
	if got != reasonNoRepoAgentSourceAndRequestCapableWorker {
		t.Fatalf("queuedReason = %q, want %q: each capability is online, but on different workers",
			got, reasonNoRepoAgentSourceAndRequestCapableWorker)
	}
	// The diagnosis agrees with the claim: neither split worker can take the run.
	for _, w := range []struct {
		id   uuid.UUID
		caps []string
	}{{repoOnly, []string{issue4005RepoCap}}, {requestOnly, []string{issue4005RequestCap}}} {
		if _, err := e.q.ClaimRun(e.ctx, e.claimParams(w.id, w.caps, false)); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("ClaimRun by worker with %v: err = %v, want no rows", w.caps, err)
		}
	}
}

func TestQueuedReasonIssue4005OneWorkerWithBothFallsThroughLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	both := []string{issue4005RepoCap, issue4005RequestCap}
	worker := e.seedWorker(t, both)
	runID, row := e.seedAgentSourceQueuedRun(t, true, "own")

	if got := svc.queuedReason(e.ctx, time.Now(), row, thWithLimit(testCustodyLimit)); got != reasonWaitingWorker {
		t.Fatalf("queuedReason = %q, want %q when one worker carries both capabilities", got, reasonWaitingWorker)
	}
	// Positive control: that worker can claim the run.
	run, err := e.q.ClaimRun(e.ctx, e.claimParams(worker, both, false))
	if err != nil || run.ID != runID {
		t.Fatalf("ClaimRun by the worker with both: run %v err %v, want %v", run.ID, err, runID)
	}
}

func TestQueuedReasonIssue4005SingleRequirementKeepsSpecificReasonLiveDB(t *testing.T) {
	t.Run("repo source only", func(t *testing.T) {
		e := setupInterlockLiveDB(t)
		svc := e.permitService(t)
		e.seedWorker(t, []string{issue4005RequestCap})
		_, row := e.seedAgentSourceQueuedRun(t, true, "")
		if got := svc.queuedReason(e.ctx, time.Now(), row, thWithLimit(testCustodyLimit)); got != reasonNoRepoAgentSourceCapableWorker {
			t.Fatalf("queuedReason = %q, want %q", got, reasonNoRepoAgentSourceCapableWorker)
		}
	})
	t.Run("request only", func(t *testing.T) {
		e := setupInterlockLiveDB(t)
		svc := e.permitService(t)
		e.seedWorker(t, []string{issue4005RepoCap})
		_, row := e.seedAgentSourceQueuedRun(t, false, "repo")
		if got := svc.queuedReason(e.ctx, time.Now(), row, thWithLimit(testCustodyLimit)); got != reasonNoAgentSourceRequestCapableWorker {
			t.Fatalf("queuedReason = %q, want %q", got, reasonNoAgentSourceRequestCapableWorker)
		}
	})
}

// A run needing both, on a fleet with no repo_agent_source_v1 worker at all, keeps the specific
// single-capability reason: the single rungs run before the intersection rung.
func TestQueuedReasonIssue4005MissingCapabilityPrecedesPairLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	e.seedWorker(t, []string{issue4005RequestCap})
	_, row := e.seedAgentSourceQueuedRun(t, true, "repo")
	if got := svc.queuedReason(e.ctx, time.Now(), row, thWithLimit(testCustodyLimit)); got != reasonNoRepoAgentSourceCapableWorker {
		t.Fatalf("queuedReason = %q, want %q", got, reasonNoRepoAgentSourceCapableWorker)
	}
}
