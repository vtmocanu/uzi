package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Issue #4684: unit tests for RunAnalyticsSnapshot and ClassifyAnalyticsErr with a fake
// transaction (no database).

type fakeAnalyticsTx struct {
	pgx.Tx        // nil: any method the helper should not call panics
	execErr       error
	rollbacks     atomic.Int32
	rollbackCtxOK atomic.Bool
}

func (f *fakeAnalyticsTx) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, f.execErr
}

func (f *fakeAnalyticsTx) Rollback(ctx context.Context) error {
	f.rollbacks.Add(1)
	f.rollbackCtxOK.Store(ctx.Err() == nil)
	return nil
}

type fakeAnalyticsBeginner struct {
	tx       *fakeAnalyticsTx
	beginErr error
	opts     pgx.TxOptions

	beginDeadline    time.Time
	beginHasDeadline bool
}

func (b *fakeAnalyticsBeginner) BeginTx(ctx context.Context, o pgx.TxOptions) (pgx.Tx, error) {
	b.opts = o
	b.beginDeadline, b.beginHasDeadline = ctx.Deadline()
	if b.beginErr != nil {
		return nil, b.beginErr
	}
	return b.tx, nil
}

func TestRunAnalyticsSnapshotOptionsAndDeadline(t *testing.T) {
	tx := &fakeAnalyticsTx{}
	b := &fakeAnalyticsBeginner{tx: tx}
	start := time.Now()
	var dl time.Time
	err := RunAnalyticsSnapshot(context.Background(), b, func(ctx context.Context, s *AnalyticsSnapshot) error {
		var ok bool
		dl, ok = ctx.Deadline()
		if !ok {
			t.Fatal("fn context has no deadline")
		}
		if s.Q == nil || s.tx == nil {
			t.Fatal("snapshot not populated")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if end := time.Now(); dl.After(end.Add(AnalyticsSnapshotDeadline)) || dl.Before(start.Add(AnalyticsSnapshotDeadline-time.Second)) {
		t.Fatalf("fn deadline %v not within %v of the call", dl, AnalyticsSnapshotDeadline)
	}
	if b.opts.IsoLevel != pgx.RepeatableRead || b.opts.AccessMode != pgx.ReadOnly {
		t.Fatalf("tx options: %+v", b.opts)
	}
	if n := tx.rollbacks.Load(); n != 1 {
		t.Fatalf("rollbacks = %d", n)
	}
}

func TestRunAnalyticsSnapshotSharedDeadline(t *testing.T) {
	b := &fakeAnalyticsBeginner{tx: &fakeAnalyticsTx{}}
	var fnDeadline time.Time
	var fnHasDeadline, ran bool
	err := RunAnalyticsSnapshot(context.Background(), b, func(ctx context.Context, _ *AnalyticsSnapshot) error {
		ran = true
		fnDeadline, fnHasDeadline = ctx.Deadline()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !ran {
		t.Fatal("callback did not run")
	}
	if !b.beginHasDeadline || !fnHasDeadline {
		t.Fatalf("deadline present: BeginTx %v, callback %v, want both", b.beginHasDeadline, fnHasDeadline)
	}
	// One budget covers BeginTx and the callback: a fresh timeout for the callback would
	// give it a later deadline.
	if !b.beginDeadline.Equal(fnDeadline) {
		t.Fatalf("BeginTx deadline %v != callback deadline %v: not one shared budget", b.beginDeadline, fnDeadline)
	}
}

func TestRunAnalyticsSnapshotBeginErrorNoRollback(t *testing.T) {
	want := errors.New("begin failed")
	tx := &fakeAnalyticsTx{}
	err := RunAnalyticsSnapshot(context.Background(), &fakeAnalyticsBeginner{tx: tx, beginErr: want},
		func(context.Context, *AnalyticsSnapshot) error { t.Fatal("fn ran"); return nil })
	if !errors.Is(err, want) {
		t.Fatalf("err = %v", err)
	}
	if tx.rollbacks.Load() != 0 {
		t.Fatal("rollback without a transaction")
	}
}

func TestRunAnalyticsSnapshotRollbackOnSetLocalFailure(t *testing.T) {
	want := errors.New("set local failed")
	tx := &fakeAnalyticsTx{execErr: want}
	err := RunAnalyticsSnapshot(context.Background(), &fakeAnalyticsBeginner{tx: tx},
		func(context.Context, *AnalyticsSnapshot) error { t.Fatal("fn ran"); return nil })
	if !errors.Is(err, want) {
		t.Fatalf("err = %v", err)
	}
	if n := tx.rollbacks.Load(); n != 1 {
		t.Fatalf("rollbacks = %d, want exactly 1", n)
	}
}

func TestRunAnalyticsSnapshotRollbackOnCancellation(t *testing.T) {
	tx := &fakeAnalyticsTx{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := RunAnalyticsSnapshot(ctx, &fakeAnalyticsBeginner{tx: tx}, func(c context.Context, _ *AnalyticsSnapshot) error {
		cancel()
		<-c.Done()
		return c.Err()
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if n := tx.rollbacks.Load(); n != 1 {
		t.Fatalf("rollbacks = %d, want exactly 1", n)
	}
	if !tx.rollbackCtxOK.Load() {
		t.Fatal("rollback ran on the cancelled request context")
	}
}

func TestRunAnalyticsSnapshotRollbackOnPanic(t *testing.T) {
	tx := &fakeAnalyticsTx{}
	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("panic was swallowed")
			}
		}()
		_ = RunAnalyticsSnapshot(context.Background(), &fakeAnalyticsBeginner{tx: tx},
			func(context.Context, *AnalyticsSnapshot) error { panic("boom") })
	}()
	if n := tx.rollbacks.Load(); n != 1 {
		t.Fatalf("rollbacks = %d, want exactly 1", n)
	}
}

type fakeNetTimeout struct{}

func (fakeNetTimeout) Error() string   { return "i/o timeout" }
func (fakeNetTimeout) Timeout() bool   { return true }
func (fakeNetTimeout) Temporary() bool { return false }

func TestClassifyAnalyticsErr(t *testing.T) {
	other := errors.New("other")
	cases := []struct {
		name string
		err  error
		want error
	}{
		{"nil", nil, nil},
		{"57014", &pgconn.PgError{Code: "57014"}, ErrAnalyticsTimeout},
		{"wrapped 57014", fmt.Errorf("q: %w", &pgconn.PgError{Code: "57014"}), ErrAnalyticsTimeout},
		{"net timeout", fakeNetTimeout{}, ErrAnalyticsTimeout},
		{"deadline", context.DeadlineExceeded, ErrAnalyticsTimeout},
		{"wrapped deadline", fmt.Errorf("q: %w", context.DeadlineExceeded), ErrAnalyticsTimeout},
		{"other pg code", &pgconn.PgError{Code: "23505"}, nil},
		{"canceled", context.Canceled, context.Canceled},
		{"plain", other, other},
	}
	for _, c := range cases {
		got := ClassifyAnalyticsErr(c.err)
		want := c.want
		if c.name == "other pg code" {
			want = c.err
		}
		if got != want { //nolint:errorlint // identity is the contract: unchanged or the sentinel
			t.Errorf("%s: got %v, want %v", c.name, got, want)
		}
	}
}

// TestAnalyticsSQLOptionsBlocksAvoidUsageTables reads queries/analytics.sql and asserts no
// authority block (options, accessibility, existence, history start) mentions a usage table.
// The named list is required so renaming or deleting a block cannot make the sweep vacuous;
// the suffix sweep still covers any authority query added later.
func TestAnalyticsSQLOptionsBlocksAvoidUsageTables(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("queries", "analytics.sql"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	nameRe := regexp.MustCompile(`(?m)^--\s*name:\s*(\S+)`)
	locs := nameRe.FindAllStringSubmatchIndex(src, -1)
	bodies := map[string]string{}
	for i, loc := range locs {
		end := len(src)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		bodies[src[loc[2]:loc[3]]] = src[loc[0]:end]
	}
	required := []string{
		"AnalyticsTimezoneNames", "AnalyticsOwnerRepoOptions", "AnalyticsAllRepoOptions",
		"AnalyticsUserOptions", "AnalyticsOwnerRepoAccessible", "AnalyticsRepoExists",
		"AnalyticsUserExists", "AnalyticsOwnerHistoryStart", "AnalyticsAllHistoryStart",
	}
	for _, name := range required {
		body, ok := bodies[name]
		if !ok {
			t.Errorf("required authority block %s is missing from queries/analytics.sql", name)
			continue
		}
		if strings.Contains(strings.ToLower(body), "run_usage") {
			t.Errorf("block %s references a run_usage relation", name)
		}
	}
	authority := regexp.MustCompile(`^Analytics\w*(Options|Accessible|Exists|HistoryStart)$`)
	for name, body := range bodies {
		if authority.MatchString(name) && strings.Contains(strings.ToLower(body), "run_usage") {
			t.Errorf("block %s references a run_usage relation", name)
		}
	}
}
