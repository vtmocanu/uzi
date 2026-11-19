package uzicli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestListRepoEnvDecodes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.EscapedPath() != "/api/repos/r%2F1/env" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.EscapedPath())
		}
		_, _ = w.Write([]byte(`{"variables":[
			{"name":"A","secret":false,"description":"d","value":"v","value_withheld":false,"state":"enabled","rev":2,"disabled_at":null,"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-02T00:00:00Z"},
			{"name":"B","secret":true,"description":"","value":null,"value_withheld":false,"state":"disabled","rev":1,"disabled_at":"2026-01-03T00:00:00Z","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-02T00:00:00Z"}],
			"counts":{"total":2,"disabled":1,"pending_deletion":0,"limit":50},"vault_locked":true}`))
	}))
	defer srv.Close()
	got, err := newTestClient(srv).ListRepoEnv(context.Background(), "r/1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Variables) != 2 || !got.VaultLocked || got.Counts.Limit != 50 || got.Counts.Disabled != 1 {
		t.Fatalf("decoded = %+v", got)
	}
	if v := got.Variables[0]; v.Value == nil || *v.Value != "v" || v.Rev != 2 {
		t.Errorf("plain var = %+v", v)
	}
	if v := got.Variables[1]; !v.Secret || v.Value != nil || v.DisabledAt == nil {
		t.Errorf("secret var = %+v", v)
	}
}

func TestListRepoEnvNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
	}))
	defer srv.Close()
	if _, err := newTestClient(srv).ListRepoEnv(context.Background(), "nope"); err == nil {
		t.Fatal("want error on 404")
	}
}
