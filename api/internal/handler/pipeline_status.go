package handler

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/apitypes"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// pipelineDTO (apitypes.PipelineDTO) moved to the stdlib-only apitypes leaf
// (PRD #64 M1); the mappers below stay here.

// pipelineFromStatus maps a full cache row to the DTO.
func pipelineFromStatus(s store.PipelineStatus) *apitypes.PipelineDTO {
	return &apitypes.PipelineDTO{Ref: s.Ref, Status: s.Status, WebURL: s.WebUrl, PipelineID: s.PipelineID, SyncedAt: s.SyncedAt.Time}
}

// pipelineDTOFrom maps the projected columns the list/board queries return (they
// select only the display fields, not the whole row) to the DTO.
func pipelineDTOFrom(ref, status, webURL string, pipelineID int64, syncedAt pgtype.Timestamptz) *apitypes.PipelineDTO {
	return &apitypes.PipelineDTO{Ref: ref, Status: status, WebURL: webURL, PipelineID: pipelineID, SyncedAt: syncedAt.Time}
}

// defaultBranchPipelines returns default-branch CI badges keyed by repo id for the
// given repos (PRD #6 repos + projects lists). A repo with no cached default-branch
// pipeline is simply absent from the map, so its DTO field stays null. The query
// error is returned for the caller to log; badges are non-essential, so callers
// render the list regardless.
func (h *Handler) defaultBranchPipelines(ctx context.Context, repoIDs []uuid.UUID) (map[uuid.UUID]*apitypes.PipelineDTO, error) {
	out := make(map[uuid.UUID]*apitypes.PipelineDTO, len(repoIDs))
	if len(repoIDs) == 0 {
		return out, nil
	}
	rows, err := h.q.ListDefaultBranchPipelineStatuses(ctx, repoIDs)
	if err != nil {
		return out, err
	}
	for _, r := range rows {
		out[r.RepoID] = pipelineDTOFrom(r.Ref, r.Status, r.WebUrl, r.PipelineID, r.SyncedAt)
	}
	return out, nil
}
