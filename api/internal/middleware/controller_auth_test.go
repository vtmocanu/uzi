package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/jointoken"
)

func TestRequireControllerAcceptsTheConfiguredToken(t *testing.T) {
	const token = "uzc_the-controller-credential"
	called := false
	h := RequireController(jointoken.Hash(token))(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/api/controller/poll", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !called {
		t.Fatal("handler not reached")
	}
}

func TestRequireControllerRejectsMissingAndBadTokens(t *testing.T) {
	h := RequireController(jointoken.Hash("uzc_the-real-one"))
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	cases := []struct{ name, authz string }{
		{"no header", ""},
		{"wrong scheme", "Basic abc"},
		{"empty credential", "Bearer "},
		{"wrong token", "Bearer uzc_not-the-real-one"},
		// A worker's join token must not authenticate as the controller: the two
		// credentials are separate trust anchors with very different scopes.
		{"a worker join token", "Bearer uzw_some-workers-token"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/controller/poll", nil)
			if tc.authz != "" {
				req.Header.Set("Authorization", tc.authz)
			}
			rec := httptest.NewRecorder()
			h(next).ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
		})
	}
}

// An unconfigured hash (hosting off, or a wiring mistake) must never degrade into
// "everything authenticates". config refuses to enable hosting without a hash and
// Routes does not mount the group when hosting is off, so this is defence in depth
// against a future caller wiring it up wrong.
func TestRequireControllerRejectsEverythingWhenNoHashIsConfigured(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	for _, want := range [][]byte{nil, {}} {
		req := httptest.NewRequest(http.MethodPost, "/api/controller/poll", nil)
		req.Header.Set("Authorization", "Bearer anything-at-all")
		rec := httptest.NewRecorder()
		RequireController(want)(next).ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d for hash %v, want 401", rec.Code, want)
		}
	}
}
