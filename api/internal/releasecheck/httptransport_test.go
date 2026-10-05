package releasecheck

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestReleaseCheckHTTPTransportHealth(t *testing.T) {
	first, second := newHTTPClient(), newHTTPClient()
	pool, ok := first.Transport.(*http.Transport)
	if !ok || pool == nil {
		t.Fatalf("releasecheck transport = %T, want non-nil *http.Transport", first.Transport)
	}
	if pool != releaseCheckHTTPTransport || second.Transport != pool {
		t.Fatal("releasecheck clients must share the persistent releasecheck pool")
	}
	if first == second {
		t.Fatal("releasecheck must construct separate clients")
	}
	if pool == http.DefaultTransport {
		t.Fatal("releasecheck shares the process default connection pool")
	}
	if pool.HTTP2 == nil || pool.HTTP2.SendPingTimeout != 30*time.Second || pool.HTTP2.PingTimeout != 15*time.Second {
		t.Fatalf("HTTP/2 health configuration = %+v, want 30s send / 15s ping", pool.HTTP2)
	}
	for _, client := range []*http.Client{first, second} {
		if client.Timeout != 15*time.Second {
			t.Errorf("Timeout = %v, want exactly 15s", client.Timeout)
		}
		if client.CheckRedirect == nil {
			t.Fatal("releasecheck client must retain redirect refusal")
		}
		if err := client.CheckRedirect(&http.Request{}, nil); err != http.ErrUseLastResponse {
			t.Errorf("CheckRedirect = %v, want ErrUseLastResponse", err)
		}
	}
}

func TestReleaseCheckHTTPTransportRejectsRedirect(t *testing.T) {
	// Serial: withBaseURL changes the package fetch endpoint.
	var targetRequests atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetRequests.Add(1)
		_, _ = w.Write([]byte(`{"tag_name":"v1.0.0"}`))
	}))
	defer target.Close()

	var sourceRequests atomic.Int64
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sourceRequests.Add(1)
		if r.URL.Path != releasePath {
			t.Errorf("request path = %q, want %q", r.URL.Path, releasePath)
		}
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer source.Close()
	withBaseURL(t, source.URL)

	client := newHTTPClient()
	defer client.CloseIdleConnections()
	_, err := fetchLatest(context.Background(), client, "")
	if err == nil || !strings.Contains(err.Error(), "unexpected status 302") {
		t.Fatalf("fetchLatest error = %v, want rejection of non-200 redirect response", err)
	}
	if got := sourceRequests.Load(); got != 1 {
		t.Errorf("redirect source received %d requests, want 1", got)
	}
	if got := targetRequests.Load(); got != 0 {
		t.Errorf("redirect target received %d requests, want 0", got)
	}
}
