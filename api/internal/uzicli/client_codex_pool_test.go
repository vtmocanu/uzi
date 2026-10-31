package uzicli

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Issue #3719: the two Codex pool routes and the typed 422 refusal.

func TestHTTPClientSetCodexAutoEligible(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Method != http.MethodPatch || r.URL.Path != "/api/me/secrets/codex_auth/c1/auto-eligible" || strings.TrimSpace(string(body)) != `{"auto_eligible":true}` {
			t.Errorf("request = %s %s %s", r.Method, r.URL.Path, body)
		}
		_, _ = w.Write([]byte(`{"secret":{"id":"c1","kind":"codex_auth","label":"work","auto_eligible":true}}`))
	}))
	defer srv.Close()
	got, err := newTestClient(srv).SetCodexAutoEligible(context.Background(), "c1", true)
	if err != nil || got.ID != "c1" || !got.AutoEligible {
		t.Fatalf("got %+v err %v", got, err)
	}
}

func TestHTTPClientSetCodexAccountMode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Method != http.MethodPut || r.URL.Path != "/api/me/codex-account-mode" || strings.TrimSpace(string(body)) != `{"codex_account_mode":"auto"}` {
			t.Errorf("request = %s %s %s", r.Method, r.URL.Path, body)
		}
		_, _ = w.Write([]byte(`{"codex_account_mode":"auto"}`))
	}))
	defer srv.Close()
	got, err := newTestClient(srv).SetCodexAccountMode(context.Background(), "auto")
	if err != nil || got != "auto" {
		t.Fatalf("got %q err %v", got, err)
	}
}

// TestStatusErrorNamesCodexPoolUnavailable: the 422 keeps the server message and adds the typed
// reason, so a refused run start or MR rework says why without --json; the reason also rides
// the ExitError. A 422 without the reason is unchanged.
func TestStatusErrorNamesCodexPoolUnavailable(t *testing.T) {
	body := []byte(`{"error":"no pooled Codex account is available","code":"no_credential_for_harness","reason":"codex_pool_unavailable"}`)
	e := statusError(http.StatusUnprocessableEntity, body, "")
	if e.Code != ExitUsage || e.Reason != ReasonCodexPoolUnavailable || !strings.Contains(e.Error(), "no pooled Codex account is available (reason: codex_pool_unavailable)") {
		t.Fatalf("got code=%d reason=%q msg=%q", e.Code, e.Reason, e.Error())
	}
	plain := statusError(http.StatusUnprocessableEntity, []byte(`{"error":"bad until"}`), "")
	if strings.Contains(plain.Error(), "reason:") {
		t.Fatalf("an unrelated 422 gained a reason: %q", plain.Error())
	}
}
