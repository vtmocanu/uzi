package workersvc

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4479 live-DB controls for the followup_finalize admission: an interactive task run parked
// at awaiting_followup is moved back to running for the worker's finalize, for exactly the park the
// worker observed, and then cannot re-park or accept an owner follow-up on that claim.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; run via
// ./e2e/run-store-it.sh. A package that prints `ok` with PASS=0 is INVALID, not green.

// ffNewTask returns an interactive task run claimed by the fixture worker at generation 1, running.
func (f *gateFix) ffNewTask() uuid.UUID {
	f.t.Helper()
	id := f.newRun("issue")
	f.exec(`UPDATE runs SET kind='task', issue_iid=NULL, branch='uzi/interactive-test', interactive=true WHERE id=$1`, id)
	f.liveWorker()
	return id
}

func (f *gateFix) ffPark(id uuid.UUID, gen int64, watermark *int64) store.Run {
	f.t.Helper()
	run, applied, err := f.svc.SetState(f.ctx, f.wkr, id, StateRequest{State: "awaiting_followup", ClaimGeneration: i64(gen), OpenFollowupID: watermark})
	if err != nil || !applied || run.Status != "awaiting_followup" {
		f.t.Fatalf("park = (status %s, applied %v, err %v)", run.Status, applied, err)
	}
	return run
}

// ffParked walks a fresh interactive task to awaiting_followup at generation 1 and returns its
// park sequence.
func (f *gateFix) ffParked() (uuid.UUID, int64) {
	f.t.Helper()
	id := f.ffNewTask()
	run := f.ffPark(id, 1, nil)
	return id, run.FollowupParkSeq
}

func (f *gateFix) ffFinalize(id uuid.UUID, gen, seq int64) (store.Run, bool, error) {
	f.t.Helper()
	return f.svc.SetState(f.ctx, f.wkr, id, StateRequest{State: stateFollowupFinalize, ClaimGeneration: i64(gen), FollowupParkSeq: i64(seq)})
}

// ffRefused requires a refusal carrying want (or ErrStaleClaim) and leaves the run untouched.
func (f *gateFix) ffRefused(id uuid.UUID, gen, seq int64, want error) {
	f.t.Helper()
	before := f.ffRow(id)
	_, applied, err := f.ffFinalize(id, gen, seq)
	if applied || !errors.Is(err, want) {
		f.t.Fatalf("finalize = (applied %v, err %v), want refusal %v", applied, err, want)
	}
	if after := f.ffRow(id); after != before {
		f.t.Fatalf("a refusal wrote: before %+v after %+v", before, after)
	}
}

type ffState struct {
	status       string
	parkSeq      int64
	finalizeGen  int64 // -1 when NULL
	iterations   int32
	openFollowup int64
	stopKind     string
	session      string
	claimGen     int64
	statusSince  time.Time
}

func (f *gateFix) ffRow(id uuid.UUID) ffState {
	f.t.Helper()
	var s ffState
	if err := f.pool.QueryRow(f.ctx, `SELECT status, followup_park_seq, COALESCE(followup_finalize_generation, -1), iteration_count,
	        COALESCE(open_followup_id, 0), COALESCE(stop_kind, ''), COALESCE(session_id, ''), claim_generation, status_since
	      FROM runs WHERE id = $1`, id).Scan(&s.status, &s.parkSeq, &s.finalizeGen, &s.iterations, &s.openFollowup, &s.stopKind, &s.session, &s.claimGen, &s.statusSince); err != nil {
		f.t.Fatalf("read run: %v", err)
	}
	return s
}

// ffFollowUp submits an owner follow-up and returns its input id.
func (f *gateFix) ffFollowUp(id uuid.UUID) int64 {
	f.t.Helper()
	res, err := f.submit(id, "follow_up", nil, SubmitInputOptions{})
	if err != nil {
		f.t.Fatalf("follow_up: %v", err)
	}
	return res.ID
}

func (f *gateFix) ffEnroll(id uuid.UUID, gen int64) error {
	f.t.Helper()
	f.svc.p.PublicationResume = true
	f.wkr.ProtocolCapabilities = append(append([]string{}, f.wkr.ProtocolCapabilities...), capability.PublicationResumeV1)
	f.exec(`UPDATE workers SET protocol_capabilities = $2 WHERE id = $1`, f.wkr.ID, f.wkr.ProtocolCapabilities)
	f.exec(`UPDATE runs SET started_at = COALESCE(started_at, now()) WHERE id = $1`, id)
	intent, err := json.Marshal(map[string]any{
		"run_id": id, "repo_id": f.repoID, "branch": "uzi/interactive-test", "claim_generation": gen,
		"original_head": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "floor": map[string]any{"kind": "absent"}, "candidate": nil,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	_, err = f.svc.RegisterPublicationIntent(f.ctx, f.wkr, id, PublicationIntentRequest{ClaimGeneration: gen, Intent: intent})
	return err
}

func TestFollowupFinalizeRegistersPublicationLiveDB(t *testing.T) {
	f := newGateFix(t, 3)
	id, seq := f.ffParked()
	if err := f.ffEnroll(id, 1); !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("enrollment at awaiting_followup = %v, want ErrStaleClaim (the bug this admission fixes)", err)
	}
	run, applied, err := f.ffFinalize(id, 1, seq)
	if err != nil || !applied || run.Status != "running" {
		t.Fatalf("finalize = (status %s, applied %v, err %v)", run.Status, applied, err)
	}
	if err := f.ffEnroll(id, 1); err != nil {
		t.Fatalf("enrollment after admission: %v", err)
	}
}

func TestFollowupFinalizeLostAckReadoptLiveDB(t *testing.T) {
	for _, frozen := range []bool{false, true} {
		for _, admitted := range []bool{false, true} {
			t.Run(fmt.Sprintf("frozen_%v/admitted_%v", frozen, admitted), func(t *testing.T) {
				f := newGateFix(t, 3)
				id, seq := f.ffParked()
				if admitted {
					// The server committed admission, but the lost ACK leaves the worker's
					// local snapshot at awaiting_followup.
					if _, applied, err := f.ffFinalize(id, 1, seq); err != nil || !applied {
						t.Fatalf("admission: applied=%v err=%v", applied, err)
					}
				}
				f.exec(`UPDATE runs SET status='queued', stale_requeue_generation=claim_generation WHERE id=$1`, id)
				f.exec(`INSERT INTO worker_active_runs(worker_id,run_id,claim_generation,phase,terminal_pending,snapshot_epoch,reported_at)
				        VALUES($1,$2,1,'awaiting_followup',false,1,now())`, f.wkr.ID, id)
				tx, err := f.pool.Begin(f.ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = tx.Rollback(f.ctx) }()
				q := store.New(tx)
				if _, err := q.GetWorkerForUpdate(f.ctx, f.wkr.ID); err != nil {
					t.Fatal(err)
				}
				if frozen {
					locks, err := captureWorkerRecoveryLocks(f.ctx, q, f.wkr.ID, []validatedEntry{{runID: id}})
					if err != nil {
						t.Fatal(err)
					}
					ledger, parents, err := locks.parameters()
					if err != nil {
						t.Fatal(err)
					}
					rows, err := q.FrozenReadoptRunsFromSnapshot(f.ctx, store.FrozenReadoptRunsFromSnapshotParams{WorkerID: f.wkr.ID, FrozenTargets: ledger, LockedParentIds: parents})
					if err != nil || len(rows) != 1 || rows[0].ID != id {
						t.Fatalf("frozen readopt: rows=%+v err=%v", rows, err)
					}
				} else {
					rows, err := q.ReadoptRunsFromSnapshot(f.ctx, f.wkr.ID)
					if err != nil || len(rows) != 1 || rows[0].ID != id {
						t.Fatalf("readopt: rows=%+v err=%v", rows, err)
					}
				}
				if err := tx.Commit(f.ctx); err != nil {
					t.Fatal(err)
				}
				wantStatus, wantGen := "awaiting_followup", int64(-1)
				if admitted {
					wantStatus, wantGen = "running", 1
				}
				got := f.ffRow(id)
				if got.status != wantStatus || got.parkSeq != seq || got.finalizeGen != wantGen || got.claimGen != 1 {
					t.Fatalf("readopt changed the admitted park identity: %+v", got)
				}
				if admitted {
					if _, applied, err := f.ffFinalize(id, 1, seq); err != nil || !applied {
						t.Fatalf("replay: applied=%v err=%v", applied, err)
					}
					if replay := f.ffRow(id); replay != got {
						t.Fatalf("replay wrote: before=%+v after=%+v", got, replay)
					}
					if err := f.ffEnroll(id, 1); err != nil {
						t.Fatalf("publication after readopt: %v", err)
					}
				}
			})
		}
	}
}

func TestFollowupParkSeqLiveDB(t *testing.T) {
	f := newGateFix(t, 3)

	t.Run("a same-generation re-adopt of a requeued park keeps the seq and still admits", func(t *testing.T) {
		id, seq := f.ffParked()
		// The stale-heartbeat requeue and the snapshot re-adopt (ReadoptRunsFromSnapshot /
		// FrozenReadoptRunsFromSnapshot write status = a.phase) at the SAME claim generation.
		f.exec(`UPDATE runs SET status = 'queued' WHERE id = $1`, id)
		f.exec(`UPDATE runs SET status = 'awaiting_followup' WHERE id = $1`, id)
		if got := f.ffRow(id).parkSeq; got != seq {
			t.Fatalf("re-adopt moved seq %d -> %d", seq, got)
		}
		if _, applied, err := f.ffFinalize(id, 1, seq); err != nil || !applied {
			t.Fatalf("finalize with the ACK seq after re-adopt = (applied %v, err %v)", applied, err)
		}
	})

	t.Run("a first park onto a stale-requeued row still gets a seq and admits", func(t *testing.T) {
		id := f.ffNewTask()
		f.exec(`UPDATE runs SET status = 'queued', stale_requeue_generation = claim_generation WHERE id = $1`, id)
		run := f.ffPark(id, 1, nil)
		if run.FollowupParkSeq <= 0 {
			t.Fatalf("first park onto a queued row: seq = %d, want > 0", run.FollowupParkSeq)
		}
		if _, applied, err := f.ffFinalize(id, 1, run.FollowupParkSeq); err != nil || !applied {
			t.Fatalf("finalize = (applied %v, err %v)", applied, err)
		}
	})

	t.Run("a re-adopt of a never-numbered park allocates its first seq", func(t *testing.T) {
		id := f.ffNewTask()
		f.exec(`UPDATE runs SET status = 'queued' WHERE id = $1`, id)
		f.exec(`UPDATE runs SET status = 'awaiting_followup' WHERE id = $1`, id)
		if got := f.ffRow(id).parkSeq; got != 1 {
			t.Fatalf("re-adopt of a never-numbered park: seq = %d, want 1", got)
		}
	})

	t.Run("park allocates a positive seq; the same park retried keeps it", func(t *testing.T) {
		id := f.ffNewTask()
		first := f.ffPark(id, 1, nil)
		if first.FollowupParkSeq <= 0 {
			t.Fatalf("park seq = %d, want > 0", first.FollowupParkSeq)
		}
		again := f.ffPark(id, 1, nil)
		if again.FollowupParkSeq != first.FollowupParkSeq {
			t.Fatalf("retried park moved seq %d -> %d", first.FollowupParkSeq, again.FollowupParkSeq)
		}
	})

	t.Run("a re-park after a running wake gets a new seq", func(t *testing.T) {
		id, seq := f.ffParked()
		f.exec(`UPDATE runs SET status = 'running' WHERE id = $1`, id)
		if got := f.ffRow(id).parkSeq; got != seq {
			t.Fatalf("leaving the park moved seq %d -> %d", seq, got)
		}
		if next := f.ffPark(id, 1, nil).FollowupParkSeq; next != seq+1 {
			t.Fatalf("re-park seq = %d, want %d", next, seq+1)
		}
	})

	t.Run("a lost-wake re-park with a higher watermark gets a new seq", func(t *testing.T) {
		id, seq := f.ffParked()
		fu := f.ffFollowUp(id)
		if _, err := f.svc.ConsumeInputs(f.ctx, f.wkr, id, i64(1)); err != nil {
			t.Fatal(err)
		}
		f.exec(`UPDATE run_user_inputs SET applied_at = now() WHERE id = $1`, fu)
		next := f.ffPark(id, 1, i64(fu)).FollowupParkSeq
		if next != seq+1 {
			t.Fatalf("lost-wake re-park seq = %d, want %d", next, seq+1)
		}
		if same := f.ffPark(id, 1, i64(fu)).FollowupParkSeq; same != next {
			t.Fatalf("retry of that re-park moved seq %d -> %d", next, same)
		}
	})

	t.Run("the trigger leaves unrelated updates alone", func(t *testing.T) {
		id := f.ffNewTask()
		f.exec(`UPDATE runs SET updated_at = now(), iteration_count = iteration_count + 1 WHERE id = $1`, id)
		if got := f.ffRow(id); got.parkSeq != 0 || got.finalizeGen != -1 {
			t.Fatalf("an unrelated update of a running run touched the park identity: %+v", got)
		}
		pid, seq := f.ffParked()
		f.exec(`UPDATE runs SET updated_at = now() WHERE id = $1`, pid)
		if got := f.ffRow(pid).parkSeq; got != seq {
			t.Fatalf("an unrelated update of a parked run moved seq %d -> %d", seq, got)
		}
		// A write that neither enters nor leaves the park from a non-park status is not touched.
		f.exec(`UPDATE runs SET status = 'running' WHERE id = $1`, pid)
		f.exec(`UPDATE runs SET status = 'paused' WHERE id = $1`, pid)
		if got := f.ffRow(pid).parkSeq; got != seq {
			t.Fatalf("leaving the park changed seq %d -> %d", seq, got)
		}
	})
}

func TestFollowupFinalizeAdmissionLiveDB(t *testing.T) {
	f := newGateFix(t, 3)

	t.Run("admits, stamps the generation and changes nothing else", func(t *testing.T) {
		id := f.ffNewTask()
		f.exec(`UPDATE runs SET iteration_count = 3, session_id = 'sess-ff' WHERE id = $1`, id)
		fu := f.ffFollowUp(id)
		if _, err := f.svc.ConsumeInputs(f.ctx, f.wkr, id, i64(1)); err != nil {
			t.Fatal(err)
		}
		f.exec(`UPDATE run_user_inputs SET applied_at = now() WHERE id = $1`, fu)
		seq := f.ffPark(id, 1, i64(fu)).FollowupParkSeq
		f.exec(`UPDATE runs SET stop_kind = 'stopped' WHERE id = $1`, id)
		before := f.ffRow(id)
		run, applied, err := f.ffFinalize(id, 1, seq)
		if err != nil || !applied || run.Status != "running" {
			t.Fatalf("finalize = (status %s, applied %v, err %v)", run.Status, applied, err)
		}
		after := f.ffRow(id)
		if after.finalizeGen != 1 || after.status != "running" {
			t.Fatalf("admission not stamped: %+v", after)
		}
		want := before
		want.status, want.finalizeGen, want.statusSince = "running", 1, after.statusSince
		if after != want {
			t.Fatalf("admission changed more than status and the stamp:\nbefore %+v\nafter  %+v", before, after)
		}
	})

	t.Run("a lost ACK replays to applied with no second write", func(t *testing.T) {
		id, seq := f.ffParked()
		if _, applied, err := f.ffFinalize(id, 1, seq); err != nil || !applied {
			t.Fatalf("first = (applied %v, err %v)", applied, err)
		}
		var before time.Time
		if err := f.pool.QueryRow(f.ctx, `SELECT updated_at FROM runs WHERE id = $1`, id).Scan(&before); err != nil {
			t.Fatal(err)
		}
		run, applied, err := f.ffFinalize(id, 1, seq)
		if err != nil || !applied || run.Status != "running" {
			t.Fatalf("replay = (status %s, applied %v, err %v)", run.Status, applied, err)
		}
		var after time.Time
		if err := f.pool.QueryRow(f.ctx, `SELECT updated_at FROM runs WHERE id = $1`, id).Scan(&after); err != nil || !after.Equal(before) {
			t.Fatalf("the replay wrote: updated_at %v -> %v (err %v)", before, after, err)
		}
	})

	t.Run("refusals", func(t *testing.T) {
		t.Run("non-task kind", func(t *testing.T) {
			id, seq := f.ffParked()
			f.exec(`UPDATE runs SET kind = 'prompt', branch = NULL WHERE id = $1`, id)
			f.ffRefused(id, 1, seq, ErrFollowupFinalizeRefused)
		})
		t.Run("non-interactive task", func(t *testing.T) {
			id, seq := f.ffParked()
			f.exec(`UPDATE runs SET interactive = false WHERE id = $1`, id)
			f.ffRefused(id, 1, seq, ErrFollowupFinalizeRefused)
		})
		t.Run("released claim is stale", func(t *testing.T) {
			id, seq := f.ffParked()
			f.exec(`UPDATE runs SET claim_released_at = now() WHERE id = $1`, id)
			f.ffRefused(id, 1, seq, ErrStaleClaim)
		})
		t.Run("superseded generation is stale", func(t *testing.T) {
			id, seq := f.ffParked()
			f.reclaim(id)
			f.ffRefused(id, 1, seq, ErrStaleClaim)
		})
		t.Run("cancelled run", func(t *testing.T) {
			id, seq := f.ffParked()
			f.exec(`UPDATE runs SET status = 'cancelled' WHERE id = $1`, id)
			f.ffRefused(id, 1, seq, ErrFollowupFinalizeRefused)
		})
		t.Run("completed run", func(t *testing.T) {
			id, seq := f.ffParked()
			f.exec(`UPDATE runs SET status = 'completed' WHERE id = $1`, id)
			f.ffRefused(id, 1, seq, ErrFollowupFinalizeRefused)
		})
		t.Run("a running run that never parked", func(t *testing.T) {
			id := f.ffNewTask()
			f.ffRefused(id, 1, 1, ErrFollowupFinalizeRefused)
		})
		t.Run("a superseded flight onto an iteration-budget pause is stale, not refused", func(t *testing.T) {
			id, seq := f.ffParked()
			f.reclaim(id)
			f.exec(`UPDATE runs SET status = 'paused', hold_reason = $2 WHERE id = $1`, id, IterationBudgetExhausted)
			_, applied, err := f.ffFinalize(id, 1, seq)
			if applied || !errors.Is(err, ErrStaleClaim) {
				t.Fatalf("finalize = (applied %v, err %v), want ErrStaleClaim", applied, err)
			}
		})
		t.Run("held run", func(t *testing.T) {
			id, seq := f.ffParked()
			f.exec(`UPDATE runs SET hold_reason = 'budget_exhausted' WHERE id = $1`, id)
			f.ffRefused(id, 1, seq, ErrFollowupFinalizeRefused)
		})
		t.Run("stale park seq", func(t *testing.T) {
			id, seq := f.ffParked()
			f.ffRefused(id, 1, seq+1, ErrFollowupFinalizeRefused)
			f.ffRefused(id, 1, seq-1, ErrInvalidState) // seq 0 is a malformed report
		})
		t.Run("a stale finalize after a later park with the same watermark", func(t *testing.T) {
			id, seq := f.ffParked()
			f.exec(`UPDATE runs SET status = 'running' WHERE id = $1`, id)
			if next := f.ffPark(id, 1, nil).FollowupParkSeq; next != seq+1 {
				t.Fatalf("later park seq = %d, want %d", next, seq+1)
			}
			f.ffRefused(id, 1, seq, ErrFollowupFinalizeRefused)
		})
		t.Run("a stale finalize after a lost-wake re-park with a higher watermark", func(t *testing.T) {
			id, seq := f.ffParked()
			fu := f.ffFollowUp(id)
			if _, err := f.svc.ConsumeInputs(f.ctx, f.wkr, id, i64(1)); err != nil {
				t.Fatal(err)
			}
			f.exec(`UPDATE run_user_inputs SET applied_at = now() WHERE id = $1`, fu)
			if next := f.ffPark(id, 1, i64(fu)).FollowupParkSeq; next != seq+1 {
				t.Fatalf("re-park seq = %d, want %d", next, seq+1)
			}
			f.ffRefused(id, 1, seq, ErrFollowupFinalizeRefused)
		})
		t.Run("legacy park with seq 0", func(t *testing.T) {
			id, _ := f.ffParked()
			// A park written before the column existed: seq 0 at awaiting_followup. The trigger is
			// bypassed on a dedicated connection only (replica role), never table-wide.
			conn, err := f.pool.Acquire(f.ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Release()
			for _, stmt := range []string{`SET session_replication_role = replica`} {
				if _, err := conn.Exec(f.ctx, stmt); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := conn.Exec(f.ctx, `UPDATE runs SET followup_park_seq = 0 WHERE id = $1`, id); err != nil {
				t.Fatal(err)
			}
			if _, err := conn.Exec(f.ctx, `RESET session_replication_role`); err != nil {
				t.Fatal(err)
			}
			f.ffRefused(id, 1, 1, ErrFollowupFinalizeRefused)
		})
		t.Run("malformed shapes are invalid", func(t *testing.T) {
			id, _ := f.ffParked()
			for _, req := range []StateRequest{
				{State: stateFollowupFinalize, FollowupParkSeq: i64(1)},
				{State: stateFollowupFinalize, ClaimGeneration: i64(0), FollowupParkSeq: i64(1)},
				{State: stateFollowupFinalize, ClaimGeneration: i64(1)},
				{State: stateFollowupFinalize, ClaimGeneration: i64(1), FollowupParkSeq: i64(0)},
				{State: "running", ClaimGeneration: i64(1), FollowupParkSeq: i64(1)},
			} {
				if _, applied, err := f.svc.SetState(f.ctx, f.wkr, id, req); applied || !errors.Is(err, ErrInvalidState) {
					t.Fatalf("%+v = (applied %v, err %v), want ErrInvalidState", req, applied, err)
				}
			}
		})
	})

	t.Run("deliverable cancel", func(t *testing.T) {
		id, seq := f.ffParked()
		if _, err := f.submit(id, "cancel", nil, SubmitInputOptions{}); err != nil {
			t.Fatalf("cancel: %v", err)
		}
		f.ffRefused(id, 1, seq, ErrFollowupFinalizeCancelPending)
	})

	t.Run("cancel beats stop", func(t *testing.T) {
		id, seq := f.ffParked()
		if _, err := f.submit(id, "stop", nil, SubmitInputOptions{}); err != nil {
			t.Fatalf("stop: %v", err)
		}
		if _, err := f.submit(id, "cancel", nil, SubmitInputOptions{}); err != nil {
			t.Fatalf("cancel: %v", err)
		}
		f.exec(`UPDATE runs SET stop_kind = 'stopped' WHERE id = $1`, id) // a later stop overwrote 'cancelled'
		f.ffRefused(id, 1, seq, ErrFollowupFinalizeCancelPending)
	})

	t.Run("a cancel after admission turns the replay into a refusal", func(t *testing.T) {
		id, seq := f.ffParked()
		if _, applied, err := f.ffFinalize(id, 1, seq); err != nil || !applied {
			t.Fatalf("first = (applied %v, err %v)", applied, err)
		}
		f.exec(`INSERT INTO run_user_inputs (run_id, kind) VALUES ($1, 'cancel')`, id)
		f.ffRefused(id, 1, seq, ErrFollowupFinalizeCancelPending)
	})

	t.Run("pending follow-up refuses an idle finalize", func(t *testing.T) {
		t.Run("queued", func(t *testing.T) {
			id, seq := f.ffParked()
			f.ffFollowUp(id)
			f.ffRefused(id, 1, seq, ErrFollowupFinalizePending)
		})
		t.Run("consumed but unapplied", func(t *testing.T) {
			id, seq := f.ffParked()
			f.ffFollowUp(id)
			if _, err := f.svc.ConsumeInputs(f.ctx, f.wkr, id, i64(1)); err != nil {
				t.Fatal(err)
			}
			f.ffRefused(id, 1, seq, ErrFollowupFinalizePending)
		})
		t.Run("applied above the park watermark", func(t *testing.T) {
			id, seq := f.ffParked()
			fu := f.ffFollowUp(id)
			if _, err := f.svc.ConsumeInputs(f.ctx, f.wkr, id, i64(1)); err != nil {
				t.Fatal(err)
			}
			f.exec(`UPDATE run_user_inputs SET applied_at = now() WHERE id = $1`, fu)
			f.ffRefused(id, 1, seq, ErrFollowupFinalizePending)
		})
		t.Run("a stop wins over a pending follow-up", func(t *testing.T) {
			id, seq := f.ffParked()
			f.ffFollowUp(id)
			f.exec(`UPDATE runs SET stop_kind = 'stopped' WHERE id = $1`, id)
			if _, applied, err := f.ffFinalize(id, 1, seq); err != nil || !applied {
				t.Fatalf("stopped finalize = (applied %v, err %v), want admitted", applied, err)
			}
		})
	})
}

func TestFollowupFinalizeRaceOrdersLiveDB(t *testing.T) {
	f := newGateFix(t, 3)

	t.Run("a follow-up submitted while the admission holds the run lock is refused after it commits", func(t *testing.T) {
		id, seq := f.ffParked()
		tx, err := f.pool.Begin(f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(f.ctx) }()
		rows, err := store.New(tx).AdmitRunFollowupFinalize(f.ctx, store.AdmitRunFollowupFinalizeParams{ID: id, WorkerID: pgconv.UUID(f.wkr.ID), ClaimGeneration: 1, ParkSeq: seq})
		if err != nil || rows != 1 {
			t.Fatalf("admit in tx = (%d, %v)", rows, err)
		}
		conn, err := f.pool.Acquire(f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Release()
		submitting := New(store.New(conn), f.svc.box, f.svc.p)
		done := make(chan error, 1)
		go func() {
			_, err := submitting.SubmitInputWithOptions(f.ctx, f.userID, id, "follow_up", "because", nil, SubmitInputOptions{})
			done <- err
		}()
		f.ffWaitForBackendBlock(conn.Conn().PgConn().PID(), tx.Conn().PgConn().PID(), done)
		if err := tx.Commit(f.ctx); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-done:
			if !errors.Is(err, ErrFollowUpFinalizing) {
				t.Fatalf("follow-up after the admission = %v, want ErrFollowUpFinalizing", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the follow-up never returned after the admission committed")
		}
	})

	t.Run("an admission started while a follow-up insert is uncommitted waits for it and refuses with followup_pending", func(t *testing.T) {
		id, seq := f.ffParked()
		tx, err := f.pool.Begin(f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(f.ctx) }()
		// The insert trigger takes the run row lock; the follow-up stays uncommitted.
		in, err := store.New(tx).CreateRunInput(f.ctx, store.CreateRunInputParams{RunID: id, Kind: "follow_up"})
		if err != nil {
			t.Fatalf("follow-up insert in tx: %v", err)
		}
		conn, err := f.pool.Acquire(f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Release()
		admitting := New(store.New(conn), f.svc.box, f.svc.p)
		admitting.SetTxBeginner(conn)
		done := make(chan error, 1)
		go func() {
			_, _, err := admitting.SetState(f.ctx, f.wkr, id, StateRequest{State: stateFollowupFinalize, ClaimGeneration: i64(1), FollowupParkSeq: i64(seq)})
			done <- err
		}()
		f.ffWaitForBackendBlock(conn.Conn().PgConn().PID(), tx.Conn().PgConn().PID(), done)
		if err := tx.Commit(f.ctx); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-done:
			if !errors.Is(err, ErrFollowupFinalizePending) {
				t.Fatalf("admission after the follow-up committed = %v, want ErrFollowupFinalizePending", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the admission never returned after the follow-up committed")
		}
		if got := f.ffRow(id); got.status != "awaiting_followup" || got.finalizeGen != -1 {
			t.Fatalf("a refused admission wrote: %+v", got)
		}
		var deliverable bool
		if err := f.pool.QueryRow(f.ctx, `SELECT disposition IS NULL AND applied_at IS NULL FROM run_user_inputs WHERE id = $1`, in.ID).Scan(&deliverable); err != nil || !deliverable {
			t.Fatalf("the follow-up is no longer deliverable (deliverable %v, err %v)", deliverable, err)
		}
	})

	t.Run("a follow-up committed first is seen by the admission", func(t *testing.T) {
		id, seq := f.ffParked()
		f.ffFollowUp(id)
		f.ffRefused(id, 1, seq, ErrFollowupFinalizePending)
	})
}

func (f *gateFix) ffWaitForBackendBlock(waiter, blocker uint32, done <-chan error) {
	f.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var blocked bool
		if err := f.pool.QueryRow(f.ctx, `SELECT $2::int = ANY(pg_blocking_pids($1::int))`, waiter, blocker).Scan(&blocked); err != nil {
			f.t.Fatal(err)
		}
		if blocked {
			return
		}
		select {
		case err := <-done:
			f.t.Fatalf("backend %d completed before waiting on backend %d: %v", waiter, blocker, err)
		default:
		}
		if time.Now().After(deadline) {
			f.t.Fatalf("backend %d never blocked on backend %d", waiter, blocker)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestFollowupFinalizeAfterAdmissionLiveDB(t *testing.T) {
	f := newGateFix(t, 3)

	t.Run("owner follow-ups are refused until a re-claim", func(t *testing.T) {
		id, seq := f.ffParked()
		if _, applied, err := f.ffFinalize(id, 1, seq); err != nil || !applied {
			t.Fatalf("finalize = (applied %v, err %v)", applied, err)
		}
		if _, err := f.submit(id, "follow_up", nil, SubmitInputOptions{}); !errors.Is(err, ErrFollowUpFinalizing) {
			t.Fatalf("SubmitInput follow_up = %v, want ErrFollowUpFinalizing", err)
		}
		_, err := f.q.CreateRunInput(f.ctx, store.CreateRunInputParams{RunID: id, Kind: "follow_up"})
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != followupFinalizingSQLState {
			t.Fatalf("raw follow_up insert = %v, want SQLSTATE %s", err, followupFinalizingSQLState)
		}
		// Other kinds are untouched.
		if _, err := f.q.CreateRunInput(f.ctx, store.CreateRunInputParams{RunID: id, Kind: "cancel"}); err != nil {
			t.Fatalf("a cancel must still be accepted: %v", err)
		}
		f.exec(`DELETE FROM run_user_inputs WHERE run_id = $1 AND kind = 'cancel'`, id)
		f.reclaim(id)
		if _, err := f.submit(id, "follow_up", nil, SubmitInputOptions{}); err != nil {
			t.Fatalf("follow_up after a re-claim: %v", err)
		}
	})

	t.Run("a follow-up for a non-task or non-interactive run is never refused", func(t *testing.T) {
		id, seq := f.ffParked()
		if _, applied, err := f.ffFinalize(id, 1, seq); err != nil || !applied {
			t.Fatalf("finalize = (applied %v, err %v)", applied, err)
		}
		f.exec(`UPDATE runs SET interactive = false WHERE id = $1`, id)
		if _, err := f.q.CreateRunInput(f.ctx, store.CreateRunInputParams{RunID: id, Kind: "follow_up"}); err != nil {
			t.Fatalf("non-interactive follow_up refused: %v", err)
		}
	})

	t.Run("a delayed park report leaves the admitted run running", func(t *testing.T) {
		id, seq := f.ffParked()
		if _, applied, err := f.ffFinalize(id, 1, seq); err != nil || !applied {
			t.Fatalf("finalize = (applied %v, err %v)", applied, err)
		}
		before := f.ffRow(id)
		run, applied, err := f.svc.SetState(f.ctx, f.wkr, id, StateRequest{State: "awaiting_followup", ClaimGeneration: i64(1)})
		if err != nil || applied || run.Status != "running" {
			t.Fatalf("delayed park via SetState = (status %s, applied %v, err %v), want running and not applied", run.Status, applied, err)
		}
		// The old statement, as an older api pod runs it.
		rows, err := f.q.SetRunAwaitingFollowup(f.ctx, store.SetRunAwaitingFollowupParams{ID: id, WorkerID: pgconv.UUID(f.wkr.ID)})
		if err != nil || rows != 0 {
			t.Fatalf("old SetRunAwaitingFollowup = (%d, %v), want 0 rows", rows, err)
		}
		if after := f.ffRow(id); after != before {
			t.Fatalf("delayed parks wrote: before %+v after %+v", before, after)
		}
	})

	t.Run("a stop survives the admission to completion", func(t *testing.T) {
		id := f.ffNewTask()
		seq := f.ffPark(id, 1, nil).FollowupParkSeq
		f.exec(`UPDATE runs SET stop_kind = 'stopped' WHERE id = $1`, id)
		if _, applied, err := f.ffFinalize(id, 1, seq); err != nil || !applied {
			t.Fatalf("finalize = (applied %v, err %v)", applied, err)
		}
		run, applied, err := f.svc.SetState(f.ctx, f.wkr, id, StateRequest{State: "completed", ClaimGeneration: i64(1)})
		if err != nil || !applied || run.Status != "completed" || run.StopKind.String != "stopped" {
			t.Fatalf("completed = (status %s, stop_kind %q, applied %v, err %v)", run.Status, run.StopKind.String, applied, err)
		}
	})
}

// Issue #4479: after the finalize admission the run is running and enrolls its publication intent;
// a forge outage at publication then takes the EXISTING arms: a stamped stop cancels through
// CancelRunByWorker, an idle finalize parks in recovery_wait with its forge_unreachable cause.
func TestFollowupFinalizeForgeOutageLiveDB(t *testing.T) {
	// drive: interactive task -> park -> (optional stop) -> followup_finalize -> enroll -> forge park.
	drive := func(t *testing.T, stop bool) store.Run {
		t.Helper()
		base := newPublicationResumeFixture(t)
		base.svc.p.RunProviderParkMaxDuration = time.Hour
		f := &publicationParkFixture{publicationResumeFixture: base, clock: time.Now().UTC().Truncate(time.Microsecond)}
		base.svc.SetClock(func() time.Time { return f.clock })
		f.openHold(t, f.runID, 1) // an unguarded custody hold at generation 1, as a running claim has
		f.env.exec(`UPDATE runs SET kind='task', issue_iid=NULL, interactive=true, branch='agent/issue-1' WHERE id = $1`, f.runID)
		f.env.exec(`UPDATE workers SET last_heartbeat_at = now(), status = 'online' WHERE id = $1`, f.workerID)
		parked, applied, err := f.svc.SetState(f.env.ctx, f.wkr, f.runID, StateRequest{State: "awaiting_followup", ClaimGeneration: i64(1)})
		if err != nil || !applied || parked.FollowupParkSeq <= 0 {
			t.Fatalf("park = (seq %d, applied %v, err %v)", parked.FollowupParkSeq, applied, err)
		}
		if stop {
			if _, err := f.svc.SubmitInputWithOptions(f.env.ctx, f.userID, f.runID, "stop", "", nil, SubmitInputOptions{}); err != nil {
				t.Fatalf("stop: %v", err)
			}
		}
		if _, applied, err := f.svc.SetState(f.env.ctx, f.wkr, f.runID, StateRequest{State: stateFollowupFinalize, ClaimGeneration: i64(1), FollowupParkSeq: i64(parked.FollowupParkSeq)}); err != nil || !applied {
			t.Fatalf("finalize = (applied %v, err %v)", applied, err)
		}
		f.enroll(t)
		if _, _, err := f.park(1, "git"); err != nil {
			t.Fatalf("forge park report: %v", err)
		}
		return f.run(t)
	}

	t.Run("a stop-admitted run cancels through the stamped-stop arm", func(t *testing.T) {
		run := drive(t, true)
		if run.Status != "cancelled" || run.StopKind.String != "stopped" {
			t.Fatalf("after the forge outage: status %q stop_kind %q, want cancelled / stopped", run.Status, run.StopKind.String)
		}
	})

	t.Run("an idle-admitted run parks in recovery_wait for the publication recovery", func(t *testing.T) {
		run := drive(t, false)
		if run.Status != "recovery_wait" || run.RecoveryWaitCause.String != "forge_unreachable" || run.StopKind.Valid && run.StopKind.String != "" {
			t.Fatalf("after the forge outage: status %q cause %q stop_kind %q, want recovery_wait / forge_unreachable / none", run.Status, run.RecoveryWaitCause.String, run.StopKind.String)
		}
	})
}
