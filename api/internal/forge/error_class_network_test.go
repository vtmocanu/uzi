package forge

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	gh "github.com/google/go-github/v92/github"
	gitlab "gitlab.com/gitlab-org/api/client-go/v3"
)

func dialOpError(inner error) error {
	return &net.OpError{Op: "dial", Net: "tcp", Err: inner}
}

func TestNetworkClassErrnoAllowlist(t *testing.T) {
	r := newRedactor("some-secret-value")
	for _, errno := range []syscall.Errno{
		syscall.ECONNREFUSED, syscall.ECONNRESET, syscall.ECONNABORTED,
		syscall.EHOSTUNREACH, syscall.EHOSTDOWN, syscall.ENETUNREACH,
		syscall.ENETDOWN, syscall.ENETRESET, syscall.EPIPE, syscall.EADDRNOTAVAIL,
	} {
		t.Run(errno.Error(), func(t *testing.T) {
			err := dialOpError(os.NewSyscallError("connect", errno))
			if got := Class(r.error(fmt.Errorf("wrapped: %w", err))); got != ErrorClassNetwork {
				t.Fatalf("class = %q, want network", got)
			}
		})
	}
}

func TestNetworkClassTemporaryDNS(t *testing.T) {
	err := &net.DNSError{Err: "server misbehaving", Name: "forge.example", IsTemporary: true}
	if got := Class(newRedactor().error(err)); got != ErrorClassNetwork {
		t.Fatalf("class = %q, want network", got)
	}
}

func TestNetworkClassExcluded(t *testing.T) {
	for name, err := range map[string]error{
		"addr error":      dialOpError(&net.AddrError{Err: "missing port in address", Addr: "x"}),
		"parse error":     &net.ParseError{Type: "IP address", Text: "x"},
		"permission":      dialOpError(os.NewSyscallError("connect", syscall.EPERM)),
		"nxdomain":        &net.DNSError{Err: "no such host", Name: "x", IsNotFound: true},
		"dns no flags":    &net.DNSError{Err: "odd", Name: "x"},
		"canceled dial":   dialOpError(context.Canceled),
		"unknown ca":      x509.UnknownAuthorityError{},
		"hostname":        x509.HostnameError{Certificate: &x509.Certificate{}, Host: "x"},
		"invalid cert":    x509.CertificateInvalidError{Reason: x509.Expired},
		"refused+cancel":  fmt.Errorf("%w: %w", syscall.ECONNREFUSED, context.Canceled),
		"nxdomain+reset":  fmt.Errorf("%w: %w", &net.DNSError{IsNotFound: true}, syscall.ECONNRESET),
		"unknown ca wrap": fmt.Errorf("verify: %w", x509.UnknownAuthorityError{}),
	} {
		t.Run(name, func(t *testing.T) {
			if got := Class(newRedactor().error(err)); got != ErrorClassOther {
				t.Fatalf("class = %q, want other", got)
			}
		})
	}
}

// TestNetworkClassExclusionsBeatAllowlist pins each exclusion arm: every excluded
// cause is joined with an allowlisted ECONNRESET, so removing any one exclusion
// turns its row into network and fails it. The nxdomain row also sets IsTemporary,
// pinning that IsNotFound is checked first.
func TestNetworkClassExclusionsBeatAllowlist(t *testing.T) {
	reset := syscall.ECONNRESET
	for name, cause := range map[string]error{
		"canceled":         context.Canceled,
		"cert verify":      &tls.CertificateVerificationError{Err: errors.New("x")},
		"unknown ca":       x509.UnknownAuthorityError{},
		"hostname":         x509.HostnameError{Certificate: &x509.Certificate{}, Host: "x"},
		"invalid cert":     x509.CertificateInvalidError{Reason: x509.Expired},
		"record header":    tls.RecordHeaderError{Msg: "first record does not look like a TLS handshake"},
		"addr error":       &net.AddrError{Err: "missing port in address", Addr: "x"},
		"parse error":      &net.ParseError{Type: "IP address", Text: "x"},
		"dns nxdomain":     &net.DNSError{Err: "no such host", Name: "x", IsNotFound: true, IsTemporary: true},
		"dns without flag": &net.DNSError{Err: "odd", Name: "x"},
	} {
		t.Run(name, func(t *testing.T) {
			err := dialOpError(errors.Join(cause, reset))
			if got := Class(newRedactor().error(err)); got != ErrorClassOther {
				t.Fatalf("class = %q, want other (exclusion must win over ECONNRESET)", got)
			}
		})
	}
	// Control: the same join without an excluded cause is network.
	if got := Class(newRedactor().error(dialOpError(errors.Join(errors.New("x"), reset)))); got != ErrorClassNetwork {
		t.Fatalf("control class = %q, want network", got)
	}
}

func TestNetworkClassFallThrough(t *testing.T) {
	plain := errors.New("x")
	reset := dialOpError(os.NewSyscallError("read", syscall.ECONNRESET))
	cases := []struct {
		name   string
		err    error
		status int
		want   ErrorClass
	}{
		{"429", plain, 429, ErrorClassRateLimited},
		{"503", plain, 503, ErrorClassServerError},
		{"401", plain, 401, ErrorClassAuth},
		{"reset with 401", reset, 401, ErrorClassAuth},
		{"reset with 503", reset, 503, ErrorClassServerError},
		{"github rate limit", &gh.RateLimitError{Response: &http.Response{StatusCode: 403, Request: &http.Request{Method: "GET", URL: &url.URL{Path: "/x"}}}}, 0, ErrorClassRateLimited},
		{"gitlab 503", &gitlab.ErrorResponse{StatusCode: 503, Response: &http.Response{StatusCode: 503, Request: &http.Request{Method: "GET", URL: &url.URL{Path: "/x"}}}}, 0, ErrorClassServerError},
		{"unclassified", plain, 0, ErrorClassOther},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Class(newRedactor().errorStatus(tc.err, tc.status)); got != tc.want {
				t.Fatalf("class = %q, want %q", got, tc.want)
			}
		})
	}
}

// timeoutNetErr is a net.Error whose Timeout() is true and that carries no
// context.DeadlineExceeded in its chain.
type timeoutNetErr struct{}

func (timeoutNetErr) Error() string   { return "i/o timeout" }
func (timeoutNetErr) Timeout() bool   { return true }
func (timeoutNetErr) Temporary() bool { return false }

// TestClassAuthStatusBeatsTimeout pins that a supplied 401/403 wins over a
// timeout-carrying error (a refused credential is definitive), while 0 and 503
// leave the timeout class unchanged: only auth outranks it.
func TestClassAuthStatusBeatsTimeout(t *testing.T) {
	errs := map[string]error{
		"net.Error timeout": fmt.Errorf("read body: %w", timeoutNetErr{}),
		"deadline exceeded": fmt.Errorf("read body: %w", context.DeadlineExceeded),
		"op error timeout":  &net.OpError{Op: "read", Net: "tcp", Err: timeoutNetErr{}},
		"joined deadline":   errors.Join(errors.New("x"), context.DeadlineExceeded),
	}
	for name, err := range errs {
		for _, tc := range []struct {
			status int
			want   ErrorClass
		}{
			{401, ErrorClassAuth},
			{403, ErrorClassAuth},
			{0, ErrorClassTimeout},
			{503, ErrorClassTimeout},
		} {
			t.Run(fmt.Sprintf("%s status %d", name, tc.status), func(t *testing.T) {
				if got := Class(newRedactor().errorStatus(err, tc.status)); got != tc.want {
					t.Fatalf("class = %q, want %q", got, tc.want)
				}
			})
		}
	}
}

func TestNetworkClassKeepsRedaction(t *testing.T) {
	token := "glpat-" + "abcdefghijklmnopqrst"
	err := fmt.Errorf("PRIVATE-TOKEN %s rejected: %w", token, dialOpError(os.NewSyscallError("read", syscall.ECONNRESET)))
	out := newRedactor(token).error(err)
	if strings.Contains(out.Error(), token) {
		t.Fatalf("message retained the token: %q", out.Error())
	}
	if !strings.Contains(out.Error(), redactPlaceholder) {
		t.Fatalf("message not scrubbed: %q", out.Error())
	}
	if got := Class(out); got != ErrorClassNetwork {
		t.Fatalf("class = %q, want network", got)
	}
}

// closedMockURL returns a base URL whose server is already closed.
func closedMockGitLab(t *testing.T) *mockGitLab {
	t.Helper()
	m := newMockGitLab(t, nil)
	m.srv.Close()
	return m
}

func TestGitLabClosedServerIsNetworkAfterRetries(t *testing.T) {
	m := closedMockGitLab(t)
	var calls atomic.Int32
	d, err := NewWithGitLabBackoff(TypeGitLab, m.srv.URL, "glpat-"+"abcdefghijklmnopqrst", 5*time.Second,
		func(_, _ time.Duration, _ int, _ *http.Response) time.Duration {
			calls.Add(1)
			return 0
		})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = d.VerifyToken(context.Background())
	if got := Class(err); got != ErrorClassNetwork {
		t.Fatalf("VerifyToken class = %q, want network (err %v)", got, err)
	}
	if calls.Load() < 2 {
		t.Fatalf("backoff invoked %d times, want the SDK to retry the dial failure", calls.Load())
	}
	_, err = d.DefaultBranchProtection(context.Background(), 7, "main", 1)
	if got := Class(err); got != ErrorClassNetwork {
		t.Fatalf("DefaultBranchProtection class = %q, want network (err %v)", got, err)
	}
}

func TestGitHubClosedServerIsNetwork(t *testing.T) {
	m := newMockGitHub(t, nil)
	m.srv.Close()
	d := newGitHubDriver(t, m, "ghp_classicTokenValue1234567890")
	_, err := d.VerifyToken(context.Background())
	if got := Class(err); got != ErrorClassNetwork {
		t.Fatalf("class = %q, want network (err %v)", got, err)
	}
}

func TestForgejoClosedServerIsNetwork(t *testing.T) {
	m := newMockForgejo(t, nil)
	m.srv.Close()
	d := newForgejoDriver(t, m, "forgejo-abcdefabcdef")
	_, err := d.VerifyToken(context.Background())
	if got := Class(err); got != ErrorClassNetwork {
		t.Fatalf("class = %q, want network (err %v)", got, err)
	}
}

func TestForgejoStatusClasses(t *testing.T) {
	status := func(code int) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) }
	}
	for _, tc := range []struct {
		code int
		want ErrorClass
	}{
		{429, ErrorClassRateLimited},
		{503, ErrorClassServerError},
		{401, ErrorClassAuth},
		{403, ErrorClassAuth},
	} {
		name := fmt.Sprint(tc.code)
		t.Run("version "+name, func(t *testing.T) {
			m := newMockForgejo(t, map[string]http.HandlerFunc{"/version": status(tc.code)})
			_, err := newForgejoDriver(t, m, "forgejo-abcdefabcdef").VerifyToken(context.Background())
			if got := Class(err); got != tc.want {
				t.Fatalf("class = %q, want %q (err %v)", got, tc.want, err)
			}
		})
		t.Run("user "+name, func(t *testing.T) {
			m := newMockForgejo(t, map[string]http.HandlerFunc{
				"/version": versionHandler("16.0.0+gitea-1.22.0"),
				"/user":    status(tc.code),
			})
			_, err := newForgejoDriver(t, m, "forgejo-abcdefabcdef").VerifyToken(context.Background())
			if got := Class(err); got != tc.want {
				t.Fatalf("class = %q, want %q (err %v)", got, tc.want, err)
			}
		})
		t.Run("branch "+name, func(t *testing.T) {
			m := newMockForgejo(t, map[string]http.HandlerFunc{
				"/repos/acme/widgets/branches/main": status(tc.code),
			})
			_, err := newForgejoDriver(t, m, "forgejo-abcdefabcdef").DefaultBranchProtection(context.Background(), 7, "main", 1)
			if got := Class(err); got != tc.want {
				t.Fatalf("class = %q, want %q (err %v)", got, tc.want, err)
			}
		})
	}
}

func TestGitHubRulesetReadClass(t *testing.T) {
	branch := func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"name": "main", "protected": true})
	}
	t.Run("503 sets class", func(t *testing.T) {
		m := newMockGitHub(t, map[string]http.HandlerFunc{
			"/repos/acme/widgets/branches/main": branch,
			"/repos/acme/widgets/rules/branches/main": func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, `{"message":"unavailable"}`, http.StatusServiceUnavailable)
			},
		})
		d := newGitHubDriver(t, m, "ghp_classicTokenValue1234567890")
		bp, err := d.DefaultBranchProtection(context.Background(), 7, "main", 1)
		if err != nil {
			t.Fatalf("DefaultBranchProtection: %v", err)
		}
		if !bp.Protected || !bp.ProtectionUnverified || bp.UnverifiedReadClass != ErrorClassServerError {
			t.Fatalf("want protected+unverified with server_error class, got %+v", bp)
		}
	})
	t.Run("legacy protection sets none", func(t *testing.T) {
		m := newMockGitHub(t, map[string]http.HandlerFunc{
			"/repos/acme/widgets/branches/main": branch,
			"/repos/acme/widgets/rules/branches/main": func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte("[]"))
			},
		})
		d := newGitHubDriver(t, m, "ghp_classicTokenValue1234567890")
		bp, err := d.DefaultBranchProtection(context.Background(), 7, "main", 1)
		if err != nil {
			t.Fatalf("DefaultBranchProtection: %v", err)
		}
		if !bp.ProtectionUnverified || bp.UnverifiedReadClass != "" {
			t.Fatalf("want unverified with no class, got %+v", bp)
		}
	})
}
