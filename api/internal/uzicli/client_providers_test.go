package uzicli

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

const providerEnvelope = `{"provider":{"id":"p/1","name":"corp","engine":"claude","retired":false,"current_revision":2,"protocol":"anthropic_messages","base_url":"https://gw.example.com/a","auth_style":"bearer","model_map":{"haiku":{"model":"h"}},"has_key":false,"key_enabled":false,"probe":null}}`

func TestHTTPClientProviders(t *testing.T) {
	type seen struct{ method, path, body string }
	var got seen
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = seen{r.Method, r.URL.EscapedPath(), string(b)}
		switch {
		case r.URL.EscapedPath() == "/api/me/providers" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"enabled":true,"providers":[],"allowed_base_urls":["https://gw.example.com/a"]}`))
		case r.URL.EscapedPath() == "/api/me/providers/p%2F1/test":
			_, _ = w.Write([]byte(`{"probe":{"auth_result":"ok","tool_result":"not_supported","probed_at":"2026-01-01T00:00:00Z"}}`))
		case r.Method == http.MethodPost && r.URL.EscapedPath() == "/api/me/providers":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(providerEnvelope))
		default:
			_, _ = w.Write([]byte(providerEnvelope))
		}
	}))
	defer srv.Close()
	c := newTestClient(srv)
	ctx := context.Background()

	list, err := c.ListProviders(ctx)
	if err != nil || !list.Enabled || len(list.AllowedBaseURLs) != 1 || got.method != http.MethodGet || got.path != "/api/me/providers" {
		t.Fatalf("list: %+v err=%v seen=%+v", list, err, got)
	}

	created, err := c.CreateProvider(ctx, apitypes.ProviderCreateRequest{Name: "corp", Engine: "claude", Protocol: "anthropic_messages", BaseURL: "https://gw.example.com/a", AuthStyle: "bearer", ModelMap: apitypes.ModelMapDTO{Haiku: apitypes.ModelEntryDTO{Model: "h"}}})
	if err != nil || created.ID != "p/1" || created.CurrentRevision != 2 || got.method != http.MethodPost || got.path != "/api/me/providers" {
		t.Fatalf("create: %+v err=%v seen=%+v", created, err, got)
	}
	if want := `{"name":"corp","engine":"claude","protocol":"anthropic_messages","base_url":"https://gw.example.com/a","auth_style":"bearer","model_map":{"haiku":{"model":"h"}}}`; got.body != want {
		t.Fatalf("create body = %s", got.body)
	}

	name := "new"
	edited, err := c.EditProvider(ctx, "p/1", apitypes.ProviderPatchRequest{Name: &name})
	if err != nil || edited.Name != "corp" || got.method != http.MethodPatch || got.path != "/api/me/providers/p%2F1" || got.body != `{"name":"new"}` {
		t.Fatalf("edit: %+v err=%v seen=%+v", edited, err, got)
	}

	retired, err := c.RetireProvider(ctx, "p/1")
	if err != nil || retired.ID != "p/1" || got.method != http.MethodPost || got.path != "/api/me/providers/p%2F1/retire" {
		t.Fatalf("retire: %+v err=%v seen=%+v", retired, err, got)
	}

	probe, err := c.TestProvider(ctx, "p/1")
	if err != nil || probe == nil || probe.AuthResult != "ok" || probe.ToolResult != "not_supported" || got.method != http.MethodPost {
		t.Fatalf("test: %+v err=%v seen=%+v", probe, err, got)
	}
}

func TestHTTPClientSetClaudeProviderSource(t *testing.T) {
	var method, path, body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		method, path, body = r.Method, r.URL.EscapedPath(), string(b)
		if strings.Contains(body, "unusable") {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":"the provider cannot be the Claude source: the provider is retired","reason":"provider_source_unusable"}`))
			return
		}
		if strings.Contains(body, "null") {
			_, _ = w.Write([]byte(`{"claude_provider_id":null}`))
			return
		}
		_, _ = w.Write([]byte(`{"claude_provider_id":"p-1"}`))
	}))
	defer srv.Close()
	c := newTestClient(srv)
	ctx := context.Background()

	id := "p-1"
	got, err := c.SetClaudeProviderSource(ctx, &id)
	if err != nil || got == nil || *got != "p-1" || method != http.MethodPut || path != "/api/me/providers/claude-source" || body != `{"provider_id":"p-1"}` {
		t.Fatalf("select: %v err=%v %s %s %s", got, err, method, path, body)
	}
	got, err = c.SetClaudeProviderSource(ctx, nil)
	if err != nil || got != nil || body != `{"provider_id":null}` {
		t.Fatalf("clear: %v err=%v body=%s", got, err, body)
	}
	bad := "unusable"
	_, err = c.SetClaudeProviderSource(ctx, &bad)
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Code != ExitConflict || ee.Reason != "provider_source_unusable" {
		t.Fatalf("refusal = %#v, want a conflict carrying the reason", err)
	}
}

func TestHTTPClientSetCodexProviderSource(t *testing.T) {
	var method, path, body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		method, path, body = r.Method, r.URL.EscapedPath(), string(b)
		if strings.Contains(body, "unusable") {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":"the provider cannot be the Codex source: the provider is retired","reason":"provider_source_unusable"}`))
			return
		}
		if strings.Contains(body, "null") {
			_, _ = w.Write([]byte(`{"codex_provider_id":null}`))
			return
		}
		_, _ = w.Write([]byte(`{"codex_provider_id":"p-1"}`))
	}))
	defer srv.Close()
	c := newTestClient(srv)
	ctx := context.Background()

	id := "p-1"
	got, err := c.SetCodexProviderSource(ctx, &id)
	if err != nil || got == nil || *got != "p-1" || method != http.MethodPut || path != "/api/me/providers/codex-source" || body != `{"provider_id":"p-1"}` {
		t.Fatalf("select: %v err=%v %s %s %s", got, err, method, path, body)
	}
	got, err = c.SetCodexProviderSource(ctx, nil)
	if err != nil || got != nil || body != `{"provider_id":null}` {
		t.Fatalf("clear: %v err=%v body=%s", got, err, body)
	}
	bad := "unusable"
	_, err = c.SetCodexProviderSource(ctx, &bad)
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Code != ExitConflict || ee.Reason != "provider_source_unusable" {
		t.Fatalf("refusal = %#v, want a conflict carrying the reason", err)
	}
}
