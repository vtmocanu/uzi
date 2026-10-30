package handler

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Exercise real transport reads, with an earlier parent deadline to avoid a
// production-duration sleep. A context timeout alone cannot unblock the body.
func TestWorkerCodexOperationBodyDeadline(t *testing.T) {
	done := make(chan struct{})
	entered := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := codexWorkerReq(true, uuid.NewString(), "")
		ctx, cancel := context.WithTimeout(req.Context(), 75*time.Millisecond)
		defer cancel()
		req = req.WithContext(ctx)
		req.Body = r.Body
		close(entered)
		(&Handler{}).WorkerCodexOperationAdmission(w, req)
		close(done)
	}))
	defer srv.Close()
	conn, err := net.DialTimeout("tcp", srv.Listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err = fmt.Fprint(conn, "POST / HTTP/1.1\r\nHost: example.test\r\nContent-Length: 1\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("request did not reach operation admission")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("operation admission body outlived the earlier request deadline")
	}
}
