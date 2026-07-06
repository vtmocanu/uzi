package handler

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// pipelineDTO is the CI-badge payload (PRD #6) a repo row, board header, or card
// carries: the raw GitLab pipeline status (the web layer maps it to a tone), the
// pipeline's web URL, its id, and when uzi last synced it (badge staleness). A
// null pipeline on a DTO means "no CI, or not yet synced" for that ref.
type pipelineDTO struct {
	// Ref is the watched ref this pipeline is for (default branch or an agent
	// branch) — what the Fix CI trigger POSTs to fix this pipeline (PRD #6).
	Ref        string    `json:"ref"`
	Status     string    `json:"status"`
	WebURL     string    `json:"web_url"`
	PipelineID int64     `json:"pipeline_id"`
	SyncedAt   time.Time `json:"synced_at"`
}

// pipelineFromStatus maps a full cache row to the DTO.
func pipelineFromStatus(s store.PipelineStatus) *pipelineDTO {
	return &pipelineDTO{Ref: s.Ref, Status: s.Status, WebURL: s.WebUrl, PipelineID: s.PipelineID, SyncedAt: s.SyncedAt.Time}
}

// pipelineDTOFrom maps the projected columns the list/board queries return (they
// select only the display fields, not the whole row) to the DTO.
func pipelineDTOFrom(ref, status, webURL string, pipelineID int64, syncedAt pgtype.Timestamptz) *pipelineDTO {
	return &pipelineDTO{Ref: ref, Status: status, WebURL: webURL, PipelineID: pipelineID, SyncedAt: syncedAt.Time}
}

// defaultBranchPipelines returns default-branch CI badges keyed by repo id for the
// given repos (PRD #6 repos + projects lists). A repo with no cached default-branch
// pipeline is simply absent from the map, so its DTO field stays null. The query
// error is returned for the caller to log; badges are non-essential, so callers
// render the list regardless.
func (h *Handler) defaultBranchPipelines(ctx context.Context, repoIDs []uuid.UUID) (map[uuid.UUID]*pipelineDTO, error) {
	out := make(map[uuid.UUID]*pipelineDTO, len(repoIDs))
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
