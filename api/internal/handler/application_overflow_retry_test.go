package handler

import (
	"encoding/json"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type r3HTTP struct {
	server                          *httptest.Server
	metadata, target, events, notes atomic.Int64
	beforeNote                      func(string)
}

func r3Forge(t *testing.T, kind, mode string) (*r3HTTP, forge.Forge) {
	t.Helper()
	h := &r3HTTP{}
	h.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/repositories/42"):
			h.metadata.Add(1)
			if mode == "metadata" {
				_, _ = w.Write([]byte(`{"padding":"` + strings.Repeat("REMOTE_PRIVATE", (1<<20)/14+100) + `"}`))
				return
			}
			_, _ = w.Write([]byte(`{"id":42,"owner":{"login":"owner"},"name":"repo"}`))
		case strings.HasSuffix(r.URL.Path, "/resource_label_events"):
			h.events.Add(1)
			_, _ = w.Write([]byte(`[{"id":9,"action":"add","label":{"name":"autopilot"},"user":{"username":"alice"}}]`))
		case strings.HasSuffix(r.URL.Path, "/events"):
			h.events.Add(1)
			_, _ = w.Write([]byte(`[{"id":9,"event":"labeled","label":{"name":"autopilot"},"actor":{"login":"alice"}}]`))
		case r.Method == http.MethodPost && (strings.HasSuffix(r.URL.Path, "/notes") || strings.HasSuffix(r.URL.Path, "/comments")):
			h.notes.Add(1)
			body, _ := io.ReadAll(r.Body)
			if h.beforeNote != nil {
				h.beforeNote(string(body))
			}
			_, _ = w.Write([]byte(`{"id":1,"body":"posted"}`))
		case strings.HasSuffix(r.URL.Path, "/issues/7"):
			h.target.Add(1)
			if mode == "503" {
				w.Header().Set("Content-Type", "text/html")
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(strings.Repeat("REMOTE_PRIVATE", 400)))
				return
			}
			w.(http.Flusher).Flush()
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1, "iid": 7, "number": 7, "body": strings.Repeat("REMOTE_PRIVATE", (2<<20)/14+100), "description": strings.Repeat("REMOTE_PRIVATE", (2<<20)/14+100)})
		case strings.HasSuffix(r.URL.Path, "/issues/8"):
			h.target.Add(1)
			_, _ = w.Write([]byte(`{"id":2,"iid":8,"number":8,"title":"fresh","body":"body","description":"body","labels":[]}`))
		default:
			t.Errorf("unexpected HTTP %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected fixture request", http.StatusBadRequest)
		}
	}))
	t.Cleanup(h.server.Close)
	f, err := forge.New(forge.Type(kind), h.server.URL, "fixture", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return h, f
}
func (h *r3HTTP) assertAttempts(t *testing.T, metadata, target, events, notes int64) {
	t.Helper()
	if h.metadata.Load() != metadata || h.target.Load() != target || h.events.Load() != events || h.notes.Load() != notes {
		t.Fatalf("HTTP attempts metadata=%d target=%d events=%d notes=%d; want %d/%d/%d/%d", h.metadata.Load(), h.target.Load(), h.events.Load(), h.notes.Load(), metadata, target, events, notes)
	}
}
