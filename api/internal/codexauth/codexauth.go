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
// exactly what DiscoverIdentity's zero oauth traffic gives it. Refresh exists now
// only as the raw primitive the m1 instrument test uses to prove the oauth counter
// is observable at all; the COORDINATED refresh orchestration (lease/intent/
// generation) is a later unit and is not built here.
//
// The network is the only thing tests fake: they inject an httpDoer (or an
// http.Client wrapping a fake http.RoundTripper), and the real client code —
// request assembly, header, JSON decode, status handling — runs unchanged.
package codexauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
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
// 2xx but the identity is not fully determined: either user_id or account_id (or
// both) was absent/empty. It is a sentinel so a caller can branch on "the login
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
// provider's user_id; WorkspaceAccountID is its account_id. Both are always
// non-empty on a value returned by DiscoverIdentity.
type Identity struct {
	ProviderUserID     string
	WorkspaceAccountID string
}

// RefreshResult is the outcome of an oauth token refresh. AccessToken is always
// non-empty (Refresh errors otherwise). RefreshToken is a pointer because the
// provider MAY omit a rotated refresh token, and nil ("keep using the old one")
// is a distinct, legal answer from an empty string.
type RefreshResult struct {
	AccessToken  string
	RefreshToken *string
}

// Client is the direct HTTP provider client. It holds the network seam, the two
// configurable base URLs and the oauth client id. The zero value is not usable;
// build one with NewClient.
type Client struct {
	doer      httpDoer
	usageBase string
	oauthBase string
	clientID  string
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

// NewClient builds a Client with the provider defaults, then applies opts. The
// only option the dark M1 layer needs is WithHTTPDoer (tests inject a fake
// transport); the base URLs and client id are the fixed provider defaults. A later
// unit that must point at a non-default endpoint can add the override option then,
// with a caller — an unused exported option would redden the deadcode gate now.
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

// usageResponse is the subset of the usage endpoint's body we read. Both fields
// are optional: the provider may omit either, and an omitted field decodes to the
// empty string, which DiscoverIdentity treats as "identity incomplete".
type usageResponse struct {
	AccountID string `json:"account_id"`
	UserID    string `json:"user_id"`
}

// DiscoverIdentity establishes a login's canonical identity tuple with a single
// NONROTATING GET against the usage surface — it never touches the oauth host, so
// a caller can prove identity was established without spending a refresh (PRD
// #1147 M1). accessToken is presented as a bearer credential.
//
//   - 2xx with BOTH user_id and account_id present → Identity{user_id, account_id}.
//   - 2xx with either absent/empty → ErrIdentityIncomplete (the login authenticated
//     but its account is not yet fully named).
//   - non-2xx (including 401) → *AuthError carrying the status.
func (c *Client) DiscoverIdentity(ctx context.Context, accessToken string) (Identity, error) {
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
	if body.UserID == "" || body.AccountID == "" {
		return Identity{}, ErrIdentityIncomplete
	}
	return Identity{ProviderUserID: body.UserID, WorkspaceAccountID: body.AccountID}, nil
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
// oauth surface. This is the RAW primitive (PRD #1147 M1): it performs exactly one
// oauth call and returns the new material. The coordinated refresh orchestration
// (lease/intent/generation) is a later unit and is not built here.
//
//   - 2xx with an access_token → RefreshResult{AccessToken, RefreshToken}, where
//     RefreshToken is nil when the provider omitted a rotated token.
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
	return RefreshResult{AccessToken: *body.AccessToken, RefreshToken: body.RefreshToken}, nil
}
