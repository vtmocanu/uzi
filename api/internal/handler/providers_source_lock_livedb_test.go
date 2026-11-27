package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestClaudeSourceClearQueuesBehindSharedLockLiveDB: the REAL PUT /api/me/providers/claude-source clear
// (SetMyClaudeProviderSource, through the production router) takes the exclusive secret-mutation lock,
// so while a transaction holds the shared lock (the lock a provider job create takes and holds to its
// commit) the clear is observed queued in pg_locks and unfinished, and it applies only after that
// transaction commits. The shared holder here is a bare transaction, standing in for a job create held
// mid-flight: the create side's lock use is proven by workersvc.TestProviderJobCreateRaceCreateFirstLiveDB,
// whose pause seam (jobDecidedHook) is unexported and so unreachable from this package; this test covers the handler half of that race.
//
// MUTATION CHECK: taking no (or only the shared) lock in setMyProviderSource's withSecretLock makes the
// queued-waiter wait time out red, or the clear finish while the holder is open.
func TestClaudeSourceClearQueuesBehindSharedLockLiveDB(t *testing.T) {
	f := newProviderFixture(t, testGatewayURL)
	p := f.create(t, validCreateBody)
	f.setKey(t, p.ID)
	if code, _, body := f.putSource(t, true, `{"provider_id":"`+p.ID+`"}`); code != http.StatusOK {
		t.Fatalf("select = %d %s", code, body)
	}
	ctx := context.Background()

	holder, err := f.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.Rollback(context.Background()) })
	if err := store.LockSecretMutationShared(ctx, holder, f.user); err != nil {
		t.Fatalf("shared lock: %v", err)
	}

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- cookieReq(t, f.router, http.MethodPut, "/api/me/providers/claude-source", f.jwt, `{"provider_id":null}`)
	}()

	waiters := func() int {
		var c int
		if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND NOT granted AND objsubid = 2
			AND classid::bigint = ($1::bigint & 4294967295) AND objid::bigint = ($2::bigint & 4294967295)`,
			int64(store.SecretMutationLockClass), int64(store.SecretMutationLockObjID(f.user))).Scan(&c); err != nil {
			t.Fatalf("count lock waiters: %v", err)
		}
		return c
	}
	deadline := time.Now().Add(10 * time.Second)
	for waiters() < 1 {
		select {
		case rec := <-done:
			t.Fatalf("the clear finished (%d %s) while the shared lock was held", rec.Code, rec.Body.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the clear was never seen queued behind the shared lock")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := f.sourceInSettings(t); got == nil || *got != p.ID {
		t.Fatalf("while the clear is queued the source = %v, want it unchanged (%s)", got, p.ID)
	}
	select {
	case rec := <-done:
		t.Fatalf("the clear finished (%d) while the shared lock was held", rec.Code)
	default:
	}

	if err := holder.Commit(ctx); err != nil {
		t.Fatalf("holder commit: %v", err)
	}
	select {
	case rec := <-done:
		if rec.Code != http.StatusOK {
			t.Fatalf("clear after the holder committed = %d %s", rec.Code, rec.Body.String())
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the clear did not finish after the shared lock was released")
	}
	if got := f.sourceInSettings(t); got != nil {
		t.Fatalf("after the clear the source = %s, want cleared", *got)
	}
}
