package forge

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestForgeHTTPTransportDefaults(t *testing.T) {
	first := timeoutClient(time.Second)
	second := timeoutClient(2 * time.Second)
	if first.Transport != second.Transport || first.Transport != ancestryClient(time.Second).Transport {
		t.Fatal("rebuilt clients must share the forge pool")
	}

	server := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(server.Close)
	githubDriver, err := newGitHub(server.URL, "transport-test-token", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if githubDriver.logClient.Transport != forgeHTTPTransport {
		t.Fatal("GitHub log client does not share the forge pool")
	}
	gitlabDriver, err := newGitLab(server.URL, "transport-test-token", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if gitlabDriver.logClient.Transport != forgeHTTPTransport {
		t.Fatal("GitLab log client does not share the forge pool")
	}
}

func TestForgeTransportIndependentOfDefaultReplacement(t *testing.T) {
	original := http.DefaultTransport
	http.DefaultTransport = &wireLog{}
	t.Cleanup(func() { http.DefaultTransport = original })
	if forgeTransport() != forgeHTTPTransport {
		t.Fatal("replacing the process default disabled forge health pings")
	}
	if timeoutClient(time.Second).Transport != forgeHTTPTransport {
		t.Fatal("a rebuilt client lost the forge pool after default replacement")
	}
}

func TestForgeProductionPoolHealth(t *testing.T) {
	var pool http.RoundTripper = forgeHTTPTransport
	transport, ok := pool.(*http.Transport)
	if !ok || transport == nil {
		t.Fatalf("production forge pool must be a concrete transport, got %T", pool)
	}
	if pool == http.DefaultTransport {
		t.Fatal("production forge pool must be independent of the process default")
	}
	if transport.HTTP2 == nil || transport.HTTP2.SendPingTimeout != 30*time.Second || transport.HTTP2.PingTimeout != 15*time.Second {
		t.Fatalf("production HTTP/2 health configuration: %+v", transport.HTTP2)
	}
}
