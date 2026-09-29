package workersvc

import (
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// lane_purpose_livedb_test.go pins PRD #1906 M5's lane purpose check (Decision D-D) on the two
// worker routes whose authorization is NOT runOwnedByWorker: the input receipts (ack, applied,
// discarded share inputReceipt and LockRunForInputReceipt) and the message-gap read (one SQL
// statement). A run held by a worker on the other side of the isolated lane is a state the claim
// clauses never produce, so each case writes it directly and asserts the not-owned answer, then
// moves the run to an agreeing worker as the positive control. Skipped unless
// UZI_TEST_DATABASE_URL points at a throwaway Postgres.

// lanePurposeCase is one mismatched pair: a run (bound or not) held by wrong, and the agreeing
// worker right that the positive control moves it to.
type lanePurposeCase struct {
	name  string
	run   uuid.UUID
	wrong store.Worker
	right store.Worker
}

// lanePurposeCases seeds both mismatch shapes: a profile-bound run held by an ordinary worker,
// and an unbound run held by a lane worker. Each run is running at claim generation 1 with one
// unconsumed follow_up input, whose id is returned alongside.
func lanePurposeCases(t *testing.T, f *isoFix) ([]lanePurposeCase, map[uuid.UUID]int64) {
	t.Helper()
	caps := []string{capability.InputReceiptsV1, capability.IsolatedFetchV1}
	ordinary := f.seedWorker(t, laneWorkerSpec{protoCaps: caps})
	running := func(iid int64, bound bool, holder uuid.UUID) uuid.UUID {
		id := f.queuedRun(t, iid, bound)
		f.env.exec(`UPDATE runs SET status = 'running', worker_id = $2, claim_generation = 1 WHERE id = $1`, id, holder)
		return id
	}
	boundRun := f.queuedRun(t, 501, true)
	lane := f.seedWorker(t, laneWorkerSpec{hosted: true, ephemeral: true, boundRun: boundRun, isolated: true, protoCaps: caps})
	f.env.exec(`UPDATE runs SET status = 'running', worker_id = $2, claim_generation = 1 WHERE id = $1`, boundRun, ordinary.ID)
	unboundRun := running(502, false, lane.ID)
	inputs := map[uuid.UUID]int64{}
	for _, run := range []uuid.UUID{boundRun, unboundRun} {
		var id int64
		if err := f.env.pool.QueryRow(f.env.ctx, `INSERT INTO run_user_inputs (run_id, kind, body) VALUES ($1, 'follow_up', 'secret steering text') RETURNING id`, run).Scan(&id); err != nil {
			t.Fatalf("seed input: %v", err)
		}
		inputs[run] = id
	}
	return []lanePurposeCase{
		{name: "bound run, ordinary worker", run: boundRun, wrong: ordinary, right: lane},
		{name: "unbound run, lane worker", run: unboundRun, wrong: lane, right: ordinary},
	}, inputs
}

// hand moves run to holder at the same generation, as if the claim had placed it there.
func (f *isoFix) hand(run uuid.UUID, holder store.Worker) {
	f.env.exec(`UPDATE runs SET worker_id = $2 WHERE id = $1`, run, holder.ID)
}

func TestInputReceiptsLanePurposeCheckLiveDB(t *testing.T) {
	f := newIsoFix(t)
	cases, inputs := lanePurposeCases(t, f)
	for _, tc := range cases {
		ids := []int64{inputs[tc.run]}
		for mode, call := range map[string]func(store.Worker) (InputReceiptResult, error){
			"ack": func(w store.Worker) (InputReceiptResult, error) { return f.svc.AckInputs(f.env.ctx, w, tc.run, 1, ids) },
			"applied": func(w store.Worker) (InputReceiptResult, error) {
				return f.svc.ApplyInputs(f.env.ctx, w, tc.run, 1, ids)
			},
			"discarded": func(w store.Worker) (InputReceiptResult, error) {
				return f.svc.DiscardInputs(f.env.ctx, w, tc.run, 1, ids)
			},
		} {
			res, err := call(tc.wrong)
			if !errors.Is(err, ErrRunNotOwned) || len(res.Inputs) != 0 {
				t.Errorf("%s: %s = %+v, %v; want ErrRunNotOwned and no input bodies", tc.name, mode, res, err)
			}
		}
		var consumed pgtype.Timestamptz
		if err := f.env.pool.QueryRow(f.env.ctx, `SELECT consumed_at FROM run_user_inputs WHERE id = $1`, ids[0]).Scan(&consumed); err != nil {
			t.Fatal(err)
		}
		if consumed.Valid {
			t.Errorf("%s: a refused receipt stamped the input consumed", tc.name)
		}
		// Positive control: the agreeing worker's ACK returns the body.
		f.hand(tc.run, tc.right)
		res, err := f.svc.AckInputs(f.env.ctx, tc.right, tc.run, 1, ids)
		if err != nil || len(res.Inputs) != 1 || !res.Active {
			t.Errorf("%s: agreeing worker's ack = %+v, %v; want the active input", tc.name, res, err)
		}
	}
}

func TestRunMessageGapsLanePurposeCheckLiveDB(t *testing.T) {
	f := newIsoFix(t)
	cases, _ := lanePurposeCases(t, f)
	for _, tc := range cases {
		if _, err := f.svc.RunMessageGaps(f.env.ctx, tc.wrong, tc.run, 1, 5, 0, 10); !errors.Is(err, ErrRunNotOwned) {
			t.Errorf("%s: RunMessageGaps = %v, want ErrRunNotOwned", tc.name, err)
		}
		f.hand(tc.run, tc.right)
		page, err := f.svc.RunMessageGaps(f.env.ctx, tc.right, tc.run, 1, 5, 0, 10)
		if err != nil || len(page.Gaps) != 1 || page.Gaps[0] != (MessageGap{First: 1, Last: 5}) {
			t.Errorf("%s: agreeing worker's RunMessageGaps = %+v, %v; want the one gap [1..5]", tc.name, page, err)
		}
	}
}
