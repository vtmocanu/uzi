package handler

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/forge"
	"gitlab.example.com/vtmocanu/uzi/api/internal/forgesvc"
	"gitlab.example.com/vtmocanu/uzi/api/internal/httpx"
	mw "gitlab.example.com/vtmocanu/uzi/api/internal/middleware"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// defaultColumnColor is used when a user adds a brand-new column whose label
// does not yet exist on the forge.
const defaultColumnColor = "#8c8c8c"

// maxBoardColumns caps how many columns a board may have. Each column is an
// EnsureLabels call on the forge and a rendered lane; the cap bounds both the
// request's forge work and the board width.
const maxBoardColumns = 10

// ── DTOs ────────────────────────────────────────────────────────────────────

type columnDTO struct {
	LabelName string `json:"label_name"`
	Position  int    `json:"position"`
}

type cardDTO struct {
	IID        int64    `json:"iid"`
	Title      string   `json:"title"`
	State      string   `json:"state"`
	Labels     []string `json:"labels"`
	WebURL     string   `json:"web_url"`
	Author     *string  `json:"author"`
	HasPRDLink bool     `json:"has_prd_link"`
	// Column is the resolved column label; "" means the implicit Open column.
	// Ignored when Closed is true.
	Column string `json:"column"`
	// Closed places the card in the implicit Closed column (issue state=closed).
	Closed bool `json:"closed"`
	// Conflict flags an issue that arrived carrying more than one column label;
	// it is shown in the highest-positioned one until the next move normalizes it.
	Conflict bool `json:"conflict"`
}

type boardDTO struct {
	RepoID  string      `json:"repo_id"`
	Path    string      `json:"path_with_namespace"`
	WebURL  string      `json:"web_url"`
	Columns []columnDTO `json:"columns"`
	Cards   []cardDTO   `json:"cards"`
}

// ── Board ───────────────────────────────────────────────────────────────────

// GetBoard returns a repo's kanban board: its configured columns plus the
// cached PRD-labeled issues as cards, each resolved to a column. The first time
// a board is opened (no columns yet) it seeds the default columns as labels on
// the forge and imports the current PRD issues — a deliberate, documented side
// effect.
func (h *Handler) GetBoard(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.repoForRequest(w, r)
	if !ok {
		return
	}

	count, err := h.q.CountBoardColumns(r.Context(), repo.ID)
	if err != nil {
		slog.Error("count board columns", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	if count == 0 {
		if !h.seedBoard(w, r, repo) {
			return
		}
	}

	board, ok := h.buildBoard(w, r, repo)
	if !ok {
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"board": board})
}

// seedBoard creates the default column labels on the forge, records them as
// board columns, and imports the current PRD issues. Returns false (after
// writing an error) only when the label-seeding step fails; an import failure
// is logged but non-fatal (the board still renders and Refresh retries).
func (h *Handler) seedBoard(w http.ResponseWriter, r *http.Request, repo store.GetRepoForUserRow) bool {
	f, err := h.svc.ForgeForConnection(repo.ForgeType, repo.BaseUrl, repo.TokenCiphertext)
	if err != nil {
		slog.Error("build forge for connection", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return false
	}
	if err := f.EnsureLabels(r.Context(), repo.ForgeProjectID, forgesvc.DefaultColumns); err != nil {
		httpx.Error(w, http.StatusBadGateway, "could not seed board labels on the forge: "+err.Error())
		return false
	}
	for i, col := range forgesvc.DefaultColumns {
		if err := h.q.InsertBoardColumn(r.Context(), store.InsertBoardColumnParams{
			RepoID:    repo.ID,
			LabelName: col.Name,
			Position:  int32(i),
		}); err != nil {
			slog.Error("insert board column", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return false
		}
	}
	if _, err := h.svc.FullSync(r.Context(), repo.ID, repo.ForgeProjectID, f); err != nil {
		// Non-fatal: the board renders empty and the poller/Refresh will fill it.
		slog.Warn("initial board import failed", "repo", repo.PathWithNamespace, "error", err)
	}
	return true
}

// buildBoard assembles the board response from cached columns and issues.
func (h *Handler) buildBoard(w http.ResponseWriter, r *http.Request, repo store.GetRepoForUserRow) (boardDTO, bool) {
	cols, err := h.q.ListBoardColumns(r.Context(), repo.ID)
	if err != nil {
		slog.Error("list board columns", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return boardDTO{}, false
	}
	issues, err := h.q.ListIssuesByRepo(r.Context(), repo.ID)
	if err != nil {
		slog.Error("list issues", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return boardDTO{}, false
	}

	columns := make([]columnDTO, 0, len(cols))
	position := make(map[string]int, len(cols))
	for _, c := range cols {
		columns = append(columns, columnDTO{LabelName: c.LabelName, Position: int(c.Position)})
		position[c.LabelName] = int(c.Position)
	}

	cards := make([]cardDTO, 0, len(issues))
	for _, is := range issues {
		var labels []string
		if err := json.Unmarshal(is.Labels, &labels); err != nil {
			labels = []string{}
		}
		col, closed, conflict := resolveColumn(labels, is.State, position)
		card := cardDTO{
			IID:        is.ForgeIssueIid,
			Title:      is.Title,
			State:      is.State,
			Labels:     labels,
			WebURL:     is.WebUrl,
			HasPRDLink: is.HasPrdLink,
			Column:     col,
			Closed:     closed,
			Conflict:   conflict,
		}
		if is.Author.Valid {
			a := is.Author.String
			card.Author = &a
		}
		cards = append(cards, card)
	}

	return boardDTO{
		RepoID:  repo.ID.String(),
		Path:    repo.PathWithNamespace,
		WebURL:  repo.WebUrl,
		Columns: columns,
		Cards:   cards,
	}, true
}

// resolveColumn decides which column a card belongs to. Closed issues go to the
// implicit Closed column regardless of labels. Otherwise the card sits in its
// single column label, or Open ("") if it has none. An issue carrying more than
// one column label is shown in the highest-positioned one and flagged as a
// conflict until the next move normalizes it.
func resolveColumn(labels []string, state string, position map[string]int) (column string, closed bool, conflict bool) {
	if state == "closed" {
		return "", true, false
	}
	best := ""
	bestPos := -1
	matches := 0
	for _, l := range labels {
		pos, isCol := position[l]
		if !isCol {
			continue
		}
		matches++
		if pos > bestPos {
			bestPos = pos
			best = l
		}
	}
	return best, false, matches > 1
}

type configureColumnsRequest struct {
	Columns []struct {
		LabelName string `json:"label_name"`
	} `json:"columns"`
}

// ConfigureColumns replaces a repo's column set. Positions are taken from array
// order. Each column name is ensured to exist as a label on the forge (so a
// newly typed column becomes a real, addressable label), then the board_columns
// rows are replaced wholesale.
func (h *Handler) ConfigureColumns(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.repoForRequest(w, r)
	if !ok {
		return
	}
	var req configureColumnsRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// Normalize: trim, drop blanks, dedupe preserving order.
	seen := map[string]struct{}{}
	var names []string
	for _, c := range req.Columns {
		name := strings.TrimSpace(c.LabelName)
		if name == "" {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	if len(names) > maxBoardColumns {
		httpx.Error(w, http.StatusBadRequest, "too many columns (max "+strconv.Itoa(maxBoardColumns)+")")
		return
	}

	f, err := h.svc.ForgeForConnection(repo.ForgeType, repo.BaseUrl, repo.TokenCiphertext)
	if err != nil {
		slog.Error("build forge for connection", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	if len(names) > 0 {
		labels := make([]forge.Label, len(names))
		for i, n := range names {
			labels[i] = forge.Label{Name: n, Color: defaultColumnColor}
		}
		if err := f.EnsureLabels(r.Context(), repo.ForgeProjectID, labels); err != nil {
			httpx.Error(w, http.StatusBadGateway, "could not ensure column labels on the forge: "+err.Error())
			return
		}
	}

	if err := h.q.DeleteBoardColumnsByRepo(r.Context(), repo.ID); err != nil {
		slog.Error("delete board columns", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	for i, n := range names {
		if err := h.q.InsertBoardColumn(r.Context(), store.InsertBoardColumnParams{
			RepoID:    repo.ID,
			LabelName: n,
			Position:  int32(i),
		}); err != nil {
			slog.Error("insert board column", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
	}

	board, ok := h.buildBoard(w, r, repo)
	if !ok {
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"board": board})
}

// ── Move ────────────────────────────────────────────────────────────────────

type moveIssueRequest struct {
	ToColumn string `json:"to_column"`
}

// MoveIssue moves a card to a column by writing the label change to the forge
// FIRST, then updating the cache. Single-column enforcement: the target column
// label is added and every other column label removed in one atomic call.
// Moving to Open ("" / "open") removes all column labels; moving to Closed is
// not supported in the MVP (close/reopen stays on the forge).
func (h *Handler) MoveIssue(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.repoForRequest(w, r)
	if !ok {
		return
	}
	iid, err := parseInt64(chi.URLParam(r, "iid"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid issue id")
		return
	}
	var req moveIssueRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	target := strings.TrimSpace(req.ToColumn)
	if strings.EqualFold(target, "open") {
		target = ""
	}
	if strings.EqualFold(target, "closed") {
		httpx.Error(w, http.StatusBadRequest, "moving to the Closed column is not supported; close the issue on the forge")
		return
	}

	cols, err := h.q.ListBoardColumns(r.Context(), repo.ID)
	if err != nil {
		slog.Error("list board columns", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	columnSet := make(map[string]struct{}, len(cols))
	for _, c := range cols {
		columnSet[c.LabelName] = struct{}{}
	}
	if target != "" {
		if _, ok := columnSet[target]; !ok {
			httpx.Error(w, http.StatusBadRequest, "target column is not configured for this repo")
			return
		}
	}

	issue, err := h.q.GetIssueByIID(r.Context(), store.GetIssueByIIDParams{RepoID: repo.ID, ForgeIssueIid: iid})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.Error(w, http.StatusNotFound, "issue not found")
			return
		}
		slog.Error("get issue", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	var labels []string
	if err := json.Unmarshal(issue.Labels, &labels); err != nil {
		labels = []string{}
	}

	add, remove, newLabels := planLabelMove(labels, columnSet, target)

	f, err := h.svc.ForgeForConnection(repo.ForgeType, repo.BaseUrl, repo.TokenCiphertext)
	if err != nil {
		slog.Error("build forge for connection", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	// Forge-first: apply the label change remotely before touching the cache.
	// On failure the cache is untouched and the client snaps the card back.
	if err := f.UpdateIssueLabels(r.Context(), repo.ForgeProjectID, iid, add, remove); err != nil {
		httpx.Error(w, http.StatusBadGateway, "could not update the issue on the forge: "+err.Error())
		return
	}

	labelsJSON, err := json.Marshal(newLabels)
	if err != nil {
		slog.Error("marshal labels", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	updated, err := h.q.UpsertIssue(r.Context(), store.UpsertIssueParams{
		RepoID:         repo.ID,
		ForgeIssueIid:  issue.ForgeIssueIid,
		Title:          issue.Title,
		State:          issue.State,
		Labels:         labelsJSON,
		WebUrl:         issue.WebUrl,
		Author:         issue.Author,
		HasPrdLink:     issue.HasPrdLink,
		ForgeUpdatedAt: issue.ForgeUpdatedAt,
	})
	if err != nil {
		slog.Error("upsert issue after move", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}

	position := make(map[string]int, len(cols))
	for _, c := range cols {
		position[c.LabelName] = int(c.Position)
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"card": issueToCard(updated, position)})
}

// planLabelMove computes the atomic add/remove sets for a single-column move and
// the resulting label set for the cache. add is the target column (if not
// already present); remove is every OTHER column label the issue currently
// carries. Moving to Open (target == "") removes all column labels and adds none.
func planLabelMove(current []string, columnSet map[string]struct{}, target string) (add, remove, newLabels []string) {
	has := map[string]struct{}{}
	for _, l := range current {
		has[l] = struct{}{}
	}
	// remove: current column labels that are not the target.
	for _, l := range current {
		if _, isCol := columnSet[l]; isCol && l != target {
			remove = append(remove, l)
		}
	}
	// add: the target, if set and not already present.
	if target != "" {
		if _, present := has[target]; !present {
			add = append(add, target)
		}
	}
	// newLabels: current minus removed, plus target.
	removeSet := map[string]struct{}{}
	for _, l := range remove {
		removeSet[l] = struct{}{}
	}
	for _, l := range current {
		if _, dropped := removeSet[l]; dropped {
			continue
		}
		newLabels = append(newLabels, l)
	}
	if target != "" {
		if _, present := has[target]; !present {
			newLabels = append(newLabels, target)
		}
	}
	return add, remove, newLabels
}

// issueToCard resolves a cached issue row into a card DTO.
func issueToCard(is store.Issue, position map[string]int) cardDTO {
	var labels []string
	if err := json.Unmarshal(is.Labels, &labels); err != nil {
		labels = []string{}
	}
	col, closed, conflict := resolveColumn(labels, is.State, position)
	card := cardDTO{
		IID:        is.ForgeIssueIid,
		Title:      is.Title,
		State:      is.State,
		Labels:     labels,
		WebURL:     is.WebUrl,
		HasPRDLink: is.HasPrdLink,
		Column:     col,
		Closed:     closed,
		Conflict:   conflict,
	}
	if is.Author.Valid {
		a := is.Author.String
		card.Author = &a
	}
	return card
}

// ── Manual sync ─────────────────────────────────────────────────────────────

// SyncRepo runs an on-demand full sync (the Refresh button): fetch the complete
// PRD set, upsert, and evict anything gone forge-side. Returns the refreshed
// board so the client updates in one round-trip.
func (h *Handler) SyncRepo(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.repoForRequest(w, r)
	if !ok {
		return
	}
	f, err := h.svc.ForgeForConnection(repo.ForgeType, repo.BaseUrl, repo.TokenCiphertext)
	if err != nil {
		slog.Error("build forge for connection", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	if _, err := h.svc.FullSync(r.Context(), repo.ID, repo.ForgeProjectID, f); err != nil {
		httpx.Error(w, http.StatusBadGateway, "sync failed: "+err.Error())
		return
	}
	board, ok := h.buildBoard(w, r, repo)
	if !ok {
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"board": board})
}

// ── Helpers ─────────────────────────────────────────────────────────────────

// repoForRequest loads the repo named by the {id} path param, authorized to the
// current user, writing the appropriate error response on any failure.
func (h *Handler) repoForRequest(w http.ResponseWriter, r *http.Request) (store.GetRepoForUserRow, bool) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return store.GetRepoForUserRow{}, false
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid repo id")
		return store.GetRepoForUserRow{}, false
	}
	repo, err := h.q.GetRepoForUser(r.Context(), store.GetRepoForUserParams{ID: id, UserID: user.ID})
	if err != nil {
		httpx.Error(w, http.StatusNotFound, "repo not found")
		return store.GetRepoForUserRow{}, false
	}
	return repo, true
}

// pgtypeTextOrNull maps an empty string to SQL NULL, a non-empty one to a valid
// text value.
func pgtypeTextOrNull(s string) pgtype.Text {
	if s == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: s, Valid: true}
}

// parseInt64 parses a base-10 int64 path param (issue IID).
func parseInt64(s string) (int64, error) {
	return strconv.ParseInt(s, 10, 64)
}
