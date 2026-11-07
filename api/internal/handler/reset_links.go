package handler

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/auth"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/resettoken"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
)

// Admin-issued account reset links (PRD #3989). Lock order everywhere: the vault
// lifecycle mutex, the lifecycle state locks, then the link row. Issue takes only
// the users rows (id-ordered), then touches link rows.

const (
	resetLinkTTL = time.Hour

	resetKindPassword  = "password"
	resetKindVaultOnly = "vault_only"
)

// errResetLinkInvalid aborts the redeem transaction when the locked re-check fails.
var errResetLinkInvalid = errors.New("reset link invalid")

// resetKindFor derives the link kind from the target's current users row.
func resetKindFor(hasPassword bool) string {
	if hasPassword {
		return resetKindPassword
	}
	return resetKindVaultOnly
}

// resetLinkState is the shared view of a link and its target and issuer rows.
type resetLinkState = store.GetResetLinkStateRow

// resetLinkUsable is the one validity rule shared by preview, the pre-lock redeem
// lookup and the locked redeem re-check: unredeemed, unexpired, unsuperseded,
// target active, issuer active and still an admin, and the stored kind equals the
// kind the target's current row derives.
func resetLinkUsable(s resetLinkState, now time.Time) bool {
	return !s.RedeemedAt.Valid &&
		!s.SupersededAt.Valid &&
		s.ExpiresAt.Valid && s.ExpiresAt.Time.After(now) &&
		s.TargetIsActive &&
		s.IssuerIsActive && s.IssuerIsAdmin &&
		s.Kind == resetKindFor(s.TargetHasPassword)
}

// writeResetLinkNotFound is the single writer of the generic failure response for
// preview and redeem, so every failure cause is byte-identical.
func writeResetLinkNotFound(w http.ResponseWriter) {
	httpx.Error(w, http.StatusNotFound, "this link is invalid or expired")
}

// IssueResetLink issues a one-time reset link for another active, non-seed user
// (POST /api/admin/users/{id}/reset-link, cookie-only admin write group).
func (h *Handler) IssueResetLink(w http.ResponseWriter, r *http.Request) {
	actor, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	target, ok := httpx.PathUUID(w, r, "id", "user")
	if !ok {
		return
	}
	if target == actor.ID {
		httpx.Error(w, http.StatusBadRequest, "you cannot issue a reset link for your own account")
		return
	}
	if h.pool == nil {
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}

	ctx := r.Context()
	token, hash, err := resettoken.Generate()
	if err != nil {
		slog.Error("reset link: generate token", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	tx, err := h.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		slog.Error("reset link: begin", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after a successful Commit
	q := h.q.WithTx(tx)

	rows, err := q.LockUsersForResetIssue(ctx, []uuid.UUID{target, actor.ID})
	if err != nil {
		slog.Error("reset link: lock users", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	var tRow, iRow *store.LockUsersForResetIssueRow
	for i := range rows {
		switch rows[i].ID {
		case target:
			tRow = &rows[i]
		case actor.ID:
			iRow = &rows[i]
		}
	}
	// Re-check the issuer from the locked row: the middleware's copy may predate a
	// concurrent deactivation or demotion.
	if iRow == nil || !iRow.IsActive || !iRow.IsAdmin {
		httpx.Error(w, http.StatusForbidden, "admin access required")
		return
	}
	if tRow == nil {
		httpx.Error(w, http.StatusNotFound, "user not found")
		return
	}
	if !tRow.IsActive {
		httpx.Error(w, http.StatusBadRequest, "the user is deactivated")
		return
	}
	if h.cfg.SeedEmail != "" && tRow.Email == h.cfg.SeedEmail {
		httpx.Error(w, http.StatusBadRequest, "the seed admin cannot be reset with a link")
		return
	}
	kind := resetKindFor(tRow.HasPassword)

	if _, err := q.SupersedeLiveResetLinksForTarget(ctx, target); err != nil {
		slog.Error("reset link: supersede", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	expires := h.clock().Add(resetLinkTTL)
	link, err := q.InsertResetLink(ctx, store.InsertResetLinkParams{
		UserID:        target,
		TokenHash:     hash,
		Kind:          kind,
		TargetIsAdmin: tRow.IsAdmin,
		IssuedBy:      actor.ID,
		ExpiresAt:     pgtype.Timestamptz{Time: expires, Valid: true},
	})
	if err != nil {
		slog.Error("reset link: insert", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		slog.Error("reset link: commit", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	slog.Info("reset link issued", "issuer", actor.ID, "target", target, "target_is_admin", tRow.IsAdmin)
	// The response carries a one-time reset URL; never let a cache keep it.
	w.Header().Set("Cache-Control", "no-store")
	httpx.JSON(w, http.StatusCreated, apitypes.AccountResetLinkDTO{
		Token:         token,
		ExpiresAt:     link.ExpiresAt.Time,
		Kind:          kind,
		TargetIsAdmin: tRow.IsAdmin,
	})
}

// PreviewResetLink answers POST /api/auth/reset/preview: {kind, expires_at} for a
// usable link, otherwise the one generic 404. It writes nothing.
func (h *Handler) PreviewResetLink(w http.ResponseWriter, r *http.Request) {
	var req apitypes.ResetLinkPreviewRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	hash, ok := resettoken.Hash(req.Token)
	if !ok {
		writeResetLinkNotFound(w)
		return
	}
	row, err := h.q.GetResetLinkState(r.Context(), hash)
	if errors.Is(err, pgx.ErrNoRows) {
		writeResetLinkNotFound(w)
		return
	}
	if err != nil {
		slog.Error("reset link preview: lookup", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	state := resetLinkState(row)
	if !resetLinkUsable(state, h.clock()) {
		writeResetLinkNotFound(w)
		return
	}
	httpx.JSON(w, http.StatusOK, apitypes.ResetLinkPreviewDTO{
		Kind:      resetKindFor(state.TargetHasPassword),
		ExpiresAt: state.ExpiresAt.Time,
	})
}

// RedeemResetLink answers POST /api/auth/reset/redeem. It resets the account's
// credentials and sessions in vault.Reset's transaction and does not log in.
func (h *Handler) RedeemResetLink(w http.ResponseWriter, r *http.Request) {
	var req apitypes.ResetLinkRedeemRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	// Bound and hash the password before any lookup or lock: the expensive step must
	// not run under a lock, and the bounds depend on nothing the token reveals.
	var passwordHash string
	if req.Password != "" {
		if len(req.Password) < auth.MinPasswordLen {
			httpx.Error(w, http.StatusBadRequest, "password must be at least 12 characters")
			return
		}
		if len(req.Password) > auth.MaxPasswordLen {
			httpx.Error(w, http.StatusBadRequest, "password is too long")
			return
		}
		hashed, err := auth.HashPassword(req.Password)
		if err != nil {
			slog.Error("reset link redeem: hash password", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
		passwordHash = hashed
	}
	tokenHash, ok := resettoken.Hash(req.Token)
	if !ok {
		writeResetLinkNotFound(w)
		return
	}
	ctx := r.Context()
	row, err := h.q.GetResetLinkState(ctx, tokenHash)
	if errors.Is(err, pgx.ErrNoRows) {
		writeResetLinkNotFound(w)
		return
	}
	if err != nil {
		slog.Error("reset link redeem: lookup", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	state := resetLinkState(row)
	if !resetLinkUsable(state, h.clock()) || !resetPasswordMatchesKind(state.Kind, req.Password != "") {
		writeResetLinkNotFound(w)
		return
	}
	if h.vault == nil {
		httpx.Error(w, http.StatusServiceUnavailable, "account reset is unavailable")
		return
	}

	owner := state.UserID
	err = h.vault.Reset(ctx, owner, func(ctx context.Context, bq vault.BrowserStore) error {
		return h.consumeResetLink(ctx, bq, owner, tokenHash, passwordHash, req.Password != "")
	})
	switch {
	case err == nil:
	case errors.Is(err, errResetLinkInvalid):
		writeResetLinkNotFound(w)
		return
	case errors.Is(err, vault.ErrUncertainOutcome):
		slog.Warn("reset link redeem outcome uncertain", "user", owner)
		msg := "the reset could not be confirmed; try signing in with the new password, or ask for a new link"
		if state.Kind != resetKindPassword {
			// A vault_only link sets no password, so there is no new password to try.
			msg = "the reset could not be confirmed; try signing in with single sign-on, or ask for a new link"
		}
		httpx.Error(w, http.StatusServiceUnavailable, msg)
		return
	default:
		slog.Error("reset link redeem failed", "user", owner, "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	slog.Info("reset link redeemed", "user", owner, "issuer", state.IssuedBy, "kind", state.Kind, "target_is_admin", state.TargetIsAdmin)
	w.WriteHeader(http.StatusNoContent)
}

// resetPasswordMatchesKind: a password link needs a password, a vault_only link
// must not carry one.
func resetPasswordMatchesKind(kind string, hasPassword bool) bool {
	return (kind == resetKindPassword) == hasPassword
}

// consumeResetLink is the caller callback of vault.Reset. Reset may run it twice
// (one deadlock retry), so it is a pure function of the transaction it is given.
func (h *Handler) consumeResetLink(ctx context.Context, bq vault.BrowserStore, owner uuid.UUID, tokenHash []byte, passwordHash string, hasPassword bool) error {
	q, ok := bq.(*store.Queries)
	if !ok {
		return vault.ErrNotConfigured
	}
	row, err := q.LockResetLinkForRedeem(ctx, tokenHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return errResetLinkInvalid
	}
	if err != nil {
		return err
	}
	locked := resetLinkState(store.GetResetLinkStateRow(row))
	if locked.UserID != owner || !resetLinkUsable(locked, h.clock()) || !resetPasswordMatchesKind(locked.Kind, hasPassword) {
		return errResetLinkInvalid
	}
	if locked.Kind == resetKindPassword {
		// UpdatePassword also bumps token_version, ending every browser session.
		if err := q.UpdatePassword(ctx, store.UpdatePasswordParams{ID: owner, PasswordHash: pgtype.Text{String: passwordHash, Valid: true}}); err != nil {
			return err
		}
	} else if _, err := q.BumpTokenVersion(ctx, owner); err != nil {
		return err
	}
	if _, err := q.DenyApprovedCLIAuthRequestsForUser(ctx, pgtype.UUID{Bytes: owner, Valid: true}); err != nil {
		return err
	}
	n, err := q.MarkResetLinkRedeemed(ctx, locked.ID)
	if err != nil {
		return err
	}
	if n == 0 {
		return errResetLinkInvalid
	}
	return q.SetUserCredentialsResetAt(ctx, owner)
}

// setUserActiveSuperseding is the non-vault fallback of PatchUser: the users-row
// update and, on deactivation, the reset-link supersession are one transaction,
// the supersession a separate statement after the update (PRD #3989 D15).
func (h *Handler) setUserActiveSuperseding(ctx context.Context, arg store.SetUserActiveParams) (store.User, error) {
	var user store.User
	err := h.inUsersTx(ctx, func(q *store.Queries) error {
		var err error
		user, err = q.SetUserActive(ctx, arg)
		if err != nil {
			return err
		}
		if !arg.IsActive {
			_, err = q.SupersedeLiveResetLinksForUser(ctx, store.SupersedeLiveResetLinksForUserParams{ID: arg.ID, AsTarget: true})
		}
		return err
	})
	return user, err
}

// setUserAdminSuperseding writes is_admin and, on demotion, supersedes the links
// the user issued (a separate statement after the update, same transaction). A
// demoted user's own reset links as a target stay valid: their kind does not
// depend on admin status.
func (h *Handler) setUserAdminSuperseding(ctx context.Context, arg store.SetUserAdminParams) (store.User, error) {
	var user store.User
	err := h.inUsersTx(ctx, func(q *store.Queries) error {
		var err error
		user, err = q.SetUserAdmin(ctx, arg)
		if err != nil {
			return err
		}
		if !arg.IsAdmin {
			_, err = q.SupersedeLiveResetLinksForUser(ctx, store.SupersedeLiveResetLinksForUserParams{ID: arg.ID, AsTarget: false})
		}
		return err
	})
	return user, err
}

func (h *Handler) inUsersTx(ctx context.Context, fn func(*store.Queries) error) error {
	tx, err := h.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after a successful Commit
	if err := fn(h.q.WithTx(tx)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
