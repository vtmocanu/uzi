package handler

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/schedsvc"
	"github.com/vtmocanu/uzi/api/internal/schedtmpl"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// schedules_dto.go holds the RunSchedule -> ScheduleDTO mapper and its repo-path
// helper (PRD #1022 file split).

// repoPathFor resolves a schedule's repo path best-effort (owner-scoped), returning ""
// when the repo is gone or no longer owned so the DTO can still render.
func (h *Handler) repoPathFor(ctx context.Context, s store.RunSchedule) string {
	repo, err := h.q.GetRepoForUser(ctx, store.GetRepoForUserParams{ID: s.RepoID, UserID: s.UserID})
	if err != nil {
		return ""
	}
	return repo.PathWithNamespace
}

// scheduleDTO builds the response view, computing the next-fires preview (up to 3) for
// a recurring schedule from the same cron logic the modal preview uses.
func (h *Handler) scheduleDTO(s store.RunSchedule, repoPath string) apitypes.ScheduleDTO {
	dto := apitypes.ScheduleDTO{
		ID:              s.ID.String(),
		RepoID:          s.RepoID.String(),
		RepoPath:        repoPath,
		Target:          s.Target,
		Labels:          scheduleLabelsToSlice(s.Labels),
		Prompt:          s.Prompt.String,
		Timing:          s.Timing,
		CronExpr:        s.CronExpr.String,
		Timezone:        s.Timezone,
		AutoApprove:     s.AutoApprove,
		WaitOnLimit:     s.WaitOnLimit,
		MrReworkEnabled: boolPtrValue(s.MrReworkEnabled),
		Enabled:         s.Enabled,
		Status:          s.Status,
		Origin:          s.Origin,
		Customized:      s.Customized,
		CreatedAt:       s.CreatedAt.Time,
		UpdatedAt:       s.UpdatedAt.Time,
	}
	if s.CatalogSlug.Valid && s.CatalogSlug.String != "" {
		v := s.CatalogSlug.String
		dto.CatalogSlug = &v
	}
	// sibling_group_id (PRD #636): a display-only group tag, surfaced as a uuid string only
	// when the row is grouped (nil for a standalone row).
	if s.SiblingGroupID.Valid {
		v := uuid.UUID(s.SiblingGroupID.Bytes).String()
		dto.SiblingGroupID = &v
	}
	// Default-origin resolution (PRD #589 Decision 2): a default row stores NULL
	// prompt/labels/guidance and carries them in the builtin catalog, resolved by
	// catalog_slug. Surface the RESOLVED values here (never persisted) so the modal can show
	// the read-only baked prompt / selector. A gone slug leaves the (empty) columns as-is.
	if s.Origin == "default" && s.CatalogSlug.Valid {
		if job, ok := schedtmpl.BySlug(s.CatalogSlug.String); ok {
			dto.Prompt = job.Prompt
			dto.Labels = job.Labels
			g := job.Guidance
			// Owner-guidance overlay (issue #675): for a SWEEP default the catalog guidance is
			// the BAKED value shown read-only; Guidance is reserved for the owner overlay,
			// populated below from the stored column. For a prompt/other default the baked value
			// still travels through Guidance as before.
			if s.Target == "sweep" {
				dto.BakedGuidance = &g
			} else {
				dto.Guidance = &g
			}
		}
	}
	if s.IssueIid.Valid {
		v := s.IssueIid.Int64
		dto.IssueIID = &v
	}
	if s.RunAt.Valid {
		t := s.RunAt.Time
		dto.RunAt = &t
	}
	if s.NextFireAt.Valid {
		t := s.NextFireAt.Time
		dto.NextFireAt = &t
	}
	if s.LastFiredAt.Valid {
		t := s.LastFiredAt.Time
		dto.LastFiredAt = &t
	}
	if s.MaxIssues.Valid {
		v := int(s.MaxIssues.Int32)
		dto.MaxIssues = &v
	}
	if s.Guidance.Valid && s.Guidance.String != "" {
		v := s.Guidance.String
		dto.Guidance = &v
	}
	if s.Model.Valid && s.Model.String != "" {
		v := s.Model.String
		dto.Model = &v
	}
	// override_subagent_model is a plain bool column (never NULL, PRD #305), so always set it.
	ov := s.OverrideSubagentModel
	dto.OverrideSubagentModel = &ov
	// last_fire is the persisted jsonb summary of the most recent fire (PRD #308 M3). NULL
	// or empty ⇒ never fired, leave nil. A malformed payload is logged and left nil — a
	// bad summary must never fail the whole DTO.
	if len(s.LastFire) > 0 {
		var lf apitypes.LastFire
		if err := json.Unmarshal(s.LastFire, &lf); err != nil {
			slog.Error("unmarshal schedule last_fire", "schedule", s.ID.String(), "error", err)
		} else {
			dto.LastFire = &lf
		}
	}
	if s.Timing == "recurring" && s.CronExpr.Valid {
		if fires, err := schedsvc.NextFires(s.CronExpr.String, s.Timezone, h.clock(), 3); err == nil {
			dto.NextFires = fires
		}
	}
	return dto
}
