package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/board"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forgesvc"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/schedsvc"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// defaultColumnColor is used when a user adds a brand-new column whose label
// does not yet exist on the forge.
const defaultColumnColor = forgesvc.DefaultColumnColor

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
	IID    int64    `json:"iid"`
	Title  string   `json:"title"`
	State  string   `json:"state"`
	Labels []string `json:"labels"`
	WebURL string   `json:"web_url"`
	// ForgeType is the card's forge ("gitlab"|"forgejo"|"github"), so the web picks the
	// per-card MR/PR noun (PRD #65 D2). Every card on one board shares the repo's
	// connection, but a cross-repo view (dashboard) mixes forges, so it rides the
	// card. Threaded from the repo's connection, not a query change.
	ForgeType  string  `json:"forge_type"`
	Author     *string `json:"author"`
	HasPRDLink bool    `json:"has_prd_link"`
	// Column is the resolved column label; "" means the implicit Open column.
	// Ignored when Closed is true.
	Column string `json:"column"`
	// Closed places the card in the implicit Closed column (issue state=closed).
	Closed bool `json:"closed"`
	// Conflict flags an issue that arrived carrying more than one column label;
	// it is shown in the highest-positioned one until the next move normalizes it.
	Conflict bool `json:"conflict"`
	// ForgeUpdatedAt is the issue's forge-side updated_at (issues.forge_updated_at,
	// NOT NULL), for the board's "Last updated" sort mode (PRD #102 M5).
	//
	// It must be set by BOTH card builders — assembleCards AND issueToCard. Missing
	// it in issueToCard (the single-card path, behind MoveIssue, PromoteIssue AND
	// CreateIssue in handler/issues.go — three call sites, not the two this comment
	// named until 2026-07-27) is silent: the card comes back with the zero time,
	// marshals as "0001-01-01T00:00:00Z", and instantly sinks to the bottom in Last
	// updated mode while every other card is fine. The typechecker cannot see it; only
	// a test that drags in that mode can.
	//
	// board_position deliberately does NOT ride the card. The order is expressed by
	// ListIssuesByRepo's ORDER BY, never as a number the client reasons about, which
	// is what lets the web client's Manual mode be the identity function.
	ForgeUpdatedAt time.Time `json:"forge_updated_at"`
	// LatestRun is the newest run for this issue, or null when it has never run.
	// It carries only display state (no secrets); the run-view link is owner-only,
	// so IsMine tells the client whether the viewer may open it.
	LatestRun *latestRunDTO `json:"latest_run"`
	// Pipeline is the CI status of the card's MOST RECENT run's branch (PRD #6),
	// null when that run has no branch, no CI, or the card has never run. It is what
	// renders the per-card badge and gates the "Fix CI" affordance.
	Pipeline *apitypes.PipelineDTO `json:"pipeline"`
}

// latestRunDTO is the run summary a card carries (PRD #12 M2), so the board needs
// no second listRuns fan-in. OwnerName drives the "started by X" treatment;
// IsMine gates the run-view link (a non-owner would 403 on GetRunByIDForUser).
type latestRunDTO struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	MrIID  *int64 `json:"mr_iid"`
	// MrWebURL is the forge-supplied merge/pull-request web URL persisted by the
	// worker at MR creation (PRD #65 D8), null on rows created before it landed —
	// the web renders it directly (through isHttpsUrl) and falls back to the legacy
	// GitLab URL reconstruction only for those null rows. Stops uzi guessing a URL
	// the forge already told it, and is the ONLY correct MR link on Forgejo (whose
	// `/{owner}/{repo}/pulls/N` grammar the reconstruction never knew).
	MrWebURL *string `json:"mr_web_url"`
	// MrState is the PRD #24 watcher's last-observed merge-request state, null when
	// never observed. Display-only (PRD #33 Decision 1): the board card's chip
	// renders merged/closed distinctly and everything else as the plain open chip.
	// Kept fresh by the watcher only for the board card (the issue's latest run).
	MrState *string `json:"mr_state"`
	// FailureReason is OWNER-ONLY (PRD #33 Decision 5): it can carry a user's verbatim
	// typed reject reason or a raw agent error, so a shared board must not expose it
	// to non-owner viewers — they get null. Owners keep it (the failed-badge tooltip).
	// The non-sensitive StopKind enum below stays visible to everyone for the
	// stopped-vs-failed badge, so classification never depends on this field.
	FailureReason *string `json:"failure_reason"`
	// StopKind is the server-stamped deliberate-stop signal (PRD #33): "cancelled"
	// or "plan_rejected", null otherwise. The board badge reads it (not the
	// failure_reason text) to render a deliberate stop as calm "stopped".
	StopKind *string `json:"stop_kind"`
	// Health is the run-health flag (PRD #47): ok|stalled|looping|slow|
	// waiting_worker|approval_idle. Non-sensitive (like StopKind), so it rides the
	// shared board card unconditionally — runBadge renders the warn variant only for
	// a flaggable status. HealthSince (when the flag was raised) is likewise
	// unconditional and drives the "stuck for Xm" elapsed. HealthReason, which can
	// name owner state ("your vault is locked"), is OWNER-ONLY, exactly like
	// FailureReason (Decision 6): a non-owner viewer gets null.
	Health       string     `json:"health"`
	HealthReason *string    `json:"health_reason"`
	HealthSince  *time.Time `json:"health_since"`
	// IsPlanning is issue #321's planning-phase display flag on the board card;
	// non-sensitive, rides the shared card unconditionally like Status. Server-computed
	// (running, iteration_count 0, no persisted plan yet; chat/judge excluded).
	IsPlanning bool `json:"is_planning"`
	// IsRevising is issue #750's plan-revise display flag on the board card: true when
	// the run's latest plan-ish message ({plan, plan_revising}) is a plan_revising — a
	// "revise" replan in flight. Non-sensitive, rides the shared card unconditionally
	// like IsPlanning/Status. Server-computed (planRevisingSet), meaningful only while
	// status == "awaiting_approval". Set by assembleCards after mapLatestRun, since the
	// signal needs a batched per-run message fetch mapLatestRun's columns do not carry.
	IsRevising bool    `json:"is_revising"`
	OwnerName  string  `json:"owner_name"`
	WorkerName *string `json:"worker_name"`
	IsMine     bool    `json:"is_mine"`
	// RunCount is how many runs the issue has had (this run being the newest). >1
	// drives the board's "×N" retry hint; full history lives in the issue view.
	RunCount  int64     `json:"run_count"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// mapLatestRun builds the card's run summary from the shared run + owner + worker
// columns the board and single-card queries both return. viewerID is the board
// viewer; IsMine is set when the run belongs to them (only then does the client
// render the run-view link). ownerName is the owner's display name or empty — never
// the email (PRD #33 Decision 5): a shared board must not leak another user's email
// on a card, and the web already renders a no-owner badge for empty. The query no
// longer even selects the email, so there is nothing to fall back to here.
func mapLatestRun(runID, ownerID uuid.UUID, status string, kind string, iterationCount int32, hasPlanMd bool, mrIID pgtype.Int8, mrWebURL, mrState, failureReason, stopKind pgtype.Text, health string, healthReason pgtype.Text, healthSince pgtype.Timestamptz, ownerName, workerName pgtype.Text, runCount int64, createdAt, updatedAt pgtype.Timestamptz, viewerID uuid.UUID) *latestRunDTO {
	dto := &latestRunDTO{
		ID:          runID.String(),
		Status:      status,
		MrWebURL:    textPtrValue(mrWebURL.Valid, mrWebURL.String),
		MrState:     textPtrValue(mrState.Valid, mrState.String),
		StopKind:    textPtrValue(stopKind.Valid, stopKind.String),
		Health:      health,
		HealthSince: timePtr(healthSince.Valid, healthSince.Time),
		IsPlanning:  isPlanningPhase(kind, status, iterationCount, hasPlanMd),
		WorkerName:  textPtrValue(workerName.Valid, workerName.String),
		IsMine:      ownerID == viewerID,
		RunCount:    runCount,
		CreatedAt:   createdAt.Time,
		UpdatedAt:   updatedAt.Time,
	}
	// failure_reason and health_reason are owner-only (Decisions 5 & 6): both can carry
	// text about the owner (a verbatim reject reason, a raw agent error, "your vault is
	// locked"), so a non-owner viewer of a shared board gets null. stop_kind, health, and
	// health_since (above, unconditional) already give everyone the classification and
	// the elapsed for the badge.
	if dto.IsMine {
		dto.FailureReason = textPtrValue(failureReason.Valid, failureReason.String)
		dto.HealthReason = textPtrValue(healthReason.Valid, healthReason.String)
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

// setLatestRunRevising stamps issue #750's derived is_revising flag on a
// single-card latest-run DTO. The board-wide path (assembleCards) uses the batch
// PlanRevisingForRuns map; the single-card refresh responses (drag, promote) go
// through here so a revising run keeps the flag — otherwise the card
// would blank it and pop the attention ring back until the next full board poll.
// Best-effort like the batch path: the flag is decoration, so a query error just
// leaves it false rather than failing the response.
func (h *Handler) setLatestRunRevising(ctx context.Context, dto *latestRunDTO, runID uuid.UUID) {
	if dto == nil {
		return
	}
	if m, err := h.wsvc.PlanRevisingForRuns(ctx, []uuid.UUID{runID}); err == nil {
		dto.IsRevising = m[runID]
	} else {
		slog.Warn("single-card plan revising state", "error", err)
	}
}

type boardDTO struct {
	RepoID string `json:"repo_id"`
	Path   string `json:"path_with_namespace"`
	WebURL string `json:"web_url"`
	// ForgeType is the board's forge ("gitlab"|"forgejo"|"github"), so board-level chrome
	// (the "columns are <forge> labels" hint, the create-issue "opened on <forge>"
	// note) names the right platform (PRD #65 D2). A board is one repo/connection, so
	// this is a single value; per-card forge rides each card. From repo.ForgeType.
	ForgeType string      `json:"forge_type"`
	Columns   []columnDTO `json:"columns"`
	Cards     []cardDTO   `json:"cards"`
	// Pipeline is the repo's default-branch CI status (PRD #6, the board header
	// badge), null when there is no cached default-branch pipeline.
	Pipeline *apitypes.PipelineDTO `json:"pipeline"`
}

// ── Board ───────────────────────────────────────────────────────────────────

// GetBoard returns a repo's kanban board: its configured columns plus the cached
// issues as cards, each resolved to a column. The first time a board is opened (no
// columns yet) it seeds the default columns as labels on the forge and imports the
// current issues — a deliberate, documented side effect.
//
// The payload carries every cached issue, PRD-labelled or not (PRD #102 M6). The
// non-PRD ones are filtered CLIENT-side, at render, behind a per-browser toggle:
// they have to be in the payload regardless, because the board freeze is computed
// from the whole set and dropping the hidden cards from it would null their stored
// order (Decision 7b's payload-set rule).
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
	// Bump the columns Human Review displaces up one so the new column lands right
	// after In Progress with a distinct position — the same RELATIVE placement fresh
	// boards seed. (PRD #102 Decision 2 moved Planned ahead of In Progress, so a
	// fresh board no longer leads with In Progress; humanReviewPlacement anchors off
	// In Progress's position rather than an absolute index, which is what keeps this
	// retrofit correct either way.) The shift is a no-op when appending (In Progress
	// absent).
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

	// Plan-revise display flag per card's latest run (issue #750). Best-effort like the
	// pipeline badges above: the card is decoration, so a failure logs and leaves every
	// flag false (a nil map indexes to false) rather than failing the board.
	revisingIDs := make([]uuid.UUID, 0, len(runRows))
	for _, rr := range runRows {
		revisingIDs = append(revisingIDs, rr.ID)
	}
	revising, err := h.wsvc.PlanRevisingForRuns(r.Context(), revisingIDs)
	if err != nil {
		slog.Error("board plan revising states", "error", err)
		revising = nil
	}

	// repo.UserID is the board viewer (the connection owner); IsMine gates the
	// owner-only run-view link. repo.ForgeType stamps every card's forge for the
	// per-card MR/PR noun (all cards on one board share the repo's connection).
	cards := assembleCards(issues, runRows, cardPipelines, position, repo.UserID, repo.ForgeType, revising)

	return boardDTO{
		RepoID:    repo.ID.String(),
		Path:      repo.PathWithNamespace,
		WebURL:    repo.WebUrl,
		ForgeType: repo.ForgeType,
		Columns:   columns,
		Cards:     cards,
		Pipeline:  repoPipeline,
	}, true
}

// defaultBranchPipeline reads the repo's default-branch CI status from the cache
// for the board header (PRD #6). Returns nil when the repo has no default branch,
// no cached pipeline for it, or on a read error (logged) — the badge is enrichment.
func (h *Handler) defaultBranchPipeline(r *http.Request, repo store.GetRepoForUserRow) *apitypes.PipelineDTO {
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
func (h *Handler) cardPipelines(r *http.Request, repoID uuid.UUID) map[int64]*apitypes.PipelineDTO {
	out := map[int64]*apitypes.PipelineDTO{}
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

// decodeLabels turns a cached issue's labels jsonb into the slice every card DTO
// ships. It GUARANTEES a non-nil result, which is the whole reason it exists as a
// function rather than two inline unmarshals.
//
// The column is `jsonb NOT NULL DEFAULT '[]'`, and SQL NOT NULL does not exclude
// the jsonb scalar `null` — a distinct value that json.Unmarshal decodes into a
// nil slice with NO error, so the error branch below never sees it. A nil slice
// marshals as JSON `null`, not `[]`, and the web then calls .filter and .includes
// on it.
//
// Unreachable before PRD #102 M6: the sync filter guaranteed every cached issue
// carried the PRD label, so labels was never empty. The additive fetch caches open
// issues regardless of label, and an issue with NO labels at all is the ordinary
// shape of a freshly filed one.
//
// Fixed on the READ side deliberately. The driver mapping is fixed too (a nil
// []string for a label-less issue is where the null originates), but that alone
// would leave every row already stored as jsonb null broken, because the nil
// survives the round trip without ever erroring.
func decodeLabels(raw []byte) []string {
	var labels []string
	if err := json.Unmarshal(raw, &labels); err != nil {
		return []string{}
	}
	return nonNilLabels(labels)
}

// nonNilLabels is decodeLabels' guarantee on its own, for the card builder that
// takes labels straight from a forge response rather than from the cache
// (handler/issues.go's create path). Same reason: a nil []string marshals as JSON
// null and the web calls .filter on it.
func nonNilLabels(labels []string) []string {
	if labels == nil {
		return []string{}
	}
	return labels
}

// assembleCards builds the board's cards from the cached issues, the newest run
// per issue (runRows, one row per issue that has run), the column position map,
// and the board viewer. It is the pure, DB-free core of the board payload: it
// keys each issue's latest_run by issue_iid (issues with no run get null), and
// resolves each card's column. viewerID drives IsMine. revising is the set of run
// ids whose latest plan-ish message is a plan_revising (issue #750); a nil map leaves
// every IsRevising false.
func assembleCards(issues []store.Issue, runRows []store.ListLatestRunsForRepoRow, cardPipelines map[int64]*apitypes.PipelineDTO, position map[string]int, viewerID uuid.UUID, forgeType string, revising map[uuid.UUID]bool) []cardDTO {
	latestByIID := make(map[int64]*latestRunDTO, len(runRows))
	for _, rr := range runRows {
		dto := mapLatestRun(rr.ID, rr.UserID, rr.Status, rr.Kind, rr.IterationCount, rr.HasPlanMd.Bool, rr.MrIid, rr.MrWebUrl,
			rr.MrState, rr.FailureReason, rr.StopKind, rr.Health, rr.HealthReason, rr.HealthSince,
			rr.OwnerName, rr.WorkerName, rr.RunCount, rr.CreatedAt, rr.UpdatedAt, viewerID)
		dto.IsRevising = revising[rr.ID] // nil map ⇒ false (issue #750)
		latestByIID[rr.IssueIid.Int64] = dto
	}

	cards := make([]cardDTO, 0, len(issues))
	for _, is := range issues {
		labels := decodeLabels(is.Labels)
		col, closed, conflict := board.ResolveColumn(labels, is.State, position)
		card := cardDTO{
			IID:        is.ForgeIssueIid,
			Title:      is.Title,
			State:      is.State,
			Labels:     labels,
			WebURL:     is.WebUrl,
			ForgeType:  forgeType,
			HasPRDLink: is.HasPrdLink,
			Column:     col,
			Closed:     closed,
			Conflict:   conflict,
			// Sibling of issueToCard's — keep the two in step (see cardDTO.ForgeUpdatedAt).
			ForgeUpdatedAt: is.ForgeUpdatedAt.Time,
			LatestRun:      latestByIID[is.ForgeIssueIid],
			Pipeline:       cardPipelines[is.ForgeIssueIid],
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

// ── Manual order ────────────────────────────────────────────────────────────

// maxBoardOrderIids caps how many iids one freeze may carry. Far above the PRD's
// own scale expectation (boards here are hundreds of cards); it exists because an
// array-accepting endpoint must not be handable an unbounded one, not because any
// real board approaches it. Mirrors maxBoardColumns' shape.
const maxBoardOrderIids = 5000

type setBoardOrderRequest struct {
	IIDs []int64 `json:"iids"`
}

// SetBoardOrder replaces the repo's manual card order wholesale (PRD #102 M5,
// Decision 7b). The client sends the WHOLE intended order as a board-global iid
// list and the server's only job is to number it.
//
// Why the client owns the order and not the server: the sort mode lives in the
// browser's localStorage (Decision 8) and two of the five modes are computed from
// data the server would have to re-derive, so any "put #7 after #12, you work out
// the rest" contract would need a Go reimplementation of the TypeScript sort. Two
// implementations of one ordering is a contract needing a differential test, for no
// benefit.
//
// The freeze is board-wide and unconditional on every drop, including one already in
// Manual mode. Gating it on "the mode is not Manual" reintroduces the exact bug
// Decision 7b exists to prevent: on an untouched board every position is NULL, so a
// single written position sorts ahead of every NULL under NULLS LAST and a card
// dragged to the BOTTOM of its column renders at the TOP.
//
// Both statements run in ONE transaction. Between them the board is torn (some rows
// renumbered, others still holding old numbers and not yet cleared), and the board
// polls every 10s from possibly more than one tab, so a GET landing in that window
// would render a scrambled order. The transaction also makes a failure of the second
// statement roll the first back rather than persist half a freeze. It deliberately
// does NOT span the forge label write a cross-column drag performs first: that is a
// separate HTTP call to another system.
//
// Concurrency is last-writer-wins across the owner's own tabs and devices, accepted
// in Decision 7b: boardDTO carries no version or etag for a client to send, and the
// existing 10s refetch resolves it. There is no second user (issues are per-owner via
// repos -> forge_connections.user_id), which is what makes that acceptable.
func (h *Handler) SetBoardOrder(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.repoForRequest(w, r)
	if !ok {
		return
	}
	var req setBoardOrderRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// THE CAP IS CHECKED ON THE RAW REQUEST, BEFORE THE DEDUPE, and the order is the
	// whole point. Checking it after would size the two allocations below off the
	// decoded body rather than off the cap: measured, a maximal 1 MiB body decodes to
	// 524,283 iids that dedupe down to 1, having already cost ~22 MiB of
	// pre-allocation — on a route that deliberately carries no forge limiter. Reject
	// first, then allocate against a bounded length.
	//
	// Consequence, stated because it is a real behaviour choice: a body that is over
	// the cap only because it repeats iids is now rejected rather than deduped down to
	// something legal. No client produces one (the board sends each card once), and
	// bounding the allocation is worth more than accepting a malformed body.
	if len(req.IIDs) > maxBoardOrderIids {
		httpx.Error(w, http.StatusBadRequest, "too many issues (max "+strconv.Itoa(maxBoardOrderIids)+")")
		return
	}

	// Dedupe preserving first occurrence, the precedent ConfigureColumns sets for its
	// column names. A duplicate iid is a client bug and must not 400 somebody's drag;
	// left in, it would consume an ordinal and shift every card after it.
	seen := make(map[int64]struct{}, len(req.IIDs))
	iids := make([]int64, 0, len(req.IIDs))
	for _, iid := range req.IIDs {
		if _, dup := seen[iid]; dup {
			continue
		}
		seen[iid] = struct{}{}
		iids = append(iids, iid)
	}

	// THE EMPTY-LIST GUARD IS LOAD-BEARING, NOT DEFENSIVE TIDINESS. ClearBoardOrderExcept
	// filters on `forge_issue_iid <> ALL(@iids)`, and `<> ALL('{}')` is TRUE for every
	// row, so running it with an empty array wipes every position on the board. Same
	// trap DeleteIssuesNotIn's comment documents for its keep-set. Running neither
	// statement is also the right answer semantically: an empty submit expresses no
	// order, so there is nothing to record.
	if len(iids) > 0 {
		tx, err := h.pool.Begin(r.Context())
		if err != nil {
			slog.Error("begin board order tx", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
		defer tx.Rollback(r.Context()) //nolint:errcheck // no-op after a successful Commit
		qtx := h.q.WithTx(tx)

		if err := qtx.SetBoardOrderPositions(r.Context(), store.SetBoardOrderPositionsParams{
			RepoID: repo.ID,
			Iids:   iids,
		}); err != nil {
			slog.Error("set board order positions", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
		if err := qtx.ClearBoardOrderExcept(r.Context(), store.ClearBoardOrderExceptParams{
			RepoID: repo.ID,
			Iids:   iids,
		}); err != nil {
			slog.Error("clear board order", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
		if err := tx.Commit(r.Context()); err != nil {
			slog.Error("commit board order tx", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
	}

	// The full board, matching ConfigureColumns and SyncRepo. The client adopts it
	// wholesale, which is what makes Manual mode "render the payload order" work with
	// no client-side bookkeeping.
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

	// Project the move onto a linked GitHub Projects v2 Status (PRD #364 M5),
	// best-effort. Hooked here (a uzi-originated move) rather than inside AutoMove,
	// which the reverse poller also calls. Never fail the response on a sync error.
	if h.projectSync != nil {
		if err := h.projectSync.ForwardMove(r.Context(), repo.ID, issue.ForgeIssueIid, target); err != nil {
			slog.Warn("project sync: forward move after drag", "repo", repo.ID, "issue", issue.ForgeIssueIid, "error", err)
		}
	}

	position := make(map[string]int, len(cols))
	for _, c := range cols {
		position[c.LabelName] = int(c.Position)
	}
	card := issueToCard(updated, position, repo.ForgeType)
	// Carry the issue's latest run on the single-card response too, so a drag never
	// blanks the run badge the board is showing (the client replaces the card).
	if lr, err := h.q.GetLatestRunForIssue(r.Context(), store.GetLatestRunForIssueParams{RepoID: repo.ID, IssueIid: pgtype.Int8{Int64: iid, Valid: true}}); err == nil {
		card.LatestRun = mapLatestRun(lr.ID, lr.UserID, lr.Status, lr.Kind, lr.IterationCount, lr.HasPlanMd.Bool, lr.MrIid, lr.MrWebUrl,
			lr.MrState, lr.FailureReason, lr.StopKind, lr.Health, lr.HealthReason, lr.HealthSince,
			lr.OwnerName, lr.WorkerName, lr.RunCount, lr.CreatedAt, lr.UpdatedAt, repo.UserID)
		h.setLatestRunRevising(r.Context(), card.LatestRun, lr.ID) // issue #750
	} else if !errors.Is(err, pgx.ErrNoRows) {
		slog.Warn("latest run for moved card", "error", err)
	}
	// Carry the card's CI badge too (PRD #6), so a manual drag never blanks it (a
	// drag touches neither runs nor pipelines — the client replaces the whole card).
	card.Pipeline = h.cardPipelines(r, repo.ID)[iid]
	httpx.JSON(w, http.StatusOK, map[string]any{"card": card})
}

// promotable reports whether an issue may be given the uzi label from the board
// (PRD #102 Decision 13a). The one exclusion is uzi's own self-improvement
// tracking issue, which carries schedsvc.SelfImproveTrackingLabel deliberately INSTEAD
// of a uzi or autopilot label, is open on uzi's own repo, and is therefore cached and
// rendered by M6's additive fetch like any other non-uzi card.
//
// A pure predicate rather than an inline check because the handler cannot be unit
// tested (h.q is a concrete *store.Queries) and this is the part worth testing.
func promotable(labels []string) bool {
	return !slices.Contains(labels, schedsvc.SelfImproveTrackingLabel)
}

// PromoteIssue applies the configured uzi run-eligibility label (PRD #764) to a
// non-uzi issue, forge-first (PRD #102 Decision 15). One click makes a card uzi's
// work: it becomes runnable (the run gate reads the same label), it stops depending
// on the toggle to be visible, and it loses the dashed treatment.
//
// It is apply-only. There is no demote (Decision 15): removing a label in the
// forge's own UI is easy and nobody has asked for the button. The auto-created
// label is pinned to a color (forgesvc.PromoteLabelColor) because SetIssueLabel's
// apply path auto-creates a missing label and GitLab's label-create API requires
// one. It refuses the self-improvement tracking issue (Decision 13a): that issue is
// open on uzi's own repo, carries TrackingLabel deliberately INSTEAD of a uzi or
// autopilot label, and the additive fetch is what makes it visible on the board at
// all. Promoting it would slap the uzi label onto internal machinery and let a
// self-improve run be started by hand from a card.
//
// Returns the refreshed card with its latest_run re-hydrated, exactly like
// MoveIssue, so a single-card replace never blanks the run badge.
func (h *Handler) PromoteIssue(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.repoForRequest(w, r)
	if !ok {
		return
	}
	iid, err := parseInt64(chi.URLParam(r, "iid"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid issue id")
		return
	}

	// UziLabel already falls back to the compiled-in default when unset, so label is
	// never empty here.
	label, _ := h.settings.UziLabel(r.Context())

	issue, err := h.q.GetIssueByIID(r.Context(), store.GetIssueByIIDParams{RepoID: repo.ID, ForgeIssueIid: iid})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.Error(w, http.StatusNotFound, "issue not found")
			return
		}
		slog.Error("promote: get issue", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}

	// Decision 13a, enforced SERVER-side and not only by hiding the button: the web
	// hides Promote on this card, but the endpoint is reachable regardless and the
	// blast radius is a PRD label on uzi's own internal tracking issue.
	if !promotable(decodeLabels(issue.Labels)) {
		httpx.Error(w, http.StatusUnprocessableEntity, "this issue is uzi's own self-improvement tracker and cannot be promoted")
		return
	}

	f, err := h.svc.ForgeForConnection(repo.ForgeType, repo.BaseUrl, repo.TokenCiphertext)
	if err != nil {
		slog.Error("build forge for connection", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	// Forge-first, then the incremental cache update (forgesvc.SetIssueLabel). On
	// failure the cache is untouched and the client keeps showing the pre-promote
	// state. Applying a label the issue already carries is a local no-op success
	// with no forge call, so a double click costs nothing.
	updated, err := h.svc.SetIssueLabel(r.Context(), f, repo.ForgeProjectID, issue, label, forgesvc.PromoteLabelColor, true)
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
	card := issueToCard(updated, position, repo.ForgeType)
	if lr, err := h.q.GetLatestRunForIssue(r.Context(), store.GetLatestRunForIssueParams{RepoID: repo.ID, IssueIid: pgtype.Int8{Int64: iid, Valid: true}}); err == nil {
		card.LatestRun = mapLatestRun(lr.ID, lr.UserID, lr.Status, lr.Kind, lr.IterationCount, lr.HasPlanMd.Bool, lr.MrIid, lr.MrWebUrl,
			lr.MrState, lr.FailureReason, lr.StopKind, lr.Health, lr.HealthReason, lr.HealthSince,
			lr.OwnerName, lr.WorkerName, lr.RunCount, lr.CreatedAt, lr.UpdatedAt, repo.UserID)
		h.setLatestRunRevising(r.Context(), card.LatestRun, lr.ID) // issue #750
	} else if !errors.Is(err, pgx.ErrNoRows) {
		slog.Warn("latest run for promoted card", "error", err)
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"card": card})
}

// issueToCard resolves a cached issue row into a card DTO. forgeType stamps the
// card's forge for the per-card MR/PR noun (PRD #65 D2), from the repo's connection.
// Three production callers: MoveIssue, PromoteIssue and CreateIssue (issues.go).
func issueToCard(is store.Issue, position map[string]int, forgeType string) cardDTO {
	labels := decodeLabels(is.Labels)
	col, closed, conflict := board.ResolveColumn(labels, is.State, position)
	card := cardDTO{
		IID:        is.ForgeIssueIid,
		Title:      is.Title,
		State:      is.State,
		Labels:     labels,
		WebURL:     is.WebUrl,
		ForgeType:  forgeType,
		HasPRDLink: is.HasPrdLink,
		Column:     col,
		Closed:     closed,
		Conflict:   conflict,
		// Sibling of assembleCards' — this is the single-card path (MoveIssue,
		// PromoteIssue), and omitting it here is the silent half of the bug
		// cardDTO.ForgeUpdatedAt describes: only a just-dragged card would carry the
		// zero time, so every other card looks right.
		ForgeUpdatedAt: is.ForgeUpdatedAt.Time,
	}
	if is.Author.Valid {
		a := is.Author.String
		card.Author = &a
	}
	return card
}

// ── Manual sync ─────────────────────────────────────────────────────────────

// SyncRepo runs an on-demand full sync (the Refresh button): fetch the complete
// uzi-labeled set, upsert, and evict anything gone forge-side. Returns the
// refreshed board so the client updates in one round-trip.
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
