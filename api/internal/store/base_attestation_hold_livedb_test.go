package store_test

import (
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// issue #3194 M4: the base-attestation hold keeps its worker and live claim (custody stays there),
// so the worker-report writers that admit a paused row on (worker, unreleased claim) must refuse
// it, as they refuse the other worker-side holds. Each case runs the same report against an owner
// pause (NULL hold_reason) as the control that shows the hold reason is what refuses it. Skipped
// unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (./e2e/run-store-it.sh).

func (fx *wpFixture) heldRun(worker uuid.UUID, holdReason string) uuid.UUID {
	fx.t.Helper()
	w := worker
	id := fx.run(wpRun{status: "paused", worker: &w, claimGen: 3, holdReason: holdReason})
	mustExec(fx.ctx, fx.t, fx.pool, `UPDATE runs SET require_base_match = true, planned_base_commit = 'abcdef1234567' WHERE id = $1`, id)
	return id
}

func TestBaseAttestationHoldRefusesLateWorkerReportsLiveDB(t *testing.T) {
	fx := newWPFixture(t)
	worker := fx.worker("w-3194", wpWorker{})
	wid := pgtype.UUID{Bytes: worker, Valid: true}

	reports := map[string]func(id uuid.UUID) (int64, error){
		"SetRunAwaitingInput": func(id uuid.UUID) (int64, error) {
			return fx.q.SetRunAwaitingInput(fx.ctx, store.SetRunAwaitingInputParams{
				OpenQuestionID: pgtype.Text{String: "q1", Valid: true}, ID: id, WorkerID: wid})
		},
		"SetRunAwaitingFollowup": func(id uuid.UUID) (int64, error) {
			return fx.q.SetRunAwaitingFollowup(fx.ctx, store.SetRunAwaitingFollowupParams{ID: id, WorkerID: wid})
		},
		"SupersedeRunByWorker": func(id uuid.UUID) (int64, error) {
			return fx.q.SupersedeRunByWorker(fx.ctx, store.SupersedeRunByWorkerParams{StopReason: "branch moved", ID: id, WorkerID: wid})
		},
	}
	for name, report := range reports {
		t.Run(name, func(t *testing.T) {
			held := fx.heldRun(worker, "base_attestation_missing")
			n, err := report(held)
			if err != nil || n != 0 {
				t.Fatalf("%s on the base-attestation hold = (%d, %v), want (0, nil)", name, n, err)
			}
			if s := fx.status(held); s != "paused" {
				t.Fatalf("held run status = %q, want paused", s)
			}
			control := fx.heldRun(worker, "")
			if n, err := report(control); err != nil || n != 1 {
				t.Fatalf("control: %s on an owner pause = (%d, %v), want (1, nil)", name, n, err)
			}
			// Positive control: the ordinary current-worker transition from running, same worker,
			// generation and --require-base shape, still applies.
			live := fx.heldRun(worker, "")
			mustExec(fx.ctx, t, fx.pool, `UPDATE runs SET status = 'running' WHERE id = $1`, live)
			if n, err := report(live); err != nil || n != 1 {
				t.Fatalf("control: %s on a running run = (%d, %v), want (1, nil)", name, n, err)
			}
		})
	}
}

// TestSetRunBaseAttestationHoldStatementLiveDB executes the setter's fence directly: only the
// owning worker's live claim at the exact generation, in running, with --require-base, matches.
func TestSetRunBaseAttestationHoldStatementLiveDB(t *testing.T) {
	fx := newWPFixture(t)
	worker := fx.worker("w-3194s", wpWorker{})
	wid := pgtype.UUID{Bytes: worker, Valid: true}
	running := func(mut string) uuid.UUID {
		w := worker
		id := fx.run(wpRun{worker: &w, claimGen: 3})
		mustExec(fx.ctx, fx.t, fx.pool, `UPDATE runs SET require_base_match = true, planned_base_commit = 'abcdef1234567' WHERE id = $1`, id)
		if mut != "" {
			mustExec(fx.ctx, fx.t, fx.pool, `UPDATE runs SET `+mut+` WHERE id = $1`, id)
		}
		return id
	}
	hold := func(id uuid.UUID, w pgtype.UUID, gen int64) (store.Run, error) {
		return fx.q.SetRunBaseAttestationHold(fx.ctx, store.SetRunBaseAttestationHoldParams{
			HoldCapturedHead: pgtype.Text{String: "h", Valid: true}, ID: id, WorkerID: w, ClaimGeneration: gen})
	}
	refused := map[string]func() error{
		"wrong generation": func() error { _, err := hold(running(""), wid, 2); return err },
		"other worker":     func() error { _, err := hold(running(""), pgtype.UUID{Bytes: uuid.New(), Valid: true}, 3); return err },
		"released claim":   func() error { _, err := hold(running("claim_released_at = now()"), wid, 3); return err },
		"not running":      func() error { _, err := hold(running("status = 'awaiting_input'"), wid, 3); return err },
		"no require_base":  func() error { _, err := hold(running("require_base_match = false"), wid, 3); return err },
		"no planned base":  func() error { _, err := hold(running("planned_base_commit = NULL"), wid, 3); return err },
	}
	for name, try := range refused {
		if err := try(); !errors.Is(err, pgx.ErrNoRows) {
			t.Errorf("%s: err = %v, want ErrNoRows", name, err)
		}
	}
	id := running("")
	run, err := hold(id, wid, 3)
	if err != nil {
		t.Fatalf("live claim: %v", err)
	}
	if run.Status != "paused" || run.HoldReason.String != "base_attestation_missing" || run.HoldCapturedHead.String != "h" {
		t.Fatalf("held = (%q, %q, %q)", run.Status, run.HoldReason.String, run.HoldCapturedHead.String)
	}
	if _, err := hold(id, wid, 3); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("the statement itself must be a 0-row no-op on an already-held row; err = %v", err)
	}
}
