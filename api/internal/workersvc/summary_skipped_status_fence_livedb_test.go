package workersvc

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestSummarySkippedStatusFenceLiveDB (issue #3248 M3): the worker's "summary skipped" note is an
// ordinary kind "status" message, so it rides the same claim fence as every other worker frame. A
// foreign worker is refused (ErrRunNotOwned), a stale or released claim generation is refused
// (ErrStaleClaim), none of them stores a row, and the live generation does store exactly one.
// Skipped unless UZI_TEST_DATABASE_URL is set (./e2e/run-store-it.sh).
func TestSummarySkippedStatusFenceLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	wkr, runID := env.seedNoteRun(t, "codex")
	svc := New(env.q, env.box, testParams())

	raw, err := json.Marshal(map[string]any{
		"text":   "Plan summary skipped: the run's Codex credential needed a refresh, which summaries may not do. The run continues.",
		"event":  "summary_skipped",
		"pass":   "plan",
		"reason": "refresh_denied",
	})
	if err != nil {
		t.Fatal(err)
	}
	batch := []IncomingMessage{{Seq: 1, Kind: "status", Agent: "worker", Payload: raw}}

	countMsgs := func() int {
		t.Helper()
		var n int
		if err := env.pool.QueryRow(env.ctx, `SELECT count(*) FROM run_messages WHERE run_id = $1`, runID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	assertCount := func(label string, want int) {
		t.Helper()
		if got := countMsgs(); got != want {
			t.Fatalf("%s: run_messages count = %d, want %d", label, got, want)
		}
	}

	live := int64(1)
	assertCount("before any append", 0)

	other := store.Worker{ID: uuid.New(), UserID: wkr.UserID, Status: "online"}
	if err := svc.AppendMessagesForClaim(env.ctx, other, runID, batch, &live); !errors.Is(err, ErrRunNotOwned) {
		t.Fatalf("foreign worker err = %v, want ErrRunNotOwned", err)
	}
	assertCount("foreign worker", 0)

	stale := int64(0)
	if err := svc.AppendMessagesForClaim(env.ctx, wkr, runID, batch, &stale); !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("stale generation err = %v, want ErrStaleClaim", err)
	}
	assertCount("stale generation", 0)

	// Positive control: the live generation stores exactly one status row.
	if err := svc.AppendMessagesForClaim(env.ctx, wkr, runID, batch, &live); err != nil {
		t.Fatalf("append at live generation: %v", err)
	}
	assertCount("live generation", 1)
	var kind string
	if err := env.pool.QueryRow(env.ctx, `SELECT kind FROM run_messages WHERE run_id = $1`, runID).Scan(&kind); err != nil || kind != "status" {
		t.Fatalf("stored kind = %q, err %v, want status", kind, err)
	}

	// Release the claim: the same generation is now fenced out and nothing more is stored.
	env.exec(`UPDATE runs SET claim_released_at = now() WHERE id = $1`, runID)
	next := []IncomingMessage{{Seq: 2, Kind: "status", Agent: "worker", Payload: raw}}
	if err := svc.AppendMessagesForClaim(env.ctx, wkr, runID, next, &live); !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("released claim err = %v, want ErrStaleClaim", err)
	}
	assertCount("released claim", 1)
}
