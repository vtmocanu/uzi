package handler

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/jointoken"
)

// The production server drains HTTP before shutting down the prerequisite lane.
// Shutdown does not cancel HTTP handlers: S6's own bounded listing must let the
// drain finish. Exercise the authenticated route and real workersvc/broker port.
func TestHandlerPrerequisiteFinalShutdownDrainsListingLiveDB(t *testing.T) {
	testPrerequisiteFinalShutdown(t, false)
}

func TestHandlerPrerequisiteFinalShutdownAtomicCommitLiveDB(t *testing.T) {
	testPrerequisiteFinalShutdown(t, true)
}

func testPrerequisiteFinalShutdown(t *testing.T, present bool) {
	t.Helper()
	f := newWiredFinal(t, 100)
	token, hash, err := jointoken.Generate()
	if err != nil {
		t.Fatal(err)
	}
	f.mustExec("UPDATE workers SET token_hash=$2 WHERE id=$1", f.worker.ID, hash)
	started, stopped := make(chan struct{}), make(chan struct{})
	release := make(chan struct{})
	validListing := make(chan bool, 1)
	f.git.onListContext = func(ctx context.Context) {
		close(started)
		defer close(stopped)
		if present {
			select {
			case <-release:
				validListing <- ctx.Err() == nil
			case <-ctx.Done():
				validListing <- false
			}
		} else {
			<-ctx.Done()
		}
	}
	before := f.closure()
	var beforeCapture string
	if err := f.pool.QueryRow(f.ctx, "SELECT to_jsonb(c)::text FROM recovery_captures c WHERE id=$1", f.capture).Scan(&beforeCapture); err != nil {
		t.Fatal(err)
	}
	var active atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		active.Add(1)
		defer active.Add(-1)
		f.router.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	if present {
		// The valid listing cannot finish until graceful drain actually starts.
		server.Config.RegisterOnShutdown(func() { close(release) })
	}
	body := `{"generation":1,"final_disposition":{"kind":"archive","capture_id":"` + f.capture.String() +
		`","source_sha":"aaaa1111","coverage_digest":"` + strings.Repeat("a", 64) + `"}}`
	ctx, cancel := context.WithTimeout(f.ctx, 12*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/api/worker/runs/"+f.run.String()+"/archives/release", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	type answer struct {
		status   int
		reason   string
		released bool
		err      error
	}
	done := make(chan answer, 1)
	go func() {
		res, err := server.Client().Do(req)
		if err != nil {
			done <- answer{err: err}
			return
		}
		defer func() { _ = res.Body.Close() }()
		var out struct {
			Reason   string `json:"reason"`
			Released bool   `json:"released"`
		}
		err = json.NewDecoder(io.LimitReader(res.Body, 4096)).Decode(&out)
		done <- answer{status: res.StatusCode, reason: out.Reason, released: out.Released, err: err}
	}()
	select {
	case <-started:
	case a := <-done:
		t.Fatalf("request never entered the listing: %+v", a)
	case <-ctx.Done():
		t.Fatal("request never entered the listing")
	}
	drain, stopDrain := context.WithTimeout(f.ctx, 10*time.Second) // production HTTP drain budget
	defer stopDrain()
	start := time.Now()
	if err := server.Config.Shutdown(drain); err != nil {
		t.Fatalf("HTTP shutdown failed to drain S6: %v", err)
	}
	var reply answer
	select {
	case a := <-done:
		if a.err != nil {
			t.Fatalf("drained FINAL: %+v", a)
		}
		reply = a
		if a.status != http.StatusOK && (a.status != http.StatusServiceUnavailable || a.reason != "prerequisite_pin_unavailable" || a.released) {
			t.Fatalf("neither complete commit nor bounded refusal: %+v", a)
		}
	case <-ctx.Done():
		t.Fatal("HTTP shutdown left the request behind")
	}
	select {
	case <-stopped:
	case <-ctx.Done():
		t.Fatal("shutdown left a detached listing")
	}
	if present && !<-validListing {
		t.Fatal("positive listing expired before drain started; this is not the valid in-flight control")
	}
	if err := f.h.wsvc.DrainPrerequisiteClosures(drain); err != nil {
		t.Fatalf("prerequisite drain after HTTP shutdown: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 7*time.Second {
		t.Fatalf("shutdown plus prerequisite drain exceeded S6 budget plus margin: %v", elapsed)
	}
	if n := active.Load(); n != 0 {
		t.Fatalf("shutdown returned with %d request handlers still active", n)
	}
	// An ambiguous timeout response is not proof of an abort. Judge atomicity
	// from the persisted hold/capture pair after both drains have completed.
	var state string
	if err := f.pool.QueryRow(f.ctx, "SELECT state FROM recovery_custody_holds WHERE id=$1", f.holdID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	committed := state == "released"
	if present && (reply.status != http.StatusOK || !reply.released || !committed) {
		t.Fatalf("valid in-flight FINAL did not commit during drain; a load/deadline refusal is not a positive control: reply=%+v state=%s", reply, state)
	}
	if (!present && committed) || (reply.status == http.StatusOK && (!reply.released || !committed)) {
		t.Fatalf("drained response disagrees with complete database outcome: reply=%+v state=%s", reply, state)
	}
	if committed {
		f.assertReleased()
		var protected bool
		if err := f.pool.QueryRow(f.ctx, `SELECT local_replica_worker_id=$2 AND ready_retention_seconds>0
 FROM recovery_captures WHERE id=$1`, f.capture, f.worker.ID).Scan(&protected); err != nil || !protected {
			t.Fatalf("release committed without capture protection: %v %v", protected, err)
		}
	} else {
		f.assertOpen()
		var afterCapture string
		if err := f.pool.QueryRow(f.ctx, "SELECT to_jsonb(c)::text FROM recovery_captures c WHERE id=$1", f.capture).Scan(&afterCapture); err != nil || afterCapture != beforeCapture {
			t.Fatalf("aborted FINAL partially changed capture protection: %v", err)
		}
	}
	if got := f.git.hits.Load(); got != 1 {
		t.Fatalf("drain left repeated or detached listing work: %d listings", got)
	}
	after := f.closure()
	if after.State != "pinned" || after.AbsentSince.Valid || after.CleanupAttempts != before.CleanupAttempts ||
		!after.AuditNextAt.Time.Equal(before.AuditNextAt.Time) || !after.LastAuditPassID.Valid || after.LastAuditPassID == before.LastAuditPassID {
		t.Fatalf("shutdown changed more than the listing reservation: before=%+v after=%+v", before, after)
	}
	var open int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM pg_stat_activity
 WHERE datname=current_database() AND state LIKE 'idle in transaction%'`).Scan(&open); err != nil || open != 0 {
		t.Fatalf("shutdown left %d idle transactions: %v", open, err)
	}
}
