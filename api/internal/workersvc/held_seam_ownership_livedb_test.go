package workersvc

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

// Issue #3860 m2: the server half of the worker's held-seam cancel. RunOwnership reports
// cancellation_requested (a read-only signal), and the stop_kind='cancelled' failed report
// settles the run's cancel inputs in the same statement as the terminal transition.
// Skipped unless UZI_TEST_DATABASE_URL is set (run via ./e2e/run-store-it.sh).

// heldSeamOwnFix is a published running run owned by a fresh worker at claim generation 1,
// with a fresh heartbeat so the owner cancel is routed to the live poller.
type heldSeamOwnFix struct {
	f   *retentionFix
	env codexTestEnv
}

func newHeldSeamOwnFix(t *testing.T) heldSeamOwnFix {
	t.Helper()
	f := newRetentionFix(t)
	f.svc.SetLifecycle(&fakeLifecycle{})
	return heldSeamOwnFix{f: f, env: codexTestEnv{ctx: f.e.ctx, pool: f.e.pool, q: f.e.q}}
}

// run seeds a running run owned by a fresh worker and returns both ids.
func (h heldSeamOwnFix) run(t *testing.T) (uuid.UUID, uuid.UUID) {
	t.Helper()
	w := h.f.e.seedWorker(t, nil)
	h.f.e.exec(t, `UPDATE workers SET last_heartbeat_at = now() WHERE id = $1`, w)
	id, _ := h.f.seedPublishedRun(t, w)
	return id, w
}

// cancelledRun seeds a run and submits the owner cancel through the live poller, leaving
// stop_kind='cancelled' and one unapplied cancel input.
func (h heldSeamOwnFix) cancelledRun(t *testing.T) (uuid.UUID, uuid.UUID) {
	t.Helper()
	id, w := h.run(t)
	heldSeamCancelViaPoller(t, h.env, h.f.svc, h.f.e.userID, id, "operator stop")
	return id, w
}

func (h heldSeamOwnFix) requested(t *testing.T, id, w uuid.UUID) bool {
	t.Helper()
	_, _, _, _, _, _, requested, err := h.f.svc.RunOwnership(h.f.e.ctx, h.f.wkr(w), id)
	if err != nil {
		t.Fatalf("RunOwnership: %v", err)
	}
	return requested
}

func (h heldSeamOwnFix) report(t *testing.T, id, w uuid.UUID, gen int64) (bool, error) {
	t.Helper()
	_, applied, err := h.f.svc.SetState(h.f.e.ctx, h.f.wkr(w), id, StateRequest{
		State: "failed", ClaimGeneration: &gen, FailureReason: strPtr("cancelled by operator"),
	})
	return applied, err
}

// TestHeldSeamOwnershipCancellationRequestedLiveDB pins the field semantics through the
// service: true only with the cancelled stamp AND a deliverable cancel row; false (never NULL
// or an error) for every other combination.
func TestHeldSeamOwnershipCancellationRequestedLiveDB(t *testing.T) {
	h := newHeldSeamOwnFix(t)

	t.Run("stamp and deliverable row", func(t *testing.T) {
		id, w := h.cancelledRun(t)
		if !h.requested(t, id, w) {
			t.Fatal("cancellation_requested = false, want true with the stamp and a deliverable cancel row")
		}
		// The read is read-only: nothing applied or consumed.
		if total, unapplied := heldSeamCancelInputs(t, h.env, id); total != 1 || unapplied != 1 {
			t.Fatalf("cancel inputs total/unapplied after the read = %d/%d, want 1/1", total, unapplied)
		}
	})

	for name, mutate := range map[string]string{
		"row applied":    `UPDATE run_user_inputs SET applied_at = now() WHERE run_id = $1 AND kind = 'cancel'`,
		"row superseded": `UPDATE run_user_inputs SET disposition = 'superseded' WHERE run_id = $1 AND kind = 'cancel'`,
	} {
		t.Run(name, func(t *testing.T) {
			id, w := h.cancelledRun(t)
			h.f.e.exec(t, mutate, id)
			if h.requested(t, id, w) {
				t.Fatal("cancellation_requested = true, want false: the cancel row is no longer deliverable")
			}
		})
	}

	t.Run("covered by a reconciliation stop_cutoff", func(t *testing.T) {
		id, w := h.cancelledRun(t)
		var input int64
		if err := h.env.pool.QueryRow(h.env.ctx,
			`INSERT INTO run_user_inputs(run_id, kind, body) VALUES ($1, 'approve_plan', '') RETURNING id`, id).Scan(&input); err != nil {
			t.Fatal(err)
		}
		// stop_cutoff = the newest input id, which covers the earlier cancel row.
		h.f.e.exec(t, `INSERT INTO run_milestone_reconciliations(id, run_id, owner_id, gate_revision, presentation_id,
			gate_digest, reset_state_digest, worker_id, claim_generation, reason, consent, before_state, after_state,
			applied_input_id, stop_cutoff)
			VALUES ($1, $2, $3, 1, $4, $5, 'fixture', $6, 1, 'fixture', '{}', '{}', '{}', $7, $7)`,
			uuid.New(), id, h.f.e.userID, uuid.New(), []byte{1}, w, input)
		if h.requested(t, id, w) {
			t.Fatal("cancellation_requested = true, want false: the cancel row is covered by a stop_cutoff")
		}
	})

	for _, kind := range []string{"auto_stopped", "stopped"} {
		t.Run("stop_kind "+kind, func(t *testing.T) {
			id, w := h.cancelledRun(t)
			h.f.e.exec(t, `UPDATE runs SET stop_kind = $2 WHERE id = $1`, id, kind)
			if h.requested(t, id, w) {
				t.Fatalf("cancellation_requested = true with stop_kind %q, want false", kind)
			}
		})
	}

	t.Run("stop_kind NULL", func(t *testing.T) {
		id, w := h.cancelledRun(t)
		h.f.e.exec(t, `UPDATE runs SET stop_kind = NULL, stop_reason = NULL WHERE id = $1`, id)
		if h.requested(t, id, w) {
			t.Fatal("cancellation_requested = true with NULL stop_kind, want false (not NULL, not an error)")
		}
	})

	t.Run("no cancel row", func(t *testing.T) {
		id, w := h.run(t)
		h.f.e.exec(t, `UPDATE runs SET stop_kind = 'cancelled', stop_reason = 'x' WHERE id = $1`, id)
		if h.requested(t, id, w) {
			t.Fatal("cancellation_requested = true with the stamp but no cancel row, want false")
		}
	})

	t.Run("another worker is not owned", func(t *testing.T) {
		id, _ := h.cancelledRun(t)
		other := h.f.e.seedWorker(t, nil)
		if _, _, _, _, _, _, _, err := h.f.svc.RunOwnership(h.f.e.ctx, h.f.wkr(other), id); !errors.Is(err, ErrRunNotOwned) {
			t.Fatalf("RunOwnership(other worker) err = %v, want ErrRunNotOwned", err)
		}
	})

	t.Run("cross_check at generation 0 is not owned", func(t *testing.T) {
		lead, _ := h.run(t)
		id, w := h.run(t)
		h.f.e.exec(t, `UPDATE runs SET kind = 'cross_check', issue_iid = NULL, target_run_id = $2, report_only = true,
			budget_wall_seconds = 600, claim_generation = 0 WHERE id = $1`, id, lead)
		if _, _, _, _, _, _, _, err := h.f.svc.RunOwnership(h.f.e.ctx, h.f.wkr(w), id); !errors.Is(err, ErrRunNotOwned) {
			t.Fatalf("RunOwnership(cross_check gen 0) err = %v, want ErrRunNotOwned", err)
		}
	})
}

// TestHeldSeamCancelSettlementLiveDB pins the atomic settlement of the stop_kind='cancelled'
// failed report: the run never ends cancelled with its cancel row pending, and the row is never
// applied without the terminal transition.
func TestHeldSeamCancelSettlementLiveDB(t *testing.T) {
	h := newHeldSeamOwnFix(t)

	t.Run("applied report settles the row", func(t *testing.T) {
		id, w := h.cancelledRun(t)
		if applied, err := h.report(t, id, w, 1); err != nil || !applied {
			t.Fatalf("report: applied=%v err=%v", applied, err)
		}
		if r := heldSeamRead(t, h.env, id); r.status != "cancelled" {
			t.Fatalf("status = %q, want cancelled", r.status)
		}
		if total, unapplied := heldSeamCancelInputs(t, h.env, id); total != 1 || unapplied != 0 {
			t.Fatalf("cancel inputs total/unapplied = %d/%d, want 1/0", total, unapplied)
		}
		if h.requested(t, id, w) {
			t.Fatal("cancellation_requested = true after the settled report, want false")
		}
	})

	t.Run("pre-ACKed row is settled", func(t *testing.T) {
		id, w := h.cancelledRun(t)
		h.f.e.exec(t, `UPDATE run_user_inputs SET consumed_at = now() WHERE run_id = $1 AND kind = 'cancel'`, id)
		if applied, err := h.report(t, id, w, 1); err != nil || !applied {
			t.Fatalf("report: applied=%v err=%v", applied, err)
		}
		var consumed, applied bool
		if err := h.env.pool.QueryRow(h.env.ctx,
			`SELECT consumed_at IS NOT NULL, applied_at IS NOT NULL FROM run_user_inputs WHERE run_id = $1 AND kind = 'cancel'`, id).
			Scan(&consumed, &applied); err != nil || !consumed || !applied {
			t.Fatalf("pre-ACKed row consumed=%v applied=%v err=%v, want both set", consumed, applied, err)
		}
	})

	t.Run("superseded row is left alone", func(t *testing.T) {
		id, w := h.cancelledRun(t)
		h.f.e.exec(t, `UPDATE run_user_inputs SET disposition = 'superseded' WHERE run_id = $1 AND kind = 'cancel'`, id)
		if applied, err := h.report(t, id, w, 1); err != nil || !applied {
			t.Fatalf("report: applied=%v err=%v", applied, err)
		}
		if total, unapplied := heldSeamCancelInputs(t, h.env, id); total != 1 || unapplied != 1 {
			t.Fatalf("superseded row total/unapplied = %d/%d, want 1/1 (not settled)", total, unapplied)
		}
	})

	t.Run("stale generation is refused and the row stays pending", func(t *testing.T) {
		id, w := h.cancelledRun(t)
		h.f.e.exec(t, `UPDATE runs SET claim_generation = 2 WHERE id = $1`, id)
		applied, err := h.report(t, id, w, 1)
		if applied || !errors.Is(err, ErrStaleClaim) {
			t.Fatalf("stale report: applied=%v err=%v, want ErrStaleClaim and not applied", applied, err)
		}
		if r := heldSeamRead(t, h.env, id); r.status == "cancelled" {
			t.Fatal("stale report terminalised the run")
		}
		if total, unapplied := heldSeamCancelInputs(t, h.env, id); total != 1 || unapplied != 1 {
			t.Fatalf("cancel inputs total/unapplied = %d/%d, want 1/1", total, unapplied)
		}
	})

	t.Run("already-terminal run keeps its inputs untouched", func(t *testing.T) {
		id, w := h.cancelledRun(t)
		h.f.e.exec(t, `UPDATE runs SET status = 'completed', finished_at = now() WHERE id = $1`, id)
		// Whether SetState answers with an error or a not-applied result is not the contract
		// here; the run and its inputs must not move.
		applied, _ := h.report(t, id, w, 1)
		if applied {
			t.Fatal("a report onto an already-terminal run was applied")
		}
		if r := heldSeamRead(t, h.env, id); r.status != "completed" {
			t.Fatalf("status = %q, want completed (unchanged)", r.status)
		}
		if total, unapplied := heldSeamCancelInputs(t, h.env, id); total != 1 || unapplied != 1 {
			t.Fatalf("cancel inputs total/unapplied = %d/%d, want 1/1 (untouched)", total, unapplied)
		}
	})

	t.Run("stopped arm does not settle cancel rows", func(t *testing.T) {
		id, w := h.cancelledRun(t)
		h.f.e.exec(t, `UPDATE runs SET stop_kind = 'stopped' WHERE id = $1`, id)
		if applied, err := h.report(t, id, w, 1); err != nil || !applied {
			t.Fatalf("report: applied=%v err=%v", applied, err)
		}
		if r := heldSeamRead(t, h.env, id); r.status != "cancelled" {
			t.Fatalf("status = %q, want cancelled", r.status)
		}
		if total, unapplied := heldSeamCancelInputs(t, h.env, id); total != 1 || unapplied != 1 {
			t.Fatalf("stopped arm cancel inputs total/unapplied = %d/%d, want 1/1 (unchanged behaviour)", total, unapplied)
		}
	})
}
