package historyretention

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/vtmocanu/uzi/api/internal/store"
)

const (
	tPR    = store.HistoryRetentionPRHistoryEvents
	tAnth  = store.HistoryRetentionAnthropicUsage
	tCodex = store.HistoryRetentionCodexUsage
	tCap   = store.HistoryRetentionWorkerCapacitySample
)

var baseOrder = []store.HistoryRetentionTable{tPR, tAnth, tCodex, tCap}

type delCall struct {
	table  store.HistoryRetentionTable
	cutoff time.Time
	limit  int32
	ctx    context.Context
}

// fakeSession scripts DeleteBatch per table; a table without a script deletes 7 rows.
type fakeSession struct {
	mu        sync.Mutex
	cutoffs   int
	cutoffCtx context.Context
	cutoffErr error
	onCutoff  func()
	calls     []delCall
	script    map[store.HistoryRetentionTable]func(attempt int, limit int32, ctx context.Context) (int64, error)
	attempts  map[store.HistoryRetentionTable]int
	idle      bool
	closeCtx  context.Context
	closed    int
}

func newFake() *fakeSession {
	return &fakeSession{
		script:   map[store.HistoryRetentionTable]func(int, int32, context.Context) (int64, error){},
		attempts: map[store.HistoryRetentionTable]int{},
		idle:     true,
	}
}

var refTime = time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

func (f *fakeSession) Cutoff(ctx context.Context, retention time.Duration) (time.Time, time.Time, error) {
	f.mu.Lock()
	f.cutoffs++
	f.cutoffCtx = ctx
	hook := f.onCutoff
	err := f.cutoffErr
	n := f.cutoffs
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	// a distinct cutoff per call, so "one cutoff reaches all four deletes" is observable
	ref := refTime.Add(time.Duration(n) * time.Hour)
	return ref, ref.Add(-retention), nil
}

func (f *fakeSession) DeleteBatch(ctx context.Context, table store.HistoryRetentionTable, cutoff time.Time, limit int32) (int64, error) {
	f.mu.Lock()
	f.calls = append(f.calls, delCall{table, cutoff, limit, ctx})
	f.attempts[table]++
	attempt := f.attempts[table]
	fn := f.script[table]
	f.mu.Unlock()
	if fn != nil {
		return fn(attempt, limit, ctx)
	}
	return 7, nil
}

func (f *fakeSession) Idle() bool { return f.idle }

func (f *fakeSession) Close(ctx context.Context) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed++
	f.closeCtx = ctx
	return true, nil
}

func (f *fakeSession) tables() []store.HistoryRetentionTable {
	var out []store.HistoryRetentionTable
	for _, c := range f.calls {
		out = append(out, c.table)
	}
	return out
}

func openerOf(sessions ...*fakeSession) (opener, *int) {
	opens := 0
	return func(context.Context, context.Context) (session, bool, error) {
		opens++
		if opens > len(sessions) {
			return sessions[len(sessions)-1], true, nil
		}
		return sessions[opens-1], true, nil
	}, &opens
}

func rollback(table store.HistoryRetentionTable, code string) error {
	return &store.HistoryRetentionRollbackError{Table: table, Code: code, Err: &pgconn.PgError{Code: code}}
}

func timeoutErr(table store.HistoryRetentionTable) error { return rollback(table, "57014") }

func equalTables(a, b []store.HistoryRetentionTable) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestPassOffNeverOpens(t *testing.T) {
	open, opens := openerOf(newFake())
	r := newRetention(open, time.Now, 0, 0)
	n, err := r.Pass(context.Background())
	if n != 0 || err != nil || *opens != 0 {
		t.Fatalf("off: n=%d err=%v opens=%d, want 0 nil 0", n, err, *opens)
	}
}

func TestPassLockNotAcquired(t *testing.T) {
	r := newRetention(func(context.Context, context.Context) (session, bool, error) { return nil, false, nil },
		time.Now, 24*time.Hour, 0)
	n, err := r.Pass(context.Background())
	if n != 0 || err != nil {
		t.Fatalf("contended: n=%d err=%v, want 0 nil", n, err)
	}
	// a contended tick holds no lock, so it must not advance the rotation
	if r.counter != 0 {
		t.Fatalf("counter = %d after a tick that did not hold the lock, want 0", r.counter)
	}
}

func TestPassOpenErrorIsReturned(t *testing.T) {
	boom := errors.New("setup failed")
	r := newRetention(func(context.Context, context.Context) (session, bool, error) { return nil, false, boom },
		time.Now, 24*time.Hour, 0)
	if n, err := r.Pass(context.Background()); n != 0 || !errors.Is(err, boom) {
		t.Fatalf("n=%d err=%v, want 0 and the open error", n, err)
	}
}

func TestPassOneCutoffReachesAllTablesCappedAt1000(t *testing.T) {
	f := newFake()
	open, _ := openerOf(f)
	r := newRetention(open, time.Now, 48*time.Hour, 0)
	n, err := r.Pass(context.Background())
	if err != nil || n != 28 {
		t.Fatalf("n=%d err=%v, want 28 nil", n, err)
	}
	if f.cutoffs != 1 {
		t.Fatalf("cutoff reads = %d, want 1", f.cutoffs)
	}
	if len(f.calls) != 4 {
		t.Fatalf("delete calls = %d, want 4", len(f.calls))
	}
	want := refTime.Add(time.Hour).Add(-48 * time.Hour)
	for _, c := range f.calls {
		if !c.cutoff.Equal(want) {
			t.Errorf("%s cutoff = %v, want the one cutoff %v", c.table, c.cutoff, want)
		}
		if c.limit != 1000 {
			t.Errorf("%s limit = %d, want 1000", c.table, c.limit)
		}
	}
	if f.closed != 1 {
		t.Errorf("closed = %d, want 1", f.closed)
	}
}

func TestPassTableOrderFollowsCounterAndAdvancesPerLockHoldingTick(t *testing.T) {
	var sessions []*fakeSession
	for i := 0; i < 8; i++ {
		sessions = append(sessions, newFake())
	}
	// tick 3 fails at Cutoff: a failed lock-holding tick still advances the rotation
	sessions[2].cutoffErr = errors.New("cutoff broke")
	open, _ := openerOf(sessions...)
	r := newRetention(open, time.Now, 24*time.Hour, 2) // random-start stand-in: begin at 2
	for i := 0; i < 8; i++ {
		_, _ = r.Pass(context.Background())
	}
	for i, s := range sessions {
		if i == 2 {
			if len(s.calls) != 0 {
				t.Errorf("tick %d: deletes after a failed cutoff: %v", i, s.tables())
			}
			continue
		}
		start := (2 + i) % 4
		var want []store.HistoryRetentionTable
		for j := 0; j < 4; j++ {
			want = append(want, baseOrder[(start+j)%4])
		}
		if !equalTables(s.tables(), want) {
			t.Errorf("tick %d: order %v, want %v", i, s.tables(), want)
		}
	}
}

func TestPassKnownRollbackLetsOtherTablesRun(t *testing.T) {
	f := newFake()
	f.script[tAnth] = func(int, int32, context.Context) (int64, error) { return 0, rollback(tAnth, "55P03") }
	open, _ := openerOf(f)
	r := newRetention(open, time.Now, 24*time.Hour, 0)
	n, err := r.Pass(context.Background())
	if n != 21 {
		t.Fatalf("n=%d, want 21 (three tables x 7)", n)
	}
	if err == nil || !strings.Contains(err.Error(), string(tAnth)) {
		t.Fatalf("err=%v, want it to name %s", err, tAnth)
	}
	if len(f.calls) != 4 || f.closed != 1 {
		t.Fatalf("calls=%d closed=%d, want 4 and 1", len(f.calls), f.closed)
	}
}

func TestPassConnectionErrorEndsTickAndCloses(t *testing.T) {
	f := newFake()
	f.script[tAnth] = func(int, int32, context.Context) (int64, error) { return 0, errors.New("connection reset") }
	open, _ := openerOf(f)
	r := newRetention(open, time.Now, 24*time.Hour, 0)
	n, err := r.Pass(context.Background())
	if n != 7 {
		t.Fatalf("n=%d, want only the confirmed 7 from the first table", n)
	}
	if err == nil || !strings.Contains(err.Error(), string(tAnth)) || !strings.Contains(err.Error(), "unknown outcome") {
		t.Fatalf("err=%v, want an unknown outcome naming %s", err, tAnth)
	}
	if !equalTables(f.tables(), []store.HistoryRetentionTable{tPR, tAnth}) {
		t.Fatalf("tables reached = %v, want the tick to end after %s", f.tables(), tAnth)
	}
	if f.closed != 1 {
		t.Fatalf("closed = %d, want 1", f.closed)
	}
}

func TestPassNonIdleSessionEndsTick(t *testing.T) {
	f := newFake()
	f.script[tPR] = func(int, int32, context.Context) (int64, error) {
		f.idle = false
		return 0, &pgconn.PgError{Code: "XX000"}
	}
	open, _ := openerOf(f)
	r := newRetention(open, time.Now, 24*time.Hour, 0)
	n, err := r.Pass(context.Background())
	if n != 0 || err == nil || len(f.calls) != 1 || f.closed != 1 {
		t.Fatalf("n=%d err=%v calls=%d closed=%d, want 0, an error, 1 and 1", n, err, len(f.calls), f.closed)
	}
}

func TestPassIdleServerErrorIsUnknownButContinues(t *testing.T) {
	f := newFake()
	f.script[tAnth] = func(int, int32, context.Context) (int64, error) { return 0, &pgconn.PgError{Code: "XX000"} }
	open, _ := openerOf(f)
	r := newRetention(open, time.Now, 24*time.Hour, 0)
	n, err := r.Pass(context.Background())
	if n != 21 || err == nil || !strings.Contains(err.Error(), "unknown outcome") {
		t.Fatalf("n=%d err=%v, want 21 and an unknown outcome", n, err)
	}
}

func TestPassContextCoverage(t *testing.T) {
	f := newFake()
	var gotWork, gotCleanup context.Context
	open := func(w, c context.Context) (session, bool, error) {
		gotWork, gotCleanup = w, c
		return f, true, nil
	}
	start := time.Now()
	r := newRetention(open, func() time.Time { return start }, 24*time.Hour, 0)
	if _, err := r.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	deadline := start.Add(2 * time.Second)
	check := func(what string, ctx context.Context, want time.Time) {
		t.Helper()
		got, ok := ctx.Deadline()
		if !ok || !got.Equal(want) {
			t.Errorf("%s deadline = %v (set=%v), want %v", what, got, ok, want)
		}
	}
	check("opener work ctx", gotWork, deadline.Add(-250*time.Millisecond))
	check("opener cleanup ctx", gotCleanup, deadline)
	check("Cutoff ctx", f.cutoffCtx, deadline.Add(-250*time.Millisecond))
	for _, c := range f.calls {
		check("DeleteBatch ctx "+string(c.table), c.ctx, deadline.Add(-250*time.Millisecond))
	}
	check("Close ctx", f.closeCtx, deadline)
}

func TestPassNoWorkAfterWorkDeadlineButCloseRuns(t *testing.T) {
	f := newFake()
	start := time.Now()
	now := start
	f.onCutoff = func() { now = start.Add(1800 * time.Millisecond) } // past D - 250ms
	open, _ := openerOf(f)
	r := newRetention(open, func() time.Time { return now }, 24*time.Hour, 0)
	n, err := r.Pass(context.Background())
	if n != 0 || err != nil || len(f.calls) != 0 {
		t.Fatalf("n=%d err=%v calls=%d, want no delete after the work deadline", n, err, len(f.calls))
	}
	if f.closed != 1 {
		t.Fatalf("closed = %d, want Close to run", f.closed)
	}

	// the same when the deadline is already behind us before Cutoff
	g := newFake()
	now = start
	open2, _ := openerOf(g)
	r2 := newRetention(open2, func() time.Time {
		defer func() { now = now.Add(5 * time.Second) }() // the first read sets D, every later read is past it
		return now
	}, 24*time.Hour, 0)
	if _, err := r2.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if g.cutoffs != 0 || len(g.calls) != 0 || g.closed != 1 {
		t.Fatalf("cutoffs=%d calls=%d closed=%d, want 0 0 1", g.cutoffs, len(g.calls), g.closed)
	}
}

func TestPassBlockingSessionCancelsAndCloses(t *testing.T) {
	f := newFake()
	f.script[tPR] = func(_ int, _ int32, ctx context.Context) (int64, error) {
		<-ctx.Done()
		f.idle = false
		return 0, ctx.Err()
	}
	open, _ := openerOf(f)
	r := newRetention(open, time.Now, 24*time.Hour, 0)
	type result struct {
		n   int64
		err error
	}
	done := make(chan result, 1)
	begin := time.Now()
	go func() {
		n, err := r.Pass(context.Background())
		done <- result{n, err}
	}()
	var res result
	select {
	case res = <-done:
	case <-time.After(30 * time.Second): // hang detector only, not a timing assertion
		t.Fatal("Pass did not return: the blocked delete was never cancelled")
	}
	elapsed := time.Since(begin)
	if res.n != 0 || !errors.Is(res.err, context.DeadlineExceeded) || f.closed != 1 {
		t.Fatalf("n=%d err=%v closed=%d, want 0, a deadline error and Close", res.n, res.err, f.closed)
	}
	if elapsed < 1500*time.Millisecond {
		t.Fatalf("blocked pass returned after %v, before the work deadline: the block was not exercised", elapsed)
	}
}

type starveFn = func(int, int32, context.Context) (int64, error)

// starvationRun drives len(schedule) ticks. schedule[i] names the instance (an index into
// counters, each its own rotation counter) that wins tick i. The shared fake clock is reset to
// the real time at the start of every tick, so the contexts are live; a shape can advance it to
// consume the work budget without a real sleep. It returns, per instance, the set of tables
// attempted in each tick that instance won, in order.
func starvationRun(t *testing.T, shapes map[store.HistoryRetentionTable]func(clock *time.Time) starveFn, counters []uint64, schedule []int) ([][]map[store.HistoryRetentionTable]bool, [][]store.HistoryRetentionTable) {
	t.Helper()
	var order [][]store.HistoryRetentionTable // the tables attempted in each tick, in call order
	var clock time.Time
	var sessions []*fakeSession
	opener := func(context.Context, context.Context) (session, bool, error) {
		f := newFake()
		for table, mk := range shapes {
			f.script[table] = mk(&clock)
		}
		sessions = append(sessions, f)
		return f, true, nil
	}
	instances := make([]*Retention, len(counters))
	for i, c := range counters {
		instances[i] = newRetention(opener, func() time.Time { return clock }, 24*time.Hour, c)
	}
	wins := make([][]map[store.HistoryRetentionTable]bool, len(counters))
	for _, inst := range schedule {
		clock = time.Now()
		before := len(sessions)
		_, _ = instances[inst].Pass(context.Background())
		if len(sessions) != before+1 {
			t.Fatalf("a tick opened %d sessions, want 1", len(sessions)-before)
		}
		got := map[store.HistoryRetentionTable]bool{}
		for _, c := range sessions[before].calls {
			got[c.table] = true
		}
		wins[inst] = append(wins[inst], got)
		order = append(order, sessions[before].tables())
	}
	return wins, order
}

// assertEveryTableWithinFour fails when, in any four consecutive ticks won by one instance, a
// table in want was never attempted (the rotation advances once per lock-holding tick, so each
// table leads once per four).
func assertEveryTableWithinFour(t *testing.T, wins [][]map[store.HistoryRetentionTable]bool, want []store.HistoryRetentionTable) {
	t.Helper()
	for inst, ticks := range wins {
		if len(ticks) < 8 {
			t.Fatalf("instance %d won only %d ticks, want at least 8", inst, len(ticks))
		}
		for from := 0; from+4 <= len(ticks); from++ {
			for _, tb := range want {
				seen := false
				for _, tick := range ticks[from : from+4] {
					seen = seen || tick[tb]
				}
				if !seen {
					t.Errorf("instance %d: %s was not attempted in its ticks %d..%d (attempted per tick: %v)", inst, tb, from, from+3, ticks[from:from+4])
				}
			}
		}
	}
}

// unevenSchedule is n ticks in which instance 1 wins every third one and instance 0 the rest.
func unevenSchedule(n int) []int {
	out := make([]int, n)
	for i := range out {
		if i%3 == 2 {
			out[i] = 1
		}
	}
	return out
}

func TestPassStarvationOverManyTicks(t *testing.T) {
	// One failure shape per table over 24 ticks that alternate unevenly between two instances
	// (each with its own rotation counter): a known rollback, a statement timeout and a connection
	// error. A table that is skipped by the tick that failed before it must still lead, and so be
	// attempted, within four ticks of the instance that won them.
	shapes := map[store.HistoryRetentionTable]func(*time.Time) starveFn{
		tPR: func(*time.Time) starveFn {
			return func(int, int32, context.Context) (int64, error) { return 0, rollback(tPR, "55P03") }
		},
		tAnth: func(*time.Time) starveFn {
			return func(int, int32, context.Context) (int64, error) { return 0, timeoutErr(tAnth) }
		},
		tCodex: func(*time.Time) starveFn {
			return func(int, int32, context.Context) (int64, error) { return 0, errors.New("connection reset") }
		},
	}
	wins, _ := starvationRun(t, shapes, []uint64{1, 3}, unevenSchedule(24))
	assertEveryTableWithinFour(t, wins, baseOrder)
}

func TestPassStarvationDeadlineTable(t *testing.T) {
	// pr_history_events consumes the whole work budget whenever it is reached: the fake clock jumps
	// past the work deadline and the delete then succeeds, so only the deadline check (or the end of
	// the rotation, when it comes last) can end the tick; no error does. Nothing after it in that tick's rotation may be
	// attempted, and every other table must still get an attempt within four ticks, alone and with
	// two instances alternating, whatever position the blocking table has in a rotation.
	blocking := map[store.HistoryRetentionTable]func(*time.Time) starveFn{
		tPR: func(clock *time.Time) starveFn {
			return func(int, int32, context.Context) (int64, error) {
				*clock = clock.Add(totalBudget)
				return 3, nil
			}
		},
	}
	others := []store.HistoryRetentionTable{tAnth, tCodex, tCap}
	for _, tc := range []struct {
		name     string
		counters []uint64
		schedule []int
	}{
		{"one instance", []uint64{0}, make([]int, 12)},
		{"one instance, start 2 mod 4", []uint64{6}, make([]int, 12)},
		{"two instances", []uint64{1, 3}, unevenSchedule(24)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wins, order := starvationRun(t, blocking, tc.counters, tc.schedule)
			assertEveryTableWithinFour(t, wins, others)
			// The blocking table must itself be reached (it is what exhausts the budget), or the
			// scenario never exercised the deadline. Where it is, it is the last table attempted.
			reached := false
			for i, tables := range order {
				for j, tb := range tables {
					if tb != tPR {
						continue
					}
					reached = true
					if j != len(tables)-1 {
						t.Errorf("tick %d attempted %v: tables followed the deadline table", i, tables)
					}
				}
			}
			if !reached {
				t.Fatal("the deadline table was never attempted")
			}
		})
	}
}

func TestPassAdaptiveBatchProgressAndRegrowth(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	f := newFake()
	var limits []int32
	f.script[tPR] = func(attempt int, limit int32, _ context.Context) (int64, error) {
		limits = append(limits, limit)
		if attempt <= 2 {
			return 0, timeoutErr(tPR)
		}
		return int64(limit), nil
	}
	open, _ := openerOf(f)
	r := newRetention(open, time.Now, 24*time.Hour, 0)
	var deleted []int64
	for i := 0; i < 5; i++ {
		r.counter = 0 // pr_history_events leads each tick; the other tables are not under test
		n, _ := r.Pass(context.Background())
		deleted = append(deleted, n-21)
	}
	// 1000 and 500 time out; the table then grows back halfway toward 500 but stays below it.
	wantLimits := []int32{1000, 500, 250, 375, 437}
	for i := range wantLimits {
		if limits[i] != wantLimits[i] {
			t.Fatalf("limits = %v, want %v", limits, wantLimits)
		}
	}
	if deleted[0] != 0 || deleted[1] != 0 || deleted[2] != 250 || deleted[3] != 375 {
		t.Fatalf("deleted per tick = %v, want 0 0 250 375", deleted)
	}
	// Run past the hold: the batch stays below the timed-out 500 for 20 successful ticks, then
	// probes upward and is back at 1000.
	for i := 0; i < 30; i++ {
		r.counter = 0
		_, _ = r.Pass(context.Background())
	}
	back := -1
	for i, l := range limits {
		if i >= 2 && i < 22 && l >= 500 {
			t.Fatalf("limit %d at tick %d during the hold, want below the timed-out 500: %v", l, i+1, limits)
		}
		if i >= 2 && l == 1000 && back < 0 {
			back = i
		}
	}
	if back < 22 || back > 30 {
		t.Fatalf("batch returned to 1000 at index %d, want right after the 20-tick hold: %v", back, limits)
	}
	if strings.Contains(buf.String(), "repeated statement timeouts") {
		t.Fatalf("a warning was logged although the table made progress:\n%s", buf.String())
	}
}

// backlogSim drives tPR through Pass: each tick adds arrivals to the backlog, then the fake times
// out when the limit is at or above timeoutAt and otherwise deletes min(limit, backlog).
type backlogSim struct {
	backlog, arrived, deleted int64
	minBacklog                int64
}

func (s *backlogSim) run(t *testing.T, timeoutAt int32, start, arrivals int64, ticks int) {
	t.Helper()
	s.backlog, s.minBacklog = start, start
	f := newFake()
	f.script[tPR] = func(_ int, limit int32, _ context.Context) (int64, error) {
		if limit >= timeoutAt {
			return 0, timeoutErr(tPR)
		}
		n := min(int64(limit), s.backlog)
		s.backlog -= n
		s.deleted += n
		return n, nil
	}
	open, _ := openerOf(f)
	r := newRetention(open, time.Now, 24*time.Hour, 0)
	for i := 0; i < ticks; i++ {
		s.backlog += arrivals
		s.arrived += arrivals
		r.counter = 0
		_, _ = r.Pass(context.Background())
		s.minBacklog = min(s.minBacklog, s.backlog)
	}
}

func TestPassAdaptiveRecoverableBacklogDrains(t *testing.T) {
	// 1000 rows time out, anything smaller works: the pass must keep deleting near the limit
	// instead of alternating between 0 and 500 (about 250 rows per tick, no faster than arrivals).
	var s backlogSim
	s.run(t, 1000, 20000, 250, 200)
	if s.minBacklog != 0 {
		t.Fatalf("backlog never drained: min %d, end %d", s.minBacklog, s.backlog)
	}
	if s.deleted <= s.arrived {
		t.Fatalf("deleted %d <= arrived %d", s.deleted, s.arrived)
	}
}

func TestPassAdaptiveMixedTimeoutsDrain(t *testing.T) {
	var s backlogSim
	s.run(t, 500, 20000, 250, 200)
	if s.minBacklog != 0 {
		t.Fatalf("backlog never drained: min %d, end %d", s.minBacklog, s.backlog)
	}
	if s.deleted <= s.arrived {
		t.Fatalf("deleted %d <= arrived %d", s.deleted, s.arrived)
	}
}

func TestPassAdaptiveCannotDrainWarns(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	// Anything from 100 rows up times out and the backlog never empties, so every success is a
	// full batch below the maximum.
	var s backlogSim
	s.run(t, 100, 1_000_000, 250, 100)
	if !strings.Contains(buf.String(), "deleting below the maximum batch") {
		t.Fatalf("no behind warning after 100 ticks of full batches below the maximum:\n%s", buf.String())
	}
}

func TestPassAdaptiveFloorEpisodeRecovers(t *testing.T) {
	f := newFake()
	var limits []int32
	f.script[tPR] = func(attempt int, limit int32, _ context.Context) (int64, error) {
		limits = append(limits, limit)
		if attempt <= 8 { // down to the floor and a few timeouts at it
			return 0, timeoutErr(tPR)
		}
		return int64(limit), nil
	}
	open, _ := openerOf(f)
	r := newRetention(open, time.Now, 24*time.Hour, 0)
	for i := 0; i < 13; i++ {
		r.counter = 0
		_, _ = r.Pass(context.Background())
	}
	if limits[7] != 50 {
		t.Fatalf("limits = %v, want the floor 50 at the last timeout", limits)
	}
	// the first success is at the floor; within five successes the batch is well above it
	if limits[12] <= 400 {
		t.Fatalf("limit after five successes = %d, want above 400: %v", limits[12], limits)
	}
}

func TestPassAdaptiveFloorAndWarningOnFifthTimeout(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	f := newFake()
	var limits []int32
	f.script[tPR] = func(_ int, limit int32, _ context.Context) (int64, error) {
		limits = append(limits, limit)
		return 0, timeoutErr(tPR)
	}
	open, _ := openerOf(f)
	r := newRetention(open, time.Now, 24*time.Hour, 0)
	warned := func() int { return strings.Count(buf.String(), "repeated statement timeouts") }
	for i := 0; i < 6; i++ {
		r.counter = 0
		_, err := r.Pass(context.Background())
		if err == nil {
			t.Fatalf("tick %d: no error for a table that cannot make progress", i)
		}
		if i < 4 && warned() != 0 {
			t.Fatalf("warning after only %d timeouts", i+1)
		}
		if i == 4 && warned() != 1 {
			t.Fatalf("warnings after the fifth timeout = %d, want 1", warned())
		}
	}
	want := []int32{1000, 500, 250, 125, 62, 50}
	for i, w := range want {
		if limits[i] != w {
			t.Fatalf("limits = %v, want %v (floor 50)", limits, want)
		}
	}
	if warned() != 1 {
		t.Fatalf("warnings = %d after six timeouts, want exactly 1 (next at the tenth)", warned())
	}
}

func TestPassLockTimeoutDoesNotShrinkBatch(t *testing.T) {
	f := newFake()
	var limits []int32
	f.script[tPR] = func(_ int, limit int32, _ context.Context) (int64, error) {
		limits = append(limits, limit)
		return 0, rollback(tPR, "55P03")
	}
	open, _ := openerOf(f)
	r := newRetention(open, time.Now, 24*time.Hour, 0)
	for i := 0; i < 3; i++ {
		r.counter = 0
		_, _ = r.Pass(context.Background())
	}
	for _, l := range limits {
		if l != 1000 {
			t.Fatalf("limits = %v, want a lock timeout to leave the batch at 1000", limits)
		}
	}
}
