package store_test

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4700 M2: the UsageFolder sweeper pass against a live database. Every test skips
// without UZI_TEST_DATABASE_URL (run via ./e2e/run-store-it.sh). The database is shared with
// other tests, so assertions are always about the test's OWN runs and pass counts are bounded
// by a count taken from the database, never by a guess.

// newFolderEnv is newFoldEnv plus a clean slate: it folds every OTHER eligible run that is
// missing or stale, so the lane assertions below only ever see the test's own runs. The shared
// test database (./e2e/run-store-it.sh runs the whole store package, and others, against one
// DB) holds terminal runs from ~200 earlier test files; without this, "Pass 2 finds nothing" or
// "my stuck run is among the first 100 stale" depend on that leftover state. Folding them
// (rather than flipping their state to fresh) keeps their state rows and fold rows correct for
// the tests that read the folds later. A run that cannot be folded (its row is locked) makes no
// progress, which ends the loop; bounded by the number of rounds as well.
func newFolderEnv(t *testing.T) *foldEnv {
	t.Helper()
	e := newFoldEnv(t)
	for round := 0; round < 10000; round++ {
		rows, err := e.pool.Query(e.ctx, `SELECT r.id FROM runs r
			WHERE r.status IN ('completed','failed','cancelled') AND r.kind <> 'chat' AND r.usage_refolded
			  AND NOT EXISTS (SELECT 1 FROM run_usage_fold_state s WHERE s.run_id = r.id AND NOT s.stale)
			ORDER BY r.id LIMIT 100`)
		if err != nil {
			t.Fatalf("list foreign candidates: %v", err)
		}
		var ids []uuid.UUID
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			ids = append(ids, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if len(ids) == 0 {
			return e
		}
		n, err := store.MaterializeRunUsageFolds(e.ctx, e.pool, ids)
		if err != nil {
			t.Fatalf("clean slate: materialize: %v", err)
		}
		if n == 0 {
			return e // nothing foldable is left (the rest is locked by someone else)
		}
	}
	return e
}

// lowRunID is a random uuid that sorts before every ordinary random one (first 4 bytes zero),
// so a stale-lane cursor starting at uuid.Nil meets it first.
func lowRunID() uuid.UUID {
	id := uuid.New()
	copy(id[:4], []byte{0, 0, 0, 0})
	return id
}

// lockRow runs sql (a SELECT ... FOR UPDATE of one row of the given run) in an open transaction
// and holds the lock until release is called. On a runs row a materializer skips the run (its
// step a0 takes FOR KEY SHARE SKIP LOCKED); on a state row it skips it in step b.
func (e *foldEnv) lockRow(t *testing.T, sql string, id uuid.UUID) (release func()) {
	t.Helper()
	tx, err := e.pool.Begin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	released := false
	release = func() {
		if !released {
			released = true
			_ = tx.Rollback(e.ctx)
		}
	}
	t.Cleanup(release)
	var got uuid.UUID
	if err := tx.QueryRow(e.ctx, sql, id).Scan(&got); err != nil {
		t.Fatalf("lock row: %v", err)
	}
	return release
}

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

// passBound is a generous bound on the Passes a backlog needs: each Pass folds at least one run
// of a non-empty lane, and the missing lane also spends one Pass per 2000-run window it crosses
// (a database with more eligible runs than one window takes several ticks per traversal).
func (e *foldEnv) passBound(t *testing.T) int {
	t.Helper()
	var eligible int
	err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM runs
		WHERE status IN ('completed','failed','cancelled') AND kind <> 'chat' AND usage_refolded`).Scan(&eligible)
	if err != nil {
		t.Fatalf("eligible count: %v", err)
	}
	return 2*e.pendingWork(t) + 2*(eligible/2000+2) + 5
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
func TestUsageFolderPassFoldsNonKickingTerminalWriteLiveDB(t *testing.T) {
	e := newFolderEnv(t)
	f := store.NewUsageFolder(e.pool, store.UsageFolderOptions{})

	// Missing lane: a running run that goes terminal by direct SQL.
	direct := e.run(t, "running")
	mustExec(e.ctx, t, e.pool, `UPDATE runs SET status = 'completed' WHERE id = $1`, direct)
	// Stale lane: a terminal run whose usage was written after (or without) any fold.
	stale := e.staleRun(t)

	bound := e.passBound(t)
	passUntil(t, f, bound, func() bool { return e.isFresh(t, direct) && e.isFresh(t, stale) })
	if rows := e.foldRows(t, e.pool, stale); len(rows) != 1 {
		t.Fatalf("stale run fold rows = %v, want exactly one model row", rows)
	}
}

// A Pass never folds what is not eligible: non-terminal and chat runs stay unfolded.
func TestUsageFolderPassSkipsIneligibleRunsLiveDB(t *testing.T) {
	e := newFolderEnv(t)
	f := store.NewUsageFolder(e.pool, store.UsageFolderOptions{})
	running := e.run(t, "running")
	chat := e.chatRun(t)
	ok := e.missingRun(t)

	passUntil(t, f, e.passBound(t), func() bool { return e.isFresh(t, ok) })
	e.wantState(t, "running", running, false, false)
	e.wantState(t, "chat", chat, false, false)
}

// More eligible runs than the per-tick limit: successive Passes fold all of them.
func TestUsageFolderCursorProgressAndWrapLiveDB(t *testing.T) {
	e := newFolderEnv(t)
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
	passUntil(t, f, e.passBound(t), all)

}

// A stale run the materializer skips (its state row is held locked) must not pin the stale
// cursor: with LaneLimit 1 the other stale runs are folded within a few Passes while it is
// locked (a cursor that never advanced would reselect the lowest id forever), and once the lock
// is released the cursor wraps back to it.
func TestUsageFolderStaleCursorAdvancesPastSkippedRunLiveDB(t *testing.T) {
	e := newFolderEnv(t)
	low := lowRunID()
	mustExec(e.ctx, t, e.pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, usage_refolded)
		 VALUES ($1, $2, $3, $4, 't', 'd', 'completed', true)`, low, e.userID, e.repoID, e.nextIID())
	if err := e.upsert(t, e.q, low, "s1", "m1", 0, 1000); err != nil {
		t.Fatal(err)
	}
	e.wantState(t, "low", low, true, true)
	others := []uuid.UUID{e.staleRun(t), e.staleRun(t), e.staleRun(t)}

	one := store.NewUsageFolder(e.pool, store.UsageFolderOptions{LaneLimit: 1})
	release := e.lockRow(t, `SELECT run_id FROM run_usage_fold_state WHERE run_id = $1 FOR UPDATE`, low)
	passUntil(t, one, e.passBound(t), func() bool {
		for _, r := range others {
			if !e.isFresh(t, r) {
				return false
			}
		}
		return true
	})
	e.wantState(t, "low while locked", low, true, true)
	release()
	passUntil(t, one, e.passBound(t), func() bool { return e.isFresh(t, low) })
}

// The missing lane likewise walks past a head-of-traversal run it cannot fold (its runs row is
// held locked, so the materializer skips it and never creates its state): the older missing runs
// are folded within a few Passes. A cursor that always reset would reselect the stuck newest run
// forever.
func TestUsageFolderMissingCursorAdvancesPastSkippedRunLiveDB(t *testing.T) {
	e := newFolderEnv(t)
	others := []uuid.UUID{e.missingRun(t), e.missingRun(t), e.missingRun(t)}
	stuck := e.missingRun(t) // newest: the first candidate of every traversal
	one := store.NewUsageFolder(e.pool, store.UsageFolderOptions{LaneLimit: 1})
	release := e.lockRow(t, `SELECT id FROM runs WHERE id = $1 FOR UPDATE`, stuck)
	passUntil(t, one, e.passBound(t), func() bool {
		for _, r := range others {
			if !e.isFresh(t, r) {
				return false
			}
		}
		return true
	})
	e.wantState(t, "stuck while locked", stuck, false, false)
	release()
	passUntil(t, one, e.passBound(t), func() bool { return e.isFresh(t, stuck) })
}

// One lane failing or running out of budget does not stop the other, in either order.
func TestUsageFolderLaneIsolationLiveDB(t *testing.T) {
	for _, blockStale := range []bool{true, false} {
		name := "missing lane blocked"
		if blockStale {
			name = "stale lane blocked"
		}
		t.Run(name, func(t *testing.T) {
			e := newFolderEnv(t)
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

			// Several Passes: the lane order alternates, so the blocked lane runs first in some of
			// them. The other lane's runs are folded within a bounded number of Passes (a database
			// with more eligible runs than one missing-lane window needs a few ticks to cross it).
			var others []uuid.UUID
			bound := e.passBound(t)
			for i := 0; i < bound; i++ {
				if i < 2 {
					if blockStale {
						others = append(others, e.missingRun(t))
					} else {
						others = append(others, e.staleRun(t))
					}
				}
				start := time.Now()
				_, err := f.Pass(context.Background())
				if elapsed := time.Since(start); elapsed > 3*time.Second {
					t.Fatalf("Pass %d took %v with one lane blocked", i, elapsed)
				}
				if err != nil {
					t.Fatalf("Pass %d err = %v, want nil: a lane running out of budget is routine, not an error", i, err)
				}
				if i >= 1 && e.isFresh(t, others[0]) && e.isFresh(t, others[1]) {
					break
				}
				if i == bound-1 {
					t.Fatalf("the other lane's runs are still not folded after %d Passes with one lane blocked", bound)
				}
			}
			if e.isFresh(t, stuck) {
				t.Fatal("the blocked run was folded")
			}
		})
	}
}

// The parent deadline bounds the whole Pass even when the per-lane budget is larger: both
// lanes block, and Pass still returns within the parent bound.
func TestUsageFolderParentDeadlineBoundsPassLiveDB(t *testing.T) {
	e := newFolderEnv(t)
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
	if err != nil {
		t.Fatalf("Pass err = %v, want nil: running out of the parent budget is routine, not an error", err)
	}
}

// A run that fails on its own is retried alone, skipped for the TTL, and picked up again once
// the (injected) clock passes it; the rest of its batch still folds. A missing lane that found
// nothing backs off before looking again.
func TestUsageFolderPoisonRunSkipTTLAndBackoffLiveDB(t *testing.T) {
	e := newFolderEnv(t)
	var mu sync.Mutex
	var poison uuid.UUID
	poisonFails := true
	poisonCalls := 0
	clock := time.Now()
	db := &recordingDB{UsageFolderDB: e.pool}
	f := store.NewUsageFolder(db, store.UsageFolderOptions{
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

	// The poison is excluded, so a whole traversal finds nothing, which starts the missing-lane
	// backoff. The traversal crosses several windows on a database with more than 2000 eligible
	// runs, so Pass until the missing lane stops querying (a Pass that begins only the stale
	// lane's transaction).
	bound := e.passBound(t)
	backedOff := false
	for i := 0; i < bound && !backedOff; i++ {
		before := db.begins.Load()
		if _, err := f.Pass(context.Background()); err != nil {
			t.Fatalf("settling Pass %d: %v", i, err)
		}
		backedOff = db.begins.Load()-before == 1
	}
	if !backedOff {
		t.Fatalf("the missing lane never backed off in %d Passes", bound)
	}
	if calls() != afterFirst {
		t.Fatalf("poison retried inside its TTL (%d calls, want %d)", calls(), afterFirst)
	}

	// Same clock: a new missing run is not looked at while the lane backs off.
	later := e.missingRun(t)
	if _, err := f.Pass(context.Background()); err != nil {
		t.Fatalf("Pass during backoff: %v", err)
	}
	e.wantState(t, "run created during backoff", later, false, false)

	// 6 minutes on: the 5 minute backoff is over (the 10 minute TTL is not).
	advance(6 * time.Minute)
	passUntil(t, f, bound, func() bool { return e.isFresh(t, later) })
	if calls() != afterFirst {
		t.Fatalf("poison retried before its TTL expired (%d calls)", calls())
	}
	e.wantState(t, "poison before TTL", poison, false, false)

	// 12 minutes: the skip expired and the poison no longer fails; a later traversal folds it.
	advance(6 * time.Minute)
	mu.Lock()
	poisonFails = false
	mu.Unlock()
	passUntil(t, f, bound, func() bool { return e.isFresh(t, poison) })
}

// Kicks and Passes share one materializer lane: they never run Materialize concurrently.
func TestUsageFolderSerialisesMaterializerCallsLiveDB(t *testing.T) {
	e := newFolderEnv(t)
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
func TestUsageFolderNotifyFoldsRealRunLiveDB(t *testing.T) {
	e := newFolderEnv(t)
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

// A batch that failed because the lane deadline expired is remembered and retried run by run on
// the next tick, so the run whose fold outlasts the lane budget cannot block its neighbours
// forever; that run, timing out alone, is skipped for the TTL.
func TestUsageFolderDeadlineBatchRetriedRunByRunLiveDB(t *testing.T) {
	e := newFolderEnv(t)
	var slow uuid.UUID
	var slowCalls atomic.Int32
	f := store.NewUsageFolder(e.pool, store.UsageFolderOptions{
		ParentTimeout: 10 * time.Second,
		LaneTimeout:   600 * time.Millisecond,
		Materialize: func(ctx context.Context, ids []uuid.UUID) (int64, error) {
			if containsID(ids, slow) {
				slowCalls.Add(1)
				<-ctx.Done()
				return 0, ctx.Err()
			}
			return e.realMaterialize(ctx, ids)
		},
	})
	n1 := e.missingRun(t)
	slow = e.missingRun(t)
	n2, n3 := e.missingRun(t), e.missingRun(t)

	passUntil(t, f, 6, func() bool { return e.isFresh(t, n1) && e.isFresh(t, n2) && e.isFresh(t, n3) })
	if e.isFresh(t, slow) {
		t.Fatal("the slow run was folded")
	}
	// It was skipped for the TTL: further Passes do not attempt it again.
	calls := slowCalls.Load()
	for i := 0; i < 3; i++ {
		if _, err := f.Pass(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if got := slowCalls.Load(); got != calls {
		t.Fatalf("slow run attempted %d more times after it was skipped", got-calls)
	}
}

// A lane that has pending deadline retries AND new work attempts at most LaneLimit distinct runs
// in one Pass: the retries are charged against the same per-tick allowance as new ids.
func TestUsageFolderRetriesChargeLaneAllowanceLiveDB(t *testing.T) {
	e := newFolderEnv(t)
	const limit = 3
	var (
		mu       sync.Mutex
		attempts []uuid.UUID
		blocking atomic.Bool
	)
	f := store.NewUsageFolder(e.pool, store.UsageFolderOptions{
		ParentTimeout: 10 * time.Second,
		LaneTimeout:   500 * time.Millisecond,
		LaneLimit:     limit,
		Materialize: func(ctx context.Context, ids []uuid.UUID) (int64, error) {
			mu.Lock()
			attempts = append(attempts, ids...)
			mu.Unlock()
			if blocking.Load() && len(ids) > 1 {
				<-ctx.Done()
				return 0, ctx.Err()
			}
			return e.realMaterialize(ctx, ids)
		},
	})

	// Pass 1: a batch of two missing runs (short of LIMIT, so the lane's cursor resets) outlasts
	// the lane budget; its ids become the lane's pending retries.
	old := []uuid.UUID{e.missingRun(t), e.missingRun(t)}
	blocking.Store(true)
	if _, err := f.Pass(context.Background()); err != nil {
		t.Fatalf("Pass 1: %v", err)
	}
	blocking.Store(false)
	for _, r := range old {
		if e.isFresh(t, r) {
			t.Fatal("Pass 1 folded a run of the batch that was meant to time out")
		}
	}

	// Pass 2: two newer missing runs exist, but the two retries used two of the lane's three
	// attempts, so only the newest new run may be taken (fresh[1]; the lane walks newest first).
	fresh := []uuid.UUID{e.missingRun(t), e.missingRun(t)}
	mu.Lock()
	attempts = nil
	mu.Unlock()
	if _, err := f.Pass(context.Background()); err != nil {
		t.Fatalf("Pass 2: %v", err)
	}
	mu.Lock()
	distinct := map[uuid.UUID]bool{}
	for _, id := range attempts {
		distinct[id] = true
	}
	mu.Unlock()
	if len(distinct) > limit {
		t.Fatalf("one Pass attempted %d distinct runs, want at most LaneLimit=%d", len(distinct), limit)
	}
	for _, r := range old {
		e.wantFresh(t, "retried run", r)
	}
	if !e.isFresh(t, fresh[1]) {
		t.Fatal("the lane did not use the allowance the retries left over")
	}
	if e.isFresh(t, fresh[0]) {
		t.Fatal("the lane took a second new run: retries were not charged against its allowance")
	}

	// Pass 3: the rest of the new work follows on the next tick.
	passUntil(t, f, 4, func() bool { return e.isFresh(t, fresh[0]) && e.isFresh(t, fresh[1]) })
}

// Both lanes run in a read-only transaction with a server-side statement_timeout of one lane
// budget, and lane order alternates between Passes.
type recordingDB struct {
	store.UsageFolderDB
	mu     sync.Mutex
	execs  []string
	ro     []bool
	failTx atomic.Bool
	begins atomic.Int32
}

type recordingTx struct {
	pgx.Tx
	db *recordingDB
}

func (d *recordingDB) BeginTx(ctx context.Context, o pgx.TxOptions) (pgx.Tx, error) {
	d.begins.Add(1)
	if d.failTx.Load() {
		return nil, errors.New("injected begin failure")
	}
	tx, err := d.UsageFolderDB.BeginTx(ctx, o)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	d.ro = append(d.ro, o.AccessMode == pgx.ReadOnly)
	d.mu.Unlock()
	return &recordingTx{Tx: tx, db: d}, nil
}

func (t *recordingTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	t.db.mu.Lock()
	t.db.execs = append(t.db.execs, sql)
	t.db.mu.Unlock()
	return t.Tx.Exec(ctx, sql, args...)
}

func TestUsageFolderLaneQueriesAreBoundedAndLanesAlternateLiveDB(t *testing.T) {
	e := newFolderEnv(t)
	db := &recordingDB{UsageFolderDB: e.pool}
	var mu sync.Mutex
	var order []string
	// The clock jumps past the missing-lane backoff on every read, so an empty traversal never
	// suppresses the next Pass's missing query.
	var clock atomic.Int64
	f := store.NewUsageFolder(db, store.UsageFolderOptions{
		Materialize: e.realMaterialize, // only the lane transactions go through recordingDB
		LaneTimeout: 1500 * time.Millisecond,
		Now:         func() time.Time { return time.Unix(0, 0).Add(time.Duration(clock.Add(1)) * time.Hour) },
		OnLane:      func(l string) { mu.Lock(); order = append(order, l); mu.Unlock() },
	})
	for i := 0; i < 4; i++ {
		if _, err := f.Pass(context.Background()); err != nil {
			t.Fatalf("Pass %d: %v", i, err)
		}
	}
	mu.Lock()
	gotOrder := strings.Join(order, ",")
	mu.Unlock()
	if want := "stale,missing,missing,stale,stale,missing,missing,stale"; gotOrder != want {
		t.Fatalf("lane order = %s, want %s", gotOrder, want)
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	if len(db.execs) != 8 {
		t.Fatalf("lane queries ran %d SET statements, want 8 (one per lane per Pass): %v", len(db.execs), db.execs)
	}
	for _, x := range db.execs {
		if x != "SET LOCAL statement_timeout = 1500" {
			t.Fatalf("statement bound = %q, want the 1500ms lane budget", x)
		}
	}
	for _, ro := range db.ro {
		if !ro {
			t.Fatal("a lane query ran in a read-write transaction")
		}
	}
}

// A failing lane query must not spin: the missing lane backs off MissingBackoff, and the stale
// lane keeps its cursor and simply tries again on the next tick. Real failures are returned.
func TestUsageFolderLaneQueryErrorBacksOffMissingLaneLiveDB(t *testing.T) {
	e := newFolderEnv(t)
	db := &recordingDB{UsageFolderDB: e.pool}
	var cmu sync.Mutex
	clock := time.Now()
	f := store.NewUsageFolder(db, store.UsageFolderOptions{
		Materialize: e.realMaterialize,
		Now:         func() time.Time { cmu.Lock(); defer cmu.Unlock(); return clock },
	})
	db.failTx.Store(true)
	if _, err := f.Pass(context.Background()); err == nil {
		t.Fatal("Pass with failing lane queries returned nil, want the query error")
	}
	if got := db.begins.Load(); got != 2 {
		t.Fatalf("begins after the failing Pass = %d, want 2 (stale + missing)", got)
	}
	// Immediately again: the missing lane is backing off, the stale lane tries once more.
	if _, err := f.Pass(context.Background()); err == nil {
		t.Fatal("second failing Pass returned nil")
	}
	if got := db.begins.Load(); got != 3 {
		t.Fatalf("begins after the second Pass = %d, want 3 (stale only: the missing lane backs off)", got)
	}
	// After the backoff the missing lane is retried.
	cmu.Lock()
	clock = clock.Add(6 * time.Minute)
	cmu.Unlock()
	if _, err := f.Pass(context.Background()); err == nil {
		t.Fatal("third failing Pass returned nil")
	}
	if got := db.begins.Load(); got != 5 {
		t.Fatalf("begins after the backoff = %d, want 5", got)
	}
	// And the folder recovers once the database does.
	db.failTx.Store(false)
	r := e.missingRun(t)
	cmu.Lock()
	clock = clock.Add(6 * time.Minute)
	cmu.Unlock()
	passUntil(t, f, 6, func() bool { return e.isFresh(t, r) })
}

// A missing-lane query that fails after the run-by-run retry of a remembered deadline batch has
// consumed more than half of the lane budget says nothing about the query (the retry ate its
// time), so it must NOT start the MissingBackoff: the next Pass queries again at once.
func TestUsageFolderLaneQueryErrorAfterRetryDoesNotBackOffLiveDB(t *testing.T) {
	e := newFolderEnv(t)
	db := &recordingDB{UsageFolderDB: e.pool}
	var batchPhase atomic.Bool // true: a multi-run batch holding the marked runs hangs until the lane deadline
	var retryPhase atomic.Bool // true: the first single-run call takes 400ms (more than half of 600ms)
	var slept atomic.Bool
	var a, b uuid.UUID
	f := store.NewUsageFolder(db, store.UsageFolderOptions{
		ParentTimeout: 10 * time.Second,
		LaneTimeout:   600 * time.Millisecond,
		// 3, with only two missing runs: the retried batch of two leaves one attempt of the
		// per-tick allowance, so the lane still reaches its query after the retry.
		LaneLimit: 3,
		Now:       func() time.Time { return time.Unix(1_700_000_000, 0) }, // frozen: never passes the backoff
		Materialize: func(ctx context.Context, ids []uuid.UUID) (int64, error) {
			if batchPhase.Load() && len(ids) > 1 && (containsID(ids, a) || containsID(ids, b)) {
				<-ctx.Done()
				return 0, ctx.Err()
			}
			if retryPhase.Load() && len(ids) == 1 && slept.CompareAndSwap(false, true) {
				select {
				case <-time.After(400 * time.Millisecond):
				case <-ctx.Done():
				}
			}
			return e.realMaterialize(ctx, ids)
		},
	})
	a = e.missingRun(t)
	b = e.missingRun(t)

	// Pass 1: the batch of the two newest missing runs hangs to the lane deadline and is remembered.
	batchPhase.Store(true)
	if _, err := f.Pass(context.Background()); err != nil {
		t.Fatalf("Pass 1: %v", err)
	}
	batchPhase.Store(false)

	// Pass 2: the missing lane runs first (lanes alternate); its retry takes 400ms of the 600ms
	// budget and succeeds, then the lane query fails with less than half a budget left.
	retryPhase.Store(true)
	db.failTx.Store(true)
	before2 := db.begins.Load()
	if _, err := f.Pass(context.Background()); err == nil {
		t.Fatal("Pass 2 with failing lane queries returned nil, want the query error")
	}
	if !slept.Load() {
		t.Fatal("the run-by-run retry never ran: the deadline batch was not remembered")
	}
	// Non-vacuity: the missing lane must actually have reached its (failing) query with the short
	// budget left after the retry; if the retry overran the lane deadline instead, Pass 3 below
	// would see 2 begins on fixed and reverted code alike.
	if got := db.begins.Load() - before2; got != 2 {
		t.Fatalf("lane queries begun by Pass 2 = %d, want 2 (the missing lane did not reach its query after the retry)", got)
	}
	retryPhase.Store(false)

	// Pass 3, same frozen clock: no backoff was started, so both lanes query again.
	before := db.begins.Load()
	if _, err := f.Pass(context.Background()); err == nil {
		t.Fatal("Pass 3 with failing lane queries returned nil")
	}
	if got := db.begins.Load() - before; got != 2 {
		t.Fatalf("lane queries begun by Pass 3 = %d, want 2 (stale + missing: a query failure after the retry ate the budget must not back off)", got)
	}
}

// What this proves: with sequential scans disabled and the generic plan forced, the missing-lane
// query's window over runs is planned through idx_runs_usage_fold_missing (migration 00550), i.e.
// the index exists and the query's predicate implies the partial index. It does NOT prove the
// default planner picks that plan: at this fixture's scale a sequential scan is legitimately
// cheaper. The window size MUST stay a SQL literal (asserted below as text): as a parameter the
// generic plan flips to a hash anti join that seq-scans run_usage_fold_state at steady state
// (a DBA measurement on a 1M-run seed, not asserted by this test).
func TestUsageFolderMissingQueryPlanLiveDB(t *testing.T) {
	e := newFolderEnv(t)
	const rows = 6000
	foldIIDMu.Lock()
	base := foldIIDNext
	foldIIDNext += rows + 1
	foldIIDMu.Unlock()

	tx, err := e.pool.Begin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }() // the seeded runs and the ANALYZE vanish
	if _, err := tx.Exec(e.ctx, `INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, usage_refolded, created_at)
		SELECT gen_random_uuid(), $1, $2, $3 + g, 't', 'd',
		       CASE WHEN g % 4 = 0 THEN 'running' ELSE 'completed' END, true, now() - g * interval '1 second'
		FROM generate_series(1, $4::int) g`, e.userID, e.repoID, base, rows); err != nil {
		t.Fatalf("seed: %v", err)
	}
	mustExecTx(e.ctx, t, tx, `ANALYZE runs`)
	mustExecTx(e.ctx, t, tx, `SET LOCAL plan_cache_mode = force_generic_plan`)
	mustExecTx(e.ctx, t, tx, `SET LOCAL enable_seqscan = off`)
	// PREPARE is session state and survives the rollback on a pooled connection: use a name unique
	// to this run and deallocate it before the rollback (defers run last-in first-out).
	stmt := "fold_missing_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	mustExecTx(e.ctx, t, tx, "PREPARE "+stmt+" AS "+store.ListMissingRunUsageFoldRunsSQL)
	defer func() { _, _ = tx.Exec(context.Background(), "DEALLOCATE "+stmt) }()
	prows, err := tx.Query(e.ctx, `EXPLAIN EXECUTE `+stmt+`('9999-12-31 00:00:00+00', 'ffffffff-ffff-ffff-ffff-ffffffffffff', '{}', 100)`, pgx.QueryExecModeSimpleProtocol)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer prows.Close()
	var sb strings.Builder
	for prows.Next() {
		var line string
		if err := prows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		sb.WriteString(line + "\n")
	}
	if err := prows.Err(); err != nil {
		t.Fatal(err)
	}
	plan := sb.String()
	if !strings.Contains(plan, "idx_runs_usage_fold_missing") {
		t.Errorf("missing-lane query does not use idx_runs_usage_fold_missing:\n%s", plan)
	}
	if regexp.MustCompile(`Seq Scan on runs(\s|$)`).MatchString(plan) {
		t.Errorf("missing-lane query scans runs sequentially:\n%s", plan)
	}
	if !strings.Contains(store.ListMissingRunUsageFoldRunsSQL, "LIMIT 2000") {
		t.Error("the window size is no longer the literal LIMIT 2000")
	}
}
