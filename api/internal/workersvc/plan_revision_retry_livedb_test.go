package workersvc

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func seedRevisionTarget(t *testing.T, f *gateFix, run uuid.UUID, source int64) int64 {
	t.Helper()
	var id int64
	if err := f.pool.QueryRow(f.ctx, `INSERT INTO run_user_inputs
 (run_id, kind, body, gate_binding, gate_revision, consumed_at, consumed_worker_id, consumed_claim_generation)
 VALUES ($1, 'revise_plan', 'revise', 'bound', $2, now(), $3, 1) RETURNING id`, run, source, f.wkr.ID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestPlanRevisionRetrySettlementLiveDB(t *testing.T) {
	f := newGateFix(t, 1)
	run := f.newRun("issue")
	source := gateReq(i64(1), uid())
	f.mustPublish(run, source, 1)
	spare := seedRevisionTarget(t, f, run, 1)
	predecessor := seedRevisionTarget(t, f, run, 1)
	active := seedRevisionTarget(t, f, run, 1)
	req := gateReq(i64(1), uid())
	plan := "# Revised plan"
	req.PlanMd, req.ExpectedGateRevision, req.RevisionInputID = &plan, i64(1), &active
	req.SupersededRevisionInputIDs = []int64{predecessor}
	result, err := f.svc.SetStateReportWithReconciliation(f.ctx, f.wkr, run, req)
	if err != nil || !result.Applied || result.GateRevision != 2 ||
		result.RevisionInputID == nil || *result.RevisionInputID != active ||
		!reflect.DeepEqual(result.SupersededRevisionInputIDs, []int64{predecessor}) {
		t.Fatalf("publication: %+v %v", result, err)
	}
	var settled bool
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) = 2 AND bool_and(applied_at IS NOT NULL)
 AND bool_and(CASE WHEN id = $2 THEN disposition = 'superseded' ELSE disposition IS DISTINCT FROM 'superseded' END)
 FROM run_user_inputs WHERE run_id = $1 AND id = ANY($3::bigint[])`, run, predecessor, []int64{predecessor, active}).Scan(&settled); err != nil || !settled {
		t.Fatalf("settlement=%v err=%v", settled, err)
	}
	// The same plan published without receipt metadata has the same approval digest
	// and candidate snapshot. Retaining the presentation must preserve replay proof.
	plainRun := f.newRun("issue")
	plainReq := req
	plainReq.ExpectedGateRevision, plainReq.RevisionInputID, plainReq.SupersededRevisionInputIDs = nil, nil, nil
	f.mustPublish(plainRun, plainReq, 1)
	published, plain := f.row(run), f.row(plainRun)
	if !reflect.DeepEqual(published.digest, plain.digest) || !reflect.DeepEqual(published.candidate, plain.candidate) {
		t.Fatal("receipt changed approval digest or candidate")
	}
	f.mustPublish(run, plainReq, 2)
	retained := f.row(run)
	if !reflect.DeepEqual(published.payload, retained.payload) || !reflect.DeepEqual(published.digest, retained.digest) || !reflect.DeepEqual(published.candidate, retained.candidate) {
		t.Fatal("retain changed snapshot or receipt proof")
	}
	var before string
	if err := f.pool.QueryRow(f.ctx, `SELECT applied_at::text FROM run_user_inputs WHERE id=$1`, active).Scan(&before); err != nil {
		t.Fatal(err)
	}
	replay, err := f.svc.SetStateReportWithReconciliation(f.ctx, f.wkr, run, req)
	if err != nil || !replay.Applied || replay.GateRevision != 2 || replay.RevisionInputID == nil || *replay.RevisionInputID != active {
		t.Fatalf("replay: %+v %v", replay, err)
	}
	var after string
	if err := f.pool.QueryRow(f.ctx, `SELECT applied_at::text FROM run_user_inputs WHERE id=$1`, active).Scan(&after); err != nil || before != after {
		t.Fatalf("replay changed settlement: %q %q %v", before, after, err)
	}
	for _, mode := range []string{"extra target", "historical", "changed payload"} {
		t.Run(mode, func(t *testing.T) {
			stale := req
			switch mode {
			case "extra target":
				// A pending older target cannot be swept into an already committed receipt.
				stale.SupersededRevisionInputIDs = []int64{spare, predecessor}
			case "historical":
				stale.PresentationID = source.PresentationID
			case "changed payload":
				changed := "# Changed"
				stale.PlanMd = &changed
			}
			_, applied, _, err := f.report(run, stale)
			if !errors.Is(err, ErrStaleRevisionSource) || applied {
				t.Fatalf("refusal: applied=%v err=%v", applied, err)
			}
			row := f.row(run)
			if row.revision != 2 || row.refusals != 0 || row.status != "awaiting_approval" {
				t.Fatalf("refusal changed newer plan: %+v", row)
			}
		})
	}
}

func TestPlanRevisionRetryRequiresPersistablePlanLiveDB(t *testing.T) {
	planPointer := func(body string) *string { return &body }
	for _, value := range []*string{nil, planPointer(""), planPointer(" \n\t"), planPointer("\x00")} {
		f := newGateFix(t, 1)
		run := f.newRun("issue")
		f.mustPublish(run, gateReq(i64(1), uid()), 1)
		active := seedRevisionTarget(t, f, run, 1)
		req := gateReq(i64(1), uid())
		req.PlanMd, req.ExpectedGateRevision, req.RevisionInputID = value, i64(1), &active
		if _, applied, _, err := f.report(run, req); !errors.Is(err, ErrInvalidState) || applied {
			t.Fatalf("missing plan accepted: applied=%v err=%v", applied, err)
		}
		var pending bool
		if err := f.pool.QueryRow(f.ctx, "SELECT applied_at IS NULL FROM run_user_inputs WHERE id=$1", active).Scan(&pending); err != nil || !pending {
			t.Fatalf("missing plan settled feedback: pending=%v err=%v", pending, err)
		}
		if row := f.row(run); row.revision != 1 || row.refusals != 0 {
			t.Fatalf("missing plan changed the gate: %+v", row)
		}
	}
}

func TestPlanRevisionRetryValidationAndRestorationLiveDB(t *testing.T) {
	f := newGateFix(t, 1)
	run := f.newRun("issue")
	source := gateReq(i64(1), uid())
	f.mustPublish(run, source, 1)
	active := seedRevisionTarget(t, f, run, 1)
	req := gateReq(i64(1), uid())
	req.ExpectedGateRevision, req.RevisionInputID = i64(1), &active
	// A current publication must not persist if one target is not ACKed by this claim.
	f.exec(`UPDATE run_user_inputs SET consumed_worker_id=$2 WHERE id=$1`, active, uuid.New())
	if _, applied, _, err := f.report(run, req); !errors.Is(err, ErrStaleRevisionSource) || applied {
		t.Fatalf("wrong ACK: %v %v", applied, err)
	}
	if row := f.row(run); row.revision != 1 || row.refusals != 0 {
		t.Fatalf("failed publication mutated: %+v", row)
	}
	f.exec(`UPDATE runs SET status='awaiting_input', status_since=now() - interval '1 hour' WHERE id=$1`, run)
	source.ExpectedGateRevision = i64(1)
	f.mustPublish(run, source, 1)
	var pending bool
	if err := f.pool.QueryRow(f.ctx, `SELECT applied_at IS NULL FROM run_user_inputs WHERE id=$1`, active).Scan(&pending); err != nil || !pending {
		t.Fatalf("restoration settled input: %v %v", pending, err)
	}
}

func TestPlanRevisionRetryControlsLiveDB(t *testing.T) {
	for _, mode := range []string{"stale claim", "released", "paused", "terminal", "wrong target source", "duplicate predecessor"} {
		t.Run(mode, func(t *testing.T) {
			f := newGateFix(t, 1)
			run := f.newRun("issue")
			f.mustPublish(run, gateReq(i64(1), uid()), 1)
			predecessor := seedRevisionTarget(t, f, run, 1)
			active := seedRevisionTarget(t, f, run, 1)
			req := gateReq(i64(1), uid())
			req.ExpectedGateRevision, req.RevisionInputID = i64(1), &active
			req.SupersededRevisionInputIDs = []int64{predecessor}
			var want error
			switch mode {
			case "stale claim":
				req.ClaimGeneration = i64(2)
				want = ErrStaleClaim
			case "released":
				f.exec("UPDATE runs SET claim_released_at=now() WHERE id=$1", run)
				want = ErrStaleClaim
			case "paused":
				f.exec("UPDATE runs SET status='paused' WHERE id=$1", run)
			case "terminal":
				f.exec("UPDATE runs SET status='cancelled' WHERE id=$1", run)
			case "wrong target source":
				f.exec("UPDATE run_user_inputs SET gate_revision=2 WHERE id=$1", predecessor)
				want = ErrStaleRevisionSource
			case "duplicate predecessor":
				req.SupersededRevisionInputIDs = []int64{predecessor, predecessor}
				want = ErrInvalidState
			}
			before := f.row(run)
			_, applied, _, err := f.report(run, req)
			if applied || !errors.Is(err, want) {
				t.Fatalf("control: applied=%v err=%v want=%v", applied, err, want)
			}
			if after := f.row(run); !reflect.DeepEqual(before, after) {
				t.Fatalf("declined report mutated run: before=%+v after=%+v", before, after)
			}
			var pending bool
			if err := f.pool.QueryRow(f.ctx, "SELECT bool_and(applied_at IS NULL) FROM run_user_inputs WHERE run_id=$1", run).Scan(&pending); err != nil || !pending {
				t.Fatalf("declined report settled targets: %v %v", pending, err)
			}
		})
	}
}

// revisionSourceRaceStore advances the committed source between the initial
// ownership read and SetState's locking read. It fires once and never retries.
type revisionSourceRaceStore struct {
	Store
	afterRead func()
}

func (q *revisionSourceRaceStore) GetRunOwnedByWorker(ctx context.Context, arg store.GetRunOwnedByWorkerParams) (store.Run, error) {
	run, err := q.Store.GetRunOwnedByWorker(ctx, arg)
	if err == nil && q.afterRead != nil {
		hook := q.afterRead
		q.afterRead = nil
		hook()
	}
	return run, err
}

func TestPlanRevisionRetrySourceLockRaceLiveDB(t *testing.T) {
	for _, state := range []string{"awaiting_approval", "failed"} {
		t.Run(state, func(t *testing.T) {
			f := newGateFix(t, 1)
			run := f.newRun("issue")
			f.mustPublish(run, gateReq(i64(1), uid()), 1)
			req := StateRequest{State: state, ClaimGeneration: i64(1), ExpectedGateRevision: i64(1)}
			if state == "awaiting_approval" {
				req = gateReq(i64(1), uid())
				req.ExpectedGateRevision = i64(1)
				id := seedRevisionTarget(t, f, run, 1)
				req.RevisionInputID = &id
			} else {
				f.exec(`UPDATE runs SET stop_kind='plan_rejected' WHERE id=$1`, run)
			}
			f.svc.q = &revisionSourceRaceStore{Store: f.q, afterRead: func() {
				f.exec(`UPDATE runs SET gate_revision=2, gate_presentation_id=$2 WHERE id=$1`, run, uuid.New())
			}}
			_, applied, _, err := f.report(run, req)
			if !errors.Is(err, ErrStaleRevisionSource) || applied {
				t.Fatalf("stale source race: applied=%v err=%v", applied, err)
			}
			row := f.row(run)
			if row.revision != 2 || row.status != "awaiting_approval" || row.refusals != 0 {
				t.Fatalf("race changed winner: %+v", row)
			}
		})
	}
}
