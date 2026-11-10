package uzicli

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

func TestHTTPClientResumePolicyHold(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/runs/"+testRunID+"/resume-now" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		got = string(b)
		writeJSON(w, map[string]any{"run": apitypes.RunDTO{ID: testRunID, Status: "queued"}, "replay": true})
	}))
	t.Cleanup(srv.Close)

	run, replay, err := newTestClient(srv).ResumePolicyHold(context.Background(), testRunID, "preserve", "ep-1", "")
	if err != nil || !replay || run.ID != testRunID {
		t.Fatalf("got %s replay=%v err=%v", run.ID, replay, err)
	}
	want := `{"mode":"preserve","expected_episode_id":"ep-1","guidance":""}`
	if got != want && got != want+"\n" {
		t.Errorf("body = %q, want %q", got, want)
	}
}
