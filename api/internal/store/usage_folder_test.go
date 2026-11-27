package store_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4700 M2: UsageFolder.Notify and Wait, no database (Materialize is injected).

type foldCall struct {
	ids []uuid.UUID
	n   int64
	err error
}

func recvFold(t *testing.T, ch <-chan foldCall) foldCall {
	t.Helper()
	select {
	case c := <-ch:
		return c
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for onFolded")
		return foldCall{}
	}
}

func TestUsageFolderNotifyIgnoresNonTerminal(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	f := store.NewUsageFolder(nil, store.UsageFolderOptions{
		Materialize: func(context.Context, []uuid.UUID) (int64, error) {
			mu.Lock()
			calls++
			mu.Unlock()
			return 1, nil
		},
	})
	for _, st := range []string{"queued", "running", "awaiting_approval", "paused", ""} {
		f.Notify(uuid.New(), st)
	}
	if err := f.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 0 {
		t.Fatalf("non-terminal notifications materialized %d times, want 0", calls)
	}
}

func TestUsageFolderNotifyKicksTerminalAndReportsOutcome(t *testing.T) {
	for _, st := range []string{"completed", "failed", "cancelled"} {
		t.Run(st, func(t *testing.T) {
			got := make(chan foldCall, 1)
			var seen []uuid.UUID
			f := store.NewUsageFolder(nil, store.UsageFolderOptions{
				Materialize: func(_ context.Context, ids []uuid.UUID) (int64, error) {
					seen = ids
					return 1, nil
				},
				OnFolded: func(id uuid.UUID, n int64, err error) { got <- foldCall{ids: []uuid.UUID{id}, n: n, err: err} },
			})
			id := uuid.New()
			f.Notify(id, st)
			c := recvFold(t, got)
			if len(c.ids) != 1 || c.ids[0] != id || c.n != 1 || c.err != nil {
				t.Fatalf("onFolded = %+v, want run %s n=1 err=nil", c, id)
			}
			if err := f.Wait(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(seen) != 1 || seen[0] != id {
				t.Fatalf("materialized %v, want just [%s]", seen, id)
			}
		})
	}
}

func TestUsageFolderNotifySlotCapDropsThirdKick(t *testing.T) {
	release := make(chan struct{})
	started := make(chan uuid.UUID, 8)
	f := store.NewUsageFolder(nil, store.UsageFolderOptions{
		// Two kicks hold a slot each; the semaphore serialises the actual materialization, so
		// the second kick's goroutine waits on it and still occupies its slot.
		Materialize: func(_ context.Context, ids []uuid.UUID) (int64, error) {
			started <- ids[0]
			<-release
			return 1, nil
		},
	})
	a, b, c := uuid.New(), uuid.New(), uuid.New()
	f.Notify(a, "completed")
	f.Notify(b, "completed")
	f.Notify(c, "completed") // third concurrent kick: no slot, dropped
	if got := recvFoldID(t, started); got != a && got != b {
		t.Fatalf("first materialization was %s, want one of the two accepted kicks", got)
	}
	close(release)
	if err := f.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	close(started)
	seen := map[uuid.UUID]bool{}
	for id := range started {
		seen[id] = true
	}
	if seen[c] {
		t.Fatal("the dropped third kick was materialized")
	}
}

func recvFoldID(t *testing.T, ch <-chan uuid.UUID) uuid.UUID {
	t.Helper()
	select {
	case id := <-ch:
		return id
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a materialization")
		return uuid.Nil
	}
}

func TestUsageFolderSlotsFreeAfterKickFinishes(t *testing.T) {
	got := make(chan foldCall, 8)
	f := store.NewUsageFolder(nil, store.UsageFolderOptions{
		Materialize: func(context.Context, []uuid.UUID) (int64, error) { return 1, nil },
		OnFolded:    func(id uuid.UUID, n int64, err error) { got <- foldCall{ids: []uuid.UUID{id}} },
	})
	// onFolded runs just before the kick's slot is released, so a notify straight after it may
	// still be dropped; retry (bounded) until a kick is accepted. More than MaxKicks (2)
	// accepted kicks in a row proves slots are returned.
	for i := 0; i < 5; i++ {
		deadline := time.Now().Add(5 * time.Second)
		for accepted := false; !accepted; {
			if time.Now().After(deadline) {
				t.Fatalf("kick %d never accepted: slots are not released", i)
			}
			f.Notify(uuid.New(), "completed")
			select {
			case <-got:
				accepted = true
			case <-time.After(20 * time.Millisecond):
			}
		}
	}
}

func TestUsageFolderWaitStopsNewKicksAndIsBoundedByCtx(t *testing.T) {
	block := make(chan struct{})
	entered := make(chan struct{}, 1)
	calls := 0
	var mu sync.Mutex
	f := store.NewUsageFolder(nil, store.UsageFolderOptions{
		Materialize: func(ctx context.Context, _ []uuid.UUID) (int64, error) {
			mu.Lock()
			calls++
			mu.Unlock()
			entered <- struct{}{}
			select {
			case <-block:
			case <-ctx.Done():
			}
			return 0, ctx.Err()
		},
	})
	f.Notify(uuid.New(), "completed")
	<-entered
	wctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := f.Wait(wctx)
	if err == nil {
		t.Fatal("Wait returned nil while a kick was blocked")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("Wait took %v, want it bounded by its ctx", time.Since(start))
	}
	// Wait cancelled the stuck kick and closed the folder: nothing starts afterwards.
	f.Notify(uuid.New(), "completed")
	time.Sleep(20 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("materialize calls = %d, want 1 (no kick after Wait)", calls)
	}
}
