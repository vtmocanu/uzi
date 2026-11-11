package workersvc

import (
	"context"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

const reasonAssignedRepoAgentSourceGap = "assigned worker lacks repo_agent_source_v1 required by this continuation; upgrade the assigned worker or wait for a compatible worker"
const heldRepoAgentSourceGapSuffix = "; assigned worker also lacks repo_agent_source_v1 required by this continuation"

// assignedRepoAgentSourceGap reads the assignment independently of owner-pin projections.
// A failed lookup or an incomplete fake cannot establish a capability gap.
func (s *Service) assignedRepoAgentSourceGap(ctx context.Context, r store.ListActiveRunsForHealthRow) bool {
	if r.Status != "queued" || r.Kind == "cross_check" || r.EgressProfileID.Valid || !r.WorkerID.Valid || r.ClaimGeneration <= 0 ||
		(!r.RepoAgentSourceRequired && !r.RepoAgentSourceCommit.Valid) {
		return false
	}
	id := uuid.UUID(r.WorkerID.Bytes)
	if id == uuid.Nil || r.UserID == uuid.Nil {
		return false
	}
	w, err := s.q.GetWorkerByIDForUser(ctx, store.GetWorkerByIDForUserParams{ID: id, UserID: r.UserID})
	return err == nil && w.ID == id && w.UserID == r.UserID && !slices.Contains(w.ProtocolCapabilities, capability.RepoAgentSourceV1)
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
