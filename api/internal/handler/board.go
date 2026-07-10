package handler

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/board"
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
	// LatestRun is the newest run for this issue, or null when it has never run.
	// It carries only display state (no secrets); the run-view link is owner-only,
	// so IsMine tells the client whether the viewer may open it.
	LatestRun *latestRunDTO `json:"latest_run"`
	// Pipeline is the CI status of the card's MOST RECENT run's branch (PRD #6),
	// null when that run has no branch, no CI, or the card has never run. It is what
	// renders the per-card badge and gates the "Fix CI" affordance.
	Pipeline *pipelineDTO `json:"pipeline"`
}

// latestRunDTO is the run summary a card carries (PRD #12 M2), so the board needs
// no second listRuns fan-in. OwnerName drives the "started by X" treatment;
// IsMine gates the run-view link (a non-owner would 403 on GetRunByIDForUser).
type latestRunDTO struct {
	ID            string    `json:"id"`
	Status        string    `json:"status"`
	MrIID         *int64    `json:"mr_iid"`
	// MrState is the PRD #24 watcher's last-observed merge-request state, null when
	// never observed. Display-only (PRD #33 Decision 1): the board card's chip
	// renders merged/closed distinctly and everything else as the plain open chip.
	// Kept fresh by the watcher only for the board card (the issue's latest run).
	MrState       *string   `json:"mr_state"`
	// FailureReason is OWNER-ONLY (PRD #33 Decision 5): it can carry a user's verbatim
	// typed reject reason or a raw agent error, so a shared board must not expose it
	// to non-owner viewers — they get null. Owners keep it (the failed-badge tooltip).
	// The non-sensitive StopKind enum below stays visible to everyone for the
	// stopped-vs-failed badge, so classification never depends on this field.
	FailureReason *string   `json:"failure_reason"`
	// StopKind is the server-stamped deliberate-stop signal (PRD #33): "cancelled"
	// or "plan_rejected", null otherwise. The board badge reads it (not the
	// failure_reason text) to render a deliberate stop as calm "stopped".
	StopKind      *string   `json:"stop_kind"`
	OwnerName     string    `json:"owner_name"`
	WorkerName    *string   `json:"worker_name"`
	IsMine        bool      `json:"is_mine"`
	// RunCount is how many runs the issue has had (this run being the newest). >1
	// drives the board's "×N" retry hint; full history lives in the issue view.
	RunCount      int64     `json:"run_count"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// mapLatestRun builds the card's run summary from the shared run + owner + worker
// columns the board and single-card queries both return. viewerID is the board
// viewer; IsMine is set when the run belongs to them (only then does the client
// render the run-view link). ownerName is the owner's display name or empty — never
// the email (PRD #33 Decision 5): a shared board must not leak another user's email
// on a card, and the web already renders a no-owner badge for empty. The query no
// longer even selects the email, so there is nothing to fall back to here.
func mapLatestRun(runID, ownerID uuid.UUID, status string, mrIID pgtype.Int8, mrState, failureReason, stopKind, ownerName, workerName pgtype.Text, runCount int64, createdAt, updatedAt pgtype.Timestamptz, viewerID uuid.UUID) *latestRunDTO {
	dto := &latestRunDTO{
		ID:         runID.String(),
		Status:     status,
		MrState:    textPtrValue(mrState.Valid, mrState.String),
		StopKind:   textPtrValue(stopKind.Valid, stopKind.String),
		WorkerName: textPtrValue(workerName.Valid, workerName.String),
		IsMine:     ownerID == viewerID,
		RunCount:   runCount,
		CreatedAt:  createdAt.Time,
		UpdatedAt:  updatedAt.Time,
	}
	// failure_reason is owner-only (Decision 5): it can carry a verbatim reject reason
	// or a raw agent error, so a non-owner viewer of a shared board gets null. stop_kind
	// (above, unconditional) already gives everyone the stopped-vs-failed classification.
	if dto.IsMine {
		dto.FailureReason = textPtrValue(failureReason.Valid, failureReason.String)
	}
	if mrIID.Valid {
		v := mrIID.Int64
		dto.MrIID = &v
	}
	// Display name or empty — never the email (Decision 5). An empty owner_name is a
	// legal, rendered no-owner badge on the web.
	if ownerName.Valid {
		dto.OwnerName = ownerName.String
	}
	return dto
}

type boardDTO struct {
	RepoID  string      `json:"repo_id"`
	Path    string      `json:"path_with_namespace"`
	WebURL  string      `json:"web_url"`
	Columns []columnDTO `json:"columns"`
	Cards   []cardDTO   `json:"cards"`
	// Pipeline is the repo's default-branch CI status (PRD #6, the board header
	// badge), null when there is no cached default-branch pipeline.
	Pipeline *pipelineDTO `json:"pipeline"`
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
	} else if !h.ensureHumanReviewColumn(w, r, repo) {
		// Boards seeded before PRD #12 lack the Human Review column the automation
		// parks completed runs in; add it on load. Freshly seeded boards already
		// carry it (it is in DefaultColumns), so this only runs for older boards.
		return
	}

	b, ok := h.buildBoard(w, r, repo)
	if !ok {
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"board": b})
}

// ensureHumanReviewColumn makes sure the repo's board has the Human Review column
// (label on the forge + board_columns row), positioned right after In Progress.
// It is idempotent: once present, later loads see it and return without touching
// the forge. A forge outage is non-fatal — the board still renders with its
// existing columns and the next successful load ensures the column. Returns false
// (after writing an error) only on a DB failure.
func (h *Handler) ensureHumanReviewColumn(w http.ResponseWriter, r *http.Request, repo store.GetRepoForUserRow) bool {
	cols, err := h.q.ListBoardColumns(r.Context(), repo.ID)
	if err != nil {
		slog.Error("list board columns", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return false
	}
	pos, needed := humanReviewPlacement(cols)
	if !needed {
		return true // already present — idempotent, no forge or DB work
	}

	f, err := h.svc.ForgeForConnection(repo.ForgeType, repo.BaseUrl, repo.TokenCiphertext)
	if err != nil {
		slog.Error("build forge for connection", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return false
	}
	// Color matches DefaultColumns' Human Review entry.
	if err := f.EnsureLabels(r.Context(), repo.ForgeProjectID, []forge.Label{{Name: board.ColumnHumanReview, Color: "#6e49cb"}}); err != nil {
		slog.Warn("ensure Human Review label on the forge", "repo", repo.PathWithNamespace, "error", err)
		return true // non-fatal: render with existing columns, retry next load
	}
	// Bump the columns Human Review displaces (the backlog buckets) up one so the
	// new column lands right after In Progress with a distinct position — the same
	// order fresh boards seed (In Progress, Human Review, then the rest). The shift
	// is a no-op when appending (In Progress absent).
	if err := h.q.ShiftBoardColumnsFrom(r.Context(), store.ShiftBoardColumnsFromParams{
		RepoID:       repo.ID,
		FromPosition: int32(pos),
	}); err != nil {
		slog.Error("shift board columns for Human Review", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return false
	}
	if err := h.q.InsertBoardColumn(r.Context(), store.InsertBoardColumnParams{
		RepoID:    repo.ID,
		LabelName: board.ColumnHumanReview,
		Position:  int32(pos),
	}); err != nil {
		slog.Error("insert Human Review board column", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return false
	}
	return true
}

// humanReviewPlacement decides whether the Human Review column must be added to a
// board and at what position. needed is false when the column is already present
// (which is what makes the retrofit idempotent — no re-shift on later loads).
// Otherwise the position is right after In Progress, or the end of the board when
// In Progress is somehow absent.
func humanReviewPlacement(cols []store.BoardColumn) (pos int, needed bool) {
	inProgressPos, maxPos := -1, -1
	for _, c := range cols {
		if c.LabelName == board.ColumnHumanReview {
			return 0, false // already present
		}
		if c.LabelName == board.ColumnInProgress {
			inProgressPos = int(c.Position)
		}
		if int(c.Position) > maxPos {
			maxPos = int(c.Position)
		}
	}
	if inProgressPos < 0 {
		return maxPos + 1, true // In Progress absent → append at the end
	}
	return inProgressPos + 1, true
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
	runRows, err := h.q.ListLatestRunsForRepo(r.Context(), repo.ID)
	if err != nil {
		slog.Error("list latest runs", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return boardDTO{}, false
	}

	columns := make([]columnDTO, 0, len(cols))
	position := make(map[string]int, len(cols))
	for _, c := range cols {
		columns = append(columns, columnDTO{LabelName: c.LabelName, Position: int(c.Position)})
		position[c.LabelName] = int(c.Position)
	}

	// CI badges (PRD #6): the board header shows the repo's default-branch pipeline;
	// each card shows its most-recent run's branch pipeline. Both are enrichment —
	// a cache-read failure logs and renders the board without badges.
	repoPipeline := h.defaultBranchPipeline(r, repo)
	cardPipelines := h.cardPipelines(r, repo.ID)

	// repo.UserID is the board viewer (the connection owner); IsMine gates the
	// owner-only run-view link.
	cards := assembleCards(issues, runRows, cardPipelines, position, repo.UserID)

	return boardDTO{
		RepoID:   repo.ID.String(),
		Path:     repo.PathWithNamespace,
		WebURL:   repo.WebUrl,
		Columns:  columns,
		Cards:    cards,
		Pipeline: repoPipeline,
	}, true
}

// defaultBranchPipeline reads the repo's default-branch CI status from the cache
// for the board header (PRD #6). Returns nil when the repo has no default branch,
// no cached pipeline for it, or on a read error (logged) — the badge is enrichment.
func (h *Handler) defaultBranchPipeline(r *http.Request, repo store.GetRepoForUserRow) *pipelineDTO {
	if !repo.DefaultBranch.Valid || repo.DefaultBranch.String == "" {
		return nil
	}
	ps, err := h.q.GetPipelineStatusByRef(r.Context(), store.GetPipelineStatusByRefParams{
		RepoID: repo.ID, Ref: repo.DefaultBranch.String,
	})
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			slog.Warn("board default-branch pipeline", "repo", repo.PathWithNamespace, "error", err)
		}
		return nil
	}
	return pipelineFromStatus(ps)
}

// cardPipelines reads the per-card CI statuses (each issue's most-recent run's
// branch pipeline) keyed by issue iid (PRD #6). A read error logs and yields an
// empty map so cards render without badges.
func (h *Handler) cardPipelines(r *http.Request, repoID uuid.UUID) map[int64]*pipelineDTO {
	out := map[int64]*pipelineDTO{}
	rows, err := h.q.ListRunPipelineStatusesForRepo(r.Context(), repoID)
	if err != nil {
		slog.Warn("board card pipelines", "repo", repoID, "error", err)
		return out
	}
	for _, row := range rows {
		out[row.IssueIid.Int64] = pipelineDTOFrom(row.Ref, row.Status, row.WebUrl, row.PipelineID, row.SyncedAt)
	}
	return out
}

// assembleCards builds the board's cards from the cached issues, the newest run
// per issue (runRows, one row per issue that has run), the column position map,
// and the board viewer. It is the pure, DB-free core of the board payload: it
// keys each issue's latest_run by issue_iid (issues with no run get null), and
// resolves each card's column. viewerID drives IsMine.
func assembleCards(issues []store.Issue, runRows []store.ListLatestRunsForRepoRow, cardPipelines map[int64]*pipelineDTO, position map[string]int, viewerID uuid.UUID) []cardDTO {
	latestByIID := make(map[int64]*latestRunDTO, len(runRows))
	for _, rr := range runRows {
		latestByIID[rr.IssueIid.Int64] = mapLatestRun(rr.ID, rr.UserID, rr.Status, rr.MrIid,
			rr.MrState, rr.FailureReason, rr.StopKind, rr.OwnerName, rr.WorkerName, rr.RunCount, rr.CreatedAt, rr.UpdatedAt, viewerID)
	}

	cards := make([]cardDTO, 0, len(issues))
	for _, is := range issues {
		var labels []string
		if err := json.Unmarshal(is.Labels, &labels); err != nil {
			labels = []string{}
		}
		col, closed, conflict := board.ResolveColumn(labels, is.State, position)
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
			LatestRun:  latestByIID[is.ForgeIssueIid],
			Pipeline:   cardPipelines[is.ForgeIssueIid],
		}
		if is.Author.Valid {
			a := is.Author.String
			card.Author = &a
		}
		cards = append(cards, card)
	}
	return cards
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

	b, ok := h.buildBoard(w, r, repo)
	if !ok {
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"board": b})
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

	f, err := h.svc.ForgeForConnection(repo.ForgeType, repo.BaseUrl, repo.TokenCiphertext)
	if err != nil {
		slog.Error("build forge for connection", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	// Forge-first single-column move + cache snapshot, shared with the run
	// automation (forgesvc.AutoMove). On failure the cache is untouched and the
	// client snaps the card back.
	updated, err := h.svc.AutoMove(r.Context(), f, repo.ForgeProjectID, issue, cols, target)
	if err != nil {
		httpx.Error(w, http.StatusBadGateway, "could not update the issue on the forge: "+err.Error())
		return
	}
	// A manual drag is the operator's explicit intent and overrides any pending
	// automatic move: clear the lifecycle's pending markers for this issue's runs
	// so the reconcile loop stops trying to reposition the card the human placed.
	if _, err := h.q.ClearIssueRunsMovePending(r.Context(), store.ClearIssueRunsMovePendingParams{
		RepoID:   repo.ID,
		IssueIid: pgtype.Int8{Int64: iid, Valid: true},
	}); err != nil {
		// Non-fatal: the move already landed; a stray marker is at worst one
		// reconcile attempt that the manual-drag guard itself skips.
		slog.Warn("clear pending column moves after manual drag", "error", err)
	}

	position := make(map[string]int, len(cols))
	for _, c := range cols {
		position[c.LabelName] = int(c.Position)
	}
	card := issueToCard(updated, position)
	// Carry the issue's latest run on the single-card response too, so a drag never
	// blanks the run badge the board is showing (the client replaces the card).
	if lr, err := h.q.GetLatestRunForIssue(r.Context(), store.GetLatestRunForIssueParams{RepoID: repo.ID, IssueIid: pgtype.Int8{Int64: iid, Valid: true}}); err == nil {
		card.LatestRun = mapLatestRun(lr.ID, lr.UserID, lr.Status, lr.MrIid,
			lr.MrState, lr.FailureReason, lr.StopKind, lr.OwnerName, lr.WorkerName, lr.RunCount, lr.CreatedAt, lr.UpdatedAt, repo.UserID)
	} else if !errors.Is(err, pgx.ErrNoRows) {
		slog.Warn("latest run for moved card", "error", err)
	}
	// Carry the card's CI badge too (PRD #6), so a manual drag never blanks it (a
	// drag touches neither runs nor pipelines — the client replaces the whole card).
	card.Pipeline = h.cardPipelines(r, repo.ID)[iid]
	httpx.JSON(w, http.StatusOK, map[string]any{"card": card})
}

// ── PRDLESS label toggle ──────────────────────────────────────────────────────

type prdlessRequest struct {
	Apply bool `json:"apply"`
}

// SetIssuePrdless applies or removes the configured PRDLESS label on an issue
// directly from the uzi UI (PRD #22 M4, Decision 10). The label name is resolved
// server-side from settings — the client never names it — so this endpoint only
// ever touches the one escape-hatch label, never arbitrary labels. It is
// forge-first (the label write lands on GitLab before the cache) and rides the
// per-user forge limiter like move/sync. When the feature is disabled
// instance-wide it 422s (the label could still be applied from GitLab's own UI,
// but uzi will not). Returns the refreshed card with its latest_run re-hydrated,
// exactly like MoveIssue, so a single-card replace never blanks the run badge.
func (h *Handler) SetIssuePrdless(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.repoForRequest(w, r)
	if !ok {
		return
	}
	iid, err := parseInt64(chi.URLParam(r, "iid"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid issue id")
		return
	}
	var req prdlessRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// Feature gate: the label endpoint is inert when prdless is disabled
	// instance-wide (Decision 10).
	enabled, err := h.settings.PrdlessEnabled(r.Context())
	if err != nil {
		slog.Error("prdless: read settings", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	if !enabled {
		httpx.Error(w, http.StatusUnprocessableEntity, "the PRDLESS label feature is disabled")
		return
	}
	// PrdlessLabel already falls back to the compiled-in default when unset, so
	// label is never empty here.
	label, _ := h.settings.PrdlessLabel(r.Context())

	issue, err := h.q.GetIssueByIID(r.Context(), store.GetIssueByIIDParams{RepoID: repo.ID, ForgeIssueIid: iid})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.Error(w, http.StatusNotFound, "issue not found")
			return
		}
		slog.Error("prdless: get issue", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}

	f, err := h.svc.ForgeForConnection(repo.ForgeType, repo.BaseUrl, repo.TokenCiphertext)
	if err != nil {
		slog.Error("build forge for connection", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	// Forge-first single-label add/remove + incremental cache update
	// (forgesvc.SetIssueLabel). On failure the cache is untouched and the client
	// keeps showing the pre-toggle state (no optimistic update on the web side).
	updated, err := h.svc.SetIssueLabel(r.Context(), f, repo.ForgeProjectID, issue, label, req.Apply)
	if err != nil {
		httpx.Error(w, http.StatusBadGateway, "could not update the issue label on the forge: "+err.Error())
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
	card := issueToCard(updated, position)
	// Carry the issue's latest run on the single-card response (like MoveIssue), so
	// a toggle never blanks the run badge the board/issue view is showing.
	if lr, err := h.q.GetLatestRunForIssue(r.Context(), store.GetLatestRunForIssueParams{RepoID: repo.ID, IssueIid: pgtype.Int8{Int64: iid, Valid: true}}); err == nil {
		card.LatestRun = mapLatestRun(lr.ID, lr.UserID, lr.Status, lr.MrIid,
			lr.MrState, lr.FailureReason, lr.StopKind, lr.OwnerName, lr.WorkerName, lr.RunCount, lr.CreatedAt, lr.UpdatedAt, repo.UserID)
	} else if !errors.Is(err, pgx.ErrNoRows) {
		slog.Warn("latest run for prdless card", "error", err)
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"card": card})
}

// issueToCard resolves a cached issue row into a card DTO.
func issueToCard(is store.Issue, position map[string]int) cardDTO {
	var labels []string
	if err := json.Unmarshal(is.Labels, &labels); err != nil {
		labels = []string{}
	}
	col, closed, conflict := board.ResolveColumn(labels, is.State, position)
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
	b, ok := h.buildBoard(w, r, repo)
	if !ok {
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"board": b})
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
