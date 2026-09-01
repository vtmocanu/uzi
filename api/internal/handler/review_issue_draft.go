package handler

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	"github.com/vtmocanu/uzi/api/internal/issuedraft"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// GetIssueDraft serves the templated, human-editable draft for filing a forge issue
// from one judge recommendation (PRD #68 M2, Decision 2). It is a READ: owner-or-admin
// scoped exactly like GetRunReview (GetReviewForTarget → GetRunForViewer), mounted on
// RequireUser so a CSRF-safe CLI token can read it too (Interactions). No forge write,
// no token spend. The body is rendered deterministically by the issuedraft package,
// which fences + strips + secret-scans every untrusted field — but per Decision 10 this
// draft is a UX convenience, NOT the security boundary: the M3 POST handler re-applies
// the write-boundary controls to the client's (possibly edited) body.
func (h *Handler) GetIssueDraft(w http.ResponseWriter, r *http.Request) {
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
	ctx := r.Context()

	// Owner-or-admin visibility on the judged run (mirrors GetRunReview). A run the
	// caller can't see is 404, identical to an unknown id.
	run, err := h.wsvc.GetRunForViewer(ctx, user.ID, user.IsAdmin, runID)
	if err != nil {
		if errors.Is(err, workersvc.ErrRunNotFound) {
			httpx.Error(w, http.StatusNotFound, "run not found")
			return
		}
		slog.Error("issue draft: get run", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	res, err := h.wsvc.GetReviewForTarget(ctx, user.ID, user.IsAdmin, runID)
	if err != nil {
		if errors.Is(err, workersvc.ErrRunNotFound) {
			httpx.Error(w, http.StatusNotFound, "run not found")
			return
		}
		slog.Error("issue draft: get review", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	// A visible-but-unjudged run has no recommendation to draft. Report the same 404 as
	// an unknown recID — a missing coordinate, not an error.
	if res == nil {
		httpx.Error(w, http.StatusNotFound, "recommendation not found")
		return
	}
	rec, found := findRecommendation(res.Recommendations, recID)
	if !found {
		httpx.Error(w, http.StatusNotFound, "recommendation not found")
		return
	}

	// The judged run's repo, for the Context section + deep link. Owner-scoped (the run
	// owner always owns its repo), so this resolves even when an admin is the viewer;
	// display-only, so a lookup miss just drops the path.
	var repoPath string
	if run.RepoID.Valid {
		if rr, rerr := h.q.GetRepoForUser(ctx, store.GetRepoForUserParams{
			ID: uuid.UUID(run.RepoID.Bytes), UserID: run.UserID,
		}); rerr == nil {
			repoPath = rr.PathWithNamespace
		}
	}

	runURL := ""
	if base, berr := h.settings.PublicBaseURL(ctx); berr == nil && base != "" {
		runURL = strings.TrimRight(base, "/") + "/runs/" + run.ID.String()
	}

	// Labels are assembled server-side from settings (Decision 3/6) — never the request
	// body, and never autopilot. PRD #764: a filed issue carries the single uzi
	// run-eligibility label. The draft only DISPLAYS them; M3 re-derives them the same
	// way for the actual write.
	uziLabel, _ := h.settings.UziLabel(ctx)

	defaultRepoID, defaultNote := h.resolveDefaultRepo(ctx, user.ID, rec.Category, run.RepoID)

	producingUser := h.producerHandle(ctx, user, res.Review.UserID, rec.ProducedByUserID)
	producingRun := run.ID
	if rec.ProducedByRunID.Valid {
		producingRun = uuid.UUID(rec.ProducedByRunID.Bytes)
	}

	var issueIID int64
	if run.IssueIid.Valid {
		issueIID = run.IssueIid.Int64
	}

	draft := issuedraft.Render(issuedraft.Input{
		Category:            rec.Category,
		Target:              rec.Target,
		RationaleMd:         rec.RationaleMd,
		Confidence:          rec.Confidence,
		Verdict:             res.Review.Verdict,
		SummaryMd:           res.Review.SummaryMd,
		JudgeModel:          res.Review.JudgeModel,
		ReviewDate:          res.Review.UpdatedAt.Time,
		RunKind:             run.Kind,
		RunStatus:           run.Status,
		RepoPath:            repoPath,
		IssueIID:            issueIID,
		RunURL:              runURL,
		RequestingUser:      userHandle(user),
		ProducingUser:       producingUser,
		ProducingRunShortID: shortID(producingRun),
	})

	httpx.JSON(w, http.StatusOK, map[string]any{"draft": apitypes.IssueDraftDTO{
		DefaultRepoID: defaultRepoID,
		Title:         draft.Title,
		Description:   draft.Description,
		Labels:        []string{uziLabel},
		Provenance:    draft.Provenance,
		DefaultNote:   defaultNote,
	}})
}

func findRecommendation(recs []store.ReviewRecommendation, id uuid.UUID) (store.ReviewRecommendation, bool) {
	for _, rc := range recs {
		if rc.ID == id {
			return rc, true
		}
	}
	return store.ReviewRecommendation{}, false
}

// resolveDefaultRepo implements the category→repo default (Decision 4). The default is a
// PRE-SELECTION, not a constraint: it is returned only when it is one of the CALLER's
// enabled repos (the picker set), so it stays a valid pick. When nothing resolves — the
// judged run's repo isn't the caller's (an admin viewing another user's run), or the
// caller has no self_improve default schedule enabled / its repo isn't the caller's — the
// draft opens with an empty picker and a reason (mock state D) rather than guessing.
//
// PRD #590 M2: the uzi-own-repo default was formerly read from the instance-wide
// selfimprove_repo setting (deleted with the engine). It now comes from the CALLER's own
// enabled `self-improve` default schedule (per-user), which is the direct successor of
// that setting under the new per-user model.
func (h *Handler) resolveDefaultRepo(ctx context.Context, callerID uuid.UUID, category string, runRepo pgtype.UUID) (string, string) {
	var candidate uuid.UUID
	var have bool
	var okNote string
	switch category {
	case "improve_agent", "add_agent":
		if runRepo.Valid {
			candidate, have = uuid.UUID(runRepo.Bytes), true
		}
		okNote = "Defaulted to the judged run's repo — repo agents live in its .claude/agents/. Pick any repo you have connected."
	default: // improve_uzi, install_worker_tool, adjust_template, enable_tool, cost_efficiency
		if rows, err := h.q.ListEnabledDefaultsForUser(ctx, callerID); err == nil {
			for _, row := range rows {
				if row.Enabled && row.CatalogSlug.String == "self-improve" {
					candidate, have = row.RepoID, true
					break
				}
			}
		}
		okNote = "Defaulted from the category to your self-improvement repo. Pick any repo you have connected."
	}
	if have {
		if repos, err := h.q.ListEnabledReposForUser(ctx, callerID); err == nil {
			for _, rp := range repos {
				if rp.ID == candidate {
					return candidate.String(), okNote
				}
			}
		}
	}
	switch category {
	case "improve_agent", "add_agent":
		return "", "The judged run's repo isn't one you've connected. Pick the repo to file this against."
	default:
		return "", "No uzi repo is configured on this instance (or it isn't one you've connected), so there's no default. Pick the repo to file this against."
	}
}

// producerHandle resolves the display handle of the user whose worker produced the
// recommendation text (Decision 8 provenance). Prefers produced_by_user_id, falling back
// to the review owner; uses the session user directly when it is the producer (no query).
// A lookup miss yields "" so the renderer drops the provenance line rather than guessing.
func (h *Handler) producerHandle(ctx context.Context, caller store.User, reviewOwner uuid.UUID, producedBy pgtype.UUID) string {
	producer := reviewOwner
	if producedBy.Valid {
		producer = uuid.UUID(producedBy.Bytes)
	}
	if producer == caller.ID {
		return userHandle(caller)
	}
	pu, err := h.q.GetUserByID(ctx, producer)
	if err != nil {
		return ""
	}
	return userHandle(pu)
}

// userHandle is the short "@handle" for a user: the display name if set, else the email
// local-part. Used for the draft footer + provenance line (display only).
func userHandle(u store.User) string {
	if u.DisplayName.Valid {
		if dn := strings.TrimSpace(u.DisplayName.String); dn != "" {
			return dn
		}
	}
	if i := strings.IndexByte(u.Email, '@'); i > 0 {
		return u.Email[:i]
	}
	return u.Email
}

func shortID(id uuid.UUID) string {
	s := id.String()
	if len(s) >= 8 {
		return s[:8]
	}
	return s
}
