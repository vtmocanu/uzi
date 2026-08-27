package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/config"
	"github.com/vtmocanu/uzi/api/internal/settings"
)

// newAgentSourceHandler builds a settings handler whose cfg carries the given
// agent-source SSRF allowlist. Other paths (pool/box) are nil: the allowlist gate
// runs BEFORE any transaction, so a rejection returns without touching them.
func newAgentSourceHandler(allowlist ...string) *Handler {
	return &Handler{
		settings: settings.New(&settingsStore{}, time.Minute),
		cfg:      config.Config{AgentSourceAllowedBaseURLs: allowlist},
	}
}

// TestUpdateSettingsAgentSourceAllowlistRejectsNonAllowlisted is the load-bearing
// SSRF-bypass test (PRD #602 M2): a generic PUT setting agent_source_repo_url to a
// URL not in AGENT_SOURCE_ALLOWED_BASE_URLS is rejected in the generic handler, so
// there is no generic-PUT bypass around a dedicated route.
func TestUpdateSettingsAgentSourceAllowlistRejectsNonAllowlisted(t *testing.T) {
	admin := adminUser()

	// Empty allowlist: nothing is allowed, so any URL is rejected.
	h := newAgentSourceHandler()
	rec := httptest.NewRecorder()
	h.UpdateSettings(rec, putSettings(&admin, `{"settings":{"agent_source_repo_url":"https://git.example.com/roster.git"}}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty allowlist: status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), settings.KeyAgentSourceRepoURL) {
		t.Errorf("rejection body should name the key; got %s", rec.Body.String())
	}

	// A configured allowlist that does not contain the submitted host: still rejected.
	h = newAgentSourceHandler("https://git.example.com")
	rec = httptest.NewRecorder()
	h.UpdateSettings(rec, putSettings(&admin, `{"settings":{"agent_source_repo_url":"https://evil.example.com/roster.git"}}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("non-allowlisted host: status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
}

// TestUpdateSettingsAgentSourceAllowlistPassesAllowlisted confirms an allowlisted URL
// PASSES the SSRF gate: it is not rejected with a 400, and instead proceeds to the DB
// write, which panics on the nil test pool. Recovering that panic proves the gate let
// it through rather than short-circuiting with the allowlist rejection.
func TestUpdateSettingsAgentSourceAllowlistPassesAllowlisted(t *testing.T) {
	admin := adminUser()
	h := newAgentSourceHandler("https://git.example.com")
	rec := httptest.NewRecorder()

	defer func() {
		// A panic here means we reached the transaction (nil pool.Begin) — i.e. the
		// allowlist gate passed. The absence of a 400 is the assertion.
		_ = recover()
		if rec.Code == http.StatusBadRequest {
			t.Errorf("allowlisted URL rejected by the gate: %s", rec.Body.String())
		}
	}()

	h.UpdateSettings(rec, putSettings(&admin, `{"settings":{"agent_source_repo_url":"https://git.example.com/roster.git"}}`))
	// If UpdateSettings returned without panicking (no DB reached), a non-400 status
	// still proves the gate passed — the deferred check covers both paths.
}
