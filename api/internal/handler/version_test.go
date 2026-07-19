package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"gitlab.example.com/vtmocanu/uzi/api/internal/config"
)

// GET /api/version reports the stamped build version, and SetVersion is the only
// way to change it: an empty value (the plain-`go build` / un-stamped case) must
// not clobber whatever it already holds.
func TestVersionEndpoint(t *testing.T) {
	get := func(h *Handler) string {
		t.Helper()
		rec := httptest.NewRecorder()
		h.Version(rec, httptest.NewRequest(http.MethodGet, "/api/version", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /api/version = %d, want 200", rec.Code)
		}
		var body struct {
			Version string `json:"version"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v (body=%q)", err, rec.Body.String())
		}
		return body.Version
	}

	// New defaults to "dev" — an un-stamped local/compose build.
	h := New(nil, nil, config.Config{}, nil, nil, nil, nil, nil, nil)
	if got := get(h); got != "dev" {
		t.Fatalf("default version = %q, want %q", got, "dev")
	}

	// A stamped value is reported verbatim (the release tag, incl. any leading v).
	h.SetVersion("v1.2.3")
	if got := get(h); got != "v1.2.3" {
		t.Fatalf("after SetVersion = %q, want %q", got, "v1.2.3")
	}

	// An empty stamp (ldflags var unset) leaves the last value untouched.
	h.SetVersion("")
	if got := get(h); got != "v1.2.3" {
		t.Fatalf("after SetVersion(\"\") = %q, want it unchanged (%q)", got, "v1.2.3")
	}
}
