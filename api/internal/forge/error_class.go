package forge

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"syscall"

	gh "github.com/google/go-github/v92/github"
	gitlab "gitlab.com/gitlab-org/api/client-go/v3"
)

// ErrorClass is a forge-neutral outcome. Class normalizes values to this closed set.
type ErrorClass string

const (
	// ErrorClassTimeout identifies a deadline or network timeout.
	ErrorClassTimeout ErrorClass = "timeout"
	// ErrorClassAuth identifies an authentication or permission rejection.
	ErrorClassAuth ErrorClass = "auth"
	// ErrorClassServerError identifies an HTTP 5xx response.
	ErrorClassServerError ErrorClass = "server_error"
	// ErrorClassRateLimited identifies a typed rate limit or HTTP 429 response.
	ErrorClassRateLimited ErrorClass = "rate_limited"
	// ErrorClassNetwork identifies a transport failure with no HTTP response that is
	// plausibly transient (connection refused or reset, unreachable host, temporary
	// DNS failure). Certificate, malformed-address and NXDOMAIN failures are not.
	ErrorClassNetwork ErrorClass = "network"
	// ErrorClassOther identifies an outcome without a recognized class.
	ErrorClassOther ErrorClass = "other"
)

// Class reads an outcome through safe error wraps. Missing or unknown classes,
// including nil errors, return other; arbitrary class strings never escape.
func Class(err error) ErrorClass {
	var classified interface{ Class() ErrorClass }
	if errors.As(err, &classified) {
		switch c := classified.Class(); c {
		case ErrorClassTimeout, ErrorClassAuth, ErrorClassServerError, ErrorClassRateLimited, ErrorClassNetwork:
			return c
		}
	}
	return ErrorClassOther
}

// classifiedError retains only a scrubbed message and a normalized outcome.
type classifiedError struct {
	message string
	class   ErrorClass
}

func (e *classifiedError) Error() string     { return e.message }
func (e *classifiedError) Class() ErrorClass { return e.class }

// transientErrnos are connection-level failures worth retrying.
var transientErrnos = []syscall.Errno{
	syscall.ECONNREFUSED, syscall.ECONNRESET, syscall.ECONNABORTED,
	syscall.EHOSTUNREACH, syscall.EHOSTDOWN, syscall.ENETUNREACH,
	syscall.ENETDOWN, syscall.ENETRESET, syscall.EPIPE, syscall.EADDRNOTAVAIL,
}

// classifyTransport reports network for an allowlisted transient transport
// failure and other for a known-permanent one (cancellation, certificate
// problems, malformed addresses, NXDOMAIN). ok is false when err is neither, so
// the caller falls through to its remaining checks.
func classifyTransport(err error) (class ErrorClass, ok bool) {
	// Caller cancellation and TLS/address problems will not heal on retry.
	var (
		certVerify  *tls.CertificateVerificationError
		unknownCA   x509.UnknownAuthorityError
		hostname    x509.HostnameError
		certInvalid x509.CertificateInvalidError
		recHeader   tls.RecordHeaderError
		addrErr     *net.AddrError
		parseErr    *net.ParseError
	)
	if errors.Is(err, context.Canceled) ||
		errors.As(err, &certVerify) || errors.As(err, &unknownCA) ||
		errors.As(err, &hostname) || errors.As(err, &certInvalid) ||
		errors.As(err, &recHeader) ||
		errors.As(err, &addrErr) || errors.As(err, &parseErr) {
		return ErrorClassOther, true
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		switch {
		case dns.IsNotFound:
			return ErrorClassOther, true
		case dns.IsTemporary:
			return ErrorClassNetwork, true
		case !dns.IsTimeout:
			// No flag at all: nothing says it is transient.
			return ErrorClassOther, true
		}
	}
	for _, errno := range transientErrnos {
		if errors.Is(err, errno) {
			return ErrorClassNetwork, true
		}
	}
	return "", false
}

// classifyError inspects raw typed errors before the redactor severs their chain.
// status is supplied where an SDK returns HTTP metadata separately.
func classifyError(err error, status int) ErrorClass {
	// A supplied 401/403 is a definitive answer from the forge: an error that also
	// carries a timeout (e.g. the body read timed out after the response arrived)
	// must not turn a refused credential into a retryable class.
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return ErrorClassAuth
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return ErrorClassTimeout
	}
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		return ErrorClassTimeout
	}
	// Transport failures are classified only when no HTTP response exists; a
	// supplied status lets the status switch below decide.
	if status == 0 {
		if class, ok := classifyTransport(err); ok {
			return class
		}
	}
	var primary *gh.RateLimitError
	var abuse *gh.AbuseRateLimitError
	var neutral *RateLimitError
	if errors.As(err, &primary) || errors.As(err, &abuse) || errors.As(err, &neutral) {
		return ErrorClassRateLimited
	}
	if status == 0 {
		var githubError *gh.ErrorResponse
		var gitlabError *gitlab.ErrorResponse
		switch {
		case errors.As(err, &githubError) && githubError.Response != nil:
			status = githubError.Response.StatusCode
		case errors.As(err, &gitlabError):
			status = gitlabError.StatusCode
		}
	}
	switch {
	case status == http.StatusTooManyRequests:
		return ErrorClassRateLimited
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return ErrorClassAuth
	case status >= 500 && status < 600:
		return ErrorClassServerError
	}
	return Class(err)
}
