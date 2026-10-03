package forge

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Run each mode in a child process: installing the test CA on the shared
// forge transport must not affect other tests or leave a frozen pooled socket.
func TestForgeHTTP2StallInvestigation(t *testing.T) {
	if os.Getenv("FORGE_STALL_CHILD") == "" {
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for _, mode := range []string{"baseline", "recovery"} {
			//nolint:gosec // os.Executable returns this test binary; arguments are fixed, with no external input.
			cmd := exec.CommandContext(ctx, executable, "-test.run=^TestForgeHTTP2StallInvestigation$", "-test.v")
			cmd.Env = append(os.Environ(), "FORGE_STALL_CHILD="+mode)
			output, err := cmd.CombinedOutput()
			t.Logf("isolated %s:\n%s", mode, output)
			if err != nil {
				t.Fatalf("%s child: %v", mode, err)
			}
		}
		return
	}

	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 {
			http.Error(w, "HTTP/2 required", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":1,"login":"test-bot"}`)
	}))
	listener := &stallListener{Listener: server.Listener}
	server.Listener = listener
	server.EnableHTTP2 = true
	server.StartTLS()
	// Close the wrapped sockets before server.Close waits for HTTP/2 handlers.
	t.Cleanup(func() {
		listener.closeConnections()
		server.Close()
	})
	transport := http.DefaultTransport.(*http.Transport).Clone()
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	transport.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	recovery := os.Getenv("FORGE_STALL_CHILD") == "recovery"
	if recovery {
		transport = newForgeHTTPTransport(http.DefaultTransport)
		transport.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
		// Scale only the durations in the child; production defaults have their
		// own contract test. Exercise the real constructor and shared pool.
		if transport.HTTP2.SendPingTimeout > 0 {
			transport.HTTP2.SendPingTimeout = 50 * time.Millisecond
		}
		if transport.HTTP2.PingTimeout > 0 {
			transport.HTTP2.PingTimeout = 200 * time.Millisecond
		}
	}
	forgeHTTPTransport = transport
	t.Cleanup(transport.CloseIdleConnections)

	var warmConnection net.Conn
	request := func() (bool, net.Conn, time.Duration, error) {
		// Rebuild the real driver for every attempt, preserving its ETag wrapper,
		// origin pin, redirect policy, timeout and redaction path.
		driver, err := newGitHub(server.URL, "stall-test-token", 500*time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
		var reused bool
		var connection net.Conn
		trace := &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) {
			reused = info.Reused
			connection = info.Conn
		}}
		ctx := httptrace.WithClientTrace(context.Background(), trace)
		start := time.Now()
		_, err = driver.VerifyToken(ctx)
		return reused, connection, time.Since(start), err
	}
	reused, connection, elapsed, err := request()
	if err != nil || reused || connection == nil {
		t.Fatalf("warm request: reused=%v connection=%v elapsed=%v err=%v", reused, connection != nil, elapsed, err)
	}
	warmConnection = connection
	listener.freezeFirst(t)
	start := time.Now()
	for attempt := 1; attempt <= 3; attempt++ {
		reused, connection, elapsed, err = request()
		t.Logf("attempt=%d reused=%v same_connection=%v elapsed=%v since_freeze=%v error=%v", attempt, reused, connection == warmConnection, elapsed, time.Since(start), err)
		if recovery && err == nil {
			if connection == nil || connection == warmConnection {
				t.Fatal("recovery did not establish a fresh connection")
			}
			t.Logf("recovered on attempt=%d", attempt)
			return
		}
		if recovery && err != nil && strings.Contains(err.Error(), "http2: client connection lost") {
			continue // An unanswered health ping evicts the frozen connection.
		}
		if err == nil || (!strings.Contains(err.Error(), "Client.Timeout") && !strings.Contains(err.Error(), "context deadline exceeded")) {
			t.Fatalf("attempt %d: expected client timeout, got %v", attempt, err)
		}
		if !recovery && elapsed < 450*time.Millisecond {
			t.Fatalf("attempt %d failed before the 500ms client deadline: %v", attempt, elapsed)
		}
		if !recovery && (!reused || connection != warmConnection) {
			t.Fatalf("attempt %d did not reuse the blackholed connection", attempt)
		}
	}
	if recovery {
		t.Fatal("no recovery after three rebuilt-client attempts")
	}
}

// Freezing operates below TLS: a read already blocked in the kernel can finish,
// but its bytes never reach HTTP/2, and subsequent writes cannot reach the peer.
// The socket stays open until cleanup. Pings are therefore unanswered too.
type stallConn struct {
	net.Conn
	frozen atomic.Bool
	closed chan struct{}
	once   sync.Once
}

func (c *stallConn) awaitCloseWhenFrozen() error {
	if c.frozen.Load() {
		<-c.closed
		return net.ErrClosed
	}
	return nil
}

func (c *stallConn) Read(p []byte) (int, error) {
	if err := c.awaitCloseWhenFrozen(); err != nil {
		return 0, err
	}
	n, err := c.Conn.Read(p)
	if frozenErr := c.awaitCloseWhenFrozen(); frozenErr != nil {
		return 0, frozenErr
	}
	return n, err
}

func (c *stallConn) Write(p []byte) (int, error) {
	if err := c.awaitCloseWhenFrozen(); err != nil {
		return 0, err
	}
	return c.Conn.Write(p)
}

func (c *stallConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

type stallListener struct {
	net.Listener
	mu    sync.Mutex
	conns []*stallConn
}

func (l *stallListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	wrapped := &stallConn{Conn: conn, closed: make(chan struct{})}
	l.mu.Lock()
	l.conns = append(l.conns, wrapped)
	l.mu.Unlock()
	return wrapped, nil
}

func (l *stallListener) freezeFirst(t *testing.T) {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.conns) != 1 {
		t.Fatalf("warm request established %d connections, want one", len(l.conns))
	}
	l.conns[0].frozen.Store(true)
}

func (l *stallListener) closeConnections() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, conn := range l.conns {
		_ = conn.Close()
	}
}
