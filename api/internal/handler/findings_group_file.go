package handler

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	"github.com/vtmocanu/uzi/api/internal/issuedraft"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/termsafe"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

func parseFindingGroupBodyIDs(raw []string) ([]uuid.UUID, bool) {
	if len(raw) < 1 {
		return nil, false
	}
	seen := make(map[uuid.UUID]bool, len(raw))
	ids := make([]uuid.UUID, 0, len(raw))
	for _, s := range raw {
		id, err := uuid.Parse(s)
		if err != nil || id == uuid.Nil {
			return nil, false
		}
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
			if len(ids) > 50 {
				return nil, false
			}
		}
	}
	return ids, true
}

func findingGroupDeadline(now time.Time, stuck, forgeTimeout time.Duration) time.Time {
	duration := 2 * time.Minute
	if stuck > duration {
		duration = stuck
	}
	if forgeTimeout > 0 && 2*forgeTimeout > duration {
		duration = 2 * forgeTimeout
	}
	return now.Add(duration)
}

// composeFiledFindingGroup preserves the server's roster even when the body was edited.
func composeFiledFindingGroup(parts []groupDraftPart, edited *string, operation uuid.UUID) (string, bool) {
	var roster strings.Builder
	roster.WriteString("## Findings\n\n")
	for i, p := range parts {
		roster.WriteString(strconv.Itoa(i + 1))
		roster.WriteString(". ")
		roster.WriteString(issuedraft.SafeInlineCode(p.title))
		roster.WriteString(" — ")
		roster.WriteString(issuedraft.SafeInlineCode(p.location))
		roster.WriteString("\n")
	}
	// The marker is opaque to the forge and remains visible for manual reconciliation.
	marker := "\n\n<!-- uzi-finding-group-operation: " + operation.String() + " -->"
	body := roster.String()
	if edited != nil {
		userText := *edited
		previewRoster := issuedraft.SanitizeFiledBody(roster.String())
		if strings.HasPrefix(userText, previewRoster) {
			userText = strings.TrimPrefix(userText, previewRoster)
		}
		body += "\n## Description\n\n" + termsafe.SanitizeTTY(userText)
	} else {
		_, defaultBody := composeFindingGroupDraft(parts)
		if at := strings.Index(defaultBody, "\n## Evidence\n"); at >= 0 {
			body += defaultBody[at:]
		}
	}
	body = issuedraft.SanitizeFiledBody(body)
	if len(body)+len(marker) > workersvc.MaxIssueDescriptionBytes {
		if len(roster.String())+len(marker) > workersvc.MaxIssueDescriptionBytes {
			return "", false
		}
		body = truncateUTF8(body, workersvc.MaxIssueDescriptionBytes-len(marker))
	}
	return body + marker, true
}

func (h *Handler) FileFindingGroup(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var req apitypes.FindingGroupFileRequest
	if httpx.DecodeJSONStrict(r, &req) != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	ids, ok := parseFindingGroupBodyIDs(req.IDs)
	if !ok {
		httpx.Error(w, http.StatusBadRequest, "invalid ids")
		return
	}
	if req.Description != nil && len(*req.Description) > workersvc.MaxIssueDescriptionBytes {
		httpx.Error(w, http.StatusBadRequest, "description is too large")
		return
	}
	ctx := r.Context()
	deadline := findingGroupDeadline(time.Now(), h.cfg.IssueFilingStuckTimeout, h.cfg.ForgeHTTPTimeout)
	op, members, err := store.ClaimFindingGroup(ctx, h.pool, user.ID, ids, deadline)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrFindingGroupUnavailable):
			httpx.Error(w, http.StatusNotFound, "finding not found")
		case errors.Is(err, store.ErrFindingGroupMixedRepo):
			httpx.Error(w, http.StatusBadRequest, "mixed repositories")
		case errors.Is(err, store.ErrFindingGroupConflict):
			httpx.Error(w, http.StatusConflict, "finding not fileable")
		default:
			slog.Error("file finding group: claim", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	dispositionIDs := make([]string, 0, len(members))
	for _, member := range members {
		dispositionIDs = append(dispositionIDs, member.DispositionID.String())
	}
	release := func() bool { return h.releaseDefinitiveFindingGroup(user.ID, op.ID) }
	parts := make([]groupDraftPart, 0, len(members))
	for _, member := range members {
		finding, e := h.q.GetIncidentalFinding(ctx, store.GetIncidentalFindingParams{ID: member.FindingID, UserID: user.ID})
		if e != nil {
			if !releaseFindingGroupOrReport(w, op.ID, dispositionIDs, "pre_call", release) {
				return
			}
			slog.Error("file finding group: evidence", "error", e)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
		draft := h.buildFindingDraft(ctx, finding)
		parts = append(parts, groupDraftPart{title: draft.Title, location: member.Location, evidence: draft.Description})
	}
	title, _ := composeFindingGroupDraft(parts)
	if req.Title != nil {
		title = issuedraft.SanitizeTitle(termsafe.SanitizeTTY(*req.Title))
	}
	if title == "" {
		if !releaseFindingGroupOrReport(w, op.ID, dispositionIDs, "pre_call", release) {
			return
		}
		httpx.Error(w, http.StatusBadRequest, "title must be non-empty")
		return
	}
	description, fits := composeFiledFindingGroup(parts, req.Description, op.ID)
	if !fits {
		if !releaseFindingGroupOrReport(w, op.ID, dispositionIDs, "pre_call", release) {
			return
		}
		httpx.Error(w, http.StatusBadRequest, "description is too large")
		return
	}
	marker, err := h.settings.FindingLabel(ctx)
	if err != nil {
		if !releaseFindingGroupOrReport(w, op.ID, dispositionIDs, "pre_call", release) {
			return
		}
		slog.Error("file finding group: marker", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	repo, err := h.q.GetRepoForUser(ctx, store.GetRepoForUserParams{ID: op.RepoID, UserID: user.ID})
	if err != nil {
		if !releaseFindingGroupOrReport(w, op.ID, dispositionIDs, "pre_call", release) {
			return
		}
		slog.Error("file finding group: repo", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	f, err := h.svc.ForgeForConnection(repo.ForgeType, repo.BaseUrl, repo.TokenCiphertext)
	if err != nil {
		if !releaseFindingGroupOrReport(w, op.ID, dispositionIDs, "pre_call", release) {
			return
		}
		slog.Error("file finding group: forge connection", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	callCtx, cancel := context.WithDeadline(ctx, op.Deadline)
	defer cancel()
	if callCtx.Err() != nil {
		writeStoppedFindingGroup(w, op.ID, dispositionIDs, "pre_call")
		return
	}
	if err := f.EnsureLabels(callCtx, repo.ForgeProjectID, []forge.Label{{Name: marker}}); err != nil {
		if callCtx.Err() != nil {
			writeStoppedFindingGroup(w, op.ID, dispositionIDs, "pre_call")
			return
		}
		if !releaseFindingGroupOrReport(w, op.ID, dispositionIDs, "pre_call", release) {
			return
		}
		httpx.Error(w, http.StatusBadGateway, "could not ensure the finding label on the forge: "+err.Error())
		return
	}
	if callCtx.Err() != nil {
		writeStoppedFindingGroup(w, op.ID, dispositionIDs, "pre_call")
		return
	}
	begun, err := store.BeginFindingGroupCall(callCtx, h.pool, user.ID, op.ID)
	if err != nil || !begun {
		writeStoppedFindingGroup(w, op.ID, dispositionIDs, "pre_call")
		return
	}
	if callCtx.Err() != nil {
		writeStoppedFindingGroup(w, op.ID, dispositionIDs, "in_flight")
		return
	}
	created, err := f.CreateIssue(callCtx, repo.ForgeProjectID, title, description, assembleFindingLabels(marker, req.Labels))
	if err != nil {
		if forge.IsCreateIssueDefinitiveRejection(err) {
			if !h.releaseDefinitiveFindingGroup(user.ID, op.ID) {
				writeStoppedFindingGroup(w, op.ID, dispositionIDs, "in_flight")
				return
			}
			httpx.Error(w, http.StatusBadGateway, "could not create the issue on the forge: "+err.Error())
			return
		}
		marked, markErr := store.MarkFindingGroupUncertain(context.WithoutCancel(ctx), h.pool, user.ID, op.ID)
		phase := "returned_uncertain"
		if markErr != nil || !marked {
			slog.Error("file finding group: mark uncertain", "error", markErr)
			phase = "in_flight"
		}
		httpx.JSON(w, http.StatusAccepted, apitypes.FindingGroupFileResultDTO{OperationID: op.ID.String(), DispositionIDs: dispositionIDs, Phase: phase, Warning: "Issue creation is uncertain; inspect the forge before releasing this operation."})
		return
	}
	warning := ""
	recorded, recordErr := store.RecordFindingGroupIssue(context.WithoutCancel(ctx), h.pool, user.ID, op.ID, created.IID, created.WebURL)
	if recordErr != nil || !recorded {
		slog.Error("file finding group: record issue", "error", recordErr)
		warning = "The issue was created, but recording it failed. Reconcile this operation manually."
	} else {
		settled, settleErr := store.SettleFindingGroup(context.WithoutCancel(ctx), h.pool, user.ID, op.ID)
		if settleErr != nil || !settled {
			slog.Error("file finding group: settle", "error", settleErr)
			warning = "The issue was created, but settling the findings failed. Reconcile this operation manually."
		}
	}
	phase := "settled"
	if warning != "" {
		phase = "issue_recorded"
		if !recorded {
			phase = "in_flight"
		}
	}
	httpx.JSON(w, http.StatusCreated, apitypes.FindingGroupFileResultDTO{OperationID: op.ID.String(), DispositionIDs: dispositionIDs, Phase: phase, Issue: &apitypes.IncidentalFindingFiledIssueDTO{IID: created.IID, WebURL: created.WebURL, Title: created.Title}, Warning: warning})
}

func writeStoppedFindingGroup(w http.ResponseWriter, operation uuid.UUID, ids []string, phase string) {
	httpx.JSON(w, http.StatusAccepted, apitypes.FindingGroupFileResultDTO{
		OperationID: operation.String(), DispositionIDs: ids, Phase: phase,
		Warning: "Filing stopped before a confirmed issue result. Inspect the forge before releasing this operation after its deadline.",
	})
}

func releaseFindingGroupOrReport(w http.ResponseWriter, operation uuid.UUID, ids []string, phase string, release func() bool) bool {
	if release() {
		return true
	}
	writeStoppedFindingGroup(w, operation, ids, phase)
	return false
}

func (h *Handler) releaseDefinitiveFindingGroup(user, operation uuid.UUID) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ok, err := store.ReleaseFindingGroupDefinitive(ctx, h.pool, user, operation)
	if err != nil || !ok {
		slog.Error("file finding group: definitive release", "operation_id", operation, "error", err)
	}
	return ok && err == nil
}

func (h *Handler) ReleaseFindingGroup(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "operation-id"))
	if err != nil || id == uuid.Nil {
		httpx.Error(w, http.StatusBadRequest, "invalid operation id")
		return
	}
	var req apitypes.FindingGroupReleaseRequest
	if httpx.DecodeJSON(r, &req) != nil || !req.ConfirmedAbsent {
		httpx.Error(w, http.StatusBadRequest, "confirmed_absent must be true")
		return
	}
	_, err = store.GetFindingGroupOperation(r.Context(), h.pool, user.ID, id)
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.Error(w, http.StatusNotFound, "operation not found")
		return
	}
	if err != nil {
		slog.Error("release finding group: lookup", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	released, err := store.ReleaseFindingGroupAfterDeadline(r.Context(), h.pool, user.ID, id)
	if err != nil {
		slog.Error("release finding group: release", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	if !released {
		httpx.Error(w, http.StatusConflict, "operation cannot be released")
		return
	}
	httpx.JSON(w, http.StatusOK, apitypes.FindingGroupReleaseResultDTO{OperationID: id.String(), Phase: "released"})
}
