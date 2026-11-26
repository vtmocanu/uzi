package handler

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	"github.com/vtmocanu/uzi/api/internal/providers"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Admin-shared provider definitions (PRD #4324). A shared definition has no owner: an admin fixes its
// gateway URL and auth style at creation and may later change only its name and model map, while each
// user attaches their own key through the ordinary key routes. Reads are a RequireAdminRO surface (a
// uza_ token may list and show); every write is cookie-only (RequireAuth + RequireAdmin).
//
// Locking: create takes the advisory catalogue lock; edit, retire and delete take the definition row
// lock first. None of them takes the per-user secret-mutation lock (withSecretLock): a recipient's
// select takes that lock, then the definition FOR SHARE, so an admin path that held the definition and
// then asked for a per-user lock would cycle with it.

const (
	// maxSharedProviders is the cap on shared definitions across all admins, retired ones included.
	maxSharedProviders = 50

	// sharedProviderNameIndex is the case-insensitive unique index over shared definition names.
	sharedProviderNameIndex = "model_providers_shared_name_lower_key"
)

var (
	errSharedProviderCap   = errors.New("shared provider limit reached")
	errSharedProviderInUse = errors.New("shared provider is selected as a source")
)

// sharedProviderMode is the credential_mode of every admin-shared definition (model_providers_credential_mode_check).
const sharedProviderMode = "per_user_key"

// sharedProviderAdminRow is the shape of GetSharedModelProviderAdminRow and ListSharedModelProvidersAdminRow.
type sharedProviderAdminRow = store.GetSharedModelProviderAdminRow

func adminSharedProviderDTO(row sharedProviderAdminRow) apitypes.AdminSharedProviderDTO {
	return apitypes.AdminSharedProviderDTO{
		ID:                row.ID.String(),
		Name:              row.Name,
		Engine:            row.Engine,
		Retired:           row.RetiredAt.Valid,
		CurrentRevision:   int(row.CurrentRevision),
		Protocol:          row.Protocol,
		BaseURL:           row.BaseUrl,
		AuthStyle:         row.AuthStyle,
		ModelMap:          modelMapDTO(row.ModelMap),
		CredentialMode:    sharedProviderMode,
		SelectedUserCount: row.SelectedUserCount,
		AttachedKeyCount:  row.AttachedKeyCount,
	}
}

// withAdminTx runs fn in one READ COMMITTED transaction with no advisory or per-user lock of its own.
func (h *Handler) withAdminTx(ctx context.Context, fn func(q *store.Queries) error) error {
	var tx pgx.Tx
	var err error
	switch {
	case h.secretBeginTx != nil:
		tx, err = h.secretBeginTx(ctx)
	case h.pool != nil:
		tx, err = h.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	default:
		return store.ErrVaultTransactionRequired
	}
	if err != nil {
		return err
	}
	if tx == nil {
		return store.ErrVaultTransactionRequired
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(cleanup)
	}()
	if _, err := tx.Exec(ctx, "SET TRANSACTION ISOLATION LEVEL READ COMMITTED"); err != nil {
		return err
	}
	if err := fn(store.New(tx)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// writeSharedProviderError maps the admin transaction's sentinel errors, then the shared ones.
func writeSharedProviderError(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, errSharedProviderCap):
		httpx.ErrorReason(w, http.StatusConflict,
			"shared provider limit reached (50 definitions, retired ones included)", reasonProviderLimit)
	case errors.Is(err, errSharedProviderInUse),
		isPGForeignKeyViolation(err, "users_claude_provider_fkey"),
		isPGForeignKeyViolation(err, "users_codex_provider_fkey"):
		httpx.ErrorReason(w, http.StatusConflict,
			"the provider is selected as a source by at least one user; they must choose another source before it can be deleted",
			reasonProviderInUseAsSource)
	case isPGUnique(err, sharedProviderNameIndex):
		httpx.ErrorReason(w, http.StatusConflict, "a shared provider with that name already exists", reasonProviderNameTaken)
	default:
		return writeProviderError(w, err)
	}
	return true
}

// AdminListSharedProviders is GET /api/admin/providers: every shared definition with its selected-user
// and attached-key counts.
func (h *Handler) AdminListSharedProviders(w http.ResponseWriter, r *http.Request) {
	if _, ok := egressProfileAdmin(w, r); !ok {
		return
	}
	rows, err := h.q.ListSharedModelProvidersAdmin(r.Context())
	if err != nil {
		internalProviderError(w, "list shared model providers", err)
		return
	}
	out := apitypes.AdminSharedProviderListDTO{Providers: []apitypes.AdminSharedProviderDTO{}}
	for _, row := range rows {
		out.Providers = append(out.Providers, adminSharedProviderDTO(sharedProviderAdminRow(row)))
	}
	httpx.JSON(w, http.StatusOK, out)
}

// AdminGetSharedProvider is GET /api/admin/providers/{id}. A personal provider id is a 404.
func (h *Handler) AdminGetSharedProvider(w http.ResponseWriter, r *http.Request) {
	if _, ok := egressProfileAdmin(w, r); !ok {
		return
	}
	id, ok := httpx.PathUUID(w, r, "id", "provider")
	if !ok {
		return
	}
	row, err := h.q.GetSharedModelProviderAdmin(r.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.Error(w, http.StatusNotFound, "provider not found")
		return
	}
	if err != nil {
		internalProviderError(w, "get shared model provider", err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"provider": adminSharedProviderDTO(row)})
}

// AdminCreateSharedProvider is POST /api/admin/providers: a keyless shared definition with revision 1.
// The engine is claude (the default when omitted) or codex; any other engine is a 400 and writes no
// row. An omitted protocol defaults per engine (anthropic_messages, openai_responses).
func (h *Handler) AdminCreateSharedProvider(w http.ResponseWriter, r *http.Request) {
	actor, ok := egressProfileAdmin(w, r)
	if !ok {
		return
	}
	var req struct {
		Name      string             `json:"name"`
		Engine    string             `json:"engine"`
		Protocol  string             `json:"protocol"`
		BaseURL   string             `json:"base_url"`
		AuthStyle string             `json:"auth_style"`
		ModelMap  providers.ModelMap `json:"model_map"`
	}
	if !decodeProviderBody(w, r, &req, providerKeyFieldRefusals) {
		return
	}
	name, err := validateSecretLabel(req.Name)
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "name: "+err.Error())
		return
	}
	if req.Engine == "" {
		req.Engine = providers.EngineClaude
	}
	if req.Engine != providers.EngineClaude && req.Engine != providers.EngineCodex {
		httpx.Error(w, http.StatusBadRequest, "a shared provider supports the claude and codex engines only")
		return
	}
	if req.Protocol == "" {
		req.Protocol = providers.ProtocolAnth
		if req.Engine == providers.EngineCodex {
			req.Protocol = providers.ProtocolResponses
		}
	}
	if err := providers.ValidateProfile(req.Engine, req.Protocol, req.AuthStyle, req.ModelMap); err != nil {
		httpx.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	baseURL, ok := h.allowedProviderBaseURL(w, req.BaseURL)
	if !ok {
		return
	}
	mm, err := modelMapJSON(req.ModelMap)
	if err != nil {
		internalProviderError(w, "marshal model map", err)
		return
	}

	var row sharedProviderAdminRow
	err = h.withAdminTx(r.Context(), func(q *store.Queries) error {
		if err := q.LockSharedModelProviderCatalogue(r.Context()); err != nil {
			return err
		}
		n, err := q.CountSharedModelProviders(r.Context())
		if err != nil {
			return err
		}
		if n >= maxSharedProviders {
			return errSharedProviderCap
		}
		p, err := q.InsertSharedModelProvider(r.Context(), store.InsertSharedModelProviderParams{Name: name, Engine: req.Engine})
		if err != nil {
			return err
		}
		if err := q.InsertModelProviderRevision(r.Context(), store.InsertModelProviderRevisionParams{
			ProviderID: p.ID, Revision: 1, Protocol: req.Protocol,
			BaseUrl: baseURL, AuthStyle: req.AuthStyle, ModelMap: mm,
		}); err != nil {
			return err
		}
		row, err = q.GetSharedModelProviderAdmin(r.Context(), p.ID)
		return err
	})
	if err != nil {
		if writeSharedProviderError(w, err) {
			return
		}
		internalProviderError(w, "create shared model provider", err)
		return
	}
	auditProvider("admin_create", actor.ID, row.ID)
	httpx.JSON(w, http.StatusCreated, map[string]any{"provider": adminSharedProviderDTO(row)})
}

// AdminPatchSharedProvider is PATCH /api/admin/providers/{id}: rename and/or replace the model map.
// base_url, auth_style, engine and protocol are fixed at creation: sending one is a 400. The new
// revision copies the destination from the current one.
func (h *Handler) AdminPatchSharedProvider(w http.ResponseWriter, r *http.Request) {
	actor, ok := egressProfileAdmin(w, r)
	if !ok {
		return
	}
	id, ok := httpx.PathUUID(w, r, "id", "provider")
	if !ok {
		return
	}
	var req struct {
		Name     *string             `json:"name"`
		ModelMap *providers.ModelMap `json:"model_map"`
	}
	endpointRefusals := []providerBodyRefusal{
		{"base_url", "base_url and auth_style are fixed when a shared provider is created; create a new definition instead"},
		{"auth_style", "base_url and auth_style are fixed when a shared provider is created; create a new definition instead"},
		{"engine", "engine cannot be changed"},
		{"protocol", "protocol cannot be changed"},
	}
	if !decodeProviderBody(w, r, &req, providerKeyFieldRefusals, endpointRefusals) {
		return
	}
	if req.Name == nil && req.ModelMap == nil {
		httpx.Error(w, http.StatusBadRequest, "name or model_map is required")
		return
	}
	var rename *string
	if req.Name != nil {
		name, err := validateSecretLabel(*req.Name)
		if err != nil {
			httpx.Error(w, http.StatusBadRequest, "name: "+err.Error())
			return
		}
		rename = &name
	}
	var newMap []byte
	if req.ModelMap != nil {
		// The definition's engine decides the model-map shape. An unknown id validates as claude so the
		// invalid-map 400 keeps its precedence over the 404; the transaction re-validates against the
		// locked row's engine.
		engine := providers.EngineClaude
		if cur, err := h.q.GetSharedModelProviderAdmin(r.Context(), id); err == nil {
			engine = cur.Engine
		} else if !errors.Is(err, pgx.ErrNoRows) {
			internalProviderError(w, "get shared model provider", err)
			return
		}
		if err := providers.ValidateModelMapForEngine(engine, *req.ModelMap); err != nil {
			httpx.Error(w, http.StatusBadRequest, err.Error())
			return
		}
		mm, err := modelMapJSON(*req.ModelMap)
		if err != nil {
			internalProviderError(w, "marshal model map", err)
			return
		}
		newMap = mm
	}

	var row sharedProviderAdminRow
	err := h.withAdminTx(r.Context(), func(q *store.Queries) error {
		cur, err := q.LockSharedModelProviderForEdit(r.Context(), id)
		if errors.Is(err, pgx.ErrNoRows) {
			return errProviderNotFound
		}
		if err != nil {
			return err
		}
		if req.ModelMap != nil {
			// Defensive: the engine never changes, but the validation above ran before the lock.
			if err := providers.ValidateModelMapForEngine(cur.Engine, *req.ModelMap); err != nil {
				return &providerProfileError{err}
			}
		}
		if cur.RetiredAt.Valid {
			return errProviderRetired
		}
		if rename != nil {
			if _, err := q.RenameSharedModelProvider(r.Context(), store.RenameSharedModelProviderParams{ID: id, Name: *rename}); err != nil {
				return err
			}
		}
		if newMap != nil {
			prev, err := q.GetModelProviderRevision(r.Context(), store.GetModelProviderRevisionParams{ProviderID: id, Revision: cur.CurrentRevision})
			if err != nil {
				return err
			}
			if _, ok := h.cfg.ProviderBaseURLAllowed(prev.BaseUrl); !ok {
				return errProviderBaseURLGone
			}
			next, err := q.BumpSharedModelProviderRevision(r.Context(), id)
			if errors.Is(err, pgx.ErrNoRows) {
				return errProviderRetired
			}
			if err != nil {
				return err
			}
			// Destination and profile copy forward unchanged; only the model map is new.
			if err := q.InsertModelProviderRevision(r.Context(), store.InsertModelProviderRevisionParams{
				ProviderID: id, Revision: next, Protocol: prev.Protocol,
				BaseUrl: prev.BaseUrl, AuthStyle: prev.AuthStyle, ModelMap: newMap,
			}); err != nil {
				return err
			}
		}
		row, err = q.GetSharedModelProviderAdmin(r.Context(), id)
		return err
	})
	if err != nil {
		if writeSharedProviderError(w, err) {
			return
		}
		internalProviderError(w, "edit shared model provider", err)
		return
	}
	auditProvider("admin_edit", actor.ID, id)
	if newMap != nil {
		// Best effort and idempotent: a failed prune leaves unreferenced revisions for the next edit.
		if _, perr := h.q.PruneSharedModelProviderRevisions(r.Context(), id); perr != nil {
			slog.Warn("prune shared model provider revisions", "error", perr)
		}
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"provider": adminSharedProviderDTO(row)})
}

// AdminRetireSharedProvider is POST /api/admin/providers/{id}/retire: stops fresh selections and runs.
// Idempotent. A selected source stays selected but reads as unusable; frozen runs keep their binding.
func (h *Handler) AdminRetireSharedProvider(w http.ResponseWriter, r *http.Request) {
	actor, ok := egressProfileAdmin(w, r)
	if !ok {
		return
	}
	id, ok := httpx.PathUUID(w, r, "id", "provider")
	if !ok {
		return
	}
	var row sharedProviderAdminRow
	err := h.withAdminTx(r.Context(), func(q *store.Queries) error {
		if _, err := q.LockSharedModelProviderForEdit(r.Context(), id); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return errProviderNotFound
			}
			return err
		}
		if _, err := q.RetireSharedModelProvider(r.Context(), id); err != nil {
			return err
		}
		var err error
		row, err = q.GetSharedModelProviderAdmin(r.Context(), id)
		return err
	})
	if err != nil {
		if writeSharedProviderError(w, err) {
			return
		}
		internalProviderError(w, "retire shared model provider", err)
		return
	}
	auditProvider("admin_retire", actor.ID, id)
	httpx.JSON(w, http.StatusOK, map[string]any{"provider": adminSharedProviderDTO(row)})
}

// AdminDeleteSharedProvider is DELETE /api/admin/providers/{id}. It is refused (409
// provider_in_use_as_source) while any user has the definition selected; otherwise it deletes every
// user's attached key with it (their Test evidence cascades) and answers with the exact number of keys
// removed. Order inside the one transaction: definition FOR UPDATE first, then the selection count,
// then the keys, then the definition, so a key mutation holding KEY SHARE on the definition cannot cycle
// with the key delete.
func (h *Handler) AdminDeleteSharedProvider(w http.ResponseWriter, r *http.Request) {
	actor, ok := egressProfileAdmin(w, r)
	if !ok {
		return
	}
	id, ok := httpx.PathUUID(w, r, "id", "provider")
	if !ok {
		return
	}
	var deletedKeys int
	err := h.withAdminTx(r.Context(), func(q *store.Queries) error {
		if _, err := q.LockSharedModelProviderForDelete(r.Context(), id); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return errProviderNotFound
			}
			return err
		}
		selected, err := q.CountSharedProviderSelections(r.Context(), id)
		if err != nil {
			return err
		}
		if selected > 0 {
			return errSharedProviderInUse
		}
		keys, err := q.DeleteSharedProviderKeys(r.Context(), id)
		if err != nil {
			return err
		}
		deletedKeys = len(keys)
		n, err := q.DeleteSharedModelProvider(r.Context(), id)
		if err != nil {
			return err
		}
		if n == 0 {
			return errProviderNotFound
		}
		return nil
	})
	if err != nil {
		if writeSharedProviderError(w, err) {
			return
		}
		internalProviderError(w, "delete shared model provider", err)
		return
	}
	auditProvider("admin_delete", actor.ID, id, "deleted_key_count", deletedKeys)
	httpx.JSON(w, http.StatusOK, map[string]any{"deleted": true, "deleted_key_count": deletedKeys})
}
