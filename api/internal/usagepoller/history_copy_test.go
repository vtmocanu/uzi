package usagepoller

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/anthropic"
	"github.com/vtmocanu/uzi/api/internal/notifysvc"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4692: the usage poller copies each ACCEPTED gauge reading into the raw usage history,
// after the write and after the path's existing post-success work, best effort.

type eventLog struct {
	mu sync.Mutex
	ev []string
}

func (l *eventLog) add(s string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ev = append(l.ev, s)
}

func (l *eventLog) get() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.ev...)
}

type orderWarner struct{ log *eventLog }

func (w orderWarner) ObserveAnthropicWeekly(context.Context, uuid.UUID, uuid.UUID, int64, *int16, *time.Time, time.Time) {
	w.log.add("warner")
}

type orderNotifier struct {
	log *eventLog
	err error
}

func (n orderNotifier) NotifyEarlyReset(context.Context, uuid.UUID, uuid.UUID, int64, time.Time, time.Time) (store.Notification, error) {
	n.log.add("notify")
	return store.Notification{}, n.err
}

// recordingHistory is a HistoryRecorder fake. onCall runs inside the copy (it can inspect the engine).
type recordingHistory struct {
	log    *eventLog
	mu     sync.Mutex
	params []store.CopyAnthropicUsageSampleParams
	ctxs   []context.Context
	n      int64
	err    error
	block  bool // wait for the context to end, then return its error
	onCall func(ctx context.Context)
}

func (h *recordingHistory) CopyAnthropicUsageSample(ctx context.Context, p store.CopyAnthropicUsageSampleParams) (int64, error) {
	if h.log != nil {
		h.log.add("history")
	}
	h.mu.Lock()
	h.params = append(h.params, p)
	h.ctxs = append(h.ctxs, ctx)
	h.mu.Unlock()
	if h.onCall != nil {
		h.onCall(ctx)
	}
	if h.block {
		<-ctx.Done()
		return 0, ctx.Err()
	}
	return h.n, h.err
}

func (h *recordingHistory) calls() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.params)
}

// earlyFireFixture arms the engine so an opt-in poll FIRES the early-reset alert (the Arm B case of
// TestEarlyResetDetection), returning the user, the fake store and the engine.
func earlyFireFixture(t *testing.T, notifyEarly bool) (uuid.UUID, *fakeStore, *Engine) {
	t.Helper()
	tReset := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	u := uuid.New()
	st := newFakeStore(u)
	st.setNotify(u, notifyEarly)
	st.setPrev(u, prevRow(u, u, 12, anthropic.SourceHeaderProbe, tReset, tReset.Add(-72*time.Hour)))
	cl := &fakeClient{usage: func([]byte) (anthropic.Reading, error) {
		return readingWithReset(0, anthropic.SourceHeaderProbe, &tReset), nil
	}}
	e, clk := newEngine(t, st, &fakeOpener{}, cl, true)
	clk.set(tReset.Add(-10 * time.Hour))
	return u, st, e
}

// The copy runs after the observe-time post-success work on every branch, after the backoff was
// cleared, and exactly once.
func TestHistoryCopyRunsAfterPostSuccessWork(t *testing.T) {
	for _, tc := range []struct {
		name        string
		notifyEarly bool
		fire        bool
		notifier    bool
		notifyErr   error
		want        []string
	}{
		{"opt-out", false, false, true, nil, []string{"warner", "history"}},
		{"opt-in, no fire", true, false, true, nil, []string{"warner", "history"}},
		{"fire, nil notifier", true, true, false, nil, []string{"warner", "history"}},
		{"fire, notifier delivers", true, true, true, nil, []string{"warner", "notify", "history"}},
		{"fire, notifier declines (credential not current)", true, true, true, notifysvc.ErrCredentialNotCurrent, []string{"warner", "notify", "history"}},
		{"fire, notifier errors", true, true, true, errors.New("db down"), []string{"warner", "notify", "history"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u, st, e := earlyFireFixture(t, tc.notifyEarly)
			if !tc.fire {
				// Same fixture but a reading that does not trip the early-reset arms.
				e.client = &fakeClient{usage: func([]byte) (anthropic.Reading, error) {
					return reading(10, 20, anthropic.SourceUsageEndpoint), nil
				}}
			}
			log := &eventLog{}
			e.SetWeeklyWarner(orderWarner{log})
			if tc.notifier {
				e.SetNotifier(orderNotifier{log: log, err: tc.notifyErr})
			}
			var backoffAtCopy = true
			h := &recordingHistory{log: log, n: 1, onCall: func(context.Context) { backoffAtCopy = e.inBackoff(u) }}
			e.SetHistory(h)
			// A backoff armed by this very poll must be cleared before the copy runs.
			cl := e.client.(*fakeClient)
			inner := cl.usage
			cl.usage = func(tok []byte) (anthropic.Reading, error) {
				e.setBackoff(u)
				return inner(tok)
			}

			e.tickAll(context.Background())

			if got := log.get(); strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("event order = %v, want %v", got, tc.want)
			}
			if backoffAtCopy {
				t.Fatal("the copy ran before the backoff was cleared")
			}
			if _, ok := st.got(u); !ok {
				t.Fatal("no gauge write")
			}
		})
	}
}

// The probe fallback (the second observe call site) copies too.
func TestHistoryCopyAfterProbeFallback(t *testing.T) {
	u := uuid.New()
	st := newFakeStore(u)
	cl := &fakeClient{
		usage: func([]byte) (anthropic.Reading, error) { return anthropic.Reading{}, httpErr(429) },
		probe: func([]byte) (anthropic.Reading, error) { return reading(5, 6, anthropic.SourceHeaderProbe), nil },
	}
	e, _ := newEngine(t, st, &fakeOpener{}, cl, true)
	h := &recordingHistory{n: 1}
	e.SetHistory(h)
	e.tickAll(context.Background())
	if h.calls() != 1 {
		t.Fatalf("copies = %d, want 1", h.calls())
	}
	if h.params[0].Source.String != anthropic.SourceHeaderProbe {
		t.Fatalf("source = %q, want header_probe", h.params[0].Source.String)
	}
}

// On the probe-fallback path the copy also runs after the backoff was cleared.
func TestHistoryCopyAfterProbeFallbackOrdering(t *testing.T) {
	u := uuid.New()
	st := newFakeStore(u)
	var e *Engine
	cl := &fakeClient{
		usage: func([]byte) (anthropic.Reading, error) { return anthropic.Reading{}, httpErr(429) },
		probe: func([]byte) (anthropic.Reading, error) {
			e.setBackoff(u) // armed during the poll; must be cleared before the copy
			return reading(5, 6, anthropic.SourceHeaderProbe), nil
		},
	}
	e, _ = newEngine(t, st, &fakeOpener{}, cl, true)
	backoffAtCopy := true
	h := &recordingHistory{n: 1, onCall: func(context.Context) { backoffAtCopy = e.inBackoff(u) }}
	e.SetHistory(h)
	e.tickAll(context.Background())
	if h.calls() != 1 {
		t.Fatalf("copies = %d, want 1", h.calls())
	}
	if backoffAtCopy {
		t.Fatal("the probe-path copy ran before the backoff was cleared")
	}
}

// On the probe-fallback path a write the fence refused never reaches the recorder.
func TestHistoryCopyProbeFallbackFenceRefused(t *testing.T) {
	u := uuid.New()
	st := newFakeStore(u)
	cl := &fakeClient{
		usage: func([]byte) (anthropic.Reading, error) { return anthropic.Reading{}, httpErr(429) },
		probe: func([]byte) (anthropic.Reading, error) {
			st.mu.Lock()
			st.rev[u] = 9 // the credential moved on during the probe
			st.mu.Unlock()
			return reading(5, 6, anthropic.SourceHeaderProbe), nil
		},
	}
	e, _ := newEngine(t, st, &fakeOpener{}, cl, true)
	h := &recordingHistory{n: 1}
	e.SetHistory(h)
	e.tickAll(context.Background())
	if h.calls() != 0 {
		t.Fatalf("recorder called %d times for a fence-refused probe write, want 0", h.calls())
	}
	if _, ok := st.got(u); ok {
		t.Fatal("the gauge was written; the case did not exercise a refused write")
	}
}

// The recorder receives the exact accepted write, field for field.
func TestHistoryCopyParamsMatchTheAcceptedWrite(t *testing.T) {
	fiveReset := time.Date(2026, 7, 15, 17, 0, 0, 0, time.UTC)
	sevenReset := time.Date(2026, 7, 22, 0, 0, 0, 0, time.UTC)
	u := uuid.New()
	st := newFakeStore(u)
	st.rev[u] = 7
	cl := &fakeClient{usage: func([]byte) (anthropic.Reading, error) {
		return anthropic.Reading{
			FiveHour: &anthropic.Window{Pct: 41, ResetsAt: &fiveReset},
			SevenDay: anthropic.Window{Pct: 62, ResetsAt: &sevenReset},
			Source:   anthropic.SourceUsageEndpoint,
		}, nil
	}}
	e, clk := newEngine(t, st, &fakeOpener{}, cl, false)
	clk.set(time.Date(2026, 7, 15, 12, 0, 0, 987654321, time.UTC))
	h := &recordingHistory{n: 1}
	e.SetHistory(h)

	e.tickAll(context.Background())

	if h.calls() != 1 {
		t.Fatalf("copies = %d, want 1", h.calls())
	}
	got := h.params[0]
	want := store.CopyAnthropicUsageSampleParams{
		UserSecretID:     u,
		UserID:           u,
		EnablementRev:    7,
		SyncedAt:         pgtype.Timestamptz{Time: time.Date(2026, 7, 15, 12, 0, 0, 987654000, time.UTC), Valid: true},
		Source:           pgtype.Text{String: "usage_endpoint", Valid: true},
		FiveHourPct:      pgtype.Int2{Int16: 41, Valid: true},
		FiveHourResetsAt: pgtype.Timestamptz{Time: fiveReset, Valid: true},
		SevenDayPct:      pgtype.Int2{Int16: 62, Valid: true},
		SevenDayResetsAt: pgtype.Timestamptz{Time: sevenReset, Valid: true},
	}
	if got != want {
		t.Fatalf("copy params = %+v\nwant         %+v", got, want)
	}
	up, _ := st.got(u)
	if !up.SyncedAt.Time.Equal(got.SyncedAt.Time) || up.EnablementRev != got.EnablementRev || up.SevenDayPct != got.SevenDayPct {
		t.Fatalf("the copy is not the write: upsert %+v copy %+v", up, got)
	}
}

// A reading without a five-hour window copies NULLs.
func TestHistoryCopyKeepsNullFiveHour(t *testing.T) {
	u := uuid.New()
	st := newFakeStore(u)
	cl := &fakeClient{usage: func([]byte) (anthropic.Reading, error) {
		return anthropic.Reading{SevenDay: anthropic.Window{Pct: 3}, Source: anthropic.SourceUsageEndpoint}, nil
	}}
	e, _ := newEngine(t, st, &fakeOpener{}, cl, false)
	h := &recordingHistory{n: 1}
	e.SetHistory(h)
	e.tickAll(context.Background())
	if h.calls() != 1 || h.params[0].FiveHourPct.Valid || h.params[0].FiveHourResetsAt.Valid {
		t.Fatalf("copies=%d params=%+v, want one copy with a NULL five-hour window", h.calls(), h.params)
	}
}

// A write the fence refused, or one that failed, never reaches the recorder.
func TestHistoryCopyOnlyWhenWritten(t *testing.T) {
	for _, tc := range []string{"fence refused (revision moved mid-poll)", "store error"} {
		t.Run(tc, func(t *testing.T) {
			u := uuid.New()
			st := newFakeStore(u)
			cl := &fakeClient{usage: func([]byte) (anthropic.Reading, error) {
				if tc == "store error" {
					st.mu.Lock()
					st.upsertErr = errors.New("db down")
					st.mu.Unlock()
				} else {
					st.mu.Lock()
					st.rev[u] = 9 // the credential moved on after the listing
					st.mu.Unlock()
				}
				return reading(1, 2, anthropic.SourceUsageEndpoint), nil
			}}
			e, _ := newEngine(t, st, &fakeOpener{}, cl, false)
			h := &recordingHistory{n: 1}
			e.SetHistory(h)
			e.tickAll(context.Background())
			if h.calls() != 0 {
				t.Fatalf("recorder called %d times for an unwritten reading, want 0", h.calls())
			}
			if _, ok := st.got(u); ok {
				t.Fatal("the gauge was written; the case did not exercise a refused write")
			}
		})
	}
}

const (
	histNotRecorded    = "usage history copy not recorded"
	histOutcomeUnknown = "usage history copy outcome unknown"
)

// A failing, declining or timing-out recorder changes nothing else, and its diagnostic is fixed
// text: a category and the row id, never the error.
func TestHistoryCopyFailureChangesNothingElse(t *testing.T) {
	for _, tc := range []struct {
		name     string
		h        *recordingHistory
		category string
		msg      string // the per-category message (issue #4755)
		notMsg   string // the other category's message, which must be absent
	}{
		{"error", &recordingHistory{err: errors.New("SECRET-ERROR-TEXT dsn=postgres://u:p@h")}, "error", histNotRecorded, histOutcomeUnknown},
		{"zero rows", &recordingHistory{n: 0}, "skipped", histNotRecorded, histOutcomeUnknown},
		// A timeout can land after PostgreSQL committed the sample, so its outcome is unknown.
		{"timeout", &recordingHistory{block: true}, "timeout", histOutcomeUnknown, histNotRecorded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u, st, e := earlyFireFixture(t, true)
			var buf bytes.Buffer
			e.logger = slog.New(slog.NewTextHandler(&buf, nil))
			log := &eventLog{}
			e.SetWeeklyWarner(orderWarner{log})
			e.SetNotifier(orderNotifier{log: log})
			e.SetHistory(tc.h)
			// A backoff armed during the poll is cleared whatever the copy does.
			cl := e.client.(*fakeClient)
			inner := cl.usage
			cl.usage = func(tok []byte) (anthropic.Reading, error) {
				e.setBackoff(u)
				return inner(tok)
			}
			e.tickAll(context.Background())

			if _, ok := st.got(u); !ok {
				t.Fatal("the gauge write is gone")
			}
			if got := strings.Join(log.get(), ","); !strings.HasPrefix(got, "warner,notify") {
				t.Fatalf("post-success work = %q, want warner then notify", got)
			}
			if e.inBackoff(u) {
				t.Fatal("backoff was not cleared")
			}
			out := buf.String()
			if !strings.Contains(out, tc.msg) || !strings.Contains(out, "category="+tc.category) || !strings.Contains(out, u.String()) {
				t.Fatalf("diagnostic missing %q, category %q or the row id: %s", tc.msg, tc.category, out)
			}
			if strings.Contains(out, tc.notMsg) {
				t.Fatalf("category %q logged %q: %s", tc.category, tc.notMsg, out)
			}
			if strings.Contains(out, "SECRET-ERROR-TEXT") || strings.Contains(out, "postgres://") {
				t.Fatalf("the diagnostic leaked error text: %s", out)
			}
		})
	}
}

// One 250 ms deadline covers the call, derived from the poll's context.
func TestHistoryCopyDeadlineAndContext(t *testing.T) {
	type ctxKey struct{}
	u := uuid.New()
	st := newFakeStore(u)
	cl := &fakeClient{usage: func([]byte) (anthropic.Reading, error) { return reading(1, 2, anthropic.SourceUsageEndpoint), nil }}
	e, _ := newEngine(t, st, &fakeOpener{}, cl, false)
	var remaining time.Duration
	var hasDeadline bool
	var viaParent any
	h := &recordingHistory{n: 1, onCall: func(ctx context.Context) {
		var dl time.Time
		dl, hasDeadline = ctx.Deadline()
		remaining = time.Until(dl)
		viaParent = ctx.Value(ctxKey{})
	}}
	e.SetHistory(h)
	e.tickAll(context.WithValue(context.Background(), ctxKey{}, "poll-ctx"))
	if !hasDeadline || remaining <= 0 || remaining > 250*time.Millisecond {
		t.Fatalf("deadline present=%v remaining=%v, want within (0, 250ms]", hasDeadline, remaining)
	}
	if remaining < 150*time.Millisecond {
		t.Fatalf("remaining %v is far below 250ms; the deadline is not the copy's own", remaining)
	}
	if viaParent != "poll-ctx" {
		t.Fatal("the copy context is not derived from the poll context")
	}
}

// No recorder wired (the default) is safe and changes nothing.
func TestHistoryCopyNilRecorderIsSafe(t *testing.T) {
	u := uuid.New()
	st := newFakeStore(u)
	cl := &fakeClient{usage: func([]byte) (anthropic.Reading, error) { return reading(1, 2, anthropic.SourceUsageEndpoint), nil }}
	e, _ := newEngine(t, st, &fakeOpener{}, cl, false)
	e.SetHistory(nil)
	e.tickAll(context.Background())
	if _, ok := st.got(u); !ok {
		t.Fatal("the gauge write is gone")
	}
}
