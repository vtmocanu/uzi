package workersvc

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

const reasonAssignedRepoAgentSourceGap = "assigned worker lacks repo_agent_source_v1 required by this continuation; upgrade the assigned worker or wait for a compatible worker"
const sourcePeerUnavailableSuffix = "; compatible worker currently unavailable"
const heldRepoAgentSourceGapSuffix = "; assigned worker also lacks repo_agent_source_v1 required by this continuation"

// assignedRepoAgentSourceGap reads the assignment independently of owner-pin projections.
// A failed lookup or an incomplete fake cannot establish a capability gap.
func (s *Service) assignedRepoAgentSourceGap(ctx context.Context, r store.ListActiveRunsForHealthRow) (store.Worker, bool) {
	if r.Status != "queued" || r.Kind == "cross_check" || r.EgressProfileID.Valid || !r.WorkerID.Valid || r.ClaimGeneration <= 0 ||
		(!r.RepoAgentSourceRequired && !r.RepoAgentSourceCommit.Valid) {
		return store.Worker{}, false
	}
	id := uuid.UUID(r.WorkerID.Bytes)
	if id == uuid.Nil || r.UserID == uuid.Nil {
		return store.Worker{}, false
	}
	w, err := s.q.GetWorkerByIDForUser(ctx, store.GetWorkerByIDForUserParams{ID: id, UserID: r.UserID})
	if err == nil && w.ID == id && w.UserID == r.UserID && !slices.Contains(w.ProtocolCapabilities, capability.RepoAgentSourceV1) {
		return w, true
	}
	return store.Worker{}, false
}

// liveAssignedPin reports the instant ClaimRun's affinity arm stops pinning this run to a live
// assigned worker that lacks the capability. It is true only while the pin really holds:
// StaleRequeueWorkerName is projected non-empty only when fn_forge_origin_owner_pins holds, the
// worker's last heartbeat is inside the stale window, and now is not past updated_at plus the
// affinity ceiling (inclusive, like staleRequeuePin).
func (s *Service) liveAssignedPin(now time.Time, r store.ListActiveRunsForHealthRow, w store.Worker) (time.Time, bool) {
	if r.StaleRequeueWorkerName == "" || !r.UpdatedAt.Valid || !w.LastHeartbeatAt.Valid ||
		w.LastHeartbeatAt.Time.Before(now.Add(-s.p.WorkerHeartbeatStale)) {
		return time.Time{}, false
	}
	ceilingEnd := r.UpdatedAt.Time.Add(s.p.WorkerAffinityCeiling)
	return ceilingEnd, !now.After(ceilingEnd)
}

func withAssignedRepoAgentSourceGap(reason string, gap bool) string {
	if gap {
		return reason + "; " + reasonAssignedRepoAgentSourceGap
	}
	return reason
}

func withHeldRepoAgentSourceGap(reason string, gap bool) string {
	if !gap {
		return reason
	}
	// Preserve the deadline-leading hold shape without predicting post-hold placement.
	return strings.TrimSuffix(reason, "; another worker may take it after that") + heldRepoAgentSourceGapSuffix
}
