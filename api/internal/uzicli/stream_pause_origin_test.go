package uzicli

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

func TestRunStreamPauseCoverageReplayFailure(t *testing.T) {
	server := newStreamServer(t)
	original := server.Config.Handler
	var fail atomic.Bool
	fail.Store(true)
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/messages") && fail.Load() {
			http.Error(w, "offline", http.StatusServiceUnavailable)
			return
		}
		original.ServeHTTP(w, r)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := server.client("test").StreamRun(ctx, testRunID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		deadline := time.Now().Add(5 * time.Second)
		stream.Close()
		if !stream.waitStopped(time.Until(deadline)) {
			t.Error("stream did not stop")
		}
	}()
	conn := server.nextConn(t)
	stream.NoteSeen(100)
	_ = conn.CloseNow()
	conn = server.nextConn(t)
	// Failed message replay still reconciles state. This used to hide the read error.
	nextEvent(t, stream, "state reconcile after failed replay")
	waitCoverage := func(predicate func(MessageCoverage) bool) MessageCoverage {
		t.Helper()
		deadline := time.NewTimer(5 * time.Second)
		defer deadline.Stop()
		tick := time.NewTicker(time.Millisecond)
		defer tick.Stop()
		for {
			c := stream.Coverage()
			if predicate(c) {
				return c
			}
			select {
			case <-deadline.C:
				t.Fatalf("coverage did not settle: %+v", c)
			case <-tick.C:
			}
		}
	}
	first := waitCoverage(func(c MessageCoverage) bool { return c.Failed && !c.Busy })
	if first.FailedAfter != 100 || first.Gapped || first.Generation != 1 || first.Revision != 1 {
		t.Fatalf("failed replay floor %+v", first)
	}
	pushFrame(t, conn, apitypes.RunEventDTO{Type: RunEventTypeMessage, Seq: 102, Kind: "paused"})
	if event := nextEvent(t, stream, "live frame above missing 101"); event.Seq != 102 {
		t.Fatal(event)
	}
	gap := stream.Coverage()
	if gap.After != 100 || gap.Through != 102 || gap.Revision != first.Revision {
		t.Fatalf("gap %+v", gap)
	}
	fail.Store(false)
	server.setMessages([]apitypes.MessageDTO{{Seq: 103, Kind: "paused", CreatedAt: time.Now()}})
	_ = conn.CloseNow()
	_ = server.nextConn(t)
	if event := nextEvent(t, stream, "successful replay above unresolved hole"); event.Seq != 103 {
		t.Fatal(event)
	}
	recovered := waitCoverage(func(c MessageCoverage) bool { return !c.Busy })
	if !recovered.Failed || recovered.After != 100 || recovered.Through < 102 {
		t.Fatalf("cursor erased hole: %+v", recovered)
	}
	if stream.LastSeen() != 103 {
		t.Fatalf("transport cursor changed semantics: %d", stream.LastSeen())
	}
}
