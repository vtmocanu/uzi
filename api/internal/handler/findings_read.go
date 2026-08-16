package handler

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// ListFindings serves the per-repo Findings backlog (PRD #333 M4, D7/D8): every finding
// coordinate the caller owns, deduped by (repo, location), with each coordinate's disposition
// status, "seen in N runs" count, actionable evidence id, and the D8 open-findings count in
// the response meta. It mirrors JudgeRecommendations — RequireUser (so `uzi findings list`
// works from a CLI token), owner-scoped by the query's user_id filter, read-only (no forge
// write, no token spend).
//
// ?bucket=to_file|filed|dismissed|all (default to_file) filters by disposition status;
// ?repo=<uuid> and ?run=<uuid> narrow by coordinate repo and by a run semi-join. All three are
// validated here — an unknown bucket or an unparseable repo/run id is a 400 rather than a
// silently-ignored filter, so a typo can never look like an empty backlog. A well-formed but
// foreign/unknown repo or run is NOT an error: it returns an empty list, leaking no existence
// oracle (the owner-scoped query matches none of another user's coordinates).
func (h *Handler) ListFindings(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	bucket := r.URL.Query().Get("bucket")
	if bucket == "" {
		// The backlog's reason to exist: what still needs filing. Referenced, not spelled,
		// so drifting BucketToFile cannot 400 every default-bucket request.
		bucket = workersvc.BucketToFile
	}
	if !workersvc.ValidFindingBucket(bucket) {
		httpx.Error(w, http.StatusBadRequest, "invalid bucket")
		return
	}
	var repoFilter uuid.UUID
	if raw := r.URL.Query().Get("repo"); raw != "" {
		parsed, err := uuid.Parse(raw)
		if err != nil {
			httpx.Error(w, http.StatusBadRequest, "invalid repo id")
			return
		}
		repoFilter = parsed
	}
	var runFilter uuid.UUID
	if raw := r.URL.Query().Get("run"); raw != "" {
		parsed, err := uuid.Parse(raw)
		if err != nil {
			httpx.Error(w, http.StatusBadRequest, "invalid run id")
			return
		}
		runFilter = parsed
	}

	backlog, err := h.wsvc.FindingsBacklog(r.Context(), user.ID, bucket, repoFilter, runFilter)
	if err != nil {
		slog.Error("findings backlog", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, backlog)
}

// GetFindingIssueDraft serves the templated, human-editable draft for filing a forge issue
// from one incidental finding (PRD #333 M4, D4). It is a READ: owner-scoped (GetIncidental
// Finding's (id, user_id) match — a foreign or unknown id is a 404, no existence oracle),
// mounted on RequireUser so a CLI token can read it. No forge write, no token spend. The body
// is rendered deterministically by issuedraft.RenderFinding, which routes every untrusted
// field through the field-level sanitisers (title/description/location) — NOT issuedraft.Render
// (judge-hardcoded). Per D4 this draft is a UX convenience; the M5 file POST re-applies the
// write-boundary controls to the client's (possibly edited) body.
func (h *Handler) GetFindingIssueDraft(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	findingID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid finding id")
		return
	}
	ctx := r.Context()

	finding, err := h.q.GetIncidentalFinding(ctx, store.GetIncidentalFindingParams{ID: findingID, UserID: user.ID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.Error(w, http.StatusNotFound, "finding not found")
			return
		}
		slog.Error("finding issue draft: get finding", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}

	// The draft is built by the SAME buildFindingDraft that FileFinding (M5) uses for its
	// default filed text, so this preview is byte-identical to what an omitted body files.
	// The reporting run supplies the provenance footer (kind + issue iid) and the repo path,
	// both owner-scoped and display-only — a lookup miss just drops that line, never fails the
	// draft.
	draft := h.buildFindingDraft(ctx, finding)

	httpx.JSON(w, http.StatusOK, apitypes.IncidentalFindingIssueDraftDTO{
		Title:       draft.Title,
		Description: draft.Description,
		Location:    draft.Location,
		// The stored, already-sanitised label suggestions seed the editable selection; the
		// server-mandated marker is added at file time (M5, D5), never surfaced here as an
		// editable chip.
		Labels:     decodeFindingLabels(finding.Labels),
		Provenance: draft.Provenance,
	})
}

// decodeFindingLabels decodes the finding row's jsonb labels into a slice, normalising a
// nil/invalid blob to an empty slice so the DTO always carries a JSON array, never null.
func decodeFindingLabels(raw []byte) []string {
	out := []string{}
	if len(raw) == 0 {
		return out
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return []string{}
	}
	return out
}
