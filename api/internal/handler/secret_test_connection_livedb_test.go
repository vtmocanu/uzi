package handler

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/auth"
	"github.com/vtmocanu/uzi/api/internal/store"
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
// its own context. The provider call drives the probe deadline after it starts; the
// credential never changes, so the verdict must stay inconclusive/generic. With the
// re-read on the expired probe context it failed and read as superseded.
func TestSecretConnectionSpentBudgetIsNotSupersededLiveDB(t *testing.T) {
	h, router, pool := cliLiveDB(t)
	owner := cliSeedUser(t, pool, false)
	session := cliMintJWT(t, pool, owner)
	id := seedTestConnection(t, h, pool, owner, "openai_api_key", "sk-"+"test-openai-"+uuid.NewString())
	var parent context.Context
	var expire context.CancelFunc
	var expiredAt time.Time
	var reread bool
	h.secretTestProbeContext = func(ctx context.Context, budget time.Duration) (context.Context, context.CancelFunc) {
		if budget != 12*time.Second {
			t.Fatalf("probe budget = %s, want 12s", budget)
		}
		parent = ctx
		child, cancel := context.WithCancel(ctx)
		expire = cancel
		return expiredSecretProbeContext{Context: child, deadline: time.Now().Add(budget)}, cancel
	}
	h.q = store.New(secretRereadObserver{DBTX: pool, observe: func(ctx context.Context, sql string) {
		if expiredAt.IsZero() || !strings.HasPrefix(sql, "-- name: GetSecretEnablement ") {
			return
		}
		reread = true
		if parent.Err() != nil || ctx.Err() != nil {
			t.Errorf("final read must retain a live parent and independent live context: parent=%v read=%v", parent.Err(), ctx.Err())
		}
		deadline, ok := ctx.Deadline()
		if !ok || deadline.Before(expiredAt.Add(2*time.Second)) || deadline.After(time.Now().Add(2*time.Second)) {
			t.Errorf("final read deadline %v (present=%v) must be a fresh 2s budget", deadline, ok)
		}
	}})
	installTestTransport(h, func(r *http.Request) (*http.Response, error) {
		if parent == nil || expire == nil {
			t.Fatal("probe deadline arm was not invoked")
		}
		if parent.Err() != nil || r.Context().Err() != nil {
			t.Fatal("provider must enter before child expiry, with live parent and probe")
		}
		expiredAt = time.Now()
		expire()
		<-r.Context().Done()
		if r.Context().Err() != context.DeadlineExceeded || parent.Err() != nil {
			t.Fatal("only the probe deadline must expire")
		}
		return nil, r.Context().Err()
	})
	requireTestResult(t, cookieReq(t, router, http.MethodPost, testConnectionPath("openai_api_key", id), session, ""),
		http.StatusOK, "inconclusive", "generic")
	if !reread {
		t.Fatal("final credential re-read was not observed")
	}
}

type expiredSecretProbeContext struct {
	context.Context
	deadline time.Time
}

func (c expiredSecretProbeContext) Deadline() (time.Time, bool) { return c.deadline, true }

func (c expiredSecretProbeContext) Err() error {
	if c.Context.Err() != nil {
		return context.DeadlineExceeded
	}
	return nil
}

type secretRereadObserver struct {
	store.DBTX
	observe func(context.Context, string)
}

func (o secretRereadObserver) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	o.observe(ctx, sql)
	return o.DBTX.QueryRow(ctx, sql, args...)
}
