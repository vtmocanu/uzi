// Package httptransport constructs independent HTTP transports with HTTP/2 health pings.
package httptransport

import (
	"net"
	"net/http"
	"time"
)

// New clones a concrete base transport or uses standard settings for other bases.
func New(base http.RoundTripper) *http.Transport {
	var transport *http.Transport
	if standard, ok := base.(*http.Transport); ok && standard != nil {
		transport = standard.Clone()
	} else {
		// A wrapped default cannot be cloned. Use Go's standard settings rather
		// than panic during package initialization (net/http/transport.go).
		transport = &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: time.Second,
		}
	}
	transport.HTTP2 = &http.HTTP2Config{
		SendPingTimeout: 30 * time.Second,
		PingTimeout:     15 * time.Second,
	}
	return transport
}
