package pushbroker_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/pushbroker"
)

// Copying an opaque prepared value must not mint a second send capability.
func TestPrereqPreparedCopiesShareSend(t *testing.T) {
	f, remote, url, _, _, _ := heldHTTPFixture(t)
	var posts atomic.Int32
	proxy := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git-receive-pack") {
			posts.Add(1)
		}
		remote.srv.Config.Handler.ServeHTTP(w, r)
	}))
	defer proxy.Close()
	url = proxy.URL + strings.TrimPrefix(url, remote.srv.URL)
	p, err := pushbroker.PreparePrereqCreate(context.Background(), prereqOpts(url))
	if err != nil {
		t.Fatal(err)
	}
	copied := *p
	if out, err := pushbroker.SendPrereqCreate(context.Background(), p); out != pushbroker.PrereqCreateCreated || err != nil {
		t.Fatalf("first send = %v, %v", out, err)
	}
	if _, err := pushbroker.SendPrereqCreate(context.Background(), &copied); !errors.Is(err, pushbroker.ErrInvalidRef) {
		t.Fatalf("copied preparation replayed: sends=%d err=%v ref=%s", posts.Load(), err, f.originRef(p.Ref()))
	}
	if posts.Load() != 1 {
		t.Fatalf("copied capability sent %d POSTs, want one", posts.Load())
	}
}
