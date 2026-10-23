package schedsvc

import (
	"context"
	"encoding/json"
	"github.com/jackc/pgx/v5/pgtype"
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

func TestR3IssueScheduleOverflowRetry(t *testing.T) {
	for _, kind := range []string{"github", "gitlab"} {
		for _, mode := range []string{"503", "metadata", "issue"} {
			if mode == "metadata" && kind != "github" {
				continue
			}
			t.Run(kind+"/"+mode, func(t *testing.T) {
				wire, f := r3Forge(t, kind, mode)
				h := newHarness()
				h.sched.forge = m2OverflowBuilder{h.fb, f}
				s := h.issueSchedule()
				s.Timing = "once"
				s.NextFireAt = pgtype.Timestamptz{Time: h.now.Add(-time.Hour), Valid: true}
				h.st.due = []store.RunSchedule{s}
				for attempt := int64(1); attempt <= 2; attempt++ {
					h.sched.Boot(context.Background())
					if len(h.runs.runs)+len(h.runs.autopilot) != 0 {
						t.Fatal("failed capture created run")
					}
					if mode == "issue" {
						if len(h.st.advanceCalls) != int(attempt) {
							t.Fatal("own overflow must consume fire")
						}
						_, reasons := heldLastFire(t, h.st.advanceCalls[attempt-1].LastFire)
						if len(reasons) != 1 || reasons[0] != string(SkipDescriptionTooLarge) {
							t.Fatalf("reasons=%v", reasons)
						}
					} else if len(h.st.advanceCalls)+len(h.st.heldFireCalls)+len(h.st.statusCalls) != 0 {
						t.Fatal("transient capture changed schedule")
					}
					meta, target := int64(0), attempt
					if kind == "github" {
						meta = 1
					}
					if mode == "metadata" {
						meta, target = attempt, 0
					}
					wire.assertAttempts(t, meta, target, 0, 0)
				}
			})
		}
	}
}
func TestR3SweepOverflowRetryBackfill(t *testing.T) {
	for _, kind := range []string{"github", "gitlab"} {
		for _, mode := range []string{"503", "metadata", "issue"} {
			if mode == "metadata" && kind != "github" {
				continue
			}
			t.Run(kind+"/"+mode, func(t *testing.T) {
				fixtureMode := mode
				if mode == "metadata" {
					fixtureMode = "metadata_backfill"
				}
				wire, f := r3Forge(t, kind, fixtureMode)
				h := newHarness()
				h.sched.forge = m2OverflowBuilder{h.fb, f}
				h.st.sweepRows = []store.ListSweepCandidateIssuesRow{{ForgeIssueIid: 7}, {ForgeIssueIid: 8}}
				h.st.sweepCount = 2
				out, err := h.sched.RunNow(context.Background(), h.sweepSchedule(pgtype.Int4{Int32: 1, Valid: true}))
				want := SkipFetchFailed
				if mode == "issue" {
					want = SkipDescriptionTooLarge
				}
				if err != nil || len(out.Skips) == 0 || out.Skips[0].Reason != want || out.Skips[0].Title != "" || out.Skips[0].WebURL != "" {
					t.Fatalf("out=%+v err=%v", out, err)
				}
				if out.Matched != 2 || len(out.Started) != 1 || *out.Started[0].IssueIID != 8 {
					t.Fatalf("backfill=%+v", out)
				}
				meta, target := int64(0), int64(2)
				if kind == "github" {
					meta = 1
				}
				if mode == "metadata" {
					meta, target = 2, 1
				}
				wire.assertAttempts(t, meta, target, 0, 0)
				if wire.commentReads.Load() != 1 {
					t.Fatalf("comment reads=%d want=1", wire.commentReads.Load())
				}
				assertBalances(t, out)
			})
		}
	}
}

type r3HTTP struct {
	server                                        *httptest.Server
	metadata, target, events, notes, commentReads atomic.Int64
	beforeNote                                    func(string)
}

func r3Forge(t *testing.T, kind, mode string) (*r3HTTP, forge.Forge) {
	t.Helper()
	h := &r3HTTP{}
	h.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/repositories/42"):
			n := h.metadata.Add(1)
			if mode == "metadata" || (mode == "metadata_backfill" && n == 1) {
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
		case r.Method == http.MethodGet && (strings.HasSuffix(r.URL.Path, "/issues/8/comments") || strings.HasSuffix(r.URL.Path, "/issues/8/notes")):
			h.commentReads.Add(1)
			_, _ = w.Write([]byte("[]"))
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
