// Package youtracktest is an in-process HTTPS fake of the YouTrack REST API,
// modelling only the routes the issuetracker YouTrack driver uses. A route it
// does not model answers 501 and fails the test at cleanup, so an unmodelled
// call is loud rather than silently green.
package youtracktest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
)

// Request is one request the server received.
type Request struct {
	Method string
	// Path is the escaped request path, without the query.
	Path     string
	RawQuery string
}

// Server is the fake. Build it with NewServer; the embedded httptest.Server
// provides URL and Client (which trusts the server's certificate).
type Server struct {
	*httptest.Server
	// Token is the bearer token the server accepts.
	Token string

	t   testing.TB
	mux *http.ServeMux // built once by NewServer
	mu  sync.Mutex     // guards every field below
	m   *model
	// requests logs every request in arrival order.
	requests   []Request
	unmodelled []string
}

// NewServer starts the fake over TLS and registers its cleanup. The
// cleanup fails the test when a request reached an unmodelled route.
func NewServer(t testing.TB) *Server {
	t.Helper()
	s := &Server{
		t:     t,
		m:     newModel(),
		Token: strings.Join([]string{"perm", ":", "fixture-", "id.", "Yk3mQ8vR2xTn5Lw7"}, ""),
	}
	s.mux = s.routes()
	s.Server = httptest.NewTLSServer(http.HandlerFunc(s.serve))
	t.Cleanup(func() {
		s.Close()
		s.mu.Lock()
		defer s.mu.Unlock()
		if len(s.unmodelled) > 0 {
			t.Errorf("youtracktest: requests reached routes the fake does not model:\n  %s", strings.Join(s.unmodelled, "\n  "))
		}
	})
	return s
}

// Requests returns a copy of the request log.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.requests)
}

// ResetRequests empties the request log.
func (s *Server) ResetRequests() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = nil
}

// SetStateField renames the State custom field, so a test can prove the driver
// honours Options.StateField.
func (s *Server) SetStateField(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m.stateField = name
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, Request{Method: r.Method, Path: r.URL.EscapedPath(), RawQuery: r.URL.RawQuery})
	if r.Header.Get("Authorization") != "Bearer "+s.Token {
		writeError(w, http.StatusUnauthorized, "Unauthorized", "invalid or missing bearer token")
		return
	}
	s.mux.ServeHTTP(w, r)
}

func (s *Server) unmodelledRoute(w http.ResponseWriter, r *http.Request) {
	s.unmodelled = append(s.unmodelled, r.Method+" "+r.URL.EscapedPath())
	writeError(w, http.StatusNotImplemented, "Not Implemented", "route not modelled by youtracktest")
}

func writeError(w http.ResponseWriter, status int, kind, description string) {
	writeJSON(w, status, map[string]any{"error": kind, "error_description": description})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json;charset=UTF-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
