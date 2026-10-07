package store_test

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// reviewAuthorEnv opens the throwaway database and seeds one repo (the queue, verdict and
// ledger tables all reference repos). Skipped unless UZI_TEST_DATABASE_URL is set; a package
// that prints ok with PASS=0 is INVALID, not green.
func reviewAuthorEnv(t *testing.T) (context.Context, *pgxpool.Pool, *store.Queries, uuid.UUID) {
	t.Helper()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	user, conn, repo := uuid.New(), uuid.New(), uuid.New()
	exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, user, fmt.Sprintf("ra-%s@e2e", user))
	exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, conn, user, []byte{0x1})
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, 1, 'g/ra', 'https://forge.e2e/g/ra', 'main', true)`, repo, conn)
	return ctx, pool, store.New(pool), repo
}

// locked runs fn in its own transaction after taking the queue lock, as the production
// workersvc.Service.MutateReviewAuthorQueue does.
func locked(ctx context.Context, t *testing.T, pool *pgxpool.Pool, repo uuid.UUID, ref string, fn func(q *store.Queries)) {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := store.New(tx)
	if err := q.LockReviewAuthorQueue(ctx, store.LockReviewAuthorQueueParams{RepoID: repo, Ref: ref}); err != nil {
		t.Fatal(err)
	}
	fn(q)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func queueRows(ctx context.Context, t *testing.T, q *store.Queries, repo uuid.UUID, ref string) []store.ListReviewAuthorQueueRow {
	t.Helper()
	rows, err := q.ListReviewAuthorQueue(ctx, store.ListReviewAuthorQueueParams{RepoID: repo, Ref: ref})
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func authorIDs(rows []store.ListReviewAuthorQueueRow) []int64 {
	out := []int64{}
	for _, r := range rows {
		out = append(out, r.ForgeUserID)
	}
	return out
}

func maxSeq(rows []store.ListReviewAuthorQueueRow) int64 {
	var m int64
	for _, r := range rows {
		m = max(m, r.QueueSeq)
	}
	return m
}

func admit(ctx context.Context, t *testing.T, pool *pgxpool.Pool, repo uuid.UUID, ref string, ids ...int64) {
	t.Helper()
	locked(ctx, t, pool, repo, ref, func(q *store.Queries) {
		if err := q.AdmitReviewAuthors(ctx, store.AdmitReviewAuthorsParams{RepoID: repo, Ref: ref, ForgeUserIds: ids}); err != nil {
			t.Fatal(err)
		}
	})
}

func prune(ctx context.Context, t *testing.T, pool *pgxpool.Pool, repo uuid.UUID, ref string, observedMax int64, keep ...int64) int64 {
	t.Helper()
	var n int64
	locked(ctx, t, pool, repo, ref, func(q *store.Queries) {
		var err error
		if keep == nil {
			keep = []int64{}
		}
		n, err = q.PruneReviewAuthorQueue(ctx, store.PruneReviewAuthorQueueParams{RepoID: repo, Ref: ref, ObservedMaxSeq: observedMax, KeepIds: keep})
		if err != nil {
			t.Fatal(err)
		}
	})
	return n
}

// TestReviewAuthorQueueOrderingLiveDB pins the ordering contract of the queue statements: the
// authors given to AdmitReviewAuthors and RequeueReviewAuthors are numbered in ARRAY order from
// one sequence, so the queue order is exactly the order the caller asked for, whatever the
// numeric ids are.
func TestReviewAuthorQueueOrderingLiveDB(t *testing.T) {
	ctx, pool, q, repo := reviewAuthorEnv(t)
	ref := "agent/issue-ordering"

	// 60 ids in an order that is neither ascending nor descending.
	var want []int64
	for i := int64(0); i < 60; i++ {
		want = append(want, (i*37)%61+1000)
	}
	admit(ctx, t, pool, repo, ref, want...)
	rows := queueRows(ctx, t, q, repo, ref)
	if got := authorIDs(rows); !reflect.DeepEqual(got, want) {
		t.Fatalf("admission order = %v, want array order %v", got, want)
	}
	for i := 1; i < len(rows); i++ {
		if rows[i].QueueSeq <= rows[i-1].QueueSeq {
			t.Fatalf("queue_seq not strictly increasing at %d: %v", i, rows)
		}
	}

	// An already-queued author keeps its place; a new one goes to the back.
	before := queueRows(ctx, t, q, repo, ref)
	admit(ctx, t, pool, repo, ref, want[10], 5000)
	after := queueRows(ctx, t, q, repo, ref)
	if len(after) != len(before)+1 || after[len(after)-1].ForgeUserID != 5000 {
		t.Fatalf("re-admit changed the queue: %v", authorIDs(after))
	}
	if !reflect.DeepEqual(authorIDs(after[:len(before)]), authorIDs(before)) {
		t.Fatalf("an already-queued author moved: %v vs %v", authorIDs(after), authorIDs(before))
	}

	// Requeue moves the given authors to the back in the given order and skips one that is gone.
	locked(ctx, t, pool, repo, ref, func(tq *store.Queries) {
		if err := tq.RequeueReviewAuthors(ctx, store.RequeueReviewAuthorsParams{RepoID: repo, Ref: ref, ForgeUserIds: []int64{want[3], 999999, want[0]}}); err != nil {
			t.Fatal(err)
		}
	})
	final := queueRows(ctx, t, q, repo, ref)
	tail := authorIDs(final)[len(final)-2:]
	if !reflect.DeepEqual(tail, []int64{want[3], want[0]}) {
		t.Fatalf("requeue tail = %v, want [%d %d]", tail, want[3], want[0])
	}
	if len(final) != len(after) {
		t.Fatalf("requeue of a missing author changed the row count: %d -> %d", len(after), len(final))
	}
	var attempted int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM mr_review_author_queue WHERE repo_id=$1 AND ref=$2 AND last_attempt_at IS NOT NULL`, repo, ref).Scan(&attempted); err != nil {
		t.Fatal(err)
	}
	if attempted != 2 {
		t.Fatalf("last_attempt_at stamped on %d rows, want exactly the 2 requeued", attempted)
	}
}

// TestReviewAuthorQueueStalePruneNeverDeletesReadmittedLiveDB is the interleaving the sequence
// exists for. A pruner observes the queue's max queue_seq; another transaction then removes
// every row; a new author is admitted; the pruner finally runs with its STALE observed max.
// The new row must survive. With a max(queue_seq)+1 scheme the re-admitted author restarts at 1,
// which is <= the stale observed max and not in the pruner's keep set, so the pruner would
// delete a waiting author and reset its position. With a never-reset sequence the new row's
// queue_seq is above any value observed earlier.
func TestReviewAuthorQueueStalePruneNeverDeletesReadmittedLiveDB(t *testing.T) {
	ctx, pool, q, repo := reviewAuthorEnv(t)
	ref := "agent/issue-stale-prune"

	// The prune guard needs values monotonic in lock order: CACHE 1 (no per-session block of
	// preallocated values that a later lock holder could draw below an earlier holder's) and NO
	// CYCLE (a wrapped value never repeats). Pin both settings from the catalog.
	var cacheSize int64
	var cycle bool
	if err := pool.QueryRow(ctx, `SELECT cache_size, cycle FROM pg_sequences WHERE sequencename = 'mr_review_author_queue_seq'`).Scan(&cacheSize, &cycle); err != nil {
		t.Fatalf("read mr_review_author_queue_seq settings: %v", err)
	}
	if cacheSize != 1 || cycle {
		t.Fatalf("mr_review_author_queue_seq cache_size=%d cycle=%v, want 1 and false", cacheSize, cycle)
	}

	// Draw the sequence up so the observed max is well above 1.
	admit(ctx, t, pool, repo, ref, 11, 12, 13, 14, 15)
	observed := maxSeq(queueRows(ctx, t, q, repo, ref)) // the stale pruner's observation

	// Another locked transaction removes every row (an eviction, or a prune with an empty keep set).
	if n := prune(ctx, t, pool, repo, ref, observed); n != 5 {
		t.Fatalf("clearing prune deleted %d rows, want 5", n)
	}
	if rows := queueRows(ctx, t, q, repo, ref); len(rows) != 0 {
		t.Fatalf("queue not empty after the clearing prune: %v", rows)
	}

	// A new author is admitted into the emptied queue.
	admit(ctx, t, pool, repo, ref, 77)
	readmitted := queueRows(ctx, t, q, repo, ref)
	if len(readmitted) != 1 || readmitted[0].ForgeUserID != 77 {
		t.Fatalf("re-admission = %v, want author 77", readmitted)
	}
	if readmitted[0].QueueSeq <= observed {
		t.Errorf("re-admitted queue_seq = %d, want > the earlier observed max %d: sequence values must never repeat", readmitted[0].QueueSeq, observed)
	}

	// The stale pruner now runs with its old observation and a keep set that lacks author 77.
	if n := prune(ctx, t, pool, repo, ref, observed, 11); n != 0 {
		t.Fatalf("the stale prune deleted %d rows, want 0", n)
	}
	after := queueRows(ctx, t, q, repo, ref)
	if len(after) != 1 || after[0].ForgeUserID != 77 || after[0].QueueSeq != readmitted[0].QueueSeq {
		t.Fatalf("the re-admitted author was removed or moved by a stale prune: %v", after)
	}
}

// TestReviewAuthorQueueConcurrentMutationsLiveDB covers the advisory lock: mutations of one
// (repo, ref) queue are serialized (a second transaction blocks on the lock until the first
// commits and then sees its rows), while a different ref is not blocked.
func TestReviewAuthorQueueConcurrentMutationsLiveDB(t *testing.T) {
	ctx, pool, q, repo := reviewAuthorEnv(t)

	waiters := func() int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND NOT granted AND classid = $1`, int64(store.ReviewAuthorQueueLockClass)).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	waitForWaiter := func() {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for waiters() == 0 {
			if time.Now().After(deadline) {
				t.Fatal("no transaction ever waited on the review-author queue lock")
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	t.Run("admission vs requeue", func(t *testing.T) {
		ref := "agent/issue-admit-requeue"
		admit(ctx, t, pool, repo, ref, 1, 2)

		tx1, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx1.Rollback(ctx) }()
		q1 := store.New(tx1)
		if err := q1.LockReviewAuthorQueue(ctx, store.LockReviewAuthorQueueParams{RepoID: repo, Ref: ref}); err != nil {
			t.Fatal(err)
		}
		if err := q1.AdmitReviewAuthors(ctx, store.AdmitReviewAuthorsParams{RepoID: repo, Ref: ref, ForgeUserIds: []int64{3, 4}}); err != nil {
			t.Fatal(err)
		}

		done := make(chan struct{})
		go func() {
			defer close(done)
			locked(ctx, t, pool, repo, ref, func(q2 *store.Queries) {
				// Runs only after tx1 committed: it must see 3 and 4 and move author 1 behind them.
				if err := q2.RequeueReviewAuthors(ctx, store.RequeueReviewAuthorsParams{RepoID: repo, Ref: ref, ForgeUserIds: []int64{1, 4}}); err != nil {
					t.Error(err)
				}
			})
		}()
		waitForWaiter()
		select {
		case <-done:
			t.Fatal("the requeue finished while the admission still held the lock")
		default:
		}
		if err := tx1.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		<-done

		rows := queueRows(ctx, t, q, repo, ref)
		if got := authorIDs(rows); !slices.Equal(got, []int64{2, 3, 1, 4}) {
			t.Fatalf("queue after the interleave = %v, want [2 3 1 4]", got)
		}
		seen := map[int64]bool{}
		for i, r := range rows {
			if seen[r.QueueSeq] || (i > 0 && r.QueueSeq <= rows[i-1].QueueSeq) {
				t.Fatalf("queue_seq not unique and strictly increasing: %v", rows)
			}
			seen[r.QueueSeq] = true
		}
	})

	t.Run("admission vs stale prune", func(t *testing.T) {
		ref := "agent/issue-admit-prune"
		admit(ctx, t, pool, repo, ref, 21, 22)
		observed := maxSeq(queueRows(ctx, t, q, repo, ref))

		tx1, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx1.Rollback(ctx) }()
		q1 := store.New(tx1)
		if err := q1.LockReviewAuthorQueue(ctx, store.LockReviewAuthorQueueParams{RepoID: repo, Ref: ref}); err != nil {
			t.Fatal(err)
		}
		if err := q1.AdmitReviewAuthors(ctx, store.AdmitReviewAuthorsParams{RepoID: repo, Ref: ref, ForgeUserIds: []int64{23}}); err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			prune(ctx, t, pool, repo, ref, observed) // the pruner observed before author 23 existed
		}()
		waitForWaiter()
		if err := tx1.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		<-done
		if got := authorIDs(queueRows(ctx, t, q, repo, ref)); !slices.Equal(got, []int64{23}) {
			t.Fatalf("queue = %v, want only the author admitted after the observation", got)
		}
	})

	t.Run("different refs are not serialized", func(t *testing.T) {
		tx1, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx1.Rollback(ctx) }()
		if err := store.New(tx1).LockReviewAuthorQueue(ctx, store.LockReviewAuthorQueueParams{RepoID: repo, Ref: "agent/issue-ref-a"}); err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			admit(ctx, t, pool, repo, "agent/issue-ref-b", 31)
		}()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("a mutation of a different ref blocked behind another ref's lock")
		}
		if waiters() != 0 {
			t.Fatal("a waiter exists on the queue lock though the two refs differ")
		}
	})
}

// TestReviewAuthorVerdictLiveDB covers the not-eligible verdict cache: a fresh verdict is
// listed, an older one is not, an upsert only moves the time forward, and expired rows are
// deleted per repo.
func TestReviewAuthorVerdictLiveDB(t *testing.T) {
	ctx, _, q, repo := reviewAuthorEnv(t)
	at := time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC)
	ts := func(d time.Duration) pgtype.Timestamptz { return pgtype.Timestamptz{Time: at.Add(d), Valid: true} }
	put := func(id int64, d time.Duration) {
		t.Helper()
		if err := q.UpsertReviewAuthorVerdict(ctx, store.UpsertReviewAuthorVerdictParams{RepoID: repo, ForgeUserID: id, NotEligibleAt: ts(d)}); err != nil {
			t.Fatal(err)
		}
	}
	fresh := func(since time.Duration) []int64 {
		t.Helper()
		ids, err := q.ListFreshNotEligibleAuthors(ctx, store.ListFreshNotEligibleAuthorsParams{RepoID: repo, Since: ts(since)})
		if err != nil {
			t.Fatal(err)
		}
		slices.Sort(ids)
		return ids
	}
	put(41, 0)
	put(42, -7*time.Hour)
	if got := fresh(-6 * time.Hour); !slices.Equal(got, []int64{41}) {
		t.Fatalf("fresh within the 6h window = %v, want [41]", got)
	}
	put(42, 0)
	if got := fresh(-6 * time.Hour); !slices.Equal(got, []int64{41, 42}) {
		t.Fatalf("after refreshing 42 = %v, want [41 42]", got)
	}
	put(41, -time.Hour) // an older write must not rewind the verdict
	if got := fresh(-30 * time.Minute); !slices.Equal(got, []int64{41, 42}) {
		t.Fatalf("an older upsert rewound the verdict: %v", got)
	}
	if n, err := q.DeleteExpiredReviewAuthorVerdicts(ctx, store.DeleteExpiredReviewAuthorVerdictsParams{RepoID: repo, Before: ts(time.Second)}); err != nil || n != 2 {
		t.Fatalf("delete expired = %d, %v, want 2", n, err)
	}
	if got := fresh(-100 * time.Hour); len(got) != 0 {
		t.Fatalf("verdicts survived expiry: %v", got)
	}
}

// TestReviewAuthorStaleEvictionLiveDB covers the housekeeping statements: refs holding old rows
// are listed per repo and DeleteStaleReviewAuthorQueue only removes rows untouched since the cutoff.
func TestReviewAuthorStaleEvictionLiveDB(t *testing.T) {
	ctx, pool, q, repo := reviewAuthorEnv(t)
	ref := "agent/issue-evict"
	admit(ctx, t, pool, repo, ref, 51, 52)
	if _, err := pool.Exec(ctx, `UPDATE mr_review_author_queue SET admitted_at = now() - interval '8 days' WHERE repo_id=$1 AND ref=$2 AND forge_user_id=51`, repo, ref); err != nil {
		t.Fatal(err)
	}
	// 52 was attempted recently: GREATEST(admitted_at, last_attempt_at) keeps it.
	if _, err := pool.Exec(ctx, `UPDATE mr_review_author_queue SET admitted_at = now() - interval '9 days', last_attempt_at = now() WHERE repo_id=$1 AND ref=$2 AND forge_user_id=52`, repo, ref); err != nil {
		t.Fatal(err)
	}
	before := pgtype.Timestamptz{Time: time.Now().Add(-7 * 24 * time.Hour), Valid: true}
	refs, err := q.ListStaleReviewAuthorQueueRefs(ctx, store.ListStaleReviewAuthorQueueRefsParams{RepoID: repo, Before: before})
	if err != nil || !slices.Equal(refs, []string{ref}) {
		t.Fatalf("stale refs = %v, %v, want [%s]", refs, err, ref)
	}
	locked(ctx, t, pool, repo, ref, func(tq *store.Queries) {
		n, err := tq.DeleteStaleReviewAuthorQueue(ctx, store.DeleteStaleReviewAuthorQueueParams{RepoID: repo, Ref: ref, Before: before})
		if err != nil || n != 1 {
			t.Fatalf("stale delete = %d, %v, want 1", n, err)
		}
	})
	if got := authorIDs(queueRows(ctx, t, q, repo, ref)); !slices.Equal(got, []int64{52}) {
		t.Fatalf("queue after eviction = %v, want [52]", got)
	}
}

// TestMRReworkPendingUnknownIDsLiveDB pins the pending-set merge shared by UpsertMRReworkLedger
// and the atomic on-demand create: only ids above the high-water mark the row had BEFORE the
// update are added (so a stale writer cannot resurrect an id a faster one consumed), removals
// apply, and at most the oldest 10000 survive within the column's CHECK.
func TestMRReworkPendingUnknownIDsLiveDB(t *testing.T) {
	ctx, pool, q, repo := reviewAuthorEnv(t)
	user := uuid.New()
	if err := pool.QueryRow(ctx, `SELECT user_id FROM forge_connections WHERE id = (SELECT connection_id FROM repos WHERE id = $1)`, repo).Scan(&user); err != nil {
		t.Fatal(err)
	}
	pending := func(ref string) []int64 {
		t.Helper()
		led, err := q.GetMRReworkLedger(ctx, store.GetMRReworkLedgerParams{RepoID: repo, Ref: ref})
		if err != nil {
			t.Fatal(err)
		}
		return led.PendingUnknownIds
	}
	upsert := func(ref string, hw int64, add, remove []int64) {
		t.Helper()
		if err := q.UpsertMRReworkLedger(ctx, store.UpsertMRReworkLedgerParams{RepoID: repo, Ref: ref, HighWater: hw, PendingAdd: add, PendingRemove: remove}); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("insert and update paths", func(t *testing.T) {
		ref := "agent/issue-pending-1"
		upsert(ref, 9, []int64{5, 9}, nil) // INSERT: existing is empty, the filter is against 0
		if got := pending(ref); !slices.Equal(got, []int64{5, 9}) {
			t.Fatalf("after insert = %v, want [5 9]", got)
		}
		// 7 is at/below the prior mark (9) and is dropped; 12 is above and stays; 5 is consumed.
		upsert(ref, 12, []int64{7, 12}, []int64{5})
		if got := pending(ref); !slices.Equal(got, []int64{9, 12}) {
			t.Fatalf("after update = %v, want [9 12]", got)
		}
		// NULL arrays (a caller that sets neither list) leave the set alone.
		if err := q.UpsertMRReworkLedger(ctx, store.UpsertMRReworkLedgerParams{RepoID: repo, Ref: ref, HighWater: 13}); err != nil {
			t.Fatal(err)
		}
		if got := pending(ref); !slices.Equal(got, []int64{9, 12}) {
			t.Fatalf("nil lists changed the set: %v", got)
		}
	})

	t.Run("a stale add cannot resurrect a consumed id", func(t *testing.T) {
		ref := "agent/issue-pending-2"
		upsert(ref, 10, []int64{5}, nil)
		if got := pending(ref); !slices.Equal(got, []int64{5}) {
			t.Fatalf("setup = %v", got)
		}
		// Writer A consumed 5 and moved the mark to 20.
		upsert(ref, 20, nil, []int64{5})
		// Writer B read the ledger before A and still sees 5 and 19 as unknown: both are now at/below the mark.
		upsert(ref, 18, []int64{5, 19}, nil)
		if got := pending(ref); len(got) != 0 {
			t.Fatalf("pending = %v, want empty: a stale writer resurrected a consumed id", got)
		}
	})

	t.Run("overflow keeps the oldest ids", func(t *testing.T) {
		ref := "agent/issue-pending-3"
		var add []int64
		for i := int64(1); i <= 10050; i++ {
			add = append(add, i)
		}
		upsert(ref, 10050, add, nil)
		got := pending(ref)
		if len(got) != 10000 || got[0] != 1 || got[9999] != 10000 {
			t.Fatalf("pending = %d ids from %d to %d, want the oldest 10000 (1..10000)", len(got), got[0], got[len(got)-1])
		}
		// A later flood cannot evict an older id either.
		upsert(ref, 20000, []int64{15000, 15001}, nil)
		if got := pending(ref); len(got) != 10000 || got[0] != 1 || got[9999] != 10000 {
			t.Fatalf("a later add changed the oldest-10000 set: %d ids %d..%d", len(got), got[0], got[len(got)-1])
		}
	})

	t.Run("a 10000-id removal is a set operation", func(t *testing.T) {
		ref := "agent/issue-pending-5"
		var add []int64
		for i := int64(1); i <= 10000; i++ {
			add = append(add, i)
		}
		upsert(ref, 10000, add, nil)
		var remove []int64
		for i := int64(1); i <= 10000; i += 2 { // the odd ids, 5000 of them, plus the same again as noise
			remove = append(remove, i, i)
		}
		start := time.Now()
		upsert(ref, 10001, nil, remove)
		if d := time.Since(start); d > 5*time.Second {
			t.Fatalf("removing %d ids took %v, want well under 5s", len(remove), d)
		}
		got := pending(ref)
		if len(got) != 5000 || got[0] != 2 || got[4999] != 10000 {
			t.Fatalf("pending = %d ids from %d to %d, want the 5000 even ids", len(got), got[0], got[len(got)-1])
		}
	})

	t.Run("atomic on-demand create carries the same delta", func(t *testing.T) {
		ref := "agent/issue-pending-4"
		target := uuid.New()
		if _, err := pool.Exec(ctx, `INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status) VALUES ($1, $2, $3, 'issue', 901, 't', 'd', 'completed')`, target, user, repo); err != nil {
			t.Fatal(err)
		}
		upsert(ref, 10, []int64{4}, nil)
		create := func(hw int64, add, remove []int64) {
			t.Helper()
			run, err := q.CreateManualMRReworkRunAndAdvance(ctx, store.CreateManualMRReworkRunAndAdvanceParams{
				Harness: "claude", UserID: user, RepoID: repo, IssueTitle: "t", IssueDescription: "d",
				PipelineRef: pgtype.Text{String: ref, Valid: true}, MrIid: pgtype.Int8{Int64: 66, Valid: true},
				TargetRunID: pgtype.UUID{Bytes: target, Valid: true}, ReviewComments: []byte(`{}`),
				HighWater: hw, PendingAdd: add, PendingRemove: remove,
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `UPDATE runs SET status = 'completed' WHERE id = $1`, run.ID); err != nil {
				t.Fatal(err)
			}
		}
		create(30, []int64{8, 25}, []int64{4}) // 8 <= prior mark 10: dropped; 25 added; 4 consumed
		if got := pending(ref); !slices.Equal(got, []int64{25}) {
			t.Fatalf("pending after the atomic create = %v, want [25]", got)
		}
		create(40, nil, nil)
		if got := pending(ref); !slices.Equal(got, []int64{25}) {
			t.Fatalf("a create without lists changed the set: %v", got)
		}
	})
}

// TestRemoveMRReworkPendingIDsLiveDB pins the pending-only removal used when the snapshot caps
// evict an eligible pending id: array subtraction on the pending set, nothing else changes,
// and a ref with no ledger row is left without one.
func TestRemoveMRReworkPendingIDsLiveDB(t *testing.T) {
	ctx, _, q, repo := reviewAuthorEnv(t)
	ref := "agent/issue-pending-remove"
	if err := q.UpsertMRReworkLedger(ctx, store.UpsertMRReworkLedgerParams{RepoID: repo, Ref: ref, HighWater: 20, PendingAdd: []int64{5, 9, 12}}); err != nil {
		t.Fatal(err)
	}
	before, err := q.GetMRReworkLedger(ctx, store.GetMRReworkLedgerParams{RepoID: repo, Ref: ref})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(before.PendingUnknownIds, []int64{5, 9, 12}) {
		t.Fatalf("seed pending = %v", before.PendingUnknownIds)
	}
	if err := q.RemoveMRReworkPendingIDs(ctx, store.RemoveMRReworkPendingIDsParams{RepoID: repo, Ref: ref, Ids: []int64{9, 5, 777}}); err != nil {
		t.Fatal(err)
	}
	after, err := q.GetMRReworkLedger(ctx, store.GetMRReworkLedgerParams{RepoID: repo, Ref: ref})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(after.PendingUnknownIds, []int64{12}) {
		t.Fatalf("pending after removal = %v, want [12]", after.PendingUnknownIds)
	}
	if after.AttemptCount != before.AttemptCount || after.HighWater != before.HighWater || after.HaltNotified != before.HaltNotified || !after.UpdatedAt.Time.Equal(before.UpdatedAt.Time) {
		t.Fatalf("a pending-only removal changed other columns: %+v -> %+v", before, after)
	}
	// A nil id list (pgx sends NULL) must be a no-op too, not empty the set.
	if err := q.RemoveMRReworkPendingIDs(ctx, store.RemoveMRReworkPendingIDsParams{RepoID: repo, Ref: ref, Ids: nil}); err != nil {
		t.Fatal(err)
	}
	if nilKept, _ := q.GetMRReworkLedger(ctx, store.GetMRReworkLedgerParams{RepoID: repo, Ref: ref}); !slices.Equal(nilKept.PendingUnknownIds, []int64{12}) {
		t.Fatalf("nil Ids changed the pending set: %v, want [12]", nilKept.PendingUnknownIds)
	}
	// An empty id list is a no-op, and an unknown ref creates no row.
	if err := q.RemoveMRReworkPendingIDs(ctx, store.RemoveMRReworkPendingIDsParams{RepoID: repo, Ref: ref, Ids: []int64{}}); err != nil {
		t.Fatal(err)
	}
	if err := q.RemoveMRReworkPendingIDs(ctx, store.RemoveMRReworkPendingIDsParams{RepoID: repo, Ref: "agent/issue-none", Ids: []int64{1}}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.GetMRReworkLedger(ctx, store.GetMRReworkLedgerParams{RepoID: repo, Ref: "agent/issue-none"}); err == nil {
		t.Fatal("removal created a ledger row")
	}
	kept, _ := q.GetMRReworkLedger(ctx, store.GetMRReworkLedgerParams{RepoID: repo, Ref: ref})
	if !slices.Equal(kept.PendingUnknownIds, []int64{12}) {
		t.Fatalf("empty removal changed the set: %v", kept.PendingUnknownIds)
	}
}

// TestMRReworkStaleSupersessionKeepsRepresentativeLiveDB pins the conditional supersession of
// mr_rework_merge_pending: an author's older pending id is replaced by its newer representative
// only when the representative is retained in the same statement. A stale writer whose add the
// high-water filter rejects, or whose replacement is removed or capped away, must not cost the
// author its only pending id.
func TestMRReworkStaleSupersessionKeepsRepresentativeLiveDB(t *testing.T) {
	ctx, pool, q, repo := reviewAuthorEnv(t)
	pending := func(ref string) []int64 {
		t.Helper()
		led, err := q.GetMRReworkLedger(ctx, store.GetMRReworkLedgerParams{RepoID: repo, Ref: ref})
		if err != nil {
			t.Fatal(err)
		}
		return led.PendingUnknownIds
	}
	upsert := func(ref string, hw int64, add, remove, sup, supBy []int64) {
		t.Helper()
		if err := q.UpsertMRReworkLedger(ctx, store.UpsertMRReworkLedgerParams{
			RepoID: repo, Ref: ref, HighWater: hw, PendingAdd: add, PendingRemove: remove,
			PendingSuperseded: sup, PendingSupersededBy: supBy,
		}); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("a stale writer's rejected replacement keeps the older id", func(t *testing.T) {
		ref := "agent/issue-stale-1"
		upsert(ref, 120, []int64{100}, nil, nil, nil)
		upsert(ref, 300, []int64{100}, nil, nil, nil) // a faster writer moved the mark to 300
		// The stale writer read the ledger at mark 120: its add of 170 is now at/below the mark.
		upsert(ref, 180, []int64{170}, nil, []int64{100}, []int64{170})
		if got := pending(ref); !slices.Contains(got, 100) {
			t.Fatalf("pending = %v, want 100 kept: the replacement 170 was rejected by the mark", got)
		}
	})

	t.Run("an accepted replacement supersedes the older id", func(t *testing.T) {
		ref := "agent/issue-stale-2"
		upsert(ref, 120, []int64{100}, nil, nil, nil)
		upsert(ref, 200, []int64{170}, nil, []int64{100}, []int64{170})
		if got := pending(ref); !slices.Equal(got, []int64{170}) {
			t.Fatalf("pending = %v, want [170]", got)
		}
	})

	t.Run("a replacement removed in the same call keeps the older id", func(t *testing.T) {
		ref := "agent/issue-stale-3"
		upsert(ref, 120, []int64{100}, nil, nil, nil)
		upsert(ref, 200, []int64{170}, []int64{170}, []int64{100}, []int64{170})
		if got := pending(ref); !slices.Equal(got, []int64{100}) {
			t.Fatalf("pending = %v, want [100]", got)
		}
	})

	t.Run("at the cap the replacement keeps the older id's slot", func(t *testing.T) {
		ref := "agent/issue-stale-4"
		var ids []int64
		for i := int64(1); i <= 10000; i++ {
			ids = append(ids, i)
		}
		upsert(ref, 10000, ids, nil, nil, nil)
		// 12000 (another author) and 15000 (the replacement for 10000) both pass the mark filter
		// (above the prior mark 10000, at or below the new one), so the merged set is 10001 and the
		// cap cuts one id. The retained replacement takes the older id's slot, so the newer
		// non-replacement add 12000 is the one cut, although 12000 < 15000.
		upsert(ref, 20000, []int64{12000, 15000}, nil, []int64{10000}, []int64{15000})
		got := pending(ref)
		if len(got) != 10000 || !slices.Contains(got, 15000) || slices.Contains(got, 12000) || slices.Contains(got, 10000) {
			t.Fatalf("pending = %d ids (contains 15000: %t, 12000: %t, 10000: %t), want 10000 ids with 15000 kept, 12000 cut and 10000 gone",
				len(got), slices.Contains(got, 15000), slices.Contains(got, 12000), slices.Contains(got, 10000))
		}
	})

	t.Run("the atomic on-demand create applies the same conditional supersession", func(t *testing.T) {
		var user uuid.UUID
		if err := pool.QueryRow(ctx, `SELECT user_id FROM forge_connections WHERE id = (SELECT connection_id FROM repos WHERE id = $1)`, repo).Scan(&user); err != nil {
			t.Fatal(err)
		}
		ref := "agent/issue-stale-5"
		target := uuid.New()
		if _, err := pool.Exec(ctx, `INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status) VALUES ($1, $2, $3, 'issue', 902, 't', 'd', 'completed')`, target, user, repo); err != nil {
			t.Fatal(err)
		}
		create := func(hw int64, add, sup, supBy []int64) {
			t.Helper()
			run, err := q.CreateManualMRReworkRunAndAdvance(ctx, store.CreateManualMRReworkRunAndAdvanceParams{
				Harness: "claude", UserID: user, RepoID: repo, IssueTitle: "t", IssueDescription: "d",
				PipelineRef: pgtype.Text{String: ref, Valid: true}, MrIid: pgtype.Int8{Int64: 67, Valid: true},
				TargetRunID: pgtype.UUID{Bytes: target, Valid: true}, ReviewComments: []byte(`{}`),
				HighWater: hw, PendingAdd: add, PendingRemove: []int64{},
				PendingSuperseded: sup, PendingSupersededBy: supBy,
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `UPDATE runs SET status = 'completed' WHERE id = $1`, run.ID); err != nil {
				t.Fatal(err)
			}
		}
		upsert(ref, 120, []int64{100}, nil, nil, nil)
		upsert(ref, 300, []int64{100}, nil, nil, nil)
		create(180, []int64{170}, []int64{100}, []int64{170})
		if got := pending(ref); !slices.Contains(got, 100) {
			t.Fatalf("pending = %v, want 100 kept through the atomic create", got)
		}
	})
}
