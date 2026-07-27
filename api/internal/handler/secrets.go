package handler

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"unicode"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/apitypes"
	"gitlab.example.com/vtmocanu/uzi/api/internal/httpx"
	mw "gitlab.example.com/vtmocanu/uzi/api/internal/middleware"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
	"gitlab.example.com/vtmocanu/uzi/api/internal/vault"
)

// sealUserSecret encrypts a user secret for storage, returning the ciphertext and
// the sealed_with value to record. With a vault wired (production) it seals under
// the user's DEK (sealed_with='dek') and returns vault.ErrLocked when the vault is
// locked; without one (tests) it falls back to the legacy master box
// (sealed_with='master'), preserving pre-vault behavior.
func (h *Handler) sealUserSecret(userID uuid.UUID, kind string, plaintext []byte) (sealed []byte, sealedWith string, err error) {
	if h.vault != nil {
		sealed, err = h.vault.Seal(userID, kind, plaintext)
		return sealed, store.SealedWithDEK, err
	}
	sealed, err = h.box.Seal(plaintext)
	return sealed, store.SealedWithMaster, err
}

// maxTokenBytes bounds a pasted credential. Generous enough for both
// `claude setup-token` OAuth tokens and console API keys; no format assumption
// is made beyond length + no control/whitespace.
const maxTokenBytes = 4096

// maxLabelBytes bounds a token label (PRD #104 D7). Mirrors the migration's CHECK
// (char_length BETWEEN 1 AND 64); the Go validator additionally rejects control
// characters, which the CHECK does not.
const maxLabelBytes = 64

// secretMeta builds the metadata-only DTO (apitypes.SecretDTO) for one stored
// secret. The value appears in no field — there is no reveal endpoint.
//
// autoEligible is passed rather than defaulted (PRD #111 M2): every query feeding
// this now RETURNs the column, so a caller that does not have it has taken a row
// from somewhere that cannot answer, and should say so rather than guess `false` —
// a wrong `false` reads as "you never opted this token in", which is a setting the
// user did set.
func secretMeta(id uuid.UUID, kind, label string, isDefault, autoEligible bool, created, updated pgtype.Timestamptz) apitypes.SecretDTO {
	return apitypes.SecretDTO{
		ID:           id.String(),
		Kind:         kind,
		Label:        label,
		IsDefault:    isDefault,
		AutoEligible: autoEligible,
		CreatedAt:    created.Time,
		UpdatedAt:    updated.Time,
	}
}

// ListMySecrets returns metadata for the current user's Anthropic tokens (PRD #104
// M2): one entry per token, default first, each with its id/label/default flag and
// timestamps — never the ciphertext.
func (h *Handler) ListMySecrets(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	rows, err := h.q.ListUserSecretsForKind(r.Context(), store.ListUserSecretsForKindParams{
		UserID: user.ID,
		Kind:   store.KindAnthropicToken,
	})
	if err != nil {
		slog.Error("list user secrets", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := make([]apitypes.SecretDTO, 0, len(rows))
	for _, s := range rows {
		out = append(out, secretMeta(s.ID, s.Kind, s.Label, s.IsDefault, s.AutoEligible, s.CreatedAt, s.UpdatedAt))
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"secrets": out})
}

// withSecretLock runs fn inside a transaction that first takes the per-(user,kind)
// advisory lock serializing this user's token mutations (PRD #104 M2/D12). It is
// what makes create, set-default and delete atomic against each other, so the
// "exactly one default while any token exists" invariant — which no index can
// enforce — holds across concurrent requests.
//
// With no pool wired (unit tests only; main always wires one) it runs fn directly
// on h.q with no transaction. That is sound in a test precisely because the lock's
// only job is serializing CONCURRENT callers, and a single-threaded unit test has
// none; the concurrency itself is proven by the live-DB tests. This mirrors
// sealUserSecret's vault-nil fallback.
func (h *Handler) withSecretLock(ctx context.Context, userID uuid.UUID, fn func(q *store.Queries) error) error {
	if h.pool == nil {
		return fn(h.q)
	}
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after a successful Commit
	// FIRST statement, before any read the lock protects (the identical placement
	// rule as the hosted-provision quota lock). Serializes this user's token
	// mutations until the transaction ends; XACT-scoped, so there is no unlock to
	// forget.
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1, $2)",
		store.SecretMutationLockClass, secretMutationLockObjID(userID)); err != nil {
		return err
	}
	if err := fn(h.q.WithTx(tx)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// secretMutationLockObjID derives the objid half of the per-user advisory lock from
// a user's uuid, exactly as hostedProvisionLockObjID does: a uuid's leading bytes
// are random, so two users can collide and serialize for a moment — a contention
// non-event, never a correctness one.
func secretMutationLockObjID(userID uuid.UUID) int32 {
	return int32(binary.BigEndian.Uint32(userID[:4])) //nolint:gosec // wraparound is fine: a lock key, not a number
}

// PutAnthropicToken stores (or rotates) the current user's DEFAULT Anthropic token,
// encrypted at rest (PRD #104 D14 compatibility alias — deprecated in favor of the
// id-keyed POST/PATCH below). The plaintext is never logged, never echoed back, and
// never appears in any error string. Marked deprecated via the Deprecation header.
//
// It stays a single-statement upsert (UpsertDefaultUserSecret) and does NOT take the
// mutation lock, because a single INSERT..ON CONFLICT is atomic: it cannot interleave
// into a two-default state, and the "no default" state it once could 500 on is now
// unreachable — every mutation that could create it (set-default, delete-default)
// serializes under the lock and preserves exactly-one-default (M2/D12).
func (h *Handler) PutAnthropicToken(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Deprecation", "true")
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}

	var req struct {
		Token string `json:"token"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}

	token, err := validateAnthropicToken(req.Token)
	if err != nil {
		// err carries no token bytes (see validateAnthropicToken).
		httpx.Error(w, http.StatusBadRequest, err.Error())
		return
	}

	// Seal under the user's per-user vault DEK (PRD #32) so a DB dump + every env
	// var + Infisical cannot recover the token. The vault must be unlocked (it is
	// right after login; a mid-session pod restart is the only way to hit locked
	// here) → 409 vault_locked, which the SPA turns into an unlock prompt. When no
	// vault is wired (tests only; main always wires one) fall back to the legacy
	// master box so existing behavior and tests are unchanged.
	sealed, sealedWith, err := h.sealUserSecret(user.ID, store.KindAnthropicToken, []byte(token))
	if err != nil {
		if h.writeVaultLocked(w, err) {
			return
		}
		slog.Error("seal anthropic token", "error", err) // error carries no plaintext
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}

	// Rotates the user's DEFAULT token, or creates their first one labelled
	// 'default' (PRD #104 D14 — this kind-path route is a compatibility alias over
	// the default; M2 adds the id-keyed routes and deprecates this one).
	row, err := h.q.UpsertDefaultUserSecret(r.Context(), store.UpsertDefaultUserSecretParams{
		UserID:     user.ID,
		Kind:       store.KindAnthropicToken,
		Ciphertext: sealed,
		SealedWith: sealedWith,
	})
	if err != nil {
		slog.Error("store anthropic token", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	// Poke the rate-limit poller so this user's meters appear within seconds of
	// saving, not up to a full poll interval later (PRD #53 D3b). Best-effort and
	// non-blocking; nil when the poller is disabled or in tests.
	if h.usagePoker != nil {
		h.usagePoker.Poke(user.ID)
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"secret": secretMeta(row.ID, row.Kind, row.Label, row.IsDefault, row.AutoEligible, row.CreatedAt, row.UpdatedAt)})
}

// DeleteAnthropicToken removes the current user's DEFAULT Anthropic token (PRD #104
// D14 compatibility alias — deprecated in favor of the id-keyed DELETE below).
//
// It 409s for a MULTI-TOKEN user: with several tokens, "delete the default"
// contradicts D6 (the default cannot be deleted while others exist), so a
// multi-token user must delete by id. That breaks this route's former
// unconditional-204 contract, deliberately (D14/R5). A single-token user's delete
// returns them to the token-less state; a token-less user is the idempotent 204.
// Runs under the mutation lock so the count-then-delete cannot race a concurrent
// create into deleting-the-default-while-a-second-token-lands.
func (h *Handler) DeleteAnthropicToken(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	w.Header().Set("Deprecation", "true")

	var multiToken, deleted bool
	err := h.withSecretLock(r.Context(), user.ID, func(q *store.Queries) error {
		secretID, gerr := q.GetDefaultUserSecretID(r.Context(), store.GetDefaultUserSecretIDParams{
			UserID: user.ID, Kind: store.KindAnthropicToken,
		})
		if errors.Is(gerr, pgx.ErrNoRows) {
			return nil // token-less: idempotent 204, nothing to delete
		}
		if gerr != nil {
			return gerr
		}
		n, cerr := q.CountUserSecretsForKind(r.Context(), store.CountUserSecretsForKindParams{
			UserID: user.ID, Kind: store.KindAnthropicToken,
		})
		if cerr != nil {
			return cerr
		}
		if n > 1 {
			multiToken = true // 409: a multi-token user deletes by id (D14)
			return nil
		}
		// The sole token (which is the default). Its gauge row goes with it via the
		// ON DELETE CASCADE (M5); no DeleteRateLimits call needed.
		if _, derr := q.DeleteUserSecret(r.Context(), store.DeleteUserSecretParams{
			ID: secretID, UserID: user.ID,
		}); derr != nil {
			return derr
		}
		deleted = true
		return nil
	})
	if err != nil {
		slog.Error("delete anthropic token", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	if multiToken {
		httpx.Error(w, http.StatusConflict,
			"you have multiple tokens; delete a specific one by id (DELETE /api/me/secrets/anthropic_token/{id})")
		return
	}
	if deleted && h.usagePoker != nil {
		h.usagePoker.Poke(user.ID)
	}
	w.WriteHeader(http.StatusNoContent)
}

// CreateAnthropicToken creates a NEW named Anthropic token for the current user
// (PRD #104 M2). This is the request-body create path: `default` comes from the
// body, and it is the exact caller the M1 InsertUserSecret hardening protects — a
// user's FIRST token is forced default whatever the body asks, so a wrong client
// can never mint an invisible token. A second token is the caller's choice; asking
// for it as default clears the old default in the same transaction.
func (h *Handler) CreateAnthropicToken(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var req struct {
		Token   string `json:"token"`
		Label   string `json:"label"`
		Default bool   `json:"default"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	token, err := validateAnthropicToken(req.Token)
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	label, err := validateSecretLabel(req.Label)
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	sealed, sealedWith, err := h.sealUserSecret(user.ID, store.KindAnthropicToken, []byte(token))
	if err != nil {
		if h.writeVaultLocked(w, err) {
			return
		}
		slog.Error("seal anthropic token", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}

	var row store.InsertUserSecretRow
	err = h.withSecretLock(r.Context(), user.ID, func(q *store.Queries) error {
		// If the caller wants this the default, clear the current one first — the
		// insert then sets is_default=true and there is exactly one default at commit.
		// Under the advisory lock this clear-then-insert cannot interleave with a
		// concurrent mutation into a two-default or no-default state (D12).
		if req.Default {
			if _, cerr := q.ClearDefaultUserSecret(r.Context(), store.ClearDefaultUserSecretParams{
				UserID: user.ID, Kind: store.KindAnthropicToken,
			}); cerr != nil {
				return cerr
			}
		}
		var ierr error
		row, ierr = q.InsertUserSecret(r.Context(), store.InsertUserSecretParams{
			UserID:      user.ID,
			Kind:        store.KindAnthropicToken,
			Label:       label,
			WantDefault: req.Default, // first token is forced default by the query regardless
			Ciphertext:  sealed,
			SealedWith:  sealedWith,
		})
		return ierr
	})
	if err != nil {
		if isLabelCollision(err) {
			httpx.Error(w, http.StatusConflict, "a token with that label already exists")
			return
		}
		slog.Error("create anthropic token", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	// A newly created (or re-defaulted) token changes what the poller should read;
	// poke so its meter appears within seconds (PRD #53 D3b).
	if h.usagePoker != nil {
		h.usagePoker.Poke(user.ID)
	}
	httpx.JSON(w, http.StatusCreated, map[string]any{
		"secret": secretMeta(row.ID, row.Kind, row.Label, row.IsDefault, row.AutoEligible, row.CreatedAt, row.UpdatedAt),
	})
}

// PatchAnthropicToken renames, sets-default, and/or rotates the value of ONE of the
// user's tokens (PRD #104 M2). All requested changes apply in one transaction under
// the advisory lock, so a set-default's clear-then-set swap cannot race another
// mutation. Every field is optional; an id that is not the caller's is a 404.
func (h *Handler) PatchAnthropicToken(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	secretID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid secret id")
		return
	}
	var req struct {
		Label   *string `json:"label"`
		Default *bool   `json:"default"`
		Token   *string `json:"token"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// Validate + seal the new value (if any) BEFORE the transaction: sealing needs
	// no DB, and a locked vault must surface as a 409 without ever opening a tx.
	var newLabel string
	if req.Label != nil {
		newLabel, err = validateSecretLabel(*req.Label)
		if err != nil {
			httpx.Error(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	var sealed []byte
	var sealedWith string
	if req.Token != nil {
		tok, verr := validateAnthropicToken(*req.Token)
		if verr != nil {
			httpx.Error(w, http.StatusBadRequest, verr.Error())
			return
		}
		sealed, sealedWith, err = h.sealUserSecret(user.ID, store.KindAnthropicToken, []byte(tok))
		if err != nil {
			if h.writeVaultLocked(w, err) {
				return
			}
			slog.Error("seal anthropic token", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
	}
	// default=false is refused: there is always a default while any token exists
	// (D6), so you promote another token, you never un-default this one.
	if req.Default != nil && !*req.Default {
		httpx.Error(w, http.StatusBadRequest, "cannot clear the default; set another token as default instead")
		return
	}

	var out store.RenameUserSecretRow // id/kind/label/is_default/timestamps, shared shape
	var found bool
	err = h.withSecretLock(r.Context(), user.ID, func(q *store.Queries) error {
		cur, gerr := q.GetUserSecretForUpdate(r.Context(), store.GetUserSecretForUpdateParams{
			ID: secretID, UserID: user.ID,
		})
		if gerr != nil {
			if errors.Is(gerr, pgx.ErrNoRows) {
				return nil // found stays false → 404
			}
			return gerr
		}
		found = true
		// AutoEligible is carried from the CURRENT row, not left zero: this handler
		// never changes the pool flag (that is its own route, D13), so the response
		// must report what the token's flag actually is. A zero here would answer
		// "not pooled" to a rotate of a pooled token — a wrong answer about a
		// setting the user did set, on the one response they are looking at.
		out = store.RenameUserSecretRow{
			ID: cur.ID, Kind: cur.Kind, Label: cur.Label,
			IsDefault: cur.IsDefault, AutoEligible: cur.AutoEligible,
		}

		if req.Token != nil {
			if _, rerr := q.RotateUserSecret(r.Context(), store.RotateUserSecretParams{
				ID: secretID, UserID: user.ID, Ciphertext: sealed, SealedWith: sealedWith,
			}); rerr != nil {
				return rerr
			}
		}
		if req.Label != nil {
			renamed, rerr := q.RenameUserSecret(r.Context(), store.RenameUserSecretParams{
				ID: secretID, UserID: user.ID, Label: newLabel,
			})
			if rerr != nil {
				return rerr
			}
			out = renamed
		}
		if req.Default != nil && *req.Default && !cur.IsDefault {
			if _, cerr := q.ClearDefaultUserSecret(r.Context(), store.ClearDefaultUserSecretParams{
				UserID: user.ID, Kind: store.KindAnthropicToken,
			}); cerr != nil {
				return cerr
			}
			promoted, perr := q.SetUserSecretDefault(r.Context(), store.SetUserSecretDefaultParams{
				ID: secretID, UserID: user.ID,
			})
			if perr != nil {
				return perr
			}
			out = store.RenameUserSecretRow(promoted)
		}
		return nil
	})
	if err != nil {
		if isLabelCollision(err) {
			httpx.Error(w, http.StatusConflict, "a token with that label already exists")
			return
		}
		slog.Error("patch anthropic token", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	if !found {
		httpx.Error(w, http.StatusNotFound, "token not found")
		return
	}
	// A rotate or a default change alters what the poller should read for this user.
	if h.usagePoker != nil {
		h.usagePoker.Poke(user.ID)
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		"secret": secretMeta(out.ID, out.Kind, out.Label, out.IsDefault, out.AutoEligible, out.CreatedAt, out.UpdatedAt),
	})
}

// PatchAnthropicTokenAutoEligible opts ONE of the user's tokens into or out of the
// auto-selection pool (PRD #111 M2, D2). An id that is not the caller's is a 404.
//
// 🔴 WHY THIS IS ITS OWN ROUTE INSTEAD OF A FIELD ON PatchAnthropicToken (D13).
// The obvious implementation — one more optional field on the PATCH above — cannot
// be reached by the CLI, because every secrets WRITE is deliberately cookie-only
// (RequireAuth): "a Bearer-reachable mint would let a stolen uzc_ replace a user's
// tokens" (PRD #104 D8). Moving that PATCH under RequireUser to reach the toggle
// would make RENAME, ROTATE and SET-DEFAULT Bearer-reachable as collateral damage,
// which is precisely the exposure D8 closes.
//
// So the toggle gets a narrow route of its own, mounted under RequireUser, and the
// existing writes stay exactly where they are. The precedent is exact: PATCH
// /workers/{id} is RequireUser because "it mints nothing and yields no credential
// the caller lacks — it only re-points a worker at a token they already hold". This
// is the same class of action: it re-points SPEND among tokens the caller already
// holds. It creates nothing, reveals nothing, and its worst outcome from a stolen
// CLI token is that the thief changes which of the victim's own credentials the
// victim's own runs bill — bad, but strictly less than replacing them.
//
// The ownership check is COPIED, not reinvented, from the mutating handlers above:
// the advisory lock, then the owner-scoped GetUserSecretForUpdate, then 404 (never
// 403) for a foreign id — a 403 would confirm the id names a real credential
// belonging to someone else (handler/judge.go says the same). The lock is arguably
// unnecessary for a lone boolean, since it guards the exactly-one-default
// invariant this write does not touch; it is taken anyway because it costs nothing
// and keeps the toggle from racing a concurrent delete of the same row.
func (h *Handler) PatchAnthropicTokenAutoEligible(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	secretID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid secret id")
		return
	}
	// A POINTER, so "field omitted" is distinguishable from "set to false". A plain
	// bool would silently opt a token OUT for any client that sent `{}`, which is
	// the wrong direction to be lenient in for a spend control.
	var req struct {
		AutoEligible *bool `json:"auto_eligible"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.AutoEligible == nil {
		httpx.Error(w, http.StatusBadRequest, "auto_eligible is required")
		return
	}

	var out store.SetUserSecretAutoEligibleRow
	var found bool
	err = h.withSecretLock(r.Context(), user.ID, func(q *store.Queries) error {
		if _, gerr := q.GetUserSecretForUpdate(r.Context(), store.GetUserSecretForUpdateParams{
			ID: secretID, UserID: user.ID,
		}); gerr != nil {
			if errors.Is(gerr, pgx.ErrNoRows) {
				return nil // found stays false → 404
			}
			return gerr
		}
		found = true
		row, serr := q.SetUserSecretAutoEligible(r.Context(), store.SetUserSecretAutoEligibleParams{
			ID: secretID, UserID: user.ID, AutoEligible: *req.AutoEligible,
		})
		if serr != nil {
			return serr
		}
		out = row
		return nil
	})
	if err != nil {
		slog.Error("patch anthropic token auto-eligible", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	if !found {
		httpx.Error(w, http.StatusNotFound, "token not found")
		return
	}
	// No usagePoker.Poke here, unlike rotate and set-default: pooling a token
	// changes which credential future claims PREFER, not which one the poller reads
	// or what it would read. Poking would spend a header probe to learn nothing.
	httpx.JSON(w, http.StatusOK, map[string]any{
		"secret": secretMeta(out.ID, out.Kind, out.Label, out.IsDefault, out.AutoEligible, out.CreatedAt, out.UpdatedAt),
	})
}

// DeleteAnthropicTokenByID deletes ONE of the user's tokens by id (PRD #104 M2).
// D6: the default may NOT be deleted while other tokens exist — promote another
// first (409). Deleting the LAST token (even though it is the default) is allowed
// and returns the user to the token-less state. D5: workers/judge bound to the
// deleted token fall back to their default automatically via the ON DELETE SET NULL
// FKs, and the token's rate-limit gauge row is dropped by the ON DELETE CASCADE
// (PRD #104 M5) — no app-level cleanup needed.
func (h *Handler) DeleteAnthropicTokenByID(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	secretID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid secret id")
		return
	}

	var found, refusedDefault bool
	err = h.withSecretLock(r.Context(), user.ID, func(q *store.Queries) error {
		cur, gerr := q.GetUserSecretForUpdate(r.Context(), store.GetUserSecretForUpdateParams{
			ID: secretID, UserID: user.ID,
		})
		if gerr != nil {
			if errors.Is(gerr, pgx.ErrNoRows) {
				return nil // found stays false → 404
			}
			return gerr
		}
		found = true
		if cur.IsDefault {
			n, cerr := q.CountUserSecretsForKind(r.Context(), store.CountUserSecretsForKindParams{
				UserID: user.ID, Kind: store.KindAnthropicToken,
			})
			if cerr != nil {
				return cerr
			}
			// The default can be deleted only when it is the LAST token; otherwise the
			// user would be left with tokens and no default. Under the lock this count is
			// stable — no concurrent create can slip a second token in after it (D12).
			if n > 1 {
				refusedDefault = true
				return nil
			}
		}
		_, derr := q.DeleteUserSecret(r.Context(), store.DeleteUserSecretParams{ID: secretID, UserID: user.ID})
		return derr
	})
	if err != nil {
		slog.Error("delete anthropic token by id", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	if !found {
		httpx.Error(w, http.StatusNotFound, "token not found")
		return
	}
	if refusedDefault {
		httpx.Error(w, http.StatusConflict,
			"cannot delete the default token while other tokens exist; set another token as default first")
		return
	}
	if h.usagePoker != nil {
		h.usagePoker.Poke(user.ID)
	}
	w.WriteHeader(http.StatusNoContent)
}

// validateSecretLabel trims and checks a token label (PRD #104 D7): non-empty, at
// most maxLabelBytes, no control characters and no Unicode FORMAT characters.
// Leading/trailing whitespace is trimmed (so the stored value satisfies the
// migration's `label = btrim(label)` CHECK); interior spaces are allowed (a label
// is a human name, not a token).
//
// 🔴 THE Cf HALF IS ABOUT WHAT A LABEL IS FOR, NOT ABOUT TERMINAL SAFETY.
// Since PRD #111 a label is the string a user reads to answer "which account did
// this run bill?" — so a label that can render as something it is not defeats the
// feature it exists to serve. Two measured consequences, both closed here:
//
//   - U+202E (RIGHT-TO-LEFT OVERRIDE) and its neighbours visually REVERSE the text
//     after them, so a label can be made to read as a different account entirely.
//   - Zero-widths make two DISTINCT tokens render identically: `work` and
//     `work`+U+200B are different rows that look the same in a browser, and 00077's
//     unique index is on `lower(label)`, which does not fold them. So the
//     collision the index exists to prevent stays possible while looking prevented.
//
// The predicate is the pair this repo has already settled on twice — sanitizeTTY
// (cmd/uzi/run.go) and workersvc.hasUnsafeChar — rather than a third spelling of
// almost the same rule.
//
// THE COST, DECIDED RATHER THAN DISCOVERED: U+200D (zero-width joiner) is itself a
// format character, so EMOJI ZWJ SEQUENCES ARE NOT STORABLE — `family 👨‍👩‍👧 key` is
// refused, and a single-codepoint emoji like `🔑 key` is fine. That is accepted on
// purpose. A token label is an operational identifier used to answer a billing
// question; the identical-rendering hazard applies to a zero-width JOINER exactly
// as it does to a bidi override, and a partial reject-list (bidi controls only)
// would leave the collision live while looking like it had solved something. The
// error message says so, because a user hitting this deserves the reason and not a
// mystery.
//
// Only NEW writes are validated. Labels stored before this landed are unaffected,
// which is why the CLI still routes every label through cellText — see the tests
// there for why that is defense in depth rather than redundancy.
func validateSecretLabel(raw string) (string, error) {
	label := strings.TrimSpace(raw)
	if label == "" {
		return "", errors.New("label must not be empty")
	}
	if len(label) > maxLabelBytes {
		return "", fmt.Errorf("label must be at most %d bytes", maxLabelBytes)
	}
	for _, r := range label {
		if r == unicode.ReplacementChar || unicode.IsControl(r) {
			return "", errors.New("label must not contain control characters")
		}
		if unicode.In(r, unicode.Cf) {
			return "", errors.New("label must not contain invisible formatting characters " +
				"(zero-width spaces and joiners, bidirectional overrides, the byte-order mark): " +
				"they let two different tokens look identical, or make a label read as a different " +
				"account. This also rules out multi-part emoji such as 👨‍👩‍👧, which are joined by one")
		}
	}
	return label, nil
}

// writeVaultLocked writes the 409 vault_locked envelope when err is vault.ErrLocked
// and reports whether it did, so a caller can `if h.writeVaultLocked(w, err) {
// return }`. The vault is unlocked right after login; a mid-session pod restart is
// the only way to reach locked, which the SPA turns into an unlock prompt.
func (h *Handler) writeVaultLocked(w http.ResponseWriter, err error) bool {
	if !errors.Is(err, vault.ErrLocked) {
		return false
	}
	httpx.JSON(w, http.StatusConflict, map[string]string{
		"error": "vault is locked; unlock it with your password, then save again",
		"code":  "vault_locked",
	})
	return true
}

// isLabelCollision reports whether err is the unique violation on
// user_secrets_user_kind_label_key — a duplicate LABEL — as opposed to the other
// unique index on this table (user_secrets_one_default_key, two defaults).
//
// Distinguishing them matters: mapping every 23505 to "that label already exists"
// would report a default-invariant violation as a naming problem, sending whoever
// hit it to rename a token that was never the issue. Under the mutation lock a
// concurrent create cannot produce the default violation (each create clears the
// old default first, serialized) — so this is defensive, and the point is that if it
// ever DOES fire the message will be honest instead of misleading.
func isLabelCollision(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		return false
	}
	return pgErr.ConstraintName == "user_secrets_user_kind_label_key"
}

// validateAnthropicToken trims and sanity-checks a pasted token. It makes no
// assumption about the token's prefix or format (Anthropic prefixes are not a
// documented contract), only that it is non-empty, within the length bound, and
// free of interior whitespace and control characters. Errors are deliberately
// generic and NEVER include the token bytes.
func validateAnthropicToken(raw string) (string, error) {
	token := strings.TrimSpace(raw)
	if token == "" {
		return "", errors.New("token must not be empty")
	}
	if len(token) > maxTokenBytes {
		return "", fmt.Errorf("token must be at most %d bytes", maxTokenBytes)
	}
	for _, r := range token {
		if r == unicode.ReplacementChar || unicode.IsControl(r) || unicode.IsSpace(r) {
			return "", errors.New("token must not contain whitespace or control characters")
		}
	}
	return token, nil
}
