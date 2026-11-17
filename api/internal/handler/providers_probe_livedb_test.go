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

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/providers"
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
}

func newFakeProviderGateway(t *testing.T) *fakeProviderGateway {
	t.Helper()
	g := &fakeProviderGateway{mode: "ok"}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
		g.mu.Lock()
		g.auth = append(g.auth, r.Header.Get("Authorization"))
		mode := g.mode
		g.mu.Unlock()
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
		`SELECT count(*) FROM model_provider_probe_results WHERE provider_id = $1`, p.ID).Scan(&rows); err != nil || rows != 1 {
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
		`SELECT count(*), max(revision) FROM model_provider_probe_results WHERE provider_id = $1`, p.ID).Scan(&rows, &rev); err != nil || rows != 1 || rev != 2 {
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
