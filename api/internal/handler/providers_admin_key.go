package handler

import (
	"errors"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/httpx"
	"github.com/vtmocanu/uzi/api/internal/operatorkey"
	"github.com/vtmocanu/uzi/api/internal/providers"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Organization key routes (PRD #4540 M1). An organization-funded shared definition (credential_mode
// operator_key) holds one gateway key, sealed with the instance key and bound to the definition by its
// id (operatorkey.Seal). The key is write-only: no route, DTO, log or error ever returns it, and only
// the admin Test (providers_admin_probe.go) reads it back, to dial the definition's own gateway.
//
// All three routes are cookie + CSRF admin writes. Each runs in one withAdminTx and locks the
// definition (LockSharedModelProviderForEdit) before the key's custody row
// (LockOperatorProviderKeyForWrite), the order the admin delete and the Test evidence fence share, so
// none of them can deadlock with another. No per-user secret lock is taken: this key is not a user's.

// operatorTrustAckMessage is the 400 of an organization-key create without the acknowledgment.
const operatorTrustAckMessage = "acknowledge_recipient_trust must be true to create an organization-key provider: " +
	"recipients' runs spend this key and can copy it, and rotating it at the gateway is the only revocation"

// operatorDefinitionError classifies the result of LockSharedModelProviderForEdit for a key route: a
// missing or personal definition is a 404, a per_user_key one is a 409. Each route calls the lock itself
// (the call order is pinned by TestKeyRouteCallOrderIsPinned).
func operatorDefinitionError(mode string, lockErr error) error {
	if errors.Is(lockErr, pgx.ErrNoRows) {
		return errProviderNotFound
	}
	if lockErr != nil {
		return lockErr
	}
	if mode != sharedProviderModeOperatorKey {
		return errNotOperatorProvider
	}
	return nil
}

// AdminPutOperatorKey is PUT /api/admin/providers/{id}/key {key}: sets the organization key, or replaces
// it (a new key_rev, the enablement kept). Allowed on a retired definition, so a key can be staged
// before it is unretired or rotated after.
func (h *Handler) AdminPutOperatorKey(w http.ResponseWriter, r *http.Request) {
	actor, ok := egressProfileAdmin(w, r)
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
	if h.box == nil {
		internalProviderError(w, "seal organization key", errors.New("no instance secret box"))
		return
	}
	plain := []byte(key)
	sealed, err := operatorkey.Seal(h.box, id, plain)
	for i := range plain {
		plain[i] = 0
	}
	if err != nil {
		internalProviderError(w, "seal organization key", err)
		return
	}

	var row sharedProviderAdminRow
	replaced := false
	var keyID string
	var keyRev int64
	err = h.withAdminTx(r.Context(), func(q *store.Queries) error {
		cur, err := q.LockSharedModelProviderForEdit(r.Context(), id)
		if err := operatorDefinitionError(cur.CredentialMode.String, err); err != nil {
			return err
		}
		if _, err := q.LockOperatorProviderKeyForWrite(r.Context(), id); err == nil {
			replaced = true
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if replaced {
			rep, err := q.ReplaceOperatorProviderKey(r.Context(), store.ReplaceOperatorProviderKeyParams{Ciphertext: sealed, SetBy: actor.ID, ProviderID: id})
			if err != nil {
				return err
			}
			keyID, keyRev = rep.ID.String(), rep.KeyRev
		} else {
			ins, err := q.InsertOperatorProviderKey(r.Context(), store.InsertOperatorProviderKeyParams{ProviderID: id, Ciphertext: sealed, SetBy: actor.ID})
			if err != nil {
				return err
			}
			keyID, keyRev = ins.ID.String(), ins.KeyRev
		}
		row, err = q.GetSharedModelProviderAdmin(r.Context(), id)
		return err
	})
	if err != nil {
		if writeSharedProviderError(w, err) {
			return
		}
		internalProviderError(w, "set organization key", err)
		return
	}
	action := "admin_key_set"
	if replaced {
		action = "admin_key_replace"
	}
	auditProvider(action, actor.ID, id, "key_id", keyID, "key_rev", keyRev)
	httpx.JSON(w, http.StatusOK, map[string]any{"provider": adminSharedProviderDTO(row)})
}

// AdminPatchOperatorKeyEnabled is PATCH /api/admin/providers/{id}/key/enabled {enabled}: disables or
// re-enables the organization key. A real change draws a new key_rev (it invalidates Test evidence);
// repeating the current state changes nothing. Always allowed, retired or not.
func (h *Handler) AdminPatchOperatorKeyEnabled(w http.ResponseWriter, r *http.Request) {
	actor, ok := egressProfileAdmin(w, r)
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
	var row sharedProviderAdminRow
	var changed bool
	err := h.withAdminTx(r.Context(), func(q *store.Queries) error {
		cur, err := q.LockSharedModelProviderForEdit(r.Context(), id)
		if err := operatorDefinitionError(cur.CredentialMode.String, err); err != nil {
			return err
		}
		if _, err := q.LockOperatorProviderKeyForWrite(r.Context(), id); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return errProviderNoKey
			}
			return err
		}
		var n int64
		if *req.Enabled {
			n, err = q.EnableOperatorProviderKey(r.Context(), id)
		} else {
			n, err = q.DisableOperatorProviderKey(r.Context(), id)
		}
		if err != nil {
			return err
		}
		changed = n > 0
		row, err = q.GetSharedModelProviderAdmin(r.Context(), id)
		return err
	})
	if err != nil {
		if writeSharedProviderError(w, err) {
			return
		}
		internalProviderError(w, "set organization key enablement", err)
		return
	}
	action := "admin_key_disable"
	if *req.Enabled {
		action = "admin_key_enable"
	}
	auditProvider(action, actor.ID, id, "changed", changed)
	httpx.JSON(w, http.StatusOK, map[string]any{"provider": adminSharedProviderDTO(row)})
}

// AdminDeleteOperatorKey is DELETE /api/admin/providers/{id}/key: removes the organization key and its
// Test evidence. A later set inserts a new custody row with a new id and a larger key_rev. Always
// allowed, retired or not.
func (h *Handler) AdminDeleteOperatorKey(w http.ResponseWriter, r *http.Request) {
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
		cur, err := q.LockSharedModelProviderForEdit(r.Context(), id)
		if err := operatorDefinitionError(cur.CredentialMode.String, err); err != nil {
			return err
		}
		if _, err := q.LockOperatorProviderKeyForWrite(r.Context(), id); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return errProviderNoKey
			}
			return err
		}
		if _, err := q.DeleteOperatorProviderKey(r.Context(), id); err != nil {
			return err
		}
		row, err = q.GetSharedModelProviderAdmin(r.Context(), id)
		return err
	})
	if err != nil {
		if writeSharedProviderError(w, err) {
			return
		}
		internalProviderError(w, "delete organization key", err)
		return
	}
	auditProvider("admin_key_delete", actor.ID, id)
	httpx.JSON(w, http.StatusOK, map[string]any{"provider": adminSharedProviderDTO(row)})
}
