package store_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// Shared by the activity probe and the VALUES regression. Even with a matching
// blocker, an idle backend's nullable wait fields must scan as false.
const codexLockPredicate = "COALESCE($2::int = ANY(blocking_pids) AND wait_event_type = 'Lock' AND wait_event = 'advisory', false)"

func scanCodexLock(row pgx.Row) (bool, string, string, error) {
	var blocked bool
	var waitType, waitEvent string
	err := row.Scan(&blocked, &waitType, &waitEvent)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, "", "", nil
	}
	return blocked, waitType, waitEvent, err
}

type codexLockResult struct {
	n   int64
	err error
}

type codexLockWriter struct {
	result   chan codexLockResult
	complete chan struct{}
	cancel   context.CancelFunc
}

func startCodexLockWriter(ctx context.Context, write func(context.Context) (int64, error)) codexLockWriter {
	wctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	w := codexLockWriter{
		result: make(chan codexLockResult, 1), complete: make(chan struct{}), cancel: cancel,
	}
	go func() {
		n, err := write(wctx)
		w.result <- codexLockResult{n, err}
		close(w.complete)
	}()
	return w
}

// The writer's context bounds its database operation; joining deliberately has no
// timeout so connection release cannot race its final use. Cleanup never drains
// result: the probe or final wait may already have consumed it.
func (w codexLockWriter) stop() {
	w.cancel()
	<-w.complete
}

func waitCodexLockResult(ctx context.Context, result <-chan codexLockResult) (codexLockResult, error) {
	select {
	case r := <-result:
		return r, nil
	case <-ctx.Done():
		return codexLockResult{}, fmt.Errorf("waiting for fenced write: %w", ctx.Err())
	}
}

// One probe failure stops this subtest. The caller's deadline bounds queries and
// retries; the ticker only spaces retries, and writer completion is checked even
// when its activity row has disappeared.
func probeCodexLock(ctx context.Context, query func(context.Context) pgx.Row, result <-chan codexLockResult) error {
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	var waitType, waitEvent string
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("the fenced write never waited on the advisory lock (last wait %s/%s): %w", waitType, waitEvent, err)
		}
		blocked, wt, we, err := scanCodexLock(query(ctx))
		waitType, waitEvent = wt, we
		if err != nil {
			return fmt.Errorf("probe fenced write: %w", err)
		}
		if blocked {
			return nil
		}
		select {
		case r := <-result:
			return fmt.Errorf("the fenced write did not wait for the secret-mutation lock: (%d, %v)", r.n, r.err)
		default:
		}
		select {
		case r := <-result:
			return fmt.Errorf("the fenced write did not wait for the secret-mutation lock: (%d, %v)", r.n, r.err)
		case <-ctx.Done():
			return fmt.Errorf("the fenced write never waited on the advisory lock (last wait %s/%s): %w", waitType, waitEvent, ctx.Err())
		case <-tick.C:
		}
	}
}

type codexLockRow func(...any) error

func (row codexLockRow) Scan(dest ...any) error { return row(dest...) }

func TestCodexLockScan(t *testing.T) {
	scanError := errors.New("scan failure")
	for _, tc := range []struct {
		name string
		err  error
		want error
	}{
		{"success", nil, nil},
		{"no rows", pgx.ErrNoRows, nil},
		{"wrapped no rows", fmt.Errorf("query: %w", pgx.ErrNoRows), nil},
		{"scan error", scanError, scanError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blocked, wt, we, err := scanCodexLock(codexLockRow(func(dest ...any) error {
				if tc.err != nil {
					// Even partial destination writes must not survive ErrNoRows.
					*(dest[0].(*bool)) = true
					*(dest[1].(*string)) = "partial"
					return tc.err
				}
				*(dest[0].(*bool)) = true
				*(dest[1].(*string)) = "Lock"
				*(dest[2].(*string)) = "advisory"
				return nil
			}))
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			if tc.err == nil && (!blocked || wt != "Lock" || we != "advisory") {
				t.Fatalf("successful scan = (%v, %q, %q)", blocked, wt, we)
			}
			if errors.Is(tc.err, pgx.ErrNoRows) && (blocked || wt != "" || we != "") {
				t.Fatalf("missing row = (%v, %q, %q)", blocked, wt, we)
			}
		})
	}
}

func TestCodexLockProbe(t *testing.T) {
	t.Run("missing row reports writer outcome", func(t *testing.T) {
		writerError := errors.New("writer failure")
		w := startCodexLockWriter(context.Background(), func(context.Context) (int64, error) {
			return 7, writerError
		})
		defer w.stop()
		<-w.complete
		err := probeCodexLock(context.Background(), func(context.Context) pgx.Row {
			return codexLockRow(func(...any) error { return pgx.ErrNoRows })
		}, w.result)
		if err == nil || !strings.Contains(err.Error(), "(7, writer failure)") {
			t.Fatalf("probe error = %v, want writer outcome", err)
		}
	})
	t.Run("query error", func(t *testing.T) {
		queryError := errors.New("query failure")
		err := probeCodexLock(context.Background(), func(context.Context) pgx.Row {
			return codexLockRow(func(...any) error { return queryError })
		}, make(chan codexLockResult, 1))
		if !errors.Is(err, queryError) {
			t.Fatalf("probe error = %v", err)
		}
	})
	t.Run("advisory wait", func(t *testing.T) {
		if err := probeCodexLock(context.Background(), func(context.Context) pgx.Row {
			return codexLockRow(func(dest ...any) error {
				*(dest[0].(*bool)) = true
				return nil
			})
		}, make(chan codexLockResult, 1)); err != nil {
			t.Fatal(err)
		}
	})
}

func TestCodexLockContexts(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(fmt.Sprintf("expired=%v", expired), func(t *testing.T) {
			newContext := func() (context.Context, context.CancelFunc) {
				if expired {
					return context.WithDeadline(context.Background(), time.Time{})
				}
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx, cancel
			}
			want := context.Canceled
			if expired {
				want = context.DeadlineExceeded
			}
			ctx, cancel := newContext()
			defer cancel()
			err := probeCodexLock(ctx, func(context.Context) pgx.Row {
				t.Fatal("query started with a finished probe context")
				return nil
			}, make(chan codexLockResult, 1))
			if !errors.Is(err, want) {
				t.Fatalf("probe = %v, want %v", err, want)
			}
			// A separate context bounds the final wait even after a successful probe.
			final, finalCancel := newContext()
			defer finalCancel()
			_, err = waitCodexLockResult(final, make(chan codexLockResult, 1))
			if !errors.Is(err, want) {
				t.Fatalf("final wait = %v, want %v", err, want)
			}
		})
	}
	t.Run("cancel during query", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		err := probeCodexLock(ctx, func(qctx context.Context) pgx.Row {
			if qctx != ctx {
				t.Fatal("query did not receive bounded probe context")
			}
			cancel()
			return codexLockRow(func(...any) error { return qctx.Err() })
		}, make(chan codexLockResult, 1))
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("probe query = %v", err)
		}
	})
}

func TestCodexLockWriterJoin(t *testing.T) {
	for _, mode := range []string{"pending", "completed", "consumed"} {
		t.Run(mode, func(t *testing.T) {
			ready := make(chan struct{})
			proceed := make(chan struct{})
			w := startCodexLockWriter(context.Background(), func(ctx context.Context) (int64, error) {
				close(ready)
				if mode == "pending" {
					<-ctx.Done()
					return 9, ctx.Err()
				}
				<-proceed
				return 9, nil
			})
			actualComplete := w.complete
			<-ready
			if mode != "pending" {
				close(proceed)
				<-actualComplete
			}
			if mode == "consumed" {
				<-w.result
			}

			notification := make(chan struct{})
			w.complete = notification
			stopReturned := make(chan struct{})
			verdict := make(chan bool)
			forwarded := make(chan struct{})
			go func() {
				defer close(forwarded)
				<-actualComplete
				// Only stop receives notification. Without its join, this send
				// cannot rendezvous, regardless of when the writer finishes.
				select {
				case notification <- struct{}{}:
					verdict <- true
				case <-stopReturned:
					verdict <- false
				}
			}()

			w.stop()
			close(stopReturned)
			joined := <-verdict
			<-forwarded
			// Diagnostics join the real writer, never the notification transport.
			<-actualComplete
			if !joined {
				t.Fatal("cleanup returned without joining completion")
			}
		})
	}
}

func TestCodexLockWriterLifecycle(t *testing.T) {
	for _, mode := range []string{"pending", "completed", "consumed", "error", "canceled", "final wait canceled", "probe expired", "probe error", "goexit"} {
		t.Run(mode, func(t *testing.T) {
			events := make(chan string, 8)
			finished := make(chan struct{})
			ready := make(chan struct{})
			proceed := make(chan struct{})
			go func() {
				defer close(finished)
				defer func() { events <- "rollback" }()
				defer func() { events <- "release" }()
				parent, cancel := context.WithCancel(context.Background())
				defer cancel()
				w := startCodexLockWriter(parent, func(ctx context.Context) (int64, error) {
					defer func() { events <- "final connection use" }()
					if _, ok := ctx.Deadline(); !ok {
						events <- "missing writer deadline"
					}
					close(ready)
					switch mode {
					case "completed", "consumed", "error":
						<-proceed
					default:
						<-ctx.Done()
						events <- "cancellation"
					}
					if mode == "error" {
						return 9, errors.New("write failed")
					}
					return 9, nil
				})
				defer func() {
					w.stop()
					select {
					case <-w.complete:
					default:
						events <- "cleanup returned before completion"
					}
					// Independently join for safe diagnostics if stop loses its join.
					<-w.complete
					events <- "completion"
					if mode != "consumed" && mode != "error" {
						select {
						case r := <-w.result:
							if r.n != 9 {
								events <- "wrong buffered result"
							}
						default:
							events <- "cleanup drained result"
						}
					}
				}()
				<-ready
				switch mode {
				case "completed", "consumed", "error":
					close(proceed)
					<-w.complete
				case "canceled":
					cancel()
				}
				if mode == "consumed" || mode == "error" {
					r, err := waitCodexLockResult(context.Background(), w.result)
					if err != nil || r.n != 9 || (mode == "error" && (r.err == nil || r.err.Error() != "write failed")) {
						events <- "wrong result"
					}
				}
				switch mode {
				case "final wait canceled":
					waitCtx, waitCancel := context.WithCancel(context.Background())
					waitCancel()
					if _, err := waitCodexLockResult(waitCtx, w.result); !errors.Is(err, context.Canceled) {
						events <- "wrong final wait error"
					}
				case "probe expired":
					probeCtx, probeCancel := context.WithDeadline(context.Background(), time.Time{})
					defer probeCancel()
					if err := probeCodexLock(probeCtx, func(context.Context) pgx.Row {
						events <- "unexpected query"
						return codexLockRow(func(...any) error { return pgx.ErrNoRows })
					}, w.result); !errors.Is(err, context.DeadlineExceeded) {
						events <- "wrong probe error"
					}
				case "probe error":
					queryError := errors.New("query failure")
					if err := probeCodexLock(context.Background(), func(context.Context) pgx.Row {
						return codexLockRow(func(...any) error { return queryError })
					}, w.result); !errors.Is(err, queryError) {
						events <- "wrong probe error"
					}
				case "goexit":
					runtime.Goexit() // t.Fatal's deferred-cleanup path without failing this test.
				}
			}()
			select {
			case <-finished:
			case <-time.After(5 * time.Second):
				t.Fatal("writer cleanup deadlocked")
			}
			close(events)
			var got []string
			for event := range events {
				got = append(got, event)
			}
			want := []string{"final connection use", "completion", "release", "rollback"}
			if mode != "completed" && mode != "consumed" && mode != "error" {
				want = append([]string{"cancellation"}, want...)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("lifecycle = %v, want %v", got, want)
			}
		})
	}
}

func TestCodexLockPredicateLiveDB(t *testing.T) {
	ctx, pool, _, _ := codexLiveDB(t)
	for _, tc := range []struct {
		name      string
		waitType  any
		waitEvent any
		blocker   int32
		want      bool
		wantType  string
		wantEvent string
	}{
		{"both null", nil, nil, 42, false, "", ""},
		{"type null", nil, "advisory", 42, false, "", "advisory"},
		{"event null", "Lock", nil, 42, false, "Lock", ""},
		{"advisory", "Lock", "advisory", 42, true, "Lock", "advisory"},
		{"wrong blocker", "Lock", "advisory", 43, false, "Lock", "advisory"},
		{"wrong event", "Lock", "transactionid", 42, false, "Lock", "transactionid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blocked, wt, we, err := scanCodexLock(pool.QueryRow(ctx, `SELECT `+codexLockPredicate+`,
				COALESCE(wait_event_type, ''), COALESCE(wait_event, '')
				FROM (VALUES ($1::text, $3::text, ARRAY[$4::int]))
					AS activity(wait_event_type, wait_event, blocking_pids)`,
				tc.waitType, int32(42), tc.waitEvent, tc.blocker))
			if err != nil || blocked != tc.want || wt != tc.wantType || we != tc.wantEvent {
				t.Fatalf("probe = (%v, %q, %q, %v), want (%v, %q, %q, nil)",
					blocked, wt, we, err, tc.want, tc.wantType, tc.wantEvent)
			}
		})
	}
}
