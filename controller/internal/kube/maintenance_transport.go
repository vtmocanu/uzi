package kube

import (
	"context"
	"net/http"
)

type maintenanceReadKey struct{}

func markMaintenanceRead(ctx context.Context) context.Context {
	return context.WithValue(ctx, maintenanceReadKey{}, true)
}

type maintenanceReadTransport struct{ next http.RoundTripper }

// WrapMaintenanceReads caps actual response bytes only for maintenance reads.
// The underlying transport's decompressed body is capped, regardless of headers.
func WrapMaintenanceReads(next http.RoundTripper) http.RoundTripper {
	return maintenanceReadTransport{next: next}
}

func (t maintenanceReadTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.next.RoundTrip(req)
	if resp != nil && resp.Body != nil && req.Context().Value(maintenanceReadKey{}) == true {
		resp.Body = http.MaxBytesReader(nil, resp.Body, 8<<20)
	}
	return resp, err
}
