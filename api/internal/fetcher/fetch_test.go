package fetcher

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var pdfBytes = []byte("%PDF-1.7\n1 0 obj << /Type /Catalog >> endobj\ntrailer << /Root 1 0 R >>\n%%EOF\n")

func pdfHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/pdf")
	_, _ = w.Write(pdfBytes)
}

func mustRefuse(t *testing.T, res *Result, ref *Refusal, reason string) *Refusal {
	t.Helper()
	if res != nil {
		t.Fatalf("got content (%d bytes from %s), want refusal %q", len(res.Body), res.FinalURL, reason)
	}
	if ref == nil || ref.Reason != reason {
		t.Fatalf("refusal = %+v, want reason %q", ref, reason)
	}
	return ref
}

func TestFetchAllowedPDF(t *testing.T) {
	ca := newTestCA(t)
	var gotAE, gotHost, gotUA atomic.Value
	_, port := upstream(t, ca.validLeaf(t, "docs.example.com"), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAE.Store(r.Header.Get("Accept-Encoding"))
		gotHost.Store(r.Host)
		gotUA.Store(r.UserAgent())
		pdfHandler(w, r)
	}))
	f := fetcherFor(publicResolver(), ca, port)
	res, ref := f.Fetch(context.Background(), "https://docs.example.com/guide.pdf?v=2#frag", []string{"docs.example.com"}, 1<<20)
	if ref != nil {
		t.Fatalf("refused: %+v", ref)
	}
	want := sha256.Sum256(pdfBytes)
	if res.SHA256 != hex.EncodeToString(want[:]) {
		t.Errorf("sha256 = %s", res.SHA256)
	}
	if !bytes.Equal(res.Body, pdfBytes) || res.ContentType != "application/pdf" || res.HTTPStatus != 200 {
		t.Errorf("result = %+v", res)
	}
	if res.FinalURL != "https://docs.example.com/guide.pdf?v=2" {
		t.Errorf("final URL = %q (fragment must be dropped)", res.FinalURL)
	}
	if gotAE.Load() != "identity" {
		t.Errorf("Accept-Encoding = %v, want identity", gotAE.Load())
	}
	if gotHost.Load() != "docs.example.com" {
		t.Errorf("Host = %v", gotHost.Load())
	}
	if gotUA.Load() != "uzi-fetcher" {
		t.Errorf("User-Agent = %v", gotUA.Load())
	}
}

// A host that differs from its normalized form (a soft hyphen, upper case, a trailing
// dot) is resolved, dialed and verified as the normalized name: the certificate is for
// docs.example.com only, so verifying against the raw string would fail.
func TestFetchUsesNormalizedHostEverywhere(t *testing.T) {
	ca := newTestCA(t)
	var gotHost atomic.Value
	_, port := upstream(t, ca.validLeaf(t, "docs.example.com"), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost.Store(r.Host)
		pdfHandler(w, r)
	}))
	res := publicResolver()
	f := fetcherFor(res, ca, port)
	for _, raw := range []string{"https://DOCS.Example.COM./a", "https://docs\u00ad.example.com/a"} {
		r, ref := f.Fetch(context.Background(), raw, []string{"docs.example.com"}, 1<<20)
		if ref != nil {
			t.Fatalf("%q refused: %+v", raw, ref)
		}
		if r.FinalURL != "https://docs.example.com/a" {
			t.Errorf("%q: final URL %q", raw, r.FinalURL)
		}
		if gotHost.Load() != "docs.example.com" {
			t.Errorf("%q: Host header %v", raw, gotHost.Load())
		}
	}
	if res.total() != res.count("docs.example.com") || res.total() != 2 {
		t.Errorf("resolver saw a non-normalized name: %v", res.calls)
	}
}

func TestFetchOffListHost(t *testing.T) {
	res := publicResolver()
	f := fetcherFor(res, nil, 0)
	r, ref := f.Fetch(context.Background(), "https://evil.example.net/x", []string{"docs.example.com", "*.example.org"}, 1<<20)
	mustRefuse(t, r, ref, ReasonOffList)
	if ref.FinalURL != "https://evil.example.net/x" {
		t.Errorf("final URL = %q", ref.FinalURL)
	}
	if res.total() != 0 {
		t.Error("an off-list host was resolved")
	}
	// A wildcard does not cover its apex.
	r, ref = f.Fetch(context.Background(), "https://example.org/", []string{"*.example.org"}, 1<<20)
	mustRefuse(t, r, ref, ReasonOffList)
}

func TestFetchURLChecks(t *testing.T) {
	f := fetcherFor(publicResolver(), nil, 0)
	entries := []string{"docs.example.com"}
	cases := map[string]string{
		"http://docs.example.com/":            ReasonNotHTTPS,
		"ftp://docs.example.com/":             ReasonNotHTTPS,
		"docs.example.com/x":                  ReasonNotHTTPS,
		"https://user:pw@docs.example.com/":   ReasonUserinfo,
		"https://user@docs.example.com/":      ReasonUserinfo,
		"https://93.184.216.34/":              ReasonIPLiteral,
		"https://[2606:2800:220:1::]/":        ReasonIPLiteral,
		"https://docs.example.com:8443/":      ReasonPort,
		"https:///path":                       ReasonInvalidURL,
		"https://docs_example.com/":           ReasonInvalidURL,
		"https://docs.example.com/%zz":        ReasonInvalidURL,
		"https://\uff19\uff13.184.216.34/":    ReasonIPLiteral,
		"https://*.example.com/":              ReasonInvalidURL,
		"https://docs.example.com.evil.net/":  ReasonOffList,
		"https://docs.example.com\\@evil.net": ReasonInvalidURL,
	}
	for raw, want := range cases {
		r, ref := f.Fetch(context.Background(), raw, entries, 1<<20)
		if r != nil || ref == nil || ref.Reason != want {
			t.Errorf("%q: got %+v, want %q", raw, ref, want)
		}
	}
}

func TestFetchExplicitPort443Allowed(t *testing.T) {
	ca := newTestCA(t)
	_, port := upstream(t, ca.validLeaf(t, "docs.example.com"), http.HandlerFunc(pdfHandler))
	f := fetcherFor(publicResolver(), ca, port)
	r, ref := f.Fetch(context.Background(), "https://docs.example.com:443/a", []string{"docs.example.com"}, 1<<20)
	if ref != nil {
		t.Fatalf("refused: %+v", ref)
	}
	if r.FinalURL != "https://docs.example.com/a" {
		t.Errorf("final URL %q", r.FinalURL)
	}
}

func TestFetchRedirects(t *testing.T) {
	ca := newTestCA(t)
	var hits sync.Map
	_, port := upstream(t, ca.validLeaf(t, "docs.example.com", "files.example.com", "evil.example.net"), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v, _ := hits.LoadOrStore(r.Host, new(atomic.Int32))
		v.(*atomic.Int32).Add(1)
		switch r.URL.Path {
		case "/to-files":
			http.Redirect(w, r, "https://files.example.com/doc.pdf", http.StatusFound)
		case "/relative":
			w.Header().Set("Location", "/doc.pdf")
			w.WriteHeader(http.StatusMovedPermanently)
			_, _ = w.Write(bytes.Repeat([]byte("x"), 1<<20))
		case "/to-evil":
			http.Redirect(w, r, "https://evil.example.net/steal", http.StatusTemporaryRedirect)
		case "/protocol-relative-evil":
			w.Header().Set("Location", "//evil.example.net/steal")
			w.WriteHeader(http.StatusPermanentRedirect)
		case "/to-http":
			http.Redirect(w, r, "http://files.example.com/doc.pdf", http.StatusFound)
		case "/to-userinfo":
			http.Redirect(w, r, "https://u:p@files.example.com/doc.pdf", http.StatusFound)
		case "/to-ip":
			http.Redirect(w, r, "https://127.0.0.1/doc.pdf", http.StatusFound)
		case "/no-location":
			w.WriteHeader(http.StatusFound)
		case "/doc.pdf":
			pdfHandler(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	f := fetcherFor(publicResolver(), ca, port)
	entries := []string{"docs.example.com", "files.example.com"}

	r, ref := f.Fetch(context.Background(), "https://docs.example.com/to-files", entries, 1<<20)
	if ref != nil || r.FinalURL != "https://files.example.com/doc.pdf" {
		t.Fatalf("on-list redirect: %+v %+v", r, ref)
	}
	r, ref = f.Fetch(context.Background(), "https://docs.example.com/relative", entries, 1<<20)
	if ref != nil || r.FinalURL != "https://docs.example.com/doc.pdf" || !bytes.Equal(r.Body, pdfBytes) {
		t.Fatalf("relative redirect: %+v %+v", r, ref)
	}

	for path, want := range map[string]string{
		"/to-evil":                ReasonRedirectOffList,
		"/protocol-relative-evil": ReasonRedirectOffList,
		"/to-http":                ReasonNotHTTPS,
		"/to-userinfo":            ReasonUserinfo,
		"/to-ip":                  ReasonIPLiteral,
		"/no-location":            ReasonRedirectInvalid,
	} {
		r, ref := f.Fetch(context.Background(), "https://docs.example.com"+path, entries, 1<<20)
		mustRefuse(t, r, ref, want)
		if want == ReasonRedirectOffList && ref.FinalURL != "https://evil.example.net/steal" {
			t.Errorf("%s: final URL %q, want the refused hop", path, ref.FinalURL)
		}
	}
	if v, ok := hits.Load("evil.example.net"); ok && v.(*atomic.Int32).Load() > 0 {
		t.Error("an off-list redirect target was contacted")
	}
}

func TestFetchTooManyRedirects(t *testing.T) {
	ca := newTestCA(t)
	var hits atomic.Int32
	_, port := upstream(t, ca.validLeaf(t, "docs.example.com"), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		http.Redirect(w, r, fmt.Sprintf("/hop/%d", n), http.StatusFound)
	}))
	f := fetcherFor(publicResolver(), ca, port)
	r, ref := f.Fetch(context.Background(), "https://docs.example.com/", []string{"docs.example.com"}, 1<<20)
	mustRefuse(t, r, ref, ReasonTooManyRedirects)
	if got := hits.Load(); got != MaxRedirects+1 {
		t.Errorf("requests = %d, want %d", got, MaxRedirects+1)
	}
}

// Every resolved address must be public, and ALL of them: an answer that mixes a public
// address with a private one is refused, whichever comes first, and nothing is dialed.
func TestFetchPrivateAddresses(t *testing.T) {
	ca := newTestCA(t)
	var hits atomic.Int32
	_, port := upstream(t, ca.validLeaf(t, "docs.example.com"), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		pdfHandler(w, r)
	}))
	cases := map[string][]string{
		"rfc1918":          {"10.0.0.1"},
		"rfc1918-172":      {"172.16.5.4"},
		"rfc1918-192":      {"192.168.1.1"},
		"loopback":         {"127.0.0.1"},
		"loopback-v6":      {"::1"},
		"ipv4-mapped":      {"::ffff:10.0.0.1"},
		"ipv4-mapped-lb":   {"::ffff:127.0.0.1"},
		"ipv4-mapped-meta": {"::ffff:169.254.169.254"},
		"nat64":            {"64:ff9b::a00:1"},
		"nat64-public-v4":  {"64:ff9b::5db8:d70e"},
		"nat64-local":      {"64:ff9b:1::a00:1"},
		"6to4":             {"2002:a00:1::1"},
		"6to4-public-v4":   {"2002:5db8:d70e::1"},
		"teredo":           {"2001:0:4136:e378:8000:63bf:f5ff:fffe"},
		"metadata":         {"169.254.169.254"},
		"cgnat":            {"100.64.0.1"},
		"benchmark":        {"198.18.0.1"},
		"ula":              {"fd00::1"},
		"link-local-v6":    {"fe80::1"},
		"site-local-v6":    {"fec0::1"},
		"multicast-v6":     {"ff02::1"},
		"unspecified":      {"0.0.0.0"},
		"broadcast":        {"255.255.255.255"},
		"mixed-first":      {publicV4, "10.0.0.1"},
		"mixed-last":       {"10.0.0.1", publicV4},
		"mixed-v6":         {publicV6, "fd12::1"},
		"mixed-mapped":     {publicV4, "::ffff:192.168.0.1"},
	}
	for name, addrs := range cases {
		t.Run(name, func(t *testing.T) {
			f, dials := testFetcher(staticResolver(map[string][]string{"docs.example.com": addrs}), ca, port, nil)
			r, ref := f.Fetch(context.Background(), "https://docs.example.com/", []string{"docs.example.com"}, 1<<20)
			mustRefuse(t, r, ref, ReasonPrivateAddress)
			if d := dials.all(); len(d) != 0 {
				t.Errorf("dialed %v for a refused answer", d)
			}
		})
	}
	if hits.Load() != 0 {
		t.Errorf("a refused address was connected to (%d requests)", hits.Load())
	}
}

// The connection goes to an address the resolver returned and the policy checked, on
// port 443, and only after the check; an all-public multi-address answer is fine.
func TestFetchPinsTheCheckedAddress(t *testing.T) {
	ca := newTestCA(t)
	_, port := upstream(t, ca.validLeaf(t, "docs.example.com"), http.HandlerFunc(pdfHandler))
	res := staticResolver(map[string][]string{"docs.example.com": {publicV6, publicV4}})
	f, dials := testFetcher(res, ca, port, nil)
	if _, ref := f.Fetch(context.Background(), "https://docs.example.com/", []string{"docs.example.com"}, 1<<20); ref != nil {
		t.Fatalf("refused: %+v", ref)
	}
	d := dials.all()
	if len(d) != 1 || d[0] != netip.MustParseAddrPort("["+publicV6+"]:443") {
		t.Fatalf("dialed %v, want exactly the first checked address on 443", d)
	}
	if res.count("docs.example.com") != 1 {
		t.Fatalf("lookups = %d, want 1", res.count("docs.example.com"))
	}
}

// DNS rebinding: the name answers a public address on the first lookup and a private one
// afterwards. Within a hop the fetcher looks the name up once and dials the address it
// checked, so the second answer is never consulted; a later hop that looks the name up
// again gets the private answer, and that hop is refused.
func TestFetchRebinding(t *testing.T) {
	ca := newTestCA(t)
	_, port := upstream(t, ca.validLeaf(t, "docs.example.com"), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "/doc.pdf", http.StatusFound)
			return
		}
		pdfHandler(w, r)
	}))
	publicThenPrivate := func() *fakeResolver {
		return newResolver(func(_ string, call int) []netip.Addr {
			if call == 1 {
				return []netip.Addr{netip.MustParseAddr(publicV4)}
			}
			return []netip.Addr{netip.MustParseAddr("10.0.0.1")}
		})
	}

	res := publicThenPrivate()
	f, dials := testFetcher(res, ca, port, nil)
	r, ref := f.Fetch(context.Background(), "https://docs.example.com/", []string{"docs.example.com"}, 1<<20)
	if ref != nil {
		t.Fatalf("refused: %+v", ref)
	}
	if !bytes.Equal(r.Body, pdfBytes) {
		t.Error("unexpected body")
	}
	if n := res.count("docs.example.com"); n != 1 {
		t.Errorf("lookups = %d, want exactly 1 (a second lookup is where rebinding bites)", n)
	}
	if d := dials.all(); len(d) != 1 || d[0].Addr() != netip.MustParseAddr(publicV4) {
		t.Errorf("dialed %v, want the checked public address", d)
	}

	// The redirect's second hop re-resolves and gets the private answer.
	res = publicThenPrivate()
	f, dials = testFetcher(res, ca, port, nil)
	r, ref = f.Fetch(context.Background(), "https://docs.example.com/redirect", []string{"docs.example.com"}, 1<<20)
	mustRefuse(t, r, ref, ReasonPrivateAddress)
	if d := dials.all(); len(d) != 1 {
		t.Errorf("dialed %v, want only the first hop's public address", d)
	}
}

// The dial path re-checks the exact address it connects to, so the policy covers what is
// dialed and not only what was resolved: handed a private address directly, dialChecked
// refuses before the connect.
func TestDialCheckedRechecksAddress(t *testing.T) {
	f, dials := testFetcher(publicResolver(), nil, 0, nil)
	for _, a := range []string{"10.0.0.1:443", "127.0.0.1:443", "[::ffff:192.168.0.1]:443", "[64:ff9b::a00:1]:443"} {
		_, err := f.dialChecked(context.Background(), netip.MustParseAddrPort(a))
		if !errors.Is(err, errAddressRefused) {
			t.Errorf("%s: err = %v, want errAddressRefused", a, err)
		}
	}
	if d := dials.all(); len(d) != 0 {
		t.Fatalf("connected to %v", d)
	}
}

// The production socket dialer checks the address at the socket (its Control hook), a
// third layer below dialChecked: a loopback listener is never connected to.
func TestNetDialerRefusesAtTheSocket(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	var accepted atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			_ = c.Close()
		}
	}()
	f := New(Options{})
	_, err = f.netDialer().DialContext(context.Background(), "tcp", ln.Addr().String())
	if !errors.Is(err, errAddressRefused) {
		t.Fatalf("dial to loopback: %v", err)
	}
	if got := classify(context.Background(), err); got.Reason != ReasonPrivateAddress {
		t.Fatalf("classified as %+v", got)
	}
	time.Sleep(50 * time.Millisecond)
	if accepted.Load() != 0 {
		t.Error("the dialer connected to a refused address")
	}
}

// An HTTPS_PROXY in the environment is ignored: the transport has no Proxy function, the
// proxy sees no connection, and the fetch still reaches the pinned address.
func TestFetchIgnoresProxyEnvironment(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	var proxied atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			proxied.Add(1)
			_ = c.Close()
		}
	}()
	proxyURL := "http://" + ln.Addr().String()
	for _, k := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY"} {
		t.Setenv(k, proxyURL)
	}
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")

	ca := newTestCA(t)
	_, port := upstream(t, ca.validLeaf(t, "docs.example.com"), http.HandlerFunc(pdfHandler))
	f := fetcherFor(publicResolver(), ca, port)
	if f.transport("docs.example.com").Proxy != nil {
		t.Fatal("the fetch transport has a Proxy function")
	}
	r, ref := f.Fetch(context.Background(), "https://docs.example.com/", []string{"docs.example.com"}, 1<<20)
	if ref != nil {
		t.Fatalf("refused: %+v", ref)
	}
	if !bytes.Equal(r.Body, pdfBytes) {
		t.Error("unexpected body")
	}
	if proxied.Load() != 0 {
		t.Errorf("the proxy saw %d connections", proxied.Load())
	}
}

// A gzip bomb: the site ignores Accept-Encoding: identity and sends a small gzip body
// that inflates far past the cap. The fetcher does not decode (DisableCompression, and
// it set Accept-Encoding itself so the transport would not decode anyway) and refuses a
// non-identity Content-Encoding outright, so the bytes it delivers are always the decoded
// bytes and the cap counts them.
func TestFetchGzipBombRefused(t *testing.T) {
	var bomb bytes.Buffer
	zw := gzip.NewWriter(&bomb)
	if _, err := zw.Write(make([]byte, 16<<20)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if bomb.Len() > 64<<10 {
		t.Fatalf("bomb is %d bytes, expected a small compressed body", bomb.Len())
	}
	ca := newTestCA(t)
	_, port := upstream(t, ca.validLeaf(t, "docs.example.com"), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write(bomb.Bytes())
	}))
	f := fetcherFor(publicResolver(), ca, port)
	r, ref := f.Fetch(context.Background(), "https://docs.example.com/", []string{"docs.example.com"}, 1<<20)
	mustRefuse(t, r, ref, ReasonContentEncoding)
}

func TestFetchOversizeBody(t *testing.T) {
	ca := newTestCA(t)
	_, port := upstream(t, ca.validLeaf(t, "docs.example.com"), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := bytes.Repeat([]byte("a"), 4096+1)
		if r.URL.Path == "/chunked" {
			// No Content-Length: the limit reader is what stops it.
			for i := 0; i < len(body); i += 512 {
				_, _ = w.Write(body[i:min(i+512, len(body))])
				w.(http.Flusher).Flush()
			}
			return
		}
		_, _ = w.Write(body)
	}))
	f := fetcherFor(publicResolver(), ca, port)
	for _, path := range []string{"/sized", "/chunked"} {
		r, ref := f.Fetch(context.Background(), "https://docs.example.com"+path, []string{"docs.example.com"}, 4096)
		mustRefuse(t, r, ref, ReasonTooLarge)
	}
	// Exactly at the cap is allowed.
	_, port2 := upstream(t, ca.validLeaf(t, "docs.example.com"), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("a"), 4096))
	}))
	f = fetcherFor(publicResolver(), ca, port2)
	if r, ref := f.Fetch(context.Background(), "https://docs.example.com/", []string{"docs.example.com"}, 4096); ref != nil || len(r.Body) != 4096 {
		t.Fatalf("at-cap body: %+v", ref)
	}
	// The fetcher's own ceiling bounds an api cap above it.
	f, _ = testFetcher(publicResolver(), ca, port, func(o *Options) { o.MaxFileBytes = 4096 })
	r, ref := f.Fetch(context.Background(), "https://docs.example.com/sized", []string{"docs.example.com"}, 1<<30)
	mustRefuse(t, r, ref, ReasonTooLarge)
}

func TestFetchOversizeHeaders(t *testing.T) {
	ca := newTestCA(t)
	_, port := upstream(t, ca.validLeaf(t, "docs.example.com"), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Big", strings.Repeat("h", 70<<10))
		_, _ = w.Write([]byte("ok"))
	}))
	f := fetcherFor(publicResolver(), ca, port)
	r, ref := f.Fetch(context.Background(), "https://docs.example.com/", []string{"docs.example.com"}, 1<<20)
	mustRefuse(t, r, ref, ReasonHeadersTooLarge)
}

func TestFetchCertificateChecks(t *testing.T) {
	ca := newTestCA(t)
	other := newTestCA(t)
	for _, c := range []struct {
		name string
		cert tls.Certificate
	}{
		{"expired", ca.leaf(t, []string{"docs.example.com"}, time.Now().Add(-48*time.Hour), time.Now().Add(-24*time.Hour))},
		{"wrong-host", ca.validLeaf(t, "other.example.org")},
		{"unknown-ca", other.validLeaf(t, "docs.example.com")},
	} {
		t.Run(c.name, func(t *testing.T) {
			var hits atomic.Int32
			_, port := upstream(t, c.cert, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				pdfHandler(w, r)
			}))
			f := fetcherFor(publicResolver(), ca, port)
			r, ref := f.Fetch(context.Background(), "https://docs.example.com/", []string{"docs.example.com"}, 1<<20)
			mustRefuse(t, r, ref, ReasonTLS)
			if hits.Load() != 0 {
				t.Error("a request was sent over an unverified connection")
			}
		})
	}
}

func TestFetchUpstreamStatus(t *testing.T) {
	ca := newTestCA(t)
	_, port := upstream(t, ca.validLeaf(t, "docs.example.com"), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("secret error page"))
	}))
	f := fetcherFor(publicResolver(), ca, port)
	r, ref := f.Fetch(context.Background(), "https://docs.example.com/missing", []string{"docs.example.com"}, 1<<20)
	mustRefuse(t, r, ref, ReasonUpstreamStatus)
	if ref.HTTPStatus != http.StatusNotFound {
		t.Errorf("status = %d", ref.HTTPStatus)
	}
}

func TestFetchTimeout(t *testing.T) {
	ca := newTestCA(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	_, port := upstream(t, ca.validLeaf(t, "docs.example.com"), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	f, _ := testFetcher(publicResolver(), ca, port, func(o *Options) { o.ResponseHeaderTimeout = 200 * time.Millisecond })
	r, ref := f.Fetch(context.Background(), "https://docs.example.com/", []string{"docs.example.com"}, 1<<20)
	mustRefuse(t, r, ref, ReasonTimeout)
}

func TestFetchDNSFailure(t *testing.T) {
	f := fetcherFor(staticResolver(nil), nil, 0)
	r, ref := f.Fetch(context.Background(), "https://docs.example.com/", []string{"docs.example.com"}, 1<<20)
	mustRefuse(t, r, ref, ReasonDNSFailed)
}

func TestSanitizeContentType(t *testing.T) {
	cases := map[string]string{
		"":                                  "application/octet-stream",
		"application/pdf":                   "application/pdf",
		"Text/HTML; Charset=UTF-8":          "text/html; charset=UTF-8",
		"text/html; charset=utf-8; foo=bar": "text/html; charset=utf-8",
		"text/html\r\nX-Injected: 1":        "application/octet-stream",
		"not a type":                        "application/octet-stream",
		"text/plain; charset=\"a\u202eb\"":  "text/plain",
		"application/json; charset=\"x y\"": "application/json",
		strings.Repeat("a", 300) + "/b":     "application/octet-stream",
		"text/plain; charset=" + strings.Repeat("u", 100): "text/plain",
	}
	for in, want := range cases {
		if got := sanitizeContentType(in); got != want {
			t.Errorf("sanitizeContentType(%q) = %q, want %q", in, got, want)
		}
	}
}

// The URL the fetcher requests, logs and returns in X-Uzi-Final-Url is url.URL.String()
// output of printable ASCII: UTF-8 in the path or query (a bidi override, say) is
// percent-encoded, and the fragment is dropped.
func TestFetchFinalURLIsPrintableASCII(t *testing.T) {
	ca := newTestCA(t)
	var gotURI atomic.Value
	_, port := upstream(t, ca.validLeaf(t, "docs.example.com"), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURI.Store(r.RequestURI)
		pdfHandler(w, r)
	}))
	f := fetcherFor(publicResolver(), ca, port)
	r, ref := f.Fetch(context.Background(), "https://docs.example.com/a\u202eb?q=x\u202ey&z=\u00e9#frag", []string{"docs.example.com"}, 1<<20)
	if ref != nil {
		t.Fatalf("refused: %+v", ref)
	}
	want := "https://docs.example.com/a%E2%80%AEb?q=x%E2%80%AEy&z=%C3%A9"
	if r.FinalURL != want {
		t.Fatalf("final URL %q, want %q", r.FinalURL, want)
	}
	if gotURI.Load() != "/a%E2%80%AEb?q=x%E2%80%AEy&z=%C3%A9" {
		t.Errorf("request URI %v", gotURI.Load())
	}
}

func TestEscapeQuery(t *testing.T) {
	for in, want := range map[string]string{
		"":            "",
		"a=b&c=d%20e": "a=b&c=d%20e",
		"a=\u00e9":    "a=%C3%A9",
		"a b":         "a%20b",
		"x=\x7f":      "x=%7F",
	} {
		if got := escapeQuery(in); got != want {
			t.Errorf("escapeQuery(%q) = %q, want %q", in, got, want)
		}
	}
}
