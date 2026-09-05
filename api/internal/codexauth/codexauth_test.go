package codexauth

import (
	"context"
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
	newAccess := assembleToken("rotated-access")
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
