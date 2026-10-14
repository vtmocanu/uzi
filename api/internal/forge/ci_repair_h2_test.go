package forge

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// h2Server is a TLS test server that negotiates HTTP/2 and reports the
// protocol major version it served over.
func h2Server(t *testing.T) (*httptest.Server, *x509.CertPool) {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"proto":`+strconv.Itoa(r.ProtoMajor)+`}`)
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	return srv, pool
}

// withTrustedPools swaps both production pools for clones trusting the server,
// built the same way as production (the HTTP/1.1 pool via newHTTP1Transport).
func withTrustedPools(t *testing.T, roots *x509.CertPool) {
	t.Helper()
	prevGeneral, prevH1 := forgeHTTPTransport, forgeHTTP1Transport
	general := prevGeneral.Clone()
	general.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	forgeHTTPTransport = general
	forgeHTTP1Transport = newHTTP1Transport(general)
	t.Cleanup(func() {
		general.CloseIdleConnections()
		forgeHTTP1Transport.CloseIdleConnections()
		forgeHTTPTransport, forgeHTTP1Transport = prevGeneral, prevH1
	})
}

func TestCIBudgetedRequestsUseHTTP1AndOthersKeepHTTP2(t *testing.T) {
	srv, roots := h2Server(t)
	withTrustedPools(t, roots)
	client := timeoutClient(5 * time.Second)

	get := func(ctx context.Context) int {
		t.Helper()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.ProtoMajor
	}
	if got := get(context.Background()); got != 2 {
		t.Fatalf("unmarked request ProtoMajor = %d, want 2", got)
	}
	budgeted := markCIJSON(WithCIRepairReadLimits(context.Background()))
	if got := get(budgeted); got != 1 {
		t.Fatalf("budgeted request ProtoMajor = %d, want 1", got)
	}
	// Marked without an installed budget is not budgeted discovery.
	if got := get(markCIJSON(context.Background())); got != 2 {
		t.Fatalf("marked-without-budget ProtoMajor = %d, want 2", got)
	}
}

func TestCIRouteTransportRoutesByBudgetedContext(t *testing.T) {
	var hit string
	mk := func(name string) func() http.RoundTripper {
		return func() http.RoundTripper {
			return ciRoundTripper(func(*http.Request) (*http.Response, error) {
				hit = name
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: http.NoBody}, nil
			})
		}
	}
	r := &ciRouteTransport{general: mk("general"), ciJSON: mk("ci")}
	for name, tc := range map[string]struct {
		ctx  context.Context
		want string
	}{
		"plain":           {context.Background(), "general"},
		"marked no limit": {markCIJSON(context.Background()), "general"},
		"budgeted":        {markCIJSON(WithCIRepairReadLimits(context.Background())), "ci"},
	} {
		req, _ := http.NewRequestWithContext(tc.ctx, http.MethodGet, "https://example.test", nil)
		if _, err := r.RoundTrip(req); err != nil || hit != tc.want {
			t.Fatalf("%s: routed to %q (err %v), want %q", name, hit, err, tc.want)
		}
	}
}

func TestCIOverrideReceivesBothKinds(t *testing.T) {
	prev := forgeTransportOverride
	forgeTransportOverride = &wireLog{}
	t.Cleanup(func() { forgeTransportOverride = prev })
	if timeoutClient(time.Second).Transport != forgeTransportOverride {
		t.Fatal("override must replace the routing transport")
	}
}

func TestCIHTTP1PoolSharesSettingsWithGeneralPool(t *testing.T) {
	if forgeHTTP1Transport.ForceAttemptHTTP2 || forgeHTTP1Transport.TLSNextProto == nil || len(forgeHTTP1Transport.TLSNextProto) != 0 {
		t.Fatal("budgeted pool must have HTTP/2 disabled")
	}
	if (forgeHTTP1Transport.Proxy == nil) != (forgeHTTPTransport.Proxy == nil) ||
		forgeHTTP1Transport.TLSHandshakeTimeout != forgeHTTPTransport.TLSHandshakeTimeout ||
		forgeHTTP1Transport.IdleConnTimeout != forgeHTTPTransport.IdleConnTimeout {
		t.Fatal("budgeted pool diverges from the general pool beyond protocol negotiation")
	}
}
