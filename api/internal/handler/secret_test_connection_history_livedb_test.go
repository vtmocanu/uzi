package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/anthropic"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4692: an accepted Anthropic test-connection gauge write is copied into the usage history
// AFTER the handler's final enablement re-read, only when the verdict is still ok, best effort.

type fakeAnthropicTester struct {
	usage func() (anthropic.Reading, error)
	probe func() (anthropic.Reading, bool, error)
}

func (f fakeAnthropicTester) Usage(context.Context, []byte) (anthropic.Reading, error) {
	return f.usage()
}

func (f fakeAnthropicTester) ProbeTest(context.Context, []byte) (anthropic.Reading, bool, error) {
	return f.probe()
}

type historyEvents struct {
	mu sync.Mutex
	ev []string
}

func (e *historyEvents) add(s string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ev = append(e.ev, s)
}

func (e *historyEvents) joined() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return strings.Join(e.ev, ",")
}

type historyRecorder struct {
	events *historyEvents
	mu     sync.Mutex
	params []store.CopyAnthropicUsageSampleParams
	ctx    context.Context
	err    error
	block  bool
}

func (r *historyRecorder) CopyAnthropicUsageSample(ctx context.Context, p store.CopyAnthropicUsageSampleParams) (int64, error) {
	r.events.add("history")
	r.mu.Lock()
	r.params = append(r.params, p)
	r.ctx = ctx
	r.mu.Unlock()
	if r.block {
		<-ctx.Done()
		return 0, ctx.Err()
	}
	return 1, r.err
}

func (r *historyRecorder) calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.params)
}

type histHandlerFx struct {
	h      *Handler
	pool   *pgxpool.Pool
	owner  uuid.UUID
	id     uuid.UUID
	events *historyEvents
}

// newHistHandlerFx wires a handler whose Anthropic client is the given fake, whose store logs each
// GetSecretEnablement read (the re-read) and runs onFinalRead before the SECOND one.
func newHistHandlerFx(t *testing.T, tester fakeAnthropicTester, onFinalRead func(pool *pgxpool.Pool, id uuid.UUID)) *histHandlerFx {
	t.Helper()
	h, _, pool := cliLiveDB(t)
	owner := cliSeedUser(t, pool, false)
	id := seedTestConnection(t, h, pool, owner, store.KindAnthropicToken, "anthropic-fixture-"+uuid.NewString())
	h.secretTestOnce.Do(func() {})
	h.secretTestClients = &secretTestClients{anthropic: tester}
	h.cfg.UsageProbe = true
	fx := &histHandlerFx{h: h, pool: pool, owner: owner, id: id, events: &historyEvents{}}
	reads := 0
	h.q = store.New(secretRereadObserver{DBTX: pool, observe: func(_ context.Context, sql string) {
		if !strings.HasPrefix(sql, "-- name: GetSecretEnablement ") {
			return
		}
		reads++
		if reads == 2 {
			fx.events.add("reread")
			if onFinalRead != nil {
				onFinalRead(pool, id)
			}
		}
	}})
	return fx
}

func (fx *histHandlerFx) call(ctx context.Context) *httptest.ResponseRecorder {
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("kind", store.KindAnthropicToken)
	rctx.URLParams.Add("id", fx.id.String())
	ctx = context.WithValue(ctx, chi.RouteCtxKey, rctx)
	ctx = mw.ContextWithUser(ctx, store.User{ID: fx.owner})
	req := httptest.NewRequest(http.MethodPost, testConnectionPath(store.KindAnthropicToken, fx.id), nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	fx.h.TestMySecret(rec, req)
	return rec
}

func okReading() (anthropic.Reading, error) {
	reset := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)
	return anthropic.Reading{
		FiveHour: &anthropic.Window{Pct: 30},
		SevenDay: anthropic.Window{Pct: 50, ResetsAt: &reset},
		Source:   anthropic.SourceUsageEndpoint,
	}, nil
}

func TestSecretConnectionAnthropicHistoryCopyAfterRereadLiveDB(t *testing.T) {
	fx := newHistHandlerFx(t, fakeAnthropicTester{usage: okReading}, nil)
	rec := &historyRecorder{events: fx.events}
	fx.h.anthropicHistory = rec

	resp := fx.call(context.Background())

	requireTestResult(t, resp, http.StatusOK, "ok", "")
	if got := fx.events.joined(); got != "reread,history" {
		t.Fatalf("event order = %q, want the copy strictly after the final re-read", got)
	}
	if rec.calls() != 1 {
		t.Fatalf("copies = %d, want 1", rec.calls())
	}
	p := rec.params[0]
	var syncedAt time.Time
	var source string
	if err := fx.pool.QueryRow(context.Background(), `SELECT synced_at, source FROM anthropic_rate_limits WHERE user_secret_id = $1`, fx.id).Scan(&syncedAt, &source); err != nil {
		t.Fatalf("gauge row: %v", err)
	}
	if p.UserSecretID != fx.id || p.UserID != fx.owner || p.Source.String != "usage_endpoint" || p.Source.String != source ||
		!p.SyncedAt.Valid || !p.SyncedAt.Time.Equal(syncedAt) || p.SyncedAt.Time.Nanosecond()%1000 != 0 ||
		p.FiveHourPct.Int16 != 30 || p.SevenDayPct.Int16 != 50 || !p.SevenDayResetsAt.Valid {
		t.Fatalf("copy params %+v do not describe the accepted write (gauge synced_at %v, source %s)", p, syncedAt, source)
	}
	if strings.Contains(resp.Body.String(), "history") {
		t.Fatalf("the response leaked the history copy: %s", resp.Body.String())
	}
}

// With no override the copy goes through h.q and lands a real sample row.
func TestSecretConnectionAnthropicHistoryDefaultsToStoreLiveDB(t *testing.T) {
	fx := newHistHandlerFx(t, fakeAnthropicTester{usage: okReading}, nil)
	requireTestResult(t, fx.call(context.Background()), http.StatusOK, "ok", "")
	var n int
	if err := fx.pool.QueryRow(context.Background(), `SELECT count(*) FROM anthropic_usage_samples WHERE user_id = $1 AND user_secret_id = $2`, fx.owner, fx.id).Scan(&n); err != nil || n != 1 {
		t.Fatalf("samples = (%d, %v), want 1 row written through the default recorder", n, err)
	}
}

// A superseded verdict (the credential changed during the test) copies nothing.
func TestSecretConnectionAnthropicHistorySupersededLiveDB(t *testing.T) {
	fx := newHistHandlerFx(t, fakeAnthropicTester{usage: okReading}, func(pool *pgxpool.Pool, id uuid.UUID) {
		if _, err := pool.Exec(context.Background(), `UPDATE user_secrets SET updated_at = updated_at + interval '1 second' WHERE id = $1`, id); err != nil {
			t.Errorf("mutate: %v", err)
		}
	})
	rec := &historyRecorder{events: fx.events}
	fx.h.anthropicHistory = rec
	requireTestResult(t, fx.call(context.Background()), http.StatusOK, "inconclusive", "superseded")
	if rec.calls() != 0 {
		t.Fatalf("copies = %d for a superseded verdict, want 0", rec.calls())
	}
}

// A successful status-only probe (no gauge reading) and a rejection write no history.
func TestSecretConnectionAnthropicHistoryClearAndRejectCopyNothingLiveDB(t *testing.T) {
	httpFail := func() (anthropic.Reading, error) {
		return anthropic.Reading{}, &anthropic.Error{Kind: anthropic.KindHTTP, Status: 429}
	}
	for _, tc := range []struct {
		name   string
		tester fakeAnthropicTester
		status string
	}{
		{"clear (status-only probe)", fakeAnthropicTester{usage: httpFail, probe: func() (anthropic.Reading, bool, error) { return anthropic.Reading{}, false, nil }}, "ok"},
		{"rejected", fakeAnthropicTester{usage: httpFail, probe: func() (anthropic.Reading, bool, error) {
			return anthropic.Reading{}, false, &anthropic.Error{Kind: anthropic.KindHTTP, Status: 401}
		}}, "rejected"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newHistHandlerFx(t, tc.tester, nil)
			rec := &historyRecorder{events: fx.events}
			fx.h.anthropicHistory = rec
			requireTestResult(t, fx.call(context.Background()), http.StatusOK, tc.status, "")
			if rec.calls() != 0 {
				t.Fatalf("copies = %d, want 0", rec.calls())
			}
		})
	}
}

// A failing or stalled recorder leaves the verdict and response exactly as without one, and the
// copy context derives from the request (never the probe) with a 250 ms deadline.
func TestSecretConnectionAnthropicHistoryFailureLeavesResponseLiveDB(t *testing.T) {
	type ctxKey struct{}
	baseline := newHistHandlerFx(t, fakeAnthropicTester{usage: okReading}, nil)
	baseline.h.anthropicHistory = &historyRecorder{events: baseline.events}
	want := baseline.call(context.Background()).Body.String()

	for _, tc := range []struct {
		name string
		rec  func(*historyEvents) *historyRecorder
	}{
		{"error", func(e *historyEvents) *historyRecorder {
			return &historyRecorder{events: e, err: errors.New("SECRET-ERROR-TEXT")}
		}},
		{"stalled until the deadline", func(e *historyEvents) *historyRecorder { return &historyRecorder{events: e, block: true} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newHistHandlerFx(t, fakeAnthropicTester{usage: okReading}, nil)
			rec := tc.rec(fx.events)
			fx.h.anthropicHistory = rec
			start := time.Now()
			resp := fx.call(context.WithValue(context.Background(), ctxKey{}, "request-ctx"))
			elapsed := time.Since(start)

			if resp.Code != http.StatusOK || resp.Body.String() != want {
				t.Fatalf("response = %d %s, want 200 %s", resp.Code, resp.Body.String(), want)
			}
			if rec.calls() != 1 {
				t.Fatalf("copies = %d, want 1", rec.calls())
			}
			if rec.ctx.Value(ctxKey{}) != "request-ctx" {
				t.Fatal("the copy context is not derived from the request context")
			}
			dl, ok := rec.ctx.Deadline()
			if !ok || dl.Sub(start) > 400*time.Millisecond {
				t.Fatalf("copy deadline = %v (present=%v), want about 250ms from the call", dl, ok)
			}
			if tc.rec != nil && strings.Contains(tc.name, "stalled") && (elapsed < 200*time.Millisecond || elapsed > 2*time.Second) {
				t.Fatalf("stalled copy took %v, want about 250ms", elapsed)
			}
		})
	}
}
