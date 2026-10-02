package handler

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/auth"
	"github.com/vtmocanu/uzi/api/internal/vault"
)

const liveModels = `{"object":"list","data":[{"id":"gpt-4o","object":"model"}]}`

func seedTestConnection(t *testing.T, h *Handler, pool *pgxpool.Pool, owner uuid.UUID, kind, plain string) uuid.UUID {
	t.Helper()
	sealed, err := h.box.Seal([]byte(plain))
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.New()
	cliMustExec(t, pool, `INSERT INTO user_secrets
		(id, user_id, kind, label, ciphertext, sealed_with)
		VALUES ($1, $2, $3, $4, $5, 'master')`, id, owner, kind, "test-"+id.String(), sealed)
	return id
}

func testConnectionPath(kind string, id uuid.UUID) string {
	return "/api/me/secrets/" + kind + "/" + id.String() + "/test"
}

func installTestTransport(h *Handler, fn func(*http.Request) (*http.Response, error)) {
	h.secretTestOnce.Do(func() {})
	h.secretTestClients = &secretTestClients{openai: &http.Client{Transport: secretRoundTrip(fn)}}
}

func modelsResponse(code int, body string) (*http.Response, error) {
	return &http.Response{StatusCode: code, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
}

func requireTestResult(t *testing.T, rec *httptest.ResponseRecorder, code int, status, reason string) {
	t.Helper()
	if rec.Code != code {
		t.Fatalf("HTTP %d, want %d: %s", rec.Code, code, rec.Body.String())
	}
	if status != "" && (!strings.Contains(rec.Body.String(), `"status":"`+status+`"`) ||
		reason != "" && !strings.Contains(rec.Body.String(), `"reason":"`+reason+`"`)) {
		t.Fatalf("body = %s; want status=%q reason=%q", rec.Body.String(), status, reason)
	}
}

func TestSecretConnectionRouterAuthAndPrivacyLiveDB(t *testing.T) {
	h, router, pool := cliLiveDB(t)
	owner, other := cliSeedUser(t, pool, false), cliSeedUser(t, pool, false)
	session := cliMintJWT(t, pool, owner)
	token := cliMintToken(t, pool, owner, "user")
	key := "sk-" + "test-openai-" + uuid.NewString()
	id := seedTestConnection(t, h, pool, owner, "openai_api_key", key)
	foreign := seedTestConnection(t, h, pool, other, "openai_api_key", key)
	var calls atomic.Int32
	providerBody := "provider-private-" + key
	installTestTransport(h, func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if r.Method != http.MethodGet || r.URL.String() != "https://api.openai.com/v1/models" ||
			r.Header.Get("Authorization") != "Bearer "+key {
			t.Errorf("unexpected provider request: %s %s", r.Method, r.URL)
		}
		return modelsResponse(http.StatusOK, liveModels)
	})
	path := testConnectionPath("openai_api_key", id)
	for _, tc := range []struct {
		name string
		req  func() *httptest.ResponseRecorder
	}{
		{"cookie with CSRF", func() *httptest.ResponseRecorder { return cookieReq(t, router, http.MethodPost, path, session, "") }},
		{"uzc bearer", func() *httptest.ResponseRecorder { return bearerReqBody(router, http.MethodPost, path, token, "") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requireTestResult(t, tc.req(), http.StatusOK, "ok", "")
		})
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("provider calls = %d, want 2", got)
	}

	withoutCSRF := httptest.NewRequest(http.MethodPost, path, nil)
	withoutCSRF.AddCookie(&http.Cookie{Name: auth.AuthCookieName, Value: session, Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, withoutCSRF)
	if rec.Code != http.StatusForbidden {
		t.Errorf("cookie without CSRF = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := bearerReqBody(router, http.MethodPost, path, "uzc_invalid", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("invalid bearer = %d: %s", rec.Code, rec.Body.String())
	}
	for _, p := range []string{
		testConnectionPath("openai_api_key", foreign),
		testConnectionPath("openai_api_key", uuid.New()),
		testConnectionPath("anthropic_token", id),
	} {
		if rec := cookieReq(t, router, http.MethodPost, p, session, ""); rec.Code != http.StatusNotFound {
			t.Errorf("%s = %d, want 404: %s", p, rec.Code, rec.Body.String())
		}
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("invalid access made %d provider calls, want 2", got)
	}

	// A provider error can include credential text; the API must emit only fixed vocabulary.
	installTestTransport(h, func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return modelsResponse(http.StatusUnauthorized, providerBody)
	})
	rec = cookieReq(t, router, http.MethodPost, path, session, "")
	requireTestResult(t, rec, http.StatusOK, "rejected", "")
	if strings.Contains(rec.Body.String(), key) || strings.Contains(rec.Body.String(), providerBody) {
		t.Fatalf("provider response leaked: %s", rec.Body.String())
	}
}

func TestSecretConnectionDisabledAndVaultLockedLiveDB(t *testing.T) {
	h, router, pool := cliLiveDB(t)
	owner := cliSeedUser(t, pool, false)
	session := cliMintJWT(t, pool, owner)
	var calls atomic.Int32
	installTestTransport(h, func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return modelsResponse(http.StatusOK, liveModels)
	})
	disabled := seedTestConnection(t, h, pool, owner, "openai_api_key", "fixture-disabled")
	cliMustExec(t, pool, `UPDATE user_secrets SET disabled_at = now() WHERE id = $1`, disabled)
	requireTestResult(t, cookieReq(t, router, http.MethodPost, testConnectionPath("openai_api_key", disabled), session, ""),
		http.StatusOK, "inconclusive", "generic")

	locked := seedTestConnection(t, h, pool, owner, "openai_api_key", "fixture-locked")
	// A DEK-sealed row requires an unlocked user vault before any provider request.
	cliMustExec(t, pool, `UPDATE user_secrets SET sealed_with = 'dek' WHERE id = $1`, locked)
	h.vault = vault.New(h.box, nil)
	requireTestResult(t, cookieReq(t, router, http.MethodPost, testConnectionPath("openai_api_key", locked), session, ""),
		http.StatusOK, "inconclusive", "vault_locked")
	if got := calls.Load(); got != 0 {
		t.Fatalf("disabled/locked secrets made %d provider calls", got)
	}
}

func TestSecretConnectionReplacementSupersedesProbeLiveDB(t *testing.T) {
	h, router, pool := cliLiveDB(t)
	owner := cliSeedUser(t, pool, false)
	session := cliMintJWT(t, pool, owner)
	id := seedTestConnection(t, h, pool, owner, "openai_api_key", "fixture-before")
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	installTestTransport(h, func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		close(entered)
		<-release
		return modelsResponse(http.StatusOK, liveModels)
	})
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- cookieReq(t, router, http.MethodPost, testConnectionPath("openai_api_key", id), session, "")
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("provider call did not start")
	}
	replacement, err := h.box.Seal([]byte("fixture-after"))
	if err != nil {
		close(release)
		t.Fatal(err)
	}
	cliMustExec(t, pool, `UPDATE user_secrets SET ciphertext = $1, updated_at = updated_at + interval '1 second'
		WHERE id = $2`, replacement, id)
	close(release)
	select {
	case rec := <-done:
		requireTestResult(t, rec, http.StatusOK, "inconclusive", "superseded")
	case <-time.After(5 * time.Second):
		t.Fatal("probe did not return")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("provider calls = %d, want 1", got)
	}
}

// TestSecretConnectionSpentBudgetIsNotSupersededLiveDB pins the final re-read to
// its own context. The provider call runs until the 12s probe budget expires; the
// credential never changes, so the verdict must stay inconclusive/generic. With the
// re-read on the expired probe context it failed and read as superseded.
func TestSecretConnectionSpentBudgetIsNotSupersededLiveDB(t *testing.T) {
	h, router, pool := cliLiveDB(t)
	owner := cliSeedUser(t, pool, false)
	session := cliMintJWT(t, pool, owner)
	id := seedTestConnection(t, h, pool, owner, "openai_api_key", "sk-"+"test-openai-"+uuid.NewString())
	installTestTransport(h, func(r *http.Request) (*http.Response, error) {
		<-r.Context().Done()
		return nil, r.Context().Err()
	})
	requireTestResult(t, cookieReq(t, router, http.MethodPost, testConnectionPath("openai_api_key", id), session, ""),
		http.StatusOK, "inconclusive", "generic")
}
