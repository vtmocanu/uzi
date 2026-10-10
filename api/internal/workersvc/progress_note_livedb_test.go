package workersvc

import (
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// progress_note_livedb_test.go is the PRD #2603 live-DB gate for the Now-summary usage rows:
// counted exactly once (including re-delivery), equal under a full refold, and the stall clock
// untouched by a note-only batch. Skipped unless UZI_TEST_DATABASE_URL is set
// (./e2e/run-store-it.sh).

func noteFrame(seq int32, text, milestone string, usage map[string]any) IncomingMessage {
	p := map[string]any{"text": text, "milestone_id": milestone}
	if usage != nil {
		p["model_usage"] = usage
	}
	raw, _ := json.Marshal(p)
	return IncomingMessage{Seq: seq, Kind: "progress_note", Agent: "worker", Payload: raw}
}

func lineResult(seq int32, usage map[string]any) IncomingMessage {
	raw, _ := json.Marshal(map[string]any{"event": "result", "modelUsage": usage})
	return IncomingMessage{Seq: seq, Kind: "status", Agent: "lead", Payload: raw}
}

func initAt(seq int32) IncomingMessage { return initFrame(seq) }

func mu(in, out int64, extra map[string]any) map[string]any {
	m := map[string]any{"inputTokens": in, "outputTokens": out, "cacheReadInputTokens": 0, "cacheCreationInputTokens": 0}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

type usageTotals struct {
	in, out, cr, cw int64
	cost            float64
	status          string
}

func (e codexTestEnv) usageTotals(t *testing.T, runID uuid.UUID) usageTotals {
	t.Helper()
	row, err := e.q.GetRunUsageTotal(e.ctx, runID)
	if err != nil {
		t.Fatalf("GetRunUsageTotal: %v", err)
	}
	f, err := row.CostUsd.Float64Value()
	if err != nil {
		t.Fatalf("cost: %v", err)
	}
	return usageTotals{in: row.InputTokens, out: row.OutputTokens, cr: row.CacheReadTokens, cw: row.CacheCreationTokens, cost: f.Float64, status: row.CostStatus}
}

func assertTotals(t *testing.T, label string, got, want usageTotals) {
	t.Helper()
	if got.in != want.in || got.out != want.out || got.cr != want.cr || got.cw != want.cw || got.status != want.status || math.Abs(got.cost-want.cost) > 5e-7 {
		t.Fatalf("%s: totals = %+v, want %+v", label, got, want)
	}
}

// claudeNoteScenario is a Claude run whose lead result names haiku in a leg that also holds
// notes. Note A (seq 2) sits at an epoch (2) equal to the lead result's init-count epoch, so a
// fold that dropped the model prefix would collapse it into the lead's haiku row; notes B and C
// (seq 5 and 6) share one leg, so a fold keyed by the init count would collapse them together.
func claudeNoteScenario() []IncomingMessage {
	return []IncomingMessage{
		initAt(1),
		noteFrame(2, "Reviewing the first change", "m1", map[string]any{"claude-haiku-4-5-20251001": mu(400, 40, nil)}),
		initAt(3),
		lineResult(4, map[string]any{
			"claude-opus-4-8":           mu(1000, 2000, map[string]any{"cacheReadInputTokens": 50000, "cacheCreationInputTokens": 3000, "costUSD": 1.5}),
			"claude-haiku-4-5-20251001": mu(700, 300, map[string]any{"costUSD": 0.001}),
		}),
		noteFrame(5, "Running the tests", "m1", map[string]any{"claude-haiku-4-5-20251001": mu(500, 60, nil)}),
		noteFrame(6, "Tests are still running", "m1", map[string]any{"claude-haiku-4-5-20251001": mu(250, 20, nil)}),
	}
}

var claudeNoteTotals = usageTotals{in: 2850, out: 2420, cr: 50000, cw: 3000, cost: 1.50275, status: "metered"}

func (e codexTestEnv) seedNoteRun(t *testing.T, harness string) (store.Worker, uuid.UUID) {
	t.Helper()
	userID, workerID, repoID := e.seedCodexInfra(t)
	runID := e.seedFoldRun(t, userID, workerID, repoID, 1, "sess-notes-"+uuid.NewString())
	if harness != "claude" {
		e.exec(`UPDATE runs SET harness=$2 WHERE id=$1`, runID, harness)
	}
	return store.Worker{ID: workerID, UserID: userID, Status: "online"}, runID
}

// TestProgressNoteUsageCountedOnceLiveDB: a Claude run, delivered in pieces and then re-delivered,
// totals exactly the sum of the lead's frame and every note, once.
func TestProgressNoteUsageCountedOnceLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	wkr, runID := env.seedNoteRun(t, "claude")
	svc := New(env.q, env.box, testParams())
	gen := int64(1)
	all := claudeNoteScenario()
	send := func(label string, msgs ...IncomingMessage) {
		t.Helper()
		if err := svc.AppendMessagesForClaim(env.ctx, wkr, runID, msgs, &gen); err != nil {
			t.Fatalf("append %s: %v", label, err)
		}
	}
	send("first piece", all[0], all[1], all[2])
	send("second piece", all[3], all[4])
	send("third piece", all[5])
	assertTotals(t, "after incremental delivery", env.usageTotals(t, runID), claudeNoteTotals)

	send("re-delivered notes", all[1], all[4], all[5])
	assertTotals(t, "after re-delivering the notes", env.usageTotals(t, runID), claudeNoteTotals)
	send("re-delivered whole batch", all...)
	assertTotals(t, "after re-delivering the whole batch", env.usageTotals(t, runID), claudeNoteTotals)

	var rows int
	if err := env.pool.QueryRow(env.ctx, `SELECT count(*) FROM run_usage WHERE run_id = $1`, runID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	// opus@2, haiku@2, and three note rows (epochs 2, 5, 6) under the prefix.
	if rows != 5 {
		t.Fatalf("run_usage rows = %d, want 5 (lead opus, lead haiku, three notes)", rows)
	}
}

// TestProgressNoteCodexUsageLiveDB: a Codex run that uses gpt-6-luna for its lead result and its
// notes. Every row is metered while every note carries its marker and cost; one note without a
// marker turns the run's cost status to unreported while its tokens are still counted.
func TestProgressNoteCodexUsageLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	wkr, runID := env.seedNoteRun(t, "codex")
	svc := New(env.q, env.box, testParams())
	gen := int64(1)
	metered := func(in, out int64, cost float64) map[string]any {
		return mu(in, out, map[string]any{"costUSD": cost, "costStatus": "metered"})
	}
	msgs := []IncomingMessage{
		initAt(1),
		noteFrame(2, "Reading the plan", "m1", map[string]any{"gpt-6-luna": metered(300, 30, 0.0004)}),
		initAt(3),
		lineResult(4, map[string]any{"gpt-6-luna": metered(1000, 100, 0.0012)}),
		noteFrame(5, "Editing the files", "m1", map[string]any{"gpt-6-luna": metered(200, 20, 0.0003)}),
		noteFrame(6, "Running the gate", "m1", map[string]any{"gpt-6-luna": metered(100, 10, 0.00015)}),
	}
	for _, batch := range [][]IncomingMessage{msgs[:3], msgs[3:], msgs[1:2], msgs} {
		if err := svc.AppendMessagesForClaim(env.ctx, wkr, runID, batch, &gen); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	assertTotals(t, "complete metered luna notes", env.usageTotals(t, runID),
		usageTotals{in: 1600, out: 160, cost: 0.00205, status: "metered"})
	var harness string
	if err := env.pool.QueryRow(env.ctx, `SELECT harness FROM run_usage WHERE run_id = $1 AND model = 'progress_note:gpt-6-luna' AND lineage_epoch = 2`, runID).Scan(&harness); err != nil || harness != "codex" {
		t.Fatalf("note row harness = %q, err %v, want codex", harness, err)
	}

	// An unmarked note: tokens counted, run cost status unreported.
	unmarked := noteFrame(7, "Almost done", "m1", map[string]any{"gpt-6-luna": mu(80, 8, nil)})
	if err := svc.AppendMessagesForClaim(env.ctx, wkr, runID, []IncomingMessage{unmarked}, &gen); err != nil {
		t.Fatalf("append unmarked: %v", err)
	}
	assertTotals(t, "one unmarked note", env.usageTotals(t, runID),
		usageTotals{in: 1680, out: 168, cost: 0.00205, status: "unreported"})
}

type usageRowSnapshot struct {
	Model, SessionID, UsageBasis, Harness, CostStatus, Cost string
	Epoch, LineageIndex                                     int32
	In, Out, CacheRead, CacheWrite                          int64
	ClaimGeneration                                         *int64
}

func (e codexTestEnv) snapshotUsage(t *testing.T, runID uuid.UUID) []usageRowSnapshot {
	t.Helper()
	rows, err := e.pool.Query(e.ctx, `SELECT model, session_id, usage_basis, harness, cost_status, cost_usd::text, lineage_epoch, lineage_index,
		input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, claim_generation
		FROM run_usage WHERE run_id = $1 ORDER BY model, lineage_epoch`, runID)
	if err != nil {
		t.Fatalf("snapshot run_usage: %v", err)
	}
	defer rows.Close()
	var out []usageRowSnapshot
	for rows.Next() {
		var r usageRowSnapshot
		if err := rows.Scan(&r.Model, &r.SessionID, &r.UsageBasis, &r.Harness, &r.CostStatus, &r.Cost, &r.Epoch, &r.LineageIndex,
			&r.In, &r.Out, &r.CacheRead, &r.CacheWrite, &r.ClaimGeneration); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestProgressNoteRefoldEquivalenceLiveDB: the incremental fold and RefoldRunUsage read the same
// frames, so deleting run_usage and refolding reproduces every row exactly. ListRunUsageFrames
// must therefore include progress_note, or the refold would silently drop the notes' spend.
func TestProgressNoteRefoldEquivalenceLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	wkr, runID := env.seedNoteRun(t, "claude")
	svc := New(env.q, env.box, testParams())
	gen := int64(1)
	if err := svc.AppendMessagesForClaim(env.ctx, wkr, runID, claudeNoteScenario(), &gen); err != nil {
		t.Fatalf("append: %v", err)
	}
	before := env.snapshotUsage(t, runID)
	if len(before) != 5 {
		t.Fatalf("incremental rows = %d, want 5: %+v", len(before), before)
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
	assertTotals(t, "after refold", env.usageTotals(t, runID), claudeNoteTotals)
}

func (e codexTestEnv) lastActivity(t *testing.T, runID uuid.UUID) (time.Time, int32) {
	t.Helper()
	var at pgtype.Timestamptz
	var seq int32
	if err := e.pool.QueryRow(e.ctx, `SELECT last_activity_at, last_seq FROM runs WHERE id = $1`, runID).Scan(&at, &seq); err != nil {
		t.Fatal(err)
	}
	return at.Time, seq
}

// TestProgressNoteOnlyBatchLeavesStallClockLiveDB: a batch of only progress_note rows advances
// last_seq but not last_activity_at, so a note cannot turn a stalled run healthy; any other
// message in the batch restores the normal bump.
func TestProgressNoteOnlyBatchLeavesStallClockLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	wkr, runID := env.seedNoteRun(t, "claude")
	svc := New(env.q, env.box, testParams())
	gen := int64(1)
	old := time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Second)
	env.exec(`UPDATE runs SET last_activity_at = $2 WHERE id = $1`, runID, old)

	if err := svc.AppendMessagesForClaim(env.ctx, wkr, runID,
		[]IncomingMessage{noteFrame(1, "Looking around", "m1", nil), noteFrame(2, "Still looking", "m1", nil)}, &gen); err != nil {
		t.Fatalf("append notes: %v", err)
	}
	at, seq := env.lastActivity(t, runID)
	if seq != 2 {
		t.Fatalf("last_seq = %d, want 2 (a note batch still advances the high-water mark)", seq)
	}
	if !at.Equal(old) {
		t.Fatalf("last_activity_at moved to %v by a note-only batch, want %v", at, old)
	}

	text := IncomingMessage{Seq: 3, Kind: "text", Agent: "lead", Payload: json.RawMessage(`{"text":"working"}`)}
	if err := svc.AppendMessagesForClaim(env.ctx, wkr, runID, []IncomingMessage{noteFrame(4, "Working", "m1", nil), text}, &gen); err != nil {
		t.Fatalf("append mixed: %v", err)
	}
	at, seq = env.lastActivity(t, runID)
	if seq != 4 || time.Since(at) > time.Minute {
		t.Fatalf("after a mixed batch last_seq=%d last_activity_at=%v, want 4 and a fresh timestamp", seq, at)
	}
}

// TestProgressNoteWorkerOnlyLiveDB: progress_note is worker-written only. A worker that does
// not own the run is refused (ErrRunNotOwned) and a stale-generation batch returns
// ErrStaleClaim; neither persists a message nor folds a usage row.
func TestProgressNoteWorkerOnlyLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	wkr, runID := env.seedNoteRun(t, "claude")
	svc := New(env.q, env.box, testParams())
	batch := []IncomingMessage{noteFrame(1, "Not mine", "m1", map[string]any{"claude-haiku-4-5-20251001": mu(400, 40, nil)})}
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
