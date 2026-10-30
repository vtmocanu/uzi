package workersvc

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// The clock used by both replicas is independent of prerequisiteMemory.now.
// Advance only while processors are stopped at the before-BEGIN barriers.
type cleanupRaceClockLiveDB struct{ nanos atomic.Int64 }

func (c *cleanupRaceClockLiveDB) nowLiveDB() time.Time {
	return time.Unix(0, c.nanos.Load()).UTC()
}

func (c *cleanupRaceClockLiveDB) setLiveDB(t *testing.T, f *prerequisiteExecutorLiveDB, at time.Time) {
	t.Helper()
	f.env.exec("UPDATE prerequisite_test_clock SET observed_at=$1", at)
	c.nanos.Store(at.UnixNano())
}

// Ambiguous deletes retain the expected tip, unlike prerequisiteHTTP. Creation
// and advertisements still use that fixture and the production broker protocol.
type cleanupRaceRemoteLiveDB struct {
	h            *prerequisiteHTTP
	deletes      atomic.Int32
	removeFirst  atomic.Bool
	listArrivals atomic.Int32
	gateMu       sync.Mutex
	listGate     chan struct{}
}

func (r *cleanupRaceRemoteLiveDB) serveLiveDB(w http.ResponseWriter, req *http.Request) {
	r.gateMu.Lock()
	gate := r.listGate
	r.gateMu.Unlock()
	if req.Method == http.MethodGet && req.URL.Query().Get("service") == "git-upload-pack" && gate != nil {
		n := r.listArrivals.Add(1)
		if n == 2 {
			close(gate)
		}
		if n <= 2 {
			select {
			case <-gate:
			case <-req.Context().Done():
				return
			}
		}
	}
	if req.Method == http.MethodPost {
		body, err := io.ReadAll(io.LimitReader(req.Body, 1<<20))
		if err != nil {
			http.Error(w, "read command", http.StatusBadRequest)
			return
		}
		scan := pktline.NewScanner(bytes.NewReader(body))
		if scan.Scan() {
			fields := strings.Fields(strings.SplitN(string(scan.Bytes()), "\x00", 2)[0])
			if len(fields) == 3 && fields[1] == strings.Repeat("0", 40) {
				if fields[0] != prerequisiteTestTip || fields[2] != r.h.refName {
					http.Error(w, "unexpected delete command", http.StatusBadRequest)
					return
				}
				n := r.deletes.Add(1)
				if r.removeFirst.Load() && n == 1 {
					r.h.mu.Lock()
					r.h.tip = ""
					r.h.mu.Unlock()
				}
				// An error response proves neither deletion nor absence.
				http.Error(w, "ambiguous receive-pack result", http.StatusServiceUnavailable)
				return
			}
		}
		req.Body = io.NopCloser(bytes.NewReader(body))
	}
	r.h.serve(w, req)
}

func newCleanupRaceLiveDB(t *testing.T, prior int) (*prerequisiteExecutorLiveDB, *cleanupRaceClockLiveDB, *cleanupRaceRemoteLiveDB, *Service) {
	t.Helper()
	f := newPrerequisiteExecutorLiveDB(t, 1)
	remote := &cleanupRaceRemoteLiveDB{h: f.h}
	server := httptest.NewServer(http.HandlerFunc(remote.serveLiveDB))
	t.Cleanup(server.Close)
	// Change both binding sources before the first pin freezes the binding.
	f.m.conn.BaseUrl = server.URL
	f.m.repo.WebUrl = server.URL + "/repo"
	f.env.exec("UPDATE forge_connections SET base_url=$1 WHERE id=$2", server.URL, f.m.conn.ID)
	f.env.exec("UPDATE repos SET web_url=$1 WHERE id=$2", f.m.repo.WebUrl, f.m.row.RepoID)
	f.runLiveDB(t)
	if r := f.readLiveDB(t); r.State != "pinned" || !r.LiveRunID.Valid {
		t.Fatal("real broker creation did not establish a live pin")
	}
	f.expireCaptureLiveDB(t)
	// Production reservations and their real backoff, with unchanged triggers.
	// These are prior crashed attempts; only the final generations execute I/O.
	for i := 1; i <= prior; i++ {
		r, err := f.env.q.ReservePrerequisiteCleanup(f.env.ctx, store.ReservePrerequisiteCleanupParams{
			ID: f.m.row.ID, PassID: prerequisiteID(uuid.New()), LeaseSeconds: 8,
		})
		if err != nil {
			t.Fatal(err)
		}
		if r.CleanupAttempts != int32(i) {
			t.Fatalf("seed reservation=%d want=%d", r.CleanupAttempts, i)
		}
		f.dueLiveDB(t)
	}
	clock := &cleanupRaceClockLiveDB{}
	clock.nanos.Store(f.m.now.UnixNano())
	f.s.SetClock(clock.nowLiveDB)
	other := New(f.env.q, f.s.box, testParams())
	other.SetTxBeginner(f.env.pool)
	other.SetClock(clock.nowLiveDB)
	other.SetForges(&m3Builder{build: func(baseURL string) (forge.Forge, error) {
		return f.fixtureForge(t, baseURL, f.builder.f)
	}})
	other.SetForgeBaseURLAllowed(func(string) bool { return true })
	// Start in the cleanup retry class: the first audit is also due after
	// capture expiry, and an audit-only item would deduplicate cleanup.
	f.s.prerequisite.lead = 4
	other.prerequisite.lead = 4
	return f, clock, remote, other
}

// The adapter counts the production audit/reserve/outcome transactions. Waiting
// precedes Begin, so it holds neither a connection nor capture/closure locks.
// Context deadlines bound every wait; a failure does not strand the other replica.
type cleanupRaceTxLiveDB struct {
	f            *prerequisiteExecutorLiveDB
	calls        atomic.Int32
	entered      chan struct{}
	release      chan struct{}
	once         sync.Once
	deletedRows  atomic.Int64
	deletedCalls atomic.Int32
	retryRows    atomic.Int64
}

type cleanupRaceQueriesLiveDB struct {
	PrerequisiteStore
	observer *cleanupRaceTxLiveDB
}

func (q *cleanupRaceQueriesLiveDB) RecordPrerequisiteDeleted(ctx context.Context, p store.RecordPrerequisiteDeletedParams) (int64, error) {
	n, err := q.PrerequisiteStore.RecordPrerequisiteDeleted(ctx, p)
	q.observer.deletedRows.Store(n)
	q.observer.deletedCalls.Add(1)
	return n, err
}

func (q *cleanupRaceQueriesLiveDB) RecordPrerequisiteCleanupRetry(ctx context.Context, p store.RecordPrerequisiteCleanupRetryParams) (int64, error) {
	n, err := q.PrerequisiteStore.RecordPrerequisiteCleanupRetry(ctx, p)
	q.observer.retryRows.Store(n)
	return n, err
}

func (o *cleanupRaceTxLiveDB) unblockLiveDB() { o.once.Do(func() { close(o.release) }) }

func (o *cleanupRaceTxLiveDB) transactionLiveDB(ctx context.Context, fn func(PrerequisiteStore) error) error {
	if o.calls.Add(1) == 3 {
		close(o.entered)
		select {
		case <-o.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	tx, err := o.f.env.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() {
		c, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = tx.Rollback(c)
	}()
	if err := fn(&cleanupRaceQueriesLiveDB{PrerequisiteStore: store.New(tx), observer: o}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func cleanupRaceWaitLiveDB(t *testing.T, ctx context.Context, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-ctx.Done():
		t.Fatal("barrier: ", ctx.Err())
	}
}

func cleanupRaceSweepLiveDB(t *testing.T, ctx context.Context, s *Service) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		_, err := s.SweepPrerequisiteClosures(ctx)
		done <- err
		close(done)
	}()
	// Each sweep is bounded by ctx and the production five-second outcome tail.
	t.Cleanup(func() {
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("join sweep: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("sweep did not join after cancellation/release")
		}
	})
	return done
}

func cleanupRaceJoinLiveDB(t *testing.T, ctx context.Context, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("sweep: ", ctx.Err())
	}
}

func cleanupRaceExhaustedLiveDB(t *testing.T, f *prerequisiteExecutorLiveDB, clock *cleanupRaceClockLiveDB) {
	t.Helper()
	r := f.readLiveDB(t)
	if r.State != "pinned" || !r.LiveRunID.Valid || r.CleanupAttempts != 8 || !r.CleanupExhaustedAt.Valid || r.CleanupDeadline.Valid || r.Reason != "pin_cleanup_exhausted" {
		t.Fatalf("state=%s live=%t attempts=%d exhausted=%t lease=%t reason=%s", r.State, r.LiveRunID.Valid, r.CleanupAttempts, r.CleanupExhaustedAt.Valid, r.CleanupDeadline.Valid, r.Reason)
	}
	// Reservation includes the short lease as well as the capped one-hour
	// backoff; subsequent listing-only retries schedule exactly one hour.
	delay := r.NextAttemptAt.Time.Sub(clock.nowLiveDB())
	if delay < time.Hour || delay > time.Hour+time.Minute {
		t.Fatalf("exhausted retry delay=%s", delay)
	}
}

func cleanupRaceFollowupsLiveDB(t *testing.T, f *prerequisiteExecutorLiveDB, clock *cleanupRaceClockLiveDB, remote *cleanupRaceRemoteLiveDB, services ...*Service) {
	t.Helper()
	before := remote.deletes.Load()
	for _, s := range services {
		clock.setLiveDB(t, f, f.readLiveDB(t).NextAttemptAt.Time)
		requests := f.h.requests.Load()
		if _, err := s.SweepPrerequisiteClosures(f.env.ctx); err != nil {
			t.Fatal(err)
		}
		cleanupRaceExhaustedLiveDB(t, f, clock)
		if f.h.requests.Load() <= requests || remote.deletes.Load() != before {
			t.Fatal("follow-up must list without another receive-pack delete")
		}
		if delay := f.readLiveDB(t).NextAttemptAt.Time.Sub(clock.nowLiveDB()); delay != time.Hour {
			t.Fatalf("listing-only backoff=%s", delay)
		}
	}
	remote.h.mu.Lock()
	tip := remote.h.tip
	remote.h.mu.Unlock()
	if tip != prerequisiteTestTip {
		t.Fatal("ambiguous cleanup changed remote expected tip")
	}
}

func TestPrerequisiteCleanupReplicaRaceAtSevenLiveDB(t *testing.T) {
	f, clock, remote, other := newCleanupRaceLiveDB(t, 7)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	// Both public sweeps own independent process slots and reach real listing
	// I/O before either may reserve generation eight.
	remote.gateMu.Lock()
	remote.listGate = make(chan struct{})
	remote.gateMu.Unlock()
	a := cleanupRaceSweepLiveDB(t, ctx, f.s)
	b := cleanupRaceSweepLiveDB(t, ctx, other)
	cleanupRaceJoinLiveDB(t, ctx, a)
	cleanupRaceJoinLiveDB(t, ctx, b)
	if remote.listArrivals.Load() < 2 || remote.deletes.Load() != 1 {
		t.Fatalf("listing arrivals=%d actual delete commands=%d", remote.listArrivals.Load(), remote.deletes.Load())
	}
	cleanupRaceExhaustedLiveDB(t, f, clock)
	cleanupRaceFollowupsLiveDB(t, f, clock, remote, f.s, other)
}

func TestPrerequisiteCleanupOldOutcomeAgainstPinnedLeaseLiveDB(t *testing.T) {
	f, clock, remote, other := newCleanupRaceLiveDB(t, 6)
	remote.removeFirst.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	a := &cleanupRaceTxLiveDB{f: f, entered: make(chan struct{}), release: make(chan struct{})}
	b := &cleanupRaceTxLiveDB{f: f, entered: make(chan struct{}), release: make(chan struct{})}
	f.s.SetPrerequisiteStore(f.env.q, a.transactionLiveDB)
	other.SetPrerequisiteStore(f.env.q, b.transactionLiveDB)
	// Register release after sweep join cleanups, so release runs first on Fatal.
	doneA := cleanupRaceSweepLiveDB(t, ctx, f.s)
	t.Cleanup(a.unblockLiveDB)
	cleanupRaceWaitLiveDB(t, ctx, a.entered)
	old := f.readLiveDB(t)
	if old.State != "pinned" || old.CleanupAttempts != 7 || !old.CleanupDeadline.Valid || remote.deletes.Load() != 1 {
		t.Fatal("A did not reserve/delete generation seven")
	}
	remote.h.mu.Lock()
	absent := remote.h.tip == ""
	// Explicit external fault: another actor recreates exactly the expected pin.
	remote.h.tip = prerequisiteTestTip
	remote.h.mu.Unlock()
	if !absent {
		t.Fatal("A's delete did not produce remote absence")
	}
	clock.setLiveDB(t, f, old.NextAttemptAt.Time)
	doneB := cleanupRaceSweepLiveDB(t, ctx, other)
	t.Cleanup(b.unblockLiveDB)
	cleanupRaceWaitLiveDB(t, ctx, b.entered)
	current := f.readLiveDB(t)
	if current.State != "pinned" || current.CleanupAttempts != 8 || !current.LiveRunID.Valid || !current.CleanupDeadline.Valid || !current.CleanupDeadline.Time.After(clock.nowLiveDB()) || current.LastAuditPassID == old.LastAuditPassID || remote.deletes.Load() != 2 {
		t.Fatal("B did not retain pinned generation eight with a current lease and distinct pass")
	}
	a.unblockLiveDB()
	cleanupRaceJoinLiveDB(t, ctx, doneA)
	if a.deletedCalls.Load() != 1 || a.deletedRows.Load() != 0 {
		t.Fatalf("old real deleted outcome calls=%d affected rows=%d", a.deletedCalls.Load(), a.deletedRows.Load())
	}
	after := f.readLiveDB(t)
	// Whole-row equality includes generation/pass/deadline/state/live pointer;
	// diagnostics name only the first differing field, never dump array payloads.
	if !reflect.DeepEqual(current, after) {
		x, y := reflect.ValueOf(current), reflect.ValueOf(after)
		for i := 0; i < x.NumField(); i++ {
			if !reflect.DeepEqual(x.Field(i).Interface(), y.Field(i).Interface()) {
				t.Fatalf("stale outcome changed current field %s", x.Type().Field(i).Name)
			}
		}
	}
	// B remains paused until after the stale SQL and whole-row comparison.
	b.unblockLiveDB()
	cleanupRaceJoinLiveDB(t, ctx, doneB)
	if b.retryRows.Load() != 1 {
		t.Fatalf("current ambiguous outcome affected rows=%d", b.retryRows.Load())
	}
	cleanupRaceExhaustedLiveDB(t, f, clock)
	cleanupRaceFollowupsLiveDB(t, f, clock, remote, f.s, other)
}
