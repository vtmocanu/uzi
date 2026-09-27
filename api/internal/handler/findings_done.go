package handler

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// bulkMarkDoneMaxIDs bounds one BulkMarkFindingsDone request, the same cap as bulk dismiss.
const bulkMarkDoneMaxIDs = 100

// MarkFindingDone is the human "Mark done" on one finding coordinate (issue #1723): POST
// /api/findings/{id}/done, keyed on an EVIDENCE id like POST /findings/{id}/dismiss. A LOCAL write
// (no forge call, no spend), mounted on RequireUser without the forge limiter.
//
// Ownership is the owner-scoped GetIncidentalFinding((id, user.ID)): a foreign or unknown id is a
// 404 for every caller, admin included. The coordinate (user_id, repo_id, location) comes from
// that stored row, never from the request. MarkFindingDoneByCoordinate then applies the judge's
// disposition semantics in one statement: done from open, filed (issue link kept), dismissed
// (reason cleared) or done (a sync done becomes a human done), and an absent disposition is
// inserted as done with the canonical content hash and title of the stored evidence. A coordinate
// mid-filing returns no row (pgx.ErrNoRows), which is a 409.
func (h *Handler) MarkFindingDone(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	findingID, ok := httpx.PathUUID(w, r, "id", "finding")
	if !ok {
		return
	}
	ctx := r.Context()

	finding, err := h.q.GetIncidentalFinding(ctx, store.GetIncidentalFindingParams{ID: findingID, UserID: user.ID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.Error(w, http.StatusNotFound, "finding not found")
			return
		}
		slog.Error("mark finding done: get finding", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}

	row, err := h.q.MarkFindingDoneByCoordinate(ctx, store.MarkFindingDoneByCoordinateParams{
		UserID:      finding.UserID,
		RepoID:      finding.RepoID,
		Location:    finding.Location,
		ContentHash: workersvc.FindingContentHash(finding.Title, finding.DescriptionMd),
		LastTitle:   finding.Title,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// The ON CONFLICT ... WHERE status <> 'filing' guard updated nothing: the coordinate
			// has a forge issue being created right now.
			httpx.Error(w, http.StatusConflict, "cannot mark done (finding is being filed)")
			return
		}
		slog.Error("mark finding done: upsert", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}

	httpx.JSON(w, http.StatusOK, apitypes.MarkFindingDoneResultDTO{Status: "done", DispositionID: row.ID.String()})
}

// BulkMarkFindingsDone marks up to bulkMarkDoneMaxIDs owned coordinates done in one statement
// (issue #1723): POST /api/findings/done, keyed on DISPOSITION ids like bulk dismiss. Owner-scoped
// by the query's user_id filter; a foreign, unknown or mid-filing id is skipped silently, a
// duplicate id is applied once, and re-asserting done on a done row counts as applied. Over the
// cap or an unparseable id is a 400. Returns {updated, findings} with the re-read rows.
func (h *Handler) BulkMarkFindingsDone(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var req apitypes.BulkMarkFindingsDoneRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(req.IDs) > bulkMarkDoneMaxIDs {
		httpx.Error(w, http.StatusBadRequest, "too many ids (max 100)")
		return
	}
	ids := make([]uuid.UUID, 0, len(req.IDs))
	for _, raw := range req.IDs {
		parsed, err := uuid.Parse(raw)
		if err != nil {
			httpx.Error(w, http.StatusBadRequest, "invalid finding id")
			return
		}
		ids = append(ids, parsed)
	}

	rows, err := h.q.BulkMarkFindingsDone(r.Context(), store.BulkMarkFindingsDoneParams{
		UserID: user.ID,
		Ids:    ids,
	})
	if err != nil {
		slog.Error("bulk mark findings done", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}

	out := apitypes.BulkMarkFindingsDoneResultDTO{
		Updated:  len(rows),
		Findings: make([]apitypes.IncidentalFindingDTO, 0, len(rows)),
	}
	for _, row := range rows {
		out.Findings = append(out.Findings, findingDispositionDTO(row))
	}
	httpx.JSON(w, http.StatusOK, out)
}

// UndoFindingDisposition undoes a human verdict (issue #1723): DELETE
// /api/findings/{id}/disposition, keyed on the disposition id. A done coordinate that still
// carries its issue link returns to filed; every other done or dismissed coordinate returns to
// open (see the UndoFindingDisposition query). Owner-scoped and guarded to dismissed/done, so an
// open, filed, filing, foreign or unknown id matches zero rows and is a 404. Returns the undone
// coordinate so the client can reconcile the row in place.
func (h *Handler) UndoFindingDisposition(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	dispositionID, ok := httpx.PathUUID(w, r, "id", "finding")
	if !ok {
		return
	}

	row, err := h.q.UndoFindingDisposition(r.Context(), store.UndoFindingDispositionParams{
		UserID: user.ID,
		ID:     dispositionID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.Error(w, http.StatusNotFound, "no dismissed or done finding to undo")
			return
		}
		slog.Error("undo finding disposition", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, findingDispositionDTO(row))
}
