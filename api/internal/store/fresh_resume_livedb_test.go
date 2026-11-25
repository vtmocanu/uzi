package store_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// freshFx builds on the custody admission fixture (owner, repo, workers, run helper) with no holds.
type freshFx struct {
	*admissionFixture
}

func newFreshFx(t *testing.T) *freshFx {
	t.Helper()
	return &freshFx{newAdmissionFixture(t, 0)}
}

// newWorker inserts an owner worker whose heartbeat is exactly the admission cutoff (healthy).
func (f *freshFx) newWorker() uuid.UUID {
	f.t.Helper()
	w := uuid.New()
	f.exec(`INSERT INTO workers(id,user_id,name,token_hash,status,last_heartbeat_at)
 VALUES($1,$2,$3,$4,'offline',$5)`, w, f.owner, w.String(), w[:], f.cutoff)
	return w
}

// transfer inserts a transfer row in the given state directly (the guard and shape CHECKs still apply).
func (f *freshFx) transfer(run uuid.UUID, state string, dest uuid.UUID, gen int64) uuid.UUID {
	f.t.Helper()
	id := uuid.New()
	var d, g any
	if dest != uuid.Nil {
		d = dest
	}
	if gen > 0 {
		g = gen
	}
	f.exec(`INSERT INTO run_fresh_resumes (id, run_id, user_id, request_id, evidence_generation, evidence_kind,
 checkpoint_ref, sha, candidate_token, state, destination_worker_id, claim_generation)
 VALUES ($1,$2,$3,$4,1,'checkpoint_ref','refs/uzi-checkpoints/x','abc','tok',$5,$6,$7)`,
		id, run, f.owner, uuid.New(), state, d, g)
	return id
}

// seeded inserts a seeded row for run: predecessor pred, seeded at generation gen on dest.
func (f *freshFx) seeded(run, pred, dest uuid.UUID, gen int64, state string) uuid.UUID {
	f.t.Helper()
	id := uuid.New()
	f.exec(`INSERT INTO run_fresh_resumes (id, run_id, user_id, request_id, evidence_generation, evidence_kind,
 checkpoint_ref, sha, candidate_token, state, predecessor_worker_id, destination_worker_id, claim_generation,
 destination_attempt_id, seeded_generation, seeded_at)
 VALUES ($1,$2,$3,$4,1,'checkpoint_ref','refs/uzi-checkpoints/x','abc','tok',$5,$6,$7,$8,$9,$8,now())`,
		id, run, f.owner, uuid.New(), state, pred, dest, gen, attemptID(gen))
	return id
}

func (f *freshFx) runningRun(worker uuid.UUID, gen int64) uuid.UUID {
	f.t.Helper()
	r := f.run("running", worker, gen)
	f.exec(`UPDATE runs SET started_at = now() WHERE id = $1`, r)
	return r
}

// runSnap is the explicit field list the old-writer tests assert unchanged.
type runSnap struct {
	Status           string
	WorkerID         pgtype.UUID
	ClaimGeneration  int64
	FinishedAt       pgtype.Timestamptz
	FailOrigin       pgtype.Text
	FailureReason    pgtype.Text
	MovePendingSince pgtype.Timestamptz
	CodexClaimEpoch  int64
	FreshHoldVersion int64
	UpdatedAt        pgtype.Timestamptz
}

func (f *freshFx) snap(run uuid.UUID) runSnap {
	f.t.Helper()
	var s runSnap
	err := f.pool.QueryRow(f.ctx, `SELECT status, worker_id, claim_generation, finished_at, fail_origin,
 failure_reason, move_pending_since, codex_claim_epoch, fresh_hold_version, updated_at FROM runs WHERE id=$1`, run).
		Scan(&s.Status, &s.WorkerID, &s.ClaimGeneration, &s.FinishedAt, &s.FailOrigin, &s.FailureReason,
			&s.MovePendingSince, &s.CodexClaimEpoch, &s.FreshHoldVersion, &s.UpdatedAt)
	if err != nil {
		f.t.Fatal(err)
	}
	return s
}

func (f *freshFx) transferState(id uuid.UUID) string {
	f.t.Helper()
	var s string
	if err := f.pool.QueryRow(f.ctx, `SELECT state FROM run_fresh_resumes WHERE id=$1`, id).Scan(&s); err != nil {
		f.t.Fatal(err)
	}
	return s
}

func (f *freshFx) count(cutoff pgtype.Timestamptz) int64 {
	f.t.Helper()
	var n int64
	if err := f.pool.QueryRow(f.ctx, `SELECT fn_custody_admission_count($1,$2)`, f.owner, cutoff).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func wantFreshInProgress(t *testing.T, what string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: expected the fresh resume guard to raise, got no error", what)
	}
	if !store.IsFreshResumeInProgress(err) {
		t.Fatalf("%s: expected UZ416 in-progress, got %v", what, err)
	}
	if store.IsFreshResumePredecessorExcluded(err) {
		t.Fatalf("%s: in-progress raise must not read as predecessor-excluded: %v", what, err)
	}
}

// Rule 1: the frozen base text of old-writer statements (an old binary, or a writer outside the registry)
// raises against pending and claimed rows and leaves the run untouched; the current queries, which carry
// the NOT EXISTS literal, match no row and also leave it untouched.
func TestFreshResumeGuardRule1RaisesLiveDB(t *testing.T) {
	for _, state := range []string{"pending", "claimed"} {
		t.Run(state, func(t *testing.T) {
			f := newFreshFx(t)
			w2 := f.newWorker()
			run := f.runningRun(w2, 4)
			f.exec(`UPDATE runs SET claim_released_at = NULL WHERE id=$1`, run)
			f.transfer(run, state, w2, 4)
			before := f.snap(run)
			wid := pgtype.UUID{Bytes: w2, Valid: true}
			gen := pgtype.Int8{Int64: 4, Valid: true}
			none := pgtype.Text{}

			exec := func(sql string, args ...any) error {
				_, err := f.pool.Exec(f.ctx, sql, args...)
				return err
			}
			wantFreshInProgress(t, "base SetRunRecoveryWait", exec(base_setRunRecoveryWait,
				pgtype.Text{String: "codex_idle_stall", Valid: true}, pgtype.Int4{}, false, gen, pgtype.Timestamptz{}, none, run, wid))
			wantFreshInProgress(t, "base SetRunFailed", exec(base_setRunFailed,
				pgtype.Text{String: "boom", Valid: true}, pgtype.Text{String: "agent_failure", Valid: true}, none, none, run, wid, gen))
			wantFreshInProgress(t, "base ParkRunRetainedRecovery", exec(base_parkRunRetainedRecovery,
				"retained_recovery_exhausted", none, false, pgtype.Timestamptz{}, none, pgtype.Bool{}, run, wid, int64(4)))
			// The stale-worker requeue sweep: the worker heartbeat is older than the cutoff.
			rows, err := f.pool.Query(f.ctx, base_requeueRunsOfStaleWorkers, int32(5),
				pgtype.Timestamptz{Time: f.cutoff.Time.Add(time.Hour), Valid: true})
			if err == nil {
				for rows.Next() {
				}
				err = rows.Err()
				rows.Close()
			}
			wantFreshInProgress(t, "base RequeueRunsOfStaleWorkers", err)
			if after := f.snap(run); after != before {
				t.Fatalf("a refused old writer changed the run:\nbefore %+v\nafter  %+v", before, after)
			}

			// Current queries: the literal excludes the run, so nothing matches and nothing raises.
			if n, err := f.q.SetRunRecoveryWait(f.ctx, store.SetRunRecoveryWaitParams{ID: run, WorkerID: wid, ClaimGeneration: gen,
				RecoveryCause: pgtype.Text{String: "codex_idle_stall", Valid: true}}); err != nil || n != 0 {
				t.Fatalf("SetRunRecoveryWait: n=%d err=%v", n, err)
			}
			if n, err := f.q.SetRunFailed(f.ctx, store.SetRunFailedParams{ID: run, WorkerID: wid, ClaimGeneration: gen,
				FailureReason: pgtype.Text{String: "boom", Valid: true}, FailOrigin: pgtype.Text{String: "agent_failure", Valid: true}}); err != nil || n != 0 {
				t.Fatalf("SetRunFailed: n=%d err=%v", n, err)
			}
			if _, err := f.q.ParkRunRetainedRecovery(f.ctx, store.ParkRunRetainedRecoveryParams{ID: run, WorkerID: wid, ClaimGeneration: 4,
				RecoveryCause: "retained_recovery_exhausted"}); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("ParkRunRetainedRecovery: %v", err)
			}
			if got, err := f.q.RequeueRunsOfStaleWorkers(f.ctx, store.RequeueRunsOfStaleWorkersParams{MaxRequeues: 5,
				Cutoff: pgtype.Timestamptz{Time: f.cutoff.Time.Add(time.Hour), Valid: true}}); err != nil {
				t.Fatalf("RequeueRunsOfStaleWorkers: %v", err)
			} else {
				for _, r := range got {
					if r.ID == run {
						t.Fatal("the stale-worker requeue picked a transfer run")
					}
				}
			}
			if after := f.snap(run); after != before {
				t.Fatalf("an excluded writer changed the run:\nbefore %+v\nafter  %+v", before, after)
			}
			if after := f.snap(run); after.FinishedAt.Valid || after.FailOrigin.Valid || after.FailureReason.Valid || after.MovePendingSince.Valid {
				t.Fatalf("terminal fields were written: %+v", after)
			}
		})
	}
}

func TestFreshResumeGuardAllowancesLiveDB(t *testing.T) {
	f := newFreshFx(t)

	t.Run("cancel passes and closes the row", func(t *testing.T) {
		w2 := f.newWorker()
		run := f.runningRun(w2, 4)
		for _, state := range []string{"pending", "claimed"} {
			id := f.transfer(run, state, w2, 4)
			f.exec(`UPDATE runs SET status='running' WHERE id=$1 AND status<>'running'`, run)
			f.exec(`UPDATE runs SET status='cancelled' WHERE id=$1`, run)
			if got := f.transferState(id); got != "cancelled" {
				t.Fatalf("%s row after run cancel = %s, want cancelled", state, got)
			}
			var closed pgtype.Timestamptz
			if err := f.pool.QueryRow(f.ctx, `SELECT closed_at FROM run_fresh_resumes WHERE id=$1`, id).Scan(&closed); err != nil || !closed.Valid {
				t.Fatalf("closed_at not stamped: %v %v", closed, err)
			}
			f.exec(`DELETE FROM run_fresh_resumes WHERE id=$1`, id)
			f.exec(`UPDATE runs SET status='running' WHERE id=$1`, run)
		}
	})

	t.Run("opt-in is per row id and per transaction", func(t *testing.T) {
		wa, wb := f.newWorker(), f.newWorker()
		runA, runB := f.runningRun(wa, 2), f.runningRun(wb, 2)
		idA := f.transfer(runA, "claimed", wa, 2)
		f.transfer(runB, "claimed", wb, 2)
		tx, err := f.pool.Begin(f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(f.ctx) }()
		qtx := f.q.WithTx(tx)
		if err := qtx.OptInFreshResume(f.ctx, idA.String()); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(f.ctx, `UPDATE runs SET status='queued', worker_id=NULL WHERE id=$1`, runA); err != nil {
			t.Fatalf("opted-in row must pass: %v", err)
		}
		if _, err := tx.Exec(f.ctx, `SAVEPOINT s`); err != nil {
			t.Fatal(err)
		}
		_, err = tx.Exec(f.ctx, `UPDATE runs SET status='queued', worker_id=NULL WHERE id=$1`, runB)
		wantFreshInProgress(t, "second row under the first row's opt-in", err)
		if _, err := tx.Exec(f.ctx, `ROLLBACK TO s`); err != nil {
			t.Fatal(err)
		}
		if err := qtx.ClearFreshResumeOptIn(f.ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(f.ctx, `UPDATE runs SET status='running', worker_id=$2 WHERE id=$1`, runA, wa); err == nil {
			t.Fatal("cleared opt-in must not pass")
		}
		_ = tx.Rollback(f.ctx)
		// A new transaction on a pooled session sees no leaked opt-in.
		_, err = f.pool.Exec(f.ctx, `UPDATE runs SET status='queued', worker_id=NULL WHERE id=$1`, runA)
		wantFreshInProgress(t, "autocommit after the opted-in transaction", err)
	})

	t.Run("claimed destination running advance passes, pending does not", func(t *testing.T) {
		w2 := f.newWorker()
		run := f.run("claimed", w2, 4)
		f.transfer(run, "claimed", w2, 4)
		f.exec(`UPDATE runs SET status='running' WHERE id=$1`, run)
		f.exec(`UPDATE runs SET status='running', updated_at=now() WHERE id=$1`, run)

		w3 := f.newWorker()
		run2 := f.run("claimed", w3, 4)
		f.transfer(run2, "pending", w3, 4)
		_, err := f.pool.Exec(f.ctx, `UPDATE runs SET status='running' WHERE id=$1`, run2)
		wantFreshInProgress(t, "pending row running advance", err)
	})

	t.Run("auto-stop passes only for its own run", func(t *testing.T) {
		w2 := f.newWorker()
		run := f.runningRun(w2, 4)
		other := f.runningRun(f.newWorker(), 4)
		f.transfer(run, "claimed", w2, 4)
		f.transfer(other, "claimed", uuid.Nil, 4)
		tx, err := f.pool.Begin(f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(f.ctx) }()
		if _, err := tx.Exec(f.ctx, `SELECT set_config('uzi.fresh_resume_terminal', $1, true)`, run.String()); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(f.ctx, `SAVEPOINT s`); err != nil {
			t.Fatal(err)
		}
		_, err = tx.Exec(f.ctx, `UPDATE runs SET status='failed' WHERE id=$1`, other)
		wantFreshInProgress(t, "auto-stop GUC for another run", err)
		if _, err := tx.Exec(f.ctx, `ROLLBACK TO s`); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(f.ctx, `UPDATE runs SET status='failed' WHERE id=$1`, run); err != nil {
			t.Fatalf("auto-stop of its own run must pass: %v", err)
		}
	})

	t.Run("worker delete cascade passes, worker_id NULL with the worker present raises", func(t *testing.T) {
		w2 := f.newWorker()
		run := f.runningRun(w2, 4)
		id := f.transfer(run, "claimed", w2, 4)
		_, err := f.pool.Exec(f.ctx, `UPDATE runs SET worker_id=NULL WHERE id=$1`, run)
		wantFreshInProgress(t, "worker_id NULL while the worker exists", err)
		f.exec(`DELETE FROM workers WHERE id=$1`, w2)
		var wid, dest pgtype.UUID
		if err := f.pool.QueryRow(f.ctx, `SELECT r.worker_id, t.destination_worker_id FROM runs r
 JOIN run_fresh_resumes t ON t.run_id = r.id WHERE t.id=$1`, id).Scan(&wid, &dest); err != nil {
			t.Fatal(err)
		}
		if wid.Valid || dest.Valid {
			t.Fatalf("cascade left worker_id=%v destination=%v", wid, dest)
		}
		if f.snap(run).Status != "running" {
			t.Fatalf("cascade changed status: %+v", f.snap(run))
		}
	})
}

// Rule 2: after seeding, the predecessor and holders of an older hold can never take the run again.
func TestFreshResumeGuardRule2PredecessorExcludedLiveDB(t *testing.T) {
	f := newFreshFx(t)
	w1, w2, w3, ctrl := f.newWorker(), f.newWorker(), f.newWorker(), f.newWorker()
	run := f.runningRun(w2, 6)
	f.seeded(run, w1, w2, 6, "seeded")
	// W3 holds an open custody hold older than the seeded generation.
	f.exec(`INSERT INTO recovery_custody_holds(id,user_id,run_id,generation,state,original_worker_id,
 original_worker_identity,live_worker_id,live_run_id) VALUES($1,$2,$3,3,'open',$4,'o',$4,$3)`,
		uuid.New(), f.owner, run, w3)
	f.exec(`UPDATE runs SET status='queued', worker_id=NULL WHERE id=$1`, run)

	for name, w := range map[string]uuid.UUID{"predecessor": w1, "older hold holder": w3} {
		_, err := f.pool.Exec(f.ctx, `UPDATE runs SET worker_id=$2 WHERE id=$1`, run, w)
		if err == nil || !store.IsFreshResumePredecessorExcluded(err) || store.IsFreshResumeInProgress(err) {
			t.Fatalf("%s: want predecessor-excluded raise, got %v", name, err)
		}
		// The raise survives any opt-in.
		tx, _ := f.pool.Begin(f.ctx)
		_, _ = tx.Exec(f.ctx, `SELECT set_config('uzi.fresh_resume_id', 'x', true)`)
		_, err = tx.Exec(f.ctx, `UPDATE runs SET worker_id=$2 WHERE id=$1`, run, w)
		_ = tx.Rollback(f.ctx)
		if err == nil || !store.IsFreshResumePredecessorExcluded(err) {
			t.Fatalf("%s: opt-in bypassed rule 2: %v", name, err)
		}
	}
	// Control: an unrelated worker and the destination pass; so does an update that keeps worker_id.
	f.exec(`UPDATE runs SET worker_id=$2 WHERE id=$1`, run, ctrl)
	f.exec(`UPDATE runs SET worker_id=$2 WHERE id=$1`, run, w2)
	// A superseded row still excludes its predecessor.
	f.exec(`UPDATE run_fresh_resumes SET state='superseded' WHERE run_id=$1`, run)
	_, err := f.pool.Exec(f.ctx, `UPDATE runs SET worker_id=$2 WHERE id=$1`, run, w1)
	if err == nil || !store.IsFreshResumePredecessorExcluded(err) {
		t.Fatalf("superseded row must still exclude the predecessor: %v", err)
	}
	// A terminal run is absorbing and unrestricted by rule 2.
	f.exec(`UPDATE runs SET status='failed' WHERE id=$1`, run)
	f.exec(`UPDATE runs SET worker_id=$2 WHERE id=$1`, run, w1)
}

func TestFreshResumeAfterTriggerTerminalLiveDB(t *testing.T) {
	f := newFreshFx(t)
	w1, w2 := f.newWorker(), f.newWorker()
	run := f.runningRun(w2, 6)
	seeded := f.seeded(run, w1, w2, 6, "seeded")
	f.exec(`UPDATE runs SET status='completed' WHERE id=$1`, run)
	if got := f.transferState(seeded); got != "finished" {
		t.Fatalf("seeded row after completion = %s", got)
	}

	run2 := f.runningRun(w2, 6)
	sup := f.seeded(run2, w1, w2, 5, "superseded")
	pend := f.transfer(run2, "pending", uuid.Nil, 0)
	f.exec(`UPDATE runs SET status='cancelled' WHERE id=$1`, run2)
	if f.transferState(sup) != "finished" || f.transferState(pend) != "cancelled" {
		t.Fatalf("superseded=%s pending=%s", f.transferState(sup), f.transferState(pend))
	}
}

func TestFreshResumeWriteOnceLiveDB(t *testing.T) {
	f := newFreshFx(t)
	w1, w2 := f.newWorker(), f.newWorker()
	run := f.runningRun(w2, 6)
	id := f.seeded(run, w1, w2, 6, "seeded")
	for _, set := range []string{"destination_attempt_id='20260101T000000Z-g6-fedcba9876543210'", "seeded_generation=7"} {
		_, err := f.pool.Exec(f.ctx, `UPDATE run_fresh_resumes SET `+set+` WHERE id=$1`, id)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
			t.Fatalf("%s: want 23514, got %v", set, err)
		}
	}
	// A state change is still allowed.
	f.exec(`UPDATE run_fresh_resumes SET state='superseded' WHERE id=$1`, id)
	// The shape CHECK: attempt id without seeded_at.
	_, err := f.pool.Exec(f.ctx, `UPDATE run_fresh_resumes SET seeded_at=NULL WHERE id=$1`, id)
	if err == nil {
		t.Fatal("clearing seeded_at alone must fail")
	}
}

// The attempt-id CHECK mirrors the agent's ATTEMPT_ID_RE with a length bound.
func TestFreshResumeAttemptIDShapeCheckLiveDB(t *testing.T) {
	f := newFreshFx(t)
	w1, w2 := f.newWorker(), f.newWorker()
	run := f.runningRun(w2, 6)
	id := f.seeded(run, w1, w2, 6, "seeded")
	f.exec(`UPDATE run_fresh_resumes SET state='superseded' WHERE id=$1`, id)
	for name, bad := range map[string]string{
		"free text":            "a-g6-x",
		"upper hex":            "20260101T000000Z-g6-0123456789ABCDEF",
		"short hex":            "20260101T000000Z-g6-0123456789abcde",
		"trailing newline":     "20260101T000000Z-g6-0123456789abcdef\n",
		"generation too wide":  "20260101T000000Z-g1234567890123456789-0123456789abcdef",
		"prefix before stamp":  "x20260101T000000Z-g6-0123456789abcdef",
		"padded past 64 bytes": "20260101T000000Z-g6-0123456789abcdef" + strings.Repeat("0", 40),
	} {
		var pgErr *pgconn.PgError
		_, err := f.pool.Exec(f.ctx, `INSERT INTO run_fresh_resumes (run_id, user_id, request_id, evidence_generation, evidence_kind,
 checkpoint_ref, sha, candidate_token, state, destination_attempt_id, seeded_generation, seeded_at)
 VALUES ($1,$2,$3,1,'checkpoint_ref','r','abc','tok','finished',$4,6,now())`, run, f.owner, uuid.New(), bad)
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != "run_fresh_resumes_attempt_id_shape" {
			t.Fatalf("%s: want the attempt id shape CHECK, got %v", name, err)
		}
	}
	for _, ok := range []string{attemptID(6), "20260101T000000Z-gx-0123456789abcdef"} {
		f.exec(`INSERT INTO run_fresh_resumes (run_id, user_id, request_id, evidence_generation, evidence_kind,
 checkpoint_ref, sha, candidate_token, state, destination_attempt_id, seeded_generation, seeded_at)
 VALUES ($1,$2,$3,1,'checkpoint_ref','r','abc','tok','finished',$4,6,now())`, run, f.owner, uuid.New(), ok)
	}
}

func TestFreshResumeVersionOnlyUpdatePassesLiveDB(t *testing.T) {
	f := newFreshFx(t)
	w2 := f.newWorker()
	run := f.run("recovery_wait", w2, 4)
	f.transfer(run, "pending", uuid.Nil, 0)
	before := f.snap(run)
	v, err := f.q.BumpFreshHoldVersion(f.ctx, run)
	if err != nil {
		t.Fatalf("version-only update must pass every row trigger: %v", err)
	}
	after := f.snap(run)
	if v != before.FreshHoldVersion+1 || after.FreshHoldVersion != v {
		t.Fatalf("version %d -> %d (returned %d)", before.FreshHoldVersion, after.FreshHoldVersion, v)
	}
	after.FreshHoldVersion = before.FreshHoldVersion
	if after != before {
		t.Fatalf("version bump changed other fields:\n%+v\n%+v", before, after)
	}
}

func TestFreshResumeRequestConflictMappingLiveDB(t *testing.T) {
	f := newFreshFx(t)
	run := f.run("recovery_wait", uuid.Nil, 4)
	req := uuid.New()
	insert := func(request uuid.UUID) error {
		_, err := f.q.InsertFreshResumePending(f.ctx, store.InsertFreshResumePendingParams{
			RunID: run, UserID: f.owner, RequestID: request, EvidenceGeneration: 3, EvidenceKind: "checkpoint_ref",
			CheckpointRef: pgtype.Text{String: "refs/uzi-checkpoints/x", Valid: true}, Sha: "abc", CandidateToken: "t"})
		return err
	}
	if err := insert(req); err != nil {
		t.Fatal(err)
	}
	// Same request id: the named constraint.
	err := insert(req)
	if err == nil || !store.IsFreshResumeRequestConflict(err) {
		t.Fatalf("duplicate request id: want request conflict, got %v", err)
	}
	// New request id while one row is pending: the per-run partial unique index, never a request conflict.
	err = insert(uuid.New())
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" || store.IsFreshResumeRequestConflict(err) {
		t.Fatalf("second active row: want a non-request 23505, got %v (constraint %q)", err, pgErr.ConstraintName)
	}
	// After the row closes, the same request id is still a request conflict (completed replay).
	if _, err := f.q.CloseFreshResume(f.ctx, store.CloseFreshResumeParams{
		ID: mustGetActive(t, f, run), State: "refused", Reason: "admission_deadline"}); err != nil {
		t.Fatal(err)
	}
	if err := insert(req); !store.IsFreshResumeRequestConflict(err) {
		t.Fatalf("completed request replay: %v", err)
	}
	if err := insert(uuid.New()); err != nil {
		t.Fatalf("a new request after the refusal must insert: %v", err)
	}
	row, err := f.q.GetFreshResumeByRequest(f.ctx, store.GetFreshResumeByRequestParams{RunID: run, RequestID: req})
	if err != nil || row.State != "refused" {
		t.Fatalf("by request: %+v %v", row, err)
	}
}

func mustGetActive(t *testing.T, f *freshFx, run uuid.UUID) uuid.UUID {
	t.Helper()
	row, err := f.q.GetActiveFreshResumeForRun(f.ctx, run)
	if err != nil {
		t.Fatal(err)
	}
	return row.ID
}

// The prepare / claim / restore round trip: the saved tuple returns exactly, the version bumps, and the
// monotonic fences (claim generation, codex epoch) are never rolled back.
func TestFreshResumePrepareRestoreRoundTripLiveDB(t *testing.T) {
	f := newFreshFx(t)
	w1, w2 := f.newWorker(), f.newWorker()
	run := f.run("recovery_wait", w1, 3)
	f.exec(`UPDATE runs SET started_at = now() - interval '1 hour', status_since = now() - interval '10 minutes',
 budget_paused_seconds = 5, requeue_count = 2, recovery_wait_cause='retained_recovery_adoption_failed',
 hold_reason = 'x', hold_captured_head = 'deadbeef', claim_released_at = now() - interval '11 minutes',
 released_worker_id = $2, released_worker_nonce = 'n1', stale_requeue_generation = 3,
 health = 'ok', worker_recovery_evidence = '{"checkpoint_tip":null,"available_capture":false,"publication_uncertain":false,"capture_uncertain":false,"custody_uncertain":false,"unknown":false,"recorded_at":"2026-01-01T00:00:00Z"}'::jsonb, forge_admission = '{"b":2}'::jsonb
 WHERE id=$1`, run, w1)
	pend := f.transfer(run, "pending", uuid.Nil, 0)
	var base struct {
		Status, Cause, HoldReason, Head, Nonce string
		Paused, Count, Baseline                int32
		StatusSince                            time.Time
		Evidence, Forge                        []byte
		Epoch                                  int64
	}
	load := func() {
		if err := f.pool.QueryRow(f.ctx, `SELECT status, recovery_wait_cause, COALESCE(hold_reason,''), COALESCE(hold_captured_head,''),
 COALESCE(released_worker_nonce,''), budget_paused_seconds, requeue_count, requeue_episode_baseline, status_since,
 worker_recovery_evidence, forge_admission, codex_claim_epoch FROM runs WHERE id=$1`, run).
			Scan(&base.Status, &base.Cause, &base.HoldReason, &base.Head, &base.Nonce, &base.Paused, &base.Count,
				&base.Baseline, &base.StatusSince, &base.Evidence, &base.Forge, &base.Epoch); err != nil {
			t.Fatal(err)
		}
	}
	load()
	orig := base

	tx, err := f.pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(f.ctx) }()
	qtx := f.q.WithTx(tx)
	if err := qtx.OptInFreshResume(f.ctx, pend.String()); err != nil {
		t.Fatal(err)
	}
	prepared, err := qtx.PrepareFreshResumeClaim(f.ctx, store.PrepareFreshResumeClaimParams{
		RunID: run, ID: pend, WorkerID: pgtype.UUID{Bytes: w2, Valid: true}})
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Status != "queued" || prepared.WorkerID.Bytes != w2 {
		t.Fatalf("prepared: %+v", prepared)
	}
	if prepared.BudgetPausedSeconds < orig.Paused+590 || prepared.BudgetPausedSeconds > orig.Paused+700 {
		t.Fatalf("paused credit = %d, want about %d", prepared.BudgetPausedSeconds, orig.Paused+600)
	}
	// Emulate ClaimRun's SET (status / worker / generation / epoch); the claim itself is m2's.
	if _, err := tx.Exec(f.ctx, `UPDATE runs SET status='claimed', claim_generation=4, claimed_at=now(),
 claim_released_at=NULL, released_worker_id=NULL, released_worker_nonce=NULL, status_since=now(),
 codex_claim_epoch=codex_claim_epoch+1, codex_cap_hash=NULL WHERE id=$1`, run); err != nil {
		t.Fatal(err)
	}
	row, err := qtx.MarkFreshResumeClaimed(f.ctx, store.MarkFreshResumeClaimedParams{
		ID: pend, WorkerID: pgtype.UUID{Bytes: w2, Valid: true}, ClaimGeneration: pgtype.Int8{Int64: 4, Valid: true}, TransferSeconds: 7200})
	if err != nil {
		t.Fatal(err)
	}
	if row.State != "claimed" || row.PredecessorWorkerID.Bytes != w1 || row.SavedWorkerID.Bytes != w1 || !row.TransferDeadline.Valid {
		t.Fatalf("claimed row: %+v", row)
	}
	// A restore at the wrong generation matches nothing.
	if _, err := tx.Exec(f.ctx, `SAVEPOINT s`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(f.ctx, `UPDATE run_fresh_resumes SET claim_generation=9 WHERE id=$1`, pend); err != nil {
		t.Fatal(err)
	}
	if n, err := qtx.RestoreFreshResumeTuple(f.ctx, pend); err != nil || n != 0 {
		t.Fatalf("wrong generation restore: n=%d err=%v", n, err)
	}
	if _, err := tx.Exec(f.ctx, `ROLLBACK TO s`); err != nil {
		t.Fatal(err)
	}
	n, err := qtx.RestoreFreshResumeTuple(f.ctx, pend)
	if err != nil || n != 1 {
		t.Fatalf("restore: n=%d err=%v", n, err)
	}
	if _, err := qtx.CloseFreshResume(f.ctx, store.CloseFreshResumeParams{ID: pend, State: "refused", Reason: "assembly"}); err != nil {
		t.Fatal(err)
	}
	if err := qtx.ClearFreshResumeOptIn(f.ctx); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	load()
	if base.Status != orig.Status || base.Cause != orig.Cause || base.HoldReason != orig.HoldReason ||
		base.Head != orig.Head || base.Nonce != orig.Nonce || base.Paused != orig.Paused ||
		base.Count != orig.Count || base.Baseline != orig.Baseline || !base.StatusSince.Equal(orig.StatusSince) ||
		string(base.Evidence) != string(orig.Evidence) || string(base.Forge) != string(orig.Forge) {
		t.Fatalf("tuple not restored:\norig %+v\nnow  %+v", orig, base)
	}
	if base.Epoch != orig.Epoch+1 {
		t.Fatalf("codex_claim_epoch rolled back or double bumped: %d -> %d", orig.Epoch, base.Epoch)
	}
	s := f.snap(run)
	if s.ClaimGeneration != 4 || s.WorkerID.Bytes != w1 || s.FreshHoldVersion != 1 {
		t.Fatalf("after restore: %+v", s)
	}
	// A restored run is startable again: no active row, a new request inserts.
	if _, err := f.q.GetActiveFreshResumeForRun(f.ctx, run); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("active row after refusal: %v", err)
	}
}

func TestFreshResumeBaseParityLiveDB(t *testing.T) {
	f := newFreshFx(t)
	w1 := f.newWorker()
	mk := func(mutate string) bool {
		run := f.run("recovery_wait", w1, 3)
		f.exec(`UPDATE runs SET recovery_wait_cause='retained_recovery_adoption_failed', started_at = now() - interval '1 hour',
 status_since = now() WHERE id=$1`, run)
		if mutate != "" {
			f.exec(`UPDATE runs SET `+strings.TrimPrefix(mutate, ", ")+` WHERE id=$1`, run)
		}
		var ok bool
		if err := f.pool.QueryRow(f.ctx, `SELECT fn_fresh_resume_run_base(r, 36000) FROM runs r WHERE id=$1`, run).Scan(&ok); err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if !mk("") {
		t.Fatal("a plain owner-held adoption-failed run must be eligible")
	}
	for _, bad := range []string{
		", recovery_retry_not_before = now() + interval '1 hour'",
		", recovery_wait_detail = 'secret_remediation_active'",
		", recovery_wait_detail = 'source_end_declined'",
		", recovery_wait_cause = 'codex_idle_stall'",
		", policy_pending_fresh = true, policy_preserve_required = true, policy_outcome_id = gen_random_uuid(), policy_continuation_mode = 'same_conversation', " +
			"policy_applying_worker_id = (SELECT id FROM workers WHERE user_id = runs.user_id LIMIT 1), policy_source_generation = 1, " +
			"policy_applying_generation = 1, policy_resolved_at = now()",
		", pause_mode = 'reserve', pause_requested_at = now()",
		", harness = 'codex'",
		", budget_wall_seconds = 60",
	} {
		if mk(bad) {
			t.Fatalf("eligible despite %q", bad)
		}
	}
	// Codex harness on a never-claimed run is not excluded.
	run := f.run("recovery_wait", w1, 0)
	f.exec(`UPDATE runs SET recovery_wait_cause='retained_recovery_exhausted', harness='codex' WHERE id=$1`, run)
	var ok bool
	if err := f.pool.QueryRow(f.ctx, `SELECT fn_fresh_resume_run_base(r, 36000) FROM runs r WHERE id=$1`, run).Scan(&ok); err != nil || !ok {
		t.Fatalf("never-claimed codex run: ok=%v err=%v", ok, err)
	}
	// Parity with the real Resume predicate on the same rows.
	for _, mut := range []string{"", ", recovery_wait_detail = 'secret_remediation_active'", ", budget_wall_seconds = 60"} {
		r := f.run("recovery_wait", w1, 3)
		f.exec(`UPDATE runs SET recovery_wait_cause='retained_recovery_adoption_failed', started_at = now() - interval '1 hour',
 status_since = now() WHERE id=$1`, r)
		if mut != "" {
			f.exec(`UPDATE runs SET `+strings.TrimPrefix(mut, ", ")+` WHERE id=$1`, r)
		}
		var base bool
		if err := f.pool.QueryRow(f.ctx, `SELECT fn_fresh_resume_run_base(r, 36000) FROM runs r WHERE id=$1`, r).Scan(&base); err != nil {
			t.Fatal(err)
		}
		_, err := f.q.ResumeWorkerRecoveryEpisode(f.ctx, store.ResumeWorkerRecoveryEpisodeParams{
			ID: r, UserID: f.owner, GlobalTimeoutSeconds: 36000})
		resumed := err == nil
		if base != resumed {
			t.Fatalf("mutation %q: fresh base=%v but ResumeWorkerRecoveryEpisode applied=%v (%v)", mut, base, resumed, err)
		}
	}
}

func TestFreshResumeReservationCountLiveDB(t *testing.T) {
	f := newFreshFx(t)
	w2 := f.newWorker()
	cutoff := f.cutoff

	members := func() (counted int64, fresh int) {
		for _, m := range f.members(cutoff) {
			if m.Counted {
				counted++
			}
			if m.Reason == "fresh_reservation" {
				fresh++
			}
		}
		return
	}
	check := func(label string, want int64, wantFresh int) {
		t.Helper()
		if got := f.count(cutoff); got != want {
			t.Fatalf("%s: fn_custody_admission_count = %d, want %d", label, got, want)
		}
		counted, fresh := members()
		if counted != want || fresh != wantFresh {
			t.Fatalf("%s: members counted=%d fresh=%d, want %d / %d", label, counted, fresh, want, wantFresh)
		}
	}

	// A pending row is one unit.
	run := f.run("recovery_wait", uuid.Nil, 3)
	pend := f.transfer(run, "pending", uuid.Nil, 0)
	check("pending", 1, 1)

	// A second owner row is a second unit; another owner's row is invisible.
	run2 := f.run("recovery_wait", uuid.Nil, 3)
	f.transfer(run2, "pending", uuid.Nil, 0)
	check("two pending", 2, 2)
	f.exec(`UPDATE run_fresh_resumes SET state='refused', closed_at=now() WHERE run_id=$1`, run2)
	check("refused frees the unit", 1, 1)

	// The pending row carries over to claimed with no gap: W2's healthy G' hold is the live claim, so the
	// 00427 count is 0 and the reservation is the one unit.
	f.exec(`DELETE FROM run_fresh_resumes WHERE id=$1`, pend)
	f.exec(`UPDATE runs SET status='running', worker_id=$2, claim_generation=4 WHERE id=$1`, run, w2)
	pend = f.transfer(run, "claimed", w2, 4)
	f.exec(`INSERT INTO recovery_custody_holds(id,user_id,repo_id,run_id,generation,state,original_worker_id,
 original_worker_identity,live_worker_id,live_run_id) VALUES($1,$2,$3,$4,4,'open',$5,'w2',$5,$4)`,
		uuid.New(), f.owner, f.repo, run, w2)
	check("claimed with healthy hold", 1, 1)

	// W2 goes offline: 00427 counts the hold itself and the reservation stops, so the unit is never doubled.
	f.exec(`UPDATE workers SET last_heartbeat_at = $2 WHERE id=$1`, w2, cutoff.Time.Add(-time.Hour))
	check("claimed, W2 offline", 1, 0)
	f.exec(`UPDATE workers SET last_heartbeat_at = $2 WHERE id=$1`, w2, cutoff.Time)

	// Seeded keeps the unit while the hold is healthy, and a terminal run releases it.
	f.exec(`UPDATE run_fresh_resumes SET state='seeded', destination_attempt_id=$2, seeded_generation=4, seeded_at=now() WHERE id=$1`, pend, attemptID(4))
	check("seeded", 1, 1)
	f.exec(`UPDATE runs SET status='completed' WHERE id=$1`, run)
	if f.count(cutoff) != 1 {
		// the open hold of a terminal run still counts as custody (00427); only the reservation vanishes
		if _, fresh := members(); fresh != 0 {
			t.Fatalf("terminal run still reserves a unit")
		}
	}
	if _, fresh := members(); fresh != 0 {
		t.Fatalf("terminal run still reserves a unit")
	}
}

func TestFreshResumeReservationOnlyFromOwnRowsLiveDB(t *testing.T) {
	f := newFreshFx(t)
	run := f.run("recovery_wait", uuid.Nil, 3)
	f.transfer(run, "pending", uuid.Nil, 0)
	var other int64
	if err := f.pool.QueryRow(f.ctx, `SELECT fn_custody_admission_count($1,$2)`, f.other, f.cutoff).Scan(&other); err != nil || other != 0 {
		t.Fatalf("another owner sees %d (%v)", other, err)
	}
}

// The Down guard's DO block refuses while an outstanding row exists and passes once it is closed.
func TestFreshResumeDownGuardLiveDB(t *testing.T) {
	f := newFreshFx(t)
	src, err := os.ReadFile("migrations/00535_run_fresh_resumes.sql")
	if err != nil {
		t.Fatal(err)
	}
	text := string(src)
	down := text[strings.Index(text, "-- +goose Down"):]
	start := strings.Index(down, "DO $$ BEGIN")
	end := strings.Index(down[start:], "END $$;")
	if start < 0 || end < 0 {
		t.Fatal("Down guard block not found")
	}
	block := down[start : start+end+len("END $$;")]
	runBlock := func() error {
		tx, err := f.pool.Begin(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(context.Background()) }()
		_, err = tx.Exec(context.Background(), block)
		return err
	}
	if err := runBlock(); err != nil {
		t.Fatalf("clean database must pass the Down guard: %v", err)
	}
	run := f.run("recovery_wait", uuid.Nil, 3)
	id := f.transfer(run, "pending", uuid.Nil, 0)
	if err := runBlock(); err == nil || !strings.Contains(err.Error(), "outstanding fresh resume transfers") {
		t.Fatalf("pending row must refuse the Down: %v", err)
	}
	f.exec(`UPDATE run_fresh_resumes SET state='seeded', destination_attempt_id=$2, seeded_generation=1, seeded_at=now() WHERE id=$1`, id, attemptID(1))
	if err := runBlock(); err == nil {
		t.Fatal("a seeded row on a live run must refuse the Down")
	}
	f.exec(`UPDATE runs SET status='cancelled' WHERE id=$1`, run)
	if err := runBlock(); err != nil {
		t.Fatalf("terminal run must pass: %v", err)
	}
}

func TestFreshResumeIndexesServeLookupsLiveDB(t *testing.T) {
	f := newFreshFx(t)
	// The plain (run_id) index was redundant with UNIQUE (run_id, request_id) and is gone.
	var plain int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM pg_indexes WHERE tablename = 'run_fresh_resumes' AND indexname = 'run_fresh_resumes_run_idx'`).Scan(&plain); err != nil || plain != 0 {
		t.Fatalf("redundant run_fresh_resumes_run_idx present: n=%d err=%v", plain, err)
	}
	run := f.run("recovery_wait", uuid.Nil, 3)
	f.transfer(run, "pending", uuid.Nil, 0)
	tx, err := f.pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(f.ctx) }()
	if _, err := tx.Exec(f.ctx, `SET LOCAL enable_seqscan = off`); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`SELECT 1 FROM run_fresh_resumes WHERE run_id = $1 AND state IN ('pending','claimed')`,
		`SELECT 1 FROM run_fresh_resumes WHERE run_id = $1 AND state IN ('seeded','superseded')`,
		// Any-state history and the request replay lookup are served by UNIQUE (run_id, request_id).
		`SELECT 1 FROM run_fresh_resumes WHERE run_id = $1`,
	} {
		rows, err := tx.Query(f.ctx, `EXPLAIN `+q, run)
		if err != nil {
			t.Fatal(err)
		}
		var plan strings.Builder
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatal(err)
			}
			plan.WriteString(line + "\n")
		}
		rows.Close()
		if strings.Contains(plan.String(), "Seq Scan on run_fresh_resumes") {
			t.Fatalf("seq scan for %s:\n%s", q, plan.String())
		}
	}
}

// attemptID builds a destination attempt id in the agent's grammar (ATTEMPT_ID_RE in
// agent/src/attempt-path.ts) for claim generation gen.
func attemptID(gen int64) string {
	return fmt.Sprintf("20260101T000000Z-g%d-0123456789abcdef", gen)
}

func TestFreshResumeStateRouteAndSeededLiveDB(t *testing.T) {
	f := newFreshFx(t)
	w1, w2 := f.newWorker(), f.newWorker()
	run := f.run("claimed", w2, 4)
	id := f.transfer(run, "claimed", w2, 4)
	wid := pgtype.UUID{Bytes: w2, Valid: true}
	g4 := pgtype.Int8{Int64: 4, Valid: true}
	// A mismatched attempt id and a wrong generation or worker never seed. The generation is parsed from
	// the agent's attempt-id grammar, never found by substring position.
	for name, p := range map[string]store.MarkFreshResumeSeededParams{
		"attempt of another generation": {ID: id, AttemptID: attemptID(5), WorkerID: wid, ClaimGeneration: g4},
		"generation with a longer tail": {ID: id, AttemptID: attemptID(14), WorkerID: wid, ClaimGeneration: g4},
		"gx attempt has no generation":  {ID: id, AttemptID: "20260101T000000Z-gx-0123456789abcdef", WorkerID: wid, ClaimGeneration: g4},
		"not the attempt grammar":       {ID: id, AttemptID: "a-g4-x", WorkerID: wid, ClaimGeneration: g4},
		"generation embedded elsewhere": {ID: id, AttemptID: "x-g4-20260101T000000Z-g9-0123456789abcdef", WorkerID: wid, ClaimGeneration: g4},
		"wrong generation":              {ID: id, AttemptID: attemptID(4), WorkerID: wid, ClaimGeneration: pgtype.Int8{Int64: 5, Valid: true}},
		"wrong worker":                  {ID: id, AttemptID: attemptID(4), WorkerID: pgtype.UUID{Bytes: w1, Valid: true}, ClaimGeneration: g4},
	} {
		if _, err := f.q.MarkFreshResumeSeeded(f.ctx, p); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// Chained: an earlier seeded row is superseded first, then the new row seeds.
	prev := uuid.New()
	_ = prev
	row, err := f.q.MarkFreshResumeSeeded(f.ctx, store.MarkFreshResumeSeededParams{ID: id, AttemptID: attemptID(4), WorkerID: wid, ClaimGeneration: g4})
	if err != nil || row.State != "seeded" || row.SeededGeneration.Int64 != 4 || row.DestinationAttemptID.String != attemptID(4) {
		t.Fatalf("seed: %+v %v", row, err)
	}
	st, err := f.q.GetFreshResumeState(f.ctx, store.GetFreshResumeStateParams{ID: id, RunID: run, WorkerID: wid})
	if err != nil || st.State != "seeded" || st.DestinationAttemptID.String != attemptID(4) {
		t.Fatalf("state: %+v %v", st, err)
	}
	if _, err := f.q.GetFreshResumeState(f.ctx, store.GetFreshResumeStateParams{ID: id, RunID: run, WorkerID: pgtype.UUID{Bytes: w1, Valid: true}}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("state for another worker: %v", err)
	}
	// Second seeded row on the same run needs the earlier one superseded first (one seeded per run).
	f.exec(`UPDATE runs SET status='recovery_wait' WHERE id=$1`, run)
	id2 := f.transfer(run, "claimed", w1, 5)
	n, err := f.q.SupersedePriorSeeded(f.ctx, store.SupersedePriorSeededParams{RunID: run, ID: id2})
	if err != nil || n != 1 {
		t.Fatalf("supersede: %d %v", n, err)
	}
	if f.transferState(id) != "superseded" {
		t.Fatalf("prior row = %s", f.transferState(id))
	}
}

func hasOwner(owners []uuid.UUID, want uuid.UUID) bool {
	for _, o := range owners {
		if o == want {
			return true
		}
	}
	return false
}

// An owner whose predecessor holds were all discarded but who has an outstanding transfer row (a reserved
// unit) still appears in both over-limit scans; once the row closes it does not.
func TestFreshResumeOverLimitScansIncludeTransferOwnersLiveDB(t *testing.T) {
	f := newFreshFx(t)
	run := f.run("recovery_wait", uuid.Nil, 3)
	id := f.transfer(run, "pending", uuid.Nil, 0)
	scan := func() (over, candidates []uuid.UUID) {
		var err error
		over, err = f.q.ListOwnersOverCustodyLimit(f.ctx, store.ListOwnersOverCustodyLimitParams{CustodyHoldLimit: 1, HeartbeatCutoff: f.cutoff})
		if err != nil {
			t.Fatal(err)
		}
		candidates, err = f.q.ListHealthCustodyCandidateOwners(f.ctx, store.ListHealthCustodyCandidateOwnersParams{
			OwnerIds: []uuid.UUID{f.owner, f.other}, CustodyHoldLimit: 1})
		if err != nil {
			t.Fatal(err)
		}
		return
	}
	over, cand := scan()
	if !hasOwner(over, f.owner) || !hasOwner(cand, f.owner) || hasOwner(cand, f.other) {
		t.Fatalf("outstanding transfer owner missing from a scan: over=%v candidates=%v", hasOwner(over, f.owner), cand)
	}
	f.exec(`UPDATE run_fresh_resumes SET state='refused', closed_at=now() WHERE id=$1`, id)
	over, cand = scan()
	if hasOwner(over, f.owner) || hasOwner(cand, f.owner) {
		t.Fatalf("a closed transfer still lists its owner: over=%v candidates=%v", hasOwner(over, f.owner), hasOwner(cand, f.owner))
	}
}

func TestFreshResumeCheckpointTipGenerationStampLiveDB(t *testing.T) {
	f := newFreshFx(t)
	run := f.run("running", f.worker, 3)
	read := func() (gen pgtype.Int8, genTip, tip pgtype.Text) {
		if err := f.pool.QueryRow(f.ctx, `SELECT checkpoint_tip_generation, checkpoint_tip_generation_tip, checkpoint_tip FROM runs WHERE id=$1`, run).
			Scan(&gen, &genTip, &tip); err != nil {
			t.Fatal(err)
		}
		return
	}
	tip1, tip2 := strings.Repeat("a", 40), strings.Repeat("b", 40)
	if _, err := f.q.SetRunCheckpointTip(f.ctx, store.SetRunCheckpointTipParams{
		ID: run, CheckpointTip: pgtype.Text{String: tip1, Valid: true}, TipGeneration: pgtype.Int8{Int64: 3, Valid: true}}); err != nil {
		t.Fatal(err)
	}
	if g, gt, tp := read(); g.Int64 != 3 || !g.Valid || gt.String != tip1 || tp.String != tip1 {
		t.Fatalf("stamped write: gen=%v genTip=%v tip=%v", g, gt, tp)
	}
	// An unfenced republish of the same tip keeps the binding; a different tip without a generation reads unbound.
	if _, err := f.q.SetRunCheckpointTip(f.ctx, store.SetRunCheckpointTipParams{ID: run, CheckpointTip: pgtype.Text{String: tip1, Valid: true}}); err != nil {
		t.Fatal(err)
	}
	if g, gt, _ := read(); g.Int64 != 3 || gt.String != tip1 {
		t.Fatalf("same-tip unfenced write dropped the binding: gen=%v genTip=%v", g, gt)
	}
	if _, err := f.q.SetRunCheckpointTip(f.ctx, store.SetRunCheckpointTipParams{ID: run, CheckpointTip: pgtype.Text{String: tip2, Valid: true}}); err != nil {
		t.Fatal(err)
	}
	if g, gt, tp := read(); g.Valid || gt.String != tip2 || tp.String != tip2 {
		t.Fatalf("a new tip without a generation must read unbound: gen=%v genTip=%v tip=%v", g, gt, tp)
	}
	// SetRunCheckpointTipIf stamps too (compare-and-set on the observed tip).
	if n, err := f.q.SetRunCheckpointTipIf(f.ctx, store.SetRunCheckpointTipIfParams{
		ID: run, CheckpointTip: tip1, ExpectedTip: pgtype.Text{String: tip2, Valid: true}, TipGeneration: pgtype.Int8{Int64: 4, Valid: true}}); err != nil || n != 1 {
		t.Fatalf("SetRunCheckpointTipIf: n=%d err=%v", n, err)
	}
	if g, gt, tp := read(); g.Int64 != 4 || gt.String != tip1 || tp.String != tip1 {
		t.Fatalf("CAS stamp: gen=%v genTip=%v tip=%v", g, gt, tp)
	}
	// The frozen pre-change statement rewrites the tip without restamping: the binding reads unbound
	// (generation_tip differs from the tip), so a capture is refused unless a candidate dominates it.
	if _, err := f.pool.Exec(f.ctx, base_setRunCheckpointTip, pgtype.Text{String: tip2, Valid: true}, pgtype.Text{}, run); err != nil {
		t.Fatal(err)
	}
	g, gt, tp := read()
	if tp.String != tip2 || gt.String == tp.String {
		t.Fatalf("base-text write must leave generation_tip != tip: genTip=%v tip=%v", gt, tp)
	}
	if !g.Valid {
		t.Fatalf("the stale generation is still stored; only the shadow binding marks it unbound")
	}
}

func TestFreshResumeCaptureGuardLiveDB(t *testing.T) {
	f := newFreshFx(t)
	// The transfer row references the capture (RESTRICT); the fixture cleanup deletes captures first.
	t.Cleanup(func() { f.exec(`DELETE FROM run_fresh_resumes WHERE user_id = $1`, f.owner) })
	w1 := f.newWorker()
	run := f.run("recovery_wait", w1, 3)
	hold := f.hold(run, 3)
	capID := uuid.New()
	f.exec(`INSERT INTO recovery_captures(id,hold_id,run_id,user_id,original_worker_id,original_worker_identity,source_sha,idempotency_key,state,manifest_bound,expires_at)
 VALUES($1,$2,$3,$4,$5,'o','abc',$6,'available',true, now() - interval '1 hour')`, capID, hold, run, f.owner, w1, uuid.New().String())
	f.exec(`INSERT INTO run_fresh_resumes (run_id, user_id, request_id, evidence_generation, evidence_kind, capture_id, sha, candidate_token, state)
 VALUES ($1,$2,$3,3,'capture',$4,'abc','tok','pending')`, run, f.owner, uuid.New(), capID)
	ref, err := f.q.CaptureHasActiveFreshResume(f.ctx, pgtype.UUID{Bytes: capID, Valid: true})
	if err != nil || !ref {
		t.Fatalf("CaptureHasActiveFreshResume = %v, %v", ref, err)
	}
	n, err := f.q.DiscardCaptureForOwner(f.ctx, store.DiscardCaptureForOwnerParams{ID: capID, RunID: run, UserID: f.owner})
	if err != nil || n != 0 {
		t.Fatalf("a referenced capture must not discard: n=%d err=%v", n, err)
	}
	if exp, err := f.q.ExpireReadyCaptures(f.ctx, pgtype.Timestamptz{Time: time.Now(), Valid: true}); err != nil {
		t.Fatal(err)
	} else {
		_ = exp
	}
	var state string
	if err := f.pool.QueryRow(f.ctx, `SELECT state FROM recovery_captures WHERE id=$1`, capID).Scan(&state); err != nil || state != "available" {
		t.Fatalf("referenced capture state = %q, %v", state, err)
	}
	// Once the transfer closes the guard releases it.
	f.exec(`UPDATE run_fresh_resumes SET state='refused', closed_at=now() WHERE capture_id=$1`, capID)
	if ref, _ := f.q.CaptureHasActiveFreshResume(f.ctx, pgtype.UUID{Bytes: capID, Valid: true}); ref {
		t.Fatal("closed transfer still references the capture")
	}
	if n, err := f.q.DiscardCaptureForOwner(f.ctx, store.DiscardCaptureForOwnerParams{ID: capID, RunID: run, UserID: f.owner}); err != nil || n != 1 {
		t.Fatalf("discard after close: n=%d err=%v", n, err)
	}
}
