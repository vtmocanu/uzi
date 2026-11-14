package forge

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

var appTestKeyOnce sync.Once
var appTestKeyValue *rsa.PrivateKey
var appTestKeyError error

func appTestCredential(t *testing.T) (GitHubAppCredential, *rsa.PrivateKey) {
	t.Helper()
	appTestKeyOnce.Do(func() { appTestKeyValue, appTestKeyError = rsa.GenerateKey(rand.Reader, 2048) })
	if appTestKeyError != nil {
		t.Fatal(appTestKeyError)
	}
	key := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(appTestKeyValue)}))
	return GitHubAppCredential{AuthType: "github_app", AppID: "42", InstallationID: 73, PrivateKey: key, Repositories: []string{"widgets"}}, appTestKeyValue
}

func appTestDocument(t *testing.T, c GitHubAppCredential) string {
	t.Helper()
	b, err := json.Marshal(c) //nolint:gosec // G117: serializes a runtime-generated fixture key to test the encrypted-credential input contract.
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestGitHubAppCredentialValidation(t *testing.T) {
	valid, _ := appTestCredential(t)
	for name, mutate := range map[string]func(*GitHubAppCredential){
		"wrong-kind":            func(c *GitHubAppCredential) { c.AuthType = "pat" },
		"invalid-app-id":        func(c *GitHubAppCredential) { c.AppID = "forty-two" },
		"negative-app-id":       func(c *GitHubAppCredential) { c.AppID = "-42" },
		"missing-installation":  func(c *GitHubAppCredential) { c.InstallationID = 0 },
		"no-repositories":       func(c *GitHubAppCredential) { c.Repositories = nil },
		"owner-and-name":        func(c *GitHubAppCredential) { c.Repositories = []string{"acme/widgets"} },
		"path-traversal":        func(c *GitHubAppCredential) { c.Repositories = []string{".."} },
		"case-duplicate":        func(c *GitHubAppCredential) { c.Repositories = []string{"widgets", "WIDGETS"} },
		"too-many-repositories": func(c *GitHubAppCredential) { c.Repositories = make([]string, 101) },
		"malformed-key":         func(c *GitHubAppCredential) { c.PrivateKey = "invalid-private-key-canary" },
		"trailing-key":          func(c *GitHubAppCredential) { c.PrivateKey += "unexpected" },
	} {
		t.Run(name, func(t *testing.T) {
			c := valid
			mutate(&c)
			_, _, err := parseGitHubAppCredential(appTestDocument(t, c))
			if err == nil {
				t.Fatal("invalid credential was accepted")
			}
			if strings.Contains(err.Error(), c.PrivateKey) {
				t.Fatal("error exposed private key")
			}
		})
	}
	for name, doc := range map[string]string{
		"malformed-json": "{",
		"unknown-field":  strings.TrimSuffix(appTestDocument(t, valid), "}") + `,"unexpected":true}`,
		"trailing-json":  appTestDocument(t, valid) + `{}`,
		"oversized":      strings.Repeat(" ", 64<<10) + appTestDocument(t, valid),
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := parseGitHubAppCredential(doc); err == nil {
				t.Fatal("invalid document was accepted")
			}
		})
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(appTestKeyValue)
	if err != nil {
		t.Fatal(err)
	}
	valid.PrivateKey = string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8}))
	if _, _, err := parseGitHubAppCredential(appTestDocument(t, valid)); err != nil {
		t.Fatalf("PKCS8 RSA key rejected: %v", err)
	}
	ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ecBytes, err := x509.MarshalPKCS8PrivateKey(ec)
	if err != nil {
		t.Fatal(err)
	}
	valid.PrivateKey = string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: ecBytes}))
	if _, _, err := parseGitHubAppCredential(appTestDocument(t, valid)); err == nil {
		t.Fatal("non-RSA key was accepted")
	}
}

type appAuthFixture struct {
	auth       *githubAppAuth
	credential GitHubAppCredential
	now        time.Time
	mints      atomic.Int64
	mutate     func(string, map[string]any)
	failPath   string
	serve      func(http.ResponseWriter, *http.Request) bool
}

func newAppAuthFixture(t *testing.T) *appAuthFixture {
	t.Helper()
	c, key := appTestCredential(t)
	f := &appAuthFixture{credential: c, now: time.Date(2026, 11, 6, 20, 0, 0, 0, time.UTC)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f.serve != nil && f.serve(w, r) {
			return
		}
		signed := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		claims := &jwt.RegisteredClaims{}
		_, err := jwt.ParseWithClaims(signed, claims, func(*jwt.Token) (any, error) { return &key.PublicKey, nil }, jwt.WithValidMethods([]string{"RS256"}), jwt.WithIssuer("42"), jwt.WithTimeFunc(func() time.Time { return f.now }))
		if err != nil {
			t.Errorf("App JWT failed signature/claims validation: %v", err)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if claims.IssuedAt == nil || claims.ExpiresAt == nil || !claims.IssuedAt.Equal(f.now.Add(-time.Minute)) || !claims.ExpiresAt.Equal(f.now.Add(8*time.Minute)) {
			t.Error("App JWT has incorrect issue/expiry bounds")
		}
		if r.URL.Path == f.failPath {
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, signed+f.credential.PrivateKey) //nolint:gosec // G705: isolated test server intentionally echoes generated secrets to verify that no response body escapes in errors.
			return
		}
		var out map[string]any
		switch r.URL.Path {
		case "/app":
			if r.Method != http.MethodGet {
				t.Error("App identity must use GET")
			}
			out = map[string]any{"id": int64(42), "slug": "multica-test"}
		case "/app/installations/73":
			if r.Method != http.MethodGet {
				t.Error("installation lookup must use GET")
			}
			out = map[string]any{"app_id": int64(42), "id": int64(73)}
		case "/app/installations/73/access_tokens":
			if r.Method != http.MethodPost {
				t.Error("token mint must use POST")
			}
			var request struct {
				Repositories []string          `json:"repositories"`
				Permissions  map[string]string `json:"permissions"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode mint request: %v", err)
			}
			expected := map[string]string{"contents": "write", "issues": "write", "pull_requests": "write", "metadata": "read", "actions": "read", "checks": "read", "statuses": "read", "administration": "read"}
			if !reflect.DeepEqual(request.Repositories, []string{"widgets"}) || !reflect.DeepEqual(request.Permissions, expected) {
				t.Error("mint request did not downscope to the exact repository and minimum permissions")
			}
			out = map[string]any{"token": "ghs_" + "installation-fixture-" + strconv.FormatInt(f.mints.Add(1), 10), "expires_at": f.now.Add(time.Hour), "permissions": expected, "repositories": []map[string]any{{"name": "widgets", "id": int64(1)}}}
		default:
			t.Errorf("unexpected App authentication endpoint: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if f.mutate != nil {
			f.mutate(r.URL.Path, out)
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(out); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	a, err := newGitHubAppAuth(appTestDocument(t, c), srv.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	a.now = func() time.Time { return f.now }
	f.auth = a
	return f
}

func TestGitHubAppCredentialCachesAndRenewsConcurrently(t *testing.T) {
	f := newAppAuthFixture(t)
	first, expiry, err := f.auth.credential(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !expiry.Equal(f.now.Add(time.Hour)) || first == "" || f.auth.slug != "multica-test" {
		t.Fatal("installation identity or expiry was not retained")
	}
	var wg sync.WaitGroup
	check := func(expected string) {
		for range 12 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				token, _, err := f.auth.credential(context.Background())
				if err != nil || token != expected {
					t.Errorf("cached token mismatch: err=%v", err)
				}
			}()
		}
		wg.Wait()
	}
	check(first)
	if f.mints.Load() != 1 {
		t.Fatal("concurrent cache reads minted additional tokens")
	}
	f.now = f.now.Add(56 * time.Minute)
	refreshed := make(chan string, 12)
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			token, _, err := f.auth.credential(context.Background())
			if err != nil {
				t.Errorf("renewal failed: %v", err)
			}
			refreshed <- token
		}()
	}
	wg.Wait()
	close(refreshed)
	second := ""
	for token := range refreshed {
		if second == "" {
			second = token
		}
		if token == "" || token == first || token != second {
			t.Fatal("concurrent renewal returned stale or inconsistent credentials")
		}
	}
	check(second)
	if f.mints.Load() != 2 {
		t.Fatal("renewal was not synchronized")
	}
	values := f.auth.secretValues()
	if !containsAppTestSecret(values, first) || !containsAppTestSecret(values, second) {
		t.Fatal("issued credentials were not retained for redaction")
	}
	values[0] = "mutated-copy"
	if f.auth.secretValues()[0] == "mutated-copy" {
		t.Fatal("secretValues exposed mutable provider state")
	}
}

func containsAppTestSecret(values []string, secret string) bool {
	for _, value := range values {
		if value == secret {
			return true
		}
	}
	return false
}

func TestGitHubAppCredentialRejectsUnboundOrBroadResponses(t *testing.T) {
	for name, mutate := range map[string]func(string, map[string]any){
		"wrong-app": func(path string, out map[string]any) {
			if path == "/app" {
				out["id"] = int64(99)
			}
		},
		"wrong-installation-app": func(path string, out map[string]any) {
			if path == "/app/installations/73" {
				out["app_id"] = int64(99)
			}
		},
		"wrong-installation-id": func(path string, out map[string]any) {
			if path == "/app/installations/73" {
				out["id"] = int64(99)
			}
		},
		"suspended-installation": func(path string, out map[string]any) {
			if path == "/app/installations/73" {
				out["suspended_at"] = time.Now()
			}
		},
		"missing-permission": func(_ string, out map[string]any) {
			if p, ok := out["permissions"].(map[string]string); ok {
				delete(p, "issues")
			}
		},
		"weaker-permission": func(_ string, out map[string]any) {
			if p, ok := out["permissions"].(map[string]string); ok {
				p["contents"] = "read"
			}
		},
		"extra-permission": func(_ string, out map[string]any) {
			if p, ok := out["permissions"].(map[string]string); ok {
				p["administration"] = "write"
			}
		},
		"wrong-repository": func(_ string, out map[string]any) {
			if _, ok := out["repositories"]; ok {
				out["repositories"] = []map[string]any{{"name": "payments", "id": int64(1)}}
			}
		},
		"too-many-repositories": func(_ string, out map[string]any) {
			if _, ok := out["repositories"]; ok {
				out["repositories"] = []map[string]any{{"name": "widgets", "id": int64(1)}, {"name": "payments", "id": int64(2)}}
			}
		},
		"no-repository-proof": func(_ string, out map[string]any) { delete(out, "repositories") },
		"expired-token": func(_ string, out map[string]any) {
			if _, ok := out["token"]; ok {
				out["expires_at"] = time.Date(2026, 11, 6, 19, 59, 0, 0, time.UTC)
			}
		},
		"short-lived-token": func(_ string, out map[string]any) {
			if _, ok := out["token"]; ok {
				out["expires_at"] = time.Date(2026, 11, 6, 20, 4, 0, 0, time.UTC)
			}
		},
		"overlong-token": func(_ string, out map[string]any) {
			if _, ok := out["token"]; ok {
				out["expires_at"] = time.Date(2026, 11, 7, 20, 0, 0, 0, time.UTC)
			}
		},
		"wrong-token-kind": func(_ string, out map[string]any) {
			if _, ok := out["token"]; ok {
				out["token"] = "unexpected-token-canary"
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newAppAuthFixture(t)
			f.mutate = mutate
			token, _, err := f.auth.credential(context.Background())
			if err == nil || token != "" || f.auth.token != "" {
				t.Fatal("unsafe response yielded a usable credential")
			}
			for _, secret := range f.auth.secretValues() {
				if strings.Contains(err.Error(), secret) {
					t.Fatal("error exposed an authentication secret")
				}
			}
		})
	}
}

func TestGitHubAppAuthenticationErrorsDoNotEchoResponses(t *testing.T) {
	f := newAppAuthFixture(t)
	f.failPath = "/app"
	_, _, err := f.auth.credential(context.Background())
	if err == nil || !strings.Contains(err.Error(), "HTTP 403") {
		t.Fatalf("expected bounded status error, got %v", err)
	}
	for _, secret := range f.auth.secretValues() {
		if strings.Contains(err.Error(), secret) {
			t.Fatal("upstream authentication response disclosed a secret")
		}
	}
}

func TestGitHubAppMintRejectsMissingNonpositiveDuplicateRepositoryIDs(t *testing.T) {
	for name, repositories := range map[string][]map[string]any{
		"missing-id":   {{"name": "widgets"}},
		"zero-id":      {{"name": "widgets", "id": int64(0)}},
		"negative-id":  {{"name": "widgets", "id": int64(-1)}},
		"duplicate-id": {{"name": "widgets", "id": int64(1)}, {"name": "services", "id": int64(1)}},
	} {
		t.Run(name, func(t *testing.T) {
			f := newAppAuthFixture(t)
			f.mutate = func(_ string, out map[string]any) {
				if _, ok := out["repositories"]; ok {
					out["repositories"] = repositories
				}
			}
			token, _, err := f.auth.credential(context.Background())
			if err == nil || !strings.Contains(err.Error(), "invalid repository identity") {
				t.Fatalf("mint did not fail at repository-identity validation: %v", err)
			}
			if token != "" || f.auth.token != "" || len(f.auth.repositoryIDs) != 0 {
				t.Fatal("rejected repository identities left usable credential state")
			}
		})
	}
}

func TestGitHubAppConnectionSecretValuesScrubPEMLinesAndRenewedTokens(t *testing.T) {
	f := newAppAuthFixture(t)
	document := appTestDocument(t, f.credential)
	first, _, err := f.auth.credential(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(56 * time.Minute)
	second, _, err := f.auth.credential(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("fixture did not issue distinct credentials")
	}
	hash := sha256.Sum256([]byte(document))
	key := hex.EncodeToString(hash[:])
	githubAppCache.Lock()
	previous, present := githubAppCache.entries[key]
	githubAppCache.entries[key] = f.auth
	githubAppCache.Unlock()
	t.Cleanup(func() {
		githubAppCache.Lock()
		defer githubAppCache.Unlock()
		if present {
			githubAppCache.entries[key] = previous
		} else {
			delete(githubAppCache.entries, key)
		}
	})
	before := f.mints.Load()
	values := ConnectionSecretValues(document)
	if f.mints.Load() != before {
		t.Fatal("evidence scrubbing initiated a network token mint")
	}
	for _, secret := range []string{document, f.credential.PrivateKey, first, second} {
		if !containsAppTestSecret(values, secret) {
			t.Fatal("connection scrubber omitted a complete secret")
		}
	}
	redact := newRedactor(values...)
	for _, secret := range append([]string{document, f.credential.PrivateKey, first, second}, strings.Split(f.credential.PrivateKey, "\n")...) {
		if len(secret) < 16 {
			continue
		}
		if !containsAppTestSecret(values, secret) {
			t.Fatal("connection scrubber omitted a PEM line")
		}
		if got := redact.string("before " + secret + " after"); got != "before "+redactPlaceholder+" after" {
			t.Fatal("connection evidence retained a private key, key fragment or minted token")
		}
	}
	pat := "classic-connection-secret-canary"
	if got := ConnectionSecretValues(pat); !reflect.DeepEqual(got, []string{pat}) {
		t.Fatal("PAT evidence scrubbing changed")
	}
}

type appTestRoundTripper func(*http.Request) (*http.Response, error)

func (f appTestRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestGitHubAppTransportPinsOriginAndPreservesRequest(t *testing.T) {
	f := newAppAuthFixture(t)
	called := false
	transport := githubAppTransport{auth: f.auth, base: appTestRoundTripper(func(r *http.Request) (*http.Response, error) {
		called = true
		if r.Header.Get("Authorization") != "Bearer "+f.auth.token {
			t.Error("transport did not attach installation credential")
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("{}")), Header: make(http.Header)}, nil
	})}
	bad, _ := http.NewRequest(http.MethodGet, "https://unrelated.example/app", nil)
	if _, err := transport.RoundTrip(bad); err == nil || called || f.mints.Load() != 0 {
		t.Fatal("off-origin request authenticated or reached underlying transport")
	}
	req, _ := http.NewRequest(http.MethodGet, f.auth.apiURL+"/repositories/1", nil)
	req.Header.Set("Authorization", "original-placeholder")
	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if !called || req.Header.Get("Authorization") != "original-placeholder" {
		t.Fatal("transport mutated caller request")
	}
}

func TestResolveGitCredentialPreservesPATAndRejectsAppDestination(t *testing.T) {
	pat := "classic-credential-canary"
	for _, kind := range []Type{TypeGitHub, TypeGitLab, TypeForgejo} {
		got, expires, err := ResolveGitCredentialWithExpiry(context.Background(), kind, "https://forge.example", pat, time.Second)
		if err != nil || got != pat || !expires.IsZero() {
			t.Fatalf("PAT path changed for %s: %v", kind, err)
		}
	}
	c, _ := appTestCredential(t)
	for _, kind := range []Type{TypeGitLab, TypeForgejo, TypeGitHub} {
		for _, base := range []string{"https://unrelated.example", "http://github.com"} {
			got, _, err := ResolveGitCredentialWithExpiry(context.Background(), kind, base, appTestDocument(t, c), time.Second)
			if err == nil || got != "" {
				t.Fatalf("App credential allowed at %s %s", kind, base)
			}
		}
	}
}

func TestAppDocumentsCannotEnterOtherForgeTransports(t *testing.T) {
	for _, kind := range []Type{TypeGitLab, TypeForgejo} {
		client, err := New(kind, "https://github.com", `{"auth_type":"github_app","private_key":"private-key-canary"}`, time.Second)
		if err == nil || client != nil {
			t.Fatal("App key document entered a PAT-only transport")
		}
	}
}
