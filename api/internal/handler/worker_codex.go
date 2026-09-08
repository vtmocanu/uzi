package handler

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Codex worker credential-operation routes (PRD #1171 M1), ships DARK. These are the
// Bearer-only worker→API bridge over the coordinated-refresh service half (codexrefresh.go):
//
//	POST /worker/runs/{id}/codex/release  → ReleaseCodexAccessToken (both auth modes)
//	POST /worker/runs/{id}/codex/refresh  → CoordinatedCodexRefresh (subscription only)
//
// Trust-boundary rules, all fail-closed (PRD #1171 M1 §5, mirroring worker_forge.go):
//
//   - The path {id} is the SOLE run identity. The worker is authenticated by its Bearer
//     join token (RequireWorker mounts these under /worker), and the service derives the
//     account/credential from the OWNED run binding alone — never from the body.
//   - Request bodies are STRICT-decoded (httpx.DecodeJSON rejects unknown fields) and
//     carry ONLY capability / operation_id / observed_generation. A body that names a run
//     id (run_id/id), a user/secret/account id, or any token/login field is an UNKNOWN
//     field to these structs, so the strict decode rejects it with a 400 before the
//     service is ever called — there is no run-id-in-body to reconcile against the path.
//   - Every response is Cache-Control: no-store (set first thing, so it covers the
//     secret-bearing success body regardless of which branch writes the response).
//   - Service sentinel errors map to FIXED, coordinate-free strings (codexHTTPError). The
//     real error is logged server-side ONLY for the internal bucket; the response body
//     NEVER carries err.Error(), so no provider text, URL, status or secret leaks.

// Fixed, coordinate-free error strings for the codex worker routes (PRD #1171 M1 §5). None
// names a run, user, account, provider host/status or any credential coordinate, so the
// mapping is non-oracular: an unauthorized caller cannot distinguish "wrong run" from
// "wrong capability" from "run does not exist" beyond the deliberately-collapsed status.
const (
	codexErrAuth               = "worker authentication required"
	codexErrInvalid            = "invalid request"
	codexErrRunNotFound        = "run not found"
	codexErrNotAuthorized      = "codex credential operation not authorized"
	codexErrCredUnavailable    = "codex credential is not available" //nolint:gosec // G101: static user-facing error message, not a credential
	codexErrRefreshContended   = "codex refresh is contended; retry"
	codexErrRefreshUnavailable = "codex refresh is unavailable"
	codexErrRefreshNoClient    = "codex refresh is not available"
	codexErrInternal           = "codex operation failed"
)

// codexReleaseRequest is the STRICT body of POST /worker/runs/{id}/codex/release. It
// carries ONLY the run-scoped capability — the run id is the path, never the body.
type codexReleaseRequest struct {
	Capability string `json:"capability"`
}

// codexReleaseResponse releases ONLY the currently-committed access token. The auth mode
// and generation the worker needs already rode the claim (ClaimCodexSecrets); the release
// is a re-fetch of the committed token for a fresh provider root, and the service method
// (ReleaseCodexAccessToken) returns only the token, so this is the minimal secret-bearing
// body. It is Cache-Control: no-store.
type codexReleaseResponse struct {
	AccessToken string `json:"access_token"`
}

// codexRefreshRequest is the STRICT body of POST /worker/runs/{id}/codex/refresh. It
// carries ONLY the run-scoped capability, the caller-generated operation id (one per
// LOGICAL refresh, RETAINED and reused after a timeout/lost reply so a retry is idempotent
// — the worker never begins a second exchange blindly), and the observed generation the
// worker last saw (from its claim or a prior refresh result). The run id is the path.
type codexRefreshRequest struct {
	Capability         string `json:"capability"`
	OperationID        string `json:"operation_id"`
	ObservedGeneration int64  `json:"observed_generation"`
}

// codexRefreshResponse releases the freshly-committed access token plus the minimal
// generation/outcome facts a worker needs to track state and decide its next observed
// generation. Non-empty AccessToken only on a success outcome (advanced/replayed/
// reconciled); a contended/quarantined outcome rides an HTTP error and no body. It is
// Cache-Control: no-store.
type codexRefreshResponse struct {
	AccessToken string `json:"access_token"`
	Generation  int64  `json:"generation"`
	Outcome     string `json:"outcome"`
}

// WorkerCodexRelease releases the run's currently-committed Codex access token to the
// owning worker. POST /worker/runs/{id}/codex/release (PRD #1171 M1). Bearer-only,
// run-scoped; api_key and subscription runs both release their usable token.
func (h *Handler) WorkerCodexRelease(w http.ResponseWriter, r *http.Request) {
	// no-store first: it must cover the secret-bearing success body, and setting it before
	// any WriteHeader (httpx.JSON writes the header) is the only way to guarantee that.
	w.Header().Set("Cache-Control", "no-store")

	wkr, ok := mw.WorkerFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, codexErrAuth)
		return
	}
	runID, ok := httpx.PathUUIDMsg(w, r, "id", codexErrInvalid)
	if !ok {
		return
	}
	var req codexReleaseRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		// Strict decode: httpx.DecodeJSON DisallowUnknownFields on the single decoded body,
		// so malformed JSON OR any unknown field (a body-supplied run id, user/secret/account
		// id, or token/login field) fails closed as 400 here. It does NOT reject a trailing
		// value after the first, but a trailing value is simply not decoded — it cannot
		// smuggle a run id or credential field into req, because only the first value is
		// decoded and that value's unknown fields are already rejected. The path {id} remains
		// the sole run identity regardless.
		httpx.Error(w, http.StatusBadRequest, codexErrInvalid)
		return
	}
	if req.Capability == "" {
		httpx.Error(w, http.StatusBadRequest, codexErrInvalid)
		return
	}

	token, err := h.wsvc.ReleaseCodexAccessToken(r.Context(), wkr, runID, req.Capability)
	if err != nil {
		h.writeCodexError(w, "release", err)
		return
	}
	httpx.JSON(w, http.StatusOK, codexReleaseResponse{AccessToken: token})
}

// WorkerCodexRefresh runs the coordinated subscription refresh for the owning worker and
// releases the freshly-committed access token. POST /worker/runs/{id}/codex/refresh (PRD
// #1171 M1). Bearer-only, run-scoped. An api_key run is refused by the service's scope
// check (ScopeStartRefresh does not apply) and performs ZERO provider calls.
func (h *Handler) WorkerCodexRefresh(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")

	wkr, ok := mw.WorkerFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, codexErrAuth)
		return
	}
	runID, ok := httpx.PathUUIDMsg(w, r, "id", codexErrInvalid)
	if !ok {
		return
	}
	var req codexRefreshRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, codexErrInvalid)
		return
	}
	if req.Capability == "" {
		httpx.Error(w, http.StatusBadRequest, codexErrInvalid)
		return
	}
	// operation_id is a worker-generated, retained UUID; a malformed one is a bad request,
	// never a silently-substituted zero id.
	opID, perr := uuid.Parse(req.OperationID)
	if perr != nil {
		httpx.Error(w, http.StatusBadRequest, codexErrInvalid)
		return
	}

	res, err := h.wsvc.CoordinatedCodexRefresh(r.Context(), wkr, runID, req.Capability, opID, req.ObservedGeneration)
	if err != nil {
		h.writeCodexError(w, "refresh", err)
		return
	}
	httpx.JSON(w, http.StatusOK, codexRefreshResponse{
		AccessToken: res.AccessToken,
		Generation:  res.Generation,
		Outcome:     codexRefreshOutcomeString(res.Outcome),
	})
}

// writeCodexError maps a service error to its fixed HTTP status + coordinate-free body and
// writes it. The internal (unmapped) bucket is logged server-side with the REAL error so
// an operator can diagnose; the response body is always one of the fixed strings, never
// err.Error(), so provider text / URLs / statuses / secrets never leave the process.
func (h *Handler) writeCodexError(w http.ResponseWriter, op string, err error) {
	status, msg := codexHTTPError(err)
	if status == http.StatusInternalServerError {
		// Only the internal bucket is noisy-logged: the mapped sentinels are expected,
		// routine authority/state rejections. The real error stays server-side.
		slog.Error("worker codex "+op, "error", err)
	}
	httpx.Error(w, status, msg)
}

// codexHTTPError is the PURE error→(status, message) mapping (PRD #1171 M1 §5), split out
// so it is unit-testable without a DB and so the "leaks no provider text/secret" property
// is a property of one small function. It branches ONLY on the EXPORTED workersvc
// sentinels; every other error (including the package-private transient errors, raw store
// errors and the wrapped provider-exchange error) falls to the generic 500 bucket, whose
// fixed body carries no detail. The authorization/ownership cases collapse to one 404 so
// the mapping cannot be used as an oracle to tell "not owned" from "not bound" from "does
// not exist".
func codexHTTPError(err error) (int, string) {
	switch {
	// Ownership / binding / existence → one indistinguishable 404 (non-oracular).
	case errors.Is(err, workersvc.ErrRunNotOwned),
		errors.Is(err, workersvc.ErrCodexRunNotBound),
		errors.Is(err, workersvc.ErrCodexWorkerMismatch):
		return http.StatusNotFound, codexErrRunNotFound

	// Capability / scope / kind rejections → 403 (authorized worker, unauthorized op).
	case errors.Is(err, workersvc.ErrCodexCapabilityMismatch),
		errors.Is(err, workersvc.ErrCodexCapabilityEpoch),
		errors.Is(err, workersvc.ErrCodexScopeNotApplicable),
		errors.Is(err, workersvc.ErrCodexKindModeMismatch):
		return http.StatusForbidden, codexErrNotAuthorized

	// Refresh outcome: contended is the retry-and-reconcile signal (same op id).
	case errors.Is(err, workersvc.ErrCodexRefreshContended):
		return http.StatusConflict, codexErrRefreshContended

	// Refresh outcome: quarantined / unrecoverable / no-refresh-token — paused, no token.
	case errors.Is(err, workersvc.ErrCodexRefreshQuarantined),
		errors.Is(err, workersvc.ErrCodexRefreshUnrecoverable),
		errors.Is(err, workersvc.ErrCodexRefreshNoToken):
		return http.StatusConflict, codexErrRefreshUnavailable

	// The service was never wired with a refresh client — a deployment misconfiguration,
	// not the worker's fault; fail closed with a distinct, retryable-later 503.
	case errors.Is(err, workersvc.ErrCodexRefreshNoClient):
		return http.StatusServiceUnavailable, codexErrRefreshNoClient

	// Stale/quarantined credential STATE that makes the run not-serviceable right now
	// (a revoke, an alias replace, a not-yet-frozen identity, a park-state exclusion, a
	// quarantined account). Retryable/reconcilable; distinct from an authorization refusal.
	case errors.Is(err, workersvc.ErrCodexRunNotActivelyClaimed),
		errors.Is(err, workersvc.ErrCodexMaterialRevisionStale),
		errors.Is(err, workersvc.ErrCodexAccountKeyUnfrozen),
		errors.Is(err, workersvc.ErrCodexAccountTupleMismatch),
		errors.Is(err, workersvc.ErrCodexAccountRevisionStale),
		errors.Is(err, workersvc.ErrCodexAccountQuarantined),
		errors.Is(err, workersvc.ErrCodexBindingConflict):
		return http.StatusConflict, codexErrCredUnavailable

	default:
		// Everything else — the package-private transient errors (vault-locked, run-vanished,
		// credential-unavailable, store-unavailable), a raw store error, or the wrapped
		// provider-exchange error whose text may name the provider host/status. NONE of that
		// text reaches the body; it is the generic 500, logged server-side by writeCodexError.
		return http.StatusInternalServerError, codexErrInternal
	}
}

// codexRefreshOutcomeString renders a CoordinatedCodexRefresh outcome as a stable wire
// token for the worker's observability. The success outcomes (advanced/replayed/
// reconciled) are the only ones that ride a 200 with a token; the others accompany an HTTP
// error and are included here only so the mapping is total.
func codexRefreshOutcomeString(o workersvc.CodexRefreshOutcome) string {
	switch o {
	case workersvc.CodexRefreshAdvanced:
		return "advanced"
	case workersvc.CodexRefreshReplayed:
		return "replayed"
	case workersvc.CodexRefreshReconciled:
		return "reconciled"
	case workersvc.CodexRefreshContended:
		return "contended"
	case workersvc.CodexRefreshQuarantined:
		return "quarantined"
	default:
		return "unknown"
	}
}
