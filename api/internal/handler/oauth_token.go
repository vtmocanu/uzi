package handler

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"slices"
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

// The token half of uzi's OAuth authorization server (PRD #1910 M3 and M4, D5 to D8): POST
// /api/oauth/token with grant_type=authorization_code or refresh_token. Every response, success or error, carries
// Cache-Control: no-store and Pragma: no-cache: oauthNoStore wraps the whole /api/oauth route group
// (mountOAuthRoutes), in front of the rate limiter and of the router's own 404 and 405, so the
// limiter's 429 and a GET on the token path have them too.

const (
	// oauthTokenBodyCap is the largest form body the endpoint reads (PRD #1910 D7).
	oauthTokenBodyCap = 16 << 10
	// oauthAccessTokenTTL is an access token's lifetime (PRD #1910 D5), and so expires_in.
	oauthAccessTokenTTL = time.Hour
	// oauthMaxLiveGrantTokens is the bound on live (unrevoked, unexpired) access tokens per grant
	// (PRD #1910 D5), enforced under the grant lock on every mint.
	oauthMaxLiveGrantTokens = 10
	// oauthMaxGrantMintsPerWindow is the bound on access tokens MINTED for one grant per
	// oauthGrantMintWindow, revoked ones included (PRD #1910 D5, Resource bounds): the live-token
	// cap alone is no bound on stored rows, because a product that refreshes and then revokes each
	// new access token (RFC 7009) keeps its live count at zero. Enforced under the grant lock on
	// every mint, with a 429 temporarily_unavailable and a Retry-After. A product refreshes about
	// once an hour, so 30 an hour is generous.
	oauthMaxGrantMintsPerWindow = 30
	oauthGrantMintWindow        = time.Hour
	// oauthStorageRetryAfter is the Retry-After (seconds) of a 503 temporarily_unavailable.
	oauthStorageRetryAfter = 5
	// oauthMaxTokenLen bounds a presented authorization code, refresh token or revoke token before
	// it is hashed: none of ours is longer than 64 characters (a uzr_ or uzp_ token is the longest).
	oauthMaxTokenLen = 256
	// oauthRefreshIdleLimit and oauthRefreshAbsoluteLimit are the refresh token's lifetimes (PRD
	// #1910 D5): 30 days idle (from the last refresh, else from issue) and 90 days absolute from
	// the user's latest consent (oauth_grants.consented_at, which a re-consent resets).
	oauthRefreshIdleLimit     = 30 * 24 * time.Hour
	oauthRefreshAbsoluteLimit = 90 * 24 * time.Hour
	// oauthGrantRowName is the name column of an OAuth-issued product_tokens row.
	oauthGrantRowName = "OAuth access token"
)

// oauthDummySecretHash is compared against when the client is unknown or has no secret, so
// neither costs less than a wrong secret: SecretMatches returns early on an empty stored hash.
var oauthDummySecretHash = func() []byte { s := sha256.Sum256([]byte("uzi oauth unknown client")); return s[:] }()

// oauthNoStore sets the OAuth endpoints' cache headers before anything else runs.
func oauthNoStore(next http.Handler) http.Handler {
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
	// errOAuthInvalidClient is the 401 (with WWW-Authenticate) for a client registration that
	// stopped qualifying between authentication and the grant lock, like the auth-time refusal of
	// a disabled product: the grant is intact, so a product must keep its refresh token.
	errOAuthInvalidClient = &oauthTokenFailure{status: http.StatusUnauthorized, code: oauthsrv.ErrInvalidClient}
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

// oauthRateLimited is the per-IP limiter's refusal on the token endpoint: the OAuth-shaped 429
// (an extension, like the per-client and ten-token ones) instead of the generic JSON error, so a
// product's client library sees a protocol error. Retry-After is already set by the limiter.
func oauthRateLimited(w http.ResponseWriter, _ int) {
	httpx.JSON(w, http.StatusTooManyRequests, apitypes.OAuthErrorResponse{Error: oauthsrv.ErrTemporarilyUnavailable})
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

// oauthAuthenticatedClient runs the request rules and client authentication shared by POST
// /api/oauth/token and POST /api/oauth/revoke (PRD #1910 D7) and returns the authenticated client
// and the parsed form. ok is false when it already wrote the refusal. limiter is the oauthLimiter
// whose per-IP Middleware fronts the route; the per-client budget is drawn from it ONLY after the
// client authenticated, so a flood of failed authentications for a client_id cannot exhaust that
// client's own budget.
//
// The order is: request rules (no query string, form-encoded, 16 KiB, no repeated parameter),
// client authentication (HTTP Basic only), then the per-client budget. Nothing is consumed or
// revoked before the client is authenticated.
func (h *Handler) oauthAuthenticatedClient(w http.ResponseWriter, r *http.Request, limiter *mw.Limiter, endpoint string) (uuid.UUID, oauthsrv.TokenForm, bool) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	ctx := r.Context()

	// RFC 6749 section 3.2: parameters travel in the form body only. A query string, even an
	// empty-valued one, is refused rather than silently merged.
	if r.URL.RawQuery != "" {
		oauthInvalidRequest(w, "parameters must be sent in the request body")
		return uuid.Nil, oauthsrv.TokenForm{}, false
	}
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/x-www-form-urlencoded" {
		oauthInvalidRequest(w, "Content-Type must be application/x-www-form-urlencoded")
		return uuid.Nil, oauthsrv.TokenForm{}, false
	}
	r.Body = http.MaxBytesReader(w, r.Body, oauthTokenBodyCap)
	if err := r.ParseForm(); err != nil {
		oauthInvalidRequest(w, "the request body is not a valid form of at most 16 KiB")
		return uuid.Nil, oauthsrv.TokenForm{}, false
	}
	form := oauthsrv.ParseTokenForm(r.PostForm)
	if form.Repeated {
		oauthInvalidRequest(w, "a parameter was sent more than once")
		return uuid.Nil, oauthsrv.TokenForm{}, false
	}

	// Client authentication: HTTP Basic only (D2). A second method next to it is invalid_request.
	basicID, basicSecret, hasBasic := oauthBasicCredentials(r)
	if hasBasic && (form.HasClientSecret || (form.ClientID != "" && form.ClientID != basicID)) {
		oauthInvalidRequest(w, "use exactly one client authentication method")
		return uuid.Nil, oauthsrv.TokenForm{}, false
	}
	if !hasBasic {
		oauthInvalidClient(w)
		return uuid.Nil, oauthsrv.TokenForm{}, false
	}
	clientID, err := uuid.Parse(basicID)
	if err != nil {
		oauthInvalidClient(w)
		return uuid.Nil, oauthsrv.TokenForm{}, false
	}
	product, err := h.q.GetProduct(ctx, clientID)
	known := true
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		known = false
	case err != nil:
		slog.Error("oauth "+endpoint+": load client", "error", err)
		oauthUnavailable(w)
		return uuid.Nil, oauthsrv.TokenForm{}, false
	}
	// Always hash and compare in constant time, so an unknown client, a product without a
	// secret and a wrong secret are the same answer at the same cost: the dummy hash stands in
	// for a missing one, and its comparison result never authenticates anyone.
	storedHash, hasSecret := oauthDummySecretHash, false
	if known && len(product.ClientSecretHash) > 0 {
		storedHash, hasSecret = product.ClientSecretHash, true
	}
	secretOK := oauthsrv.SecretMatches(basicSecret, storedHash)
	if !known || !hasSecret || !secretOK || !oauthClientFromProduct(product).IsClient() {
		oauthInvalidClient(w)
		return uuid.Nil, oauthsrv.TokenForm{}, false
	}

	// The client is authenticated: only now does the per-client budget move.
	if !limiter.Allow("client:" + clientID.String()) {
		writeOAuthTokenError(w, &oauthTokenFailure{
			status: http.StatusTooManyRequests, code: oauthsrv.ErrTemporarilyUnavailable,
			retryAfter: max(1, int(limiter.Window().Seconds())),
		}, "")
		return uuid.Nil, oauthsrv.TokenForm{}, false
	}
	return clientID, form, true
}

// OAuthToken is POST /api/oauth/token for grant_type=authorization_code and refresh_token (PRD
// #1910 D5 to D8). The request rules, client authentication and per-client budget are
// oauthAuthenticatedClient's; the grant then runs in one transaction (redeemOAuthCode,
// refreshOAuthToken). A code is redeemed only after every binding check passed.
func (h *Handler) OAuthToken(limiter *mw.Limiter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		clientID, form, ok := h.oauthAuthenticatedClient(w, r, limiter, "token")
		if !ok {
			return
		}
		ctx := r.Context()

		switch form.GrantType {
		case "authorization_code":
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
		case "refresh_token":
			if form.RefreshToken == "" {
				oauthInvalidRequest(w, "refresh_token is required")
				return
			}
			var requested []string
			if form.HasScope {
				var err error
				if requested, err = oauthsrv.ParseScopes(form.Scope); err != nil {
					writeOAuthTokenError(w, &oauthTokenFailure{status: http.StatusBadRequest, code: oauthsrv.ErrInvalidScope}, "scope is malformed or names an unknown scope")
					return
				}
			}
			resp, fail, err := h.refreshOAuthToken(ctx, clientID, form.RefreshToken, requested)
			if err != nil {
				slog.Error("oauth token: refresh", "error", err)
				oauthUnavailable(w)
				return
			}
			if fail != nil {
				writeOAuthTokenError(w, fail, "")
				return
			}
			httpx.JSON(w, http.StatusOK, resp)
		case "":
			oauthInvalidRequest(w, "grant_type is required")
		default:
			writeOAuthTokenError(w, &oauthTokenFailure{status: http.StatusBadRequest, code: oauthsrv.ErrUnsupportedGrantType}, "grant_type must be authorization_code or refresh_token")
		}
	}
}

func (h *Handler) beginOAuthTx(ctx context.Context) (pgx.Tx, error) {
	if h.oauthBeginTx != nil {
		return h.oauthBeginTx(ctx)
	}
	return h.pool.Begin(ctx)
}

// redeemOAuthCode redeems an authorization code in ONE transaction, in D8 lock order (grant, then
// its tokens, then its requests): the code's request is found by its hash without a lock, then the
// grant is locked FOR UPDATE, then the request is re-read under that lock WITHOUT a row lock (the
// grant lock serializes every writer of a grant's requests, and the redeem UPDATE below is itself
// conditional on status). The request row is first locked by that UPDATE, after the access token
// is inserted, and by revokeGrantLocked on the replay arm, after the grant's tokens. Every check
// runs BEFORE the conditional UPDATE that
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
	if len(form.Code) > oauthMaxTokenLen {
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
	row, err := q.GetOAuthAuthorizeRequest(ctx, found.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errOAuthInvalidGrant, nil
	}
	if err != nil {
		return nil, nil, err
	}

	// Bindings: the code belongs to this client, this exact redirect URI and this PKCE verifier,
	// and its grant is the client's own (a belt: the grant is created for the request's product).
	if row.ProductID != clientID || grant.ProductID != row.ProductID || form.RedirectURI == "" || form.RedirectURI != row.RedirectUri ||
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

	// The product and user may have changed since the code was issued (an admin change waiting on
	// approve's product share lock lands after the code exists, and approve checks the user outside
	// its transaction): the product must still be an enabled client registering this URI and
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
	if fail, err := grantMintBlocked(ctx, q, grant.ID); err != nil || fail != nil {
		return nil, fail, err
	}

	// Every check passed: mint, issue the refresh token, redeem, commit. The redeem UPDATE comes
	// last so the request row is locked after the grant and its tokens (D8); a lost race on it
	// rolls the whole transaction back, minting nothing.
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
	if _, err := q.RedeemOAuthAuthorizeRequest(ctx, row.ID); errors.Is(err, pgx.ErrNoRows) {
		return nil, errOAuthInvalidGrant, nil
	} else if err != nil {
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

// grantMintBlocked enforces the two per-grant mint bounds (PRD #1910 D5, Resource bounds) on every
// mint, code exchange and refresh alike. The caller holds the grant's row lock, so no concurrent
// mint of the grant can change either count between the check and the insert:
//   - at most oauthMaxLiveGrantTokens live (unrevoked, unexpired) access tokens;
//   - at most oauthMaxGrantMintsPerWindow minted in the last oauthGrantMintWindow, revoked rows
//     included, which bounds the rows a refresh-then-revoke loop can add.
//
// Either refusal is a 429 temporarily_unavailable with the Retry-After the product should honour.
// A nil failure and nil error means the mint may proceed.
func grantMintBlocked(ctx context.Context, q *store.Queries, grantID uuid.UUID) (*oauthTokenFailure, error) {
	gid := pgtype.UUID{Bytes: grantID, Valid: true}
	live, err := q.CountLiveGrantTokens(ctx, gid)
	if err != nil {
		return nil, err
	}
	if live.Live >= oauthMaxLiveGrantTokens {
		wait := 1
		if live.OldestExpiresAt.Valid {
			wait = max(1, int(time.Until(live.OldestExpiresAt.Time).Seconds())+1)
		}
		return &oauthTokenFailure{status: http.StatusTooManyRequests, code: oauthsrv.ErrTemporarilyUnavailable, retryAfter: wait}, nil
	}
	rate, err := q.CountGrantTokensMintedSince(ctx, store.CountGrantTokensMintedSinceParams{
		WindowSeconds: int64(oauthGrantMintWindow.Seconds()), GrantID: gid,
	})
	if err != nil {
		return nil, err
	}
	if rate.Minted >= oauthMaxGrantMintsPerWindow {
		return &oauthTokenFailure{status: http.StatusTooManyRequests, code: oauthsrv.ErrTemporarilyUnavailable, retryAfter: int(rate.RetryAfterSeconds)}, nil
	}
	return nil, nil
}

// revokeGrantLocked is the one revoke of a grant (PRD #1910 D6): every access token of the grant
// is set revoked (expired or not, so the ListRevokedProductJobs sweep cancels the jobs it created),
// its unredeemed codes are superseded, and the grant itself gets revoked_at with its refresh token
// cleared. It REQUIRES the grant row to be locked FOR UPDATE already by the caller's transaction
// (D8), and takes the rest in the D8 order: the grant's product_tokens rows, then its
// oauth_authorize_requests rows, then the grant row's own UPDATE (already locked). Every revoke of
// a whole grant calls this (the code replay here, the owner's and the admin's revoke of one grant
// token, Revoke all, and RFC 7009 revoke of a refresh token), and none takes a token or request lock
// before its grant's. RFC 7009 revoke of a single access token is the one revoke that does not
// end the grant: it revokes that token alone (revokeAccessToken), still under the grant's lock.
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

// revokeGrantOfTokenTx is the D6 revoke of the grant behind one product token, for the owner's and
// the admin's revoke-by-id routes: inside the caller's transaction it locks the grant FOR UPDATE
// (D8), refuses with pgx.ErrNoRows when the grant is gone, already revoked (a concurrent revoke won,
// so the token is revoked and the id is a 404, like any revoked id) or not the owner's when owner
// is non-nil, then runs revokeGrantLocked.
func revokeGrantOfTokenTx(ctx context.Context, q *store.Queries, grantID uuid.UUID, owner *uuid.UUID) error {
	grant, err := q.LockOAuthGrant(ctx, grantID)
	if err != nil {
		return err
	}
	if grant.RevokedAt.Valid || (owner != nil && grant.UserID != *owner) {
		return pgx.ErrNoRows
	}
	return revokeGrantLocked(ctx, q, grant.ID)
}

// refreshOAuthToken serves grant_type=refresh_token (PRD #1910 D5, D8) in ONE transaction in D8
// lock order. The grant is found by sha256(refresh token) WITHOUT a lock, then locked FOR UPDATE,
// and EVERYTHING is re-checked under that lock before a token is minted: the presented hash must
// still equal the grant's CURRENT refresh hash (so a re-consent or revoke that committed first
// kills it), the grant must be live and the client's own, and the idle (30 days from the last
// refresh, else from issue) and absolute (90 days from consented_at) lifetimes must hold, judged
// on the database clock. Those, and an inactive user, are invalid_grant. The product's own state
// is invalid_client (401 with WWW-Authenticate), as its authentication would have answered: it
// must still be an enabled client, and its allowed scopes must cover what the new token carries.
// A requested scope must be a non-empty subset of the grant's CURRENT scopes (invalid_scope), and
// is what the product must still be allowed, so a product narrowed below the grant can refresh
// with the scopes it still holds. The grant must also pass grantMintBlocked (ten live access
// tokens, thirty mints an hour). Then a one-hour access token is minted with the requested (else
// the grant's) scopes and refresh_last_used_at is stamped. The refresh token is NOT rotated and the
// grant is unchanged, so a retried refresh or several replicas never revoke anything.
//
// requested is the already-parsed scope parameter, nil when absent. A non-nil error is a storage
// failure (503). Every refusal rolls back, so nothing is written on failure.
func (h *Handler) refreshOAuthToken(ctx context.Context, clientID uuid.UUID, refreshToken string, requested []string) (*apitypes.OAuthRefreshResponse, *oauthTokenFailure, error) {
	if len(refreshToken) > oauthMaxTokenLen {
		return nil, errOAuthInvalidGrant, nil
	}
	hash := sha256.Sum256([]byte(refreshToken))

	tx, err := h.beginOAuthTx(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit; every refusal relies on it to write nothing
	q := h.q.WithTx(tx)

	grantID, err := q.GetOAuthGrantIDByRefreshHash(ctx, hash[:])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errOAuthInvalidGrant, nil
	}
	if err != nil {
		return nil, nil, err
	}
	grant, err := q.LockOAuthGrant(ctx, grantID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errOAuthInvalidGrant, nil
	}
	if err != nil {
		return nil, nil, err
	}

	// Re-check under the lock. The first three are the credential itself: a grant that was revoked
	// or re-consented meanwhile no longer holds this hash, and one that belongs to another client
	// is not this client's to refresh (nothing is revoked: the token alone is not proof of being
	// the issuing client).
	if grant.RevokedAt.Valid || len(grant.RefreshTokenHash) == 0 || subtle.ConstantTimeCompare(hash[:], grant.RefreshTokenHash) != 1 || grant.ProductID != clientID {
		return nil, errOAuthInvalidGrant, nil
	}
	// The idle and absolute lifetimes are judged on the database clock, in SQL, so application and
	// database clock skew cannot shift them.
	within, err := q.OAuthGrantRefreshWithinLifetimes(ctx, store.OAuthGrantRefreshWithinLifetimesParams{
		IdleSeconds: int64(oauthRefreshIdleLimit.Seconds()), AbsoluteSeconds: int64(oauthRefreshAbsoluteLimit.Seconds()), ID: grant.ID,
	})
	if err != nil {
		return nil, nil, err
	}
	if !within {
		return nil, errOAuthInvalidGrant, nil
	}

	// The product's own state is the client's registration, not the connection: a product that is
	// no longer an enabled client (disabled or deleted while this request waited for the lock, or
	// its registration cleared) is the 401 invalid_client that its authentication would have got a
	// moment earlier, so it keeps its refresh token and retries after an admin fixes it.
	product, err := q.GetProduct(ctx, grant.ProductID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errOAuthInvalidClient, nil
	}
	if err != nil {
		return nil, nil, err
	}
	if !oauthClientFromProduct(product).IsClient() {
		return nil, errOAuthInvalidClient, nil
	}
	// A deactivated user is invalid_grant (RFC 6749 section 5.2): the grant is intact and
	// reactivation restores it, but a product may discard the connection meanwhile (PRD D7).
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

	// A requested scope may only narrow the grant's CURRENT scopes. What the new token carries
	// (the request, else the whole grant) must still be allowed to the product: a product whose
	// registration was narrowed below it is invalid_client, but one that asks for scopes it still
	// holds is served even though the grant as a whole is no longer covered.
	scopes := grant.Scopes
	if requested != nil {
		for _, sc := range requested {
			if !slices.Contains(grant.Scopes, sc) {
				return nil, &oauthTokenFailure{status: http.StatusBadRequest, code: oauthsrv.ErrInvalidScope}, nil
			}
		}
		scopes = requested
	}
	if !oauthClientAllowsScopes(product, scopes) {
		return nil, errOAuthInvalidClient, nil
	}

	// The mint bounds (D5), under the grant lock, exactly as on a code exchange.
	if fail, err := grantMintBlocked(ctx, q, grant.ID); err != nil || fail != nil {
		return nil, fail, err
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
		Scopes:      scopes,
		ExpiresAt:   pgtype.Timestamptz{Time: time.Now().Add(oauthAccessTokenTTL), Valid: true},
		GrantID:     grant.ID,
	}); errors.Is(err, pgx.ErrNoRows) {
		return nil, errOAuthInvalidClient, nil // the product was disabled or deleted meanwhile
	} else if err != nil {
		return nil, nil, err
	}
	if n, err := q.TouchOAuthGrantRefreshUse(ctx, grant.ID); err != nil {
		return nil, nil, err
	} else if n != 1 {
		return nil, errOAuthInvalidGrant, nil
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, nil, err
	}
	return &apitypes.OAuthRefreshResponse{
		AccessToken: access,
		TokenType:   "Bearer",
		ExpiresIn:   int64(oauthAccessTokenTTL.Seconds()),
		Scope:       strings.Join(scopes, " "),
	}, nil, nil
}
