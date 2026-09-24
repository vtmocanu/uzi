package uzicli

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newBlockingServer starts a server whose handler blocks until the request's
// context ends or the test finishes, so a caller-side timeout is the only way the
// request returns. The release channel is closed in a cleanup registered AFTER
// srv.Close, so (cleanups run LIFO) the handler is unblocked before Close waits
// on it: no leaked handler goroutine and no slow Close.
func newBlockingServer(t *testing.T) *httptest.Server {
	t.Helper()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	return srv
}

// assertTimeoutMessage pins the #1620 wording: a timeout reads "did not respond in
// time", never "cannot reach", and still exits ExitUnreachable.
func assertTimeoutMessage(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("err = nil, want a timeout error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "did not respond in time") {
		t.Errorf("message = %q, want it to contain %q", msg, "did not respond in time")
	}
	if strings.Contains(msg, "cannot reach") {
		t.Errorf("message = %q, must not say %q for a timeout", msg, "cannot reach")
	}
	if got := ExitCodeFor(err); got != ExitUnreachable {
		t.Errorf("ExitCodeFor = %d, want %d", got, ExitUnreachable)
	}
}

// TestTransportTimeoutContextDeadline: the caller's context deadline (the TUI's
// per-poll timeout shape) expiring on a reachable-but-slow server.
func TestTransportTimeoutContextDeadline(t *testing.T) {
	srv := newBlockingServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err := newTestClient(srv).Whoami(ctx)
	assertTimeoutMessage(t, err)
}

// TestTransportTimeoutHTTPClientTimeout: the http.Client Timeout (surfacing as a
// *url.Error with Timeout() true, "Client.Timeout exceeded"), with no context
// deadline at all.
func TestTransportTimeoutHTTPClientTimeout(t *testing.T) {
	srv := newBlockingServer(t)
	c := newTestClient(srv)
	c.HTTP.Timeout = 100 * time.Millisecond

	_, err := c.Whoami(context.Background())
	assertTimeoutMessage(t, err)
}

// TestTransportTimeoutRecoveryDownload: the recovery-archive download is the
// second producer and uses the same wording.
func TestTransportTimeoutRecoveryDownload(t *testing.T) {
	srv := newBlockingServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err := newTestClient(srv).DownloadRecoveryArchive(ctx, "r1", "c1", &strings.Builder{})
	assertTimeoutMessage(t, err)
}

// TestTransportRefusedKeepsCannotReach: a closed port is a genuine connection
// failure and keeps the "cannot reach" wording, still ExitUnreachable.
func TestTransportRefusedKeepsCannotReach(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	c := &HTTPClient{BaseURL: "http://" + addr, Token: "uzc_test", HTTP: &http.Client{Timeout: 5 * time.Second}}

	_, err = c.Whoami(context.Background())
	if err == nil {
		t.Fatal("err = nil, want a connection error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "cannot reach uzi") {
		t.Errorf("message = %q, want it to contain %q", msg, "cannot reach uzi")
	}
	if strings.Contains(msg, "did not respond in time") {
		t.Errorf("message = %q, a refused connection is not a timeout", msg)
	}
	if got := ExitCodeFor(err); got != ExitUnreachable {
		t.Errorf("ExitCodeFor = %d, want %d", got, ExitUnreachable)
	}
}

// TestTransportExitCanceledIsNotTimeout: a cancelled context is not a timeout.
func TestTransportExitCanceledIsNotTimeout(t *testing.T) {
	err := transportExit("http://uzi.example", context.Canceled)
	if !strings.Contains(err.Error(), "cannot reach uzi") {
		t.Errorf("message = %q, want %q for context.Canceled", err.Error(), "cannot reach uzi")
	}
	if got := ExitCodeFor(err); got != ExitUnreachable {
		t.Errorf("ExitCodeFor = %d, want %d", got, ExitUnreachable)
	}
}

// fakeTimeoutErr is a net.Error whose Timeout() is true but which, unlike every
// timeout the standard library produces today, does NOT match
// context.DeadlineExceeded: it isolates isTimeout's net.Error branch.
type fakeTimeoutErr struct{}

func (fakeTimeoutErr) Error() string   { return "fake connect timeout" }
func (fakeTimeoutErr) Timeout() bool   { return true }
func (fakeTimeoutErr) Temporary() bool { return true }

// TestTransportExitNetErrorTimeout: a connect timeout surfaced only as a
// net.Error (Timeout() true, no context.DeadlineExceeded in the chain) still gets
// the "did not respond in time" wording.
func TestTransportExitNetErrorTimeout(t *testing.T) {
	cause := &net.OpError{Op: "dial", Net: "tcp", Err: fakeTimeoutErr{}}
	if errors.Is(cause, context.DeadlineExceeded) {
		t.Fatal("fixture matches context.DeadlineExceeded; it no longer isolates the net.Error branch")
	}
	assertTimeoutMessage(t, transportExit("http://uzi.example", cause))
}
