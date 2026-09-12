package handler

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	"github.com/vtmocanu/uzi/api/internal/issuedraft"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// AdminGetJudgeIssueDraft serves the admin "All users" issue DRAFT (PRD #1184 M3): the
// templated, human-editable draft for filing a forge issue from a (category, target)
// coordinate's NEWEST OPEN occurrence across every user. It is the cross-user twin of
// GetIssueDraft, mounted in the admin READ group (RequireUser + RequireAdminRO), so a uza_
// admin_ro token reads it and a masked uzc_/non-admin session is 403 before the handler runs.
//
// The draft card is the ONE surface where attribution is intentionally shown (Decision 8):
// filing publishes a user's worker text to a forge, so the reader MUST see whose text they are
// about to publish. The Provenance line and the "on behalf of / from a retrospective of …'s run"
// footer are therefore populated here, unlike the anonymized aggregate list (§379's four-layer
// hiding does not apply to the draft). It renders the SAME apitypes.IssueDraftDTO through the
// SAME issuedraft.Render as the owner draft, reusing producerHandle/userHandle/resolveDefaultRepo/
// shortID verbatim.
//
// category is validated against the closed recommendation taxonomy and target must be non-empty
// (400 on either), matching the owner reads' closed-enum contract, so a typo cannot masquerade as
// an empty draft. A coordinate with no open occurrence — unknown, all filed, or all disposed — is
// a 404.
func (h *Handler) AdminGetJudgeIssueDraft(w http.ResponseWriter, r *http.Request) {
	admin, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	category := strings.TrimSpace(r.URL.Query().Get("category"))
	target := strings.TrimSpace(r.URL.Query().Get("target"))
	if !workersvc.ValidRecommendationCategory(category) {
		httpx.Error(w, http.StatusBadRequest, "invalid category")
		return
	}
	if target == "" {
		httpx.Error(w, http.StatusBadRequest, "target required")
		return
	}
	ctx := r.Context()

	occ, err := h.wsvc.AdminNewestOpenOccurrence(ctx, category, target)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.Error(w, http.StatusNotFound, "no open occurrence")
			return
		}
		slog.Error("admin issue draft: resolve occurrence", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}

	draft, defaultRepoID, defaultNote, uziLabel := h.renderAdminIssueDraft(ctx, admin, occ)

	httpx.JSON(w, http.StatusOK, map[string]any{"draft": apitypes.IssueDraftDTO{
		DefaultRepoID: defaultRepoID,
		Title:         draft.Title,
		Description:   draft.Description,
		Labels:        []string{uziLabel},
		Provenance:    draft.Provenance,
		DefaultNote:   defaultNote,
	}})
}

// renderAdminIssueDraft builds the draft body, provenance and default-repo picker for one
// resolved occurrence, sharing the owner draft's helpers. The repo PATH is resolved against the
// RUN OWNER (occ.ReviewOwnerUserID) — the judged run's repo, display-only, so a miss just drops
// the path — while the default-repo PRE-SELECTION is resolved against the ADMIN (the picker set
// is the caller's own connected repos, so an admin viewing another user's run opens with an empty
// picker + a reason, exactly as GetIssueDraft does for an admin viewer). producingRun is the
// produced-by run when present, else the judged run.
func (h *Handler) renderAdminIssueDraft(ctx context.Context, admin store.User, occ store.NewestOpenOccurrenceForCoordRow) (issuedraft.Draft, string, string, string) {
	var repoPath string
	if occ.RepoID.Valid {
		if rr, rerr := h.q.GetRepoForUser(ctx, store.GetRepoForUserParams{
			ID: uuid.UUID(occ.RepoID.Bytes), UserID: occ.ReviewOwnerUserID,
		}); rerr == nil {
			repoPath = rr.PathWithNamespace
		}
	}

	runURL := ""
	if base, berr := h.settings.PublicBaseURL(ctx); berr == nil && base != "" {
		runURL = strings.TrimRight(base, "/") + "/runs/" + occ.RunID.String()
	}

	// Labels are assembled server-side from settings (Decision 3/6), never the request body.
	uziLabel, _ := h.settings.UziLabel(ctx)

	defaultRepoID, defaultNote := h.resolveDefaultRepo(ctx, admin.ID, occ.Category, occ.RepoID)

	producingUser := h.producerHandle(ctx, admin, occ.ReviewOwnerUserID, occ.ProducedByUserID)
	producingRun := occ.RunID
	if occ.ProducedByRunID.Valid {
		producingRun = uuid.UUID(occ.ProducedByRunID.Bytes)
	}

	var issueIID int64
	if occ.IssueIid.Valid {
		issueIID = occ.IssueIid.Int64
	}

	draft := issuedraft.Render(issuedraft.Input{
		Category:            occ.Category,
		Target:              occ.Target,
		RationaleMd:         occ.RationaleMd,
		Confidence:          occ.Confidence,
		Verdict:             occ.Verdict,
		SummaryMd:           occ.SummaryMd,
		JudgeModel:          occ.JudgeModel,
		ReviewDate:          occ.ReviewDate.Time,
		RunKind:             occ.RunKind,
		RunStatus:           occ.RunStatus,
		RepoPath:            repoPath,
		IssueIID:            issueIID,
		RunURL:              runURL,
		RequestingUser:      userHandle(admin),
		ProducingUser:       producingUser,
		ProducingRunShortID: shortID(producingRun),
	})
	return draft, defaultRepoID, defaultNote, uziLabel
}

// adminFileIssueRequest is the admin file-issue body: the coordinate to file (category, target),
// the ADMIN's own repo to file into (repo_id), and the possibly-edited draft (title, description).
// It carries the coordinate instead of the owner path's run/rec ids — an occurrence id on the wire
// would be the attribution this design hides, so the coordinate is resolved server-side to its
// newest open occurrence at file time (decision log 2026-09-07). Mirrors the owner path's local
// fileIssueRequest struct; the response reuses fileIssueResponse/createdIssueDTO.
type adminFileIssueRequest struct {
	Category    string `json:"category"`
	Target      string `json:"target"`
	RepoID      string `json:"repo_id"`
	Title       string `json:"title"`
	Description string `json:"description"`
}

// AdminFileJudgeIssue files a forge issue from a (category, target) coordinate's NEWEST OPEN
// occurrence across all users (PRD #1184 M3). It is the cross-user twin of FileIssue, mounted in
// the admin WRITE group (RequireAuth + RequireAdmin) behind forgeLimiter.PerUserMiddleware: the
// write is cookie-only, so a uza_ admin_ro Bearer 401s at RequireAuth before this handler exists
// (§379's read/write split), while the draft read above stays CLI-reachable.
//
// It RESOLVES THE OCCURRENCE AGAIN at file time — the deliberate resolve-at-file-time behaviour
// (decision log 2026-09-07): a fresher review that landed since the draft moves the filed link to
// that newer occurrence, rather than putting an occurrence id on the wire. The filed link lands on
// the resolved coordinate, so that owner's row moves to `filed`.
//
// Then it reuses the owner filer verbatim: bound + write-boundary sanitize the CLIENT's title/body
// (Decision 10), caller-owns-repo against the ADMIN's own repo (GetRepoForUser, session-scoped so
// a repo the admin does not own is 404), server-side [uzi] label, claim-first
// (ClaimRecommendationFiledIssue → 409 on a lost claim), forge-first CreateIssue, and
// settleFiledIssue / revertFiledClaim.
func (h *Handler) AdminFileJudgeIssue(w http.ResponseWriter, r *http.Request) {
	admin, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var req adminFileIssueRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	category := strings.TrimSpace(req.Category)
	target := strings.TrimSpace(req.Target)
	if !workersvc.ValidRecommendationCategory(category) {
		httpx.Error(w, http.StatusBadRequest, "invalid category")
		return
	}
	if target == "" {
		httpx.Error(w, http.StatusBadRequest, "target required")
		return
	}
	repoID, err := uuid.Parse(strings.TrimSpace(req.RepoID))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid repo id")
		return
	}
	ctx := r.Context()

	// Resolve-at-file-time (decision log 2026-09-07): the link lands on whatever is newest NOW,
	// which may differ from the occurrence the draft was rendered from. No open occurrence — the
	// coordinate is unknown, or every occurrence is already filed/disposed — is a 404.
	occ, err := h.wsvc.AdminNewestOpenOccurrence(ctx, category, target)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.Error(w, http.StatusNotFound, "no open occurrence")
			return
		}
		slog.Error("admin file issue: resolve occurrence", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}

	// Bound then re-apply the write-boundary controls SERVER-SIDE on the client's body
	// (Decision 10): the client may have edited or replaced the draft, so its inertness is
	// re-established here, exactly as the owner filer does.
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

	// Caller-owns-repo (Decision 8 write side): the ADMIN's own repo. A repo the admin does not
	// own is 404 — an admin cannot file into someone else's repo any more than a plain user can.
	repo, err := h.q.GetRepoForUser(ctx, store.GetRepoForUserParams{ID: repoID, UserID: admin.ID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.Error(w, http.StatusNotFound, "repo not found")
			return
		}
		slog.Error("admin file issue: load repo", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}

	// Labels are assembled server-side (Decision 3/6): the single uzi run-eligibility label
	// (PRD #764), never from the request body.
	uziLabel, _ := h.settings.UziLabel(ctx)
	labels := []string{uziLabel}

	// Claim-first (Decision 7): the claim lands on the RESOLVED occurrence's coordinate, so that
	// owner's row moves to `filed`. A lost claim — already filed or mid-filing — is 409.
	claimID, err := h.q.ClaimRecommendationFiledIssue(ctx, store.ClaimRecommendationFiledIssueParams{
		ReviewID:      occ.ReviewID,
		Category:      occ.Category,
		Target:        occ.Target,
		FiledByUserID: pgtype.UUID{Bytes: admin.ID, Valid: true},
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.Error(w, http.StatusConflict, "this recommendation already has an issue, or one is being filed")
			return
		}
		slog.Error("admin file issue: claim", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}

	f, err := h.svc.ForgeForConnection(repo.ForgeType, repo.BaseUrl, repo.TokenCiphertext)
	if err != nil {
		h.revertFiledClaim(ctx, claimID)
		slog.Error("admin file issue: build forge for connection", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	created, err := f.CreateIssue(ctx, repo.ForgeProjectID, title, description, labels)
	if err != nil {
		// Forge rejected the write: the claim reverts, nothing persists, the coordinate is
		// fileable again. err is already PAT-redacted by the driver.
		h.revertFiledClaim(ctx, claimID)
		httpx.Error(w, http.StatusBadGateway, "could not create the issue on the forge: "+err.Error())
		return
	}

	// Forge-first done: settle the link + cache the issue in one tx (Decision 9). A tx failure or
	// a swept-out claim is created-with-warning — the real issue exists, so never revert/retry.
	warning := h.settleFiledIssue(ctx, claimID, repo, created, description)

	httpx.JSON(w, http.StatusCreated, fileIssueResponse{
		Issue:   createdIssueDTO{IID: created.IID, WebURL: created.WebURL, Title: created.Title},
		Warning: warning,
	})
}
