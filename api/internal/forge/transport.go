package forge

import (
	"crypto/tls"
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

// forgeHTTP1Transport carries budgeted CI discovery requests. Over HTTP/2,
// DATA-frame padding and empty DATA frames never reach the response body, so
// the compressed-byte counter in ciCompressedReader would not see them. HTTP/1.1
// delivers every body byte through the body reader, so that counter is the
// network bound. It is cloned from the shared transport so proxy, TLS and dial
// settings match; only protocol negotiation differs.
var forgeHTTP1Transport = newHTTP1Transport(forgeHTTPTransport)

func newHTTP1Transport(base *http.Transport) *http.Transport {
	t := base.Clone()
	t.ForceAttemptHTTP2 = false
	t.HTTP2 = nil
	// A non-nil empty map disables automatic HTTP/2 for TLS connections.
	t.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	// Clone of a transport that already set up HTTP/2 carries "h2" in its ALPN
	// list, which would still negotiate HTTP/2 and then fail to speak it.
	if t.TLSClientConfig == nil {
		t.TLSClientConfig = &tls.Config{}
	}
	t.TLSClientConfig.NextProtos = []string{"http/1.1"}
	return t
}

// ciRouteTransport is the bottom-most transport of timeoutClient: requests
// marked for budgeted CI discovery (isCIJSON) use HTTP/1.1 only, everything else
// uses the shared pool unchanged. Both pools are read at call time so a test
// that swaps forgeHTTPTransport is honored.
type ciRouteTransport struct {
	general, ciJSON func() http.RoundTripper
}

func (r *ciRouteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if isCIJSON(req.Context()) {
		return r.ciJSON().RoundTrip(req)
	}
	return r.general().RoundTrip(req)
}

var forgeRouteTransport = &ciRouteTransport{
	general: func() http.RoundTripper { return forgeHTTPTransport },
	ciJSON:  func() http.RoundTripper { return forgeHTTP1Transport },
}

// forgeBudgetTransport is what timeoutClient uses. A test override receives
// both kinds of request.
func forgeBudgetTransport() http.RoundTripper {
	if forgeTransportOverride != nil {
		return forgeTransportOverride
	}
	return forgeRouteTransport
}
