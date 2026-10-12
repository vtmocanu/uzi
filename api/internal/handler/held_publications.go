package handler

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Owner surface of held-work publication (issue #2545): the refs a failed run's work was
// published to (refs/uzi-held/<run-id>/<generation>), and an early expiry. Both routes sit in the
// RequireUser /runs group, so the web run page and the CLI (uzc_ Bearer) reach them; a worker
// token is refused by the middleware. Ownership is the strict GetRun owner-or-404 gate (an admin
// acting on a foreign run is refused, as for the archive and hold routes beside them). The
// publication switch (UZI_HELD_PUBLICATION) gates only new step A and B work, so these routes
// answer with it off too: refs already published must stay visible and releasable.

func heldTimePtr(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	v := t.Time
	return &v
}

// heldPublicationDTO maps a row to the owner DTO. last_error is stored sanitized; it is
// sanitized again here so no row written by an older build can reach the owner unfiltered.
func heldPublicationDTO(p store.RunHeldPublication) apitypes.HeldPublicationDTO {
	dto := apitypes.HeldPublicationDTO{
		ID: p.ID.String(), RunID: p.RunID.String(), Generation: p.Generation, Ref: p.Ref, Tip: p.Tip, State: p.State,
		CreatedAt: p.CreatedAt.Time, RefCreatedAt: heldTimePtr(p.RefCreatedAt), AcknowledgedAt: heldTimePtr(p.AcknowledgedAt),
		ExpiresAt: heldTimePtr(p.ExpiresAt), OwnerExpiredAt: heldTimePtr(p.OwnerExpiredAt), DeletedAt: heldTimePtr(p.DeletedAt),
	}
	if p.LastError.Valid {
		dto.LastError = workersvc.SanitizeHeldText(p.LastError.String)
	}
	if p.RefusalReason.Valid {
		dto.RefusalReason = p.RefusalReason.String
	}
	return dto
}

func heldPublicationDTOs(rows []store.RunHeldPublication) []apitypes.HeldPublicationDTO {
	out := make([]apitypes.HeldPublicationDTO, 0, len(rows))
	for _, p := range rows {
		out = append(out, heldPublicationDTO(p))
	}
	return out
}

// ListRunHeldPublications answers GET /api/runs/{id}/held-publications.
func (h *Handler) ListRunHeldPublications(w http.ResponseWriter, r *http.Request) {
	runID, user, ok := h.heldOwnerRun(w, r, "held publications list")
	if !ok {
		return
	}
	rows, err := h.q.ListHeldPublicationsForRun(r.Context(), store.ListHeldPublicationsForRunParams{RunID: runID, UserID: user})
	if err != nil {
		slog.Error("held publications list", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, apitypes.HeldPublicationsDTO{Publications: heldPublicationDTOs(rows)})
}

// ExpireRunHeldPublications answers POST /api/runs/{id}/held-publications/expire: every
// ACKNOWLEDGED publication of the caller's run becomes due now (the cleanup pass deletes the ref).
// It deletes nothing itself. A publication that is not acknowledged is never touched, so a run
// whose work is still held by an open hold keeps its ref.
func (h *Handler) ExpireRunHeldPublications(w http.ResponseWriter, r *http.Request) {
	runID, user, ok := h.heldOwnerRun(w, r, "held publications expire")
	if !ok {
		return
	}
	rows, err := h.q.OwnerExpireHeldPublications(r.Context(), store.OwnerExpireHeldPublicationsParams{RunID: runID, UserID: user})
	if err != nil {
		slog.Error("held publications expire", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, apitypes.HeldPublicationsExpireDTO{Expired: len(rows), Publications: heldPublicationDTOs(rows)})
}

// heldOwnerRun resolves the path run for its owner (strict owner-or-404) and writes the failure
// response itself. The returned user id is the owner.
func (h *Handler) heldOwnerRun(w http.ResponseWriter, r *http.Request, op string) (runID, userID uuid.UUID, ok bool) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return runID, userID, false
	}
	id, ok := httpx.PathUUID(w, r, "id", "run")
	if !ok {
		return runID, userID, false
	}
	if _, err := h.wsvc.GetRun(r.Context(), user.ID, id); err != nil {
		if errors.Is(err, workersvc.ErrRunNotFound) {
			httpx.Error(w, http.StatusNotFound, "run not found")
			return runID, userID, false
		}
		slog.Error(op+": get run", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return runID, userID, false
	}
	return id, user.ID, true
}
