package releasecheck

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunnerInitialWait(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, stamp string
		err         error
		want        time.Duration
	}{
		{"overdue", now.Add(-7 * time.Hour).Format(time.RFC3339), nil, time.Minute},
		{"recent", now.Add(-2 * time.Hour).Format(time.RFC3339), nil, 4 * time.Hour},
		{"missing", "", nil, time.Minute},
		{"read error", now.Format(time.RFC3339), errors.New("read failed"), time.Minute},
		{"malformed", "yesterday", nil, time.Minute},
		{"future", now.Add(time.Hour).Format(time.RFC3339), nil, 6 * time.Hour},
		{"equality", now.Add(-6 * time.Hour).Format(time.RFC3339), nil, time.Minute},
		{"remainder below boot delay", now.Add(-6*time.Hour + 30*time.Second).Format(time.RFC3339), nil, time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newManualWait()
			set := &fakeSettings{interval: 6 * time.Hour, checkedAt: tc.stamp, checkedErr: tc.err}
			var calls atomic.Int64
			rn := &Runner{settings: set, now: func() time.Time { return now }, wait: w.wait,
				logger: quietLogger(), check: checkerFunc(func(context.Context) (Result, error) {
					calls.Add(1)
					return Result{Status: statusOK}, nil
				})}
			stop := startRunner(t, rn)
			first := w.next(t)
			if first.delay != tc.want {
				t.Fatalf("first wait = %v, want %v", first.delay, tc.want)
			}
			if calls.Load() != 0 {
				t.Fatal("check ran before initial wait released")
			}
			close(first.release)
			second := w.next(t)
			if second.delay != 6*time.Hour {
				t.Fatalf("second wait = %v, want 6h", second.delay)
			}
			if calls.Load() != 1 || set.checkedReads.Load() != 1 {
				t.Fatalf("calls=%d checked-at reads=%d, want 1 each", calls.Load(), set.checkedReads.Load())
			}
			stop()
		})
	}
}

func TestRunnerFullIntervalAfterEveryOutcome(t *testing.T) {
	for _, outcome := range []string{statusOK, "returned error", statusError, statusDisabled, "panic"} {
		t.Run(outcome, func(t *testing.T) {
			w := newManualWait()
			set := &fakeSettings{interval: 3 * time.Hour}
			var calls atomic.Int64
			rn := &Runner{settings: set, wait: w.wait, logger: quietLogger(),
				check: checkerFunc(func(context.Context) (Result, error) {
					calls.Add(1)
					switch outcome {
					case "panic":
						panic("boom")
					case "returned error":
						return Result{}, errors.New("failed")
					default:
						return Result{Status: outcome}, nil
					}
				})}
			startRunner(t, rn)
			first := w.next(t)
			if first.delay != time.Minute {
				t.Fatalf("first wait = %v, want 1m", first.delay)
			}
			close(first.release)
			second := w.next(t)
			if second.delay != 3*time.Hour {
				t.Fatalf("second wait = %v, want 3h", second.delay)
			}
			close(second.release)
			third := w.next(t)
			if third.delay != 3*time.Hour || calls.Load() != 2 || set.checkedReads.Load() != 1 {
				t.Fatalf("third wait=%v calls=%d checked-at reads=%d", third.delay, calls.Load(), set.checkedReads.Load())
			}
		})
	}
}

func TestRunnerCadenceReadAfterCompletion(t *testing.T) {
	w := newManualWait()
	entered, complete := make(chan struct{}), make(chan struct{})
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	set := &fakeSettings{interval: 6 * time.Hour, checkedAt: now.Add(-7 * time.Hour).Format(time.RFC3339)}
	rn := &Runner{settings: set, wait: w.wait, now: func() time.Time { return now }, logger: quietLogger(),
		check: checkerFunc(func(ctx context.Context) (Result, error) {
			close(entered)
			select {
			case <-complete:
			case <-ctx.Done():
				return Result{}, ctx.Err()
			}
			set.interval = 2 * time.Hour
			now = now.Add(24 * time.Hour)
			return Result{Status: statusOK}, nil
		})}
	startRunner(t, rn)
	first := w.next(t)
	close(first.release)
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("check did not start")
	}
	if set.intervalReads.Load() != 1 {
		t.Fatal("interval reread before check completed")
	}
	select {
	case <-w.requests:
		t.Fatal("wait requested while check still running")
	default:
	}
	close(complete)
	second := w.next(t)
	if second.delay != 2*time.Hour || set.intervalReads.Load() != 2 || set.checkedReads.Load() != 1 {
		t.Fatalf("after completion wait=%v interval reads=%d checked-at reads=%d", second.delay, set.intervalReads.Load(), set.checkedReads.Load())
	}
}

func TestRunnerCancellation(t *testing.T) {
	for _, phase := range []string{"initial wait", "later wait", "check"} {
		t.Run(phase, func(t *testing.T) {
			w := newManualWait()
			var calls atomic.Int64
			entered := make(chan struct{})
			rn := &Runner{settings: &fakeSettings{interval: time.Hour}, wait: w.wait, logger: quietLogger(),
				check: checkerFunc(func(ctx context.Context) (Result, error) {
					calls.Add(1)
					if phase == "check" {
						close(entered)
						<-ctx.Done()
						return Result{}, ctx.Err()
					}
					return Result{Status: statusOK}, nil
				})}
			stop := startRunner(t, rn)
			first := w.next(t)
			if phase != "initial wait" {
				close(first.release)
				if phase == "check" {
					select {
					case <-entered:
					case <-time.After(2 * time.Second):
						t.Fatal("check did not start")
					}
				} else {
					w.next(t)
				}
			}
			stop()
			want := int64(1)
			if phase == "initial wait" {
				want = 0
			}
			if calls.Load() != want {
				t.Fatalf("calls=%d, want %d", calls.Load(), want)
			}
		})
	}
}

func TestRunnerDefaultsAndRealTimerCancellation(t *testing.T) {
	for _, delay := range []time.Duration{0, -time.Second} {
		w := newManualWait()
		rn := &Runner{settings: &fakeSettings{interval: time.Hour}, bootDelay: delay, wait: w.wait}
		startRunner(t, rn)
		if got := w.next(t).delay; got != time.Minute {
			t.Fatalf("default boot wait=%v, want 1m", got)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var calls atomic.Int64
	rn := &Runner{settings: &fakeSettings{interval: time.Hour}, check: checkerFunc(func(context.Context) (Result, error) {
		calls.Add(1)
		return Result{}, nil
	})}
	rn.Start(ctx)
	if calls.Load() != 0 {
		t.Fatal("default Runner checked after cancellation")
	}
}

func TestRunnerRealTimerInvokesCheck(t *testing.T) {
	checked := make(chan struct{}, 1)
	rn := &Runner{
		settings:  &fakeSettings{interval: time.Hour},
		bootDelay: time.Nanosecond,
		logger:    quietLogger(),
		check: checkerFunc(func(context.Context) (Result, error) {
			checked <- struct{}{}
			return Result{Status: statusOK}, nil
		}),
	}
	stop := startRunner(t, rn)
	select {
	case <-checked:
	case <-time.After(2 * time.Second):
		t.Fatal("real timer elapsed without invoking the scheduled check")
	}
	stop()
}

func TestRunnerPositiveIntervalAndBootDelay(t *testing.T) {
	w := newManualWait()
	set := &fakeSettings{interval: 30 * time.Second}
	rn := &Runner{settings: set, bootDelay: 2 * time.Minute, wait: w.wait,
		check: checkerFunc(func(context.Context) (Result, error) { return Result{Status: statusDisabled}, nil })}
	startRunner(t, rn)
	first := w.next(t)
	if first.delay != 2*time.Minute {
		t.Fatalf("injected boot delay=%v, want 2m", first.delay)
	}
	close(first.release)
	if got := w.next(t).delay; got != 30*time.Second {
		t.Fatalf("positive cadence=%v, want 30s", got)
	}
}

func TestRunnerFloorsNonPositiveInterval(t *testing.T) {
	for _, tc := range []struct{ input, want time.Duration }{
		{0, time.Hour}, {-time.Second, time.Hour}, {time.Millisecond, time.Millisecond}, {3 * time.Hour, 3 * time.Hour},
	} {
		w := newManualWait()
		rn := &Runner{settings: &fakeSettings{interval: tc.input}, wait: w.wait,
			check:  checkerFunc(func(context.Context) (Result, error) { return Result{Status: statusDisabled}, nil }),
			logger: quietLogger()}
		stop := startRunner(t, rn)
		first := w.next(t)
		close(first.release)
		if got := w.next(t).delay; got != tc.want {
			t.Errorf("configured interval %v produced wait %v, want %v", tc.input, got, tc.want)
		}
		stop()
	}
}
