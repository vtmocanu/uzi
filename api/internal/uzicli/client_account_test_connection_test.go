package uzicli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHTTPClientTestSecret(t *testing.T) {
	called := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called++
		if r.Method != http.MethodPost || r.URL.EscapedPath() != "/api/me/secrets/codex_auth/id%2F1/test" {
			t.Errorf("request %s %s", r.Method, r.URL.EscapedPath())
		}
		_, _ = w.Write([]byte(`{"status":"permission_denied","reason":"generic","display":"safe"}`))
	}))
	defer srv.Close()
	got, err := newTestClient(srv).TestSecret(context.Background(), "codex_auth", "id/1")
	if err != nil || called != 1 || got.Status != "permission_denied" || got.Reason != "generic" || got.Display != "safe" {
		t.Fatalf("got=%+v err=%v calls=%d", got, err, called)
	}
}
