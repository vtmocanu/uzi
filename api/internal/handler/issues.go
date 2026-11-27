package handler

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/board"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forgesvc"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// maxIssueTitleBytes bounds a created issue's title (GitLab caps titles at 255).
const maxIssueTitleBytes = 255

type createIssueRequest struct {
	Title       string `json:"title"`
	Description string `json:"description"`
}

// CreateIssue opens a new uzi-labelled issue on the forge for the given repo and
// caches it so the board reflects it immediately. The forge is the source of
// truth (the next sync reconciles it); this never fabricates a local-only card —
// the issue returned here is the real one the forge just created. The user must
// own the repo (enforced by repoForRequest).
func (h *Handler) CreateIssue(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.repoForRequest(w, r)
	if !ok {
		return
	}
	// PRD 4173 D13: refused before the forge write, so nothing is created that the
	// issue-number guard would then strand.
	if refuseNeverEnabled(w, repo.AwaitingFirstEnable, repo.IssueSourceLocked) {
		return
	}
	var req createIssueRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	title := strings.TrimSpace(req.Title)
	if title == "" || len(title) > maxIssueTitleBytes {
		httpx.Error(w, http.StatusBadRequest, "title must be non-empty and at most 255 characters")
		return
	}
	if len(req.Description) > workersvc.MaxIssueDescriptionBytes {
		httpx.Error(w, http.StatusBadRequest, "description is too large")
		return
	}

	tr, err := h.issueTrackerForRepo(r.Context(), repo.UserID, repo.ID, repo.IssueTrackerConnectionID, repo.ForgeType, repo.BaseUrl, repo.TokenCiphertext)
	if err != nil {
		slog.Error("build forge for connection", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	// The created issue carries the configured uzi run-eligibility label (PRD #764), on a
	// linked repo the link's stored eligibility name (PRD #4173 D14); refused with a 500
	// before the write when the label settings cannot be read.
	labels, err := h.createLabels(r.Context(), repo)
	if err != nil {
		slog.Error("create issue: read the label settings", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	created, err := tr.CreateIssue(r.Context(), repo.ForgeProjectID, title, req.Description, labels)
	if err != nil {
		// err is already PAT-redacted by the driver.
		if writeIssueWriteNotOffered(w, err) {
			return
		}
		httpx.Error(w, http.StatusBadGateway, "could not create the issue on the forge: "+err.Error())
		return
	}

	// Cache the real, just-created issue (same write the sync path performs) so
	// the card appears without waiting for the next poll. has_prd_link is computed
	// from the description exactly as sync does.
	labelsJSON, err := json.Marshal(created.Labels)
	if err != nil {
		slog.Error("marshal issue labels", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	assigneeIDsJSON, err := json.Marshal(created.Assignees)
	if err != nil {
		slog.Error("marshal issue assignee ids", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	updated := created.UpdatedAt
	if updated.IsZero() {
		updated = time.Now()
	}
	upserted, err := h.q.UpsertIssue(r.Context(), store.UpsertIssueParams{
		RepoID:         repo.ID,
		ForgeIssueIid:  created.IID,
		Title:          created.Title,
		State:          created.State,
		Labels:         labelsJSON,
		AssigneeIds:    assigneeIDsJSON,
		WebUrl:         created.WebURL,
		Author:         pgconv.TextOrNull(created.Author),
		HasPrdLink:     forgesvc.HasPRDLink(req.Description),
		ForgeUpdatedAt: pgtype.Timestamptz{Time: updated, Valid: true},
		IssueKey:       pgtype.Text{}, // only the linked sync writes issue_key (D10)
	})
	if err != nil {
		// The issue exists on the forge; only the local cache write failed. The
		// next sync will pick it up, so report success with the forge facts.
		slog.Warn("cache new issue after create", "repo", repo.PathWithNamespace, "error", err)
		httpx.JSON(w, http.StatusCreated, map[string]any{
			// nonNilLabels, not created.Labels: this is the THIRD card builder (the two
			// in board.go go through decodeLabels) and it hands the forge's slice
			// straight to the DTO, where a nil marshals as JSON null.
			"card": cardDTO{IID: created.IID, Title: created.Title, State: created.State, Labels: nonNilLabels(created.Labels), AssigneeIds: nonNilAssigneeIDs(created.Assignees), WebURL: created.WebURL, ForgeType: repo.ForgeType, HasPRDLink: forgesvc.HasPRDLink(req.Description), IssueKey: linkedIssueKey(repo, created.IID)},
		})
		return
	}

	cols, err := h.q.ListBoardColumns(r.Context(), repo.ID)
	if err != nil {
		slog.Error("list board columns", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	position := make(map[string]int, len(cols))
	for _, c := range cols {
		position[c.LabelName] = int(c.Position)
	}
	card := issueToCard(upserted, position, repo.ForgeType, repo.IssueTrackerConnectionID.Valid)
	if k := linkedIssueKey(repo, card.IID); k != "" {
		card.IssueKey = k
	}
	httpx.JSON(w, http.StatusCreated, map[string]any{"card": card})
}

// issueDetailDTO is the in-app issue view payload (PRD #12 §3): the board card
// fields plus the issue Description. The description is fetched live from the
// forge and never cached (it carries the PRD link and is the basis for deciding
// to run); the SPA renders it as markdown.
type issueDetailDTO struct {
	IID    int64    `json:"iid"`
	Title  string   `json:"title"`
	State  string   `json:"state"`
	Labels []string `json:"labels"`
	// AssigneeIds are the forge user ids assigned to the issue (PRD #767 M5), fresh from
	// the live forge fetch. The issue view widens "runnable" to "carries `uzi` OR the
	// repo's bot is one of these assignees", mirroring the board card.
	AssigneeIds []int64 `json:"assignee_ids"`
	WebURL      string  `json:"web_url"`
	Author      *string `json:"author"`
	HasPRDLink  bool    `json:"has_prd_link"`
	Column      string  `json:"column"`
	Closed      bool    `json:"closed"`
	Conflict    bool    `json:"conflict"`
	Description string  `json:"description"`
	// ForgeType is the issue's forge ("gitlab"|"forgejo"|"github"), so the issue view's
	// "Open on <forge>" button names the right platform (PRD #65 D2). From
	// repo.ForgeType (the issue's repo has one connection), not the live forge fetch.
	ForgeType string `json:"forge_type"`
	// BotForgeUserID is the repo's connection's bot forge user id (PRD #767 M5), so the
	// issue view can evaluate assignment-eligibility with the SAME predicate the board
	// uses. Per-connection, carried at issue-detail level, not the user session.
	BotForgeUserID int64 `json:"bot_forge_user_id"`
	// IssueKey and EligibilityLabel are set on a repo linked to an issue tracker only
	// (PRD #4173 M6): the derived key ("ABC-12") and the stored eligibility tag name an
	// issue must carry to be uzi's. Both are omitted on an unlinked repo.
	IssueKey         string `json:"issue_key,omitempty"`
	EligibilityLabel string `json:"eligibility_label,omitempty"`
}

// applyLinkedIssueDetail adds what a linked repo's issue page carries beyond the forge
// shape: the derived key, the stored eligibility name, and the column from the cached
// State column (column "" for an issue the cache does not hold, the Open lane). Labels
// never decide the column there, because a tag the account owns can be named like a
// column (PRD #4173 D9, M6). A repo with no link is left exactly as buildIssueDetail made it.
func applyLinkedIssueDetail(dto *issueDetailDTO, repo store.GetRepoForUserRow, issue forge.Issue, cachedColumn string, position map[string]int) {
	if !repo.IssueTrackerConnectionID.Valid {
		return
	}
	dto.IssueKey = linkedIssueKey(repo, issue.IID)
	dto.EligibilityLabel = linkedEligibilityLabel(repo)
	dto.Column, dto.Closed, dto.Conflict = board.ResolveColumn(board.CardColumnLabels(true, issue.Labels, cachedColumn), issue.State, position)
}

// buildIssueDetail assembles the issue-view payload from a freshly-fetched forge
// issue and the repo's column position map, resolving the card's column and
// computing has_prd_link the same way the board and sync paths do. Pure (no DB or
// forge I/O) so the resolution is unit-tested directly — the handler itself can't
// be, since h.q is a concrete *store.Queries.
func buildIssueDetail(issue forge.Issue, position map[string]int, forgeType string, botForgeUserID int64) issueDetailDTO {
	col, closed, conflict := board.ResolveColumn(issue.Labels, issue.State, position)
	// nonNilLabels, not a local nil check: this is the same JSON-null guarantee the
	// two card builders make, and three hand-rolled copies is how one of them ends up
	// missing it. The guard here predates the helper (PRD #102 M6) — it was already
	// right, and now it is right for a reason a reader can follow to the others.
	labels := nonNilLabels(issue.Labels)
	dto := issueDetailDTO{
		IID:         issue.IID,
		Title:       issue.Title,
		State:       issue.State,
		Labels:      labels,
		AssigneeIds: nonNilAssigneeIDs(issue.Assignees),
		WebURL:      issue.WebURL,
		HasPRDLink:  forgesvc.HasPRDLink(issue.Description),
		Column:      col,
		Closed:      closed,
		Conflict:    conflict,
		Description: issue.Description,
		ForgeType:   forgeType,
		// Fresh from the forge fetch (issue.Assignees) plus the connection's bot id, so
		// the issue view evaluates the same "uzi OR assigned-to-bot" predicate as the board.
		BotForgeUserID: botForgeUserID,
	}
	if issue.Author != "" {
		a := issue.Author
		dto.Author = &a
	}
	return dto
}

// GetIssueDetail returns one issue's card fields plus its Description, read live
// from the forge via the connection PAT (the description is never cached). Powers
// the in-app issue view (PRD #12 §3); the run history is a separate ListRuns call
// with the ?repo_id=&issue_iid= filters. Owner-scoped like the rest of the board
// endpoints (repoForRequest authorizes the repo to the current user).
func (h *Handler) GetIssueDetail(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.repoForRequest(w, r)
	if !ok {
		return
	}
	// PRD 4173 D13: a re-listed project whose issue source was an issue tracker reads no forge
	// issue by number (the forge's numbers are not the ones its history used).
	if refuseNeverEnabled(w, repo.AwaitingFirstEnable, repo.IssueSourceLocked) {
		return
	}
	iid, err := parseInt64(chi.URLParam(r, "iid"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid issue id")
		return
	}
	tr, err := h.issueTrackerForRepo(r.Context(), repo.UserID, repo.ID, repo.IssueTrackerConnectionID, repo.ForgeType, repo.BaseUrl, repo.TokenCiphertext)
	if err != nil {
		slog.Error("build forge for connection", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	issue, err := tr.GetIssue(r.Context(), repo.ForgeProjectID, iid)
	if err != nil {
		// err is already PAT-redacted by the driver.
		msg := "could not read the issue from the forge: "
		if repo.IssueTrackerConnectionID.Valid {
			msg = "could not read the issue from the issue tracker: "
		}
		httpx.Error(w, http.StatusBadGateway, msg+err.Error())
		return
	}
	cols, err := h.q.ListBoardColumns(r.Context(), repo.ID)
	if err != nil {
		slog.Error("list board columns", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	position := make(map[string]int, len(cols))
	for _, c := range cols {
		position[c.LabelName] = int(c.Position)
	}
	dto := buildIssueDetail(issue, position, repo.ForgeType, repo.BotForgeUserID)
	if repo.IssueTrackerConnectionID.Valid {
		// An issue the cache does not hold has no cached column: the Open lane.
		var cachedColumn string
		cached, err := h.q.GetIssueByIID(r.Context(), store.GetIssueByIIDParams{RepoID: repo.ID, ForgeIssueIid: iid})
		switch {
		case err == nil:
			cachedColumn = cached.IssueTrackerColumn.String
		case !errors.Is(err, pgx.ErrNoRows):
			slog.Error("issue detail: read cached issue", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
		applyLinkedIssueDetail(&dto, repo, issue, cachedColumn, position)
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"issue": dto})
}
