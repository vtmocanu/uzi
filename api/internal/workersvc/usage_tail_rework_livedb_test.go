package workersvc

import (
	"context"
	"encoding/json"
	"math"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// usage_tail_rework_livedb_test.go pins the m2 validator findings (issue #2014): the legs-cap
// foreign key, the lane guard, the stamp-versus-metered acceptance, and the writers'
// isolation level. Skipped unless UZI_TEST_DATABASE_URL is set (./e2e/run-store-it.sh).

// A new message id posted under an admitted leg AND under a leg the legs cap refused must not
// write the refused-leg copy (its (run_id, leg_id) foreign key has no row): a 2xx, never a 500,
// with the dropped record counted.
func TestUsageTailNewIDUnderAdmittedAndRefusedLegLiveDB(t *testing.T) {
	withUsageCaps(t, 20000, 3)
	e := setupUsageTail(t)
	e.mustPost(t, []UsageLegMarker{closed(e.leg(), 0), closed(e.leg(), 0)}) // 2 legs: one slot left
	a, b := e.leg(), e.leg()
	for b.String() >= a.String() { // the refused leg sorts FIRST in the write order
		a, b = e.leg(), e.leg()
	}
	if err := e.post(t, nil, um(a, "X", 1), um(b, "X", 2)); err != nil {
		t.Fatalf("post under an admitted and a refused leg = %v, want success", err)
	}
	var legOfX uuid.UUID
	if err := e.pool.QueryRow(e.ctx, `SELECT leg_id FROM run_usage_messages WHERE run_id=$1 AND message_id='X'`, e.runID).Scan(&legOfX); err != nil || legOfX != a {
		t.Fatalf("X is under leg %v (%v), want the admitted leg %v", legOfX, err, a)
	}
	if n := e.count(t, "run_usage_legs"); n != 3 {
		t.Fatalf("legs = %d, want 3", n)
	}
	var capped int64
	if err := e.pool.QueryRow(e.ctx, `SELECT capped_records FROM run_usage_tail_state WHERE run_id=$1 AND record_cap_reached`, e.runID).Scan(&capped); err != nil || capped < 1 {
		t.Fatalf("capped_records = %d (%v), want the dropped record counted with record_cap_reached", capped, err)
	}
}

// An isolated-lane (profile-bound) run takes no stamps: the route is closed to lane workers, and
// the fold must not grow legs for such a run either.
func TestUsageTailLaneRunStampsCreateNoLegsLiveDB(t *testing.T) {
	e := setupUsageTail(t)
	run, err := e.q.GetRunOwnedByWorker(e.ctx, store.GetRunOwnedByWorkerParams{ID: e.runID, WorkerID: pgtype.UUID{Bytes: e.wkr.ID, Valid: true}})
	if err != nil {
		t.Fatal(err)
	}
	run.EgressProfileID = pgtype.UUID{Bytes: uuid.New(), Valid: true}
	a := e.leg()
	if err := e.svc.foldRunUsage(e.ctx, run, []IncomingMessage{stampedInit(1, a, "S1"), stampedResult(2, a, 1, false, "claude-sonnet-5-5", 5)}); err != nil {
		t.Fatal(err)
	}
	if n := e.count(t, "run_usage_legs"); n != 0 {
		t.Fatalf("a lane run's stamps created %d legs, want 0", n)
	}
	if tot, err := e.svc.RunUsageTotal(e.ctx, e.runID); err != nil || tot.InputTokens != 5 {
		t.Fatalf("metered total = %+v (%v), want the metered fold unchanged (5)", tot, err)
	}
}

// A result frame the metered fold skips must not set covered_through: its messages stay in the
// tail. Two skip shapes: a non-string usage_basis (the frame fails resultUsagePayload's decode)
// and a model key of "".
func TestUsageTailUnmeteredResultFramesCoverNothingLiveDB(t *testing.T) {
	frames := map[string]string{
		"usage_basis not a string": `"usage_basis":5,"modelUsage":{"claude-sonnet-5-5":{"inputTokens":9,"outputTokens":1}}`,
		"empty model key":          `"modelUsage":{"":{"inputTokens":9,"outputTokens":1}}`,
	}
	for name, body := range frames {
		t.Run(name, func(t *testing.T) {
			e := setupUsageTail(t)
			a := e.leg()
			e.batch(t, stampedInit(1, a, "S1"))
			e.mustPost(t, []UsageLegMarker{closed(a, 3)}, um(a, "m1", 1), um(a, "m2", 2), um(a, "m3", 3))
			e.batch(t, IncomingMessage{Seq: 2, Kind: "status", Payload: json.RawMessage(`{"event":"result","leg_id":"` + a.String() + `","usage_through":3,` + body + `}`)})
			var covered *int32
			if err := e.pool.QueryRow(e.ctx, `SELECT covered_through FROM run_usage_legs WHERE run_id=$1 AND leg_id=$2`, e.runID, a).Scan(&covered); err != nil || covered != nil {
				t.Fatalf("covered_through = %v (%v), want NULL: the frame was not metered", covered, err)
			}
			wantTail(t, e.tail(t), 1+2+4, "complete")
		})
	}
}

// satAdd-backed sums: huge token counts clamp at MaxInt64 instead of wrapping negative.
func TestUsageTailTokenSumsSaturate(t *testing.T) {
	row := func(in int64) store.ListRunUsageTailMessagesRow {
		return store.ListRunUsageTailMessagesRow{Model: "claude-sonnet-5-5", InputTokens: in, OutputFinal: true}
	}
	d := buildUsageTail(nil, []store.ListRunUsageTailMessagesRow{row(math.MaxInt64), row(math.MaxInt64), row(2)}, false)
	if d.InputTokens != math.MaxInt64 || len(d.Models) != 1 || d.Models[0].InputTokens != math.MaxInt64 {
		t.Fatalf("total %d, per-model %+v, want both clamped at MaxInt64", d.InputTokens, d.Models)
	}
}

// isolationRecordingBeginner begins a REPEATABLE READ transaction (a pool whose default is
// stricter than the writers need) and records the isolation level seen just before commit.
type isolationRecordingBeginner struct {
	pool interface {
		BeginTx(ctx context.Context, o pgx.TxOptions) (pgx.Tx, error)
	}
	seen *[]string
}

type isolationTx struct {
	pgx.Tx
	seen *[]string
}

func (b isolationRecordingBeginner) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := b.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return nil, err
	}
	return isolationTx{Tx: tx, seen: b.seen}, nil
}

func (t isolationTx) Commit(ctx context.Context) error {
	var lvl string
	if err := t.Tx.QueryRow(ctx, "SHOW transaction_isolation").Scan(&lvl); err != nil {
		return err
	}
	*t.seen = append(*t.seen, lvl)
	return t.Tx.Commit(ctx)
}

// Both writers run READ COMMITTED whatever the pool default: the cap counts taken under the
// advisory lock must see what the previous lock holder committed.
func TestUsageTailWritersRunReadCommittedLiveDB(t *testing.T) {
	e := setupUsageTail(t)
	var seen []string
	e.svc.SetTxBeginner(isolationRecordingBeginner{pool: e.pool, seen: &seen})
	a := e.leg()
	e.batch(t, stampedInit(1, a, "S1")) // the fold's stamp transaction
	e.mustPost(t, []UsageLegMarker{closed(a, 1)}, um(a, "m1", 1))
	if !slices.Equal(seen, []string{"read committed", "read committed"}) {
		t.Fatalf("isolation seen at commit = %v, want read committed for the stamp tx and the route tx", seen)
	}
}
