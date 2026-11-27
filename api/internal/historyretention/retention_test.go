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

func TestPassReturnsWithinBudgetWithBlockingSession(t *testing.T) {
	f := newFake()
	f.script[tPR] = func(_ int, _ int32, ctx context.Context) (int64, error) {
		<-ctx.Done()
		f.idle = false
		return 0, ctx.Err()
	}
	open, _ := openerOf(f)
	r := newRetention(open, time.Now, 24*time.Hour, 0)
	begin := time.Now()
	n, err := r.Pass(context.Background())
	elapsed := time.Since(begin)
	if n != 0 || err == nil || f.closed != 1 {
		t.Fatalf("n=%d err=%v closed=%d, want 0, an error and Close", n, err, f.closed)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("blocked pass returned after %v, want within the 2s client budget", elapsed)
	}
	if elapsed < 1500*time.Millisecond {
		t.Fatalf("blocked pass returned after %v, before the work deadline: the block was not exercised", elapsed)
	}
}

func TestPassStarvationOverManyTicks(t *testing.T) {
	// Four failure shapes, one per table, over 12 ticks that alternate between two instances
	// (each with its own rotation counter): every table must still be attempted as the leading
	// table at least once, and the healthy table must make progress in every tick that reaches it.
	type shape struct {
		table store.HistoryRetentionTable
		fn    func(int, int32, context.Context) (int64, error)
	}
	shapes := []shape{
		{tPR, func(int, int32, context.Context) (int64, error) { return 0, rollback(tPR, "55P03") }},
		{tAnth, func(_ int, _ int32, ctx context.Context) (int64, error) { return 0, timeoutErr(tAnth) }},
		{tCodex, func(int, int32, context.Context) (int64, error) { return 0, errors.New("connection reset") }},
		{tCap, nil},
	}
	leading := map[store.HistoryRetentionTable]int{}
	attempted := map[store.HistoryRetentionTable]int{}
	instances := []*Retention{nil, nil}
	var sessions []*fakeSession
	opener := func(context.Context, context.Context) (session, bool, error) {
		f := newFake()
		for _, s := range shapes {
			if s.fn != nil {
				f.script[s.table] = s.fn
			}
		}
		sessions = append(sessions, f)
		return f, true, nil
	}
	instances[0] = newRetention(opener, time.Now, 24*time.Hour, 1)
	instances[1] = newRetention(opener, time.Now, 24*time.Hour, 3)
	for tick := 0; tick < 12; tick++ {
		// instance 0 wins ticks 0,1,2,3,... except where instance 1 wins: alternate unevenly
		inst := instances[0]
		if tick%3 == 2 {
			inst = instances[1]
		}
		_, _ = inst.Pass(context.Background())
		last := sessions[len(sessions)-1]
		if len(last.calls) > 0 {
			leading[last.calls[0].table]++
		}
		for _, c := range last.calls {
			attempted[c.table]++
		}
	}
	for _, tb := range baseOrder {
		if leading[tb] == 0 {
			t.Errorf("%s never led a tick over 12 ticks (leading=%v)", tb, leading)
		}
	}
	// the tick ends at the connection error, so a table after it is reached only when it leads
	// or sits before the failing table; the healthy table must at least be reached whenever it leads
	if attempted[tCap] < leading[tCap] || attempted[tCap] < 2 {
		t.Errorf("healthy table attempted %d times over 12 ticks (led %d), want it reached every time it leads (attempted=%v)", attempted[tCap], leading[tCap], attempted)
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
	wantLimits := []int32{1000, 500, 250, 500, 1000}
	for i := range wantLimits {
		if limits[i] != wantLimits[i] {
			t.Fatalf("limits = %v, want %v", limits, wantLimits)
		}
	}
	if deleted[0] != 0 || deleted[1] != 0 || deleted[2] != 250 {
		t.Fatalf("deleted per tick = %v, want 0 0 250 on the third attempt", deleted)
	}
	if strings.Contains(buf.String(), "repeated statement timeouts") {
		t.Fatalf("a warning was logged although the table made progress:\n%s", buf.String())
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
