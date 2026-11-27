package codexusagepoller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/notifysvc"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Issue #4692: the Codex poller copies each ACCEPTED gauge reading into the raw usage history, after
// the write and after the path's existing post-success work, best effort.

type eventLog struct {
	mu sync.Mutex
	ev []string
}

func (l *eventLog) add(s string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ev = append(l.ev, s)
}

func (l *eventLog) get() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.ev, ",")
}

type orderWarner struct{ log *eventLog }

func (w orderWarner) ObserveCodexWeekly(context.Context, notifysvc.CodexAccountFence, []apitypes.CodexRateLimitBucketDTO, time.Time) {
	w.log.add("warner")
}

type orderNotifier struct {
	log *eventLog
	err error
}

func (n orderNotifier) NotifyCodexEarlyReset(context.Context, notifysvc.CodexAccountFence, time.Time, time.Time) (store.Notification, error) {
	n.log.add("notify")
	return store.Notification{}, n.err
}

type recordingHistory struct {
	log    *eventLog
	mu     sync.Mutex
	params []store.CopyCodexUsageSampleParams
	n      int64
	err    error
	block  bool
	onCall func(ctx context.Context)
}

func (h *recordingHistory) CopyCodexUsageSample(ctx context.Context, p store.CopyCodexUsageSampleParams) (int64, error) {
	if h.log != nil {
		h.log.add("history")
	}
	h.mu.Lock()
	h.params = append(h.params, p)
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

// baselineStore serves a fixed early-reset baseline, so a poll can detect (or not detect) an edge.
type baselineStore struct {
	*fakeStore
	baseline store.GetCodexEarlyResetBaselineRow
}

func (s *baselineStore) GetCodexEarlyResetBaseline(context.Context, store.GetCodexEarlyResetBaselineParams) (store.GetCodexEarlyResetBaselineRow, error) {
	return s.baseline, nil
}

// hookCollector runs a hook inside the collection, after the engine's backoff check.
type hookCollector struct {
	*fakeCollector
	hook func()
}

func (c hookCollector) CollectCodexAccountUsage(ctx context.Context, u, a uuid.UUID) (workersvc.CodexUsageReading, error) {
	if c.hook != nil {
		c.hook()
	}
	return c.fakeCollector.CollectCodexAccountUsage(ctx, u, a)
}

type historyFx struct {
	user, account uuid.UUID
	st            *baselineStore
	col           *fakeCollector
	e             *Engine
	row           store.ListLinkedCodexAccountsToPollRow
}

// newHistoryFx builds an engine whose accepted poll FIRES the early-reset alert when fire is set (the
// forward-epoch arm of TestCodexEarlyResetBehavior), with a baseline for the notifier path.
func newHistoryFx(fire bool) *historyFx {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	expected := now.Add(12 * time.Hour)
	user, account := uuid.New(), uuid.New()
	sig := "11111111-1111-4111-8111-111111111111:2"
	st := &baselineStore{fakeStore: newFakeStore()}
	row := linkedRow(user, account, 3, 7, "idle", false, false)
	row.EnablementSig = sig
	st.toPoll = []store.ListLinkedCodexAccountsToPollRow{row}
	first := earlyResetBehaviorReading(20, expected)
	raw, _ := json.Marshal(first.Buckets)
	st.baseline = store.GetCodexEarlyResetBaselineRow{
		UserID: user, ProviderAccountID: account, Buckets: raw,
		LastSuccessAt: pgtype.Timestamptz{Time: now.Add(-5 * time.Minute), Valid: true},
		EnablementSig: pgtype.Text{String: sig, Valid: true}, NotifyEarlyLimitReset: true,
	}
	next := earlyResetBehaviorReading(20, now.Add(7*24*time.Hour))
	if !fire {
		next = earlyResetBehaviorReading(20, expected) // unchanged edge: no alert
	}
	col := &fakeCollector{results: map[uuid.UUID]collectResult{account: {reading: next}}}
	e := New(st, col, &fakeReconciler{}, 5*time.Minute, quietLogger())
	e.now = func() time.Time { return now }
	return &historyFx{user: user, account: account, st: st, col: col, e: e, row: row}
}

func TestHistoryCopyRunsAfterPostSuccessWork(t *testing.T) {
	for _, tc := range []struct {
		name      string
		fire      bool
		notifier  bool
		notifyErr error
		want      string
	}{
		{"no early-reset edge", false, true, nil, "warner,history"},
		{"edge, nil notifier", true, false, nil, "warner,history"},
		{"edge, notifier delivers", true, true, nil, "warner,notify,history"},
		{"edge, notifier declines", true, true, notifysvc.ErrCredentialNotCurrent, "warner,notify,history"},
		{"edge, notifier errors", true, true, errors.New("db down"), "warner,notify,history"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newHistoryFx(tc.fire)
			log := &eventLog{}
			fx.e.SetWeeklyWarner(orderWarner{log})
			if tc.notifier {
				fx.e.SetNotifier(orderNotifier{log: log, err: tc.notifyErr})
			}
			backoffAtCopy := true
			h := &recordingHistory{log: log, n: 1, onCall: func(context.Context) { backoffAtCopy = fx.e.inPollBackoff(fx.account) }}
			fx.e.SetHistory(h)
			// A backoff armed during the poll must be cleared before the copy runs.
			fx.e.collector = hookCollector{fx.col, func() { fx.e.setPollBackoff(fx.account, time.Hour) }}

			fx.e.tickAll(context.Background())

			if got := log.get(); got != tc.want {
				t.Fatalf("event order = %q, want %q", got, tc.want)
			}
			if backoffAtCopy {
				t.Fatal("the copy ran before the backoff was cleared")
			}
			if len(fx.st.upserts) != 1 {
				t.Fatalf("upserts = %d, want 1", len(fx.st.upserts))
			}
		})
	}
}

// The recorder receives the exact accepted write and the returned last_success_at.
func TestHistoryCopyParamsMatchTheAcceptedWrite(t *testing.T) {
	userID, acct := uuid.New(), uuid.New()
	st := newFakeStore()
	row := linkedRow(userID, acct, 2, 5, "idle", false, false)
	row.EnablementSig = "sig-a:1,sig-b:4"
	st.toPoll = []store.ListLinkedCodexAccountsToPollRow{row}
	col := &fakeCollector{results: map[uuid.UUID]collectResult{acct: {reading: sampleReading(2, 5)}}}
	e := newEngineFor(st, col, &fakeReconciler{})
	h := &recordingHistory{n: 1}
	e.SetHistory(h)

	e.Boot(context.Background())

	if h.calls() != 1 || len(st.upserts) != 1 {
		t.Fatalf("copies=%d upserts=%d, want 1 and 1", h.calls(), len(st.upserts))
	}
	got := h.params[0]
	// The exact bytes the gauge was written with.
	if !bytes.Equal(got.Buckets, st.upserts[0].Buckets) || len(got.Buckets) == 0 {
		t.Fatalf("copy buckets = %q, want the upsert's bytes %q", got.Buckets, st.upserts[0].Buckets)
	}
	want := store.CopyCodexUsageSampleParams{
		ObservedGeneration:         2,
		ObservedCredentialRevision: 5,
		UserID:                     userID,
		ProviderAccountID:          acct,
		EnablementSig:              "sig-a:1,sig-b:4",
		Buckets:                    got.Buckets,
		LastSuccessAt:              pgtype.Timestamptz{Time: fakeLastSuccess, Valid: true},
	}
	if !paramsEqual(got, want) {
		t.Fatalf("copy params = %+v\nwant         %+v", got, want)
	}
}

func paramsEqual(a, b store.CopyCodexUsageSampleParams) bool {
	return a.ObservedGeneration == b.ObservedGeneration && a.ObservedCredentialRevision == b.ObservedCredentialRevision &&
		a.UserID == b.UserID && a.ProviderAccountID == b.ProviderAccountID && a.EnablementSig == b.EnablementSig &&
		bytes.Equal(a.Buckets, b.Buckets) && a.LastSuccessAt == b.LastSuccessAt
}

// A refused write (no row) never reaches the recorder; neither does a failed collection.
func TestHistoryCopyOnlyWhenWritten(t *testing.T) {
	for _, tc := range []string{"fence refused", "collection failed", "store error"} {
		t.Run(tc, func(t *testing.T) {
			userID, acct := uuid.New(), uuid.New()
			st := newFakeStore()
			st.toPoll = []store.ListLinkedCodexAccountsToPollRow{linkedRow(userID, acct, 2, 5, "idle", false, false)}
			res := collectResult{reading: sampleReading(2, 5)}
			switch tc {
			case "fence refused":
				st.upsertRows = 0
			case "store error":
				st.upsertErr = errors.New("db down")
			default:
				res = collectResult{err: errors.New("provider down")}
			}
			col := &fakeCollector{results: map[uuid.UUID]collectResult{acct: res}}
			e := newEngineFor(st, col, &fakeReconciler{})
			h := &recordingHistory{n: 1}
			e.SetHistory(h)
			e.Boot(context.Background())
			if h.calls() != 0 {
				t.Fatalf("recorder called %d times, want 0", h.calls())
			}
		})
	}
}

const (
	histNotRecorded    = "usage history copy not recorded"
	histOutcomeUnknown = "usage history copy outcome unknown"
)

// A failing, declining or timing-out recorder changes nothing else; the diagnostic is fixed text.
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
			fx := newHistoryFx(true)
			var buf bytes.Buffer
			fx.e.logger = slog.New(slog.NewTextHandler(&buf, nil))
			log := &eventLog{}
			fx.e.SetWeeklyWarner(orderWarner{log})
			fx.e.SetNotifier(orderNotifier{log: log})
			fx.e.SetHistory(tc.h)
			fx.e.collector = hookCollector{fx.col, func() { fx.e.setPollBackoff(fx.account, time.Hour) }}

			fx.e.tickAll(context.Background())

			if got := log.get(); !strings.HasPrefix(got, "warner,notify") {
				t.Fatalf("post-success work = %q, want warner then notify", got)
			}
			if len(fx.st.upserts) != 1 {
				t.Fatalf("upserts = %d, want 1", len(fx.st.upserts))
			}
			if fx.e.inPollBackoff(fx.account) {
				t.Fatal("backoff was not cleared")
			}
			out := buf.String()
			if !strings.Contains(out, tc.msg) || !strings.Contains(out, "category="+tc.category) || !strings.Contains(out, fx.account.String()) {
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

func TestHistoryCopyDeadlineAndContext(t *testing.T) {
	type ctxKey struct{}
	fx := newHistoryFx(false)
	var remaining time.Duration
	var hasDeadline bool
	var viaParent any
	h := &recordingHistory{n: 1, onCall: func(ctx context.Context) {
		var dl time.Time
		dl, hasDeadline = ctx.Deadline()
		remaining = time.Until(dl)
		viaParent = ctx.Value(ctxKey{})
	}}
	fx.e.SetHistory(h)
	fx.e.tickAll(context.WithValue(context.Background(), ctxKey{}, "poll-ctx"))
	if !hasDeadline || remaining <= 0 || remaining > 250*time.Millisecond || remaining < 150*time.Millisecond {
		t.Fatalf("deadline present=%v remaining=%v, want about 250ms", hasDeadline, remaining)
	}
	if viaParent != "poll-ctx" {
		t.Fatal("the copy context is not derived from the poll context")
	}
}

func TestHistoryCopyNilRecorderIsSafe(t *testing.T) {
	fx := newHistoryFx(false)
	fx.e.SetHistory(nil)
	fx.e.tickAll(context.Background())
	if len(fx.st.upserts) != 1 {
		t.Fatalf("upserts = %d, want 1", len(fx.st.upserts))
	}
}
