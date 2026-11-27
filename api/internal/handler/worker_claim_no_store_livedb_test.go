package handler

import (
	"context"
	"net/http"
	"testing"
)

// PRD #4416 M2: a claim answer can carry secret values (credentials and run environment
// variables), so neither the 200 that delivers one nor the 204 that finds nothing may be
// stored by a cache or proxy. Through the real worker router against PostgreSQL.
func TestWorkerClaimAnswersAreNoStoreLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newSkewFixture(ctx, t)
	if rec := f.post(t, "/api/worker/register", `{"version":"0.1.0","name":"w","template":"base"}`); rec.Code != http.StatusOK {
		t.Fatalf("register = %d: %s", rec.Code, rec.Body.String())
	}
	rec := f.post(t, "/api/worker/runs/claim", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("claim = %d, want 200 with the queued run: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("200 claim Cache-Control = %q, want no-store", got)
	}
	// The only queued run is now claimed: the next claim finds nothing.
	rec = f.post(t, "/api/worker/runs/claim", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("second claim = %d, want 204", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("204 claim Cache-Control = %q, want no-store", got)
	}
	rec = f.post(t, "/api/worker/runs/claim?lane=chat", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("chat claim = %d, want 204", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("chat claim Cache-Control = %q, want no-store", got)
	}
}
