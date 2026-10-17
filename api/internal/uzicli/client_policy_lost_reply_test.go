package uzicli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// The first action commits, but its reply is lost. A later hold must not turn
// a transport retry of that old intent into consent for another fresh claim.
func TestPolicyResumeLostReplyRetainsPresentedAction(t *testing.T) {
	var mu sync.Mutex
	current := `"policy-first-1"`
	attempts, applied, guidance, reads := 0, 0, 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method != http.MethodPost || r.URL.Path != "/api/runs/r1/resume-now" {
			reads++
			http.Error(w, "unexpected refresh", http.StatusBadRequest)
			return
		}
		attempts++
		if r.Header.Get("If-Match") != current {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":"policy hold changed; refetch and renew the owner action"}`))
			return
		}
		var body struct {
			Mode     string `json:"mode"`
			Guidance string `json:"guidance"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Mode != "fresh" || body.Guidance != "inspect parser only" {
			t.Errorf("body=%+v err=%v", body, err)
			http.Error(w, "bad action", http.StatusBadRequest)
			return
		}
		applied++
		guidance++
		current = `"policy-next-hold-2"`
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack committed reply: %v", err)
			return
		}
		_ = conn.Close()
	}))
	defer srv.Close()
	c := newTestClient(srv)
	opts := RunResumeOptions{Mode: "fresh", Guidance: "inspect parser only", ActionIfMatch: `"policy-first-1"`}
	if _, err := c.ResumeRunNowWithOptions(context.Background(), "r1", opts); err == nil {
		t.Fatal("dropped committed response must remain an error")
	}
	if _, err := c.ResumeRunNowWithOptions(context.Background(), "r1", opts); ExitCodeFor(err) != ExitConflict {
		t.Fatalf("old owner intent must conflict after a later hold: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if attempts != 2 || applied != 1 || guidance != 1 || reads != 0 {
		t.Fatalf("attempts=%d applied=%d guidance=%d refreshes=%d", attempts, applied, guidance, reads)
	}
}
