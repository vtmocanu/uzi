package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/config"
)

// TestPostAgentSourceResolveLatestSSRF verifies the PRD #702 M2 SSRF recheck (Decision
// 5): with an EMPTY agent-source allowlist (deny-all), an off-allowlist https URL is
// refused with 400 BEFORE any network call — the handler returns without a panic and
// without egress (ResolveLatestTag is never reached, so an unreachable/hostile host is
// never dialed). Non-livedb: the handler method is called directly with only cfg set.
func TestPostAgentSourceResolveLatestSSRF(t *testing.T) {
	// Empty allowlist = deny-all (AgentSourceBaseURLAllowed returns false for everything).
	h := &Handler{cfg: config.Config{AgentSourceAllowedBaseURLs: nil}}

	body := `{"url":"https://evil.example/x"}`
	r := httptest.NewRequest(http.MethodPost, "/admin/agent-source/resolve-latest", strings.NewReader(body))
	w := httptest.NewRecorder()

	h.PostAgentSourceResolveLatest(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("off-allowlist url: status = %d, want %d (SSRF recheck must refuse before egress)", w.Code, http.StatusBadRequest)
	}
	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if !strings.Contains(resp["error"], "allowlist") {
		t.Errorf("error message = %q, want it to mention the allowlist", resp["error"])
	}
}

// TestPostAgentSourceResolveLatestValidation covers the input-validation branches with no
// network reach: a malformed body is a 400, and an empty url is a 400 "url is required".
func TestPostAgentSourceResolveLatestValidation(t *testing.T) {
	h := &Handler{cfg: config.Config{AgentSourceAllowedBaseURLs: nil}}

	t.Run("malformed body", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/admin/agent-source/resolve-latest", strings.NewReader("not json"))
		w := httptest.NewRecorder()
		h.PostAgentSourceResolveLatest(w, r)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("malformed body: status = %d, want 400", w.Code)
		}
	})

	t.Run("empty url", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/admin/agent-source/resolve-latest", strings.NewReader(`{"url":"   "}`))
		w := httptest.NewRecorder()
		h.PostAgentSourceResolveLatest(w, r)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("empty url: status = %d, want 400", w.Code)
		}
		var resp map[string]string
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		if !strings.Contains(resp["error"], "url is required") {
			t.Errorf("error message = %q, want 'url is required'", resp["error"])
		}
	})
}
