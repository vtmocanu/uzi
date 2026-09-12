package codexauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

// countingTransport is a fake http.RoundTripper that records how many requests hit
// each endpoint (keyed host+path) and serves a canned response per key. The REAL
// Client code runs above it — only the wire is faked. It is the instrument that
// makes "DiscoverIdentity made ZERO oauth calls" a measured fact rather than an
// assumption: every assertion below reads counts[oauthKey], and the refresh
// control proves that counter is not vacuously stuck at zero.
type countingTransport struct {
	counts    map[string]int
	responder func(key string, req *http.Request) (*http.Response, error)
}

func newCountingTransport(responder func(key string, req *http.Request) (*http.Response, error)) *countingTransport {
	return &countingTransport{counts: map[string]int{}, responder: responder}
}

func endpointKey(req *http.Request) string {
	return req.URL.Host + req.URL.Path
}

func (t *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	key := endpointKey(req)
	t.counts[key]++
	return t.responder(key, req)
}

// The two endpoint keys the fake distinguishes, derived from the default base URLs.
const (
	usageKey = "chatgpt.com/backend-api/wham/usage"
	oauthKey = "auth.openai.com/oauth/token"
)

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}
}

// newTestClient wires a Client to a counting transport through a real *http.Client,
// exactly the "http.Client with a fake RoundTripper" path the deliverable calls out.
func newTestClient(tr *countingTransport) *Client {
	return NewClient(WithHTTPDoer(&http.Client{Transport: tr}))
}

// assembleToken builds a token-SHAPED fixture string from parts at runtime, so no
// real or real-looking contiguous token literal lives in tracked source.
func assembleToken(prefix string) string {
	return strings.Join([]string{prefix, "fixture", "not", "a", "real", "token"}, "-")
}

// assembleJWT builds a three-segment "a.b.c" JWT-shaped string from raw segment bytes,
// each unpadded-base64url-encoded (base64.RawURLEncoding — the shape a real ChatGPT
// access token uses, and what cmd/codexm3btestserver builds). It lets the fallback
// tests construct a payload carrying an arbitrary claim object (or deliberately
// malformed bytes) without a contiguous token literal in tracked source.
func assembleJWT(header, payload, signature []byte) string {
	enc := base64.RawURLEncoding.EncodeToString
	return enc(header) + "." + enc(payload) + "." + enc(signature)
}

// jwtWithAccountID assembles a well-formed access-token JWT whose payload carries the
// "https://api.openai.com/auth".chatgpt_account_id claim set to accountID — the
// personal-seat shape DiscoverIdentity falls back to when /wham/usage omits account_id.
func jwtWithAccountID(t *testing.T, accountID string) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"email": "seat@example.test",
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_plan_type":  "pro",
			"chatgpt_account_id": accountID,
		},
	})
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	return assembleJWT([]byte(`{"alg":"none","typ":"JWT"}`), payload, []byte("sig"))
}

func jwtWithFreshIdentityClaims(t *testing.T, accountID, chatGPTUserID, authUserID string) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"sub": "different-subject-domain",
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id":      accountID,
			"chatgpt_user_id":         chatGPTUserID,
			"chatgpt_account_user_id": "different-membership-domain",
			"user_id":                 authUserID,
		},
	})
	if err != nil {
		t.Fatalf("marshal fresh identity claims: %v", err)
	}
	return assembleJWT([]byte(`{"alg":"none","typ":"JWT"}`), payload, []byte("sig"))
}

func TestParseFreshAccessTokenIdentityClaims(t *testing.T) {
	header := []byte(`{"alg":"none","typ":"JWT"}`)
	sig := []byte("sig")
	tests := []struct {
		name  string
		token string
		want  FreshAccessTokenIdentityClaims
	}{
		{
			name:  "complete established claims",
			token: jwtWithFreshIdentityClaims(t, "acct-a", "user-a", "user-a"),
			want: FreshAccessTokenIdentityClaims{
				ChatGPTAccountID: "acct-a",
				ChatGPTUserID:    "user-a",
				AuthUserID:       "user-a",
			},
		},
		{
			name:  "distinct user claims stay distinct",
			token: jwtWithFreshIdentityClaims(t, "acct-a", "user-a", "user-b"),
			want: FreshAccessTokenIdentityClaims{
				ChatGPTAccountID: "acct-a",
				ChatGPTUserID:    "user-a",
				AuthUserID:       "user-b",
			},
		},
		{
			name:  "missing user claim remains empty",
			token: assembleJWT(header, []byte(`{"https://api.openai.com/auth":{"chatgpt_account_id":"acct-a","chatgpt_user_id":"user-a"}}`), sig),
			want:  FreshAccessTokenIdentityClaims{ChatGPTAccountID: "acct-a", ChatGPTUserID: "user-a"},
		},
		{
			name:  "wrong claim type zeroes result",
			token: assembleJWT(header, []byte(`{"https://api.openai.com/auth":{"chatgpt_account_id":"acct-a","chatgpt_user_id":123,"user_id":"user-a"}}`), sig),
		},
		{name: "malformed jwt zeroes result", token: "not-a-jwt"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ParseFreshAccessTokenIdentityClaims(tt.token); got != tt.want {
				t.Fatalf("claims = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// (a) Both identity fields present → Identity returned, and the oauth counter is 0
// after DiscoverIdentity — the discovery path is nonrotating.
func TestDiscoverIdentityBothPresent(t *testing.T) {
	tr := newCountingTransport(func(key string, _ *http.Request) (*http.Response, error) {
		if key != usageKey {
			t.Errorf("unexpected endpoint hit: %s", key)
		}
		return jsonResponse(http.StatusOK, `{"user_id":"user-abc","account_id":"acct-xyz","extra":"ignored"}`), nil
	})
	c := newTestClient(tr)

	id, err := c.DiscoverIdentity(context.Background(), assembleToken("access"))
	if err != nil {
		t.Fatalf("DiscoverIdentity: %v", err)
	}
	if id.ProviderUserID != "user-abc" || id.WorkspaceAccountID != "acct-xyz" {
		t.Fatalf("identity = %+v, want {user-abc acct-xyz}", id)
	}
	if tr.counts[usageKey] != 1 {
		t.Fatalf("usage endpoint hit %d times, want 1", tr.counts[usageKey])
	}
	if tr.counts[oauthKey] != 0 {
		t.Fatalf("oauth endpoint hit %d times, want 0 (discovery must be nonrotating)", tr.counts[oauthKey])
	}
}

// (b) Missing user_id → ErrIdentityIncomplete, oauth counter 0.
func TestDiscoverIdentityMissingUserID(t *testing.T) {
	tr := newCountingTransport(func(string, *http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, `{"account_id":"acct-xyz"}`), nil
	})
	c := newTestClient(tr)

	_, err := c.DiscoverIdentity(context.Background(), assembleToken("access"))
	if !errors.Is(err, ErrIdentityIncomplete) {
		t.Fatalf("err = %v, want ErrIdentityIncomplete", err)
	}
	if tr.counts[oauthKey] != 0 {
		t.Fatalf("oauth endpoint hit %d times, want 0", tr.counts[oauthKey])
	}
}

// (c) Missing account_id → ErrIdentityIncomplete.
func TestDiscoverIdentityMissingAccountID(t *testing.T) {
	tr := newCountingTransport(func(string, *http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, `{"user_id":"user-abc"}`), nil
	})
	c := newTestClient(tr)

	_, err := c.DiscoverIdentity(context.Background(), assembleToken("access"))
	if !errors.Is(err, ErrIdentityIncomplete) {
		t.Fatalf("err = %v, want ErrIdentityIncomplete", err)
	}
	if tr.counts[oauthKey] != 0 {
		t.Fatalf("oauth endpoint hit %d times, want 0", tr.counts[oauthKey])
	}
}

// (d) 401 → typed auth error, oauth counter 0.
func TestDiscoverIdentityUnauthorized(t *testing.T) {
	tr := newCountingTransport(func(string, *http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusUnauthorized, `{"error":"invalid_token"}`), nil
	})
	c := newTestClient(tr)

	_, err := c.DiscoverIdentity(context.Background(), assembleToken("access"))
	var authErr *AuthError
	if !errors.As(err, &authErr) {
		t.Fatalf("err = %v, want *AuthError", err)
	}
	if authErr.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", authErr.StatusCode)
	}
	if !authErr.Unauthorized() {
		t.Fatalf("Unauthorized() = false, want true for 401")
	}
	if tr.counts[oauthKey] != 0 {
		t.Fatalf("oauth endpoint hit %d times, want 0", tr.counts[oauthKey])
	}
	// The error message must not echo the response body.
	if strings.Contains(err.Error(), "invalid_token") {
		t.Fatalf("error leaked response body: %q", err.Error())
	}
}

// (e) Known-identity refresh control: Refresh increments the oauth counter to 1 and
// returns the new access token, proving the instrument observes oauth calls (so the
// zero-counts in (a)-(d) are meaningful, not vacuous).
func TestRefreshControlObservesOAuth(t *testing.T) {
	newAccess := jwtWithFreshIdentityClaims(t, "acct-rotated", "user-rotated", "user-rotated")
	newRefresh := assembleToken("rotated-refresh")
	tr := newCountingTransport(func(key string, _ *http.Request) (*http.Response, error) {
		if key != oauthKey {
			t.Errorf("unexpected endpoint hit: %s", key)
		}
		return jsonResponse(http.StatusOK, `{"access_token":"`+newAccess+`","refresh_token":"`+newRefresh+`","id_token":"ignored"}`), nil
	})
	c := newTestClient(tr)

	res, err := c.Refresh(context.Background(), assembleToken("old-refresh"))
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if res.AccessToken != newAccess {
		t.Fatalf("access token = %q, want %q", res.AccessToken, newAccess)
	}
	if res.RefreshToken == nil || *res.RefreshToken != newRefresh {
		t.Fatalf("refresh token = %v, want %q", res.RefreshToken, newRefresh)
	}
	wantClaims := FreshAccessTokenIdentityClaims{
		ChatGPTAccountID: "acct-rotated",
		ChatGPTUserID:    "user-rotated",
		AuthUserID:       "user-rotated",
	}
	if res.IdentityClaims != wantClaims {
		t.Fatalf("identity claims = %+v, want %+v", res.IdentityClaims, wantClaims)
	}
	if tr.counts[oauthKey] != 1 {
		t.Fatalf("oauth endpoint hit %d times, want 1", tr.counts[oauthKey])
	}
	if tr.counts[usageKey] != 0 {
		t.Fatalf("usage endpoint hit %d times, want 0", tr.counts[usageKey])
	}
}

// Refresh with an omitted refresh_token → nil RefreshToken (provider kept the old
// one), access token still required and returned.
func TestRefreshOmittedRefreshToken(t *testing.T) {
	newAccess := assembleToken("rotated-access")
	tr := newCountingTransport(func(string, *http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, `{"access_token":"`+newAccess+`"}`), nil
	})
	c := newTestClient(tr)

	res, err := c.Refresh(context.Background(), assembleToken("old-refresh"))
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if res.AccessToken != newAccess {
		t.Fatalf("access token = %q, want %q", res.AccessToken, newAccess)
	}
	if res.RefreshToken != nil {
		t.Fatalf("refresh token = %v, want nil when provider omits it", *res.RefreshToken)
	}
}

// Refresh with a 2xx body that carries no access_token → ErrNoAccessToken.
func TestRefreshNoAccessToken(t *testing.T) {
	tr := newCountingTransport(func(string, *http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, `{"id_token":"only-this"}`), nil
	})
	c := newTestClient(tr)

	_, err := c.Refresh(context.Background(), assembleToken("old-refresh"))
	if !errors.Is(err, ErrNoAccessToken) {
		t.Fatalf("err = %v, want ErrNoAccessToken", err)
	}
}

// Personal-seat fallback: usage 200 with an empty account_id but a bearer JWT carrying
// chatgpt_account_id → the claim supplies WorkspaceAccountID. Also proves the exact
// bearer reached /wham/usage and that the discovery stayed nonrotating (zero oauth).
func TestDiscoverIdentityAccountIDFromTokenClaim(t *testing.T) {
	token := jwtWithAccountID(t, "acct-from-claim")
	var gotAuth string
	tr := newCountingTransport(func(key string, req *http.Request) (*http.Response, error) {
		if key != usageKey {
			t.Errorf("unexpected endpoint hit: %s", key)
		}
		gotAuth = req.Header.Get("Authorization")
		return jsonResponse(http.StatusOK, `{"user_id":"user-abc"}`), nil
	})
	c := newTestClient(tr)

	id, err := c.DiscoverIdentity(context.Background(), token)
	if err != nil {
		t.Fatalf("DiscoverIdentity: %v", err)
	}
	if id.ProviderUserID != "user-abc" || id.WorkspaceAccountID != "acct-from-claim" {
		t.Fatalf("identity = %+v, want {user-abc acct-from-claim}", id)
	}
	if gotAuth != "Bearer "+token {
		t.Fatalf("Authorization header = %q, want %q", gotAuth, "Bearer "+token)
	}
	if tr.counts[usageKey] != 1 {
		t.Fatalf("usage endpoint hit %d times, want 1", tr.counts[usageKey])
	}
	if tr.counts[oauthKey] != 0 {
		t.Fatalf("oauth endpoint hit %d times, want 0 (discovery must be nonrotating)", tr.counts[oauthKey])
	}
}

// Lazy parse: when the usage response carries account_id, a deliberately malformed
// bearer is tolerated (never required to be a valid JWT) — the token is consulted only
// on the empty-account_id fallback path. This pins tolerance, not precedence order; the
// discriminating precedence assertion is TestDiscoverIdentityResponseAccountIDBeatsTokenClaim.
func TestDiscoverIdentityResponseAccountIDToleratesMalformedToken(t *testing.T) {
	tr := newCountingTransport(func(key string, _ *http.Request) (*http.Response, error) {
		if key != usageKey {
			t.Errorf("unexpected endpoint hit: %s", key)
		}
		return jsonResponse(http.StatusOK, `{"user_id":"user-abc","account_id":"acct-from-usage"}`), nil
	})
	c := newTestClient(tr)

	id, err := c.DiscoverIdentity(context.Background(), "not-a-jwt")
	if err != nil {
		t.Fatalf("DiscoverIdentity: %v", err)
	}
	if id.ProviderUserID != "user-abc" {
		t.Fatalf("ProviderUserID = %q, want user-abc", id.ProviderUserID)
	}
	if id.WorkspaceAccountID != "acct-from-usage" {
		t.Fatalf("WorkspaceAccountID = %q, want acct-from-usage (a malformed token must not error the account_id-present path)", id.WorkspaceAccountID)
	}
}

// Precedence order (security-relevant): the provider-verified /wham/usage account_id must
// win over the caller-supplied JWT claim. A VALID token carrying a DIFFERENT account id
// discriminates this from "token wins" — the response value must be the one returned, so a
// future flip to trusting the claim over the provider read is caught here.
func TestDiscoverIdentityResponseAccountIDBeatsTokenClaim(t *testing.T) {
	token := jwtWithAccountID(t, "acct-from-token-should-lose")
	tr := newCountingTransport(func(key string, _ *http.Request) (*http.Response, error) {
		if key != usageKey {
			t.Errorf("unexpected endpoint hit: %s", key)
		}
		return jsonResponse(http.StatusOK, `{"user_id":"user-abc","account_id":"acct-from-usage"}`), nil
	})
	c := newTestClient(tr)

	id, err := c.DiscoverIdentity(context.Background(), token)
	if err != nil {
		t.Fatalf("DiscoverIdentity: %v", err)
	}
	if id.WorkspaceAccountID != "acct-from-usage" {
		t.Fatalf("WorkspaceAccountID = %q, want acct-from-usage (provider-verified response must beat the JWT claim)", id.WorkspaceAccountID)
	}
}

// Malformed/absent account-id claim: with user_id present but account_id absent from the
// usage response, each structurally broken token falls through to ErrIdentityIncomplete.
func TestDiscoverIdentityMalformedTokenClaimStaysIncomplete(t *testing.T) {
	enc := base64.RawURLEncoding.EncodeToString
	header := []byte(`{"alg":"none","typ":"JWT"}`)
	sig := []byte("sig")
	cases := []struct {
		name  string
		token string
	}{
		// (a) two segments instead of three.
		{"wrong segment count", enc(header) + "." + enc([]byte(`{"https://api.openai.com/auth":{"chatgpt_account_id":"acct"}}`))},
		// (b) payload segment is not valid base64url ('!' is outside the alphabet).
		{"bad base64 payload", enc(header) + ".!!!not-base64!!!." + enc(sig)},
		// (c) payload decodes to bytes that are not JSON.
		{"valid base64 invalid json", assembleJWT(header, []byte("not-json"), sig)},
		// (d) valid JSON but no OpenAI auth namespace.
		{"missing namespace", assembleJWT(header, []byte(`{"email":"seat@example.test"}`), sig)},
		// (e) namespace present but the account-id claim is missing.
		{"namespace without account id", assembleJWT(header, []byte(`{"https://api.openai.com/auth":{"chatgpt_plan_type":"pro"}}`), sig)},
		// (e') namespace present but the account-id claim is the empty string.
		{"empty account id", assembleJWT(header, []byte(`{"https://api.openai.com/auth":{"chatgpt_account_id":""}}`), sig)},
		// (f) account-id claim present but the wrong JSON type (number, not string).
		{"account id wrong type", assembleJWT(header, []byte(`{"https://api.openai.com/auth":{"chatgpt_account_id":12345}}`), sig)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := newCountingTransport(func(string, *http.Request) (*http.Response, error) {
				return jsonResponse(http.StatusOK, `{"user_id":"user-abc"}`), nil
			})
			c := newTestClient(tr)

			_, err := c.DiscoverIdentity(context.Background(), tc.token)
			if !errors.Is(err, ErrIdentityIncomplete) {
				t.Fatalf("err = %v, want ErrIdentityIncomplete", err)
			}
			if tr.counts[oauthKey] != 0 {
				t.Fatalf("oauth endpoint hit %d times, want 0", tr.counts[oauthKey])
			}
		})
	}
}

// user_id anchor: even a well-formed account-id claim never manufactures a subject. With
// account_id present in the response but user_id absent, identity stays incomplete.
func TestDiscoverIdentityUserIDNeverFromToken(t *testing.T) {
	token := jwtWithAccountID(t, "acct-from-claim")
	tr := newCountingTransport(func(string, *http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, `{"account_id":"acct-from-usage"}`), nil
	})
	c := newTestClient(tr)

	_, err := c.DiscoverIdentity(context.Background(), token)
	if !errors.Is(err, ErrIdentityIncomplete) {
		t.Fatalf("err = %v, want ErrIdentityIncomplete (user_id is never derived from the token)", err)
	}
	if tr.counts[oauthKey] != 0 {
		t.Fatalf("oauth endpoint hit %d times, want 0", tr.counts[oauthKey])
	}
}

// Non-2xx short-circuits before any token parse: a 401 with a valid-claim bearer yields
// the typed *AuthError, never an identity synthesised from the claim.
func TestDiscoverIdentityUnauthorizedIgnoresTokenClaim(t *testing.T) {
	token := jwtWithAccountID(t, "acct-from-claim")
	tr := newCountingTransport(func(string, *http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusUnauthorized, `{"error":"invalid_token"}`), nil
	})
	c := newTestClient(tr)

	_, err := c.DiscoverIdentity(context.Background(), token)
	var authErr *AuthError
	if !errors.As(err, &authErr) {
		t.Fatalf("err = %v, want *AuthError", err)
	}
	if authErr.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", authErr.StatusCode)
	}
	if tr.counts[oauthKey] != 0 {
		t.Fatalf("oauth endpoint hit %d times, want 0", tr.counts[oauthKey])
	}
}
