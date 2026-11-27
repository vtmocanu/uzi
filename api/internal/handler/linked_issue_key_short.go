package handler

import (
	"context"
	"log/slog"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/issuetracker"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// issueKeyFromShort is the issue tracker key ("ABC-12") of issue iid under a project short
// name, "" when the short name is empty (an unlinked repo). It is the payload helper for the
// places that hold the short name but no full repo row: the run, schedule, finding and
// created-issue payloads (PRD #4173 M6). The format matches linkedIssueKey.
func issueKeyFromShort(short string, iid int64) string {
	if short == "" {
		return ""
	}
	return issuetracker.IssueKey(short, iid)
}

// stampRunIssueKey sets dto.IssueKey from the stored project short name of the run's repo.
// It leaves the key empty for a run with no issue and for an unlinked repo.
func stampRunIssueKey(dto *apitypes.RunDTO, short string) {
	if dto.IssueIID != nil {
		dto.IssueKey = issueKeyFromShort(short, *dto.IssueIID)
	}
}

// linkedProjectShorts reads the project short name of each linked repo among ids in one
// query. An unlinked or unknown repo is absent from the map. It is best-effort: a read
// failure is logged and yields an empty map, so a payload goes out without a key rather than
// failing an otherwise good response.
func (h *Handler) linkedProjectShorts(ctx context.Context, ids ...uuid.UUID) map[uuid.UUID]string {
	out := make(map[uuid.UUID]string, len(ids))
	if h.q == nil || len(ids) == 0 {
		return out
	}
	rows, err := h.q.ListLinkedIssueTrackerProjectShorts(ctx, ids)
	if err != nil {
		slog.Error("resolve linked issue tracker project short names", "error", err)
		return out
	}
	for _, row := range rows {
		out[row.ID] = row.ProjectShort
	}
	return out
}

// runDTO is runToDTO for a single-run response, with the issue key of a run on a linked repo.
// runToDTO stays pure, so the project short name is read here, once, and only for a run that
// has both a repo and an issue. The list handlers take it from their list query instead.
func (h *Handler) runDTO(ctx context.Context, run store.Run) apitypes.RunDTO {
	dto := runToDTO(run, h.runPriorityClass(ctx, run), h.cfg.RunTimeout, h.runExtensionCapSeconds(ctx), h.cfg.RunForgeUnreachableMaxParks, h.clock(), h.cfg.RunMaxRequeues)
	if run.IssueIid.Valid && run.RepoID.Valid {
		repoID := uuid.UUID(run.RepoID.Bytes)
		stampRunIssueKey(&dto, h.linkedProjectShorts(ctx, repoID)[repoID])
	}
	return dto
}

// stampFindingIssueKeys sets filed_issue_key on each finding that carries a filed issue, from
// the stored project short name of its repo, read once for all the distinct repos. A finding
// of an unlinked repo, and one with no filed issue, keeps no key.
func (h *Handler) stampFindingIssueKeys(ctx context.Context, findings []apitypes.IncidentalFindingDTO) {
	var repoIDs []uuid.UUID
	seen := make(map[uuid.UUID]bool)
	for _, f := range findings {
		id, err := uuid.Parse(f.RepoID)
		if f.FiledIssueIID == nil || err != nil || seen[id] {
			continue
		}
		seen[id] = true
		repoIDs = append(repoIDs, id)
	}
	if len(repoIDs) == 0 {
		return
	}
	shorts := h.linkedProjectShorts(ctx, repoIDs...)
	for i := range findings {
		if id, err := uuid.Parse(findings[i].RepoID); err == nil && findings[i].FiledIssueIID != nil {
			findings[i].FiledIssueKey = issueKeyFromShort(shorts[id], *findings[i].FiledIssueIID)
		}
	}
}

// findingDispositionDTOWithKey is findingDispositionDTO plus the filed issue key, for the
// single-finding responses.
func (h *Handler) findingDispositionDTOWithKey(ctx context.Context, d store.FindingDisposition) apitypes.IncidentalFindingDTO {
	one := []apitypes.IncidentalFindingDTO{findingDispositionDTO(d)}
	h.stampFindingIssueKeys(ctx, one)
	return one[0]
}
