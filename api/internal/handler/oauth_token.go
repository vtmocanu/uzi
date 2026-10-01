package handler

import (
	"context"
	"crypto/sha256"
	"errors"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/oauthsrv"
	"github.com/vtmocanu/uzi/api/internal/producttoken"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// The token half of uzi's OAuth authorization server (PRD #1910 M3, D5 to D8): POST
// /api/oauth/token with grant_type=authorization_code. Every response, success or error, carries
// Cache-Control: no-store and Pragma: no-cache (oauthTokenNoStore sets them in front of the rate
// limiter, so its 429 has them too).

const (
	// oauthTokenBodyCap is the largest form body the endpoint reads (PRD #1910 D7).
	oauthTokenBodyCap = 16 << 10
	// oauthAccessTokenTTL is an access token's lifetime (PRD #1910 D5), and so expires_in.
	oauthAccessTokenTTL = time.Hour
	// oauthMaxLiveGrantTokens is the bound on live (unrevoked, unexpired) access tokens per grant
	// (PRD #1910 D5), enforced under the grant lock on every mint.
	oauthMaxLiveGrantTokens = 10
	// oauthStorageRetryAfter is the Retry-After (seconds) of a 503 temporarily_unavailable.
	oauthStorageRetryAfter = 5
	// oauthMaxCodeLen bounds a presented code before it is hashed: a real code is 43 characters.
	oauthMaxCodeLen = 256
	// oauthGrantRowName is the name column of an OAuth-issued product_tokens row.
	oauthGrantRowName = "OAuth access token"
)

// oauthDummySecretHash is compared against when the client is unknown, so an unknown client_id
// costs the same constant-time compare as a wrong secret.
var oauthDummySecretHash = func() []byte { s := sha256.Sum256([]byte("uzi oauth unknown client")); return s[:] }()

// oauthTokenNoStore sets the token endpoint's cache headers before anything else runs.
func oauthTokenNoStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Pragma", "no-cache")
		next.ServeHTTP(w, r)
	})
}

// oauthTokenFailure is a token-endpoint refusal decided inside the redemption transaction.
type oauthTokenFailure struct {
	status     int
	code       string
	retryAfter int // seconds; sent as Retry-After when positive
}

var (
	errOAuthInvalidGrant = &oauthTokenFailure{status: http.StatusBadRequest, code: oauthsrv.ErrInvalidGrant}
)

func writeOAuthTokenError(w http.ResponseWriter, f *oauthTokenFailure, description string) {
	if f.retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(f.retryAfter))
	}
	if f.status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Basic realm="uzi"`)
	}
	httpx.JSON(w, f.status, apitypes.OAuthErrorResponse{Error: f.code, ErrorDescription: description})
}

func oauthInvalidRequest(w http.ResponseWriter, description string) {
	writeOAuthTokenError(w, &oauthTokenFailure{status: http.StatusBadRequest, code: oauthsrv.ErrInvalidRequest}, description)
}

func oauthInvalidClient(w http.ResponseWriter) {
	writeOAuthTokenError(w, &oauthTokenFailure{status: http.StatusUnauthorized, code: oauthsrv.ErrInvalidClient}, "client authentication failed")
}

// oauthUnavailable is the 503 of a storage or lookup error (PRD #1910 D7): never invalid_grant or
// invalid_client, because a product that sees invalid_grant discards the connection.
func oauthUnavailable(w http.ResponseWriter) {
	writeOAuthTokenError(w, &oauthTokenFailure{status: http.StatusServiceUnavailable, code: oauthsrv.ErrTemporarilyUnavailable, retryAfter: oauthStorageRetryAfter}, "")
}

// oauthBasicCredentials reads the HTTP Basic client credentials and form-urlencoded-decodes the
// id and the secret (RFC 6749 section 2.3.1). ok is false when the header is absent or malformed.
func oauthBasicCredentials(r *http.Request) (id, secret string, ok bool) {
	rawID, rawSecret, has := r.BasicAuth()
	if !has {
		return "", "", false
	}
	id, err := url.QueryUnescape(rawID)
	if err != nil {
		return "", "", false
	}
	secret, err = url.QueryUnescape(rawSecret)
	if err != nil {
		return "", "", false
	}
	return id, secret, true
}

// OAuthToken is POST /api/oauth/token for grant_type=authorization_code (PRD #1910 D5 to D8).
// limiter is the oauthLimiter whose per-IP Middleware already fronts the route; this handler draws
// the per-client budget from it, and ONLY after the client authenticated, so a flood of failed
// authentications for a client_id cannot exhaust that client's own budget.
//
// The order is: request rules (no query string, form-encoded, 16 KiB, no repeated parameter),
// client authentication (HTTP Basic only), the per-client budget, then the code redemption
// transaction (redeemOAuthCode). Nothing is consumed or revoked before the client is authenticated,
// and a code is redeemed only after every binding check passed.
func (h *Handler) OAuthToken(limiter *mw.Limiter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Pragma", "no-cache")
		ctx := r.Context()

		// RFC 6749 section 3.2: parameters travel in the form body only. A query string, even an
		// empty-valued one, is refused rather than silently merged.
		if r.URL.RawQuery != "" {
			oauthInvalidRequest(w, "parameters must be sent in the request body")
			return
		}
		if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/x-www-form-urlencoded" {
			oauthInvalidRequest(w, "Content-Type must be application/x-www-form-urlencoded")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, oauthTokenBodyCap)
		if err := r.ParseForm(); err != nil {
			oauthInvalidRequest(w, "the request body is not a valid form of at most 16 KiB")
			return
		}
		form := oauthsrv.ParseTokenForm(r.PostForm)
		if form.Repeated {
			oauthInvalidRequest(w, "a parameter was sent more than once")
			return
		}

		// Client authentication: HTTP Basic only (D2). A second method next to it is invalid_request.
		basicID, basicSecret, hasBasic := oauthBasicCredentials(r)
		if hasBasic && (form.HasClientSecret || (form.ClientID != "" && form.ClientID != basicID)) {
			oauthInvalidRequest(w, "use exactly one client authentication method")
			return
		}
		if !hasBasic {
			oauthInvalidClient(w)
			return
		}
		clientID, err := uuid.Parse(basicID)
		if err != nil {
			oauthInvalidClient(w)
			return
		}
		product, err := h.q.GetProduct(ctx, clientID)
		known := true
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			known = false
		case err != nil:
			slog.Error("oauth token: load client", "error", err)
			oauthUnavailable(w)
			return
		}
		storedHash := oauthDummySecretHash
		if known {
			storedHash = product.ClientSecretHash
		}
		// Always run the constant-time compare, so an unknown client, a non-client product and a
		// wrong secret are the same answer at the same cost.
		secretOK := oauthsrv.SecretMatches(basicSecret, storedHash)
		if !known || !secretOK || !oauthClientFromProduct(product).IsClient() {
			oauthInvalidClient(w)
			return
		}

		// The client is authenticated: only now does the per-client budget move.
		if !limiter.Allow("client:" + clientID.String()) {
			writeOAuthTokenError(w, &oauthTokenFailure{
				status: http.StatusTooManyRequests, code: oauthsrv.ErrTemporarilyUnavailable,
				retryAfter: max(1, int(limiter.Window().Seconds())),
			}, "")
			return
		}

		switch form.GrantType {
		case "authorization_code":
		case "":
			oauthInvalidRequest(w, "grant_type is required")
			return
		default:
			writeOAuthTokenError(w, &oauthTokenFailure{status: http.StatusBadRequest, code: oauthsrv.ErrUnsupportedGrantType}, "grant_type must be authorization_code")
			return
		}
		if form.Code == "" {
			oauthInvalidRequest(w, "code is required")
			return
		}

		resp, fail, err := h.redeemOAuthCode(ctx, clientID, form)
		if err != nil {
			slog.Error("oauth token: redeem code", "error", err)
			oauthUnavailable(w)
			return
		}
		if fail != nil {
			writeOAuthTokenError(w, fail, "")
			return
		}
		httpx.JSON(w, http.StatusOK, resp)
	}
}

func (h *Handler) beginOAuthTx(ctx context.Context) (pgx.Tx, error) {
	if h.oauthBeginTx != nil {
		return h.oauthBeginTx(ctx)
	}
	return h.pool.Begin(ctx)
}

// redeemOAuthCode redeems an authorization code in ONE transaction, in D8 lock order: the code's
// request is found by its hash without a lock, then the grant is locked FOR UPDATE, then the
// request row is locked and re-read under it. Every check runs BEFORE the conditional UPDATE that
// marks the code redeemed, so a wrong redirect URI, a wrong verifier, an expired or superseded
// code, a revoked grant, a client or user that no longer qualifies, or a grant at its
// ten-live-token bound consumes nothing: the transaction rolls back and the same code can be
// presented again. Only then are the access token minted and the refresh token stored.
//
// A non-nil error is a storage failure (the caller answers 503, never an OAuth credential error).
// A non-nil failure is a protocol refusal. A replay of an already redeemed code revokes the grant
// ONLY when the client, redirect URI and PKCE verifier all match (D6): anything less is a plain
// invalid_grant that revokes nothing, because the code alone is not proof of being the client.
func (h *Handler) redeemOAuthCode(ctx context.Context, clientID uuid.UUID, form oauthsrv.TokenForm) (*apitypes.OAuthTokenResponse, *oauthTokenFailure, error) {
	if len(form.Code) > oauthMaxCodeLen {
		return nil, errOAuthInvalidGrant, nil
	}
	codeHash := sha256.Sum256([]byte(form.Code))

	tx, err := h.beginOAuthTx(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit; every refusal relies on it to leave the code unconsumed
	q := h.q.WithTx(tx)

	found, err := q.GetOAuthAuthorizeRequestByCodeHash(ctx, codeHash[:])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errOAuthInvalidGrant, nil
	}
	if err != nil {
		return nil, nil, err
	}
	if !found.GrantID.Valid {
		return nil, errOAuthInvalidGrant, nil
	}
	grant, err := q.LockOAuthGrant(ctx, found.GrantID.Bytes)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errOAuthInvalidGrant, nil
	}
	if err != nil {
		return nil, nil, err
	}
	row, err := q.LockOAuthAuthorizeRequest(ctx, found.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errOAuthInvalidGrant, nil
	}
	if err != nil {
		return nil, nil, err
	}

	// Bindings: the code belongs to this client, this exact redirect URI and this PKCE verifier.
	if row.ProductID != clientID || form.RedirectURI == "" || form.RedirectURI != row.RedirectUri ||
		!oauthsrv.VerifierMatchesS256(form.CodeVerifier, row.CodeChallenge) {
		return nil, errOAuthInvalidGrant, nil
	}
	switch row.Status {
	case "approved":
	case "redeemed":
		// A replay that passed every binding: RFC 6749 section 4.1.2 says to revoke what the code
		// issued, so the grant (and everything under it) goes, atomically with this refusal.
		if !grant.RevokedAt.Valid {
			if err := revokeGrantLocked(ctx, q, grant.ID); err != nil {
				return nil, nil, err
			}
			if err := tx.Commit(ctx); err != nil {
				return nil, nil, err
			}
		}
		return nil, errOAuthInvalidGrant, nil
	default: // pending, denied, superseded
		return nil, errOAuthInvalidGrant, nil
	}
	if !row.CodeExpiresAt.Valid || !row.CodeExpiresAt.Time.After(time.Now()) || grant.RevokedAt.Valid {
		return nil, errOAuthInvalidGrant, nil
	}

	// The product and user may have changed since the code was issued (approve checks them outside
	// its own transaction): the product must still be an enabled client registering this URI and
	// allowing the grant's scopes, and the user must still be active.
	product, err := q.GetProduct(ctx, row.ProductID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errOAuthInvalidGrant, nil
	}
	if err != nil {
		return nil, nil, err
	}
	if !oauthClientAllows(product, row.RedirectUri, grant.Scopes) {
		return nil, errOAuthInvalidGrant, nil
	}
	user, err := q.GetUserByID(ctx, grant.UserID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errOAuthInvalidGrant, nil
	}
	if err != nil {
		return nil, nil, err
	}
	if !user.IsActive {
		return nil, errOAuthInvalidGrant, nil
	}

	// The ten-live-token bound (D5), under the grant lock: a refusal here still leaves the code
	// redeemable, and the product is told when the oldest live token frees a slot.
	live, err := q.CountLiveGrantTokens(ctx, pgtype.UUID{Bytes: grant.ID, Valid: true})
	if err != nil {
		return nil, nil, err
	}
	if live.Live >= oauthMaxLiveGrantTokens {
		wait := 1
		if live.OldestExpiresAt.Valid {
			wait = max(1, int(time.Until(live.OldestExpiresAt.Time).Seconds())+1)
		}
		return nil, &oauthTokenFailure{status: http.StatusTooManyRequests, code: oauthsrv.ErrTemporarilyUnavailable, retryAfter: wait}, nil
	}

	// Every check passed: redeem, mint, issue the refresh token, commit.
	if _, err := q.RedeemOAuthAuthorizeRequest(ctx, row.ID); errors.Is(err, pgx.ErrNoRows) {
		return nil, errOAuthInvalidGrant, nil
	} else if err != nil {
		return nil, nil, err
	}
	access, accessHash, accessPrefix, err := producttoken.Generate()
	if err != nil {
		return nil, nil, err
	}
	if _, err := q.CreateGrantProductToken(ctx, store.CreateGrantProductTokenParams{
		UserID:      grant.UserID,
		ProductID:   product.ID,
		Name:        oauthGrantRowName,
		TokenHash:   accessHash,
		TokenPrefix: accessPrefix,
		Scopes:      grant.Scopes,
		ExpiresAt:   pgtype.Timestamptz{Time: time.Now().Add(oauthAccessTokenTTL), Valid: true},
		GrantID:     grant.ID,
	}); errors.Is(err, pgx.ErrNoRows) {
		return nil, errOAuthInvalidGrant, nil // the product was disabled or deleted meanwhile
	} else if err != nil {
		return nil, nil, err
	}
	// A fresh code exchange normally finds no refresh token (re-consent clears it). If the grant
	// somehow holds one, it is replaced: at most one refresh token is ever live (D5).
	refresh, refreshHash, refreshPrefix, err := oauthsrv.GenerateRefreshToken()
	if err != nil {
		return nil, nil, err
	}
	if _, err := q.SetOAuthGrantRefreshToken(ctx, store.SetOAuthGrantRefreshTokenParams{
		ID: grant.ID, RefreshTokenHash: refreshHash, RefreshTokenPrefix: refreshPrefix,
	}); err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, nil, err
	}
	return &apitypes.OAuthTokenResponse{
		AccessToken:  access,
		TokenType:    "Bearer",
		ExpiresIn:    int64(oauthAccessTokenTTL.Seconds()),
		RefreshToken: refresh,
		Scope:        strings.Join(grant.Scopes, " "),
	}, nil, nil
}

// revokeGrantLocked is the one revoke of a grant (PRD #1910 D6): every access token of the grant
// is set revoked (expired or not, so the ListRevokedProductJobs sweep cancels the jobs it created),
// its unredeemed codes are superseded, and the grant itself gets revoked_at with its refresh token
// cleared. It REQUIRES the grant row to be locked FOR UPDATE already by the caller's transaction
// (D8): the lock order is grant, then its product_tokens, then its request rows, so every revoke
// path (replay here; the user, admin, revoke-all and RFC 7009 paths of later milestones) calls
// this and none takes a token or request lock before the grant's.
func revokeGrantLocked(ctx context.Context, q *store.Queries, grantID uuid.UUID) error {
	if _, err := q.RevokeOAuthGrantProductTokens(ctx, pgtype.UUID{Bytes: grantID, Valid: true}); err != nil {
		return err
	}
	if _, err := q.SupersedeOAuthGrantCodes(ctx, pgtype.UUID{Bytes: grantID, Valid: true}); err != nil {
		return err
	}
	_, err := q.RevokeOAuthGrantRow(ctx, grantID)
	return err
}
