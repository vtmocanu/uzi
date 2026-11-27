package workersvc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/store"
)

type fakeCapacityClock struct{ t time.Time }

func (c *fakeCapacityClock) now() time.Time { return c.t }

type fakeCapacitySession struct {
	clock      *fakeCapacityClock
	rows       []store.SampleWorkerCapacityBatchRow
	errAt      int // 1-based batch number that fails; 0 = none
	advance    time.Duration
	params     []store.SampleWorkerCapacityBatchParams
	calls      int
	closes     int
	closeErr   error
	batchCtxDL []time.Time
	closeCtxDL time.Time
}

func (f *fakeCapacitySession) SampleBatch(ctx context.Context, arg store.SampleWorkerCapacityBatchParams) (store.SampleWorkerCapacityBatchRow, error) {
	f.calls++
	f.params = append(f.params, arg)
	if dl, ok := ctx.Deadline(); ok {
		f.batchCtxDL = append(f.batchCtxDL, dl)
	}
	f.clock.t = f.clock.t.Add(f.advance)
	if f.errAt == f.calls {
		return store.SampleWorkerCapacityBatchRow{}, errors.New("boom")
	}
	if f.calls > len(f.rows) {
		return store.SampleWorkerCapacityBatchRow{}, nil
	}
	return f.rows[f.calls-1], nil
}

func (f *fakeCapacitySession) Close(ctx context.Context) (bool, error) {
	f.closes++
	f.closeCtxDL, _ = ctx.Deadline()
	return f.closeErr == nil, f.closeErr
}

func full(inserted int64) store.SampleWorkerCapacityBatchRow {
	return store.SampleWorkerCapacityBatchRow{Scanned: 1000, Inserted: inserted}
}

func newTestSampler(t *testing.T, sess *fakeCapacitySession, acquired bool, stale time.Duration) *CapacitySampler {
	t.Helper()
	open := func(ctx context.Context) (capacitySession, bool, error) {
		if !acquired {
			return nil, false, nil
		}
		return sess, true, nil
	}
	s, err := newCapacitySampler(open, sess.clock.now, stale)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func newFake() *fakeCapacitySession {
	return &fakeCapacitySession{clock: &fakeCapacityClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}}
}

func TestCapacitySamplerRunsUpToBudgetWhileBatchesFill(t *testing.T) {
	f := newFake()
	f.rows = []store.SampleWorkerCapacityBatchRow{full(1), full(2), full(3), full(4), full(5)}
	got, err := newTestSampler(t, f, true, time.Minute).Pass(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if f.calls != 4 || got != 10 || f.closes != 1 {
		t.Fatalf("calls=%d inserted=%d closes=%d, want 4/10/1", f.calls, got, f.closes)
	}
}

func TestCapacitySamplerStopsOnShortBatch(t *testing.T) {
	f := newFake()
	f.rows = []store.SampleWorkerCapacityBatchRow{full(7), {Scanned: 999, Inserted: 3}, full(9)}
	got, err := newTestSampler(t, f, true, time.Minute).Pass(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if f.calls != 2 || got != 10 {
		t.Fatalf("calls=%d inserted=%d, want 2/10", f.calls, got)
	}
}

func TestCapacitySamplerStopsAtWorkDeadline(t *testing.T) {
	f := newFake()
	f.rows = []store.SampleWorkerCapacityBatchRow{full(1), full(1), full(1), full(1)}
	// Work deadline is 2s - 250ms = 1.75s; each batch consumes 1s.
	f.advance = time.Second
	_, err := newTestSampler(t, f, true, time.Minute).Pass(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if f.calls != 2 || f.closes != 1 {
		t.Fatalf("calls=%d closes=%d, want 2/1", f.calls, f.closes)
	}
}

func TestCapacitySamplerContextDeadlines(t *testing.T) {
	f := newFake()
	f.rows = []store.SampleWorkerCapacityBatchRow{{}}
	if _, err := newTestSampler(t, f, true, time.Minute).Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Context deadlines are built from the injected clock (already past in real time; the fake
	// session ignores ctx), so assert only the gap between them.
	if len(f.batchCtxDL) != 1 {
		t.Fatalf("batch deadlines = %d", len(f.batchCtxDL))
	}
	if gap := f.closeCtxDL.Sub(f.batchCtxDL[0]); gap != capacitySampleCleanupReserve {
		t.Fatalf("close deadline - batch deadline = %s, want %s", gap, capacitySampleCleanupReserve)
	}
}

func TestCapacitySamplerEndsAtFirstFailedBatch(t *testing.T) {
	f := newFake()
	f.rows = []store.SampleWorkerCapacityBatchRow{full(5), full(5), full(5)}
	f.errAt = 2
	got, err := newTestSampler(t, f, true, time.Minute).Pass(context.Background())
	if err == nil {
		t.Fatal("want error")
	}
	if f.calls != 2 || got != 5 || f.closes != 1 {
		t.Fatalf("calls=%d inserted=%d closes=%d, want 2/5/1", f.calls, got, f.closes)
	}
}

func TestCapacitySamplerJoinsBatchAndCloseErrors(t *testing.T) {
	f := newFake()
	f.errAt = 1
	f.closeErr = errors.New("unlock failed")
	_, err := newTestSampler(t, f, true, time.Minute).Pass(context.Background())
	if err == nil || f.closes != 1 {
		t.Fatalf("err=%v closes=%d", err, f.closes)
	}
	if !errors.Is(err, f.closeErr) {
		t.Fatalf("close error not joined: %v", err)
	}
}

func TestCapacitySamplerReturnsCloseErrorAfterCleanPass(t *testing.T) {
	f := newFake()
	f.rows = []store.SampleWorkerCapacityBatchRow{{Scanned: 1, Inserted: 1}}
	f.closeErr = errors.New("unlock failed")
	got, err := newTestSampler(t, f, true, time.Minute).Pass(context.Background())
	if !errors.Is(err, f.closeErr) || got != 1 {
		t.Fatalf("got=%d err=%v", got, err)
	}
}

func TestCapacitySamplerLockNotAcquiredIsQuiet(t *testing.T) {
	f := newFake()
	got, err := newTestSampler(t, f, false, time.Minute).Pass(context.Background())
	if err != nil || got != 0 || f.calls != 0 || f.closes != 0 {
		t.Fatalf("got=%d err=%v calls=%d closes=%d", got, err, f.calls, f.closes)
	}
}

func TestCapacitySamplerOpenErrorIsReturned(t *testing.T) {
	want := errors.New("open failed")
	s, err := newCapacitySampler(func(context.Context) (capacitySession, bool, error) { return nil, false, want }, time.Now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.Pass(context.Background()); got != 0 || !errors.Is(err, want) {
		t.Fatalf("got=%d err=%v", got, err)
	}
}

func TestCapacitySamplerIntervalAndObservedAt(t *testing.T) {
	f := newFake()
	f.rows = []store.SampleWorkerCapacityBatchRow{{}}
	if _, err := newTestSampler(t, f, true, 45500*time.Millisecond).Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.params) != 1 {
		t.Fatalf("batches = %d", len(f.params))
	}
	p := f.params[0]
	if p.StaleAfter.Microseconds != 45_500_000 || p.StaleAfter.Days != 0 || p.StaleAfter.Months != 0 || !p.StaleAfter.Valid {
		t.Fatalf("StaleAfter = %+v", p.StaleAfter)
	}
	if p.ObservedAt.Valid {
		t.Fatalf("ObservedAt must be NULL, got %+v", p.ObservedAt)
	}
	if p.BatchSize != 1000 {
		t.Fatalf("BatchSize = %d", p.BatchSize)
	}
}

func TestCapacitySamplerRefusesNonPositiveStaleWindow(t *testing.T) {
	for _, d := range []time.Duration{0, -time.Second} {
		if _, err := newCapacitySampler(nil, time.Now, d); err == nil {
			t.Fatalf("stale %s accepted", d)
		}
	}
}
