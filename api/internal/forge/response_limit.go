package forge

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"syscall"
)

// gitLabMaxResponseBytes caps the body of a successful (SDK success status, see
// statusSDKSuccess) GitLab response on a marked call. A package var so tests can
// lower it, like forgejoMaxResponseBytes.
var gitLabMaxResponseBytes int64 = 32 << 20

// gitLabMaxErrorResponseBytes caps the body of every other response on a marked
// call. An overflow discards the body so a token split at the boundary
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

// bodyReadError wraps a mid-body read failure of an SDK-success response that the
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

// statusSDKSuccess is the set of statuses the GitLab SDK's CheckResponse
// (gitlab.com/gitlab-org/api/client-go/v3@v3.16.0 gitlab.go) accepts and whose
// body can be non-empty, so it is decoded. CheckResponse also accepts 304, which
// carries no body and stays lazy; other 2xx statuses (203, 205, 206, ...) are
// errors to the SDK.
func statusSDKSuccess(code int) bool {
	switch code {
	case http.StatusOK, http.StatusCreated, http.StatusAccepted, http.StatusNoContent:
		return true
	}
	return false
}

// statusRetriedBySDK mirrors the GitLab SDK's status-only retry set: 0, 429 and
// every 5xx but 501 (including 600-999).
func statusRetriedBySDK(code int) bool {
	return code == 0 || code == http.StatusTooManyRequests || (code >= 500 && code != http.StatusNotImplemented)
}

// declaresContentEncoding reports whether any Content-Encoding header line names
// an encoding other than identity; a second line must not hide behind a first.
func declaresContentEncoding(h http.Header) bool {
	for _, enc := range h.Values("Content-Encoding") {
		if enc != "" && !strings.EqualFold(enc, "identity") {
			return true
		}
	}
	return false
}

// responseLimitTransport bounds response bodies of requests marked with
// withResponseLimit, before the SDK decodes or classifies them:
//   - bodies of SDK success statuses and of statuses the SDK retries on status
//     alone are read eagerly (32 MiB / 4 KiB). An overflow is a transport error
//     with no response, which the SDK does not retry.
//   - every other body is streamed through a lazy 4 KiB+1 window so the SDK and
//     net/http read it exactly when they would without a limit.
//
// Every counted byte is a transfer-decoded wire body byte: the request carries
// Accept-Encoding: identity so net/http neither asks for nor inflates gzip, and a
// response that still declares a content encoding is refused without being read.
// The route transport sends marked requests over HTTP/1.1, where every body byte
// reaches the reader. Content-Length is never consulted.
type responseLimitTransport struct{ base http.RoundTripper }

func (t responseLimitTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	state := responseLimitFrom(req.Context())
	if state == nil || isCIJSON(req.Context()) {
		return t.base.RoundTrip(req)
	}
	req = req.Clone(req.Context())
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := t.base.RoundTrip(req)
	if err != nil || resp == nil || resp.Body == nil {
		return resp, err
	}
	if declaresContentEncoding(resp.Header) {
		// Forced compression: the wire bytes are not the content. Read nothing; keep
		// the status so classification survives, but never hand a body on.
		_ = resp.Body.Close()
		if statusSDKSuccess(resp.StatusCode) {
			return nil, &bodyReadError{category: "unsupported content encoding"}
		}
		setBuffered(resp, nil)
		return resp, nil
	}
	switch code := resp.StatusCode; {
	case statusSDKSuccess(code):
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
