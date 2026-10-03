package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// usage_tail_livedb_test.go is the live-DB gate for the estimated usage tail (issue #2014,
// ADR-2014): the coverage predicate (D4), the coverage reasons (D5), pricing (D6), the metered
// total staying untouched (D7), the schema's monotone upserts (D8), the route's fence and request
// bounds (D9) and the per-run caps under concurrency (D11). Skipped unless UZI_TEST_DATABASE_URL
// is set (./e2e/run-store-it.sh).
//
// Message identity in assertions: a message's input_tokens is 1<<(ordinal-1), so the tail's
// input_tokens sum names exactly WHICH ordinals are in it (tail {4,5} = 8+16 = 24).

type usageTailEnv struct {
	codexTestEnv
	svc    *Service
	wkr    store.Worker
	runID  uuid.UUID
	userID uuid.UUID
	repoID uuid.UUID
}

func setupUsageTail(t *testing.T) usageTailEnv {
	t.Helper()
	env := setupCodexLiveDB(t)
	userID, workerID, repoID := env.seedCodexInfra(t)
	runID := env.seedFoldRun(t, userID, workerID, repoID, 1, "sess-tail")
	svc := New(env.q, env.box, testParams())
	svc.SetTxBeginner(env.pool)
	return usageTailEnv{
		codexTestEnv: env, svc: svc,
		wkr:   store.Worker{ID: workerID, UserID: userID, Status: "online"},
		runID: runID, userID: userID, repoID: repoID,
	}
}

// withUsageCaps lowers the per-run caps for one test and restores them.
func withUsageCaps(t *testing.T, messages, legs int) {
	t.Helper()
	m, l := maxUsageMessagesPerRun, maxUsageLegsPerRun
	maxUsageMessagesPerRun, maxUsageLegsPerRun = messages, legs
	t.Cleanup(func() { maxUsageMessagesPerRun, maxUsageLegsPerRun = m, l })
}

func (e usageTailEnv) leg() uuid.UUID { return uuid.New() }

func i64p(v int64) *int64 { return &v }

// um builds one record: ordinal ord of leg, input tokens 1<<(ord-1), final.
func um(leg uuid.UUID, id string, ord int) UsageRecord {
	return UsageRecord{
		MessageID: id, LegID: leg, Ordinal: int64(ord), Model: "claude-sonnet-5-5",
		InputTokens: 1 << (ord - 1), OutputTokens: 0, OutputFinal: true,
	}
}

func (e usageTailEnv) post(t *testing.T, legs []UsageLegMarker, msgs ...UsageRecord) error {
	t.Helper()
	return e.svc.RecordRunUsage(e.ctx, e.wkr, e.runID, UsageRequest{Legs: legs, Messages: msgs}, nil)
}

func (e usageTailEnv) mustPost(t *testing.T, legs []UsageLegMarker, msgs ...UsageRecord) {
	t.Helper()
	if err := e.post(t, legs, msgs...); err != nil {
		t.Fatalf("RecordRunUsage: %v", err)
	}
}

// batch appends frames through the real append path (so the fold's stamp extension runs).
func (e usageTailEnv) batch(t *testing.T, msgs ...IncomingMessage) {
	t.Helper()
	if err := e.svc.AppendMessagesForClaim(e.ctx, e.wkr, e.runID, msgs, nil); err != nil {
		t.Fatalf("AppendMessagesForClaim: %v", err)
	}
}

func stampedInit(seq int32, leg uuid.UUID, session string) IncomingMessage {
	p, _ := json.Marshal(map[string]any{"event": "init", "leg_id": leg.String(), "sdk_session_id": session})
	return IncomingMessage{Seq: seq, Kind: "status", Payload: p}
}

func stampedResult(seq int32, leg uuid.UUID, through int, cumulative bool, model string, input int64) IncomingMessage {
	m := map[string]any{
		"event": "result", "leg_id": leg.String(), "usage_through": through,
		"modelUsage": map[string]any{model: map[string]any{"inputTokens": input, "outputTokens": 1, "costUSD": 0.5, "costStatus": "metered"}},
	}
	if cumulative {
		m["usage_basis"] = "session_cumulative"
	}
	p, _ := json.Marshal(m)
	return IncomingMessage{Seq: seq, Kind: "status", Payload: p}
}

func closed(leg uuid.UUID, through int64) UsageLegMarker {
	return UsageLegMarker{LegID: leg, ClosedThrough: i64p(through)}
}

func (e usageTailEnv) tail(t *testing.T) *apitypes.UsageTailDTO {
	t.Helper()
	d, err := e.svc.RunUsageTail(e.ctx, e.runID)
	if err != nil {
		t.Fatalf("RunUsageTail: %v", err)
	}
	return d
}

func wantTail(t *testing.T, d *apitypes.UsageTailDTO, inputSum int64, coverage string, reasons ...string) {
	t.Helper()
	if d == nil {
		t.Fatal("RunUsageTail = nil, want a tail")
	}
	if d.InputTokens != inputSum {
		t.Errorf("tail input_tokens = %d, want %d (the ordinals in the tail)", d.InputTokens, inputSum)
	}
	if d.Coverage != coverage {
		t.Errorf("coverage = %q, want %q (reasons %v)", d.Coverage, coverage, d.CoverageReasons)
	}
	if strings.Join(d.CoverageReasons, ",") != strings.Join(reasons, ",") {
		t.Errorf("coverage_reasons = %v, want %v", d.CoverageReasons, reasons)
	}
}

func (e usageTailEnv) count(t *testing.T, table string) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM `+table+` WHERE run_id = $1`, e.runID).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// ---- reconciliation ---------------------------------------------------------------------

// A result covered ordinals 1..3 of a five-message leg, then the leg was interrupted: the tail is
// {4,5} and, because the leg never closed, the coverage says so.
func TestUsageTailResultThenInterruptionLiveDB(t *testing.T) {
	e := setupUsageTail(t)
	a := e.leg()
	e.batch(t, stampedInit(1, a, "S1"))
	e.mustPost(t, nil, um(a, "m1", 1), um(a, "m2", 2), um(a, "m3", 3), um(a, "m4", 4), um(a, "m5", 5))
	e.batch(t, stampedResult(2, a, 3, false, "claude-sonnet-5-5", 7))
	wantTail(t, e.tail(t), 8+16, "partial", UsageTailReasonLegNotClosed)
}

// A fresh-session restart (run 2dc4d842): leg B is a DIFFERENT SDK session, so its cumulative
// result does not include leg A's spend and A's uncovered messages stay in the tail.
func TestUsageTailFreshSessionRestartLiveDB(t *testing.T) {
	e := setupUsageTail(t)
	a, b := e.leg(), e.leg()
	e.batch(t, stampedInit(1, a, "S1"))
	e.mustPost(t, nil, um(a, "a1", 1), um(a, "a2", 2), um(a, "a3", 3), um(a, "a4", 4), um(a, "a5", 5))
	e.batch(t, stampedInit(2, b, "S2"))
	e.mustPost(t, []UsageLegMarker{closed(b, 1)}, um(b, "b1", 1))
	e.batch(t, stampedResult(3, b, 1, true, "claude-sonnet-5-5", 1))
	// A's {1..5} = 31 is counted; B's own message is covered by B's result (arm a).
	wantTail(t, e.tail(t), 31, "partial", UsageTailReasonLegNotClosed)
}

// A same-session resume: B's session-cumulative result supersedes A (arm b), so A's uncovered
// messages are not double counted, whatever order the records and the result arrived in.
func TestUsageTailSameSessionResumeLiveDB(t *testing.T) {
	for _, resultFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("resultFirst=%v", resultFirst), func(t *testing.T) {
			e := setupUsageTail(t)
			a, b := e.leg(), e.leg()
			records := func() {
				e.mustPost(t, []UsageLegMarker{closed(a, 5)}, um(a, "a1", 1), um(a, "a2", 2), um(a, "a3", 3), um(a, "a4", 4), um(a, "a5", 5))
			}
			e.batch(t, stampedInit(1, a, "S1"))
			if !resultFirst {
				records()
			}
			e.batch(t, stampedInit(2, b, "S1"), stampedResult(3, b, 0, true, "claude-sonnet-5-5", 99))
			e.mustPost(t, []UsageLegMarker{closed(b, 0)})
			if resultFirst {
				records()
			}
			// A is closed with every message final: the supersession is trusted, nothing in the tail.
			wantTail(t, e.tail(t), 0, "complete")
		})
	}
}

// Conservative supersession: a superseded leg we never saw close, or holding a message without its
// final usage, makes the coverage partial even though the tail is empty.
func TestUsageTailSupersededUncertainLiveDB(t *testing.T) {
	cases := map[string]struct {
		close *UsageLegMarker
		final bool
		want  []string
	}{
		"not closed":       {close: nil, final: true, want: []string{UsageTailReasonSupersededUncertain}},
		"not final":        {close: &UsageLegMarker{ClosedThrough: i64p(2)}, final: false, want: []string{UsageTailReasonSupersededUncertain}},
		"closed and final": {close: &UsageLegMarker{ClosedThrough: i64p(2)}, final: true, want: nil},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			e := setupUsageTail(t)
			a, b := e.leg(), e.leg()
			e.batch(t, stampedInit(1, a, "S1"))
			m2 := um(a, "a2", 2)
			m2.OutputFinal = c.final
			var legs []UsageLegMarker
			if c.close != nil {
				legs = append(legs, UsageLegMarker{LegID: a, ClosedThrough: c.close.ClosedThrough})
			}
			e.mustPost(t, legs, um(a, "a1", 1), m2)
			e.batch(t, stampedInit(2, b, "S1"), stampedResult(3, b, 0, true, "claude-sonnet-5-5", 5))
			e.mustPost(t, []UsageLegMarker{closed(b, 0)})
			d := e.tail(t)
			if c.want == nil {
				wantTail(t, d, 0, "complete")
			} else {
				wantTail(t, d, 0, "partial", c.want...)
			}
		})
	}
}

// A worker that restarted: A never closed and B started. A's tail survives with leg_not_closed,
// and a missing ordinal in A is an ordinal_gap too.
func TestUsageTailWorkerRestartLiveDB(t *testing.T) {
	e := setupUsageTail(t)
	a, b := e.leg(), e.leg()
	e.batch(t, stampedInit(1, a, "S1"))
	e.mustPost(t, nil, um(a, "a1", 1), um(a, "a2", 2), um(a, "a4", 4)) // ordinal 3 never arrived
	e.batch(t, stampedInit(2, b, "S2"))
	e.mustPost(t, []UsageLegMarker{closed(b, 0)})
	wantTail(t, e.tail(t), 1+2+8, "partial", UsageTailReasonLegNotClosed, UsageTailReasonOrdinalGap)
}

// ---- idempotence, ordering, conflicts ---------------------------------------------------

func TestUsageTailDuplicatesReorderAndGreatestLiveDB(t *testing.T) {
	e := setupUsageTail(t)
	a := e.leg()
	e.batch(t, stampedInit(1, a, "S1"))
	// Reversed ordinals, the same message id three times, and split-frame partial snapshots whose
	// token columns each merge with GREATEST.
	partial := um(a, "m1", 1)
	partial.InputTokens, partial.OutputTokens, partial.OutputFinal = 1, 0, false
	e.mustPost(t, nil, um(a, "m3", 3), um(a, "m2", 2), partial)
	e.mustPost(t, nil, partial, partial)
	final := um(a, "m1", 1)
	final.OutputTokens = 42
	e.mustPost(t, []UsageLegMarker{closed(a, 3)}, final)
	d := e.tail(t)
	wantTail(t, d, 1+2+4, "complete")
	if d.OutputTokens != 42 {
		t.Errorf("output_tokens = %d, want 42 (GREATEST of the split frames)", d.OutputTokens)
	}
	if n := e.count(t, "run_usage_messages"); n != 3 {
		t.Errorf("messages = %d, want 3 (a message id is one row however often it is posted)", n)
	}
	// A stale smaller re-post never regresses a column or un-finalizes it.
	lower := um(a, "m1", 1)
	lower.InputTokens, lower.OutputTokens, lower.OutputFinal = 0, 0, false
	e.mustPost(t, nil, lower)
	if d := e.tail(t); d.OutputTokens != 42 || d.InputTokens != 1+2+4 || d.Coverage != "complete" {
		t.Errorf("after a stale re-post: input %d output %d coverage %s, want 7 42 complete", d.InputTokens, d.OutputTokens, d.Coverage)
	}
}

func TestUsageTailConflictingRepostIsUnresolvedLiveDB(t *testing.T) {
	e := setupUsageTail(t)
	a, other := e.leg(), e.leg()
	e.batch(t, stampedInit(1, a, "S1"), stampedInit(2, other, "S1"))
	e.mustPost(t, []UsageLegMarker{closed(a, 2), closed(other, 0)}, um(a, "m1", 1), um(a, "m2", 2))
	wantTail(t, e.tail(t), 3, "complete")
	// The same id re-posted with a different ordinal: the row stays but is flagged and excluded.
	moved := um(a, "m2", 5)
	e.mustPost(t, nil, moved)
	wantTail(t, e.tail(t), 1, "partial", UsageTailReasonUnresolved)
	// The same id claimed by a different leg is a conflict too.
	e.mustPost(t, nil, um(other, "m1", 1))
	wantTail(t, e.tail(t), 0, "partial", UsageTailReasonUnresolved)
}

func TestUsageTailTwoIDsSameOrdinalIsUnresolvedLiveDB(t *testing.T) {
	e := setupUsageTail(t)
	a := e.leg()
	e.batch(t, stampedInit(1, a, "S1"))
	e.mustPost(t, []UsageLegMarker{closed(a, 2)}, um(a, "m1", 1), um(a, "m2", 2), um(a, "dup", 2))
	wantTail(t, e.tail(t), 1, "partial", UsageTailReasonUnresolved)
}

// A message in a leg that never got its init stamp cannot be placed in the run's order, so it is
// excluded (under-count only) and flagged, until the stamp arrives.
func TestUsageTailLegWithoutInitIsUnresolvedLiveDB(t *testing.T) {
	e := setupUsageTail(t)
	a := e.leg()
	e.mustPost(t, []UsageLegMarker{closed(a, 1)}, um(a, "m1", 1))
	wantTail(t, e.tail(t), 0, "partial", UsageTailReasonUnresolved)
	e.batch(t, stampedInit(1, a, "S1"))
	wantTail(t, e.tail(t), 1, "complete")
}

func TestUsageTailDroppedRecordsFromWorkerLiveDB(t *testing.T) {
	e := setupUsageTail(t)
	a := e.leg()
	e.batch(t, stampedInit(1, a, "S1"))
	e.mustPost(t, []UsageLegMarker{{LegID: a, ClosedThrough: i64p(1), DroppedRecords: i64p(7)}}, um(a, "m1", 1))
	wantTail(t, e.tail(t), 1, "partial", UsageTailReasonRecordsDropped)
	// dropped_records merges with GREATEST: a later smaller figure does not lower it.
	e.mustPost(t, []UsageLegMarker{{LegID: a, DroppedRecords: i64p(2)}})
	var dropped int64
	if err := e.pool.QueryRow(e.ctx, `SELECT dropped_records FROM run_usage_legs WHERE run_id=$1 AND leg_id=$2`, e.runID, a).Scan(&dropped); err != nil || dropped != 7 {
		t.Fatalf("dropped_records = %d (%v), want 7", dropped, err)
	}
}

func TestUsageTailOutputNotFinalLiveDB(t *testing.T) {
	e := setupUsageTail(t)
	a := e.leg()
	e.batch(t, stampedInit(1, a, "S1"))
	m := um(a, "m1", 1)
	m.OutputFinal = false
	e.mustPost(t, []UsageLegMarker{closed(a, 1)}, m)
	wantTail(t, e.tail(t), 1, "partial", UsageTailReasonOutputNotFinal)
}

func TestUsageTailEmptyRunHasNoTailLiveDB(t *testing.T) {
	e := setupUsageTail(t)
	if d := e.tail(t); d != nil {
		t.Fatalf("a run with no legs and no tail state must have no tail, got %+v", d)
	}
}

// ---- pricing ---------------------------------------------------------------------------

func TestUsageTailPricingAndSubagentLiveDB(t *testing.T) {
	e := setupUsageTail(t)
	a := e.leg()
	e.batch(t, stampedInit(1, a, "S1"))
	main := UsageRecord{MessageID: "main1", LegID: a, Ordinal: 1, Model: "claude-sonnet-5-5", InputTokens: 1_000_000, OutputTokens: 1_000_000, OutputFinal: true}
	sub := UsageRecord{MessageID: "sub1", LegID: a, Ordinal: 2, Model: "claude-haiku-4-5", Subagent: true, InputTokens: 1_000_000, OutputFinal: true}
	e.mustPost(t, []UsageLegMarker{closed(a, 2)}, main, sub)
	d := e.tail(t)
	if d.Coverage != "complete" || d.CostStatus != "estimated" || d.CostUSD == nil {
		t.Fatalf("tail = %+v, want complete/estimated with a cost", d)
	}
	// Sonnet 5.5: $2 in + $10 out per MTok; Haiku 4.5 subagent: $1 in per MTok; total $13.
	if *d.CostUSD != 13 {
		t.Errorf("cost_usd = %v, want 13", *d.CostUSD)
	}
	if d.InputTokens != 2_000_000 || len(d.Models) != 2 || d.PriceTableVersion == "" {
		t.Errorf("tail = %+v, want 2M input tokens, two model rows (subagent counted) and a price table version", d)
	}
}

func TestUsageTailUnknownModelIsUnpricedNeverZeroLiveDB(t *testing.T) {
	e := setupUsageTail(t)
	a := e.leg()
	e.batch(t, stampedInit(1, a, "S1"))
	known := UsageRecord{MessageID: "k", LegID: a, Ordinal: 1, Model: "claude-sonnet-5-5", InputTokens: 100, OutputFinal: true}
	unk := UsageRecord{MessageID: "u", LegID: a, Ordinal: 2, Model: "claude-future-9", InputTokens: 50, OutputFinal: true}
	e.mustPost(t, []UsageLegMarker{closed(a, 2)}, known, unk)
	d := e.tail(t)
	if d.InputTokens != 150 {
		t.Errorf("tokens = %d, want 150 (an unpriced model still has tokens)", d.InputTokens)
	}
	if d.CostUSD != nil || d.CostStatus != "unpriced" {
		t.Errorf("cost = %v status %q, want nil/unpriced (never 0)", d.CostUSD, d.CostStatus)
	}
	var perModel = map[string]apitypes.UsageTailModelDTO{}
	for _, m := range d.Models {
		perModel[m.Model] = m
	}
	if perModel["claude-sonnet-5-5"].CostUSD == nil || perModel["claude-future-9"].CostUSD != nil || perModel["claude-future-9"].CostStatus != "unpriced" {
		t.Errorf("per-model rows = %+v, want the known model priced and the unknown one null", perModel)
	}
}

func TestUsageTailNonStandardMarkersAreUnpricedLiveDB(t *testing.T) {
	e := setupUsageTail(t)
	a := e.leg()
	e.batch(t, stampedInit(1, a, "S1"))
	fast := UsageRecord{MessageID: "f", LegID: a, Ordinal: 1, Model: "claude-sonnet-5-5", InputTokens: 1, OutputFinal: true, Speed: "fast"}
	e.mustPost(t, []UsageLegMarker{closed(a, 1)}, fast)
	if d := e.tail(t); d.CostUSD != nil || d.CostStatus != "unpriced" {
		t.Fatalf("a fast-speed message must be unpriced, got %v %q", d.CostUSD, d.CostStatus)
	}
}

// ---- the metered total is untouched ------------------------------------------------------

func TestUsageTailDoesNotChangeMeteredTotalsLiveDB(t *testing.T) {
	e := setupUsageTail(t)
	a := e.leg()
	e.batch(t, stampedInit(1, a, "S1"), stampedResult(2, a, 1, false, "claude-sonnet-5-5", 123))
	beforeOne, err := e.svc.RunUsageTotal(e.ctx, e.runID)
	if err != nil {
		t.Fatal(err)
	}
	beforeMany, err := e.svc.RunUsageTotalsForRuns(e.ctx, []uuid.UUID{e.runID})
	if err != nil {
		t.Fatal(err)
	}
	// A tail with plenty of uncovered spend.
	e.mustPost(t, nil, um(a, "m1", 1), um(a, "m2", 2), um(a, "m3", 3), um(a, "m4", 4))
	if d := e.tail(t); d == nil || d.InputTokens != 2+4+8 {
		t.Fatalf("setup: tail = %+v, want ordinals 2..4", d)
	}
	afterOne, err := e.svc.RunUsageTotal(e.ctx, e.runID)
	if err != nil {
		t.Fatal(err)
	}
	afterMany, err := e.svc.RunUsageTotalsForRuns(e.ctx, []uuid.UUID{e.runID})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(beforeOne, afterOne) {
		t.Errorf("GetRunUsageTotal changed with a tail present:\n before %+v\n after  %+v", beforeOne, afterOne)
	}
	if !reflect.DeepEqual(beforeMany[e.runID], afterMany[e.runID]) {
		t.Errorf("ListRunUsageTotalsForRuns changed with a tail present:\n before %+v\n after  %+v", beforeMany[e.runID], afterMany[e.runID])
	}
	if afterOne.InputTokens != 123 {
		t.Errorf("metered input = %d, want 123 (metered only)", afterOne.InputTokens)
	}
}

// ---- hostile stamps ----------------------------------------------------------------------

func TestUsageTailHostileStampsAreSkippedAndMeteringLandsLiveDB(t *testing.T) {
	e := setupUsageTail(t)
	good := e.leg()
	raw := func(seq int32, payload string) IncomingMessage {
		return IncomingMessage{Seq: seq, Kind: "status", Payload: json.RawMessage(payload)}
	}
	huge := strings.Repeat("x", 5000)
	e.batch(t,
		raw(1, `{"event":"init","leg_id":"not-a-uuid","sdk_session_id":"S"}`),
		raw(2, `{"event":"init","leg_id":"00000000-0000-0000-0000-000000000000","sdk_session_id":"S"}`),
		raw(3, `{"event":"init","leg_id":"`+good.String()+`","sdk_session_id":""}`),
		raw(4, `{"event":"init","leg_id":42,"sdk_session_id":"S"}`),
		raw(5, `{"event":"init","leg_id":"`+good.String()+`","sdk_session_id":{"a":1}}`),
		raw(6, `{"event":"result","leg_id":"`+good.String()+`","usage_through":-1,"modelUsage":{"claude-sonnet-5-5":{"inputTokens":11,"outputTokens":1}}}`),
		raw(7, `{"event":"result","leg_id":"`+good.String()+`","usage_through":99999999999,"modelUsage":{"claude-sonnet-5-5":{"inputTokens":12,"outputTokens":1}}}`),
		raw(8, `{"event":"result","leg_id":"`+good.String()+`","usage_through":1.5,"modelUsage":{"claude-sonnet-5-5":{"inputTokens":13,"outputTokens":1}}}`),
		raw(9, `{"event":"result","leg_id":"`+good.String()+`","usage_through":"3","modelUsage":{"claude-sonnet-5-5":{"inputTokens":14,"outputTokens":1}}}`),
		raw(10, `{"event":"result","leg_id":"`+good.String()+`","usage_through":2}`), // no modelUsage: covers nothing
	)
	if n := e.count(t, "run_usage_legs"); n != 0 {
		t.Fatalf("hostile stamps created %d legs, want 0", n)
	}
	// Metering of the result frames with usable modelUsage still landed (the unchanged fold).
	tot, err := e.svc.RunUsageTotal(e.ctx, e.runID)
	if err != nil || tot.InputTokens == 0 {
		t.Fatalf("metered total = %+v (%v), want the result frames folded", tot, err)
	}
	// An over-long session id is capped, not rejected, and the leg lands.
	e.batch(t, stampedInit(11, good, huge))
	var sess string
	if err := e.pool.QueryRow(e.ctx, `SELECT sdk_session_id FROM run_usage_legs WHERE run_id=$1 AND leg_id=$2`, e.runID, good).Scan(&sess); err != nil {
		t.Fatal(err)
	}
	if len([]rune(sess)) != maxUsageIDRunes {
		t.Errorf("sdk_session_id has %d runes, want the %d cap", len([]rune(sess)), maxUsageIDRunes)
	}
}

// A stamp redelivered, or a straggler arriving late, never moves an established init_seq or
// session, and covered_through only grows.
func TestUsageTailStampUpsertsAreMonotoneLiveDB(t *testing.T) {
	e := setupUsageTail(t)
	a := e.leg()
	e.batch(t, stampedInit(5, a, "S1"))
	e.batch(t, stampedInit(9, a, "OTHER")) // a later conflicting init stamp
	e.batch(t, stampedResult(10, a, 4, false, "claude-sonnet-5-5", 1))
	e.batch(t, stampedResult(11, a, 2, true, "claude-sonnet-5-5", 1)) // lower coverage, cumulative
	var initSeq int64
	var sess string
	var through int32
	var cum bool
	if err := e.pool.QueryRow(e.ctx, `SELECT init_seq, sdk_session_id, covered_through, covered_cumulative FROM run_usage_legs WHERE run_id=$1 AND leg_id=$2`, e.runID, a).Scan(&initSeq, &sess, &through, &cum); err != nil {
		t.Fatal(err)
	}
	if initSeq != 5 || sess != "S1" || through != 4 || !cum {
		t.Fatalf("leg = init_seq %d session %q covered_through %d cumulative %v, want 5 S1 4 true", initSeq, sess, through, cum)
	}
}

// ---- the route's own guards --------------------------------------------------------------

func TestUsageTailStaleClaimWritesNothingLiveDB(t *testing.T) {
	e := setupUsageTail(t)
	a := e.leg()
	gen := int64(1)
	// A claim released (a switch park) fences out even the right generation.
	e.exec(`UPDATE runs SET claim_released_at = now() WHERE id = $1`, e.runID)
	err := e.svc.RecordRunUsage(e.ctx, e.wkr, e.runID, UsageRequest{Messages: []UsageRecord{um(a, "m1", 1)}}, &gen)
	if !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("released claim: err = %v, want ErrStaleClaim", err)
	}
	// So does a reclaim onto a newer generation, for the old flight's generation.
	e.exec(`UPDATE runs SET claim_released_at = NULL, claim_generation = 2 WHERE id = $1`, e.runID)
	err = e.svc.RecordRunUsage(e.ctx, e.wkr, e.runID, UsageRequest{Messages: []UsageRecord{um(a, "m1", 1)}}, &gen)
	if !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("old generation: err = %v, want ErrStaleClaim", err)
	}
	// A generation-less (legacy) report on a released claim is fenced too, like /messages.
	e.exec(`UPDATE runs SET claim_released_at = now() WHERE id = $1`, e.runID)
	if err := e.svc.RecordRunUsage(e.ctx, e.wkr, e.runID, UsageRequest{Messages: []UsageRecord{um(a, "m1", 1)}}, nil); !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("legacy report on a released claim: err = %v, want ErrStaleClaim", err)
	}
	for _, tbl := range []string{"run_usage_legs", "run_usage_messages", "run_usage_tail_state"} {
		if n := e.count(t, tbl); n != 0 {
			t.Errorf("%s has %d rows after fenced posts, want 0", tbl, n)
		}
	}
	// The live generation lands.
	e.exec(`UPDATE runs SET claim_released_at = NULL WHERE id = $1`, e.runID)
	gen2 := int64(2)
	if err := e.svc.RecordRunUsage(e.ctx, e.wkr, e.runID, UsageRequest{Messages: []UsageRecord{um(a, "m1", 1)}}, &gen2); err != nil {
		t.Fatalf("live generation: %v", err)
	}
	if n := e.count(t, "run_usage_messages"); n != 1 {
		t.Fatalf("messages = %d, want 1", n)
	}
}

func TestUsageTailOwnershipChatCodexAndMissingGenerationLiveDB(t *testing.T) {
	e := setupUsageTail(t)
	a := e.leg()
	req := UsageRequest{Messages: []UsageRecord{um(a, "m1", 1)}}
	// Another worker's run is not owned.
	otherWorker := uuid.New()
	e.exec(`INSERT INTO workers (id, user_id, name, token_hash, status) VALUES ($1, $2, $3, $4, 'online')`,
		otherWorker, e.userID, "w-"+otherWorker.String(), otherWorker[:])
	foreign := store.Worker{ID: otherWorker, UserID: e.userID, Status: "online"}
	if err := e.svc.RecordRunUsage(e.ctx, foreign, e.runID, req, nil); !errors.Is(err, ErrRunNotOwned) {
		t.Fatalf("foreign worker: err = %v, want ErrRunNotOwned", err)
	}
	// A Codex-harness run carries no tail.
	e.exec(`UPDATE runs SET harness = 'codex' WHERE id = $1`, e.runID)
	if err := e.svc.RecordRunUsage(e.ctx, e.wkr, e.runID, req, nil); !errors.Is(err, ErrUsageRunUnsupported) {
		t.Fatalf("codex run: err = %v, want ErrUsageRunUnsupported", err)
	}
	e.exec(`UPDATE runs SET harness = 'claude' WHERE id = $1`, e.runID)
	// A chat run carries none either.
	chatRun := uuid.New()
	e.exec(`INSERT INTO runs (id, user_id, kind, issue_title, issue_description, status, worker_id)
	        VALUES ($1, $2, 'chat', 't', 'd', 'running', $3)`, chatRun, e.userID, e.wkr.ID)
	if err := e.svc.RecordRunUsage(e.ctx, e.wkr, chatRun, req, nil); !errors.Is(err, ErrUsageRunUnsupported) {
		t.Fatalf("chat run: err = %v, want ErrUsageRunUnsupported", err)
	}
	// A capability worker must stamp its claim generation, exactly as on /messages.
	capWkr := e.wkr
	capWkr.ProtocolCapabilities = []string{"credential_switch_v1"}
	if err := e.svc.RecordRunUsage(e.ctx, capWkr, e.runID, req, nil); !errors.Is(err, ErrMissingClaimGeneration) {
		t.Fatalf("capability worker without generation: err = %v, want ErrMissingClaimGeneration", err)
	}
	// No fold stamps land for a Codex run either.
	e.exec(`UPDATE runs SET harness = 'codex' WHERE id = $1`, e.runID)
	e.batch(t, stampedInit(1, a, "S1"))
	if n := e.count(t, "run_usage_legs"); n != 0 {
		t.Fatalf("a Codex run's init stamp created %d legs, want 0", n)
	}
}

func TestUsageTailRequestBoundsAndSanitationLiveDB(t *testing.T) {
	e := setupUsageTail(t)
	a := e.leg()
	// 501 records, and 17 leg markers, are refused whole.
	var many []UsageRecord
	for i := 1; i <= maxUsagePostRecords+1; i++ {
		many = append(many, um(a, fmt.Sprintf("m%d", i), 1))
	}
	if err := e.post(t, nil, many...); !errors.Is(err, ErrUsageTooLarge) {
		t.Fatalf("501 records: err = %v, want ErrUsageTooLarge", err)
	}
	var markers []UsageLegMarker
	for i := 0; i <= maxUsagePostLegs; i++ {
		markers = append(markers, UsageLegMarker{LegID: uuid.New()})
	}
	if err := e.post(t, markers); !errors.Is(err, ErrUsageTooLarge) {
		t.Fatalf("17 markers: err = %v, want ErrUsageTooLarge", err)
	}
	// Exactly the bound lands.
	if err := e.post(t, nil, many[:maxUsagePostRecords]...); err != nil {
		t.Fatalf("500 records: %v", err)
	}
	// Malformed values are refused, writing nothing from that request.
	bad := um(a, "bad", 1)
	bad.Ordinal = 0
	neg := um(a, "neg", 1)
	neg.InputTokens = -1
	for name, rec := range map[string]UsageRecord{"ordinal 0": bad, "negative tokens": neg, "empty id": um(a, "\x00", 1), "nil leg": um(uuid.Nil, "x", 1)} {
		if err := e.post(t, nil, rec); !errors.Is(err, ErrUsageInvalid) {
			t.Errorf("%s: err = %v, want ErrUsageInvalid", name, err)
		}
	}
	// NUL bytes are stripped and over-long text is capped, never a 500.
	nul := um(a, "n\x00ul", 2)
	nul.Model = "claude-\x00sonnet-5-5"
	nul.ServiceTier = strings.Repeat("t", 500)
	if err := e.post(t, nil, nul); err != nil {
		t.Fatalf("NUL record: %v", err)
	}
	var id, model, tier string
	if err := e.pool.QueryRow(e.ctx, `SELECT message_id, model, service_tier FROM run_usage_messages WHERE run_id=$1 AND message_id = 'nul'`, e.runID).Scan(&id, &model, &tier); err != nil {
		t.Fatalf("sanitized row not found: %v", err)
	}
	if model != "claude-sonnet-5-5" || len([]rune(tier)) != maxUsageMarkerRunes {
		t.Errorf("model %q tier runes %d, want NUL-stripped model and a %d-rune tier", model, len([]rune(tier)), maxUsageMarkerRunes)
	}
}

// ---- per-run caps ------------------------------------------------------------------------

// Concurrent posts of disjoint new ids at the message cap never exceed it, and overflow is
// recorded: never complete.
func TestUsageTailMessageCapUnderConcurrencyLiveDB(t *testing.T) {
	const capMsgs, workers, perPost = 10, 8, 5
	withUsageCaps(t, capMsgs, 500)
	e := setupUsageTail(t)
	a := e.leg()
	e.batch(t, stampedInit(1, a, "S1"))
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			var recs []UsageRecord
			for i := 0; i < perPost; i++ {
				recs = append(recs, UsageRecord{MessageID: fmt.Sprintf("w%d-m%d", w, i), LegID: a, Ordinal: int64(i + 1), Model: "claude-sonnet-5-5", OutputFinal: true})
			}
			errs <- e.svc.RecordRunUsage(context.Background(), e.wkr, e.runID, UsageRequest{Legs: []UsageLegMarker{closed(a, perPost)}, Messages: recs}, nil)
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent post: %v", err)
		}
	}
	if n := e.count(t, "run_usage_messages"); n != capMsgs {
		t.Fatalf("messages = %d, want exactly the cap %d", n, capMsgs)
	}
	d := e.tail(t)
	if d.Coverage != "partial" || !slices.Contains(d.CoverageReasons, UsageTailReasonRecordCapReached) {
		t.Fatalf("coverage = %s %v, want partial with record_cap_reached (never complete)", d.Coverage, d.CoverageReasons)
	}
	var capped int64
	if err := e.pool.QueryRow(e.ctx, `SELECT capped_records FROM run_usage_tail_state WHERE run_id=$1`, e.runID).Scan(&capped); err != nil || capped == 0 {
		t.Fatalf("capped_records = %d (%v), want > 0", capped, err)
	}
	// An update to an existing row is allowed at the cap.
	upd := UsageRecord{MessageID: "w0-m0", LegID: a, Ordinal: 1, Model: "claude-sonnet-5-5", OutputTokens: 77, OutputFinal: true}
	if err := e.post(t, nil, upd); err != nil {
		t.Fatal(err)
	}
}

func TestUsageTailLegCapUnderConcurrencyLiveDB(t *testing.T) {
	const capLegs, workers = 3, 10
	withUsageCaps(t, 20000, capLegs)
	e := setupUsageTail(t)
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// A leg marker alone: the cap blocks the LEG, with no message dropped at all.
			errs <- e.svc.RecordRunUsage(context.Background(), e.wkr, e.runID, UsageRequest{Legs: []UsageLegMarker{closed(uuid.New(), 0)}}, nil)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent post: %v", err)
		}
	}
	if n := e.count(t, "run_usage_legs"); n != capLegs {
		t.Fatalf("legs = %d, want exactly the cap %d", n, capLegs)
	}
	d := e.tail(t)
	if d == nil || !slices.Contains(d.CoverageReasons, UsageTailReasonRecordCapReached) || d.Coverage != "partial" {
		t.Fatalf("legs-cap-only overflow: %+v, want partial with record_cap_reached", d)
	}
}

// The fold's stamp writes obey the same legs cap: past it the leg is skipped, record_cap_reached
// is set, and the append and the metering still succeed.
func TestUsageTailFoldLegCapSkipsAndMeteringLandsLiveDB(t *testing.T) {
	withUsageCaps(t, 20000, 2)
	e := setupUsageTail(t)
	l1, l2, l3 := e.leg(), e.leg(), e.leg()
	e.batch(t, stampedInit(1, l1, "S1"), stampedInit(2, l2, "S1"), stampedInit(3, l3, "S1"), stampedResult(4, l3, 1, false, "claude-sonnet-5-5", 55))
	if n := e.count(t, "run_usage_legs"); n != 2 {
		t.Fatalf("legs = %d, want the cap 2", n)
	}
	tot, err := e.svc.RunUsageTotal(e.ctx, e.runID)
	if err != nil || tot.InputTokens != 55 {
		t.Fatalf("metered total = %+v (%v), want 55 input (metering never blocked by the cap)", tot, err)
	}
	d := e.tail(t)
	if d == nil || !slices.Contains(d.CoverageReasons, UsageTailReasonRecordCapReached) {
		t.Fatalf("tail = %+v, want record_cap_reached", d)
	}
}

// Concurrent folds that each introduce a new leg stay within the legs cap: the stamp
// transaction takes the per-run lock first, then counts.
func TestUsageTailFoldLegCapUnderConcurrencyLiveDB(t *testing.T) {
	const capLegs, appenders = 3, 12
	withUsageCaps(t, 20000, capLegs)
	e := setupUsageTail(t)
	var wg sync.WaitGroup
	errs := make(chan error, appenders)
	for i := int32(1); i <= appenders; i++ {
		wg.Add(1)
		go func(seq int32) {
			defer wg.Done()
			errs <- e.svc.AppendMessagesForClaim(context.Background(), e.wkr, e.runID, []IncomingMessage{stampedInit(seq, uuid.New(), "S1")}, nil)
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent append: %v", err)
		}
	}
	if n := e.count(t, "run_usage_legs"); n != capLegs {
		t.Fatalf("legs = %d, want exactly the cap %d", n, capLegs)
	}
	if d := e.tail(t); d == nil || !slices.Contains(d.CoverageReasons, UsageTailReasonRecordCapReached) {
		t.Fatalf("tail = %+v, want record_cap_reached", d)
	}
}
