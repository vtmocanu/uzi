package forge

import (
	"net/http"
	"reflect"
	"testing"
	"time"
)

func TestForgeHTTPTransportDefaults(t *testing.T) {
	transport := newForgeHTTPTransport()
	base := http.DefaultTransport.(*http.Transport)
	if transport == base {
		t.Fatal("forge transport must not mutate the process default")
	}
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
