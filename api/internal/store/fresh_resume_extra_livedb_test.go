package store_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// runRow reads the whole runs row as JSON, so a test can compare every column at once.
func (f *freshFx) runRow(run uuid.UUID) map[string]any {
	f.t.Helper()
	var raw []byte
	if err := f.pool.QueryRow(f.ctx, `SELECT to_jsonb(r) FROM runs r WHERE id=$1`, run).Scan(&raw); err != nil {
		f.t.Fatal(err)
	}
	return jsonRow(f.t, raw)
}

func jsonRow(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// otherOwnerRun inserts a run (and the repo and worker it needs) for the second user.
func (f *freshFx) otherOwnerRun(status string) (run, worker uuid.UUID) {
	f.t.Helper()
	conn, repo := uuid.New(), uuid.New()
	worker, run = uuid.New(), uuid.New()
	f.exec(`INSERT INTO forge_connections (id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext)
 VALUES ($1,$2,'github','https://forge.e2e','bot',1,$3)`, conn, f.other, []byte{1})
	f.exec(`INSERT INTO repos (id,connection_id,forge_project_id,path_with_namespace,web_url,default_branch,enabled)
 VALUES ($1,$2,1,'g/other','https://forge.e2e/g/other','main',true)`, repo, conn)
	f.exec(`INSERT INTO workers(id,user_id,name,token_hash,status,last_heartbeat_at)
 VALUES($1,$2,$3,$4,'online',now())`, worker, f.other, worker.String(), worker[:])
	f.n++
	f.exec(`INSERT INTO runs(id,user_id,repo_id,kind,issue_iid,issue_title,issue_description,status,worker_id,claim_generation)
 VALUES($1,$2,$3,'issue',$4,'other','test',$5,$6,2)`, run, f.other, repo, f.n, status, worker)
	return run, worker
}

// A global sweep must skip a run with a pending or claimed transfer (the literal in its statement) and
// still act on another owner's due run in the same statement: the guard raise alone would abort the batch.
func TestFreshResumeGlobalSweepsSkipTransferRunsLiveDB(t *testing.T) {
	t.Run("ParkRunsAtWall", func(t *testing.T) {
		f := newFreshFx(t)
		w2 := f.newWorker()
		f.exec(`UPDATE workers SET status='online', last_heartbeat_at=now() WHERE id=$1`, w2)
		transferRun := f.runningRun(w2, 4)
		f.exec(`UPDATE runs SET started_at = now() - interval '2 hours', status_since = now() - interval '30 seconds',
 budget_wall_seconds = 60, claim_released_at = NULL WHERE id = $1`, transferRun)
		f.transfer(transferRun, "claimed", w2, 4)
		otherRun, _ := f.otherOwnerRun("running")
		f.exec(`UPDATE runs SET started_at = now() - interval '2 hours', status_since = now() - interval '30 seconds',
 budget_wall_seconds = 60 WHERE id = $1`, otherRun)
		before := f.snap(transferRun)

		now := time.Now().UTC()
		parked, err := f.q.ParkRunsAtWall(f.ctx, store.ParkRunsAtWallParams{
			Now: pgtype.Timestamptz{Time: now, Valid: true}, GlobalTimeoutSeconds: 1,
			WorkerStaleCutoff: pgtype.Timestamptz{Time: now.Add(-45 * time.Second), Valid: true}, GraceSeconds: 600})
		if err != nil {
			t.Fatalf("ParkRunsAtWall: %v", err)
		}
		var ids []uuid.UUID
		for _, r := range parked {
			ids = append(ids, r.ID)
		}
		if !hasOwner(ids, otherRun) {
			t.Fatalf("the other owner's due run was not parked: %v", ids)
		}
		if hasOwner(ids, transferRun) {
			t.Fatal("the sweep parked a run with a claimed transfer")
		}
		if after := f.snap(transferRun); after != before {
			t.Fatalf("transfer run changed:\nbefore %+v\nafter  %+v", before, after)
		}
		if got := f.snap(otherRun).Status; got != "paused" {
			t.Fatalf("other run status = %s, want paused", got)
		}
	})

	t.Run("SweepClaimedNeverStarted", func(t *testing.T) {
		f := newFreshFx(t)
		w2 := f.newWorker()
		transferRun := f.run("claimed", w2, 4)
		f.exec(`UPDATE runs SET claimed_at = now() - interval '3 hours' WHERE id = $1`, transferRun)
		f.transfer(transferRun, "claimed", w2, 4)
		otherRun, _ := f.otherOwnerRun("claimed")
		f.exec(`UPDATE runs SET claimed_at = now() - interval '3 hours' WHERE id = $1`, otherRun)
		before := f.snap(transferRun)

		swept, err := f.q.SweepClaimedNeverStarted(f.ctx, store.SweepClaimedNeverStartedParams{
			Cutoff:        pgtype.Timestamptz{Time: time.Now().Add(-time.Hour), Valid: true},
			WitnessCutoff: pgtype.Timestamptz{Time: time.Now().Add(-time.Hour), Valid: true}})
		if err != nil {
			t.Fatalf("SweepClaimedNeverStarted: %v", err)
		}
		var ids []uuid.UUID
		for _, r := range swept {
			ids = append(ids, r.ID)
		}
		if !hasOwner(ids, otherRun) {
			t.Fatalf("the other owner's never-started run was not swept: %v", ids)
		}
		if hasOwner(ids, transferRun) {
			t.Fatal("the sweep touched a run with a claimed transfer")
		}
		if after := f.snap(transferRun); after != before {
			t.Fatalf("transfer run changed:\nbefore %+v\nafter  %+v", before, after)
		}
	})
}

// A singleton W2 hold (repo_id NULL, so never grouped) of an offline W2 is already counted by the 00427
// count; the claimed transfer must not reserve a second unit for it. A mutant that anti-joined the
// grouped set instead of the counted set would give 2 for both the count and the members.
func TestFreshResumeReservationAntiJoinsCountedSingletonsLiveDB(t *testing.T) {
	f := newFreshFx(t)
	w2 := f.newWorker()
	f.exec(`UPDATE workers SET last_heartbeat_at = $2 WHERE id=$1`, w2, f.cutoff.Time.Add(-time.Hour))
	run := f.run("running", w2, 4)
	holdID := uuid.New()
	f.exec(`INSERT INTO recovery_custody_holds(id,user_id,run_id,generation,state,original_worker_id,
 original_worker_identity,live_worker_id,live_run_id) VALUES($1,$2,$3,4,'open',$4,'w2',$4,$3)`, holdID, f.owner, run, w2)
	f.transfer(run, "claimed", w2, 4)

	if got := f.count(f.cutoff); got != 1 {
		t.Fatalf("fn_custody_admission_count = %d, want 1 (the singleton hold, no reservation)", got)
	}
	var counted int64
	for id, m := range f.members(f.cutoff) {
		if m.Counted {
			counted++
		}
		if m.Reason == "fresh_reservation" {
			t.Fatalf("member %s is a fresh reservation although the hold is counted", id)
		}
	}
	if counted != 1 {
		t.Fatalf("members counted = %d, want 1", counted)
	}
}

// Deleting the predecessor worker while the row is pending succeeds: the ON DELETE SET NULL cascade of
// runs.worker_id is the guard's documented exception and nothing else references the worker.
func TestFreshResumeDeletePredecessorWorkerWhilePendingLiveDB(t *testing.T) {
	f := newFreshFx(t)
	w1 := f.newWorker()
	run := f.run("recovery_wait", w1, 3)
	f.exec(`UPDATE runs SET recovery_wait_cause = 'retained_recovery_adoption_failed', released_worker_id = $2 WHERE id = $1`, run, w1)
	id := f.transfer(run, "pending", uuid.Nil, 0)
	f.exec(`DELETE FROM workers WHERE id = $1`, w1)
	var worker pgtype.UUID
	var status string
	if err := f.pool.QueryRow(f.ctx, `SELECT worker_id, status FROM runs WHERE id=$1`, run).Scan(&worker, &status); err != nil {
		t.Fatal(err)
	}
	if worker.Valid || status != "recovery_wait" {
		t.Fatalf("after worker delete: worker=%v status=%s", worker, status)
	}
	if got := f.transferState(id); got != "pending" {
		t.Fatalf("pending row state = %s", got)
	}
}

// W2's allowlisted SetRunRunning advance writes more than the claim: a refusal after it must still put
// back every pre-claim value. The whole runs row is compared, minus the monotonic fences and bookkeeping.
func TestFreshResumeRestoreUndoesSetRunRunningLiveDB(t *testing.T) {
	f := newFreshFx(t)
	w1, w2 := f.newWorker(), f.newWorker()
	run := f.run("recovery_wait", w1, 3)
	f.exec(`UPDATE runs SET started_at = now() - interval '1 hour', first_started_at = NULL, status_since = now() - interval '10 minutes',
 recovery_wait_cause = 'retained_recovery_adoption_failed', released_worker_id = $2, claim_released_at = now() - interval '11 minutes',
 session_id = 'sess-old', open_question_id = 'q-old', completion_question_at = now() - interval '5 minutes', iteration_count = 2,
 hold_reason = 'budget_exhausted', hold_captured_head = 'deadbeef', required_tools = ARRAY['go'], size_class = 'm',
 agent_source = 'repo', repo_agents = '[{"name":"old"}]'::jsonb, milestones_completed = '["m0"]'::jsonb
 WHERE id = $1`, run, w1)
	pend := f.transfer(run, "pending", uuid.Nil, 0)
	orig := f.runRow(run)

	tx, err := f.pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(f.ctx) }()
	qtx := f.q.WithTx(tx)
	wid := pgtype.UUID{Bytes: w2, Valid: true}
	if err := qtx.OptInFreshResume(f.ctx, pend.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := qtx.PrepareFreshResumeClaim(f.ctx, store.PrepareFreshResumeClaimParams{RunID: run, ID: pend, WorkerID: wid}); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(f.ctx, `UPDATE runs SET status='claimed', claim_generation=4, claimed_at=now(),
 claim_released_at=NULL, released_worker_id=NULL, released_worker_nonce=NULL, status_since=now(),
 codex_claim_epoch=codex_claim_epoch+1, codex_cap_hash=NULL WHERE id=$1`, run); err != nil {
		t.Fatal(err)
	}
	if _, err := qtx.MarkFreshResumeClaimed(f.ctx, store.MarkFreshResumeClaimedParams{
		ID: pend, WorkerID: wid, ClaimGeneration: pgtype.Int8{Int64: 4, Valid: true}, TransferSeconds: 7200}); err != nil {
		t.Fatal(err)
	}
	n, err := qtx.SetRunRunning(f.ctx, store.SetRunRunningParams{
		ID: run, WorkerID: wid, IterationCount: 5,
		SessionID:   pgtype.Text{String: "sess-new", Valid: true},
		RepoAgents:  []byte(`[{"name":"w2"}]`),
		AgentSource: pgtype.Text{String: "own", Valid: true},
		SizeClass:   pgtype.Text{String: "l", Valid: true},
		// Budget inputs: a size 'l' run with at most one milestone gets budget_* stamped by this advance.
		RunMaxIterations: 10, SizeBudgetFactorL: 2, MilestoneBudgetCap: 5, RunTimeoutSeconds: 3600, BudgetWallCeilingSeconds: 7200,
		MilestonesCompleted: []byte(`["m1"]`)})
	if err != nil || n != 1 {
		t.Fatalf("SetRunRunning: n=%d err=%v", n, err)
	}
	advanced := f.runRowIn(tx, run)
	if advanced["session_id"] != "sess-new" || advanced["iteration_count"].(float64) != 5 || advanced["open_question_id"] != nil ||
		advanced["completion_question_at"] != nil || advanced["first_started_at"] == nil {
		t.Fatalf("the running advance did not write what this test restores: %v", advanced)
	}
	if n, err := qtx.RestoreFreshResumeTuple(f.ctx, pend); err != nil || n != 1 {
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
	now := f.runRow(run)
	fenced := map[string]bool{"updated_at": true, "fresh_hold_version": true, "claim_generation": true, "codex_claim_epoch": true}
	var diff []string
	for k, v := range orig {
		if !fenced[k] && !reflect.DeepEqual(v, now[k]) {
			diff = append(diff, k)
		}
	}
	sort.Strings(diff)
	if len(diff) > 0 {
		for _, k := range diff {
			t.Errorf("column %s: before claim %v, after restore %v", k, orig[k], now[k])
		}
		t.Fatal("restore left claim-written values behind")
	}
}

func (f *freshFx) runRowIn(tx pgx.Tx, run uuid.UUID) map[string]any {
	f.t.Helper()
	var raw []byte
	if err := tx.QueryRow(f.ctx, `SELECT to_jsonb(r) FROM runs r WHERE id=$1`, run).Scan(&raw); err != nil {
		f.t.Fatal(err)
	}
	return jsonRow(f.t, raw)
}

// oldClaimArgs lists ClaimRunParams in positional order without only_run_id, the argument list the
// frozen c6eee1d45 ClaimRun text (base_claimRun) was generated with.
func oldClaimArgs(p store.ClaimRunParams) []any {
	v := reflect.ValueOf(p)
	var args []any
	for i := 0; i < v.NumField(); i++ {
		if v.Type().Field(i).Name == "OnlyRunID" {
			continue
		}
		args = append(args, v.Field(i).Interface())
	}
	return args
}

func claimParamsFor(worker uuid.UUID, user uuid.UUID) store.ClaimRunParams {
	now := time.Now()
	ts := func(d time.Duration) pgtype.Timestamptz { return pgtype.Timestamptz{Time: now.Add(d), Valid: true} }
	return store.ClaimRunParams{
		WorkerID: pgtype.UUID{Bytes: worker, Valid: true}, UserID: user,
		AffinityCutoff: ts(-25 * time.Minute), HeartbeatCutoff: ts(-45 * time.Second), SpreadCutoff: ts(-9 * time.Second),
		BackgroundGraceCutoff: ts(-15 * time.Minute), SnapshotFreshCutoff: ts(-60 * time.Second),
		DockerRepoAllowlist: []uuid.UUID{}, WorkerCaps: []string{}, CapabilityAware: true, WorkerProtocolCaps: []string{},
		WorkerIdentity: "fixture", RequestActiveIds: []uuid.UUID{}, RequestActiveGens: []int64{},
	}
}

// claimPreamble is the caller's sequence before ClaimRun: lock the claimant worker, opt in to the run fences.
func (f *freshFx) claimPreamble(worker uuid.UUID) (pgx.Tx, *store.Queries) {
	f.t.Helper()
	tx, err := f.pool.Begin(f.ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { _ = tx.Rollback(f.ctx) })
	q := f.q.WithTx(tx)
	if _, err := q.GetWorkerForUpdate(f.ctx, worker); err != nil {
		f.t.Fatal(err)
	}
	if err := q.OptInRunFences(f.ctx); err != nil {
		f.t.Fatal(err)
	}
	return tx, q
}

// queuedForStaleOwner prepares a queued run pinned to a stale W2 that the user's fresh W1 may claim.
func (f *freshFx) queuedForStaleOwner(w1, w2 uuid.UUID) uuid.UUID {
	f.t.Helper()
	f.exec(`UPDATE runs SET status = 'cancelled' WHERE id = $1`, f.queued)
	f.exec(`UPDATE workers SET status = 'online', last_heartbeat_at = now() WHERE id = $1`, w1)
	f.exec(`UPDATE workers SET last_heartbeat_at = now() - interval '2 hours' WHERE id = $1`, w2)
	run := f.run("queued", w2, 5)
	f.exec(`UPDATE runs SET updated_at = now() - interval '2 hours' WHERE id = $1`, run)
	return run
}

// F2: the frozen pre-change ClaimRun text, run by the predecessor after a seeded transfer, is refused by
// rule 2 of the guard itself; the current ClaimRun excludes the predecessor and returns no row.
func TestFreshResumeOldClaimRunPredecessorLiveDB(t *testing.T) {
	t.Run("seeded row refuses the old text", func(t *testing.T) {
		f := newFreshFx(t)
		w1, w2 := f.newWorker(), f.newWorker()
		run := f.queuedForStaleOwner(w1, w2)
		f.seeded(run, w1, w2, 4, "seeded")
		before := f.snap(run)

		tx, _ := f.claimPreamble(w1)
		_, err := tx.Exec(f.ctx, base_claimRun, oldClaimArgs(claimParamsFor(w1, f.owner))...)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "UZ416" || pgErr.Message != "fresh resume predecessor excluded" {
			t.Fatalf("old ClaimRun: want UZ416 'fresh resume predecessor excluded', got %v", err)
		}
		if after := f.snap(run); after != before {
			t.Fatalf("the refused claim changed the run:\nbefore %+v\nafter  %+v", before, after)
		}
	})

	t.Run("control without a row claims", func(t *testing.T) {
		f := newFreshFx(t)
		w1, w2 := f.newWorker(), f.newWorker()
		run := f.queuedForStaleOwner(w1, w2)
		tx, _ := f.claimPreamble(w1)
		tag, err := tx.Exec(f.ctx, base_claimRun, oldClaimArgs(claimParamsFor(w1, f.owner))...)
		if err != nil || tag.RowsAffected() != 1 {
			t.Fatalf("old ClaimRun without a transfer row must claim: rows=%d err=%v", tag.RowsAffected(), err)
		}
		var worker uuid.UUID
		if err := tx.QueryRow(f.ctx, `SELECT worker_id FROM runs WHERE id=$1`, run).Scan(&worker); err != nil || worker != w1 {
			t.Fatalf("control claim: worker=%v err=%v", worker, err)
		}
	})

	t.Run("current ClaimRun returns no row", func(t *testing.T) {
		f := newFreshFx(t)
		w1, w2 := f.newWorker(), f.newWorker()
		run := f.queuedForStaleOwner(w1, w2)
		f.seeded(run, w1, w2, 4, "seeded")
		before := f.snap(run)
		tx, q := f.claimPreamble(w1)
		if _, err := q.ClaimRun(f.ctx, claimParamsFor(w1, f.owner)); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("current ClaimRun for the predecessor: want no row, got %v", err)
		}
		if err := tx.Commit(f.ctx); err != nil {
			t.Fatal(err)
		}
		if after := f.snap(run); after != before {
			t.Fatalf("the excluded claim changed the run:\nbefore %+v\nafter  %+v", before, after)
		}
	})

	t.Run("old text against a pending run claims nothing", func(t *testing.T) {
		f := newFreshFx(t)
		w1, w2 := f.newWorker(), f.newWorker()
		f.exec(`UPDATE runs SET status = 'cancelled' WHERE id = $1`, f.queued)
		f.exec(`UPDATE workers SET status = 'online', last_heartbeat_at = now() WHERE id = $1`, w1)
		run := f.run("recovery_wait", w2, 3)
		f.transfer(run, "pending", uuid.Nil, 0)
		before := f.snap(run)
		tx, _ := f.claimPreamble(w1)
		tag, err := tx.Exec(f.ctx, base_claimRun, oldClaimArgs(claimParamsFor(w1, f.owner))...)
		if err != nil || tag.RowsAffected() != 0 {
			t.Fatalf("old ClaimRun against a pending run: rows=%d err=%v", tag.RowsAffected(), err)
		}
		if after := f.snap(run); after != before {
			t.Fatalf("the old claim changed the pending run:\nbefore %+v\nafter  %+v", before, after)
		}
	})
}

// DBA m1 R1: the fresh claim (only_run_id) of a run with no open hold of its own is not charged its
// own pending reservation by ClaimRun's owner-count arm; any other reservation still counts.
func TestFreshResumeClaimNotChargedOwnReservationLiveDB(t *testing.T) {
	setup := func(t *testing.T) (*freshFx, uuid.UUID, uuid.UUID) {
		f := newFreshFx(t)
		f.exec(`UPDATE runs SET status = 'cancelled' WHERE id = $1`, f.queued)
		w2 := f.newWorker()
		f.exec(`UPDATE workers SET status = 'online', last_heartbeat_at = now() WHERE id = $1`, w2)
		run := f.run("queued", uuid.Nil, 3)
		return f, w2, run
	}
	claim := func(f *freshFx, w2, run uuid.UUID, optIn uuid.UUID) error {
		tx, q := f.claimPreamble(w2)
		if optIn != uuid.Nil {
			if err := q.OptInFreshResume(f.ctx, optIn.String()); err != nil {
				f.t.Fatal(err)
			}
		}
		p := claimParamsFor(w2, f.owner)
		p.OnlyRunID = pgtype.UUID{Bytes: run, Valid: true}
		p.CustodyHoldLimit = 1
		got, err := q.ClaimRun(f.ctx, p)
		if err == nil && got.ID != run {
			f.t.Fatalf("claimed %v, want %v", got.ID, run)
		}
		_ = tx.Rollback(f.ctx)
		return err
	}

	t.Run("own pending reservation at the limit claims", func(t *testing.T) {
		f, w2, run := setup(t)
		tid := f.transfer(run, "pending", uuid.Nil, 0)
		if n := f.count(pgtype.Timestamptz{Time: time.Now().Add(-45 * time.Second), Valid: true}); n != 1 {
			t.Fatalf("admission count = %d, want 1 (the run's own reservation)", n)
		}
		if err := claim(f, w2, run, tid); err != nil {
			t.Fatalf("fresh claim charged its own reservation: %v", err)
		}
	})

	t.Run("own pending plus another unit at the limit refuses", func(t *testing.T) {
		f, w2, run := setup(t)
		tid := f.transfer(run, "pending", uuid.Nil, 0)
		other := f.run("recovery_wait", uuid.Nil, 2)
		f.transfer(other, "pending", uuid.Nil, 0)
		if err := claim(f, w2, run, tid); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("own reservation plus another unit at limit 1: want no row, got %v", err)
		}
	})

	t.Run("another run's reservation at the limit refuses", func(t *testing.T) {
		f, w2, run := setup(t)
		other := f.run("recovery_wait", uuid.Nil, 2)
		f.transfer(other, "pending", uuid.Nil, 0)
		if err := claim(f, w2, run, uuid.Nil); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("claim over the limit: want no row, got %v", err)
		}
	})
}
