package handler

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/oauthsrv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// The consent half of uzi's OAuth authorization server (PRD #1910 M2, D3 and D4):
//
//	GET  /api/oauth/authorize               unauthenticated; validates, stores a pending request, 302s to /connect
//	GET  /api/oauth/requests/{id}           consent metadata for the SPA
//	POST /api/oauth/requests/{id}/approve   single-use claim, grant create / re-consent, code issuance
//	POST /api/oauth/requests/{id}/deny      single-use deny
//
// The three /requests routes require the cookie session AND the browser-binding cookie that
// authorize set, so a /connect link opened in another browser can neither read nor decide the
// request (consent fixation, PRD #1910 D3). Every handler in this file sets Cache-Control:
// no-store before anything else, so every response it writes (redirect, error page, JSON, error
// JSON) carries it. A 401, 403 or 429 written by the middleware in front of a route (the rate
// limiter, the session check, the CSRF check) is not from this file and carries no such header.

const (
	// oauthBindCookieName / oauthBindCookiePath: the browser-binding cookie, HttpOnly, SameSite=Lax
	// (a Strict cookie is not sent on the cross-site navigation that starts the flow), Secure per
	// cfg.CookieSecure. The OIDC state cookie (oidc.go) is the precedent. Path=/api/oauth keeps it
	// off every other request.
	oauthBindCookieName = "uzi_oauth_bind"
	oauthBindCookiePath = "/api/oauth"
	// oauthBindCookieTTL is the cookie's lifetime, refreshed by every authorize. It outlives a
	// request (oauthAuthorizeTTL) so a user who authorizes twice from one browser keeps both
	// requests decidable.
	oauthBindCookieTTL = 30 * time.Minute
	// oauthBindNonceBytes is the binding nonce's size (256 bits); the cookie carries it as 43
	// base64url characters.
	oauthBindNonceBytes = 32
	// oauthAuthorizeTTL is how long a pending request lives (PRD #1910 D3), as the CLI login's.
	oauthAuthorizeTTL = 5 * time.Minute
	// oauthCodeTTL is an authorization code's lifetime (PRD #1910 D4).
	oauthCodeTTL = 60 * time.Second
	// oauthCodeBytes is an authorization code's entropy (256 bits).
	oauthCodeBytes = 32
	// oauthGrantLockAttempts bounds the insert-or-lock loop in approve: a conflict whose live
	// grant was revoked before it could be locked is retried, never spun on.
	oauthGrantLockAttempts = 3
)

// The live (pending, unexpired) authorize requests GET /api/oauth/authorize will store are
// bounded by hierarchical per-source tiers plus two storage backstops. Per product, a source
// is bucketed (oauthSourceBucketsFor) and each tier is checked:
//
//	IPv6: per /64 <= oauthPendingPerSourceCap (20), per /56 <= oauthPendingV6MidCap (40),
//	      per /48 <= oauthPendingV6WideCap (100)
//	IPv4: per address <= 20, per /24 <= oauthPendingV4MidCap (100)
//
// A 6to4 (2002::/16) address is bucketed as the IPv4 address it embeds, so one IPv4 host cannot
// mint 65536 /64 buckets; an IPv4-mapped IPv6 address is its IPv4. THESE are the fairness
// bound: authLimiter keys on the full address, so without them one source could fill a
// product's table and lock every real user of that product out. A request lives 5 minutes and a
// human decides each, so a source needs a handful at once, never 20.
//
//   - oauthPendingPerProductCap (5000) per product and oauthPendingGlobalCap (50000) overall are
//     STORAGE BACKSTOPS only (a row is under 3 KB), far above any honest load.
//
// Residual (PRD #1910 Decision Log): the tiers cap what a source can hold at 100 per /48 (IPv6)
// or /24 (IPv4), so exhausting one product's 5000 backstop takes at least 50 IPv6 /48s or 50
// IPv4 /24s (5000/100). The tiers are per product, so one network holds up to 100 rows in EACH
// client product. The 50000 global backstop is unreachable with fewer than 10 client products
// (P x 5000 < 50000); with P >= 10 it takes about 500/P networks (50 /48s at P = 10), and then
// an attacker with that many networks can lock every product's users out. Teredo (2001::/32)
// addresses are not mapped to their embedded IPv4: their /48 comes from the Teredo server
// field, so where the api can reach a Teredo relay that accepts an arbitrary server, one IPv4
// host can mint many /48 buckets. Also, behind a reverse proxy with
// TRUSTED_PROXIES unset (the compose default) every browser shares the proxy's one bucket, so
// 20 pending requests per product is all the instance's users can hold at once (the
// shared-bucket trade-off of docs/auth-design.md).
//
// The per-source tiers and the per-product backstop are atomic: the final count and the insert
// run under a per-product advisory lock. The global backstop is counted under the product's lock
// only, so concurrent authorizes of different products can overshoot it by the requests in
// flight, which a backstop tolerates.
//
// They are variables only so a LiveDB test can lower them; nothing else assigns them.
var (
	oauthPendingPerSourceCap  = 20
	oauthPendingV6MidCap      = 40
	oauthPendingV6WideCap     = 100
	oauthPendingV4MidCap      = 100
	oauthPendingPerProductCap = 5000
	oauthPendingGlobalCap     = 50000
	// oauthAuthorizeLockTimeout bounds the wait for the per-product lock. A request that cannot
	// get it answers temporarily_unavailable; refused-by-count requests never wait at all.
	oauthAuthorizeLockTimeout = 2 * time.Second
)

// oauthSourceBuckets is the fairness buckets of one client address and the cap of each tier.
type oauthSourceBuckets struct {
	Fine, Mid, Wide          string
	FineCap, MidCap, WideCap int
}

// oauthSourceBucketsFor buckets a client address (see the cap comment above for the tiers). An
// unparsable address shares the empty bucket at every tier with the finest cap, which only ever
// makes the cap stricter. The input is mw.ClientIP's answer, which honours X-Forwarded-For only
// from TRUSTED_PROXIES.
func oauthSourceBucketsFor(clientIP string) oauthSourceBuckets {
	addr, err := netip.ParseAddr(clientIP)
	if err != nil {
		return oauthSourceBuckets{FineCap: oauthPendingPerSourceCap, MidCap: oauthPendingPerSourceCap, WideCap: oauthPendingPerSourceCap}
	}
	addr = addr.WithZone("").Unmap()
	if addr.Is6() {
		if b := addr.As16(); b[0] == 0x20 && b[1] == 0x02 {
			// 6to4: bits 16..47 are the embedded IPv4 address.
			addr = netip.AddrFrom4([4]byte{b[2], b[3], b[4], b[5]})
		}
	}
	if addr.Is4() {
		c24 := netip.PrefixFrom(addr, 24).Masked().String()
		return oauthSourceBuckets{
			Fine: addr.String(), Mid: c24, Wide: c24,
			FineCap: oauthPendingPerSourceCap, MidCap: oauthPendingV4MidCap, WideCap: oauthPendingV4MidCap,
		}
	}
	return oauthSourceBuckets{
		Fine:    netip.PrefixFrom(addr, 64).Masked().String(),
		Mid:     netip.PrefixFrom(addr, 56).Masked().String(),
		Wide:    netip.PrefixFrom(addr, 48).Masked().String(),
		FineCap: oauthPendingPerSourceCap, MidCap: oauthPendingV6MidCap, WideCap: oauthPendingV6WideCap,
	}
}

// oauthPendingOverCap counts the product's live pending requests (each tier up to its cap) and
// reports whether any tier or backstop is already at or over its cap.
func oauthPendingOverCap(ctx context.Context, q *store.Queries, productID uuid.UUID, b oauthSourceBuckets) (bool, error) {
	n, err := q.CountLivePendingOAuthRequests(ctx, store.CountLivePendingOAuthRequestsParams{
		ForProduct:   productID,
		ForPrefix:    b.Fine,
		ForMid:       b.Mid,
		ForWide:      b.Wide,
		PrefixLimit:  int32(b.FineCap),                 //nolint:gosec // G115: a small constant (or a test's lower value)
		MidLimit:     int32(b.MidCap),                  //nolint:gosec // G115: a small constant (or a test's lower value)
		WideLimit:    int32(b.WideCap),                 //nolint:gosec // G115: a small constant (or a test's lower value)
		ProductLimit: int32(oauthPendingPerProductCap), //nolint:gosec // G115: a small constant (or a test's lower value)
		GlobalLimit:  int32(oauthPendingGlobalCap),     //nolint:gosec // G115: a small constant (or a test's lower value)
	})
	if err != nil {
		return false, err
	}
	return n.PrefixPending >= int64(b.FineCap) || n.MidPending >= int64(b.MidCap) || n.WidePending >= int64(b.WideCap) ||
		n.ProductPending >= int64(oauthPendingPerProductCap) || n.GlobalPending >= int64(oauthPendingGlobalCap), nil
}

// isLockTimeout reports whether err is Postgres's lock_not_available (SQLSTATE 55P03), what a
// lock_timeout expiry raises.
func isLockTimeout(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "55P03"
}

// oauthStaticErrorPage is the fixed page for a client_id / redirect_uri that cannot be trusted
// (RFC 6749 section 4.1.2.1). It is a constant: it echoes no parameter, and the response carries
// no Location header, so it can never become an open redirect or a reflected-content sink.
const oauthStaticErrorPage = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Cannot connect</title></head>
<body><h1>Cannot connect</h1><p>This authorization request is not valid. Go back to the application you came from and start the connection again. If it keeps failing, ask the administrator of this uzi instance to check the application's registration.</p></body></html>
`

// oauthIssuer is the RFC 9207 iss value on every code and error redirect: the instance's
// public origin.
func (h *Handler) oauthIssuer() string {
	return strings.TrimRight(h.cfg.FrontendOrigin, "/")
}

func oauthStaticError(w http.ResponseWriter, status int) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(oauthStaticErrorPage))
}

// oauthRedirect writes a 302 with no body. http.Redirect would write an HTML body echoing the
// target, which is needless here.
func oauthRedirect(w http.ResponseWriter, target string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Location", target)
	w.WriteHeader(http.StatusFound)
}

func oauthClientFromProduct(p store.Product) *oauthsrv.Client {
	return &oauthsrv.Client{
		Active:       p.Enabled && !p.DeletedAt.Valid,
		RedirectURIs: p.RedirectUris,
		Scopes:       p.OauthScopes,
		HasSecret:    len(p.ClientSecretHash) > 0,
	}
}

// oauthBindingNonce returns the browser's binding nonce: the one already in its cookie when that
// is well-formed, else a fresh random one. Reusing the cookie lets two tabs of one browser each
// start and decide a request (every pending row stores the hash of the same nonce); a malformed
// or absent cookie is replaced.
func oauthBindingNonce(r *http.Request) (string, error) {
	if c, cerr := r.Cookie(oauthBindCookieName); cerr == nil && wellFormedOAuthNonce(c.Value) {
		return c.Value, nil
	}
	buf := make([]byte, oauthBindNonceBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func wellFormedOAuthNonce(v string) bool {
	if len(v) != base64.RawURLEncoding.EncodedLen(oauthBindNonceBytes) {
		return false
	}
	b, err := base64.RawURLEncoding.DecodeString(v)
	return err == nil && len(b) == oauthBindNonceBytes
}

func oauthBindingHash(nonce string) []byte {
	sum := sha256.Sum256([]byte(nonce))
	return sum[:]
}

func (h *Handler) setOAuthBindCookie(w http.ResponseWriter, nonce string) {
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: HttpOnly and SameSite=Lax are set; Secure follows cfg.CookieSecure (false only on an http dev origin), exactly as the OIDC state cookie.
		Name:     oauthBindCookieName,
		Value:    nonce,
		Path:     oauthBindCookiePath,
		MaxAge:   int(oauthBindCookieTTL.Seconds()),
		Expires:  time.Now().Add(oauthBindCookieTTL),
		HttpOnly: true,
		Secure:   h.cfg.CookieSecure,
		SameSite: http.SameSiteLaxMode,
	})
}

// OAuthAuthorize is GET /api/oauth/authorize (PRD #1910 D3). The order is the protocol's:
//
//  1. client_id and redirect_uri FIRST. An unknown, disabled or deleted product, a product that
//     is not a client, a repeated parameter or a redirect_uri that is not an exact match shows
//     the fixed static error page: no redirect, no echoed parameter.
//  2. Everything else (response_type, PKCE, state, scope) after that; a failure redirects to the
//     verified redirect URI with error, the client's state and iss.
//  3. Only then is anything stored: the pending request (5 minutes) with the sha256 of the
//     browser-binding nonce, then a 302 to the SPA's /connect page.
func (h *Handler) OAuthAuthorize(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	ctx := r.Context()
	// url.ParseQuery, not r.URL.Query(): the latter silently drops a malformed pair, which would
	// let a mangled parameter vanish instead of being refused.
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		oauthStaticError(w, http.StatusBadRequest)
		return
	}
	q := oauthsrv.ParseAuthorizeQuery(values)
	clientID, err := uuid.Parse(q.ClientID)
	if err != nil || q.Repeated("client_id") || q.Repeated("redirect_uri") {
		oauthStaticError(w, http.StatusBadRequest)
		return
	}
	product, err := h.q.GetProduct(ctx, clientID)
	var client *oauthsrv.Client
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		slog.Error("oauth authorize: load product", "error", err)
		oauthStaticError(w, http.StatusInternalServerError)
		return
	default:
		client = oauthClientFromProduct(product)
	}
	if err := oauthsrv.CheckClient(client, q); err != nil {
		oauthStaticError(w, http.StatusBadRequest)
		return
	}
	req, verr := oauthsrv.ValidateAuthorize(client, q)
	if verr != nil {
		oauthRedirect(w, oauthsrv.ErrorRedirectURL(q.RedirectURI, verr, h.oauthIssuer()))
		return
	}

	nonce, err := oauthBindingNonce(r)
	if err != nil {
		slog.Error("oauth authorize: binding nonce", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	// Opportunistic sweep, as CLIAuthStart does (best effort: a failure must not fail the flow).
	if _, err := h.q.DeleteExpiredOAuthAuthorizeRequests(ctx); err != nil {
		slog.Warn("oauth authorize: sweep expired requests", "error", err)
	}
	// Cap and insert. Over a cap the answer is temporarily_unavailable (RFC 6749 section
	// 4.1.2.1) on the already verified redirect URI and nothing is stored. A lock-free pre-count
	// refuses an over-cap request without touching the per-product lock, so a flood that is
	// already refused cannot queue on it and occupy the pool. Only a request that passes it opens
	// a transaction, takes the lock with a bounded wait (a timeout is the same refusal),
	// recounts (the pre-count was only a snapshot) and inserts: the final count and the insert
	// are one step, so concurrent requests cannot each pass a check that was true when they
	// looked.
	buckets := oauthSourceBucketsFor(mw.ClientIP(r, h.cfg.TrustedProxies))
	over, err := oauthPendingOverCap(ctx, h.q, product.ID, buckets)
	if err != nil {
		slog.Error("oauth authorize: count pending requests", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	var row store.OauthAuthorizeRequest
	refused := over
	if !refused {
		err = h.inTx(ctx, func(q *store.Queries) error {
			if err := q.SetOAuthAuthorizeLockTimeout(ctx, strconv.FormatInt(oauthAuthorizeLockTimeout.Milliseconds(), 10)+"ms"); err != nil {
				return err
			}
			if err := q.LockOAuthAuthorize(ctx, product.ID); err != nil {
				return err
			}
			over, err := oauthPendingOverCap(ctx, q, product.ID, buckets)
			if err != nil {
				return err
			}
			if over {
				refused = true
				return nil
			}
			row, err = q.CreateOAuthAuthorizeRequest(ctx, store.CreateOAuthAuthorizeRequestParams{
				ProductID:     product.ID,
				RedirectUri:   req.RedirectURI,
				Scopes:        req.Scopes,
				State:         req.State,
				CodeChallenge: req.CodeChallenge,
				BindingHash:   oauthBindingHash(nonce),
				SourcePrefix:  buckets.Fine,
				SourceMid:     buckets.Mid,
				SourceWide:    buckets.Wide,
				ExpiresAt:     pgtype.Timestamptz{Time: time.Now().Add(oauthAuthorizeTTL), Valid: true},
			})
			return err
		})
		if isLockTimeout(err) {
			slog.Warn("oauth authorize: per-product lock wait timed out", "product_id", product.ID)
			refused, err = true, nil
		}
		if err != nil {
			slog.Error("oauth authorize: store request", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
	}
	if refused {
		oauthRedirect(w, oauthsrv.ErrorRedirectURL(req.RedirectURI, &oauthsrv.Error{
			Code:        oauthsrv.ErrTemporarilyUnavailable,
			Description: "too many authorization requests are pending; try again shortly",
			State:       req.State,
		}, h.oauthIssuer()))
		return
	}
	h.setOAuthBindCookie(w, nonce)
	oauthRedirect(w, "/connect?request="+row.ID.String())
}

// loadOAuthRequest resolves the {id} request for the three /requests routes and enforces the
// browser binding. Every failure (bad id, unknown, expired, no binding cookie, a cookie whose
// sha256 does not match the row's binding_hash) is the SAME 404, so a caller cannot tell a
// request from another browser from one that does not exist. On failure it has answered and
// returns ok=false.
func (h *Handler) loadOAuthRequest(w http.ResponseWriter, r *http.Request) (store.OauthAuthorizeRequest, bool) {
	w.Header().Set("Cache-Control", "no-store")
	notFound := func() (store.OauthAuthorizeRequest, bool) {
		httpx.Error(w, http.StatusNotFound, "request not found")
		return store.OauthAuthorizeRequest{}, false
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		return notFound()
	}
	row, err := h.q.GetOAuthAuthorizeRequest(r.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		return notFound()
	}
	if err != nil {
		slog.Error("oauth request: load", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return store.OauthAuthorizeRequest{}, false
	}
	c, cerr := r.Cookie(oauthBindCookieName)
	if cerr != nil || !wellFormedOAuthNonce(c.Value) ||
		subtle.ConstantTimeCompare(oauthBindingHash(c.Value), row.BindingHash) != 1 ||
		!row.ExpiresAt.Time.After(time.Now()) {
		return notFound()
	}
	return row, true
}

// OAuthGetRequest is GET /api/oauth/requests/{id}: the consent screen's metadata. It exposes the
// product's name and description (admin-written plain strings), the redirect HOST, the requested
// scopes, the request status and its expiry; never the code challenge, the redirect URI list or
// any hash.
func (h *Handler) OAuthGetRequest(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	row, ok := h.loadOAuthRequest(w, r)
	if !ok {
		return
	}
	product, err := h.q.GetProduct(r.Context(), row.ProductID)
	if err != nil {
		slog.Error("oauth request: load product", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	host := ""
	if u, err := url.Parse(row.RedirectUri); err == nil {
		host = u.Host
	}
	httpx.JSON(w, http.StatusOK, apitypes.OAuthAuthorizeRequestDTO{
		ProductName:        product.Name,
		ProductDescription: product.Description,
		RedirectHost:       host,
		Scopes:             append([]string{}, row.Scopes...),
		Status:             row.Status,
		ExpiresAt:          row.ExpiresAt.Time,
	})
}

// OAuthDeny is POST /api/oauth/requests/{id}/deny: pending -> denied in one conditional UPDATE,
// answering the redirect carrying error=access_denied, the client's state and iss. A request that
// is no longer pending (a second deny, a deny after approve) is a 409, never a false success. A
// request whose product stopped being a client, was disabled or no longer registers the redirect
// URI is marked denied and answers the same 409 as approve, with no redirect to the stale URI.
func (h *Handler) OAuthDeny(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	row, ok := h.loadOAuthRequest(w, r)
	if !ok {
		return
	}
	product, err := h.q.GetProduct(r.Context(), row.ProductID)
	if err != nil {
		slog.Error("oauth deny: load product", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	// The same validity re-check approve makes: a redirect URI the product no longer registers (or a
	// product that stopped being an enabled client) must not be redirected to, even with
	// error=access_denied. The request is still marked denied so it cannot be approved later.
	stillValid := oauthRequestStillValid(product, row)
	_, err = h.q.DenyOAuthAuthorizeRequest(r.Context(), store.DenyOAuthAuthorizeRequestParams{
		ID:     row.ID,
		UserID: pgtype.UUID{Bytes: user.ID, Valid: true},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.Error(w, http.StatusConflict, "request is no longer pending")
		return
	}
	if err != nil {
		slog.Error("oauth deny", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	if !stillValid {
		httpx.Error(w, http.StatusConflict, "this application can no longer be connected; start again from the application")
		return
	}
	httpx.JSON(w, http.StatusOK, apitypes.OAuthRedirectResponse{
		RedirectURL: oauthsrv.DeniedRedirectURL(row.RedirectUri, row.State, h.oauthIssuer()),
	})
}

// OAuthApprove is POST /api/oauth/requests/{id}/approve (PRD #1910 D4, D6, D8). One transaction:
//
//  1. the grant, lock first (D8): INSERT ... ON CONFLICT DO NOTHING; on a conflict, in a SEPARATE
//     statement, SELECT the live grant FOR UPDATE (an ON CONFLICT DO NOTHING can meet a row its
//     own statement snapshot cannot see), retrying if it was revoked meanwhile;
//  2. for an existing live grant (re-consent): the scopes are replaced with the approved set,
//     consented_at advances and the refresh token is cleared; the grant's access tokens are
//     revoked ONLY if the new set drops a scope that ANY unrevoked token of the grant holds,
//     expired or not (an expired jobs:run token may have created a still-running job), so
//     reconnecting with the same or wider scopes never cancels a running job;
//  3. the grant's earlier unredeemed codes are superseded;
//  4. the single-use claim: a conditional UPDATE pending -> approved ... RETURNING, so a second
//     approve, or an approve after deny, finds no row (409); then this request gets its own code
//     (256-bit random, sha256 at rest, 60 seconds, bound to the grant).
//
// Lock order (D8) is grant, then its product_tokens, then request rows: the request this approve
// decides is claimed LAST, after the grant lock, so no step holds a request row while waiting for
// a grant. A claim that loses (409) rolls the grant work back with the transaction.
//
// Any failure rolls the whole transaction back, so the request stays pending. The response is the
// server-built redirect: the registered URI plus code, the client's state and iss.
func (h *Handler) OAuthApprove(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	ctx := r.Context()
	user, ok := mw.UserFromContext(ctx)
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	row, ok := h.loadOAuthRequest(w, r)
	if !ok {
		return
	}
	if row.Status != "pending" {
		httpx.Error(w, http.StatusConflict, "request is no longer pending")
		return
	}
	product, err := h.q.GetProduct(ctx, row.ProductID)
	if err != nil {
		slog.Error("oauth approve: load product", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	// The product may have changed since authorize: re-check it is still a client with this
	// redirect URI and these scopes before granting anything.
	if !oauthRequestStillValid(product, row) {
		httpx.Error(w, http.StatusConflict, "this application can no longer be connected; start again from the application")
		return
	}

	codeBuf := make([]byte, oauthCodeBytes)
	if _, err := rand.Read(codeBuf); err != nil {
		slog.Error("oauth approve: generate code", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	code := base64.RawURLEncoding.EncodeToString(codeBuf)
	codeHash := sha256.Sum256([]byte(code))

	tx, err := h.pool.Begin(ctx)
	if err != nil {
		slog.Error("oauth approve: begin tx", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit; every early return relies on it to undo the grant work and the claim
	qtx := h.q.WithTx(tx)
	userID := pgtype.UUID{Bytes: user.ID, Valid: true}

	grant, existed, err := lockOrCreateOAuthGrant(ctx, qtx, user.ID, product.ID, row.Scopes)
	if err != nil {
		slog.Error("oauth approve: grant", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	if existed {
		drops, err := qtx.OAuthGrantHasTokenOutsideScopes(ctx, store.OAuthGrantHasTokenOutsideScopesParams{GrantID: pgtype.UUID{Bytes: grant.ID, Valid: true}, Scopes: row.Scopes})
		if err != nil {
			slog.Error("oauth approve: scope narrowing check", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
		if _, err := qtx.ReconsentOAuthGrant(ctx, store.ReconsentOAuthGrantParams{ID: grant.ID, Scopes: row.Scopes}); err != nil {
			slog.Error("oauth approve: re-consent", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
		if drops {
			if _, err := qtx.RevokeOAuthGrantProductTokens(ctx, pgtype.UUID{Bytes: grant.ID, Valid: true}); err != nil {
				slog.Error("oauth approve: revoke narrowed tokens", "error", err)
				httpx.Error(w, http.StatusInternalServerError, "internal error")
				return
			}
		}
	}
	if _, err := qtx.SupersedeOAuthGrantCodes(ctx, pgtype.UUID{Bytes: grant.ID, Valid: true}); err != nil {
		slog.Error("oauth approve: supersede earlier codes", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	if _, err := qtx.ClaimOAuthAuthorizeRequest(ctx, store.ClaimOAuthAuthorizeRequestParams{ID: row.ID, UserID: userID}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.Error(w, http.StatusConflict, "request is no longer pending")
			return
		}
		slog.Error("oauth approve: claim", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	if _, err := qtx.IssueOAuthAuthorizationCode(ctx, store.IssueOAuthAuthorizationCodeParams{
		ID:            row.ID,
		UserID:        userID,
		GrantID:       pgtype.UUID{Bytes: grant.ID, Valid: true},
		CodeHash:      codeHash[:],
		CodeExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(oauthCodeTTL), Valid: true},
	}); err != nil {
		slog.Error("oauth approve: issue code", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		slog.Error("oauth approve: commit", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, apitypes.OAuthRedirectResponse{
		RedirectURL: oauthsrv.CodeRedirectURL(row.RedirectUri, code, row.State, h.oauthIssuer()),
	})
}

// oauthRequestStillValid reports whether the product can still run this stored request: a
// client, with the request's redirect URI still registered and its scopes still allowed.
func oauthRequestStillValid(p store.Product, row store.OauthAuthorizeRequest) bool {
	c := oauthClientFromProduct(p)
	if !c.IsClient() {
		return false
	}
	registered := false
	for _, u := range c.RedirectURIs {
		if u == row.RedirectUri {
			registered = true
			break
		}
	}
	if !registered {
		return false
	}
	for _, sc := range row.Scopes {
		allowed := false
		for _, a := range c.Scopes {
			if a == sc {
				allowed = true
				break
			}
		}
		if !allowed {
			return false
		}
	}
	return true
}

var errOAuthGrantContention = errors.New("oauth grant kept changing under the approve")

// lockOrCreateOAuthGrant implements PRD #1910 D8's first step: insert the live grant
// conflict-safely; on a conflict lock the live grant FOR UPDATE in a separate statement, and
// retry (bounded) if it was revoked in between. existed reports that the returned grant was
// already live, so the caller applies the re-consent rules. A fresh grant is created with the
// approved scopes and consented_at now, which is also the table default.
func lockOrCreateOAuthGrant(ctx context.Context, q *store.Queries, userID, productID uuid.UUID, scopes []string) (g store.OauthGrant, existed bool, err error) {
	for attempt := 0; attempt < oauthGrantLockAttempts; attempt++ {
		g, err = q.InsertOAuthGrant(ctx, store.InsertOAuthGrantParams{UserID: userID, ProductID: productID, Scopes: scopes})
		if err == nil {
			return g, false, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return store.OauthGrant{}, false, err
		}
		g, err = q.LockLiveOAuthGrant(ctx, store.LockLiveOAuthGrantParams{UserID: userID, ProductID: productID})
		if err == nil {
			return g, true, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return store.OauthGrant{}, false, err
		}
	}
	return store.OauthGrant{}, false, errOAuthGrantContention
}
