package forge

import (
	"net/http"
	"time"
)

// Keep one pool across driver rebuilds, without changing process-global HTTP
// behavior. Clone preserves the standard proxy, TLS and HTTP/2 negotiation.
var forgeTransportBase = http.DefaultTransport.(*http.Transport)
var forgeHTTPTransport = newForgeHTTPTransport()

// Package-private injection for wire-recording tests; unset in production.
var forgeTransportOverride http.RoundTripper

func newForgeHTTPTransport() *http.Transport {
	transport := forgeTransportBase.Clone()
	transport.HTTP2 = &http.HTTP2Config{
		SendPingTimeout: 30 * time.Second,
		PingTimeout:     15 * time.Second,
	}
	return transport
}

func forgeTransport() http.RoundTripper {
	if forgeTransportOverride != nil {
		return forgeTransportOverride
	}
	return forgeHTTPTransport
}
