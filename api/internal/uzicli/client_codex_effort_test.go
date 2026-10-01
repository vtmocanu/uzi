package uzicli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSettingsDecodeKeepsCodexEffort(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/me/settings" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"settings":{"default_effort":"high","default_codex_effort":"medium"}}`))
	}))
	defer srv.Close()
	settings, err := newTestClient(srv).GetMySettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["default_effort"] != "high" || decoded["default_codex_effort"] != "medium" {
		t.Fatalf("CLI decode dropped a lane: %s", raw)
	}
}
