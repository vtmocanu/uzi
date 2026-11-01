package handler

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/jointoken"
)

// The production server drains HTTP before shutting down the prerequisite lane.
// Shutdown does not cancel HTTP handlers: S6's own bounded listing must let the
// drain finish. Exercise the authenticated route and real workersvc/broker port.
func TestHandlerPrerequisiteFinalShutdownDrainsListingLiveDB(t *testing.T) {
	f := newWiredFinal(t, 100)
	token, hash, err := jointoken.Generate()
	if err != nil {
		t.Fatal(err)
	}
	f.mustExec("UPDATE workers SET token_hash=$2 WHERE id=$1", f.worker.ID, hash)
	started, stopped := make(chan struct{}), make(chan struct{})
	f.git.onListContext = func(ctx context.Context) {
		close(started)
		<-ctx.Done()
		close(stopped)
	}
	before := f.closure()
	server := httptest.NewServer(f.router)
	t.Cleanup(server.Close)
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
		status int
		reason string
		err    error
	}
	done := make(chan answer, 1)
	go func() {
		res, err := server.Client().Do(req)
		if err != nil {
			done <- answer{err: err}
			return
		}
		defer res.Body.Close()
		var out struct {
			Reason string `json:"reason"`
		}
		err = json.NewDecoder(io.LimitReader(res.Body, 4096)).Decode(&out)
		done <- answer{res.StatusCode, out.Reason, err}
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
	if elapsed := time.Since(start); elapsed > 7*time.Second {
		t.Fatalf("shutdown exceeded S6 budget plus margin: %v", elapsed)
	}
	select {
	case a := <-done:
		if a.err != nil || a.status != http.StatusServiceUnavailable || a.reason != "prerequisite_pin_unavailable" {
			t.Fatalf("drained FINAL: %+v", a)
		}
	case <-ctx.Done():
		t.Fatal("HTTP shutdown left the request behind")
	}
	select {
	case <-stopped:
	case <-ctx.Done():
		t.Fatal("shutdown left a detached listing")
	}
	if err := f.h.wsvc.DrainPrerequisiteClosures(drain); err != nil {
		t.Fatalf("prerequisite drain after HTTP shutdown: %v", err)
	}
	f.assertOpen()
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
