// Package codexauth is the API-owned direct HTTP client for a Codex (ChatGPT)
// subscription login (PRD #1147 M1, ships DARK). It talks to two provider
// surfaces the rest of uzi never touches directly:
//
//   - the usage/identity surface (default https://chatgpt.com/backend-api), a
//     NONROTATING read used to establish a login's canonical identity tuple
//     (provider_user_id, workspace_account_id); and
//   - the oauth token surface (default https://auth.openai.com), whose
//     /oauth/token endpoint exchanges a refresh token for a fresh access token.
//
// The identity read and the token refresh are DELIBERATELY separate methods on
// separate hosts. Identity is established WITHOUT rotating anything: a caller
// reconciling a freshly imported login must be able to prove it never spent a
// refresh (M1's whole point is identity-first, no pre-identity rotation), which is
// exactly what DiscoverIdentity's zero oauth traffic gives it. Refresh is the raw
// exchange primitive consumed by workersvc's coordinated lease/intent/generation
// state machine; it also decodes the narrow fresh-token claims used there without
// adding a second provider call.
//
// The network is the only thing tests fake: they inject an httpDoer (or an
// http.Client wrapping a fake http.RoundTripper), and the real client code —
// request assembly, header, JSON decode, status handling — runs unchanged.
package codexauth

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Default provider base URLs. The usage base already includes the /backend-api
// prefix; DiscoverIdentity appends /wham/usage to it. The oauth base is the bare
// host; Refresh appends /oauth/token.
const (
	DefaultUsageBaseURL = "https://chatgpt.com/backend-api"
	DefaultOAuthBaseURL = "https://auth.openai.com"
)

// DefaultClientID is the PUBLIC OAuth client identifier the Codex CLI ships with
// (it appears verbatim in the open-source Codex client). It is a public
// application id, not a secret, and is sent as `client_id` on the refresh
// exchange. It is the fixed provider default; a later unit that must target a
// different deployment can add a construction override (with a caller) then.
const DefaultClientID = "app_EMoamEEZ73f0CkXaXp7hrann"

// maxBodyBytes bounds a provider response body read into memory. Identity and
// token responses are tiny JSON objects; this is a guard against a hostile or
// broken upstream streaming an unbounded body, not a real size limit.
const maxBodyBytes = 1 << 20

// ErrIdentityIncomplete is returned by DiscoverIdentity when the provider replied
// 2xx but the identity is not fully determined. user_id is the sole provider-verified
// anchor: absent/empty user_id is always incomplete. account_id is incomplete only
// when it is absent from BOTH the usage response and the access-token JWT claim
// (a personal ChatGPT seat omits it from the response, so the token claim is the
// fallback source). It is a sentinel so a caller can branch on "the login
// authenticated but we cannot yet name its account" without string-matching.
var ErrIdentityIncomplete = errors.New("codexauth: provider identity incomplete")

// ErrNoAccessToken is returned by Refresh when the provider replied 2xx but omitted
// an access_token — a response that cannot be used, so it is an error rather than a
// RefreshResult with an empty token.
var ErrNoAccessToken = errors.New("codexauth: refresh response carried no access token")

// AuthError is the typed error for a non-2xx provider reply on either surface. It
// carries the operation ("discover_identity" / "refresh") and the HTTP status so a
// caller can distinguish a 401 (the login's token is no longer valid) from a 5xx
// (provider trouble) without parsing a string. Its message never includes the
// response body, which may echo request material.
type AuthError struct {
	Op         string
	StatusCode int
}

func (e *AuthError) Error() string {
	return fmt.Sprintf("codexauth: %s: provider returned status %d", e.Op, e.StatusCode)
}

// Unauthorized reports whether the provider rejected the presented token (401/403)
// — the "re-login" signal, as opposed to a transient provider fault.
func (e *AuthError) Unauthorized() bool {
	return e.StatusCode == http.StatusUnauthorized || e.StatusCode == http.StatusForbidden
}

// httpDoer is the seam over the network. *http.Client satisfies it, and so does a
// test fake; the real client code runs above it either way.
type httpDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// Identity is a Codex login's canonical account tuple. ProviderUserID is the
// provider's user_id; WorkspaceAccountID is its account_id — a workspace account id
// from the usage response, or (for a personal ChatGPT seat, whose usage response
// omits it) the chatgpt_account_id claim carried in the access-token JWT. Both are
// always non-empty on a value returned by DiscoverIdentity.
type Identity struct {
	ProviderUserID     string
	WorkspaceAccountID string
}

// FreshAccessTokenIdentityClaims is the narrow identity subset decoded from a
// freshly exchanged ChatGPT access-token JWT. The two user fields are kept
// separate because a caller must require them to be non-empty and equal before
// comparing that value with the canonical provider user id. The zero value, a
// missing field, or disagreement is unverified.
//
// These are decoded claims, not locally authenticated claims. They are suitable
// for the coordinated-refresh differential check only when the token came
// directly from Refresh's successful exchange against the fixed provider.
type FreshAccessTokenIdentityClaims struct {
	ChatGPTAccountID string
	ChatGPTUserID    string
	AuthUserID       string
}

// RefreshResult is the outcome of an oauth token refresh. AccessToken is always
// non-empty (Refresh errors otherwise). RefreshToken is a pointer because the
// provider MAY omit a rotated refresh token, and nil ("keep using the old one")
// is a distinct, legal answer from an empty string. IdentityClaims contains only
// the claim candidates needed to compare the fresh token with the account's
// canonical tuple; zero or partial claims are deliberately returned as
// unverified rather than turning a successful token exchange into a parse error.
type RefreshResult struct {
	AccessToken    string
	RefreshToken   *string
	IdentityClaims FreshAccessTokenIdentityClaims
}

// Client is the direct HTTP provider client. It holds the network seam, the two
// configurable base URLs and the oauth client id. The zero value is not usable;
// build one with NewClient.
type Client struct {
	doer      httpDoer
	usageBase string
	oauthBase string
	clientID  string
	// perRequestTimeout bounds ONE provider HTTP call (identity read OR token refresh)
	// with a context deadline (PRD #1171 M1). Zero (the default) means "no per-request
	// deadline beyond whatever the doer enforces" — the pre-#1171 dark-M1 behaviour, so
	// existing callers/tests are unchanged. Production sets it (WithPerRequestTimeout) so
	// the whole worker→API→provider→durable-commit→callback round trip stays under the
	// pinned app-server 10-second external-auth callback deadline; see the nested budget
	// documented at the NewClient call site in cmd/server/main.go.
	perRequestTimeout time.Duration
}

// Option configures a Client at construction.
type Option func(*Client)

// WithHTTPDoer injects the network seam (production: an *http.Client with sane
// timeouts; tests: a fake). Nil is ignored, keeping the default 15s-timeout client.
func WithHTTPDoer(d httpDoer) Option {
	return func(c *Client) {
		if d != nil {
			c.doer = d
		}
	}
}

// WithPerRequestTimeout bounds each provider HTTP call (DiscoverIdentity, Refresh) with
// a per-request context deadline (PRD #1171 M1). The coordinated callback uses it for its
// single oauth exchange; import and recovery use it for their nonrotating identity reads.
// A non-positive value is ignored (keeps the default: no per-request deadline), so an
// accidental zero never disables the doer's own timeout.
func WithPerRequestTimeout(d time.Duration) Option {
	return func(c *Client) {
		if d > 0 {
			c.perRequestTimeout = d
		}
	}
}

// NewClient builds a Client with the provider defaults, then applies opts. The base
// URLs, provider name and client id are the FIXED provider defaults — production never
// takes an endpoint from a claim, model, repo, saved label or callback (PRD #1171 M1);
// a localhost/fake provider is injected in tests ONLY through WithHTTPDoer, whose fake
// doer intercepts the request regardless of the (still-default) URL, so no endpoint
// override option is needed or offered. Production additionally passes
// WithPerRequestTimeout to bound each provider call under the app-server callback
// deadline. A later unit that must point at a non-default endpoint can add the override
// option then, with a caller — an unused exported option would redden the deadcode gate.
func NewClient(opts ...Option) *Client {
	c := &Client{
		doer:      &http.Client{Timeout: 15 * time.Second},
		usageBase: DefaultUsageBaseURL,
		oauthBase: DefaultOAuthBaseURL,
		clientID:  DefaultClientID,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// requestContext derives the per-request context for one provider call: when a
// per-request timeout is configured it returns ctx with that deadline (and a cancel to
// release the timer); otherwise it returns ctx unchanged with a no-op cancel, preserving
// the pre-#1171 behaviour for callers that did not set one. The caller MUST defer the
// returned cancel.
func (c *Client) requestContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if c.perRequestTimeout > 0 {
		return context.WithTimeout(ctx, c.perRequestTimeout)
	}
	return ctx, func() {}
}

// usageResponse is the subset of the usage endpoint's body we read. Both fields
// are optional: the provider may omit either, and an omitted field decodes to the
// empty string. An omitted user_id is "identity incomplete"; an omitted account_id
// (a personal ChatGPT seat) triggers DiscoverIdentity's JWT-claim fallback rather
// than being immediately incomplete.
type usageResponse struct {
	AccountID string `json:"account_id"`
	UserID    string `json:"user_id"`
}

// DiscoverIdentity establishes a login's canonical identity tuple with a single
// NONROTATING GET against the usage surface — it never touches the oauth host, so
// a caller can prove identity was established without spending a refresh (PRD
// #1147 M1). accessToken is presented as a bearer credential.
//
//   - 2xx: user_id is required — it is the sole provider-verified anchor and is never
//     derived from the token. account_id comes from the usage response; when the
//     response omits it (a personal ChatGPT seat), it falls back to the token's
//     chatgpt_account_id claim (see accountIDFromAccessToken). With both a user_id and
//     an account_id from either source → Identity{user_id, account_id}.
//   - 2xx with user_id absent, or with no account_id from either source →
//     ErrIdentityIncomplete (the login authenticated but its account is not yet fully named).
//   - non-2xx (including 401) → *AuthError carrying the status.
func (c *Client) DiscoverIdentity(ctx context.Context, accessToken string) (Identity, error) {
	ctx, cancel := c.requestContext(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.usageBase+"/wham/usage", nil)
	if err != nil {
		return Identity{}, fmt.Errorf("codexauth: build identity request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")

	resp, err := c.doer.Do(req)
	if err != nil {
		return Identity{}, fmt.Errorf("codexauth: identity request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort close of a drained response body

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBodyBytes))
		return Identity{}, &AuthError{Op: "discover_identity", StatusCode: resp.StatusCode}
	}

	var body usageResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBodyBytes)).Decode(&body); err != nil {
		return Identity{}, fmt.Errorf("codexauth: decode identity response: %w", err)
	}
	if body.UserID == "" {
		return Identity{}, ErrIdentityIncomplete
	}
	accountID := body.AccountID
	if accountID == "" {
		// Fallback source for a personal ChatGPT seat, whose /wham/usage body omits
		// account_id. The JWT is the caller-supplied bearer token itself, parsed ONLY
		// here and ONLY AFTER /wham/usage returned 2xx for that same bearer — i.e.
		// OpenAI already authenticated the token, so its signed claims are authentic
		// and no local signature verification is needed. This argument also relies on
		// the usage endpoint being the FIXED OpenAI-over-TLS endpoint (see NewClient);
		// a future configurable endpoint, or the ChatGPT-Account-Id header hardening
		// split to #1239, must revisit it. user_id is never derived from the token — it
		// stays the sole provider-verified anchor.
		accountID = accountIDFromAccessToken(accessToken)
	}
	if accountID == "" {
		return Identity{}, ErrIdentityIncomplete
	}
	return Identity{ProviderUserID: body.UserID, WorkspaceAccountID: accountID}, nil
}

type accessTokenIdentityClaims struct {
	Auth struct {
		ChatGPTAccountID string `json:"chatgpt_account_id"`
		ChatGPTUserID    string `json:"chatgpt_user_id"`
		UserID           string `json:"user_id"`
	} `json:"https://api.openai.com/auth"`
}

// ParseFreshAccessTokenIdentityClaims purely decodes the identity claims needed
// to compare a freshly exchanged ChatGPT access token with a stored canonical
// tuple. It parses only the middle JWT segment as unpadded base64url and verifies
// no signature. Any structural failure, including a wrong claim type, returns the
// zero value. Missing or empty claims remain empty so the caller can classify the
// identity as unverified. The two user claim names stay separate: their equality
// is part of the differential check, not an assumption made by the parser.
func ParseFreshAccessTokenIdentityClaims(token string) FreshAccessTokenIdentityClaims {
	claims, ok := decodeAccessTokenIdentityClaims(token)
	if !ok {
		return FreshAccessTokenIdentityClaims{}
	}
	return FreshAccessTokenIdentityClaims{
		ChatGPTAccountID: claims.Auth.ChatGPTAccountID,
		ChatGPTUserID:    claims.Auth.ChatGPTUserID,
		AuthUserID:       claims.Auth.UserID,
	}
}

// accountIDFromAccessToken extracts chatgpt_account_id for DiscoverIdentity's
// personal-seat fallback. It reuses the same JWT decoder as the fresh-token
// differential parser. The lack of local signature verification is sound only
// after the fixed /wham/usage endpoint accepted this same bearer token.
func accountIDFromAccessToken(token string) string {
	return ParseFreshAccessTokenIdentityClaims(token).ChatGPTAccountID
}

func decodeAccessTokenIdentityClaims(token string) (accessTokenIdentityClaims, bool) {
	segments := strings.Split(token, ".")
	if len(segments) != 3 {
		return accessTokenIdentityClaims{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(segments[1])
	if err != nil {
		return accessTokenIdentityClaims{}, false
	}
	var claims accessTokenIdentityClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return accessTokenIdentityClaims{}, false
	}
	return claims, true
}

// refreshRequest is the JSON body of the oauth token exchange.
type refreshRequest struct {
	ClientID     string `json:"client_id"`
	GrantType    string `json:"grant_type"`
	RefreshToken string `json:"refresh_token"`
}

// refreshResponse is the token-exchange reply. All three are optional (*string):
// the provider need not echo an id_token, and MAY omit a rotated refresh_token
// (nil ⇒ keep the presented one). Only access_token is required by Refresh.
type refreshResponse struct {
	IDToken      *string `json:"id_token"`
	AccessToken  *string `json:"access_token"`
	RefreshToken *string `json:"refresh_token"`
}

// Refresh exchanges a refresh token for a fresh access token via a POST to the
// oauth surface. This raw primitive performs exactly one provider call, returns
// the new material, and purely decodes the narrow identity claims from that same
// access token for workersvc's coordinated differential check.
//
//   - 2xx with an access_token → RefreshResult with the token pair and purely decoded
//     identity claims; RefreshToken is nil when the provider omitted a rotated token.
//   - 2xx without an access_token → ErrNoAccessToken.
//   - non-2xx (including 401) → *AuthError carrying the status.
func (c *Client) Refresh(ctx context.Context, refreshToken string) (RefreshResult, error) {
	payload, err := json.Marshal(refreshRequest{ //nolint:gosec // G117: the refresh request must carry the refresh token to the provider token endpoint
		ClientID:     c.clientID,
		GrantType:    "refresh_token",
		RefreshToken: refreshToken,
	})
	if err != nil {
		return RefreshResult{}, fmt.Errorf("codexauth: encode refresh request: %w", err)
	}
	ctx, cancel := c.requestContext(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.oauthBase+"/oauth/token", bytes.NewReader(payload))
	if err != nil {
		return RefreshResult{}, fmt.Errorf("codexauth: build refresh request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.doer.Do(req)
	if err != nil {
		return RefreshResult{}, fmt.Errorf("codexauth: refresh request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort close of a drained response body

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBodyBytes))
		return RefreshResult{}, &AuthError{Op: "refresh", StatusCode: resp.StatusCode}
	}

	var body refreshResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBodyBytes)).Decode(&body); err != nil {
		return RefreshResult{}, fmt.Errorf("codexauth: decode refresh response: %w", err)
	}
	if body.AccessToken == nil || *body.AccessToken == "" {
		return RefreshResult{}, ErrNoAccessToken
	}
	return RefreshResult{
		AccessToken:    *body.AccessToken,
		RefreshToken:   body.RefreshToken,
		IdentityClaims: ParseFreshAccessTokenIdentityClaims(*body.AccessToken),
	}, nil
}
