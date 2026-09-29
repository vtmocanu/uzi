package handler

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	"github.com/vtmocanu/uzi/api/internal/issuedraft"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// GetFindingGroupIssueDraft previews one issue for owned open dispositions in one repo.
func (h *Handler) GetFindingGroupIssueDraft(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	ids, ok := parseFindingGroupIDs(r.URL.Query().Get("ids"))
	if !ok {
		httpx.Error(w, http.StatusBadRequest, "invalid ids")
		return
	}
	ctx := r.Context()
	members, err := store.ReadFindingGroupDraftMembers(ctx, h.pool, user.ID, ids)
	if err != nil {
		slog.Error("finding group draft: read members", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	if len(members) != len(ids) {
		httpx.Error(w, http.StatusNotFound, "finding not found")
		return
	}
	repo := members[0].RepoID
	for _, m := range members {
		if m.RepoID != repo {
			httpx.Error(w, http.StatusBadRequest, "mixed repositories")
			return
		}
	}
	for _, m := range members {
		if m.Status != "open" || m.GroupOperationID.Valid || !m.FindingID.Valid {
			httpx.Error(w, http.StatusConflict, "finding not fileable")
			return
		}
	}
	drafts := make([]groupDraftPart, 0, len(members))
	labels := []string{}
	dispositionIDs := make([]string, 0, len(members))
	for _, m := range members {
		finding, e := h.q.GetIncidentalFinding(ctx, store.GetIncidentalFindingParams{ID: uuid.UUID(m.FindingID.Bytes), UserID: user.ID})
		if e != nil {
			if errors.Is(e, pgx.ErrNoRows) {
				httpx.Error(w, http.StatusConflict, "finding not fileable")
			} else {
				slog.Error("finding group draft: get evidence", "error", e)
				httpx.Error(w, http.StatusInternalServerError, "internal error")
			}
			return
		}
		draft := h.buildFindingDraft(ctx, finding)
		drafts = append(drafts, groupDraftPart{title: draft.Title, location: draft.Location, evidence: draft.Description})
		labels = append(labels, decodeFindingLabels(finding.Labels)...)
		dispositionIDs = append(dispositionIDs, m.DispositionID.String())
	}
	marker, err := h.settings.FindingLabel(ctx)
	if err != nil {
		slog.Error("finding group draft: marker", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	title, description := composeFindingGroupDraft(drafts)
	httpx.JSON(w, http.StatusOK, apitypes.FindingGroupDraftDTO{
		RepoID: repo.String(), DispositionIDs: dispositionIDs, Title: title, Description: description,
		Labels: assembleFindingLabels(marker, labels),
	})
}

func parseFindingGroupIDs(raw string) ([]uuid.UUID, bool) {
	if raw == "" {
		return nil, false
	}
	seen := map[uuid.UUID]bool{}
	ids := []uuid.UUID{}
	for _, token := range strings.Split(raw, ",") {
		id, err := uuid.Parse(strings.TrimSpace(token))
		if err != nil || id == uuid.Nil {
			return nil, false
		}
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
		if len(ids) > 50 {
			return nil, false
		}
	}
	return ids, true
}

type groupDraftPart struct{ title, location, evidence string }

// The mandatory summary precedes evidence so the byte budget cannot hide a member.
func composeFindingGroupDraft(parts []groupDraftPart) (string, string) {
	title := issuedraft.SanitizeTitle("Findings: " + parts[0].title)
	if len(parts) > 1 {
		title = issuedraft.SanitizeTitle("Findings (" + strconv.Itoa(len(parts)) + "): " + parts[0].title)
	}
	var b strings.Builder
	b.WriteString("## Findings\n\n")
	for i, p := range parts {
		b.WriteString(strconv.Itoa(i + 1))
		b.WriteString(". ")
		b.WriteString(p.title)
		b.WriteString(" — ")
		b.WriteString(p.location)
		b.WriteString("\n")
	}
	b.WriteString("\n## Evidence\n\n")
	for i, p := range parts {
		prefix := "### " + strconv.Itoa(i+1) + "\n\n"
		remaining := workersvc.MaxIssueDescriptionBytes - b.Len() - len(prefix) - 2
		if remaining <= 0 {
			break
		}
		b.WriteString(prefix)
		excerpt := p.evidence
		if len(excerpt) > remaining {
			excerpt = truncateUTF8(excerpt, remaining)
		}
		b.WriteString(excerpt)
		b.WriteString("\n\n")
	}
	description := issuedraft.SanitizeFiledBody(b.String())
	if len(description) > workersvc.MaxIssueDescriptionBytes {
		description = truncateUTF8(description, workersvc.MaxIssueDescriptionBytes)
	}
	return title, description
}

func truncateUTF8(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
