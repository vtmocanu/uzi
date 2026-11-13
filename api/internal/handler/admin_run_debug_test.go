package handler

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/auth"
	"github.com/vtmocanu/uzi/api/internal/clitoken"
	"github.com/vtmocanu/uzi/api/internal/config"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// adminDebugFakeStore is the minimal middleware.CLIUserStore for driving the real
// RequireUser, so the credential reaches the handler exactly as in production.
type adminDebugFakeStore struct {
	user store.User
	row  store.CliToken
}

func (f adminDebugFakeStore) GetUserByID(context.Context, uuid.UUID) (store.User, error) {
	return f.user, nil
}

func (f adminDebugFakeStore) GetCLITokenByHash(context.Context, []byte) (store.CliToken, error) {
	return f.row, nil
}

func (f adminDebugFakeStore) TouchCLIToken(context.Context, store.TouchCLITokenParams) error {
	return nil
}

func adminDebugCfg(on bool) config.Config {
	return config.Config{
		JWTSecret:     []byte("test-secret-test-secret-test-secret"),
		AuthTokenTTL:  time.Hour,
		AdminRunDebug: on,
	}
}

// adminDebugActorVia runs the real RequireUser and returns what adminRunDebugActor
// reports for the request it lets through.
func adminDebugActorVia(t *testing.T, h *Handler, st adminDebugFakeStore, req *http.Request) (store.User, uuid.UUID, bool) {
	t.Helper()
	var (
		u       store.User
		id      uuid.UUID
		ok      bool
		reached bool
	)
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		reached = true
		u, id, ok = h.adminRunDebugActor(r)
	})
	mw.RequireUser(st, adminDebugCfg(true))(next).ServeHTTP(httptest.NewRecorder(), req)
	if !reached {
		t.Fatal("RequireUser rejected the request")
	}
	return u, id, ok
}

func TestAdminRunDebugActor(t *testing.T) {
	cases := []struct {
		name    string
		flag    bool
		scope   string
		isAdmin bool
		cookie  bool
		want    bool
	}{
		{"all conditions met", true, clitoken.ScopeAdminRO, true, false, true},
		{"flag off", false, clitoken.ScopeAdminRO, true, false, false},
		{"uzc token of an admin", true, clitoken.ScopeUser, true, false, false},
		{"demoted admin_ro token", true, clitoken.ScopeAdminRO, false, false, false},
		{"cookie session of an admin", true, "", true, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			uid := uuid.New()
			st := adminDebugFakeStore{user: store.User{ID: uid, IsActive: true, IsAdmin: tc.isAdmin}}
			var req *http.Request
			if tc.cookie {
				tok, err := auth.IssueToken(adminDebugCfg(true).JWTSecret, uid.String(), 0, time.Hour)
				if err != nil {
					t.Fatalf("IssueToken: %v", err)
				}
				req = httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/x", nil)
				req.AddCookie(&http.Cookie{Name: auth.AuthCookieName, Value: tok}) //nolint:gosec // G124: test-only client cookie.
			} else {
				tok, hash, _, err := clitoken.Generate(tc.scope)
				if err != nil {
					t.Fatalf("Generate: %v", err)
				}
				st.row = store.CliToken{ID: uuid.New(), UserID: uid, TokenHash: hash, Scope: tc.scope}
				req = httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/x", nil)
				req.Header.Set("Authorization", "Bearer "+tok)
			}
			h := &Handler{cfg: adminDebugCfg(tc.flag)}
			user, tokenID, ok := adminDebugActorVia(t, h, st, req)
			if ok != tc.want {
				t.Fatalf("ok = %v, want %v", ok, tc.want)
			}
			if tc.want {
				if user.ID != uid || tokenID != st.row.ID {
					t.Fatalf("actor = %s token = %s, want %s / %s", user.ID, tokenID, uid, st.row.ID)
				}
			} else if user.ID != uuid.Nil || tokenID != uuid.Nil {
				t.Fatalf("denied actor must be zero, got %s / %s", user.ID, tokenID)
			}
		})
	}
}

func TestAdminRunDebugAllowed(t *testing.T) {
	adminRO := mw.CLICredential{TokenID: uuid.New(), Scope: clitoken.ScopeAdminRO}
	userScope := mw.CLICredential{TokenID: uuid.New(), Scope: clitoken.ScopeUser}
	admin := store.User{ID: uuid.New(), IsAdmin: true}
	nonAdmin := store.User{ID: uuid.New()}
	cases := []struct {
		name   string
		flag   bool
		cred   mw.CLICredential
		credOK bool
		user   store.User
		userOK bool
		want   bool
	}{
		{"all conditions met", true, adminRO, true, admin, true, true},
		{"flag off", false, adminRO, true, admin, true, false},
		{"no credential", true, adminRO, false, admin, true, false},
		{"stored scope user, admin user", true, userScope, true, admin, true, false},
		{"no user", true, adminRO, true, admin, false, false},
		{"non-admin user", true, adminRO, true, nonAdmin, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := adminRunDebugAllowed(tc.flag, tc.cred, tc.credOK, tc.user, tc.userOK); got != tc.want {
				t.Fatalf("adminRunDebugAllowed = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAdminRunDebugActorNoCredential(t *testing.T) {
	h := &Handler{cfg: adminDebugCfg(true)}
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/x", nil)
	req = req.WithContext(mw.ContextWithUser(req.Context(), store.User{ID: uuid.New(), IsAdmin: true}))
	if _, _, ok := h.adminRunDebugActor(req); ok {
		t.Fatal("admin user without a CLI credential must not qualify")
	}
}

type captureHandler struct {
	mu   sync.Mutex
	recs []slog.Record
}

func (c *captureHandler) Enabled(context.Context, slog.Level) bool { return true }
func (c *captureHandler) Handle(_ context.Context, r slog.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.recs = append(c.recs, r.Clone())
	return nil
}
func (c *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *captureHandler) WithGroup(string) slog.Handler      { return c }

func captureAdminRunDebug(t *testing.T) *captureHandler {
	t.Helper()
	c := &captureHandler{}
	prev := adminRunDebugLogger
	adminRunDebugLogger = func() *slog.Logger { return slog.New(c) }
	t.Cleanup(func() { adminRunDebugLogger = prev })
	return c
}

func recordAttrs(r slog.Record) map[string]any {
	m := map[string]any{}
	r.Attrs(func(a slog.Attr) bool {
		m[a.Key] = a.Value.Any()
		return true
	})
	return m
}

func requireOneAudit(t *testing.T, c *captureHandler) map[string]any {
	t.Helper()
	if len(c.recs) != 1 {
		t.Fatalf("records = %d, want exactly 1", len(c.recs))
	}
	r := c.recs[0]
	if r.Level != slog.LevelInfo || r.Message != "admin run debug read" {
		t.Fatalf("record = %v %q", r.Level, r.Message)
	}
	return recordAttrs(r)
}

func TestAuditRunDebugRead(t *testing.T) {
	c := captureAdminRunDebug(t)
	actor, tok, run, owner := store.User{ID: uuid.New()}, uuid.New(), uuid.New(), uuid.New()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/x", nil)
	auditRunDebugRead(req, actor, tok, adminRunDebugRouteInputs, run, owner)
	got := requireOneAudit(t, c)
	want := map[string]any{
		"actor_user_id": actor.ID.String(), "token_id": tok.String(), "route": "runs.inputs",
		"run_id": run.String(), "owner_id": owner.String(),
	}
	assertAttrs(t, got, want)
}

func TestAuditHoldsDebugRead(t *testing.T) {
	c := captureAdminRunDebug(t)
	actor, tok, owner := store.User{ID: uuid.New()}, uuid.New(), uuid.New()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/x", nil)
	auditHoldsDebugRead(req, actor, tok, adminRunDebugRouteHolds, owner)
	got := requireOneAudit(t, c)
	assertAttrs(t, got, map[string]any{
		"actor_user_id": actor.ID.String(), "token_id": tok.String(), "route": "recovery.holds",
		"target_owner_id": owner.String(),
	})
}

func TestAuditHoldsRunDebugRead(t *testing.T) {
	c := captureAdminRunDebug(t)
	actor, tok, owner, run := store.User{ID: uuid.New()}, uuid.New(), uuid.New(), uuid.New()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/x", nil)
	auditHoldsRunDebugRead(req, actor, tok, adminRunDebugRouteHolds, owner, run)
	got := requireOneAudit(t, c)
	assertAttrs(t, got, map[string]any{
		"actor_user_id": actor.ID.String(), "token_id": tok.String(), "route": "recovery.holds",
		"target_owner_id": owner.String(), "run_id": run.String(),
	})
}

func TestAuditRunsListDebugRead(t *testing.T) {
	actor, tok := store.User{ID: uuid.New()}, uuid.New()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/x", nil)

	t.Run("all owners no since", func(t *testing.T) {
		c := captureAdminRunDebug(t)
		auditRunsListDebugRead(req, actor, tok, adminRunDebugRouteRuns, nil, nil, 7)
		assertAttrs(t, requireOneAudit(t, c), map[string]any{
			"actor_user_id": actor.ID.String(), "token_id": tok.String(), "route": "admin.runs",
			"owner_filter": "all", "returned": int64(7),
		})
	})
	t.Run("owner filter and since", func(t *testing.T) {
		c := captureAdminRunDebug(t)
		owner := uuid.New()
		since := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
		auditRunsListDebugRead(req, actor, tok, adminRunDebugRouteRuns, &owner, &since, 0)
		assertAttrs(t, requireOneAudit(t, c), map[string]any{
			"actor_user_id": actor.ID.String(), "token_id": tok.String(), "route": "admin.runs",
			"owner_filter": owner.String(), "since": "2026-01-02T03:04:05Z", "returned": int64(0),
		})
	})
}

func assertAttrs(t *testing.T, got, want map[string]any) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("attrs = %v, want %v", got, want)
	}
	for k, w := range want {
		if got[k] != w {
			t.Fatalf("attr %s = %v (%T), want %v (%T)", k, got[k], got[k], w, w)
		}
	}
}
