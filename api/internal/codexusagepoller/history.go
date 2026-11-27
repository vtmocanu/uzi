package codexusagepoller

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// historyCopyTimeout bounds one history copy, pool acquisition included (issue #4692).
const historyCopyTimeout = 250 * time.Millisecond

// HistoryRecorder copies an accepted Codex gauge reading into the raw usage-limit history
// (issue #4692). *store.Queries satisfies it. Optional and nil-safe: the copy is best
// effort and never changes the poll's outcome.
type HistoryRecorder interface {
	CopyCodexUsageSample(ctx context.Context, arg store.CopyCodexUsageSampleParams) (int64, error)
}

// copyHistory runs the history copy for a reading the fenced write just accepted, under
// one 250 ms context. The diagnostic is a fixed message with a fixed category and the
// account id; error text is never logged. A timeout logs "outcome unknown": PostgreSQL may have
// committed the sample before the client gave up. error and skipped log "not recorded".
func (e *Engine) copyHistory(ctx context.Context, p store.UpsertCodexAccountRateLimitsParams, lastSuccess pgtype.Timestamptz) {
	if e.history == nil {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, historyCopyTimeout)
	defer cancel()
	n, err := e.history.CopyCodexUsageSample(cctx, store.CopyCodexUsageSampleParamsFrom(p, lastSuccess))
	category := ""
	switch {
	case err != nil && (errors.Is(err, context.DeadlineExceeded) || errors.Is(cctx.Err(), context.DeadlineExceeded)):
		category = "timeout"
	case err != nil:
		category = "error"
	case n == 0:
		category = "skipped"
	}
	switch category {
	case "timeout":
		e.logger.Warn("codex usage poller: usage history copy outcome unknown", "category", category, "account", p.ProviderAccountID.String())
	case "error", "skipped":
		e.logger.Warn("codex usage poller: usage history copy not recorded", "category", category, "account", p.ProviderAccountID.String())
	}
}
