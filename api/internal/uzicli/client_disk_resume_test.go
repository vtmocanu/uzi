package uzicli

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// One owner invocation is one bodyless POST. In particular a conflict or a
// capacity refusal must not replay consent against a subsequent disk hold.
func TestHTTPClientDiskHoldResumeBodylessNoRetry(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusConflict, http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != http.MethodPost || r.URL.Path != "/api/runs/"+testRunID+"/resume-now" {
					t.Errorf("request = %s %s; want bodyless Resume POST", r.Method, r.URL.Path)
				}
				body, err := io.ReadAll(r.Body)
				if err != nil || len(body) != 0 {
					t.Errorf("Resume body = %q, error=%v; want empty", body, err)
				}
				w.WriteHeader(status)
				if status == http.StatusOK {
					writeJSON(w, map[string]any{"run": apitypes.RunDTO{ID: testRunID, Status: "queued"}})
					return
				}
				writeJSON(w, map[string]string{"error": "Resume refused"})
			}))
			t.Cleanup(srv.Close)

			run, err := newTestClient(srv).ResumeRunNow(context.Background(), testRunID)
			if status == http.StatusOK {
				if err != nil || run.ID != testRunID || run.Status != "queued" {
					t.Fatalf("Resume = %s/%s, %v; want queued", run.ID, run.Status, err)
				}
			} else if err == nil {
				t.Fatal("Resume refusal returned no error")
			}
			if got := calls.Load(); got != 1 {
				t.Fatalf("one owner action sent %d requests, want 1", got)
			}
		})
	}
}
