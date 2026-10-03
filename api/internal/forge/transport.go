package forge

import (
	"net/http"

	"github.com/vtmocanu/uzi/api/internal/httptransport"
)

// Keep one pool across driver rebuilds, without changing process-global HTTP
// behavior. Clone preserves the standard proxy, TLS and HTTP/2 negotiation.
var forgeHTTPTransport = httptransport.New(http.DefaultTransport)

// Package-private injection for wire-recording tests; unset in production.
var forgeTransportOverride http.RoundTripper

func forgeTransport() http.RoundTripper {
	if forgeTransportOverride != nil {
		return forgeTransportOverride
	}
	return forgeHTTPTransport
}
