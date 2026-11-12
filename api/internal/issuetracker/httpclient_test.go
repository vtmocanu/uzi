package issuetracker

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestClientDoesNotFollowRedirect(t *testing.T) {
	var hits atomic.Int32
	second := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		hits.Add(1)
	}))
	defer second.Close()
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, second.URL, http.StatusFound)
	}))
	defer first.Close()

	resp, err := newHTTPClient(0, nil).Get(first.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if !isRedirect(resp) {
		t.Fatalf("status %d not detected as redirect", resp.StatusCode)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("redirect target got %d hits", n)
	}
}

func TestIsRedirect(t *testing.T) {
	for code, want := range map[int]bool{
		200: false, 204: false, 299: false,
		300: true, 301: true, 302: true, 304: true, 399: true,
		400: false, 500: false,
	} {
		if got := isRedirect(&http.Response{StatusCode: code}); got != want {
			t.Errorf("isRedirect(%d) = %v, want %v", code, got, want)
		}
	}
}

func TestClientNotModifiedWithoutLocation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotModified)
	}))
	defer srv.Close()
	resp, err := newHTTPClient(0, nil).Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if !isRedirect(resp) {
		t.Fatal("304 not detected")
	}
}

func TestClientTimeout(t *testing.T) {
	if got := newHTTPClient(0, nil).Timeout; got != defaultTimeout {
		t.Fatalf("default timeout = %v", got)
	}
	if got := newHTTPClient(3*time.Second, nil).Timeout; got != 3*time.Second {
		t.Fatalf("timeout = %v", got)
	}
}
