package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	mw "gitlab.example.com/vtmocanu/uzi/api/internal/middleware"
)

// The TLS listener's surface (PRD #58 M3), layer (a).
//
// THE VULNERABILITY THIS CLOSES, because a future reader will otherwise read
// WorkerRoutes as needless duplication of Routes:
//
// mw.ClientIP trusts X-Forwarded-For on the PEER IP alone. On dev-cluster
// TRUSTED_PROXIES is the whole pod CIDR (pod IPs are dynamic; no narrower value is
// maintainable), so hosted worker pods are trusted proxies BY CONSTRUCTION. That
// was inert only while the api NetworkPolicy admitted web pods and nothing else —
// and M3's own Decision 5(a) rule, admitting the worker namespace to the TLS port,
// is what removes that mitigation. Serving the full router there would put
// /api/auth/login inside a compromised worker's reach with a forgeable rate-limit
// key.
//
// A hosted worker is a semi-hostile position BY DESIGN: it runs the agent SDK
// against a user's cloned repo (the reason agent/src/guardrails.ts exists). So the
// route is not mounted, rather than mounted-and-defended.

func tlsRoutes(h *Handler) http.Handler {
	noLimit := mw.NewLimiter(1000, time.Minute, nil)
	return h.WorkerRoutes(noLimit)
}

// The exact set the TLS listener serves, and it is DERIVED, not assumed: the agent
// declares one base (WORKER_API_PREFIX = "/api/worker", agent/src/protocol.ts:12)
// and the controller dials "/api/controller/poll". A grep over agent/src/ finds no
// websocket use at all, so /api/ws is deliberately NOT here — an earlier proposal
// included it.
func TestTLSListenerServesOnlyTheWorkerAndControllerSurface(t *testing.T) {
	h := newControllerHandler(t, &pollStore{}, true)
	srv := tlsRoutes(h)

	// Reachable: these are what the worker and the controller actually dial. They
	// answer 401 (no credential presented), which is the point — they EXIST.
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/api/worker/register"},
		{http.MethodPost, "/api/worker/heartbeat"},
		{http.MethodPost, "/api/worker/runs/claim"},
		{http.MethodGet, "/api/controller/poll"},
	} {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code == http.StatusNotFound {
			t.Errorf("%s %s = 404 on the TLS listener; the worker/controller cannot do their job without it", tc.method, tc.path)
		}
	}

	// NOT reachable. Each of these is a route a compromised hosted worker would
	// reach for, and 404 here means it is not mounted — not that it is defended.
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/api/auth/login"},    // the rate-limit bypass target
		{http.MethodPost, "/api/auth/register"}, // account creation
		{http.MethodGet, "/api/auth/config"},
		{http.MethodGet, "/api/admin/users"},
		{http.MethodGet, "/api/admin/workers"},
		{http.MethodGet, "/api/me"},
		{http.MethodGet, "/api/runs"},
		{http.MethodGet, "/api/ws"}, // the agent never opens one; verified by grep
		{http.MethodGet, "/api/health"},
	} {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s = %d on the TLS listener, want 404 — it must not be MOUNTED there. "+
				"A hosted worker pod is a trusted proxy by construction (TRUSTED_PROXIES is the pod CIDR), "+
				"so any route reachable here has a forgeable per-IP rate-limit key.", tc.method, tc.path, rec.Code)
		}
	}
}

// The plain listener is unchanged: the refactor that extracted the mounts must not
// have narrowed the full router.
func TestThePlainListenerStillServesEverything(t *testing.T) {
	h := newControllerHandler(t, &pollStore{}, true)
	srv := controllerRoutes(h)

	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/api/auth/login"},
		{http.MethodGet, "/api/health"},
		{http.MethodPost, "/api/worker/register"},
		{http.MethodGet, "/api/controller/poll"},
	} {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code == http.StatusNotFound {
			t.Errorf("%s %s = 404 on the PLAIN listener; the subset router must not have narrowed it", tc.method, tc.path)
		}
	}
}

// Decision 12's zero-behavior-change criterion holds on this listener too: with
// hosting off the controller route does not exist here either.
func TestTLSListenerDoesNotMountTheControllerRouteWhenHostingIsOff(t *testing.T) {
	h := newControllerHandler(t, nil, false)
	rec := httptest.NewRecorder()
	tlsRoutes(h).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/controller/poll", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("/api/controller/poll = %d with hosting off, want 404", rec.Code)
	}
}
