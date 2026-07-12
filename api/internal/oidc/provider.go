// Package oidc wraps github.com/coreos/go-oidc to give uzi a small, explicit OIDC
// relying-party surface (PRD #45): lazy cached provider discovery, an
// authorization-URL builder (Authorization Code + PKCE S256), and code exchange
// with ID-token verification. go-oidc validates the issuer, audience, and
// signature (via JWKS); the two checks it does NOT do automatically — nonce
// equality and a strict boolean email_verified — are done here explicitly (audit
// L2). Every outbound call (discovery, JWKS, token) rides an http.Client with an
// explicit timeout, mirroring the forge/Slack client posture (audit L3).
package oidc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
	"golang.org/x/sync/singleflight"
)

// Config is the relying-party configuration. Every field is required; the caller
// (config.Load) validates the issuer scheme and the all-or-nothing pairing before
// constructing a Provider.
type Config struct {
	IssuerURL    string
	ClientID     string
	ClientSecret string
	RedirectURL  string
	Scopes       []string
	HTTPTimeout  time.Duration
}

// Identity is the minimal, verified claim set the callback maps to a uzi user
// (Decision 10). EmailVerified is true only when the claim was a JSON boolean
// true (audit L2).
type Identity struct {
	Issuer        string
	Subject       string
	Email         string
	EmailVerified bool
	Name          string
}

// Sentinel errors let the callback pick an enumerated redirect code without
// matching on message strings.
var (
	// ErrNoIDToken means the token response omitted the id_token field.
	ErrNoIDToken = errors.New("oidc: token response has no id_token")
	// ErrNonceMismatch means the ID token's nonce claim did not equal the value
	// sealed in the state cookie (replay / CSRF guard).
	ErrNonceMismatch = errors.New("oidc: id_token nonce does not match")
)

// Provider is a lazily-discovered OIDC relying party. Safe for concurrent use.
// Discovery runs at most once on success and is retried on every call until it
// succeeds, so a boot-time IdP outage leaves the provider degraded rather than
// crash-looping the API (Decision 8).
type Provider struct {
	cfg    Config
	client *http.Client

	// sf collapses concurrent first-time discovery attempts onto a single in-flight
	// call, so an IdP outage does not serialize every pending login behind the mutex
	// (auditor low). The RWMutex guards the cached results below.
	sf singleflight.Group

	mu       sync.RWMutex
	provider *gooidc.Provider
	verifier *gooidc.IDTokenVerifier
	oauth    *oauth2.Config
}

// GenerateVerifier returns a fresh high-entropy PKCE code verifier (RFC 7636),
// suitable to pass to AuthCodeURL (as the S256 challenge source) and later to
// Exchange (as the plaintext verifier). Thin re-export so callers do not import
// golang.org/x/oauth2 directly.
func GenerateVerifier() string { return oauth2.GenerateVerifier() }

// New constructs a Provider. It does no network I/O; discovery is deferred to the
// first Discover/AuthCodeURL/Exchange call.
func New(cfg Config) *Provider {
	return &Provider{
		cfg:    cfg,
		client: &http.Client{Timeout: cfg.HTTPTimeout},
	}
}

// Discover performs provider discovery against the issuer's
// /.well-known/openid-configuration, or re-uses a previous success. It is safe to
// call repeatedly: the boot warm-up calls it once to surface a misconfigured or
// unreachable IdP loudly, and a failure is not cached, so a later login retries.
func (p *Provider) Discover(ctx context.Context) error {
	return p.ensure(ctx)
}

// Discovered reports whether provider discovery has succeeded and is cached. It is
// non-blocking (never networks), so the admin settings status line can distinguish
// "ok" from "configured-but-degraded" without hanging on a down IdP (review Nit6).
// A false result clears itself on the next successful login-triggered discovery.
func (p *Provider) Discovered() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.provider != nil
}

// ensure runs discovery at most once on success and caches the derived verifier +
// oauth2 config. Concurrent callers collapse onto one in-flight discovery via
// singleflight, so a slow/unreachable IdP does not serialize every pending login
// behind the mutex. A failure is not cached, so a later call retries.
func (p *Provider) ensure(ctx context.Context) error {
	p.mu.RLock()
	ready := p.provider != nil
	p.mu.RUnlock()
	if ready {
		return nil
	}
	_, err, _ := p.sf.Do("discover", func() (any, error) {
		// A concurrent leader may have finished while we waited for the group.
		p.mu.RLock()
		done := p.provider != nil
		p.mu.RUnlock()
		if done {
			return nil, nil
		}
		// Detach from any single caller's request lifecycle — this is a shared cached
		// resource, and the sharers of this singleflight call may outlive the leader's
		// request. The http.Client timeout bounds the discovery HTTP call.
		dctx := gooidc.ClientContext(context.WithoutCancel(ctx), p.client)
		provider, err := gooidc.NewProvider(dctx, p.cfg.IssuerURL)
		if err != nil {
			return nil, fmt.Errorf("oidc discovery for %q: %w", p.cfg.IssuerURL, err)
		}
		p.mu.Lock()
		p.provider = provider
		p.verifier = provider.Verifier(&gooidc.Config{ClientID: p.cfg.ClientID})
		p.oauth = &oauth2.Config{
			ClientID:     p.cfg.ClientID,
			ClientSecret: p.cfg.ClientSecret,
			Endpoint:     provider.Endpoint(),
			RedirectURL:  p.cfg.RedirectURL,
			Scopes:       p.cfg.Scopes,
		}
		p.mu.Unlock()
		return nil, nil
	})
	return err
}

// AuthCodeURL builds the IdP authorization URL for the Authorization Code + PKCE
// (S256) flow. state and nonce are echoed back and checked at the callback; the
// pkceVerifier is the high-entropy secret whose S256 challenge is sent now and
// whose plaintext is sent at exchange. Triggers lazy discovery, so a degraded
// provider surfaces its error to the caller (which redirects with an error code).
func (p *Provider) AuthCodeURL(ctx context.Context, state, nonce, pkceVerifier string) (string, error) {
	if err := p.ensure(ctx); err != nil {
		return "", err
	}
	p.mu.RLock()
	oauth := p.oauth
	p.mu.RUnlock()
	return oauth.AuthCodeURL(state,
		gooidc.Nonce(nonce),
		oauth2.S256ChallengeOption(pkceVerifier),
	), nil
}

// Exchange trades the authorization code for tokens, verifies the ID token
// (issuer, audience, signature via JWKS), enforces nonce equality against
// expectedNonce, and maps the verified claims to an Identity. pkceVerifier is the
// plaintext PKCE secret from the sealed state cookie. Discovery is triggered
// lazily but its one-time network cost is paid under the lock; the per-login token
// exchange runs outside it so concurrent logins do not serialize.
func (p *Provider) Exchange(ctx context.Context, code, pkceVerifier, expectedNonce string) (Identity, error) {
	if err := p.ensure(ctx); err != nil {
		return Identity{}, err
	}
	p.mu.RLock()
	verifier, oauth := p.verifier, p.oauth
	p.mu.RUnlock()

	ctx = gooidc.ClientContext(ctx, p.client)
	tok, err := oauth.Exchange(ctx, code, oauth2.VerifierOption(pkceVerifier))
	if err != nil {
		return Identity{}, fmt.Errorf("oidc code exchange: %w", err)
	}
	rawID, ok := tok.Extra("id_token").(string)
	if !ok || rawID == "" {
		return Identity{}, ErrNoIDToken
	}
	idToken, err := verifier.Verify(ctx, rawID)
	if err != nil {
		return Identity{}, fmt.Errorf("oidc id_token verify: %w", err)
	}
	if idToken.Nonce != expectedNonce {
		return Identity{}, ErrNonceMismatch
	}
	var claims rawClaims
	if err := idToken.Claims(&claims); err != nil {
		return Identity{}, fmt.Errorf("oidc id_token claims: %w", err)
	}
	// sub/iss come from the verified token, not the unmarshalled claims blob.
	return Identity{
		Issuer:        idToken.Issuer,
		Subject:       idToken.Subject,
		Email:         claims.Email,
		EmailVerified: claims.emailVerifiedTrue(),
		Name:          claims.Name,
	}, nil
}

// rawClaims is the subset of ID-token claims uzi maps. email_verified is kept as
// raw JSON so a strict boolean-true check can reject the string "true", a missing
// value, or any non-boolean — matching sloppy IdPs that emit it as a string
// (audit L2).
type rawClaims struct {
	Email         string          `json:"email"`
	EmailVerified json.RawMessage `json:"email_verified"`
	Name          string          `json:"name"`
}

// emailVerifiedTrue reports whether email_verified was the JSON literal true.
func (c rawClaims) emailVerifiedTrue() bool {
	return string(bytes.TrimSpace(c.EmailVerified)) == "true"
}
