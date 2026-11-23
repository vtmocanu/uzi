package handler

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/providers"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Router-level coverage for POST /api/me/providers/{id}/test (PRD #4320 section 8), against a
// loopback fake gateway (the dev loopback flag on) and the real vault, queries and router. Skipped
// unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.

const (
	probeOKMessage   = `{"type":"message","role":"assistant","content":[{"type":"text","text":"hi"}],"stop_reason":"max_tokens"}`
	probeToolMessage = `{"type":"message","role":"assistant","content":[{"type":"tool_use","id":"t","name":"uzi_probe","input":{"ok":true}}],"stop_reason":"tool_use"}`
)

type fakeProviderGateway struct {
	srv  *httptest.Server
	mu   sync.Mutex
	mode string // "ok" | "unauthorized"
	auth []string
	// rejectAuth lists Authorization header values answered 401 whatever the mode (per-key outcomes).
	rejectAuth map[string]bool
	// onRequest, when set, runs on the gateway goroutine after the request is recorded and before it
	// is answered: the "between the dial and the evidence write" interleaving point.
	onRequest func()
}

func newFakeProviderGateway(t *testing.T) *fakeProviderGateway {
	t.Helper()
	g := &fakeProviderGateway{mode: "ok"}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
		g.mu.Lock()
		g.auth = append(g.auth, r.Header.Get("Authorization"))
		mode := g.mode
		if g.rejectAuth[r.Header.Get("Authorization")] {
			mode = "unauthorized"
		}
		hook := g.onRequest
		g.mu.Unlock()
		if hook != nil {
			hook()
		}
		if mode == "unauthorized" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(string(body), `"tools"`) {
			_, _ = w.Write([]byte(probeToolMessage))
			return
		}
		_, _ = w.Write([]byte(probeOKMessage))
	}))
	t.Cleanup(g.srv.Close)
	return g
}

func (g *fakeProviderGateway) setMode(m string) {
	g.mu.Lock()
	g.mode = m
	g.mu.Unlock()
}

func (g *fakeProviderGateway) seenAuth() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.auth...)
}

// probeFixture is a provider fixture whose allowlist is a loopback gateway.
func probeFixture(t *testing.T) (providerFixture, *fakeProviderGateway, string) {
	t.Helper()
	gw := newFakeProviderGateway(t)
	base := gw.srv.URL + "/anthropic"
	f := newProviderFixture(t, base)
	f.h.cfg.ProviderAllowLoopback = true
	body := strings.Replace(validCreateBody, testGatewayURL, base, 1)
	return f, gw, body
}

// Interleave a committed edit/rotation after the probe's endpoint snapshot but
// before its ciphertext read. The production handler has no test-only hook.
type providerProbeOpenInterleave struct {
	store.DBTX
	once   sync.Once
	before func()
}

func (db *providerProbeOpenInterleave) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if strings.Contains(sql, "-- name: GetUserSecretCiphertextByID ") {
		db.once.Do(db.before)
	}
	return db.DBTX.QueryRow(ctx, sql, args...)
}

func TestProviderProbeRefusesChangedSnapshotBeforeSendingLiveDB(t *testing.T) {
	for _, change := range []string{"endpoint and key", "key only", "endpoint only", "disable key"} {
		t.Run(change, func(t *testing.T) {
			f, original, createBody := probeFixture(t)
			replacement := newFakeProviderGateway(t)
			base := replacement.srv.URL + "/anthropic"
			f.h.cfg.ProviderAllowedBaseURLs = append(f.h.cfg.ProviderAllowedBaseURLs, base)
			p := f.create(t, createBody)
			path := "/api/me/providers/" + p.ID
			putKey := func(key string) {
				t.Helper()
				rec := cookieReq(t, f.router, http.MethodPut, path+"/key", f.jwt, `{"key":"`+key+`"}`)
				if rec.Code != http.StatusOK {
					t.Fatalf("rotate key = %d %s", rec.Code, rec.Body.String())
				}
			}
			putKey(testProviderKey)
			interleaved := false
			hook := &providerProbeOpenInterleave{DBTX: f.pool, before: func() {
				interleaved = true
				if change == "endpoint and key" || change == "endpoint only" {
					rec := cookieReq(t, f.router, http.MethodPatch, path+"/endpoint", f.jwt,
						`{"base_url":"`+base+`","auth_style":"bearer"}`)
					if rec.Code != http.StatusOK {
						t.Fatalf("edit endpoint = %d %s", rec.Code, rec.Body.String())
					}
				}
				if change == "endpoint and key" || change == "key only" {
					putKey("replacement-gateway-key-0002")
				}
				if change == "disable key" {
					rec := cookieReq(t, f.router, http.MethodPatch, path+"/key/enabled", f.jwt, `{"enabled":false}`)
					if rec.Code != http.StatusOK {
						t.Fatalf("disable key = %d %s", rec.Code, rec.Body.String())
					}
				}
			}}
			f.h.q = store.New(hook)
			rec := bearerReqBody(f.router, http.MethodPost, path+"/test", f.uzc, "")
			if !interleaved {
				t.Fatal("the probe never reached the key-open interleaving")
			}
			if rec.Code != http.StatusConflict {
				t.Errorf("probe = %d %s, want 409", rec.Code, rec.Body.String())
			}
			if n := len(original.seenAuth()) + len(replacement.seenAuth()); n != 0 {
				t.Errorf("gateway received %d request(s) after the snapshot changed; want none", n)
			}
		})
	}
}

func TestProviderTestProbeLiveDB(t *testing.T) {
	f, gw, createBody := probeFixture(t)
	p := f.create(t, createBody)
	test := "/api/me/providers/" + p.ID + "/test"

	// No key yet: 404 like a missing key, and nothing reaches the gateway.
	if rec := bearerReqBody(f.router, http.MethodPost, test, f.uzc, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("keyless test = %d %s, want 404", rec.Code, rec.Body.String())
	}
	if rec := cookieReq(t, f.router, http.MethodPut, "/api/me/providers/"+p.ID+"/key", f.jwt, `{"key":"`+testProviderKey+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("set key = %d %s", rec.Code, rec.Body.String())
	}
	if n := len(gw.seenAuth()); n != 0 {
		t.Fatalf("gateway saw %d request(s) before a key existed", n)
	}

	// Bearer is enough (RequireUser group).
	rec := bearerReqBody(f.router, http.MethodPost, test, f.uzc, "")
	var out struct {
		Probe *apitypes.ProviderProbeDTO `json:"probe"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil || out.Probe == nil ||
		out.Probe.AuthResult != "ok" || out.Probe.ToolResult != "passed" || out.Probe.ProbedAt.IsZero() {
		t.Fatalf("test = %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), testProviderKey) {
		t.Errorf("the response carries the key: %s", rec.Body.String())
	}
	for _, a := range gw.seenAuth() {
		if a != "Bearer "+testProviderKey {
			t.Errorf("gateway got Authorization %q", a)
		}
	}

	// The list/get DTO exposes the stored result.
	rec = bearerReqBody(f.router, http.MethodGet, "/api/me/providers/"+p.ID, f.uzc, "")
	var env providerEnvelope
	if json.Unmarshal(rec.Body.Bytes(), &env) != nil || env.Provider.Probe == nil || env.Provider.Probe.AuthResult != "ok" || env.Provider.Probe.ToolResult != "passed" {
		t.Fatalf("get after test = %s", rec.Body.String())
	}

	// A second probe replaces the row (latest only).
	gw.setMode("unauthorized")
	rec = bearerReqBody(f.router, http.MethodPost, test, f.uzc, "")
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil || out.Probe.AuthResult != "rejected" || out.Probe.ToolResult != "not_run" {
		t.Fatalf("second test = %d %s", rec.Code, rec.Body.String())
	}
	var rows int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM model_provider_key_test_results WHERE provider_id = $1`, p.ID).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("probe result rows = %d, %v; want 1", rows, err)
	}

	// An edit makes a new revision with no result; the next test stores one for it only.
	rec = bearerReqBody(f.router, http.MethodPatch, "/api/me/providers/"+p.ID, f.uzc, `{"model_map":{"haiku":{"model":"h2"}}}`)
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &env) != nil || env.Provider.CurrentRevision != 2 || env.Provider.Probe != nil {
		t.Fatalf("edit = %d %s", rec.Code, rec.Body.String())
	}
	gw.setMode("ok")
	if rec = bearerReqBody(f.router, http.MethodPost, test, f.uzc, ""); rec.Code != http.StatusOK {
		t.Fatalf("test after edit = %d %s", rec.Code, rec.Body.String())
	}
	var rev int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*), max(revision) FROM model_provider_key_test_results WHERE provider_id = $1`, p.ID).Scan(&rows, &rev); err != nil || rows != 1 || rev != 2 {
		t.Fatalf("probe rows = %d at revision %d, %v; want 1 at revision 2", rows, rev, err)
	}
}

func TestProviderTestProbeRefusalsLiveDB(t *testing.T) {
	f, gw, createBody := probeFixture(t)
	p := f.create(t, createBody)
	id := "/api/me/providers/" + p.ID
	if rec := cookieReq(t, f.router, http.MethodPut, id+"/key", f.jwt, `{"key":"`+testProviderKey+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("set key = %d", rec.Code)
	}

	// Another user's provider is a 404 and reaches no gateway.
	other := f.other(t)
	if rec := bearerReqBody(other.router, http.MethodPost, id+"/test", other.uzc, ""); rec.Code != http.StatusNotFound {
		t.Errorf("foreign test = %d %s, want 404", rec.Code, rec.Body.String())
	}
	if rec := bearerReqBody(f.router, http.MethodPost, "/api/me/providers/00000000-0000-0000-0000-000000000000/test", f.uzc, ""); rec.Code != http.StatusNotFound {
		t.Errorf("missing provider test = %d, want 404", rec.Code)
	}
	if n := len(gw.seenAuth()); n != 0 {
		t.Errorf("gateway saw %d request(s) from refused tests", n)
	}

	// A disabled key is refused like a missing one.
	if rec := cookieReq(t, f.router, http.MethodPatch, id+"/key/enabled", f.jwt, `{"enabled":false}`); rec.Code != http.StatusOK {
		t.Fatalf("disable key = %d", rec.Code)
	}
	if rec := bearerReqBody(f.router, http.MethodPost, id+"/test", f.uzc, ""); rec.Code != http.StatusNotFound {
		t.Errorf("disabled-key test = %d, want 404", rec.Code)
	}
	if rec := cookieReq(t, f.router, http.MethodPatch, id+"/key/enabled", f.jwt, `{"enabled":true}`); rec.Code != http.StatusOK {
		t.Fatalf("enable key = %d", rec.Code)
	}

	// The URL leaving the allowlist (or the feature going off) is the typed refusal, with no network.
	saved := f.h.cfg.ProviderAllowedBaseURLs
	f.h.cfg.ProviderAllowedBaseURLs = []string{"https://other.example.com/anthropic"}
	rec := bearerReqBody(f.router, http.MethodPost, id+"/test", f.uzc, "")
	if rec.Code != http.StatusBadRequest || providerReason(t, rec) != "provider_base_url_not_allowed" {
		t.Errorf("de-allowlisted test = %d %s", rec.Code, rec.Body.String())
	}
	f.h.cfg.ProviderAllowedBaseURLs = nil
	rec = bearerReqBody(f.router, http.MethodPost, id+"/test", f.uzc, "")
	if rec.Code != http.StatusBadRequest || providerReason(t, rec) != "provider_base_url_not_allowed" {
		t.Errorf("feature-off test = %d %s", rec.Code, rec.Body.String())
	}
	f.h.cfg.ProviderAllowedBaseURLs = saved
	if n := len(gw.seenAuth()); n != 0 {
		t.Errorf("gateway saw %d request(s) from refused tests", n)
	}

	// A retired provider is a 409.
	if rec := bearerReqBody(f.router, http.MethodPost, id+"/retire", f.uzc, ""); rec.Code != http.StatusOK {
		t.Fatalf("retire = %d", rec.Code)
	}
	rec = bearerReqBody(f.router, http.MethodPost, id+"/test", f.uzc, "")
	if rec.Code != http.StatusConflict || providerReason(t, rec) != "provider_retired" {
		t.Errorf("retired test = %d %s", rec.Code, rec.Body.String())
	}
	if n := len(gw.seenAuth()); n != 0 {
		t.Errorf("gateway saw %d request(s) from refused tests", n)
	}
}

func TestProviderTestProbeRateLimitedLiveDB(t *testing.T) {
	f, gw, createBody := probeFixture(t)
	f.h.providerProbeLimiter = providers.NewProbeLimiter()
	p := f.create(t, createBody)
	id := "/api/me/providers/" + p.ID
	if rec := cookieReq(t, f.router, http.MethodPut, id+"/key", f.jwt, `{"key":"`+testProviderKey+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("set key = %d", rec.Code)
	}
	release, _, _, ok := f.h.providerProbeLimiter.Acquire(f.user.String())
	if !ok {
		t.Fatal("could not occupy the owner's probe slot")
	}
	rec := bearerReqBody(f.router, http.MethodPost, id+"/test", f.uzc, "")
	if rec.Code != http.StatusTooManyRequests || providerReason(t, rec) != "provider_probe_rate_limited" || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("limited test = %d %s retry-after %q", rec.Code, rec.Body.String(), rec.Header().Get("Retry-After"))
	}
	if n := len(gw.seenAuth()); n != 0 {
		t.Errorf("gateway saw %d request(s) from a rate-limited test", n)
	}
	release()
	if rec := bearerReqBody(f.router, http.MethodPost, id+"/test", f.uzc, ""); rec.Code != http.StatusOK {
		t.Errorf("test after the slot was freed = %d %s", rec.Code, rec.Body.String())
	}
}

// Interleave a committed key rotation after the probe's evidence write, before its final re-read.
type providerProbeFinalReadInterleave struct {
	store.DBTX
	mu     sync.Mutex
	reads  int
	inHook bool
	before func()
}

func (db *providerProbeFinalReadInterleave) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if strings.Contains(sql, "-- name: GetVisibleProviderForUse ") {
		db.mu.Lock()
		db.reads++
		fire := db.reads == 3 && !db.inHook
		if fire {
			db.inHook = true
		}
		db.mu.Unlock()
		if fire {
			db.before()
		}
	}
	return db.DBTX.QueryRow(ctx, sql, args...)
}

// A key rotated after the evidence write commits leaves no current evidence for the final re-read:
// that is the benign edit race (409), not a server error.
func TestProviderTestProbeRotationAfterWriteIs409LiveDB(t *testing.T) {
	f, _, createBody := probeFixture(t)
	p := f.create(t, createBody)
	path := "/api/me/providers/" + p.ID
	if rec := cookieReq(t, f.router, http.MethodPut, path+"/key", f.jwt, `{"key":"`+testProviderKey+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("set key = %d %s", rec.Code, rec.Body.String())
	}
	fired := false
	hook := &providerProbeFinalReadInterleave{DBTX: f.pool, before: func() {
		fired = true
		if rec := cookieReq(t, f.router, http.MethodPut, path+"/key", f.jwt, `{"key":"replacement-gateway-key-0002"}`); rec.Code != http.StatusOK {
			t.Fatalf("rotate key = %d %s", rec.Code, rec.Body.String())
		}
	}}
	f.h.q = store.New(hook)
	rec := bearerReqBody(f.router, http.MethodPost, path+"/test", f.uzc, "")
	if !fired {
		t.Fatal("the probe never reached its final re-read")
	}
	if rec.Code != http.StatusConflict {
		t.Errorf("probe = %d %s, want 409", rec.Code, rec.Body.String())
	}
}
