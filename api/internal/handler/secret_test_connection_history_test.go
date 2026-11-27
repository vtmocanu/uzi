package handler

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// logCategoryRecorder is a focused history recorder: it blocks until its context ends
// (timeout), fails with err (error), or declines with zero rows (skipped).
type logCategoryRecorder struct {
	block bool
	err   error
}

func (r logCategoryRecorder) CopyAnthropicUsageSample(ctx context.Context, _ store.CopyAnthropicUsageSampleParams) (int64, error) {
	if r.block {
		<-ctx.Done()
		return 0, ctx.Err()
	}
	return 0, r.err
}

// Issue #4755: a history-copy timeout can arrive after PostgreSQL committed the sample, so the
// test-connection warning says the outcome is unknown; error and skipped keep "not recorded".
// Not parallel: it swaps the default slog logger.
func TestCopyAnthropicHistoryLogCategories(t *testing.T) {
	const (
		unknown     = `msg="test connection: usage history copy outcome unknown"`
		notRecorded = `msg="test connection: usage history copy not recorded"`
	)
	for _, tc := range []struct {
		name     string
		rec      logCategoryRecorder
		category string
		msg      string
		notMsg   string
	}{
		{"timeout", logCategoryRecorder{block: true}, "timeout", unknown, notRecorded},
		{"error", logCategoryRecorder{err: errors.New("SECRET-ERROR-TEXT")}, "error", notRecorded, unknown},
		{"skipped", logCategoryRecorder{}, "skipped", notRecorded, unknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureSlog(t)
			h := &Handler{anthropicHistory: tc.rec}
			id := uuid.New()
			h.copyAnthropicHistory(context.Background(), store.UpsertRateLimitsParams{UserSecretID: id})

			// Every captured line counts: the copy must emit exactly one record, the warning.
			out := strings.TrimSpace(logs.String())
			var lines []string
			if out != "" {
				lines = strings.Split(out, "\n")
			}
			if len(lines) != 1 {
				t.Fatalf("captured log lines = %d, want exactly 1: %q", len(lines), logs.String())
			}
			l := lines[0]
			if !strings.Contains(l, "level=WARN") || !strings.Contains(l, tc.msg) ||
				!strings.Contains(l, "category="+tc.category) || !strings.Contains(l, "secret="+id.String()) {
				t.Fatalf("log line = %q, want level=WARN, %s, category=%s and the secret id", l, tc.msg, tc.category)
			}
			if strings.Contains(l, tc.notMsg) || strings.Contains(l, "SECRET-ERROR-TEXT") {
				t.Fatalf("log line = %q, must not carry %s or error text", l, tc.notMsg)
			}
		})
	}
}
