package middleware

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/auth"
	"github.com/vtmocanu/uzi/api/internal/clitoken"
	"github.com/vtmocanu/uzi/api/internal/config"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// fakeAuthStore is a configurable SessionStore + CLIUserStore.
type fakeAuthStore struct {
	user     store.User
	userErr  error
	row      store.CliToken
	tokenErr error
}

func (f fakeAuthStore) GetUserByID(context.Context, uuid.UUID) (store.User, error) {
	return f.user, f.userErr
}

func (f fakeAuthStore) GetCLITokenByHash(context.Context, []byte) (store.CliToken, error) {
	return f.row, f.tokenErr
}

func (f fakeAuthStore) TouchCLIToken(context.Context, store.TouchCLITokenParams) error { return nil }

var (
	errConnRefused   = errors.New("dial tcp 10.0.0.1:5432: connect: connection refused")
	errWrappedNoRows = fmt.Errorf("lookup: %w", pgx.ErrNoRows)
)

func authTestCfg() config.Config {
	return config.Config{JWTSecret: []byte("test-secret-test-secret-test-secret"), AuthTokenTTL: time.Hour}
}

// sessionRequest builds a GET (CSRF-exempt) carrying a valid session cookie.
func sessionRequest(t *testing.T, ctx context.Context, userID uuid.UUID) (*http.Request, string) {
	t.Helper()
	tok, err := auth.IssueToken(authTestCfg().JWTSecret, userID.String(), 0, time.Hour)
	if err != nil {
		t.Fatalf("IssueToken: %v", err)
	}
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/x", nil)
	req.AddCookie(&http.Cookie{Name: auth.AuthCookieName, Value: tok}) //nolint:gosec // G124: test-only client cookie on an httptest request.
	return req, tok
}

func bearerRequest(ctx context.Context, tok string) *http.Request {
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/x", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	return req
}

// run serves req through h and reports the status and whether next was reached.
func run(h func(http.Handler) http.Handler, req *http.Request) (*httptest.ResponseRecorder, bool) {
	reached := false
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { reached = true })
	rec := httptest.NewRecorder()
	h(next).ServeHTTP(rec, req)
	return rec, reached
}

func TestRequireAuthUserLookupFailure(t *testing.T) {
	uid := uuid.New()
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"store error is 503", errConnRefused, http.StatusServiceUnavailable},
		{"wrapped no rows is 401", errWrappedNoRows, http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := sessionRequest(t, context.Background(), uid)
			rec, reached := run(RequireAuth(fakeAuthStore{userErr: tc.err}, authTestCfg()), req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}
			if reached {
				t.Fatal("next handler reached on lookup failure")
			}
			if strings.Contains(rec.Body.String(), "refused") {
				t.Fatalf("body echoes store error: %q", rec.Body.String())
			}
		})
	}
}

func TestRequireUserBearerLookupFailures(t *testing.T) {
	tok, hash, _, err := clitoken.Generate(clitoken.ScopeUser)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	okRow := store.CliToken{ID: uuid.New(), UserID: uuid.New(), TokenHash: hash, Scope: clitoken.ScopeUser}
	otherRow := okRow
	otherRow.TokenHash = clitoken.Hash(tok + "x")

	cases := []struct {
		name string
		st   fakeAuthStore
		want int
	}{
		{"token lookup store error", fakeAuthStore{tokenErr: errConnRefused}, http.StatusServiceUnavailable},
		{"token lookup no rows", fakeAuthStore{tokenErr: errWrappedNoRows}, http.StatusUnauthorized},
		{"user lookup store error", fakeAuthStore{row: okRow, userErr: errConnRefused}, http.StatusServiceUnavailable},
		{"user lookup no rows", fakeAuthStore{row: okRow, userErr: errWrappedNoRows}, http.StatusUnauthorized},
		{"hash mismatch", fakeAuthStore{row: otherRow}, http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec, reached := run(RequireUser(tc.st, authTestCfg()), bearerRequest(context.Background(), tok))
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}
			if reached {
				t.Fatal("next handler reached on lookup failure")
			}
			if strings.Contains(rec.Body.String(), "refused") {
				t.Fatalf("body echoes store error: %q", rec.Body.String())
			}
		})
	}
}

func TestAuthLookupFailureLogsOnlyForLiveRequests(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	dialTimeout := fmt.Errorf("dial tcp: i/o timeout: %w", context.DeadlineExceeded)
	uid := uuid.New()
	cliTok, hash, _, err := clitoken.Generate(clitoken.ScopeUser)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	row := store.CliToken{ID: uuid.New(), UserID: uid, TokenHash: hash}

	type target struct {
		name    string
		handler func(http.Handler) http.Handler
		request func(ctx context.Context) *http.Request
		event   string
		secret  string
	}
	_, sessionTok := sessionRequest(t, context.Background(), uid)
	targets := []target{
		{
			"session", RequireAuth(fakeAuthStore{userErr: dialTimeout}, authTestCfg()),
			func(ctx context.Context) *http.Request { r, _ := sessionRequest(t, ctx, uid); return r },
			"session auth: user lookup failed", sessionTok,
		},
		{
			"cli token lookup", RequireUser(fakeAuthStore{tokenErr: dialTimeout}, authTestCfg()),
			func(ctx context.Context) *http.Request { return bearerRequest(ctx, cliTok) },
			"cli auth: token lookup failed", cliTok,
		},
		{
			"cli user lookup", RequireUser(fakeAuthStore{row: row, userErr: dialTimeout}, authTestCfg()),
			func(ctx context.Context) *http.Request { return bearerRequest(ctx, cliTok) },
			"cli auth: user lookup failed", cliTok,
		},
	}
	for _, tg := range targets {
		t.Run(tg.name, func(t *testing.T) {
			buf.Reset()
			rec, _ := run(tg.handler, tg.request(context.Background()))
			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("live request: status = %d, want 503", rec.Code)
			}
			if !strings.Contains(buf.String(), tg.event) {
				t.Fatalf("live request: store failure not logged; log = %q", buf.String())
			}
			if strings.Contains(buf.String(), tg.secret) || strings.Contains(buf.String(), string(hash)) {
				t.Fatalf("log carries credential material: %q", buf.String())
			}

			buf.Reset()
			cancelled, cancel := context.WithCancel(context.Background())
			cancel()
			rec, _ = run(tg.handler, tg.request(cancelled))
			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("cancelled request: status = %d, want 503", rec.Code)
			}
			if buf.Len() != 0 {
				t.Fatalf("cancelled request: logged %q, want nothing", buf.String())
			}
		})
	}
}
