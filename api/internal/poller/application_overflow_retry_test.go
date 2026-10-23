package poller

import (
	"context"
	"encoding/json"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestR3AutopilotOverflowRetry(t *testing.T) {
	for _, kind := range []string{"github", "gitlab"} {
		for _, mode := range []string{"503", "metadata", "issue", "early_metadata"} {
			if (mode == "metadata" || mode == "early_metadata") && kind != "github" {
				continue
			}
			t.Run(kind+"/"+mode, func(t *testing.T) {
				fixtureMode := mode
				if mode == "early_metadata" {
					fixtureMode = "metadata"
				}
				wire, f := r3Forge(t, kind, fixtureMode)
				st := &apStore{candidates: []store.ListAutopilotCandidateIssuesRow{candIssue(7, "alice")}, cc: ccOwner("alice", true, true)}
				runs := &apRuns{}
				wire.beforeNote = func(body string) {
					if len(st.upserts) != 1 || st.upserts[0].LastEventID != 9 {
						t.Error("comment before record")
					}
					if strings.Contains(body, "REMOTE_PRIVATE") {
						t.Error("remote content in comment")
					}
					var note struct{ Body string }
					if err := json.Unmarshal([]byte(body), &note); err != nil || note.Body != tooLargeComment() {
						t.Error("comment is not fixed safe message")
					}
				}
				for attempt := int64(1); attempt <= 2; attempt++ {
					if mode == "metadata" {
						// handle starts at an eligible event; label-event lookup would warm the GitHub slug cache.
						fresh, err := forge.New(forge.Type(kind), wire.server.URL, "fixture", time.Second)
						if err != nil {
							t.Fatal(err)
						}
						NewAutopilot(st, runs, nil).handle(context.Background(), repoRow(), fresh, apLabel, st.cc, candIssue(7, "alice"), 9, "alice")
					} else {
						NewAutopilot(st, runs, nil).detect(context.Background(), repoRow(), f)
					}
					if len(runs.calls)+len(runs.resolutions) != 0 {
						t.Fatal("failed capture reached harness/create")
					}
					meta, target, events, notes := int64(0), attempt, attempt, int64(0)
					if kind == "github" {
						meta = 1
					}
					if mode == "issue" {
						target, notes = 1, 1
						if len(st.upserts) != 1 {
							t.Fatal("own overflow not consumed")
						}
					} else if len(st.upserts) != 0 {
						t.Fatal("transient failure consumed event")
					}
					if mode == "metadata" || mode == "early_metadata" {
						meta, target, events = attempt, 0, 0
					}
					wire.assertAttempts(t, meta, target, events, notes)
				}
			})
		}
	}
}

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
