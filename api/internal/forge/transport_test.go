package forge

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

func TestForgeHTTPTransportDefaults(t *testing.T) {
	transport := newForgeHTTPTransport(http.DefaultTransport)
	base := http.DefaultTransport.(*http.Transport)
	if transport.HTTP2 == nil || transport.HTTP2.SendPingTimeout != 30*time.Second || transport.HTTP2.PingTimeout != 15*time.Second {
		t.Fatalf("HTTP/2 health configuration: %+v", transport.HTTP2)
	}
	if reflect.ValueOf(transport.Proxy).Pointer() != reflect.ValueOf(base.Proxy).Pointer() ||
		reflect.ValueOf(transport.DialContext).Pointer() != reflect.ValueOf(base.DialContext).Pointer() ||
		!reflect.DeepEqual(transport.TLSClientConfig, base.TLSClientConfig) ||
		transport.ForceAttemptHTTP2 != base.ForceAttemptHTTP2 ||
		transport.TLSHandshakeTimeout != base.TLSHandshakeTimeout ||
		transport.IdleConnTimeout != base.IdleConnTimeout ||
		transport.MaxIdleConns != base.MaxIdleConns ||
		transport.ExpectContinueTimeout != base.ExpectContinueTimeout {
		t.Fatal("forge transport changed default proxy, dial, TLS or pooling settings")
	}
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

func TestForgeHTTPTransportFallback(t *testing.T) {
	standard := http.DefaultTransport.(*http.Transport)
	for _, tc := range []struct {
		name string
		base http.RoundTripper
	}{
		{"wrapped", &wireLog{next: standard}},
		{"nil", nil},
		{"typed_nil", (*http.Transport)(nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Clone normalizes lazy HTTP/2 TLS setup, as the standard default
			// has already done during package initialization.
			transport := newForgeHTTPTransport(tc.base).Clone()
			if reflect.ValueOf(transport.Proxy).Pointer() != reflect.ValueOf(standard.Proxy).Pointer() ||
				transport.DialContext == nil ||
				!reflect.DeepEqual(transport.TLSClientConfig, standard.TLSClientConfig) ||
				transport.ForceAttemptHTTP2 != standard.ForceAttemptHTTP2 ||
				transport.TLSHandshakeTimeout != standard.TLSHandshakeTimeout ||
				transport.IdleConnTimeout != standard.IdleConnTimeout ||
				transport.MaxIdleConns != standard.MaxIdleConns ||
				transport.ExpectContinueTimeout != standard.ExpectContinueTimeout {
				t.Fatal("fallback changed the standard proxy, TLS, HTTP/2 or pooling defaults")
			}
			if transport.HTTP2 == nil || transport.HTTP2.SendPingTimeout != 30*time.Second || transport.HTTP2.PingTimeout != 15*time.Second {
				t.Fatalf("fallback lost health pings: %+v", transport.HTTP2)
			}
		})
	}
}
