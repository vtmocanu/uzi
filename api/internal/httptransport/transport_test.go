package httptransport_test

import (
	"crypto/tls"
	"net"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/httptransport"
)

// standardTransport is independent of the mutable process default. Clone
// normalizes net/http's lazy HTTP/2 TLS setup before settings are compared.
func standardTransport() *http.Transport {
	return (&http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}).Clone()
}

func assertHealth(t *testing.T, transport *http.Transport) {
	t.Helper()
	if transport.HTTP2 == nil || transport.HTTP2.SendPingTimeout != 30*time.Second || transport.HTTP2.PingTimeout != 15*time.Second {
		t.Fatalf("HTTP/2 health configuration: %+v", transport.HTTP2)
	}
}

func assertSettings(t *testing.T, transport, base *http.Transport) {
	t.Helper()
	if reflect.ValueOf(transport.Proxy).Pointer() != reflect.ValueOf(base.Proxy).Pointer() ||
		reflect.ValueOf(transport.DialContext).Pointer() != reflect.ValueOf(base.DialContext).Pointer() ||
		!reflect.DeepEqual(transport.TLSClientConfig, base.TLSClientConfig) ||
		transport.ForceAttemptHTTP2 != base.ForceAttemptHTTP2 ||
		transport.TLSHandshakeTimeout != base.TLSHandshakeTimeout ||
		transport.IdleConnTimeout != base.IdleConnTimeout ||
		transport.MaxIdleConns != base.MaxIdleConns ||
		transport.MaxIdleConnsPerHost != base.MaxIdleConnsPerHost ||
		transport.MaxConnsPerHost != base.MaxConnsPerHost ||
		transport.ResponseHeaderTimeout != base.ResponseHeaderTimeout ||
		transport.DisableCompression != base.DisableCompression ||
		transport.ExpectContinueTimeout != base.ExpectContinueTimeout {
		t.Fatal("transport changed inherited proxy, dial, TLS or pooling settings")
	}
}

func TestNewDefaults(t *testing.T) {
	base := standardTransport()
	beforeHTTP2 := *base.HTTP2
	transport := httptransport.New(base)
	assertHealth(t, transport)
	assertSettings(t, transport, base)
	if transport == base {
		t.Fatal("constructor returned the input transport")
	}
	if !reflect.DeepEqual(*base.HTTP2, beforeHTTP2) {
		t.Fatalf("constructor changed input HTTP/2 configuration: %+v", base.HTTP2)
	}
}

func TestNewFallback(t *testing.T) {
	standard := standardTransport()
	for _, tc := range []struct {
		name string
		base http.RoundTripper
	}{
		{"wrapped", struct{ http.RoundTripper }{standard}},
		{"nil", nil},
		{"typed_nil", (*http.Transport)(nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			transport := httptransport.New(tc.base).Clone()
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
			assertHealth(t, transport)
		})
	}
}

func TestNewInheritedSettingsAndIndependentInstances(t *testing.T) {
	base := standardTransport()
	base.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, ServerName: "example.com"}
	base.ForceAttemptHTTP2 = false
	base.TLSHandshakeTimeout = 3 * time.Second
	base.IdleConnTimeout = 4 * time.Second
	base.MaxIdleConns = 7
	base.MaxIdleConnsPerHost = 8
	base.MaxConnsPerHost = 9
	base.ResponseHeaderTimeout = 5 * time.Second
	base.ExpectContinueTimeout = 2 * time.Second
	base.DisableCompression = true
	originalHTTP2 := http.HTTP2Config{SendPingTimeout: time.Minute, PingTimeout: 2 * time.Minute}
	base.HTTP2 = &originalHTTP2
	beforeHTTP2 := originalHTTP2

	first := httptransport.New(base)
	second := httptransport.New(base)
	assertSettings(t, first, base)
	assertSettings(t, second, base)
	assertHealth(t, first)
	assertHealth(t, second)
	if first == second || first == base || second == base {
		t.Fatal("constructor must return independent transports")
	}
	if first.HTTP2 == second.HTTP2 || first.HTTP2 == base.HTTP2 || second.HTTP2 == base.HTTP2 {
		t.Fatal("transports must have independent HTTP/2 configurations")
	}
	if first.TLSClientConfig == second.TLSClientConfig || first.TLSClientConfig == base.TLSClientConfig || second.TLSClientConfig == base.TLSClientConfig {
		t.Fatal("transports must have independent TLS configurations")
	}
	first.HTTP2.SendPingTimeout = time.Second
	first.HTTP2.PingTimeout = time.Second
	first.TLSClientConfig.ServerName = "changed.example.com"
	first.MaxIdleConns = 1
	assertHealth(t, second)
	assertSettings(t, second, base)
	if base.HTTP2 != &originalHTTP2 || !reflect.DeepEqual(*base.HTTP2, beforeHTTP2) {
		t.Fatalf("constructor or output mutation changed input HTTP/2 configuration: %+v", base.HTTP2)
	}
}
