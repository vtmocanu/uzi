package handler

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// These tests prove the issue #1238 secret-free route-total timing instrumentation on the
// worker refresh route: codexRouteResult's decision table and logCodexRouteTiming's schema.
// DB-free, package handler; a plain `go test ./internal/handler/` exercises them. They must
// NOT call t.Parallel — the slog default is process-global shared state.

// timingRecord is one slog record the capturing handler recorded.
type timingRecord struct {
	msg   string
	attrs map[string]slog.Value
}

type timingCaptureHandler struct {
	mu      sync.Mutex
	records []timingRecord
}

func (h *timingCaptureHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }

func (h *timingCaptureHandler) Handle(_ context.Context, r slog.Record) error {
	attrs := make(map[string]slog.Value, r.NumAttrs())
	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value
		return true
	})
	h.mu.Lock()
	h.records = append(h.records, timingRecord{msg: r.Message, attrs: attrs})
	h.mu.Unlock()
	return nil
}

func (h *timingCaptureHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *timingCaptureHandler) WithGroup(_ string) slog.Handler      { return h }

func (h *timingCaptureHandler) snapshot() []timingRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]timingRecord, len(h.records))
	copy(out, h.records)
	return out
}

func installTimingCapture(t *testing.T) *timingCaptureHandler {
	t.Helper()
	cap := &timingCaptureHandler{}
	prev := slog.Default()
	slog.SetDefault(slog.New(cap))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return cap
}

func timingKeysOf(attrs map[string]slog.Value) []string {
	keys := make([]string, 0, len(attrs))
	for k := range attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func assertTimingKeySet(t *testing.T, rec timingRecord, want ...string) {
	t.Helper()
	if len(rec.attrs) != len(want) {
		t.Fatalf("record %q: got keys %v, want exactly %v", rec.msg, timingKeysOf(rec.attrs), want)
	}
	for _, k := range want {
		if _, ok := rec.attrs[k]; !ok {
			t.Fatalf("record %q: missing key %q; have %v", rec.msg, k, timingKeysOf(rec.attrs))
		}
	}
}

func TestCodexRouteResultDecision(t *testing.T) {
	cases := []struct {
		name         string
		serviceErr   error
		wroteSuccess bool
		want         string
	}{
		{"nil-no-write", nil, false, "error"},
		{"nil-write", nil, true, "ok"},
		{"canceled", context.Canceled, false, "canceled"},
		{"deadline", context.DeadlineExceeded, false, "deadline"},
		{"error-no-write", errors.New("boom"), false, "error"},
		{"error-with-write", errors.New("boom"), true, "error"},
	}
	for _, c := range cases {
		if got := codexRouteResult(c.serviceErr, c.wroteSuccess); got != c.want {
			t.Errorf("%s: codexRouteResult(%v, %v)=%q want %q", c.name, c.serviceErr, c.wroteSuccess, got, c.want)
		}
	}
}

func TestLogCodexRouteTimingSchema(t *testing.T) {
	cap := installTimingCapture(t)

	opID := uuid.New()
	logCodexRouteTiming(opID, time.Now().Add(-1500*time.Millisecond), "ok")
	logCodexRouteTiming(uuid.New(), time.Now().Add(-1500*time.Millisecond), "error")

	recs := cap.snapshot()
	var routes []timingRecord
	for _, rec := range recs {
		if rec.msg == codexTimingMsgRoute {
			routes = append(routes, rec)
		}
	}
	if len(routes) != 2 {
		t.Fatalf("got %d route records, want exactly 2", len(routes))
	}

	wantResults := []string{"ok", "error"}
	for i, rec := range routes {
		assertTimingKeySet(t, rec, "operation_id", "route_total_ms", "result")
		if rec.attrs["route_total_ms"].Kind() != slog.KindInt64 {
			t.Fatalf("record %d: route_total_ms kind=%v want Int64", i, rec.attrs["route_total_ms"].Kind())
		}
		if rec.attrs["route_total_ms"].Int64() < 0 {
			t.Fatalf("record %d: route_total_ms=%d want >= 0", i, rec.attrs["route_total_ms"].Int64())
		}
		if got := rec.attrs["result"].String(); got != wantResults[i] {
			t.Fatalf("record %d: result=%q want %q", i, got, wantResults[i])
		}
		// operation_id must be a parseable UUID (a non-secret correlation id), not a token.
		if _, err := uuid.Parse(rec.attrs["operation_id"].String()); err != nil {
			t.Fatalf("record %d: operation_id %q is not a UUID: %v", i, rec.attrs["operation_id"].String(), err)
		}
	}
}
