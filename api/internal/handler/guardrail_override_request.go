package handler

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"unicode"

	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/privcheck"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// validateGuardrailReason is the SHARED reason validator for every #66 guardrail
// override write — the admin per-repo override (SetRepoGuardrailOverride), the member
// "request an override" (RequestGuardrailOverride), and the admin approve/reject
// decision note (M3). Extracting it into one helper is what keeps those paths from
// drifting on what a legal reason is.
//
// It trims, then rejects an empty reason (400), an over-long one (422 —
// maxGuardrailOverrideReasonBytes), or one carrying any control character (400). The
// reason is an audit note M9 renders in a badge, the admin blocked-repos list, the
// admin request queue, and potentially a plain-text CLI/log surface, so control
// characters (newline, CR, tab, ANSI ESC, the C1/Cc set unicode.IsControl covers) are
// rejected write-side: a forged audit line cannot be smuggled into a non-escaping sink,
// one check here instead of trusting every future renderer to escape (PRD #66 M8 audit
// hardening). On success it returns the trimmed reason with status==0; on rejection it
// returns the HTTP status and the message the caller emits verbatim.
func validateGuardrailReason(raw string) (clean string, status int, msg string) {
	clean = strings.TrimSpace(raw)
	if clean == "" {
		return "", http.StatusBadRequest, "a non-empty reason is required to override the guardrail"
	}
	if len(clean) > maxGuardrailOverrideReasonBytes {
		return "", http.StatusUnprocessableEntity, "reason is too long"
	}
	for _, ru := range clean {
		if unicode.IsControl(ru) {
			return "", http.StatusBadRequest, "reason must not contain control characters"
		}
	}
	return clean, 0, ""
}

// overrideRequestStateDTO maps a guardrail_override_requests row to the member-facing
// OverrideRequestStateDTO (PRD #1432). Shared by RequestGuardrailOverride's response and
// ListProjects' per-repo enrichment so the two never drift on how a stored row becomes
// the state the owner sees. DecidedAt / DecisionNote stay nil while the request is still
// pending — the pgtype .Valid discriminator is the only source, never a zero value.
func overrideRequestStateDTO(row store.GuardrailOverrideRequest) apitypes.OverrideRequestStateDTO {
	dto := apitypes.OverrideRequestStateDTO{
		Status:    row.Status,
		Reason:    row.Reason,
		CreatedAt: row.CreatedAt.Time,
	}
	if row.DecidedAt.Valid {
		t := row.DecidedAt.Time
		dto.DecidedAt = &t
	}
	if row.DecisionNote.Valid {
		s := row.DecisionNote.String
		dto.DecisionNote = &s
	}
	return dto
}

// RequestGuardrailOverride is the member "ask an admin to allow this blocked repo
// through the #66 guardrail" write (PRD #1432). It is OWNER-SCOPED and cookie-only
// (mounted in the RequireAuth repos group, mirroring SetRepoEnabled's posture): a member
// who could not enable a repo because the live guard refused it records a request an
// admin later approves or rejects (M3). It NEVER sets an override and NEVER enables the
// repo — only the admin write (SetRepoGuardrailOverride) and enable (SetRepoEnabled) do.
//
// It re-runs the LIVE guard with Overridden:false (never the stored report, D2), so the
// persisted findings snapshot is current and waivability is enforced server-side, not
// trusted from the client:
//   - a repo the live guard does NOT block → 409 (nothing to request; enable it
//     directly);
//   - a refusal whose block set is not fully waivable (protection_unreadable — the
//     fail-closed "protection could not be verified" case, which an admin override can
//     never waive, D8/D3) → 422 with "waivable": false and NO row persisted, so a member
//     never opens a request an admin could not honour;
//   - otherwise the SeverityBlock findings are snapshotted onto an upserted pending
//     request (one open request per repo — a re-request refreshes the same row).
func (h *Handler) RequestGuardrailOverride(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	id, ok := httpx.PathUUID(w, r, "id", "repo")
	if !ok {
		return
	}
	var req setGuardrailOverrideRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	reason, status, msg := validateGuardrailReason(req.Reason)
	if status != 0 {
		httpx.Error(w, status, msg)
		return
	}

	// Same owner-scoped preflight as SetRepoEnabled: a non-owned/unknown id is a 404,
	// resolved before any forge read.
	row, err := h.q.GetRepoForUser(r.Context(), store.GetRepoForUserParams{ID: id, UserID: user.ID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.Error(w, http.StatusNotFound, "repo not found")
			return
		}
		slog.Error("get repo for override request", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}

	// Run the live guard with Overridden:false so the findings are the RAW refusal set,
	// exactly what SetRepoEnabled evaluates (forge.go), never a stored or overridden
	// view. This is the same GuardInput shape the enable gate builds.
	res := h.pcheck.GuardRepo(r.Context(), privcheck.GuardInput{
		ForgeType:       row.ForgeType,
		BaseURL:         row.BaseUrl,
		TokenCiphertext: row.TokenCiphertext,
		Repo: privcheck.Repo{
			ID:             row.ID.String(),
			Path:           row.PathWithNamespace,
			ForgeProjectID: row.ForgeProjectID,
			DefaultBranch:  row.DefaultBranch.String,
		},
		Overridden: false,
	})
	if !res.Blocked {
		// Not blocked → there is nothing to request; the member can enable directly.
		httpx.JSON(w, http.StatusConflict, map[string]any{
			"error": "this repo is not blocked by the guardrail — enable it directly.",
		})
		return
	}
	if !privcheck.AllBlocksWaivable(res.Findings) {
		// A non-waivable block (protection_unreadable) can never be cleared by an admin
		// override (D8/D3), so refuse here rather than opening a doomed request.
		httpx.JSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":      "this refusal cannot be waived by an admin override (branch protection could not be verified); fix protection on the forge, then retry Enable.",
			"violations": blockFindingMessages(res.Findings),
			"waivable":   false,
		})
		return
	}

	// Snapshot the coded SeverityBlock findings that caused the refusal for the
	// findings jsonb column — audit/display only, no gate reads it back. The stored
	// shape is exactly the GuardrailFindingDTO the admin queue (M3) reads back.
	findingsJSON, err := json.Marshal(blockFindingDTOs(res.Findings))
	if err != nil {
		slog.Error("marshal override request findings", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	reqRow, err := h.q.UpsertGuardrailOverrideRequest(r.Context(), store.UpsertGuardrailOverrideRequestParams{
		RepoID:      id,
		RequestedBy: user.ID,
		Reason:      reason,
		Findings:    findingsJSON,
	})
	if err != nil {
		slog.Error("upsert guardrail override request", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"override_request": overrideRequestStateDTO(reqRow)})
}
