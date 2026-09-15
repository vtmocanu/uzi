package handler

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// SetRunCredential is the `uzi run set-token` verb for the NON-HELD run states (PRD #1247
// M4, D4/D6/D12): POST /api/runs/{id}/credential with {mode, secret_id?}. It is mounted in
// the RequireUser group (beside resume-now) so a uzc_ CLI Bearer reaches it — NOT the
// cookie-only RequireAuth group; a router-level auth test pins that mount.
//
// It decodes the body (a malformed secret_id uuid is a handler-level 400, before the
// service), then calls the owner-scoped service method, which reads the run, validates the
// override through the one validator, and — for a queued or parked run — writes the
// override and performs the state-specific transition. The typed errors map to statuses via
// writeSetRunCredentialError (the set-token-specific 409s and the 404) plus the shared
// writeCredentialOverrideError (the validator's 404/409/422/400). On success it returns
// {run: DTO, warning?: string} — the D6 warning rides the 200, never a refusal.
func (h *Handler) SetRunCredential(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	runID, ok := httpx.PathUUID(w, r, "id", "run")
	if !ok {
		return
	}
	var req struct {
		// Mode is one of pinned/auto/default/inherit; the service validates it. secret_id is
		// a *string (optional; set only for a pinned choice) so absence stays distinct from a
		// present value, mirroring the create path's inline credential_override struct.
		Mode     string  `json:"mode"`
		SecretID *string `json:"secret_id"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	var secretID *uuid.UUID
	if req.SecretID != nil {
		id, perr := uuid.Parse(strings.TrimSpace(*req.SecretID))
		if perr != nil {
			httpx.Error(w, http.StatusBadRequest, "secret_id must be a valid uuid")
			return
		}
		secretID = &id
	}

	res, err := h.wsvc.SetRunCredential(r.Context(), user.ID, runID, req.Mode, secretID)
	if err != nil {
		h.writeSetRunCredentialError(w, err)
		return
	}

	body := map[string]any{"run": runToDTO(res.Run, h.runPriorityClass(r.Context(), res.Run), h.cfg.RunTimeout, h.runExtensionCapSeconds(r.Context()), h.clock())}
	if res.Warning != "" {
		body["warning"] = res.Warning
	}
	httpx.JSON(w, http.StatusOK, body)
}

// writeSetRunCredentialError maps the set-token service's typed refusals to HTTP statuses
// (PRD #1247 M4). The set-token-specific 409s (held / claimed / terminal / raced) and the
// not-found 404 are handled here; everything else — the ONE validator's 404/409/422/400
// refusals — falls through to the shared writeCredentialOverrideError, which the create
// path (M2) also uses, so the two override write surfaces classify the same refusal the
// same way.
func (h *Handler) writeSetRunCredentialError(w http.ResponseWriter, err error) {
	var workerUnsupported *workersvc.CredentialSwitchWorkerUnsupportedError
	switch {
	case errors.Is(err, workersvc.ErrRunNotFound):
		httpx.Error(w, http.StatusNotFound, "run not found")
	case errors.As(err, &workerUnsupported):
		// PRD #1247 M5 (D4-step-9): the run's holding worker does not advertise the
		// credential_switch capability, so a held-state switch cannot be requested. Name the
		// worker so the operator knows which to upgrade (the worker name is owner-controlled
		// text but not attacker-supplied on this owner-scoped path).
		httpx.Error(w, http.StatusConflict, fmt.Sprintf("the worker running this run (%q) must be upgraded to support credential_switch before its token can be switched", workerUnsupported.WorkerName))
	case errors.Is(err, workersvc.ErrCredentialSwitchNoLiveWorker):
		httpx.Error(w, http.StatusConflict, "this run has no live worker to switch its token; retry once it is claimed")
	case errors.Is(err, workersvc.ErrCredentialSwitchClaimAssembling):
		httpx.Error(w, http.StatusConflict, "the claim is being assembled; retry in a moment")
	case errors.Is(err, workersvc.ErrCredentialSwitchRunTerminal):
		httpx.Error(w, http.StatusConflict, "run has already finished")
	case errors.Is(err, workersvc.ErrCredentialSwitchRaced):
		httpx.Error(w, http.StatusConflict, "the run changed state while switching its token; retry")
	default:
		h.writeCredentialOverrideError(w, err)
	}
}
