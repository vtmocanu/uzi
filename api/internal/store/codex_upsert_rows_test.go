package store_test

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// upsertCodexRows runs UpsertCodexAccountRateLimits and reports the old affected-row count:
// 1 when the fence let the write through, 0 when it refused it (the query now returns the
// written last_success_at, so a refusal surfaces as pgx.ErrNoRows).
func upsertCodexRows(ctx context.Context, q *store.Queries, arg store.UpsertCodexAccountRateLimitsParams) (int64, error) {
	if _, err := q.UpsertCodexAccountRateLimits(ctx, arg); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, nil
		}
		return 0, err
	}
	return 1, nil
}
