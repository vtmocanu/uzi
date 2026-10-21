package forge

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"sync/atomic"
	"syscall"
)

// gitLabMaxResponseBytes caps the decoded body of a successful (2xx) GitLab
// response on a marked call. A package var so tests can lower it, like
// forgejoMaxResponseBytes.
var gitLabMaxResponseBytes int64 = 32 << 20

// gitLabMaxErrorResponseBytes caps the decoded body of a non-2xx response on a
// marked call. An overflow discards the body so a token split at the boundary
// cannot reach an error message.
const gitLabMaxErrorResponseBytes int64 = 4 << 10

// ErrResponseTooLarge identifies a forge response body over its size ceiling.
var ErrResponseTooLarge = errors.New("forge response body exceeds size limit")

// ResponseTooLargeError carries no remote content: its text is fixed and trusted,
// so the redactor passes it through. Treat it as "no evidence", never as a
// partial answer.
type ResponseTooLargeError struct{}

func (*ResponseTooLargeError) Error() string { return ErrResponseTooLarge.Error() }
func (*ResponseTooLargeError) Unwrap() error { return ErrResponseTooLarge }

// responseLimitState is the per-call record of an overflow seen on the lazy
// (streaming) path, where the SDK ignores the read error (the deferred drain) or
// folds it into an empty ErrorResponse. One state is created per marked call and
// never shared between calls.
type responseLimitState struct{ overflow atomic.Bool }

type responseLimitKey struct{}

// withResponseLimit marks ctx so responseLimitTransport bounds the response
// bodies of the requests made under it. Unmarked requests pass through unchanged.
func withResponseLimit(ctx context.Context, state *responseLimitState) context.Context {
	return context.WithValue(ctx, responseLimitKey{}, state)
}

func responseLimitFrom(ctx context.Context) *responseLimitState {
	state, _ := ctx.Value(responseLimitKey{}).(*responseLimitState)
	return state
}

// readBoundedBody reads at most limit+1 bytes of rc and always closes it. It
// returns *ResponseTooLargeError when more than limit bytes arrive and any other
// read error as is.
func readBoundedBody(rc io.ReadCloser, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(rc, limit+1))
	_ = rc.Close()
	if int64(len(data)) > limit {
		return nil, &ResponseTooLargeError{}
	}
	if err != nil {
		return nil, err
	}
	return data, nil
}

// lazyCappedBody streams the original body through a limit+1 byte window. The
// byte past the limit fails the read with *ResponseTooLargeError and records the
// overflow in the call's state; no byte beyond the limit is handed on.
type lazyCappedBody struct {
	rc        io.ReadCloser
	remaining int64
	state     *responseLimitState
}

func (b *lazyCappedBody) Read(p []byte) (int, error) {
	if b.remaining < 0 {
		return 0, &ResponseTooLargeError{}
	}
	if int64(len(p)) > b.remaining+1 {
		p = p[:b.remaining+1]
	}
	n, err := b.rc.Read(p)
	b.remaining -= int64(n)
	if b.remaining < 0 {
		b.state.overflow.Store(true)
		return n + int(b.remaining), &ResponseTooLargeError{}
	}
	return n, err
}

func (b *lazyCappedBody) Close() error { return b.rc.Close() }

// bodyReadError wraps a mid-body read failure of a 2xx response that the
// transport now observes before the SDK's retry policy runs. It has no Unwrap:
// the SDK's errors.As switch must not reach an inner *net.OpError, *net.DNSError
// or "connection refused" text, so a body already partly delivered is never
// retried. Its text is fixed and carries no remote or OS detail.
type bodyReadError struct {
	category  string
	timeout   bool
	temporary bool
	canceled  bool
	deadline  bool
}

func newBodyReadError(err error) *bodyReadError {
	e := &bodyReadError{category: "error"}
	var timeout interface{ Timeout() bool }
	if errors.As(err, &timeout) {
		e.timeout = timeout.Timeout()
	}
	// The SDK retry policy asks *net.OpError.Temporary; capture the answer so the
	// wrapper keeps the same class without exposing the inner error.
	var temporary interface{ Temporary() bool }
	if errors.As(err, &temporary) {
		e.temporary = temporary.Temporary()
	}
	e.canceled = errors.Is(err, context.Canceled)
	e.deadline = errors.Is(err, context.DeadlineExceeded)
	switch {
	case e.canceled:
		e.category = "canceled"
	case e.deadline:
		e.category = "deadline exceeded"
	case e.timeout:
		e.category = "timeout"
	case errors.Is(err, io.ErrUnexpectedEOF):
		e.category = "unexpected EOF"
	case errors.Is(err, syscall.ECONNRESET):
		e.category = "reset"
	}
	return e
}

func (e *bodyReadError) Error() string   { return "forge response body read failed: " + e.category }
func (e *bodyReadError) Timeout() bool   { return e.timeout }
func (e *bodyReadError) Temporary() bool { return e.temporary }
func (e *bodyReadError) Is(target error) bool {
	return (target == context.Canceled && e.canceled) || (target == context.DeadlineExceeded && e.deadline)
}

// statusRetriedBySDK mirrors the GitLab SDK's status-only retry set: 0, 429 and
// every 5xx but 501 (including 600-999).
func statusRetriedBySDK(code int) bool {
	return code == 0 || code == http.StatusTooManyRequests || (code >= 500 && code != http.StatusNotImplemented)
}

// responseLimitTransport bounds response bodies of requests marked with
// withResponseLimit, before the SDK decodes or classifies them:
//   - 2xx bodies and bodies of statuses the SDK retries on status alone are read
//     eagerly (32 MiB / 4 KiB). An overflow is a transport error with no
//     response, which the SDK does not retry.
//   - every other non-2xx body is streamed through a lazy 4 KiB+1 window so the
//     SDK and net/http read it exactly when they would without a limit.
//
// The limit counts decoded bytes. Content-Length is never consulted.
type responseLimitTransport struct{ base http.RoundTripper }

func (t responseLimitTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	state := responseLimitFrom(req.Context())
	if state == nil || isCIJSON(req.Context()) {
		return t.base.RoundTrip(req)
	}
	resp, err := t.base.RoundTrip(req)
	if err != nil || resp == nil || resp.Body == nil {
		return resp, err
	}
	switch code := resp.StatusCode; {
	case code/100 == 2:
		data, err := readBoundedBody(resp.Body, gitLabMaxResponseBytes)
		if err != nil {
			var tooLarge *ResponseTooLargeError
			if errors.As(err, &tooLarge) {
				state.overflow.Store(true)
				return nil, err
			}
			return nil, newBodyReadError(err)
		}
		setBuffered(resp, data)
	case statusRetriedBySDK(code):
		data, err := readBoundedBody(resp.Body, min(gitLabMaxResponseBytes, gitLabMaxErrorResponseBytes))
		if err != nil {
			var tooLarge *ResponseTooLargeError
			if errors.As(err, &tooLarge) {
				state.overflow.Store(true)
				return nil, err
			}
			data = nil
		}
		setBuffered(resp, data)
	default:
		resp.Body = &lazyCappedBody{rc: resp.Body, remaining: min(gitLabMaxResponseBytes, gitLabMaxErrorResponseBytes), state: state}
	}
	return resp, nil
}

// setBuffered replaces the body with its complete in-memory bytes. An empty body
// is http.NoBody, which is what a failed read hands the SDK.
func setBuffered(resp *http.Response, data []byte) {
	if len(data) == 0 {
		resp.Body = http.NoBody
	} else {
		resp.Body = io.NopCloser(bytes.NewReader(data))
	}
	resp.ContentLength = int64(len(data))
	resp.Header.Del("Content-Length")
}
