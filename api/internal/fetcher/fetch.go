package fetcher

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/vtmocanu/uzi/api/internal/egressprofile"
)

// Reason codes. They are stable strings a client branches on; see the package doc for
// the HTTP status each maps to.
const (
	ReasonBadRequest         = "bad_request"
	ReasonURLTooLong         = "url_too_long"
	ReasonCredentialInvalid  = "credential_invalid" //nolint:gosec // G101: a refusal reason VOCABULARY value (package doc), not a credential.
	ReasonAdmissionRefused   = "admission_refused"
	ReasonControlUnavailable = "control_unavailable"
	ReasonLogFailed          = "log_failed"
	ReasonBusy               = "busy"

	ReasonInvalidURL       = "invalid_url"
	ReasonNotHTTPS         = "not_https"
	ReasonUserinfo         = "userinfo"
	ReasonIPLiteral        = "ip_literal"
	ReasonPort             = "port"
	ReasonOffList          = "off_list"
	ReasonRedirectOffList  = "redirect_off_list"
	ReasonPrivateAddress   = "private_address"
	ReasonTooManyRedirects = "too_many_redirects"
	ReasonRedirectInvalid  = "redirect_invalid"
	ReasonContentEncoding  = "content_encoding"
	ReasonDNSFailed        = "dns_failed"
	ReasonConnectFailed    = "connect_failed"
	ReasonTLS              = "tls"
	ReasonTimeout          = "timeout"
	ReasonHeadersTooLarge  = "headers_too_large"
	ReasonUpstreamStatus   = "upstream_status"
	ReasonUpstreamError    = "upstream_error"
	ReasonTooLarge         = "too_large"
)

const (
	defaultContentType        = "application/octet-stream"
	redirectDiscardCap        = 64 << 10
	maxContentTypeLen         = 255
	httpsPort          uint16 = 443
)

// Limits and defaults.
const (
	// MaxURLLen caps the URL a worker may send, in bytes.
	MaxURLLen = 2048
	// MaxRedirects bounds the redirect hops one fetch follows.
	MaxRedirects = 5
	// DefaultMaxHeaderBytes caps the upstream response header bytes.
	DefaultMaxHeaderBytes = 64 << 10
	// DefaultMaxFileBytes is the fetcher's own per-file ceiling; the api's per-file cap
	// applies below it.
	DefaultMaxFileBytes = 25 << 20
	// DefaultTimeout bounds one whole fetch: every hop and the body.
	DefaultTimeout = 60 * time.Second
	// DefaultDialTimeout, DefaultTLSHandshakeTimeout and DefaultResponseHeaderTimeout
	// bound the phases of one hop.
	DefaultDialTimeout           = 10 * time.Second
	DefaultTLSHandshakeTimeout   = 10 * time.Second
	DefaultResponseHeaderTimeout = 20 * time.Second
)

// Resolver resolves a host name to addresses. *net.Resolver satisfies it.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// Options configure a Fetcher. Zero durations and sizes take the defaults. Production
// builds its Options with Config.FetcherOptions only.
type Options struct {
	// Resolver resolves the normalized host; nil means net.DefaultResolver.
	Resolver Resolver
	Policy   AddressPolicy
	// RootCAs verifies upstream certificates; nil means the system roots. Config never
	// sets it; only this package's tests do.
	RootCAs               *x509.CertPool
	MaxFileBytes          int64
	MaxHeaderBytes        int64
	Timeout               time.Duration
	DialTimeout           time.Duration
	TLSHandshakeTimeout   time.Duration
	ResponseHeaderTimeout time.Duration
	UserAgent             string

	// testDial, when set, replaces only the final TCP connect to an address the policy
	// has already approved, so this package's tests can route a checked public address to
	// an httptest server on loopback. It is unexported: no other package (cmd/fetcher
	// included) can set it, Config.FetcherOptions leaves it nil (TestFetcherOptionsHaveNoTestSeam),
	// and the policy check before it is not skipped.
	testDial func(ctx context.Context, addr netip.AddrPort) (net.Conn, error)
}

// Fetcher performs checked GETs.
type Fetcher struct {
	opts Options
}

// New returns a Fetcher. A nil Resolver means net.DefaultResolver.
func New(opts Options) *Fetcher {
	if opts.Resolver == nil {
		opts.Resolver = net.DefaultResolver
	}
	if opts.MaxFileBytes <= 0 {
		opts.MaxFileBytes = DefaultMaxFileBytes
	}
	if opts.MaxHeaderBytes <= 0 {
		opts.MaxHeaderBytes = DefaultMaxHeaderBytes
	}
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultTimeout
	}
	if opts.DialTimeout <= 0 {
		opts.DialTimeout = DefaultDialTimeout
	}
	if opts.TLSHandshakeTimeout <= 0 {
		opts.TLSHandshakeTimeout = DefaultTLSHandshakeTimeout
	}
	if opts.ResponseHeaderTimeout <= 0 {
		opts.ResponseHeaderTimeout = DefaultResponseHeaderTimeout
	}
	if opts.UserAgent == "" {
		opts.UserAgent = "uzi-fetcher"
	}
	return &Fetcher{opts: opts}
}

// Result is a successful fetch.
type Result struct {
	FinalURL    string
	HTTPStatus  int
	ContentType string
	Body        []byte
	SHA256      string
}

// Refusal is a fetch that returned no content. FinalURL is the last URL attempted (the
// requested URL, or the redirect target that was refused); HTTPStatus is the upstream
// status when one was received.
type Refusal struct {
	Reason     string
	Message    string
	FinalURL   string
	HTTPStatus int
}

func refuse(reason, msg string) *Refusal { return &Refusal{Reason: reason, Message: msg} }

// errAddressRefused is returned by the dial path when the address it is about to
// connect to fails the policy.
var errAddressRefused = errors.New("address refused by policy")

// refusalError carries a Refusal out of the transport's DialContext.
type refusalError struct{ r *Refusal }

func (e *refusalError) Error() string { return e.r.Message }

// Fetch GETs rawURL if every hop passes the checks against entries, reading at most
// maxBytes of body (capped by the fetcher's own ceiling). Exactly one of the results is
// non-nil.
func (f *Fetcher) Fetch(ctx context.Context, rawURL string, entries []string, maxBytes int64) (*Result, *Refusal) {
	if maxBytes <= 0 || maxBytes > f.opts.MaxFileBytes {
		maxBytes = f.opts.MaxFileBytes
	}
	ctx, cancel := context.WithTimeout(ctx, f.opts.Timeout)
	defer cancel()

	cur := rawURL
	lastStatus := 0 // the redirect status that led to cur, for a refused redirect target
	for hop := 0; ; hop++ {
		u, host, ref := checkURL(cur, hop > 0, entries)
		if ref != nil {
			ref.FinalURL = cur
			ref.HTTPStatus = lastStatus
			return nil, ref
		}
		cur = u.String()
		res, next, status, ref := f.hop(ctx, u, host, hop, maxBytes)
		if ref != nil {
			if ref.FinalURL == "" {
				ref.FinalURL = cur
			}
			return nil, ref
		}
		if res != nil {
			res.FinalURL = cur
			return res, nil
		}
		cur, lastStatus = next, status
	}
}

// hop performs one checked request. It returns the result, or the next URL to follow and
// the redirect status that named it, or a refusal.
func (f *Fetcher) hop(ctx context.Context, u *url.URL, host string, hop int, maxBytes int64) (*Result, string, int, *Refusal) {
	resp, err := f.roundTrip(ctx, u, host)
	if err != nil {
		return nil, "", 0, classify(ctx, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if isRedirect(resp.StatusCode) {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, redirectDiscardCap))
		loc := resp.Header.Get("Location")
		if loc == "" {
			return nil, "", 0, &Refusal{Reason: ReasonRedirectInvalid, Message: "redirect without a Location", HTTPStatus: resp.StatusCode}
		}
		next, err := u.Parse(loc)
		if err != nil {
			return nil, "", 0, &Refusal{Reason: ReasonRedirectInvalid, Message: "redirect Location is not a URL", HTTPStatus: resp.StatusCode}
		}
		if hop >= MaxRedirects {
			return nil, "", 0, &Refusal{Reason: ReasonTooManyRedirects, Message: "too many redirects", FinalURL: next.String(), HTTPStatus: resp.StatusCode}
		}
		return nil, next.String(), resp.StatusCode, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, "", 0, &Refusal{Reason: ReasonUpstreamStatus, Message: "the site answered " + resp.Status, HTTPStatus: resp.StatusCode}
	}
	if ce := strings.TrimSpace(resp.Header.Get("Content-Encoding")); ce != "" && !strings.EqualFold(ce, "identity") {
		return nil, "", 0, &Refusal{Reason: ReasonContentEncoding, Message: "the site sent an encoded body although identity was requested", HTTPStatus: resp.StatusCode}
	}
	if resp.ContentLength > maxBytes {
		return nil, "", 0, &Refusal{Reason: ReasonTooLarge, Message: "the file is larger than the per-file cap", HTTPStatus: resp.StatusCode}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		r := classify(ctx, err)
		r.HTTPStatus = resp.StatusCode
		return nil, "", 0, r
	}
	if int64(len(body)) > maxBytes {
		return nil, "", 0, &Refusal{Reason: ReasonTooLarge, Message: "the file is larger than the per-file cap", HTTPStatus: resp.StatusCode}
	}
	sum := sha256.Sum256(body)
	return &Result{
		HTTPStatus:  resp.StatusCode,
		ContentType: sanitizeContentType(resp.Header.Get("Content-Type")),
		Body:        body,
		SHA256:      hex.EncodeToString(sum[:]),
	}, "", 0, nil
}

func isRedirect(code int) bool {
	switch code {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return true
	}
	return false
}

// checkURL applies the URL checks to one hop and returns the URL to request (host
// replaced by its normalized form, fragment and userinfo dropped) and that host.
func checkURL(raw string, redirect bool, entries []string) (*url.URL, string, *Refusal) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, "", refuse(ReasonInvalidURL, "not a valid URL")
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return nil, "", refuse(ReasonNotHTTPS, "only https URLs are fetched")
	}
	if u.User != nil {
		return nil, "", refuse(ReasonUserinfo, "a URL with user information is refused")
	}
	if u.Opaque != "" || u.Host == "" {
		return nil, "", refuse(ReasonInvalidURL, "the URL has no host")
	}
	hostname := u.Hostname()
	if _, err := netip.ParseAddr(hostname); err == nil {
		return nil, "", refuse(ReasonIPLiteral, "IP-address URLs are refused; use a host name")
	}
	if p := u.Port(); p != "" && p != "443" {
		return nil, "", refuse(ReasonPort, "only the default https port is fetched")
	}
	host, err := egressprofile.NormalizeHost(hostname)
	if err != nil {
		var ee *egressprofile.EntryError
		if errors.As(err, &ee) && ee.Code == egressprofile.CodeIPAddress {
			return nil, "", refuse(ReasonIPLiteral, "IP-address URLs are refused; use a host name")
		}
		return nil, "", refuse(ReasonInvalidURL, "the URL's host is not a valid host name")
	}
	if !egressprofile.Match(host, entries) {
		if redirect {
			return nil, "", refuse(ReasonRedirectOffList, "a redirect leads to a host that is not on the run's site list")
		}
		return nil, "", refuse(ReasonOffList, "the host is not on the run's site list")
	}
	out := &url.URL{
		Scheme:   "https",
		Host:     host,
		Path:     u.Path,
		RawPath:  u.RawPath,
		RawQuery: escapeQuery(u.RawQuery),
	}
	return out, host, nil
}

// escapeQuery percent-encodes every byte of a raw query that is not printable ASCII.
// url.Parse refuses ASCII control bytes but keeps UTF-8 (a bidi override, say) verbatim
// in RawQuery, and url.URL.String() does not re-escape it; this keeps the URL the fetcher
// requests, logs and returns in X-Uzi-Final-Url printable ASCII.
func escapeQuery(q string) string {
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(q); i++ {
		c := q[i]
		if c <= ' ' || c >= 0x7f {
			b.WriteByte('%')
			b.WriteByte(hexDigits[c>>4])
			b.WriteByte(hexDigits[c&0x0f])
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// resolve looks host up as a fully qualified name (a trailing dot, so no resolv.conf
// search domain can be appended) and returns its addresses, unmapped, only if every one
// passes the policy: one private answer refuses the whole name.
func (f *Fetcher) resolve(ctx context.Context, host string) ([]netip.Addr, *Refusal) {
	addrs, err := f.opts.Resolver.LookupNetIP(ctx, "ip", host+".")
	if err != nil {
		if ctx.Err() != nil {
			return nil, refuse(ReasonTimeout, "the fetch timed out")
		}
		return nil, refuse(ReasonDNSFailed, "the host name did not resolve")
	}
	if len(addrs) == 0 {
		return nil, refuse(ReasonDNSFailed, "the host name did not resolve")
	}
	out := make([]netip.Addr, 0, len(addrs))
	for _, a := range addrs {
		if !f.opts.Policy.Allowed(a) {
			return nil, refuse(ReasonPrivateAddress, "the host resolves to a non-public address")
		}
		out = append(out, a.Unmap())
	}
	return out, nil
}

// dialChecked connects to one resolved address on port 443. The policy is checked again
// here, on the exact address being dialed, and the production dialer checks it a third
// time at the socket (netDialer's Control), so what is connected to is what was checked.
func (f *Fetcher) dialChecked(ctx context.Context, ap netip.AddrPort) (net.Conn, error) {
	if !f.opts.Policy.Allowed(ap.Addr()) {
		return nil, errAddressRefused
	}
	if f.opts.testDial != nil {
		return f.opts.testDial(ctx, ap)
	}
	return f.netDialer().DialContext(ctx, "tcp", ap.String())
}

// netDialer is the production socket dialer. Its Control hook runs on the address the
// kernel is about to connect to.
func (f *Fetcher) netDialer() *net.Dialer {
	policy := f.opts.Policy
	return &net.Dialer{
		Timeout: f.opts.DialTimeout,
		Control: func(_, address string, _ syscall.RawConn) error {
			ap, err := netip.ParseAddrPort(address)
			if err != nil || !policy.Allowed(ap.Addr()) {
				return errAddressRefused
			}
			return nil
		},
	}
}

// transport returns the one-hop transport for host. Its DialContext ignores the address
// the transport asks for and instead resolves host itself, requires every answer to pass
// the policy, and dials the checked addresses (pinning): there is no second lookup a
// rebinding answer could land in. Proxy is nil, so proxy environment is ignored; SNI and
// certificate verification use host; nothing is decoded.
func (f *Fetcher) transport(host string) *http.Transport {
	return &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			ips, ref := f.resolve(ctx, host)
			if ref != nil {
				return nil, &refusalError{r: ref}
			}
			var lastErr error
			for _, ip := range ips {
				c, err := f.dialChecked(ctx, netip.AddrPortFrom(ip, httpsPort))
				if err == nil {
					return c, nil
				}
				lastErr = err
				if errors.Is(err, errAddressRefused) || ctx.Err() != nil {
					break
				}
			}
			return nil, lastErr
		},
		TLSClientConfig: &tls.Config{
			ServerName: host,
			RootCAs:    f.opts.RootCAs,
			MinVersion: tls.VersionTLS12,
		},
		TLSHandshakeTimeout:    f.opts.TLSHandshakeTimeout,
		ResponseHeaderTimeout:  f.opts.ResponseHeaderTimeout,
		MaxResponseHeaderBytes: f.opts.MaxHeaderBytes,
		DisableCompression:     true,
		DisableKeepAlives:      true,
	}
}

// roundTrip sends one GET for u (whose host is the normalized host) over a fresh
// transport for host.
func (f *Fetcher) roundTrip(ctx context.Context, u *url.URL, host string) (*http.Response, error) {
	tr := f.transport(host)
	defer tr.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("User-Agent", f.opts.UserAgent)
	return tr.RoundTrip(req)
}

// classify maps a transport or body-read error to a refusal.
func classify(ctx context.Context, err error) *Refusal {
	var (
		certErr    *tls.CertificateVerificationError
		unknownCA  x509.UnknownAuthorityError
		hostErr    x509.HostnameError
		invalidErr x509.CertificateInvalidError
		alertErr   tls.AlertError
		recordErr  tls.RecordHeaderError
		netErr     net.Error
		opErr      *net.OpError
		refErr     *refusalError
	)
	switch {
	case errors.As(err, &refErr):
		return refuse(refErr.r.Reason, refErr.r.Message)
	case errors.Is(err, errAddressRefused):
		return refuse(ReasonPrivateAddress, "the connection address is not public")
	case ctx.Err() != nil:
		return refuse(ReasonTimeout, "the fetch timed out")
	case errors.As(err, &certErr), errors.As(err, &unknownCA), errors.As(err, &hostErr),
		errors.As(err, &invalidErr), errors.As(err, &alertErr), errors.As(err, &recordErr):
		return refuse(ReasonTLS, "the site's TLS certificate or handshake failed verification")
	case strings.Contains(err.Error(), "response headers exceeded"):
		return refuse(ReasonHeadersTooLarge, "the site's response headers are too large")
	case errors.As(err, &netErr) && netErr.Timeout():
		return refuse(ReasonTimeout, "the fetch timed out")
	case errors.As(err, &opErr) && opErr.Op == "dial":
		return refuse(ReasonConnectFailed, "could not connect to the site")
	default:
		return refuse(ReasonUpstreamError, "the site's response could not be read")
	}
}

// charsetName is the shape a relayed charset parameter must have (IANA charset names).
var charsetName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:+-]{0,39}$`)

// sanitizeContentType re-serializes the upstream Content-Type: the media type plus a
// charset parameter of a plain name, or application/octet-stream when it does not parse.
func sanitizeContentType(v string) string {
	if v == "" || len(v) > 1024 {
		return defaultContentType
	}
	mt, params, err := mime.ParseMediaType(v)
	if err != nil || !strings.Contains(mt, "/") {
		return defaultContentType
	}
	keep := map[string]string{}
	if cs := params["charset"]; charsetName.MatchString(cs) {
		keep["charset"] = cs
	}
	out := mime.FormatMediaType(mt, keep)
	if out == "" || len(out) > maxContentTypeLen {
		return defaultContentType
	}
	return out
}
