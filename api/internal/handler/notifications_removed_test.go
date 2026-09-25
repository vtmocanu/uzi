package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/config"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
)

// TestNotificationsInboxRoutesAreGone pins PRD #1650 D1: the notifications inbox read
// path (list, unread count, mark-read) is retired, so each former route falls through to
// the router's not-found handling. It drives the REAL route table (h.Routes), not a
// hand-built chi router, so a stray mount left behind would answer 401 (RequireAuth runs
// before any store access) instead of 404 and fail here.
func TestNotificationsInboxRoutesAreGone(t *testing.T) {
	noLimit := mw.NewLimiter(100000, time.Minute, nil)
	h := &Handler{cfg: config.Config{WorkerHostingEnabled: true}}
	router := h.Routes(noLimit, noLimit, noLimit, noLimit, noLimit, noLimit, noLimit, noLimit, noLimit)

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/notifications"},
		{http.MethodGet, "/api/notifications/"},
		{http.MethodGet, "/api/notifications/unread_count"},
		{http.MethodPost, "/api/notifications/9b2f3c1e-6a47-4c1a-8f0e-2d7b5e4a9c10/read"},
	} {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s = %d, want 404 (the notifications inbox routes are retired)",
				tc.method, tc.path, rec.Code)
		}
	}
}
