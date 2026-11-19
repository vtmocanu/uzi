package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #4213 M3: a size that exists at the handler's pre-transaction check but is gone by the
// in-transaction FOR SHARE must be answered 400 "unknown worker size" (errUnknownHostedSize),
// never 500. The seam is the provision's own first lock: a test transaction holds the user's
// hosted-provision advisory lock, so the POST parks after its Exists check and before the size
// row lock; the size row is deleted and committed while it is parked.
func TestProvisionHostedWorkerSizeDeletedAfterPreCheckLiveDB(t *testing.T) {
	h, pool, _, _, userID := hostedLiveDB(t, "20")
	ctx := context.Background()
	const size = "zz-u9g"
	clean := func() {
		_, _ = pool.Exec(ctx, `DELETE FROM workers WHERE user_id = $1`, userID)
		_, _ = pool.Exec(ctx, `DELETE FROM worker_sizes WHERE name = $1`, size)
	}
	clean()
	if _, err := pool.Exec(ctx, `INSERT INTO worker_sizes (name, cpu_request_millicores, cpu_limit_millicores, memory_request_mib, memory_limit_mib, data_gib, dind_data_gib, display_order)
		VALUES ($1, 500, 2000, 1024, 2048, 10, 10, 96)`, size); err != nil {
		t.Fatalf("seed size: %v", err)
	}
	t.Cleanup(clean)

	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.Rollback(ctx) })
	if _, err := holder.Exec(ctx, "SELECT pg_advisory_xact_lock($1, $2)",
		store.HostedProvisionLockClass, hostedProvisionLockObjID(userID)); err != nil {
		t.Fatal(err)
	}
	var holderPid int32
	if err := holder.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&holderPid); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/workers/hosted", strings.NewReader(`{"template":"base","size":"`+size+`"}`))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(mw.ContextWithUser(req.Context(), store.User{ID: userID, Email: "u@uzi.local", IsActive: true}))
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		h.ProvisionHostedWorker(rec, req)
		done <- rec
	}()

	// The request is now past its Exists check (that precedes the transaction) and queued on
	// the advisory lock the holder owns.
	waitBlockedBy(t, pool, holderPid)
	if _, err := holder.Exec(ctx, `DELETE FROM worker_sizes WHERE name = $1`, size); err != nil {
		t.Fatal(err)
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	rec := awaitResult(t, done, "the provision request")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "unknown worker size") {
		t.Fatalf("provision after the size vanished = %d %s, want 400 unknown worker size", rec.Code, rec.Body.String())
	}
	if n := countHostedWorkers(ctx, t, pool, userID); n != 0 {
		t.Fatalf("a worker was created on a deleted size (%d)", n)
	}
}
