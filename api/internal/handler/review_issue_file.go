package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forgesvc"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	"github.com/vtmocanu/uzi/api/internal/issuedraft"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

type fileIssueRequest struct {
	RepoID      string `json:"repo_id"`
	Title       string `json:"title"`
	Description string `json:"description"`
}

// fileIssueResponse is the file-issue result: the real forge issue the click created,
// plus a non-empty warning when the issue was created but its local link/cache could not
// be settled (created-with-warning, Decision 9) — the issue still exists on the forge and
// the next sync reconciles it, so this is a success, never a retry signal.
type fileIssueResponse struct {
	Issue   createdIssueDTO `json:"issue"`
	Warning string          `json:"warning,omitempty"`
}

// FileIssue files a GitLab issue from one judge recommendation (PRD #68 M3). It mounts on
// RequireUser behind forgeLimiter.PerUserMiddleware (still a forge write): reachable from a
// CLI uzc_ Bearer token, while a browser caller stays on the cookie path with CSRF preserved
// via RequireUser's presence dispatch (PRD #365 M1). Authorization is two-part (Decision 8):
// owner-or-admin to SEE the recommendation (GetReviewForTarget), and caller-owns-repo to WRITE (GetRepoForUser,
// session-user-scoped). The body that reaches the forge is the CLIENT's, so the
// write-boundary sanitizer (Decision 10) re-runs here — the draft is never trusted to have
// preserved its inertness. Ordering is forge-first (the forge is the source of truth):
// claim-first (Decision 7) → CreateIssue → settle+cache in one tx (Decision 9).
func (h *Handler) FileIssue(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	runID, ok := httpx.PathUUID(w, r, "id", "run")
	if !ok {
		return
	}
	recID, ok := httpx.PathUUID(w, r, "recID", "recommendation")
	if !ok {
		return
	}
	var req fileIssueRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	repoID, err := uuid.Parse(strings.TrimSpace(req.RepoID))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid repo id")
		return
	}
	ctx := r.Context()

	// Owner-or-admin visibility (Decision 8 read side): resolve the recommendation to its
	// coordinate. A run the caller can't see, an unjudged run, or an unknown recID are all
	// 404 — the same not-found the draft read returns.
	res, err := h.wsvc.GetReviewForTarget(ctx, user.ID, user.IsAdmin, runID)
	if err != nil {
		if errors.Is(err, workersvc.ErrRunNotFound) {
			httpx.Error(w, http.StatusNotFound, "run not found")
			return
		}
		slog.Error("file issue: get review", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	if res == nil {
		httpx.Error(w, http.StatusNotFound, "recommendation not found")
		return
	}
	rec, found := findRecommendation(res.Recommendations, recID)
	if !found {
		httpx.Error(w, http.StatusNotFound, "recommendation not found")
		return
	}

	// Bound the sanitizer's input before scanning it, then re-apply the write-boundary
	// controls SERVER-SIDE on the client's body (Decision 10): the client may have edited
	// or replaced the draft, so its inertness must be re-established here, not assumed.
	if len(req.Description) > workersvc.MaxIssueDescriptionBytes {
		httpx.Error(w, http.StatusBadRequest, "description is too large")
		return
	}
	title := issuedraft.SanitizeTitle(req.Title)
	description := issuedraft.SanitizeFiledBody(req.Description)
	if title == "" {
		httpx.Error(w, http.StatusBadRequest, "title must be non-empty")
		return
	}

	// Caller-owns-repo (Decision 8 write side): GetRepoForUser joins repos → connections
	// WHERE user_id = caller, so a repo the caller does not own is 404 — a plain user
	// cannot file against someone else's repo.
	repo, err := h.q.GetRepoForUser(ctx, store.GetRepoForUserParams{ID: repoID, UserID: user.ID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.Error(w, http.StatusNotFound, "repo not found")
			return
		}
		slog.Error("file issue: load repo", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}

	// Labels are assembled server-side (Decision 3/6): the single uzi run-eligibility
	// label (PRD #764), NEVER autopilot and NEVER from the request body — the API
	// surface gives the client no way to attach a trigger label.
	uziLabel, _ := h.settings.UziLabel(ctx)
	labels := []string{uziLabel}

	// Claim-first (Decision 7): atomic INSERT ... ON CONFLICT DO NOTHING on the coordinate
	// BEFORE the forge write, so of two concurrent POSTs exactly one reaches CreateIssue.
	// A lost claim — the coordinate is already filed OR mid-filing — is 409.
	claimID, err := h.q.ClaimRecommendationFiledIssue(ctx, store.ClaimRecommendationFiledIssueParams{
		ReviewID:      res.Review.ID,
		Category:      rec.Category,
		Target:        rec.Target,
		FiledByUserID: pgtype.UUID{Bytes: user.ID, Valid: true},
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.Error(w, http.StatusConflict, "this recommendation already has an issue, or one is being filed")
			return
		}
		slog.Error("file issue: claim", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}

	f, err := h.svc.ForgeForConnection(repo.ForgeType, repo.BaseUrl, repo.TokenCiphertext)
	if err != nil {
		h.revertFiledClaim(ctx, claimID)
		slog.Error("file issue: build forge for connection", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	created, err := f.CreateIssue(ctx, repo.ForgeProjectID, title, description, labels)
	if err != nil {
		// Forge rejected the write (mock state E): the claim reverts, nothing is
		// persisted, the draft stays open. err is already PAT-redacted by the driver.
		h.revertFiledClaim(ctx, claimID)
		httpx.Error(w, http.StatusBadGateway, "could not create the issue on the forge: "+err.Error())
		return
	}

	// Forge-first done. Settle the link + cache the issue in one tx (Decision 9); a tx
	// failure OR a swept-out claim (0 rows settled) is created-with-warning — the real
	// issue exists, so we NEVER revert (would orphan it) and NEVER retry the forge write.
	warning := h.settleFiledIssue(ctx, claimID, repo, created, description)

	httpx.JSON(w, http.StatusCreated, fileIssueResponse{
		Issue:   createdIssueDTO{IID: created.IID, WebURL: created.WebURL, Title: created.Title},
		Warning: warning,
	})
}

// revertFiledClaim best-effort DELETEs a claim after a post-claim, pre-settle failure so
// the coordinate is fileable again. A revert error is logged but never changes the
// response the user already gets for the underlying failure.
func (h *Handler) revertFiledClaim(ctx context.Context, claimID uuid.UUID) {
	if err := h.q.RevertRecommendationFiledIssue(ctx, claimID); err != nil {
		slog.Error("file issue: revert claim after failure", "claim", claimID.String(), "error", err)
	}
}

// settleFiledIssue settles the won claim with the created issue and upserts the issue into
// the board cache, in ONE transaction (Decision 9). It runs only AFTER a successful
// CreateIssue, so every failure mode here is created-with-warning: the returned string is
// empty on a clean settle, and a human-readable warning when the link/cache could not be
// written (tx failure) or the claim was swept mid-flight (0 rows settled). The caller
// NEVER reverts or retries on a non-empty warning — the forge issue is real and the next
// sync reconciles the local state; a stranded filing_since is reaped by the sweeper.
func (h *Handler) settleFiledIssue(ctx context.Context, claimID uuid.UUID, repo store.GetRepoForUserRow, created forge.Issue, description string) string {
	const warnUnlinked = "The issue was created on the forge, but linking it in uzi failed; the next sync will reconcile it."
	const warnReclaimed = "The issue was created on the forge, but its draft claim had already been reclaimed, so it isn't linked to this recommendation."

	labelsJSON, err := json.Marshal(created.Labels)
	if err != nil {
		slog.Warn("file issue: marshal labels", "error", err)
		return warnUnlinked
	}
	assigneeIDsJSON, err := json.Marshal(created.Assignees)
	if err != nil {
		slog.Warn("file issue: marshal assignee ids", "error", err)
		return warnUnlinked
	}
	updated := created.UpdatedAt
	if updated.IsZero() {
		updated = time.Now()
	}

	tx, err := h.pool.Begin(ctx)
	if err != nil {
		slog.Warn("file issue: begin settle tx", "error", err)
		return warnUnlinked
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after a successful Commit
	qtx := h.q.WithTx(tx)

	rows, err := qtx.SettleRecommendationFiledIssue(ctx, store.SettleRecommendationFiledIssueParams{
		FiledRepoID:   pgtype.UUID{Bytes: repo.ID, Valid: true},
		FiledIssueIid: pgtype.Int8{Int64: created.IID, Valid: true},
		FiledIssueUrl: created.WebURL,
		ID:            claimID,
	})
	if err != nil {
		slog.Warn("file issue: settle claim", "error", err)
		return warnUnlinked
	}
	// Cache the real, just-created issue (the same write issues.go performs) so the board
	// card appears without a poll — independent of whether the link settled.
	if _, err := qtx.UpsertIssue(ctx, store.UpsertIssueParams{
		RepoID:         repo.ID,
		ForgeIssueIid:  created.IID,
		Title:          created.Title,
		State:          created.State,
		Labels:         labelsJSON,
		AssigneeIds:    assigneeIDsJSON,
		WebUrl:         created.WebURL,
		Author:         pgconv.TextOrNull(created.Author),
		HasPrdLink:     forgesvc.HasPRDLink(description),
		ForgeUpdatedAt: pgtype.Timestamptz{Time: updated, Valid: true},
	}); err != nil {
		slog.Warn("file issue: cache issue", "error", err)
		return warnUnlinked
	}
	if err := tx.Commit(ctx); err != nil {
		slog.Warn("file issue: commit settle tx", "error", err)
		return warnUnlinked
	}
	if rows == 0 {
		// The claim was swept out from under a slow-but-successful CreateIssue
		// (Decision 7): the issue exists + is now cached, only the link is missing.
		return warnReclaimed
	}
	return ""
}
