package handler

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/providers"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Model provider routes (PRD #4320 M1). A provider is a user-defined, engine-bound endpoint with
// its own key and model map, at a base URL the operator approved. Error codes ride the existing
// httpx.ErrorReason envelope: {"error": "<message>", "reason": "<code>"}.

const (
	// maxProvidersPerOwner is the per-owner cap, retired providers included.
	maxProvidersPerOwner = 20

	reasonProviderBaseURLNotAllowed = "provider_base_url_not_allowed"
	reasonProviderInUseAsSource     = "provider_in_use_as_source"
	reasonProviderLimit             = "provider_limit_reached"
	reasonProviderRetired           = "provider_retired"
	reasonProviderNameTaken         = "provider_name_taken"

	// maxProviderBodyBytes bounds a provider request body (a model map is a few hundred bytes).
	maxProviderBodyBytes = 64 << 10
)

var (
	errProviderNotFound = errors.New("provider not found")
	errProviderRetired  = errors.New("provider is retired")
	errProviderCap      = errors.New("provider limit reached")
	errProviderNoKey    = errors.New("provider has no key")

	// errProviderBaseURLGone: the base_url a save would carry is no longer on the operator allowlist.
	errProviderBaseURLGone = errors.New("provider base URL is no longer allowed")
)

// providerBodyRefusal names a request field a route refuses with a specific message rather than
// the generic unknown-field 400.
type providerBodyRefusal struct {
	field   string
	message string
}

var (
	// A create or edit never accepts a key: it travels only on PUT /api/me/providers/{id}/key.
	providerKeyFieldRefusals = []providerBodyRefusal{
		{"key", "a provider is saved without its key; send the key with PUT /api/me/providers/{id}/key"},
		{"api_key", "a provider is saved without its key; send the key with PUT /api/me/providers/{id}/key"},
		{"token", "a provider is saved without its key; send the key with PUT /api/me/providers/{id}/key"},
		{"secret", "a provider is saved without its key; send the key with PUT /api/me/providers/{id}/key"},
	}
	// PATCH /api/me/providers/{id} edits the name and model map only; the destination moves
	// through the browser-session endpoint route.
	providerEndpointFieldRefusals = []providerBodyRefusal{
		{"base_url", "base_url and auth_style are changed through PATCH /api/me/providers/{id}/endpoint in the browser"},
		{"auth_style", "base_url and auth_style are changed through PATCH /api/me/providers/{id}/endpoint in the browser"},
		{"engine", "engine cannot be changed"},
		{"protocol", "protocol cannot be changed"},
	}
)

// decodeProviderBody reads one bounded JSON object, answers 400 (with a specific message) when it
// carries a refused field, and otherwise decodes it strictly into dst. It reports whether the
// handler should continue.
func decodeProviderBody(w http.ResponseWriter, r *http.Request, dst any, refusals ...[]providerBodyRefusal) bool {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxProviderBodyBytes+1))
	if err != nil || len(raw) > maxProviderBodyBytes {
		httpx.Error(w, http.StatusRequestEntityTooLarge, "the request body is too large")
		return false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return false
	}
	for _, set := range refusals {
		for _, rf := range set {
			if _, present := fields[rf.field]; present {
				httpx.Error(w, http.StatusBadRequest, rf.message)
				return false
			}
		}
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return false
	}
	return true
}

// providerRow is the shared shape of ListModelProvidersForOwnerRow and GetModelProviderForOwnerRow.
type providerRow = store.ListModelProvidersForOwnerRow

func modelMapDTO(raw []byte) apitypes.ModelMapDTO {
	var out apitypes.ModelMapDTO
	_ = json.Unmarshal(raw, &out)
	return out
}

func providerDTO(row providerRow) apitypes.ProviderDTO {
	dto := apitypes.ProviderDTO{
		ID:              row.ID.String(),
		Name:            row.Name,
		Engine:          row.Engine,
		Retired:         row.RetiredAt.Valid,
		CurrentRevision: int(row.CurrentRevision),
		Protocol:        row.Protocol,
		BaseURL:         row.BaseUrl,
		AuthStyle:       row.AuthStyle,
		ModelMap:        modelMapDTO(row.ModelMap),
		HasKey:          row.KeySecretID.Valid,
		KeyEnabled:      row.KeySecretID.Valid && !row.KeyDisabledAt.Valid,
	}
	if row.ProbeAuthResult.Valid {
		dto.Probe = &apitypes.ProviderProbeDTO{
			AuthResult: row.ProbeAuthResult.String,
			ToolResult: row.ProbeToolResult.String,
			ProbedAt:   row.ProbedAt.Time,
		}
	}
	return dto
}

// providersEnabled reports whether the operator configured a provider allowlist (feature on).
func (h *Handler) providersEnabled() bool { return len(h.cfg.ProviderAllowedBaseURLs) > 0 }

// writeBaseURLRefusal answers 400 provider_base_url_not_allowed.
func writeBaseURLRefusal(w http.ResponseWriter) {
	httpx.ErrorReason(w, http.StatusBadRequest,
		"base_url is not on the operator's provider allowlist; it must equal an allowed URL exactly, path included",
		reasonProviderBaseURLNotAllowed)
}

// allowedProviderBaseURL resolves a requested base URL to the operator allowlist entry it equals
// exactly, which is the form stored. Anything else (including the feature being off) is refused
// with the typed code.
func (h *Handler) allowedProviderBaseURL(w http.ResponseWriter, raw string) (string, bool) {
	entry, ok := h.cfg.ProviderBaseURLAllowed(raw)
	if !ok {
		writeBaseURLRefusal(w)
		return "", false
	}
	return entry, true
}

func isPGUnique(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == constraint
}

func isPGForeignKeyViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503" && pgErr.ConstraintName == constraint
}

// writeProviderError maps the sentinel errors of a provider transaction to responses. It reports
// whether it wrote one.
func writeProviderError(w http.ResponseWriter, err error) bool {
	var profileErr *providerProfileError
	if errors.As(err, &profileErr) {
		httpx.Error(w, http.StatusBadRequest, profileErr.Error())
		return true
	}
	switch {
	case errors.Is(err, errProviderNotFound):
		httpx.Error(w, http.StatusNotFound, "provider not found")
	case errors.Is(err, errProviderRetired):
		httpx.ErrorReason(w, http.StatusConflict, "the provider is retired", reasonProviderRetired)
	case errors.Is(err, errProviderCap):
		httpx.ErrorReason(w, http.StatusConflict, "provider limit reached (20 per user, retired providers included)", reasonProviderLimit)
	case errors.Is(err, errProviderNoKey):
		httpx.Error(w, http.StatusNotFound, "the provider has no key")
	case errors.Is(err, errProviderBaseURLGone):
		writeBaseURLRefusal(w)
	case isPGUnique(err, "model_providers_owner_name_lower_key"):
		httpx.ErrorReason(w, http.StatusConflict, "a provider with that name already exists", reasonProviderNameTaken)
	default:
		return false
	}
	return true
}

func internalProviderError(w http.ResponseWriter, what string, err error) {
	slog.Error(what, "error", err)
	httpx.Error(w, http.StatusInternalServerError, "internal error")
}

// userForProviderRoute returns the authenticated user, answering 401 itself when absent.
func userForProviderRoute(w http.ResponseWriter, r *http.Request) (store.User, bool) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return store.User{}, false
	}
	return user, true
}

// ListMyProviders returns the caller's providers. With the feature off (no allowlist configured)
// it answers enabled=false and an empty list.
func (h *Handler) ListMyProviders(w http.ResponseWriter, r *http.Request) {
	user, ok := userForProviderRoute(w, r)
	if !ok {
		return
	}
	out := apitypes.ProviderListDTO{
		Enabled:         h.providersEnabled(),
		Providers:       []apitypes.ProviderDTO{},
		AllowedBaseURLs: append([]string{}, h.cfg.ProviderAllowedBaseURLs...),
	}
	if !out.Enabled {
		httpx.JSON(w, http.StatusOK, out)
		return
	}
	rows, err := h.q.ListModelProvidersForOwner(r.Context(), user.ID)
	if err != nil {
		internalProviderError(w, "list model providers", err)
		return
	}
	for _, row := range rows {
		out.Providers = append(out.Providers, providerDTO(row))
	}
	httpx.JSON(w, http.StatusOK, out)
}

// GetMyProvider returns one of the caller's providers; a foreign id is a 404.
func (h *Handler) GetMyProvider(w http.ResponseWriter, r *http.Request) {
	user, ok := userForProviderRoute(w, r)
	if !ok {
		return
	}
	id, ok := httpx.PathUUID(w, r, "id", "provider")
	if !ok {
		return
	}
	if !h.providersEnabled() {
		httpx.Error(w, http.StatusNotFound, "provider not found")
		return
	}
	row, err := h.q.GetModelProviderForOwner(r.Context(), store.GetModelProviderForOwnerParams{ID: id, OwnerUserID: user.ID})
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.Error(w, http.StatusNotFound, "provider not found")
		return
	}
	if err != nil {
		internalProviderError(w, "get model provider", err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"provider": providerDTO(providerRow(row))})
}

// modelMapJSON is the canonical stored form of a validated model map.
func modelMapJSON(m providers.ModelMap) ([]byte, error) { return json.Marshal(m) }

// CreateMyProvider creates a provider with its first revision. It never accepts a key.
func (h *Handler) CreateMyProvider(w http.ResponseWriter, r *http.Request) {
	user, ok := userForProviderRoute(w, r)
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
	if req.Protocol == "" && req.Engine == providers.EngineClaude {
		req.Protocol = providers.ProtocolAnth
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

	var row store.GetModelProviderForOwnerRow
	err = h.withSecretLock(r.Context(), user.ID, nil, func(q *store.Queries) error {
		if err := q.LockModelProviderOwner(r.Context(), user.ID); err != nil {
			return err
		}
		n, err := q.CountModelProvidersForOwner(r.Context(), user.ID)
		if err != nil {
			return err
		}
		if n >= maxProvidersPerOwner {
			return errProviderCap
		}
		p, err := q.InsertModelProvider(r.Context(), store.InsertModelProviderParams{
			OwnerUserID: user.ID, Name: name, Engine: req.Engine,
		})
		if err != nil {
			return err
		}
		if err := q.InsertModelProviderRevision(r.Context(), store.InsertModelProviderRevisionParams{
			ProviderID: p.ID, Revision: 1, Protocol: req.Protocol,
			BaseUrl: baseURL, AuthStyle: req.AuthStyle, ModelMap: mm,
		}); err != nil {
			return err
		}
		row, err = q.GetModelProviderForOwner(r.Context(), store.GetModelProviderForOwnerParams{ID: p.ID, OwnerUserID: user.ID})
		return err
	})
	if err != nil {
		if writeProviderError(w, err) {
			return
		}
		internalProviderError(w, "create model provider", err)
		return
	}
	httpx.JSON(w, http.StatusCreated, map[string]any{"provider": providerDTO(providerRow(row))})
}

// revisionEdit describes the parts of a new revision an edit replaces; nil keeps the previous.
type revisionEdit struct {
	baseURL   *string
	authStyle *string
	modelMap  providers.ModelMap
}

// editProvider applies an optional rename and an optional revision edit in one transaction and
// answers with the updated provider. A revision edit is refused (400 provider_base_url_not_allowed)
// unless the base URL the new revision carries, the requested one or the previous revision's, is
// still on the operator allowlist. The provider row is row-locked (by the rename or by the bump)
// before the new immutable revision is inserted; the whole edit runs under the per-user
// secret-mutation lock, and a run-create must take LockSecretMutationShared before it takes
// LockModelProviderForShare on the provider row, or the two lock orders would cycle. Unreferenced
// old revisions are pruned in a separate statement after the commit.
func (h *Handler) editProvider(w http.ResponseWriter, r *http.Request, userID, id uuid.UUID, rename *string, edit *revisionEdit) {
	var row store.GetModelProviderForOwnerRow
	err := h.withSecretLock(r.Context(), userID, nil, func(q *store.Queries) error {
		cur, err := q.GetModelProviderForOwner(r.Context(), store.GetModelProviderForOwnerParams{ID: id, OwnerUserID: userID})
		if errors.Is(err, pgx.ErrNoRows) {
			return errProviderNotFound
		}
		if err != nil {
			return err
		}
		if cur.RetiredAt.Valid {
			return errProviderRetired
		}
		if edit != nil && edit.baseURL == nil {
			if _, ok := h.cfg.ProviderBaseURLAllowed(cur.BaseUrl); !ok {
				return errProviderBaseURLGone
			}
		}
		if rename != nil {
			if _, err := q.RenameModelProvider(r.Context(), store.RenameModelProviderParams{ID: id, OwnerUserID: userID, Name: *rename}); err != nil {
				return err
			}
		}
		if edit != nil {
			next, err := q.BumpModelProviderRevision(r.Context(), store.BumpModelProviderRevisionParams{ID: id, OwnerUserID: userID})
			if errors.Is(err, pgx.ErrNoRows) {
				return errProviderRetired
			}
			if err != nil {
				return err
			}
			prev, err := q.GetModelProviderRevision(r.Context(), store.GetModelProviderRevisionParams{ProviderID: id, Revision: next - 1})
			if err != nil {
				return err
			}
			p := store.InsertModelProviderRevisionParams{
				ProviderID: id, Revision: next, Protocol: prev.Protocol,
				BaseUrl: prev.BaseUrl, AuthStyle: prev.AuthStyle, ModelMap: prev.ModelMap,
			}
			if edit.baseURL != nil {
				p.BaseUrl = *edit.baseURL
			}
			if edit.authStyle != nil {
				p.AuthStyle = *edit.authStyle
			}
			if edit.modelMap != nil {
				mm, err := modelMapJSON(edit.modelMap)
				if err != nil {
					return err
				}
				p.ModelMap = mm
			}
			var models providers.ModelMap
			if err := json.Unmarshal(p.ModelMap, &models); err != nil {
				return err
			}
			if err := providers.ValidateProfile(cur.Engine, p.Protocol, p.AuthStyle, models); err != nil {
				return &providerProfileError{err}
			}
			if err := q.InsertModelProviderRevision(r.Context(), p); err != nil {
				return err
			}
		}
		row, err = q.GetModelProviderForOwner(r.Context(), store.GetModelProviderForOwnerParams{ID: id, OwnerUserID: userID})
		return err
	})
	if err != nil {
		if writeProviderError(w, err) {
			return
		}
		internalProviderError(w, "edit model provider", err)
		return
	}
	if edit != nil {
		// Best effort and idempotent: a failed prune leaves unreferenced revisions for the next edit.
		if _, perr := h.q.PruneModelProviderRevisions(r.Context(), store.PruneModelProviderRevisionsParams{ProviderID: id, OwnerUserID: userID}); perr != nil {
			slog.Warn("prune model provider revisions", "error", perr)
		}
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"provider": providerDTO(providerRow(row))})
}

// PatchMyProvider renames a provider and/or replaces its model map (a new revision). The
// destination (base_url, auth_style) is not editable here.
func (h *Handler) PatchMyProvider(w http.ResponseWriter, r *http.Request) {
	user, ok := userForProviderRoute(w, r)
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
	if !decodeProviderBody(w, r, &req, providerKeyFieldRefusals, providerEndpointFieldRefusals) {
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
	var edit *revisionEdit
	if req.ModelMap != nil {
		profile, err := h.q.GetModelProviderForOwner(r.Context(), store.GetModelProviderForOwnerParams{ID: id, OwnerUserID: user.ID})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeProviderError(w, errProviderNotFound)
			} else {
				internalProviderError(w, "get provider", err)
			}
			return
		}
		if err := providers.ValidateModelMapForEngine(profile.Engine, *req.ModelMap); err != nil {
			httpx.Error(w, http.StatusBadRequest, err.Error())
			return
		}
		edit = &revisionEdit{modelMap: *req.ModelMap}
	}
	h.editProvider(w, r, user.ID, id, rename, edit)
}

// PatchMyProviderEndpoint changes a provider's base_url and/or auth_style (a new revision). It is
// browser-session only: the destination decides where the key is sent.
func (h *Handler) PatchMyProviderEndpoint(w http.ResponseWriter, r *http.Request) {
	user, ok := userForProviderRoute(w, r)
	if !ok {
		return
	}
	id, ok := httpx.PathUUID(w, r, "id", "provider")
	if !ok {
		return
	}
	var req struct {
		BaseURL   *string `json:"base_url"`
		AuthStyle *string `json:"auth_style"`
	}
	if !decodeProviderBody(w, r, &req, providerKeyFieldRefusals) {
		return
	}
	if req.BaseURL == nil && req.AuthStyle == nil {
		httpx.Error(w, http.StatusBadRequest, "base_url or auth_style is required")
		return
	}
	edit := &revisionEdit{}
	if req.AuthStyle != nil {
		if err := providers.ValidateAuthStyle(*req.AuthStyle); err != nil {
			httpx.Error(w, http.StatusBadRequest, err.Error())
			return
		}
		edit.authStyle = req.AuthStyle
	}
	if req.BaseURL != nil {
		entry, ok := h.allowedProviderBaseURL(w, *req.BaseURL)
		if !ok {
			return
		}
		edit.baseURL = &entry
	} else if !h.providersEnabled() {
		writeBaseURLRefusal(w)
		return
	}
	h.editProvider(w, r, user.ID, id, nil, edit)
}

// RetireMyProvider stops new selections and runs of a provider. Idempotent.
func (h *Handler) RetireMyProvider(w http.ResponseWriter, r *http.Request) {
	user, ok := userForProviderRoute(w, r)
	if !ok {
		return
	}
	id, ok := httpx.PathUUID(w, r, "id", "provider")
	if !ok {
		return
	}
	var row store.GetModelProviderForOwnerRow
	err := h.withSecretLock(r.Context(), user.ID, nil, func(q *store.Queries) error {
		n, err := q.RetireModelProvider(r.Context(), store.RetireModelProviderParams{ID: id, OwnerUserID: user.ID})
		if err != nil {
			return err
		}
		if n == 0 {
			return errProviderNotFound
		}
		row, err = q.GetModelProviderForOwner(r.Context(), store.GetModelProviderForOwnerParams{ID: id, OwnerUserID: user.ID})
		return err
	})
	if err != nil {
		if writeProviderError(w, err) {
			return
		}
		internalProviderError(w, "retire model provider", err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"provider": providerDTO(providerRow(row))})
}

// DeleteMyProvider deletes a provider with its revisions, probe result and key. A provider that
// any user has selected as their source cannot be deleted: 409 provider_in_use_as_source (the
// owner-bound Claude and Codex source foreign keys use ON DELETE RESTRICT).
func (h *Handler) DeleteMyProvider(w http.ResponseWriter, r *http.Request) {
	user, ok := userForProviderRoute(w, r)
	if !ok {
		return
	}
	id, ok := httpx.PathUUID(w, r, "id", "provider")
	if !ok {
		return
	}
	err := h.withSecretLock(r.Context(), user.ID, nil, func(q *store.Queries) error {
		n, err := q.DeleteModelProvider(r.Context(), store.DeleteModelProviderParams{ID: id, OwnerUserID: user.ID})
		if err != nil {
			return err
		}
		if n == 0 {
			return errProviderNotFound
		}
		return nil
	})
	if err != nil {
		if isPGForeignKeyViolation(err, "users_claude_provider_fkey") || isPGForeignKeyViolation(err, "users_codex_provider_fkey") {
			httpx.ErrorReason(w, http.StatusConflict,
				"the provider is selected as a source; choose another source before deleting it", reasonProviderInUseAsSource)
			return
		}
		if writeProviderError(w, err) {
			return
		}
		internalProviderError(w, "delete model provider", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// PutMyProviderKey sets or rotates the provider's gateway key, sealed with the vault under the
// existing (user, kind) AAD. Rotation bumps the key's enablement_rev, so runs that froze the old
// material are refused at their next claim (a later milestone).
func (h *Handler) PutMyProviderKey(w http.ResponseWriter, r *http.Request) {
	user, ok := userForProviderRoute(w, r)
	if !ok {
		return
	}
	id, ok := httpx.PathUUID(w, r, "id", "provider")
	if !ok {
		return
	}
	var req struct {
		Key string `json:"key"`
	}
	if !decodeProviderBody(w, r, &req) {
		return
	}
	key := strings.TrimSpace(req.Key)
	if err := providers.ValidateKey(key); err != nil {
		httpx.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	snapshot, err := h.q.GetModelProviderForOwner(r.Context(), store.GetModelProviderForOwnerParams{ID: id, OwnerUserID: user.ID})
	if errors.Is(err, pgx.ErrNoRows) {
		writeProviderError(w, errProviderNotFound)
		return
	}
	if err != nil {
		internalProviderError(w, "get provider before sealing", err)
		return
	}
	var models providers.ModelMap
	if json.Unmarshal(snapshot.ModelMap, &models) != nil || providers.ValidateProfile(snapshot.Engine, snapshot.Protocol, snapshot.AuthStyle, models) != nil {
		httpx.Error(w, http.StatusConflict, "the provider profile is invalid")
		return
	}
	expectedKind := store.KindAnthropicCompatibleKey
	if snapshot.Engine == providers.EngineCodex {
		expectedKind = store.KindOpenAICompatibleKey
	}
	material, err := h.sealUserSecret(r.Context(), user.ID, expectedKind, []byte(key))
	if err != nil {
		if h.writeVaultLocked(w, err) {
			return
		}
		internalProviderError(w, "seal provider key", err)
		return
	}
	var row store.GetModelProviderForOwnerRow
	err = h.withSecretLock(r.Context(), user.ID, &material, func(q *store.Queries) error {
		prov, err := q.GetModelProviderForOwner(r.Context(), store.GetModelProviderForOwnerParams{ID: id, OwnerUserID: user.ID})
		if errors.Is(err, pgx.ErrNoRows) {
			return errProviderNotFound
		}
		if err != nil {
			return err
		}
		if prov.Engine != snapshot.Engine || prov.CurrentRevision != snapshot.CurrentRevision || prov.KeySecretID != snapshot.KeySecretID || prov.KeyEnablementRev != snapshot.KeyEnablementRev || prov.RetiredAt.Valid {
			return &providerProfileError{errors.New("the provider changed while sealing its key; try again")}
		}
		if _, ok := h.cfg.ProviderBaseURLAllowed(prov.BaseUrl); !ok {
			return errProviderBaseURLGone
		}
		cur, err := q.GetProviderKeyForUpdate(r.Context(), store.GetProviderKeyForUpdateParams{UserID: user.ID, ProviderID: pgtype.UUID{Bytes: id, Valid: true}})
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			if prov.KeySecretID.Valid {
				return errProviderNoKey
			}
			_, err = q.InsertProviderKeySecret(r.Context(), store.InsertProviderKeySecretParams{
				UserID: user.ID, Label: id.String(), Ciphertext: material.Ciphertext, SealedWith: material.SealedWith,
				ProviderID: pgtype.UUID{Bytes: id, Valid: true},
			})
		case err != nil:
		default:
			_, err = q.RotateUserSecret(r.Context(), store.RotateUserSecretParams{
				ID: cur.ID, UserID: user.ID, Ciphertext: material.Ciphertext, SealedWith: material.SealedWith,
			})
		}
		if err != nil {
			return err
		}
		row, err = q.GetModelProviderForOwner(r.Context(), store.GetModelProviderForOwnerParams{ID: id, OwnerUserID: user.ID})
		return err
	})
	if err != nil {
		if h.writeVaultStale(w, err) {
			return
		}
		if writeProviderError(w, err) {
			return
		}
		internalProviderError(w, "set provider key", err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"provider": providerDTO(providerRow(row))})
}

// DeleteMyProviderKey removes the provider's key.
func (h *Handler) DeleteMyProviderKey(w http.ResponseWriter, r *http.Request) {
	user, ok := userForProviderRoute(w, r)
	if !ok {
		return
	}
	id, ok := httpx.PathUUID(w, r, "id", "provider")
	if !ok {
		return
	}
	err := h.withSecretLock(r.Context(), user.ID, nil, func(q *store.Queries) error {
		n, err := q.DeleteProviderKeySecret(r.Context(), store.DeleteProviderKeySecretParams{UserID: user.ID, ProviderID: pgtype.UUID{Bytes: id, Valid: true}})
		if err != nil {
			return err
		}
		if n == 0 {
			return errProviderNoKey
		}
		return nil
	})
	if err != nil {
		if writeProviderError(w, err) {
			return
		}
		internalProviderError(w, "delete provider key", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// PatchMyProviderKeyEnabled disables or re-enables the provider's key (ADR-1732 semantics: the
// enablement_rev bumps on a real transition). The generic /me/secrets/{kind}/{id}/enabled route
// does not serve this kind.
func (h *Handler) PatchMyProviderKeyEnabled(w http.ResponseWriter, r *http.Request) {
	user, ok := userForProviderRoute(w, r)
	if !ok {
		return
	}
	id, ok := httpx.PathUUID(w, r, "id", "provider")
	if !ok {
		return
	}
	var req struct {
		Enabled *bool `json:"enabled"`
	}
	if !decodeProviderBody(w, r, &req) {
		return
	}
	if req.Enabled == nil {
		httpx.Error(w, http.StatusBadRequest, "enabled is required")
		return
	}
	var row store.GetModelProviderForOwnerRow
	err := h.withSecretLock(r.Context(), user.ID, nil, func(q *store.Queries) error {
		cur, err := q.GetProviderKeyForUpdate(r.Context(), store.GetProviderKeyForUpdateParams{UserID: user.ID, ProviderID: pgtype.UUID{Bytes: id, Valid: true}})
		if errors.Is(err, pgx.ErrNoRows) {
			return errProviderNoKey
		}
		if err != nil {
			return err
		}
		if _, err := q.SetSecretEnablement(r.Context(), store.SetSecretEnablementParams{ID: cur.ID, UserID: user.ID, Enabled: *req.Enabled}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		row, err = q.GetModelProviderForOwner(r.Context(), store.GetModelProviderForOwnerParams{ID: id, OwnerUserID: user.ID})
		return err
	})
	if err != nil {
		if writeProviderError(w, err) {
			return
		}
		internalProviderError(w, "set provider key enablement", err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"provider": providerDTO(providerRow(row))})
}

type providerProfileError struct{ error }
