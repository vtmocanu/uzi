package store_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #1116 against a REAL Postgres. This is the ONLY place the ClearSlackRunLimitPause
// WHERE clause actually executes: the slacksvc fake NotifierStore runs no SQL, so its
// compare-and-swap semantics — the `AND limit_paused_at = @at` guard — are a claim about
// the statement, not about the Go around it, and only knowable by running it against the
// server. The guard is mutation-checked here: dropping `AND limit_paused_at = @at` from
// ClearSlackRunLimitPause makes the STALE-value case below pass a clear it must refuse,
// so this test fails closed if the guard is ever removed.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; the store-IT
// runner (e2e/run-store-it.sh) provides one.
func TestClearSlackRunLimitPauseLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	f, done := setupAwaitingInput(ctx, t, dsn)
	defer done()

	at1 := pgtype.Timestamptz{Time: time.Date(2026, 9, 5, 3, 0, 0, 0, time.UTC), Valid: true}
	at2 := pgtype.Timestamptz{Time: time.Date(2026, 9, 5, 5, 30, 0, 0, time.UTC), Valid: true}

	// A fresh anchor carries no park marker: limit_paused_at starts NULL/invalid.
	// The (channel_id, root_ts) pair is UNIQUE (slack_run_messages_channel_root_uniq,
	// migration 00105), so derive it from this run's fresh UUID rather than a fixed
	// literal — otherwise this test collides with any other LiveDB test that inserts an
	// anchor (e.g. TestSetSlackRunQuestionLiveDB) in the same single-process sweep.
	chanID := "D-" + f.runID.String()
	rootTs := "root-" + f.runID.String()
	anchor, err := f.q.UpsertSlackRunMessage(ctx, store.UpsertSlackRunMessageParams{
		RunID: f.runID, ChannelID: chanID, RootTs: rootTs,
	})
	if err != nil {
		t.Fatalf("UpsertSlackRunMessage: %v", err)
	}
	if anchor.LimitPausedAt.Valid {
		t.Fatalf("a fresh anchor carries no park marker: %+v", anchor.LimitPausedAt)
	}

	// Set the park marker to at1 (the run's status_since captured at the park).
	if _, err := f.q.SetSlackRunLimitPause(ctx, store.SetSlackRunLimitPauseParams{
		RunID: f.runID, At: at1,
	}); err != nil {
		t.Fatalf("SetSlackRunLimitPause: %v", err)
	}
	reread, err := f.q.GetSlackRunMessage(ctx, f.runID)
	if err != nil {
		t.Fatalf("GetSlackRunMessage: %v", err)
	}
	if !reread.LimitPausedAt.Valid || !reread.LimitPausedAt.Time.Equal(at1.Time) {
		t.Fatalf("the park start must be legible on the anchor: got %+v want %v", reread.LimitPausedAt, at1.Time)
	}

	// A STALE clear (at2 != the stored at1) must be REFUSED by the compare-and-swap guard:
	// no row returned (pgx.ErrNoRows), and the marker is left untouched. This is the case
	// that fails if `AND limit_paused_at = @at` is ever dropped.
	if _, err := f.q.ClearSlackRunLimitPause(ctx, store.ClearSlackRunLimitPauseParams{
		RunID: f.runID, At: at2,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("a stale clear (at2 != stored at1) must be refused with no row, got err=%v", err)
	}
	reread, err = f.q.GetSlackRunMessage(ctx, f.runID)
	if err != nil {
		t.Fatalf("GetSlackRunMessage (after stale clear): %v", err)
	}
	if !reread.LimitPausedAt.Valid || !reread.LimitPausedAt.Time.Equal(at1.Time) {
		t.Fatalf("a refused clear must leave the marker set to at1: got %+v", reread.LimitPausedAt)
	}

	// The matching clear (at1 == the stored value) succeeds, returns the row, and leaves
	// the column NULL/invalid so a redelivered `running` can never re-post the resume line.
	cleared, err := f.q.ClearSlackRunLimitPause(ctx, store.ClearSlackRunLimitPauseParams{
		RunID: f.runID, At: at1,
	})
	if err != nil {
		t.Fatalf("ClearSlackRunLimitPause (matching at1): %v", err)
	}
	if cleared.LimitPausedAt.Valid {
		t.Fatalf("the matching clear must NULL the marker on the returned row: %+v", cleared.LimitPausedAt)
	}
	reread, err = f.q.GetSlackRunMessage(ctx, f.runID)
	if err != nil {
		t.Fatalf("GetSlackRunMessage (after matching clear): %v", err)
	}
	if reread.LimitPausedAt.Valid {
		t.Fatalf("the marker must read NULL after a matching clear: %+v", reread.LimitPausedAt)
	}
}
