package workersvc

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// summary_usage_livedb_test.go is the issue #2686 live-DB gate for the summary-pass usage rows:
// counted exactly once (including re-delivery), equal under a full refold, and the stall clock
// untouched by a summary-usage-only batch. It reuses the helpers of progress_note_livedb_test.go.
// Skipped unless UZI_TEST_DATABASE_URL is set (./e2e/run-store-it.sh).

func summaryFrame(seq int32, pass string, usage map[string]any) IncomingMessage {
	raw, _ := json.Marshal(map[string]any{"pass": pass, "model_usage": usage})
	return IncomingMessage{Seq: seq, Kind: "summary_usage", Agent: "worker", Payload: raw}
}

// claudeSummaryScenario is a Claude run whose lead result names haiku in a leg that also holds
// summary passes. The intent pass (seq 2) sits at an epoch equal to the lead result's init-count
// epoch, so a fold that dropped the model prefix would collapse it into the lead's haiku row; the
// two pr_description passes (seq 6 and 7, a regeneration) share one leg and one pass name, so a
// fold keyed by pass or by the init count would collapse them together.
func claudeSummaryScenario() []IncomingMessage {
	const haiku = "claude-haiku-4-5-20251001"
	return []IncomingMessage{
		initAt(1),
		summaryFrame(2, "intent", map[string]any{haiku: mu(400, 40, nil)}),
		initAt(3),
		lineResult(4, map[string]any{
			"claude-opus-4-8": mu(1000, 2000, map[string]any{"cacheReadInputTokens": 50000, "cacheCreationInputTokens": 3000, "costUSD": 1.5}),
			haiku:             mu(700, 300, map[string]any{"costUSD": 0.001}),
		}),
		summaryFrame(5, "plan", map[string]any{haiku: mu(500, 60, nil)}),
		summaryFrame(6, "pr_description", map[string]any{haiku: mu(250, 20, nil)}),
		summaryFrame(7, "pr_description", map[string]any{haiku: mu(300, 30, nil)}),
	}
}

var claudeSummaryTotals = usageTotals{in: 3150, out: 2450, cr: 50000, cw: 3000, cost: 1.5032, status: "metered"}

func TestSummaryUsageCountedOnceLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	wkr, runID := env.seedNoteRun(t, "claude")
	svc := New(env.q, env.box, testParams())
	gen := int64(1)
	all := claudeSummaryScenario()
	send := func(label string, msgs ...IncomingMessage) {
		t.Helper()
		if err := svc.AppendMessagesForClaim(env.ctx, wkr, runID, msgs, &gen); err != nil {
			t.Fatalf("append %s: %v", label, err)
		}
	}
	send("first piece", all[0], all[1], all[2])
	send("second piece", all[3], all[4])
	send("third piece", all[5], all[6])
	assertTotals(t, "after incremental delivery", env.usageTotals(t, runID), claudeSummaryTotals)

	send("re-delivered passes", all[1], all[4], all[5], all[6])
	assertTotals(t, "after re-delivering the passes", env.usageTotals(t, runID), claudeSummaryTotals)
	send("re-delivered whole batch", all...)
	assertTotals(t, "after re-delivering the whole batch", env.usageTotals(t, runID), claudeSummaryTotals)

	var rows int
	if err := env.pool.QueryRow(env.ctx, `SELECT count(*) FROM run_usage WHERE run_id = $1`, runID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	// opus@2, haiku@2, and four summary_pass rows (epochs 2, 5, 6, 7).
	if rows != 6 {
		t.Fatalf("run_usage rows = %d, want 6 (lead opus, lead haiku, four passes)", rows)
	}
}

func TestSummaryUsageCodexUsageLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	wkr, runID := env.seedNoteRun(t, "codex")
	svc := New(env.q, env.box, testParams())
	gen := int64(1)
	metered := func(in, out int64, cost float64) map[string]any {
		return mu(in, out, map[string]any{"costUSD": cost, "costStatus": "metered"})
	}
	msgs := []IncomingMessage{
		initAt(1),
		lineResult(2, map[string]any{"gpt-6-luna": metered(1000, 100, 0.0012)}),
		summaryFrame(3, "pr_description", map[string]any{"gpt-6-luna": metered(200, 20, 0.0003)}),
		summaryFrame(4, "pr_description", map[string]any{"gpt-6-luna": metered(100, 10, 0.00015)}),
	}
	for _, batch := range [][]IncomingMessage{msgs[:2], msgs[2:], msgs[3:], msgs} {
		if err := svc.AppendMessagesForClaim(env.ctx, wkr, runID, batch, &gen); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	assertTotals(t, "complete metered luna passes", env.usageTotals(t, runID),
		usageTotals{in: 1300, out: 130, cost: 0.00165, status: "metered"})
	var harness string
	if err := env.pool.QueryRow(env.ctx, `SELECT harness FROM run_usage WHERE run_id = $1 AND model = 'summary_pass:gpt-6-luna' AND lineage_epoch = 3`, runID).Scan(&harness); err != nil || harness != "codex" {
		t.Fatalf("pass row harness = %q, err %v, want codex", harness, err)
	}

	unmarked := summaryFrame(5, "pr_description", map[string]any{"gpt-6-luna": mu(80, 8, nil)})
	if err := svc.AppendMessagesForClaim(env.ctx, wkr, runID, []IncomingMessage{unmarked}, &gen); err != nil {
		t.Fatalf("append unmarked: %v", err)
	}
	assertTotals(t, "one unmarked pass", env.usageTotals(t, runID),
		usageTotals{in: 1380, out: 138, cost: 0.00165, status: "unreported"})
}

// TestSummaryUsageRefoldEquivalenceLiveDB: deleting run_usage and refolding reproduces every row,
// which holds only while ListRunUsageFrames includes summary_usage.
func TestSummaryUsageRefoldEquivalenceLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	wkr, runID := env.seedNoteRun(t, "claude")
	svc := New(env.q, env.box, testParams())
	gen := int64(1)
	if err := svc.AppendMessagesForClaim(env.ctx, wkr, runID, claudeSummaryScenario(), &gen); err != nil {
		t.Fatalf("append: %v", err)
	}
	before := env.snapshotUsage(t, runID)
	if len(before) != 6 {
		t.Fatalf("incremental rows = %d, want 6: %+v", len(before), before)
	}
	run, err := env.q.GetRunByID(env.ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if err := RefoldRunUsage(env.ctx, env.pool, env.q, run); err != nil {
		t.Fatalf("RefoldRunUsage: %v", err)
	}
	after := env.snapshotUsage(t, runID)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("refold differs from the incremental fold:\n before %+v\n after  %+v", before, after)
	}
	assertTotals(t, "after refold", env.usageTotals(t, runID), claudeSummaryTotals)
}

func TestSummaryUsageOnlyBatchLeavesStallClockLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	wkr, runID := env.seedNoteRun(t, "claude")
	svc := New(env.q, env.box, testParams())
	gen := int64(1)
	old := time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Second)
	env.exec(`UPDATE runs SET last_activity_at = $2 WHERE id = $1`, runID, old)

	haiku := map[string]any{"claude-haiku-4-5-20251001": mu(10, 1, nil)}
	if err := svc.AppendMessagesForClaim(env.ctx, wkr, runID,
		[]IncomingMessage{summaryFrame(1, "intent", haiku), noteFrame(2, "Still looking", "m1", nil)}, &gen); err != nil {
		t.Fatalf("append summary usage: %v", err)
	}
	at, seq := env.lastActivity(t, runID)
	if seq != 2 {
		t.Fatalf("last_seq = %d, want 2 (a summary-usage batch still advances the high-water mark)", seq)
	}
	if !at.Equal(old) {
		t.Fatalf("last_activity_at moved to %v by a summary-usage-only batch, want %v", at, old)
	}

	text := IncomingMessage{Seq: 3, Kind: "text", Agent: "lead", Payload: json.RawMessage(`{"text":"working"}`)}
	if err := svc.AppendMessagesForClaim(env.ctx, wkr, runID, []IncomingMessage{summaryFrame(4, "plan", haiku), text}, &gen); err != nil {
		t.Fatalf("append mixed: %v", err)
	}
	at, seq = env.lastActivity(t, runID)
	if seq != 4 || time.Since(at) > time.Minute {
		t.Fatalf("after a mixed batch last_seq=%d last_activity_at=%v, want 4 and a fresh timestamp", seq, at)
	}
}

func TestSummaryUsageWorkerOnlyLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	wkr, runID := env.seedNoteRun(t, "claude")
	svc := New(env.q, env.box, testParams())
	batch := []IncomingMessage{summaryFrame(1, "plan", map[string]any{"claude-haiku-4-5-20251001": mu(400, 40, nil)})}
	assertNothingStored := func(label string) {
		t.Helper()
		var msgs, usage int
		if err := env.pool.QueryRow(env.ctx, `SELECT count(*) FROM run_messages WHERE run_id = $1`, runID).Scan(&msgs); err != nil {
			t.Fatal(err)
		}
		if err := env.pool.QueryRow(env.ctx, `SELECT count(*) FROM run_usage WHERE run_id = $1`, runID).Scan(&usage); err != nil {
			t.Fatal(err)
		}
		if msgs != 0 || usage != 0 {
			t.Fatalf("%s: persisted %d messages and %d usage rows, want none", label, msgs, usage)
		}
	}

	gen := int64(1)
	other := store.Worker{ID: uuid.New(), UserID: wkr.UserID, Status: "online"}
	if err := svc.AppendMessagesForClaim(env.ctx, other, runID, batch, &gen); !errors.Is(err, ErrRunNotOwned) {
		t.Fatalf("foreign worker err = %v, want ErrRunNotOwned", err)
	}
	assertNothingStored("foreign worker")

	stale := int64(0)
	if err := svc.AppendMessagesForClaim(env.ctx, wkr, runID, batch, &stale); !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("stale generation err = %v, want ErrStaleClaim", err)
	}
	assertNothingStored("stale generation")
}
