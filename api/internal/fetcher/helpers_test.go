package fetcher

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// testCA is a throwaway CA for upstream test servers.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pool *x509.CertPool
}

func newTestCA(t *testing.T) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "uzi fetcher test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &testCA{cert: cert, key: key, pool: pool}
}

// leaf issues a server certificate for hosts valid over [notBefore, notAfter].
func (ca *testCA) leaf(t *testing.T, hosts []string, notBefore, notAfter time.Time) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: hosts[0]},
		DNSNames:     hosts,
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func (ca *testCA) validLeaf(t *testing.T, hosts ...string) tls.Certificate {
	return ca.leaf(t, hosts, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
}

// upstream starts a TLS test server presenting cert and returns it with its port.
func upstream(t *testing.T, cert tls.Certificate, h http.Handler) (*httptest.Server, uint16) {
	t.Helper()
	srv := httptest.NewUnstartedServer(h)
	// Refused handshakes are the point of several tests; keep their log noise out.
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	_, portStr, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil {
		t.Fatal(err)
	}
	return srv, uint16(port)
}

// Public test addresses. They pass the production AddressPolicy (no test exception
// exists); the testDial seam routes the connection to them to an httptest server.
const (
	publicV4  = "93.184.215.14"
	publicV4b = "93.184.215.15"
	publicV6  = "2606:2800:21f:cb07:6820:80da:af6b:8b2c"
)

// fakeResolver answers from a table; answer(host, call) may vary per call (rebinding).
// It refuses a name without the trailing dot, so a lookup that is not fully qualified
// fails loudly. hosts are recorded and answered without the dot.
type fakeResolver struct {
	mu     sync.Mutex
	calls  map[string]int
	answer func(host string, call int) []netip.Addr
}

func newResolver(answer func(host string, call int) []netip.Addr) *fakeResolver {
	return &fakeResolver{calls: map[string]int{}, answer: answer}
}

// staticResolver resolves every listed host to addrs and every other host to nothing.
func staticResolver(addrs map[string][]string) *fakeResolver {
	return newResolver(func(host string, _ int) []netip.Addr {
		var out []netip.Addr
		for _, s := range addrs[host] {
			out = append(out, netip.MustParseAddr(s))
		}
		return out
	})
}

// publicResolver resolves every host to publicV4.
func publicResolver() *fakeResolver {
	return newResolver(func(string, int) []netip.Addr { return []netip.Addr{netip.MustParseAddr(publicV4)} })
}

func (r *fakeResolver) LookupNetIP(_ context.Context, network, name string) ([]netip.Addr, error) {
	if network != "ip" {
		return nil, errors.New("unexpected network " + network)
	}
	host, ok := strings.CutSuffix(name, ".")
	if !ok {
		return nil, errors.New("lookup of " + name + " is not fully qualified")
	}
	r.mu.Lock()
	r.calls[host]++
	n := r.calls[host]
	r.mu.Unlock()
	addrs := r.answer(host, n)
	if len(addrs) == 0 {
		return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
	}
	return addrs, nil
}

func (r *fakeResolver) count(host string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[host]
}

func (r *fakeResolver) total() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, c := range r.calls {
		n += c
	}
	return n
}

// dialLog records every address the fetcher connected to through testDial.
type dialLog struct {
	mu    sync.Mutex
	addrs []netip.AddrPort
}

func (d *dialLog) add(ap netip.AddrPort) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.addrs = append(d.addrs, ap)
}

func (d *dialLog) all() []netip.AddrPort {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]netip.AddrPort(nil), d.addrs...)
}

// testFetcher builds a Fetcher with the production AddressPolicy that trusts ca and
// routes every policy-approved connection to 127.0.0.1:port (an httptest server); port 0
// means no upstream, and any dial fails. It returns the log of approved addresses.
func testFetcher(res Resolver, ca *testCA, port uint16, mut func(*Options)) (*Fetcher, *dialLog) {
	log := &dialLog{}
	opts := Options{Resolver: res, MaxFileBytes: 1 << 20}
	if ca != nil {
		opts.RootCAs = ca.pool
	}
	opts.testDial = func(ctx context.Context, ap netip.AddrPort) (net.Conn, error) {
		log.add(ap)
		if port == 0 {
			return nil, errors.New("test: no upstream")
		}
		var d net.Dialer
		return d.DialContext(ctx, "tcp", netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), port).String())
	}
	if mut != nil {
		mut(&opts)
	}
	return New(opts), log
}

// fetcherFor is testFetcher without the dial log.
func fetcherFor(res Resolver, ca *testCA, port uint16) *Fetcher {
	f, _ := testFetcher(res, ca, port, nil)
	return f
}

// fakeControl records every Begin and Complete.
type fakeControl struct {
	mu          sync.Mutex
	entries     []string
	maxBytes    int64
	beginErr    error
	completeErr error
	begins      []string
	completes   []AttemptRecord
	creds       []string
}

func newControl(entries ...string) *fakeControl {
	return &fakeControl{entries: entries, maxBytes: 1 << 20}
}

func (c *fakeControl) Begin(_ context.Context, credential, url string) (Admission, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.begins = append(c.begins, url)
	c.creds = append(c.creds, credential)
	if c.beginErr != nil {
		return Admission{}, c.beginErr
	}
	return Admission{ReservationID: "res-" + strconv.Itoa(len(c.begins)), Entries: c.entries, MaxBytes: c.maxBytes}, nil
}

func (c *fakeControl) Complete(_ context.Context, credential string, rec AttemptRecord) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.creds = append(c.creds, credential)
	c.completes = append(c.completes, rec)
	return c.completeErr
}

// testCredential is a fake per-run fetch credential, assembled at runtime.
var testCredential = strings.Repeat("c", 8) + "-run-credential"

// doFetch sends one worker request to s.
func doFetch(t *testing.T, s *Server, cred, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/fetch", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if cred != "" {
		req.Header.Set("Authorization", "Bearer "+cred)
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

func urlBody(t *testing.T, u string) string {
	t.Helper()
	b, err := json.Marshal(apitypes.FetchRequest{URL: u})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
