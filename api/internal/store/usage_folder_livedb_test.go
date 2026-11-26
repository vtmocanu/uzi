package store_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4700 M2: the UsageFolder sweeper pass against a live database. Every test skips
// without UZI_TEST_DATABASE_URL (run via ./e2e/run-store-it.sh). The database is shared with
// other tests, so assertions are always about the test's OWN runs and pass counts are bounded
// by a count taken from the database, never by a guess.

// staleRun is a terminal run with usage written, so its state row exists and is stale.
func (e *foldEnv) staleRun(t *testing.T) uuid.UUID {
	t.Helper()
	r := e.run(t, "completed")
	if err := e.upsert(t, e.q, r, "s1", "m1", 0, 1000); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	e.wantState(t, "staleRun", r, true, true)
	return r
}

// missingRun is a terminal run with no state row at all.
func (e *foldEnv) missingRun(t *testing.T) uuid.UUID {
	t.Helper()
	r := e.run(t, "completed")
	e.wantState(t, "missingRun", r, false, false)
	return r
}

func (e *foldEnv) isFresh(t *testing.T, run uuid.UUID) bool {
	t.Helper()
	exists, stale := e.state(t, run)
	return exists && !stale
}

func (e *foldEnv) wantFresh(t *testing.T, label string, run uuid.UUID) {
	t.Helper()
	e.wantState(t, label, run, true, false)
}

// pendingWork counts the eligible runs a Pass could still pick: missing or stale.
func (e *foldEnv) pendingWork(t *testing.T) int {
	t.Helper()
	var n int
	err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM runs r
		WHERE r.status IN ('completed','failed','cancelled') AND r.kind <> 'chat' AND r.usage_refolded
		  AND (NOT EXISTS (SELECT 1 FROM run_usage_fold_state s WHERE s.run_id = r.id)
		       OR EXISTS (SELECT 1 FROM run_usage_fold_state s WHERE s.run_id = r.id AND s.stale))`).Scan(&n)
	if err != nil {
		t.Fatalf("pending work: %v", err)
	}
	return n
}

// passUntil runs Pass until cond holds, at most max times, failing on a Pass error.
func passUntil(t *testing.T, f *store.UsageFolder, max int, cond func() bool) int {
	t.Helper()
	for i := 1; i <= max; i++ {
		if _, err := f.Pass(context.Background()); err != nil {
			t.Fatalf("Pass %d: %v", i, err)
		}
		if cond() {
			return i
		}
	}
	t.Fatalf("condition still false after %d passes", max)
	return 0
}

// realMaterialize is the production materializer, for options that wrap it.
func (e *foldEnv) realMaterialize(ctx context.Context, ids []uuid.UUID) (int64, error) {
	return store.MaterializeRunUsageFolds(ctx, e.pool, ids)
}

func containsID(ids []uuid.UUID, set ...uuid.UUID) bool {
	for _, id := range ids {
		for _, s := range set {
			if id == s {
				return true
			}
		}
	}
	return false
}

// A terminal write that never kicked the folder (status set in plain SQL, no Notify) is
// folded by the Pass, through the missing lane (no state yet) and the stale lane (stale).
func TestUsageFolderPassFoldsNonKickingTerminalWrite(t *testing.T) {
	e := newFoldEnv(t)
	f := store.NewUsageFolder(e.pool, store.UsageFolderOptions{})

	// Missing lane: a running run that goes terminal by direct SQL.
	direct := e.run(t, "running")
	mustExec(e.ctx, t, e.pool, `UPDATE runs SET status = 'completed' WHERE id = $1`, direct)
	// Stale lane: a terminal run whose usage was written after (or without) any fold.
	stale := e.staleRun(t)

	bound := 2*e.pendingWork(t) + 5
	passUntil(t, f, bound, func() bool { return e.isFresh(t, direct) && e.isFresh(t, stale) })
	if rows := e.foldRows(t, e.pool, stale); len(rows) != 1 {
		t.Fatalf("stale run fold rows = %v, want exactly one model row", rows)
	}
}

// A Pass never folds what is not eligible: non-terminal and chat runs stay unfolded.
func TestUsageFolderPassSkipsIneligibleRuns(t *testing.T) {
	e := newFoldEnv(t)
	f := store.NewUsageFolder(e.pool, store.UsageFolderOptions{})
	running := e.run(t, "running")
	chat := e.chatRun(t)
	ok := e.missingRun(t)

	passUntil(t, f, 2*e.pendingWork(t)+5, func() bool { return e.isFresh(t, ok) })
	e.wantState(t, "running", running, false, false)
	e.wantState(t, "chat", chat, false, false)
}

// More eligible runs than the per-tick limit: successive Passes fold all of them, and a run
// the materializer skipped (its state row held locked) is reselected after the cursor wraps.
func TestUsageFolderCursorProgressAndWrap(t *testing.T) {
	e := newFoldEnv(t)
	f := store.NewUsageFolder(e.pool, store.UsageFolderOptions{LaneLimit: 2})

	var missing, stale []uuid.UUID
	for i := 0; i < 5; i++ {
		missing = append(missing, e.missingRun(t))
		stale = append(stale, e.staleRun(t))
	}
	all := func() bool {
		for _, r := range append(append([]uuid.UUID{}, missing...), stale...) {
			if !e.isFresh(t, r) {
				return false
			}
		}
		return true
	}
	// Each Pass folds up to 2 per lane, so the total backlog bounds the passes needed.
	passUntil(t, f, 2*e.pendingWork(t)+5, all)

	// Wrap: a stale run with the smallest possible id, whose state row is held locked so the
	// materializer skips it. With LaneLimit 1 the stale cursor walks past it; once the lane
	// runs dry it must wrap to the start and fold it after the lock is released.
	low := uuid.MustParse("00000000-0000-0000-0000-00000000a001")
	mustExec(e.ctx, t, e.pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, usage_refolded)
		 VALUES ($1, $2, $3, $4, 't', 'd', 'completed', true)`, low, e.userID, e.repoID, e.nextIID())
	if err := e.upsert(t, e.q, low, "s1", "m1", 0, 1000); err != nil {
		t.Fatal(err)
	}
	e.wantState(t, "low", low, true, true)

	one := store.NewUsageFolder(e.pool, store.UsageFolderOptions{LaneLimit: 1})
	tx, err := e.pool.Begin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	released := false
	release := func() {
		if !released {
			released = true
			_ = tx.Rollback(e.ctx)
		}
	}
	defer release()
	var locked uuid.UUID
	if err := tx.QueryRow(e.ctx, `SELECT run_id FROM run_usage_fold_state WHERE run_id = $1 FOR UPDATE`, low).Scan(&locked); err != nil {
		t.Fatal(err)
	}
	backlog := e.pendingWork(t)
	// Walk the cursor past `low` while it is locked (it is skipped, so it stays stale).
	for i := 0; i < backlog+3; i++ {
		if _, err := one.Pass(context.Background()); err != nil {
			t.Fatalf("locked pass %d: %v", i, err)
		}
	}
	e.wantState(t, "low while locked", low, true, true)
	release()
	passUntil(t, one, 2*backlog+5, func() bool { return e.isFresh(t, low) })
}

// One lane failing or running out of budget does not stop the other, in either order.
func TestUsageFolderLaneIsolation(t *testing.T) {
	for _, blockStale := range []bool{true, false} {
		name := "missing lane blocked"
		if blockStale {
			name = "stale lane blocked"
		}
		t.Run(name, func(t *testing.T) {
			e := newFoldEnv(t)
			var mu sync.Mutex
			var blocked []uuid.UUID
			f := store.NewUsageFolder(e.pool, store.UsageFolderOptions{
				ParentTimeout: 5 * time.Second,
				LaneTimeout:   300 * time.Millisecond,
				Materialize: func(ctx context.Context, ids []uuid.UUID) (int64, error) {
					mu.Lock()
					hit := containsID(ids, blocked...)
					mu.Unlock()
					if hit {
						<-ctx.Done()
						return 0, ctx.Err()
					}
					return e.realMaterialize(ctx, ids)
				},
			})
			var stuck uuid.UUID
			if blockStale {
				stuck = e.staleRun(t)
			} else {
				stuck = e.missingRun(t)
			}
			mu.Lock()
			blocked = []uuid.UUID{stuck}
			mu.Unlock()

			// Two Passes: the lane order alternates, so the blocked lane runs first once.
			for i := 0; i < 2; i++ {
				var other uuid.UUID
				if blockStale {
					other = e.missingRun(t)
				} else {
					other = e.staleRun(t)
				}
				start := time.Now()
				_, err := f.Pass(context.Background())
				if elapsed := time.Since(start); elapsed > 3*time.Second {
					t.Fatalf("Pass %d took %v with one lane blocked", i, elapsed)
				}
				if i == 0 && !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("Pass 0 err = %v, want the blocked lane's deadline", err)
				}
				e.wantFresh(t, "the other lane's run", other)
			}
			if e.isFresh(t, stuck) {
				t.Fatal("the blocked run was folded")
			}
		})
	}
}

// The parent deadline bounds the whole Pass even when the per-lane budget is larger: both
// lanes block, and Pass still returns within the parent bound.
func TestUsageFolderParentDeadlineBoundsPass(t *testing.T) {
	e := newFoldEnv(t)
	f := store.NewUsageFolder(e.pool, store.UsageFolderOptions{
		ParentTimeout: 300 * time.Millisecond,
		LaneTimeout:   20 * time.Second,
		Materialize: func(ctx context.Context, _ []uuid.UUID) (int64, error) {
			<-ctx.Done()
			return 0, ctx.Err()
		},
	})
	e.staleRun(t)
	e.missingRun(t)

	start := time.Now()
	_, err := f.Pass(context.Background())
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("Pass took %v, want it bounded by the 300ms parent deadline", elapsed)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Pass err = %v, want a deadline error", err)
	}
}

// A run that fails on its own is retried alone, skipped for the TTL, and picked up again once
// the (injected) clock passes it; the rest of its batch still folds. A missing lane that found
// nothing backs off before looking again.
func TestUsageFolderPoisonRunSkipTTLAndBackoff(t *testing.T) {
	e := newFoldEnv(t)
	var mu sync.Mutex
	var poison uuid.UUID
	poisonFails := true
	poisonCalls := 0
	clock := time.Now()
	f := store.NewUsageFolder(e.pool, store.UsageFolderOptions{
		Now: func() time.Time { mu.Lock(); defer mu.Unlock(); return clock },
		Materialize: func(ctx context.Context, ids []uuid.UUID) (int64, error) {
			mu.Lock()
			hit := containsID(ids, poison)
			if hit {
				poisonCalls++
			}
			fail := hit && poisonFails
			mu.Unlock()
			if fail {
				return 0, errors.New("injected poison failure")
			}
			return e.realMaterialize(ctx, ids)
		},
	})
	advance := func(d time.Duration) { mu.Lock(); clock = clock.Add(d); mu.Unlock() }
	calls := func() int { mu.Lock(); defer mu.Unlock(); return poisonCalls }

	poison = e.missingRun(t)
	good := e.missingRun(t)
	// Pass 1: the batch holding the poison fails, the per-run retry folds the good run.
	if _, err := f.Pass(context.Background()); err != nil {
		t.Fatalf("Pass 1: %v", err)
	}
	e.wantFresh(t, "good run in the failed batch", good)
	e.wantState(t, "poison", poison, false, false)
	afterFirst := calls()
	if afterFirst < 2 {
		t.Fatalf("poison calls after Pass 1 = %d, want the batch and the single retry", afterFirst)
	}

	// Pass 2 finds nothing (the poison is excluded), which starts the missing-lane backoff.
	if _, err := f.Pass(context.Background()); err != nil {
		t.Fatalf("Pass 2: %v", err)
	}
	if calls() != afterFirst {
		t.Fatalf("poison retried inside its TTL (%d calls, want %d)", calls(), afterFirst)
	}

	// Same clock: a new missing run is not looked at while the lane backs off.
	later := e.missingRun(t)
	if _, err := f.Pass(context.Background()); err != nil {
		t.Fatalf("Pass 3: %v", err)
	}
	e.wantState(t, "run created during backoff", later, false, false)

	// 6 minutes on: the 5 minute backoff is over (the 10 minute TTL is not).
	advance(6 * time.Minute)
	if _, err := f.Pass(context.Background()); err != nil {
		t.Fatalf("Pass 4: %v", err)
	}
	e.wantFresh(t, "run after backoff", later)
	if calls() != afterFirst {
		t.Fatalf("poison retried before its TTL expired (%d calls)", calls())
	}
	e.wantState(t, "poison before TTL", poison, false, false)

	// 12 minutes: the skip expired and the poison no longer fails; a later traversal folds it.
	advance(6 * time.Minute)
	mu.Lock()
	poisonFails = false
	mu.Unlock()
	if _, err := f.Pass(context.Background()); err != nil {
		t.Fatalf("Pass 5: %v", err)
	}
	e.wantFresh(t, "poison after TTL", poison)
}

// Kicks and Passes share one materializer lane: they never run Materialize concurrently.
func TestUsageFolderSerialisesMaterializerCalls(t *testing.T) {
	e := newFoldEnv(t)
	var active, maxActive atomic.Int32
	folded := make(chan struct{}, 8)
	f := store.NewUsageFolder(e.pool, store.UsageFolderOptions{
		Materialize: func(ctx context.Context, ids []uuid.UUID) (int64, error) {
			n := active.Add(1)
			for {
				m := maxActive.Load()
				if n <= m || maxActive.CompareAndSwap(m, n) {
					break
				}
			}
			time.Sleep(60 * time.Millisecond)
			defer active.Add(-1)
			return e.realMaterialize(ctx, ids)
		},
		OnFolded: func(uuid.UUID, int64, error) { folded <- struct{}{} },
	})
	a, b, c := e.missingRun(t), e.missingRun(t), e.missingRun(t)
	f.Notify(a, "completed")
	f.Notify(b, "completed")
	done := make(chan error, 1)
	go func() { _, err := f.Pass(context.Background()); done <- err }()
	if err := <-done; err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if err := f.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m := maxActive.Load(); m != 1 {
		t.Fatalf("max concurrent Materialize calls = %d, want 1", m)
	}
	for _, r := range []uuid.UUID{a, b, c} {
		e.wantFresh(t, "run after kicks and pass", r)
	}
}

// A kick folds its run on a real database through Notify, synchronised on onFolded.
func TestUsageFolderNotifyFoldsRealRun(t *testing.T) {
	e := newFoldEnv(t)
	got := make(chan foldCall, 1)
	f := store.NewUsageFolder(e.pool, store.UsageFolderOptions{
		OnFolded: func(id uuid.UUID, n int64, err error) { got <- foldCall{ids: []uuid.UUID{id}, n: n, err: err} },
	})
	r := e.missingRun(t)
	f.Notify(r, "failed")
	c := recvFold(t, got)
	if c.err != nil || c.n != 1 || c.ids[0] != r {
		t.Fatalf("onFolded = %+v, want run %s n=1", c, r)
	}
	e.wantFresh(t, "kicked run", r)
	if err := f.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
}
