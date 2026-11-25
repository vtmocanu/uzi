package handler

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/httpx"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/providers"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Provider source and run-creation refusal reasons (PRD #4320 module 4). Each rides the
// httpx.ErrorReason envelope as {"error": "<message>", "reason": "<code>"}.
const (
	reasonProviderSourceUnusable        = "provider_source_unusable"
	reasonProviderLaneUnsupported       = "provider_lane_unsupported"
	reasonProviderModelUnmapped         = "provider_model_unmapped"
	reasonProviderBindingStale          = "provider_binding_stale"
	reasonProviderDestinationNotAllowed = "provider_destination_not_allowed"

	providerSourceUnusableMessagePrefix    = "the provider cannot be the source: "
	providerSourceUnusableRunMessagePrefix = "your source is a model provider that cannot serve a run: "
)

// providerSourceReasonText names a ProviderSourceUnusableError reason for the user.
func providerSourceReasonText(reason string) string {
	switch reason {
	case workersvc.SourceUnusableRetired:
		return "the provider is retired"
	case workersvc.SourceUnusableBaseURL:
		return "its base URL is not on the operator allowlist"
	case workersvc.SourceUnusableKeyMissing:
		return "it has no key"
	case workersvc.SourceUnusableKeyDisabled:
		return "its key is disabled"
	case workersvc.SourceUnusableProviderMissing:
		return "the provider no longer exists"
	default:
		return "the provider is not usable"
	}
}

// writeProviderCreateRefusal answers the typed provider refusals of run creation (409, stable
// reason codes) and reports whether it wrote one. A caller's error switch calls it before its
// default arm.
func writeProviderCreateRefusal(w http.ResponseWriter, err error) bool {
	var unusable *workersvc.ProviderSourceUnusableError
	var unmapped *workersvc.ProviderModelUnmappedError
	switch {
	case errors.As(err, &unusable):
		httpx.ErrorReason(w, http.StatusConflict,
			providerSourceUnusableRunMessagePrefix+providerSourceReasonText(unusable.Reason), reasonProviderSourceUnusable)
	case errors.Is(err, workersvc.ErrProviderSourceUnusable):
		httpx.ErrorReason(w, http.StatusConflict, "your source is a model provider that cannot serve a run", reasonProviderSourceUnusable)
	case errors.Is(err, workersvc.ErrProviderLaneUnsupported):
		httpx.ErrorReason(w, http.StatusConflict,
			"this run type does not support a model-provider source yet", reasonProviderLaneUnsupported)
	case errors.As(err, &unmapped):
		httpx.ErrorReason(w, http.StatusConflict,
			"the provider's model map does not map "+quoteModel(unmapped.Model)+" ("+unmapped.Source+"); map it or change the model", reasonProviderModelUnmapped)
	case errors.Is(err, workersvc.ErrProviderModelUnmapped):
		httpx.ErrorReason(w, http.StatusConflict, "the provider's model map does not map a model this run uses", reasonProviderModelUnmapped)
	case errors.Is(err, workersvc.ErrProviderBindingStale):
		httpx.ErrorReason(w, http.StatusConflict,
			"the provider key or revision this run was bound to changed; start a new run", reasonProviderBindingStale)
	case errors.Is(err, workersvc.ErrProviderDestinationNotAllowed):
		httpx.ErrorReason(w, http.StatusConflict,
			"the provider's base URL is no longer on the operator allowlist", reasonProviderDestinationNotAllowed)
	default:
		return false
	}
	return true
}

// quoteModel renders a model name for a message (names are validated tokens, never secrets).
func quoteModel(m string) string { return "\"" + m + "\"" }

// SetMyClaudeProviderSource is PUT /api/me/providers/claude-source: it selects (or, with null,
// clears) the caller's Claude model-provider source. The selection is refused unless the provider
// is visible to the caller (their own, or an admin-shared definition), not retired, its current base URL is on the operator allowlist and it has an
// enabled key (409 provider_source_unusable); a foreign or unknown provider is a 404. It runs under
// the per-user secret-mutation lock and applies the SAME usability rules run creation does
// (workersvc.ResolveProviderSource), by writing the selection and reading it back in the one
// transaction, so a refusal rolls the write back.
func (h *Handler) SetMyClaudeProviderSource(w http.ResponseWriter, r *http.Request) {
	h.setMyProviderSource(w, r, providers.EngineClaude)
}

// SetMyCodexProviderSource selects or clears only the owner's Codex source.
func (h *Handler) SetMyCodexProviderSource(w http.ResponseWriter, r *http.Request) {
	h.setMyProviderSource(w, r, providers.EngineCodex)
}
func (h *Handler) setMyProviderSource(w http.ResponseWriter, r *http.Request, engine string) {
	user, ok := userForProviderRoute(w, r)
	if !ok {
		return
	}
	var req struct {
		ProviderID json.RawMessage `json:"provider_id"`
	}
	if !decodeProviderBody(w, r, &req) {
		return
	}
	var want *uuid.UUID
	switch string(req.ProviderID) {
	case "":
		httpx.Error(w, http.StatusBadRequest, "provider_id is required: a provider id, or null to clear the source")
		return
	case "null":
	default:
		var raw string
		if err := json.Unmarshal(req.ProviderID, &raw); err != nil {
			httpx.Error(w, http.StatusBadRequest, "provider_id must be a provider id or null")
			return
		}
		id, err := uuid.Parse(raw)
		if err != nil {
			httpx.Error(w, http.StatusBadRequest, "provider_id must be a provider id or null")
			return
		}
		want = &id
	}
	err := h.withSecretLock(r.Context(), user.ID, nil, func(q *store.Queries) error {
		if want == nil {
			_, err := q.SetUserProviderSource(r.Context(), store.SetUserProviderSourceParams{Engine: engine, ID: user.ID})
			return err
		}
		// Freeze fence: the definition's row FOR SHARE (visible to the caller: their own, or a shared
		// one) is taken after the per-user secret lock and before the usability recheck, the same
		// order run creation uses, so an admin retire or model-map edit waits for this transaction
		// and a retire that committed first reads as unusable below.
		if _, err := q.LockModelProviderForShare(r.Context(), store.LockModelProviderForShareParams{ID: *want, OwnerUserID: user.ID}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return errProviderNotFound
			}
			return err
		}
		if profile, err := q.GetVisibleProviderForUse(r.Context(), store.GetVisibleProviderForUseParams{ID: *want, UserID: user.ID}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return errProviderNotFound
			}
			return err
		} else if profile.Engine != engine {
			// Includes a shared definition on /codex-source: a shared definition is Claude only.
			return errProviderNotFound
		}
		if _, err := q.SetUserProviderSource(r.Context(), store.SetUserProviderSourceParams{
			Engine: engine, ProviderID: pgconv.UUID(*want), ID: user.ID,
		}); err != nil {
			return err
		}
		res, err := workersvc.ResolveProviderSource(r.Context(), q, user.ID, engine, h.cfg.ProviderBaseURLAllowed)
		if err != nil {
			return err
		}
		return res.Err() // unusable: the transaction rolls the selection back
	})
	if err != nil {
		var unusable *workersvc.ProviderSourceUnusableError
		switch {
		case errors.As(err, &unusable):
			httpx.ErrorReason(w, http.StatusConflict,
				providerSourceUnusableMessagePrefix+providerSourceReasonText(unusable.Reason), reasonProviderSourceUnusable)
		case isPGForeignKeyViolation(err, "users_claude_provider_fkey"), isPGForeignKeyViolation(err, "users_codex_provider_fkey"):
			// Defensive backstop: the share lock blocks a delete, and a delete that committed first
			// yields no row (404 above), so this only fires if the provider is unusable by the caller.
			writeProviderError(w, errProviderNotFound)
		case writeProviderError(w, err):
		default:
			internalProviderError(w, "set provider source", err)
		}
		return
	}
	field := engine + "_provider_id"
	out := map[string]any{field: nil}
	if want != nil {
		out[field] = want.String()
	}
	httpx.JSON(w, http.StatusOK, out)
}
