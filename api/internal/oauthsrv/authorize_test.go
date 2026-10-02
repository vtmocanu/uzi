package oauthsrv

import (
	"net/url"
	"strings"
	"testing"
)

const testChallenge = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM" // 43 base64url chars

func testClient() *Client {
	return &Client{
		Active:       true,
		RedirectURIs: []string{"https://p.example/cb", "http://127.0.0.1:8080/cb?x=1"},
		Scopes:       []string{"jobs:run", "jobs:read"},
		HasSecret:    true,
	}
}

func goodQuery() url.Values {
	return url.Values{
		"client_id":             {"11111111-1111-1111-1111-111111111111"},
		"redirect_uri":          {"https://p.example/cb"},
		"response_type":         {"code"},
		"code_challenge":        {testChallenge},
		"code_challenge_method": {"S256"},
		"state":                 {"abc 123"},
	}
}

func TestCheckClientRejectsBeforeAnyRedirect(t *testing.T) {
	notClient := func(mut func(*Client)) *Client { c := testClient(); mut(c); return c }
	tests := []struct {
		name   string
		client *Client
		mut    func(url.Values)
	}{
		{"unknown product", nil, nil},
		{"disabled or deleted product", notClient(func(c *Client) { c.Active = false }), nil},
		{"no redirect URI registered", notClient(func(c *Client) { c.RedirectURIs = nil }), nil},
		{"no allowed scopes", notClient(func(c *Client) { c.Scopes = nil }), nil},
		{"no secret", notClient(func(c *Client) { c.HasSecret = false }), nil},
		{"redirect_uri missing", testClient(), func(v url.Values) { v.Del("redirect_uri") }},
		{"redirect_uri empty", testClient(), func(v url.Values) { v.Set("redirect_uri", "") }},
		{"redirect_uri differs in path", testClient(), func(v url.Values) { v.Set("redirect_uri", "https://p.example/cb2") }},
		{"redirect_uri trailing slash", testClient(), func(v url.Values) { v.Set("redirect_uri", "https://p.example/cb/") }},
		{"redirect_uri case differs", testClient(), func(v url.Values) { v.Set("redirect_uri", "https://P.example/cb") }},
		{"redirect_uri extra query", testClient(), func(v url.Values) { v.Set("redirect_uri", "https://p.example/cb?a=b") }},
		{"redirect_uri prefix of registered", testClient(), func(v url.Values) { v.Set("redirect_uri", "http://127.0.0.1:8080/cb") }},
		{"redirect_uri with fragment", testClient(), func(v url.Values) { v.Set("redirect_uri", "https://p.example/cb#x") }},
		{"redirect_uri repeated", testClient(), func(v url.Values) { v["redirect_uri"] = []string{"https://p.example/cb", "https://p.example/cb"} }},
		{"client_id repeated", testClient(), func(v url.Values) { v["client_id"] = []string{"a", "b"} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := goodQuery()
			if tt.mut != nil {
				tt.mut(v)
			}
			if got, err := CheckClient(tt.client, ParseAuthorizeQuery(v)); err != ErrClientRejected || got != "" {
				t.Fatalf("CheckClient = %q, %v; want \"\", ErrClientRejected", got, err)
			}
		})
	}
}

func TestCheckClientAcceptsExactMatch(t *testing.T) {
	for _, uri := range []string{"https://p.example/cb", "http://127.0.0.1:8080/cb?x=1"} {
		v := goodQuery()
		v.Set("redirect_uri", uri)
		if got, err := CheckClient(testClient(), ParseAuthorizeQuery(v)); err != nil || got != uri {
			t.Errorf("CheckClient(%q) = %q, %v", uri, got, err)
		}
	}
}

func TestValidateAuthorizeRejects(t *testing.T) {
	tests := []struct {
		name      string
		mut       func(url.Values)
		wantCode  string
		wantState string // echoed state; "" = none
	}{
		{"response_type missing", func(v url.Values) { v.Del("response_type") }, ErrInvalidRequest, "abc 123"},
		{"response_type token", func(v url.Values) { v.Set("response_type", "token") }, ErrUnsupportedResponseType, "abc 123"},
		{"response_type uppercase", func(v url.Values) { v.Set("response_type", "CODE") }, ErrUnsupportedResponseType, "abc 123"},
		{"pkce method missing", func(v url.Values) { v.Del("code_challenge_method") }, ErrInvalidRequest, "abc 123"},
		{"pkce method plain", func(v url.Values) { v.Set("code_challenge_method", "plain") }, ErrInvalidRequest, "abc 123"},
		{"pkce method lowercase s256", func(v url.Values) { v.Set("code_challenge_method", "s256") }, ErrInvalidRequest, "abc 123"},
		{"challenge missing", func(v url.Values) { v.Del("code_challenge") }, ErrInvalidRequest, "abc 123"},
		{"challenge 42 chars", func(v url.Values) { v.Set("code_challenge", testChallenge[:42]) }, ErrInvalidRequest, "abc 123"},
		{"challenge 44 chars", func(v url.Values) { v.Set("code_challenge", testChallenge+"A") }, ErrInvalidRequest, "abc 123"},
		{"challenge padded", func(v url.Values) { v.Set("code_challenge", testChallenge[:42]+"=") }, ErrInvalidRequest, "abc 123"},
		{"challenge std base64 plus", func(v url.Values) { v.Set("code_challenge", testChallenge[:42]+"+") }, ErrInvalidRequest, "abc 123"},
		{"challenge huge", func(v url.Values) { v.Set("code_challenge", strings.Repeat("A", 10000)) }, ErrInvalidRequest, "abc 123"},
		{"state missing", func(v url.Values) { v.Del("state") }, ErrInvalidRequest, ""},
		{"state empty", func(v url.Values) { v.Set("state", "") }, ErrInvalidRequest, ""},
		{"state 513 bytes", func(v url.Values) { v.Set("state", strings.Repeat("s", 513)) }, ErrInvalidRequest, ""},
		{"state with control byte", func(v url.Values) { v.Set("state", "a\x00b") }, ErrInvalidRequest, ""},
		{"state with non-ascii", func(v url.Values) { v.Set("state", "café") }, ErrInvalidRequest, ""},
		{"state repeated", func(v url.Values) { v["state"] = []string{"a", "b"} }, ErrInvalidRequest, ""},
		{"scope empty", func(v url.Values) { v.Set("scope", "") }, ErrInvalidScope, "abc 123"},
		{"scope unknown", func(v url.Values) { v.Set("scope", "jobs:admin") }, ErrInvalidScope, "abc 123"},
		{"scope one unknown", func(v url.Values) { v.Set("scope", "jobs:run jobs:admin") }, ErrInvalidScope, "abc 123"},
		{"scope duplicate", func(v url.Values) { v.Set("scope", "jobs:run jobs:run") }, ErrInvalidScope, "abc 123"},
		{"scope double space", func(v url.Values) { v.Set("scope", "jobs:run  jobs:read") }, ErrInvalidScope, "abc 123"},
		{"scope over 64 bytes", func(v url.Values) { v.Set("scope", "jobs:run "+strings.Repeat("x", 60)) }, ErrInvalidScope, "abc 123"},
		{"other param repeated", func(v url.Values) { v["scope"] = []string{"jobs:run", "jobs:read"} }, ErrInvalidRequest, "abc 123"},
		{"unrelated param repeated", func(v url.Values) { v["foo"] = []string{"1", "2"} }, ErrInvalidRequest, "abc 123"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := goodQuery()
			tt.mut(v)
			req, e := ValidateAuthorize(testClient(), ParseAuthorizeQuery(v), "https://p.example/cb")
			if req != nil || e == nil {
				t.Fatalf("ValidateAuthorize = %+v, %v; want an error", req, e)
			}
			if e.Code != tt.wantCode {
				t.Errorf("code = %q, want %q", e.Code, tt.wantCode)
			}
			if e.State != tt.wantState {
				t.Errorf("echoed state = %q, want %q", e.State, tt.wantState)
			}
			if e.Description == "" {
				t.Error("description is empty")
			}
		})
	}
}

func TestValidateAuthorizeScopeNarrowerThanClient(t *testing.T) {
	c := testClient()
	c.Scopes = []string{"jobs:read"}
	v := goodQuery()
	v.Set("scope", "jobs:run")
	if _, e := ValidateAuthorize(c, ParseAuthorizeQuery(v), "https://p.example/cb"); e == nil || e.Code != ErrInvalidScope {
		t.Fatalf("scope outside the client's allowed list: %v", e)
	}
}

func TestValidateAuthorizeAccepts(t *testing.T) {
	t.Run("missing scope means all allowed", func(t *testing.T) {
		req, e := ValidateAuthorize(testClient(), ParseAuthorizeQuery(goodQuery()), "https://p.example/cb")
		if e != nil {
			t.Fatal(e)
		}
		if strings.Join(req.Scopes, " ") != "jobs:run jobs:read" || req.State != "abc 123" || req.CodeChallenge != testChallenge || req.RedirectURI != "https://p.example/cb" {
			t.Fatalf("request = %+v", req)
		}
	})
	t.Run("explicit subset", func(t *testing.T) {
		v := goodQuery()
		v.Set("scope", "jobs:read")
		req, e := ValidateAuthorize(testClient(), ParseAuthorizeQuery(v), "https://p.example/cb")
		if e != nil || strings.Join(req.Scopes, " ") != "jobs:read" {
			t.Fatalf("got %+v, %v", req, e)
		}
	})
	t.Run("state at the cap", func(t *testing.T) {
		v := goodQuery()
		v.Set("state", strings.Repeat("s", MaxStateBytes))
		if _, e := ValidateAuthorize(testClient(), ParseAuthorizeQuery(v), "https://p.example/cb"); e != nil {
			t.Fatal(e)
		}
	})
	t.Run("scope at the cap is still a normal scope list", func(t *testing.T) {
		v := goodQuery()
		v.Set("scope", "jobs:run jobs:read")
		if _, e := ValidateAuthorize(testClient(), ParseAuthorizeQuery(v), "https://p.example/cb"); e != nil {
			t.Fatal(e)
		}
	})
	t.Run("redirect URI is the verified one passed in, not the query's", func(t *testing.T) {
		// The request's redirect_uri must never flow to a redirect or the stored row: only the
		// registered value CheckClient returned does.
		v := goodQuery()
		v.Set("redirect_uri", "https://evil.example/cb")
		req, e := ValidateAuthorize(testClient(), ParseAuthorizeQuery(v), "https://p.example/cb")
		if e != nil || req.RedirectURI != "https://p.example/cb" {
			t.Fatalf("got %+v, %v", req, e)
		}
	})
	t.Run("does not alias the client's scope slice", func(t *testing.T) {
		c := testClient()
		req, _ := ValidateAuthorize(c, ParseAuthorizeQuery(goodQuery()), "https://p.example/cb")
		req.Scopes[0] = "x"
		if c.Scopes[0] != "jobs:run" {
			t.Fatal("request scopes alias the client's")
		}
	})
}

func TestRedirectURLs(t *testing.T) {
	parse := func(t *testing.T, s string) *url.URL {
		t.Helper()
		u, err := url.Parse(s)
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	const iss = "https://uzi.example"
	t.Run("code redirect adds code state iss", func(t *testing.T) {
		u := parse(t, CodeRedirectURL("https://p.example/cb", "the-code", "st ate&x=1", iss))
		q := u.Query()
		if u.Host != "p.example" || u.Path != "/cb" || u.Fragment != "" || q.Get("code") != "the-code" || q.Get("state") != "st ate&x=1" || q.Get("iss") != iss || len(q) != 3 {
			t.Fatalf("got %s", u)
		}
	})
	t.Run("registered query is preserved", func(t *testing.T) {
		u := parse(t, CodeRedirectURL("http://127.0.0.1:8080/cb?tenant=a%20b&z=1", "c", "s", iss))
		q := u.Query()
		if q.Get("tenant") != "a b" || q.Get("z") != "1" || q.Get("code") != "c" || q.Get("iss") != iss {
			t.Fatalf("got %s", u)
		}
	})
	t.Run("registered URI ending in a question mark", func(t *testing.T) {
		got := CodeRedirectURL("https://p.example/cb?", "c", "s", iss)
		if strings.Contains(got, "??") || strings.Contains(got, "?&") || parse(t, got).Query().Get("code") != "c" {
			t.Fatalf("got %s", got)
		}
	})
	t.Run("a pre-seeded reserved key in a stored URI never shadows the server's", func(t *testing.T) {
		u := parse(t, CodeRedirectURL("https://p.example/cb?keep=1&code=evil&state=evil&iss=evil&error=x&error_description=x&error_uri=x&%63ode=evil2", "real", "st", iss))
		q := u.Query()
		if len(q["code"]) != 1 || q.Get("code") != "real" || q.Get("state") != "st" || q.Get("iss") != iss || q.Get("keep") != "1" || q.Has("error") || q.Has("error_description") || q.Has("error_uri") {
			t.Fatalf("got %s", u)
		}
	})
	t.Run("error redirect", func(t *testing.T) {
		u := parse(t, ErrorRedirectURL("https://p.example/cb", &Error{Code: ErrInvalidScope, Description: "d", State: "st"}, iss))
		q := u.Query()
		if q.Get("error") != ErrInvalidScope || q.Get("error_description") != "d" || q.Get("state") != "st" || q.Get("iss") != iss || u.Fragment != "" {
			t.Fatalf("got %s", u)
		}
	})
	t.Run("error redirect without state omits it", func(t *testing.T) {
		u := parse(t, ErrorRedirectURL("https://p.example/cb", &Error{Code: ErrInvalidRequest, Description: "d"}, iss))
		if _, ok := u.Query()["state"]; ok {
			t.Fatalf("state present in %s", u)
		}
	})
	t.Run("denied redirect", func(t *testing.T) {
		q := parse(t, DeniedRedirectURL("https://p.example/cb", "st", iss)).Query()
		if q.Get("error") != ErrAccessDenied || q.Get("state") != "st" || q.Get("iss") != iss {
			t.Fatalf("got %v", q)
		}
	})
}
