package handler

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"log/slog"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/producttoken"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// OAuthRevoke is POST /api/oauth/revoke (RFC 7009, PRD #1910 D6 and D7): the product disconnects
// itself. The request rules, client authentication and per-client budget are the token endpoint's
// (oauthAuthenticatedClient). The form carries token (required) and token_type_hint (advisory: an
// unknown hint is ignored, and a hint naming the wrong type falls through to the other, because
// the default order is refresh token, then access token).
//
//   - A refresh token the client holds revokes its whole grant (revokeGrantLocked): the grant, every
//     access token under it, the refresh token itself and any unredeemed code.
//   - An access token issued to the client by an OAuth grant is revoked alone. RFC 7009 section 2.1
//     lets the server revoke the grant too, but D6 lists only the refresh path as ending the
//     connection, and a product that drops one access token must be able to refresh.
//   - A live manual (pasted) uzp_ token of the client's own product has no grant: it was not issued
//     to the client by OAuth, so it answers 200 and is NOT revoked here (the owner revokes it).
//   - A LIVE (unrevoked, unexpired) token of another client's product (an OAuth grant token or a manual one) is
//     400 invalid_grant (RFC 6749 section 5.2) and revokes nothing.
//   - An unknown token is 200, and so is an access token that is already revoked, or expired and not
//     the client's own (a dead token is answered before the other-client check, as a revoked refresh
//     token, whose hash is cleared, is simply unknown). The client's own expired access token is
//     also 200 and is marked revoked, so the revoked-jobs sweep covers the jobs it created.
//
// Every write runs with the grant locked FOR UPDATE first (D8). A storage error is 503
// temporarily_unavailable with Retry-After, never an OAuth credential error.
func (h *Handler) OAuthRevoke(limiter *mw.Limiter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		clientID, form, ok := h.oauthAuthenticatedClient(w, r, limiter, "revoke")
		if !ok {
			return
		}
		if form.Token == "" {
			oauthInvalidRequest(w, "token is required")
			return
		}
		fail, err := h.revokeOAuthToken(r.Context(), clientID, form.Token, form.TokenTypeHint)
		if err != nil {
			slog.Error("oauth revoke", "error", err)
			oauthUnavailable(w)
			return
		}
		if fail != nil {
			writeOAuthTokenError(w, fail, "")
			return
		}
		w.WriteHeader(http.StatusOK)
	}
}

// revokeOAuthToken tries the token as the two supported types in the hint's order (refresh first
// unless the hint is access_token) and stops at the first type that knows the token. A nil failure
// and nil error is the 200 (revoked, or nothing to revoke).
func (h *Handler) revokeOAuthToken(ctx context.Context, clientID uuid.UUID, token, hint string) (*oauthTokenFailure, error) {
	if len(token) > oauthMaxTokenLen {
		return nil, nil // no token of ours is that long: unknown
	}
	steps := []func(context.Context, uuid.UUID, string) (bool, *oauthTokenFailure, error){h.revokeRefreshToken, h.revokeAccessToken}
	if hint == "access_token" {
		steps[0], steps[1] = steps[1], steps[0]
	}
	for _, step := range steps {
		found, fail, err := step(ctx, clientID, token)
		if err != nil || fail != nil || found {
			return fail, err
		}
	}
	return nil, nil
}

// revokeRefreshToken revokes the grant holding refresh token tok when the client owns it. found
// is false when no grant holds the token (now), so the caller tries the next type.
func (h *Handler) revokeRefreshToken(ctx context.Context, clientID uuid.UUID, tok string) (found bool, fail *oauthTokenFailure, err error) {
	hash := sha256.Sum256([]byte(tok))
	tx, err := h.beginOAuthTx(ctx)
	if err != nil {
		return false, nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit
	q := h.q.WithTx(tx)

	grantID, err := q.GetOAuthGrantIDByRefreshHash(ctx, hash[:])
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil, nil
	}
	if err != nil {
		return false, nil, err
	}
	grant, err := q.LockOAuthGrant(ctx, grantID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil, nil
	}
	if err != nil {
		return false, nil, err
	}
	// Re-check under the lock: a revoke or re-consent that committed first took the token away,
	// which is the "already revoked" 200 (found stays true: the token is not another type's).
	if grant.RevokedAt.Valid || len(grant.RefreshTokenHash) == 0 || subtle.ConstantTimeCompare(hash[:], grant.RefreshTokenHash) != 1 {
		return true, nil, nil
	}
	if grant.ProductID != clientID {
		return true, errOAuthInvalidGrant, nil
	}
	if err := revokeGrantLocked(ctx, q, grant.ID); err != nil {
		return false, nil, err
	}
	return true, nil, tx.Commit(ctx)
}

// revokeAccessToken revokes the OAuth-issued access token tok (a uzp_ product_tokens row with a
// grant_id) of the client alone, under its grant's lock. found is false when no product token
// carries the hash.
func (h *Handler) revokeAccessToken(ctx context.Context, clientID uuid.UUID, tok string) (found bool, fail *oauthTokenFailure, err error) {
	row, err := h.q.GetProductTokenForOAuthRevoke(ctx, producttoken.Hash(tok))
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil, nil
	}
	if err != nil {
		return false, nil, err
	}
	// A token that is already revoked, or expired and not this client's, answers 200 BEFORE the
	// other-client check (the same as a revoked refresh token, whose hash is cleared so the lookup
	// misses): there is nothing left for it to revoke, and the answer must not tell a client which
	// dead tokens exist elsewhere. Only a LIVE token of another product is refused. An expired token
	// of the client's own grant still falls through to be marked revoked, so the revoked-jobs sweep
	// cancels what it created.
	if row.Revoked || (row.Expired && row.ProductID != clientID) {
		return true, nil, nil
	}
	if row.ProductID != clientID {
		return true, errOAuthInvalidGrant, nil
	}
	if !row.GrantID.Valid {
		return true, nil, nil // a manual token is not the client's to revoke
	}
	tx, err := h.beginOAuthTx(ctx)
	if err != nil {
		return false, nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit
	q := h.q.WithTx(tx)
	// D8: the grant first, then the token row. grant_id never changes after insert.
	if _, err := q.LockOAuthGrant(ctx, row.GrantID.Bytes); errors.Is(err, pgx.ErrNoRows) {
		return true, nil, nil
	} else if err != nil {
		return false, nil, err
	}
	if _, err := q.RevokeOAuthGrantAccessToken(ctx, store.RevokeOAuthGrantAccessTokenParams{ID: row.ID, GrantID: row.GrantID}); err != nil {
		return false, nil, err
	}
	return true, nil, tx.Commit(ctx)
}
