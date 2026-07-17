package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	mw "gitlab.example.com/vtmocanu/uzi/api/internal/middleware"
)

// stripXFF (PRD #58 M3), layer (b) of two.
//
// These tests assert the BYPASS is closed, not merely that a header is absent:
// each one runs the real mw.ClientIP against the real pod-CIDR TRUSTED_PROXIES
// value from deploy/values/dev-cluster.yaml, from a peer IP inside it. Asserting
// only "the header is gone" would pass against a ClientIP that had stopped reading
// XFF for some other reason, and would not notice if it started again.

// podCIDR is the dev-cluster value, verbatim: TRUSTED_PROXIES: "10.244.0.0/16"
// (deploy/values/dev-cluster.yaml). It is the whole pod CIDR because pod IPs are
// dynamic, which is exactly why hosted workers land inside it.
func podCIDR(t *testing.T) []*net.IPNet {
	t.Helper()
	_, n, err := net.ParseCIDR("10.244.0.0/16")
	if err != nil {
		t.Fatalf("parse cidr: %v", err)
	}
	return []*net.IPNet{n}
}

// workerPeer is a hosted worker pod's address — inside the pod CIDR, hence a
// trusted proxy by construction.
const workerPeer = "192.168.42.7:54321"

// The bug, demonstrated: WITHOUT the strip, a worker chooses its own rate-limit
// key. This test is what gives the fix its meaning — if it ever stops failing on
// the unstripped handler, the premise has changed and the strip may be revisited.
func TestWithoutStripXFFAWorkerPodForgesItsClientIP(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", nil)
	req.RemoteAddr = workerPeer
	req.Header.Set("X-Forwarded-For", "203.0.113.9")

	got := mw.ClientIP(req, podCIDR(t))
	if got != "203.0.113.9" {
		t.Fatalf("ClientIP = %q, want the attacker-chosen 203.0.113.9 — this test documents the "+
			"vulnerability stripXFF exists to close; if it no longer holds, re-derive the fix", got)
	}
}

// The fix: through stripXFF, the same request keys on the pod's REAL address, so
// rotating XFF cannot mint fresh rate-limit buckets and the audit log cannot be
// fed a forged IP.
func TestStripXFFPinsTheClientIPToTheRealPeer(t *testing.T) {
	var seen string
	h := stripXFF(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = mw.ClientIP(r, podCIDR(t))
	}))

	req := httptest.NewRequest(http.MethodPost, "/api/worker/register", nil)
	req.RemoteAddr = workerPeer
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if seen != "192.168.42.7" {
		t.Fatalf("ClientIP = %q, want the worker's real peer 192.168.42.7", seen)
	}
}

// Rotating the header — the actual attack shape — must not move the key.
func TestStripXFFDefeatsARotatingForgedHeader(t *testing.T) {
	keys := map[string]bool{}
	h := stripXFF(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		keys[mw.ClientIP(r, podCIDR(t))] = true
	}))

	for _, forged := range []string{"203.0.113.1", "203.0.113.2", "198.51.100.7", "10.0.0.1, 203.0.113.3"} {
		req := httptest.NewRequest(http.MethodPost, "/api/auth/login", nil)
		req.RemoteAddr = workerPeer
		req.Header.Set("X-Forwarded-For", forged)
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
	if len(keys) != 1 || !keys["192.168.42.7"] {
		t.Fatalf("rate-limit keys = %v, want exactly one: the worker's real peer. "+
			"More than one means a worker can still mint fresh per-IP buckets at will.", keys)
	}
}

// Every forwarding header goes, not just the one ClientIP reads today, so a future
// middleware that reads X-Real-IP or Forwarded cannot inherit the hole.
func TestStripXFFRemovesEveryForwardingHeader(t *testing.T) {
	h := stripXFF(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		for _, k := range []string{"X-Forwarded-For", "X-Real-IP", "Forwarded"} {
			if v := r.Header.Get(k); v != "" {
				t.Errorf("%s survived stripXFF: %q", k, v)
			}
		}
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/controller/poll", nil)
	req.RemoteAddr = workerPeer
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	req.Header.Set("X-Real-IP", "203.0.113.9")
	req.Header.Set("Forwarded", "for=203.0.113.9")
	h.ServeHTTP(httptest.NewRecorder(), req)
}

// The strip must not disturb anything else about the request — it is a header
// deletion, not a filter.
func TestStripXFFPassesTheRequestThroughUntouched(t *testing.T) {
	called := false
	h := stripXFF(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if r.Header.Get("Authorization") != "Bearer uzw_token" {
			t.Errorf("Authorization was disturbed: %q", r.Header.Get("Authorization"))
		}
		if r.URL.Path != "/api/worker/heartbeat" {
			t.Errorf("path was disturbed: %q", r.URL.Path)
		}
		w.WriteHeader(http.StatusTeapot)
	}))
	req := httptest.NewRequest(http.MethodPost, "/api/worker/heartbeat", nil)
	req.Header.Set("Authorization", "Bearer uzw_token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !called || rec.Code != http.StatusTeapot {
		t.Fatalf("called=%v code=%d, want the handler reached and its response intact", called, rec.Code)
	}
}
