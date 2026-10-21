package workersvc

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #3181: the credential-switch HOLD. A worker whose local release of a held-state switch
// cannot be proven settled reports {status:"credential_switch_held"}; the run PAUSES with
// hold_reason credential_switch_settling (claim kept, switch stamp cleared, override and worker kept,
// nothing banked) and the owner's Resume / set-token are refused while that worker is alive, has not
// re-registered since the hold and has not acknowledged with {status:"credential_switch_hold_settled"}.
// These tests execute the SQL against a real Postgres (skipped unless UZI_TEST_DATABASE_URL is set,
// run via ./e2e/run-store-it.sh).

var holdIIDSeq int64 = 700000

func nextHoldIID() int64 { return atomic.AddInt64(&holdIIDSeq, 1) }

type holdFx struct {
	env codexTestEnv
	svc *Service
	o   reevalOwner
	gen int64
}

// newHoldFx seeds an owner whose worker advertises credential_switch, heartbeats now, and whose
// registration floor is an hour back (so a hold written now sits well after it).
func newHoldFx(t *testing.T) holdFx {
	t.Helper()
	env := setupCodexLiveDB(t)
	o := seedReevalOwner(t, env, BindModeAuto, false)
	env.exec(`UPDATE workers SET protocol_capabilities = $2, last_heartbeat_at = now(),
	             dind_register_floor = now() - interval '1 hour', snapshot_register_nonce = 'nonce-hold', snapshot_epoch = 0
	          WHERE id = $1`, o.workerID, []string{capability.CredentialSwitchV1})
	return holdFx{env: env, svc: fenceSvc(env), o: o, gen: 7}
}

func (f holdFx) worker(t *testing.T, id uuid.UUID) store.Worker {
	t.Helper()
	return wkrRow(t, f.env, id)
}

// run seeds a running, switch-stamped run at f.gen carrying a pinned override on the dead token.
func (f holdFx) run(t *testing.T) uuid.UUID {
	t.Helper()
	id := seedHeldRun(t, f.env, f.o, nextHoldIID(), "running", f.gen, true, false)
	f.env.exec(`UPDATE runs SET credential_override_mode = $2, credential_override_secret_id = $3, session_id = 'sess-1' WHERE id = $1`,
		id, CredentialOverrideModePinned, f.o.deadTok)
	return id
}

func (f holdFx) report(t *testing.T, workerID, run uuid.UUID, state string, gen int64) (store.Run, bool, error) {
	t.Helper()
	g := gen
	return f.svc.SetState(f.env.ctx, f.worker(t, workerID), run, StateRequest{State: state, ClaimGeneration: &g})
}

// hold reports credential_switch_held from the owner's worker at f.gen and requires it to apply.
func (f holdFx) hold(t *testing.T, run uuid.UUID) store.Run {
	t.Helper()
	got, applied, err := f.report(t, f.o.workerID, run, "credential_switch_held", f.gen)
	if err != nil || !applied {
		t.Fatalf("credential_switch_held: applied=%v err=%v, want applied", applied, err)
	}
	return got
}

func (f holdFx) resumeParams(run uuid.UUID) store.ResumePausedRunParams {
	return store.ResumePausedRunParams{
		ID: run, UserID: f.o.userID, AllowCompletionBlockedHold: false,
		GlobalTimeoutSeconds: 7200,
		StaleCutoff:          pgconv.Time(time.Now().Add(-testParams().WorkerHeartbeatStale)),
	}
}

// ownerResume is the owner's Resume (the exact query the handler runs).
func (f holdFx) ownerResume(run uuid.UUID) error {
	_, err := f.env.q.ResumePausedRun(f.env.ctx, f.resumeParams(run))
	return err
}

func (f holdFx) mustRefuseResume(t *testing.T, run uuid.UUID, why string) {
	t.Helper()
	if err := f.ownerResume(run); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("%s: ResumePausedRun err = %v, want pgx.ErrNoRows (refused)", why, err)
	}
	if got := statusOf(t, f.env, run); got != "paused" {
		t.Fatalf("%s: refused resume left status %q, want paused", why, got)
	}
}

func (f holdFx) mustRefuseSetToken(t *testing.T, run uuid.UUID, why string) {
	t.Helper()
	_, err := f.svc.SetRunCredential(f.env.ctx, f.o.userID, run, CredentialOverrideModePinned, &f.o.altTok)
	if !errors.Is(err, ErrRunHoldSettling) {
		t.Fatalf("%s: set-token err = %v, want ErrRunHoldSettling", why, err)
	}
	r := mustRun(t, f.env, run)
	if !r.CredentialOverrideSecretID.Valid || uuid.UUID(r.CredentialOverrideSecretID.Bytes) != f.o.deadTok {
		t.Fatalf("%s: a refused set-token changed the override to %+v, want it untouched on the dead token", why, r.CredentialOverrideSecretID)
	}
	if r.Status != "paused" {
		t.Fatalf("%s: refused set-token left status %q, want paused", why, r.Status)
	}
}

func holdReasonOf(t *testing.T, env codexTestEnv, id uuid.UUID) string {
	t.Helper()
	var hr *string
	if err := env.pool.QueryRow(env.ctx, `SELECT hold_reason FROM runs WHERE id = $1`, id).Scan(&hr); err != nil {
		t.Fatalf("read hold_reason: %v", err)
	}
	if hr == nil {
		return ""
	}
	return *hr
}

func statusSinceOf(t *testing.T, env codexTestEnv, id uuid.UUID) time.Time {
	t.Helper()
	var ts time.Time
	if err := env.pool.QueryRow(env.ctx, `SELECT status_since FROM runs WHERE id = $1`, id).Scan(&ts); err != nil {
		t.Fatalf("read status_since: %v", err)
	}
	return ts
}

func reregister(t *testing.T, env codexTestEnv, workerID uuid.UUID) {
	t.Helper()
	if _, err := env.q.RegisterWorker(env.ctx, store.RegisterWorkerParams{
		ID: workerID, ProtocolCapabilities: []string{capability.CredentialSwitchV1},
		SnapshotRegisterNonce: pgconv.TextOrNull(uuid.NewString()),
	}); err != nil {
		t.Fatalf("RegisterWorker: %v", err)
	}
}

// TestCredentialSwitchHoldEffectsAndReplayLiveDB: the hold applies exactly once and leaves the
// shape the rest of the design relies on - paused/settling, claim kept (unreleased, same worker and
// generation), session and override kept, the switch stamp cleared, NO budget banked, status_since
// moved to the hold instant - and a replay of the same report is an idempotent applied that changes
// nothing (not even status_since).
func TestCredentialSwitchHoldEffectsAndReplayLiveDB(t *testing.T) {
	f := newHoldFx(t)
	id := f.run(t)
	before := mustRun(t, f.env, id)

	got := f.hold(t, id)
	if got.Status != "paused" || got.HoldReason.String != "credential_switch_settling" {
		t.Fatalf("after hold: status=%q hold_reason=%q, want paused/credential_switch_settling", got.Status, got.HoldReason.String)
	}
	if got.CredentialSwitchRequestedAt.Valid || got.CredentialSwitchGeneration.Valid {
		t.Fatalf("switch stamp not cleared: (%v, %v)", got.CredentialSwitchRequestedAt, got.CredentialSwitchGeneration)
	}
	if got.ClaimReleasedAt.Valid || got.ClaimGeneration != f.gen || !got.WorkerID.Valid || uuid.UUID(got.WorkerID.Bytes) != f.o.workerID {
		t.Fatalf("claim not kept: released=%v gen=%d worker=%v", got.ClaimReleasedAt, got.ClaimGeneration, got.WorkerID)
	}
	if got.CredentialOverrideMode.String != CredentialOverrideModePinned || !got.CredentialOverrideSecretID.Valid ||
		uuid.UUID(got.CredentialOverrideSecretID.Bytes) != f.o.deadTok {
		t.Fatalf("override not kept: mode=%q id=%v", got.CredentialOverrideMode.String, got.CredentialOverrideSecretID)
	}
	if got.SessionID.String != "sess-1" {
		t.Fatalf("session_id = %q, want kept", got.SessionID.String)
	}
	if got.BudgetPausedSeconds != before.BudgetPausedSeconds {
		t.Fatalf("budget_paused_seconds %d -> %d: the hold must not bank (the resume banks the gap once)", before.BudgetPausedSeconds, got.BudgetPausedSeconds)
	}
	if !got.StatusSince.Time.After(before.StatusSince.Time) {
		t.Fatalf("status_since %v did not move to the hold instant (was %v)", got.StatusSince.Time, before.StatusSince.Time)
	}

	since := statusSinceOf(t, f.env, id)
	again, applied, err := f.report(t, f.o.workerID, id, "credential_switch_held", f.gen)
	if err != nil || !applied {
		t.Fatalf("replayed hold: applied=%v err=%v, want idempotent applied", applied, err)
	}
	if again.Status != "paused" || again.HoldReason.String != "credential_switch_settling" {
		t.Fatalf("replay state = %q/%q, want unchanged paused/settling", again.Status, again.HoldReason.String)
	}
	if !statusSinceOf(t, f.env, id).Equal(since) {
		t.Fatal("a replayed hold rewrote status_since")
	}

	// A replay after the worker settled it is still an idempotent applied (lost-ack redelivery).
	if _, applied, err := f.report(t, f.o.workerID, id, "credential_switch_hold_settled", f.gen); err != nil || !applied {
		t.Fatalf("settle: applied=%v err=%v", applied, err)
	}
	if _, applied, err := f.report(t, f.o.workerID, id, "credential_switch_held", f.gen); err != nil || !applied {
		t.Fatalf("hold replay after settle: applied=%v err=%v, want idempotent applied", applied, err)
	}
	if got := holdReasonOf(t, f.env, id); got != "credential_switch_unsettled" {
		t.Fatalf("hold replay after settle regressed hold_reason to %q, want it to stay unsettled", got)
	}
}

// TestCredentialSwitchHoldFencesLiveDB: every conjunct of the hold's WHERE refuses a report, each
// leaving the run exactly as it was.
func TestCredentialSwitchHoldFencesLiveDB(t *testing.T) {
	f := newHoldFx(t)

	refused := func(t *testing.T, id uuid.UUID, workerID uuid.UUID, gen int64, wantErr error) {
		t.Helper()
		before := mustRun(t, f.env, id)
		_, applied, err := f.report(t, workerID, id, "credential_switch_held", gen)
		if wantErr != nil {
			if !errors.Is(err, wantErr) {
				t.Fatalf("err = %v, want %v", err, wantErr)
			}
		} else if err != nil {
			t.Fatalf("err = %v, want a clean not-applied", err)
		}
		if applied {
			t.Fatal("a fenced-out hold report was applied")
		}
		after := mustRun(t, f.env, id)
		if after.Status != before.Status || after.HoldReason.Valid || !after.StatusSince.Time.Equal(before.StatusSince.Time) ||
			after.CredentialSwitchRequestedAt.Valid != before.CredentialSwitchRequestedAt.Valid {
			t.Fatalf("a refused hold changed the run: status %q->%q hold=%v stamp %v->%v", before.Status, after.Status, after.HoldReason, before.CredentialSwitchRequestedAt.Valid, after.CredentialSwitchRequestedAt.Valid)
		}
	}

	t.Run("wrong worker", func(t *testing.T) {
		id := f.run(t)
		other := seedOutageWorker(t, f.env, f.o.userID, 0)
		refused(t, id, other, f.gen, ErrRunNotOwned)
	})
	t.Run("wrong generation", func(t *testing.T) {
		id := f.run(t)
		refused(t, id, f.o.workerID, f.gen+1, nil)
	})
	t.Run("released claim", func(t *testing.T) {
		id := seedHeldRun(t, f.env, f.o, nextHoldIID(), "running", f.gen, true, true)
		refused(t, id, f.o.workerID, f.gen, nil)
	})
	t.Run("not running", func(t *testing.T) {
		id := seedHeldRun(t, f.env, f.o, nextHoldIID(), "awaiting_input", f.gen, true, false)
		refused(t, id, f.o.workerID, f.gen, nil)
	})
	t.Run("no switch stamp", func(t *testing.T) {
		id := seedHeldRun(t, f.env, f.o, nextHoldIID(), "running", f.gen, false, false)
		refused(t, id, f.o.workerID, f.gen, nil)
	})
	t.Run("stamp at another generation", func(t *testing.T) {
		id := f.run(t)
		f.env.exec(`UPDATE runs SET credential_switch_generation = $2 WHERE id = $1`, id, f.gen-1)
		refused(t, id, f.o.workerID, f.gen, nil)
	})
	t.Run("missing generation", func(t *testing.T) {
		id := f.run(t)
		_, _, err := f.svc.SetState(f.env.ctx, f.worker(t, f.o.workerID), id, StateRequest{State: "credential_switch_held"})
		if err == nil {
			t.Fatal("a hold without claim_generation was accepted")
		}
		if got := statusOf(t, f.env, id); got != "running" {
			t.Fatalf("status = %q, want running", got)
		}
	})

	// job and cross_check runs never hold. The service refuses a job's park outright and a
	// cross_check's report is not a lead flight, so the kind conjunct is exercised on the query.
	t.Run("job kind", func(t *testing.T) {
		id := uuid.New()
		f.env.exec(`INSERT INTO runs(id,user_id,kind,job_type,issue_title,issue_description,status,worker_id,claim_generation,started_at,
		               credential_switch_requested_at,credential_switch_generation)
		            VALUES($1,$2,'job','research','j','d','running',$3,$4,now(),now(),$4)`, id, f.o.userID, f.o.workerID, f.gen)
		rows, err := f.env.q.HoldCredentialSwitchUnsettled(f.env.ctx, store.HoldCredentialSwitchUnsettledParams{ID: id, WorkerID: pgconv.UUID(f.o.workerID), Generation: f.gen})
		if err != nil || rows != 0 {
			t.Fatalf("job hold: rows=%d err=%v, want 0 rows", rows, err)
		}
		if got := statusOf(t, f.env, id); got != "running" {
			t.Fatalf("job status = %q, want running", got)
		}
	})
	t.Run("cross_check kind", func(t *testing.T) {
		lead := f.run(t)
		id := uuid.New()
		f.env.exec(`INSERT INTO runs(id,user_id,repo_id,kind,target_run_id,harness,report_only,budget_wall_seconds,issue_title,issue_description,status,worker_id,claim_generation,started_at,
		               credential_switch_requested_at,credential_switch_generation)
		            VALUES($1,$2,$3,'cross_check',$4,'codex',true,600,'c','d','running',$5,$6,now(),now(),$6)`, id, f.o.userID, f.o.repoID, lead, f.o.workerID, f.gen)
		rows, err := f.env.q.HoldCredentialSwitchUnsettled(f.env.ctx, store.HoldCredentialSwitchUnsettledParams{ID: id, WorkerID: pgconv.UUID(f.o.workerID), Generation: f.gen})
		if err != nil || rows != 0 {
			t.Fatalf("cross_check hold: rows=%d err=%v, want 0 rows", rows, err)
		}
		if got := statusOf(t, f.env, id); got != "running" {
			t.Fatalf("cross_check status = %q, want running", got)
		}
	})
}

// TestCredentialSwitchHoldNoSnapshotRowRefusesOwnerLiveDB: the hold is reported while
// worker_active_runs has NO row for the run (before the worker's first matching snapshot). The
// owner's Resume and set-token are refused anyway - the fence is the hold row plus the worker's
// liveness, not a snapshot - and set-token writes nothing.
func TestCredentialSwitchHoldNoSnapshotRowRefusesOwnerLiveDB(t *testing.T) {
	f := newHoldFx(t)
	id := f.run(t)
	if n := countActiveRuns(t, f.env, f.o.workerID); n != 0 {
		t.Fatalf("precondition: %d worker_active_runs rows, want none", n)
	}
	f.hold(t, id)
	f.mustRefuseResume(t, id, "no snapshot row")
	f.mustRefuseSetToken(t, id, "no snapshot row")
	if got := holdReasonOf(t, f.env, id); got != "credential_switch_settling" {
		t.Fatalf("hold_reason = %q after refusals, want settling", got)
	}
}

// TestCredentialSwitchHoldPreHoldSnapshotLiveDB: a snapshot captured BEFORE the hold (it still lists
// the run, or omits it) is applied AFTER the hold with an advancing epoch. Neither the replacement of
// worker_active_runs nor the missing-from-snapshot reconcile (run with the clock far past the missing
// window) moves the paused hold, and Resume and set-token stay refused.
func TestCredentialSwitchHoldPreHoldSnapshotLiveDB(t *testing.T) {
	f := newHoldFx(t)
	id := f.run(t)
	f.hold(t, id)
	since := statusSinceOf(t, f.env, id)

	late := New(f.env.q, f.env.box, testParams())
	late.SetTxBeginner(f.env.pool)
	skew := time.Now().Add(10 * time.Minute)
	late.now = func() time.Time { return skew }

	// Pre-hold snapshot 1 listed the run; the post-hold epoch-2 snapshot (captured before the hold
	// reached the worker's loop) omits it.
	snap1 := &ActiveSnapshot{SnapshotEpoch: 1, RegisterNonce: "nonce-hold", Active: []ActiveRunEntry{entry(id, f.gen, "running", false)}}
	if _, err := late.Heartbeat(f.env.ctx, f.worker(t, f.o.workerID), nil, nil, snap1); err != nil {
		t.Fatalf("heartbeat snapshot 1: %v", err)
	}
	snap2 := &ActiveSnapshot{SnapshotEpoch: 2, RegisterNonce: "nonce-hold", Active: []ActiveRunEntry{}}
	if _, err := late.Heartbeat(f.env.ctx, f.worker(t, f.o.workerID), nil, nil, snap2); err != nil {
		t.Fatalf("heartbeat snapshot 2: %v", err)
	}
	if got := statusOf(t, f.env, id); got != "paused" {
		t.Fatalf("status = %q after a pre-hold snapshot, want paused", got)
	}
	if !statusSinceOf(t, f.env, id).Equal(since) {
		t.Fatal("a snapshot rewrote status_since on the hold")
	}
	f.mustRefuseResume(t, id, "pre-hold snapshot applied after the hold")
	f.mustRefuseSetToken(t, id, "pre-hold snapshot applied after the hold")
}

// TestCredentialSwitchHoldAffinityClaimLiveDB: with updated_at backdated past the affinity ceiling and
// the holding worker's heartbeat fresh, a second live worker's ClaimRun does not admit the held run -
// before a refused Resume and after it. Once the worker settles and the owner resumes, only the
// holding worker's ClaimRun admits it; a sibling inside the affinity window does not.
func TestCredentialSwitchHoldAffinityClaimLiveDB(t *testing.T) {
	f := newHoldFx(t)
	sibling := seedOutageWorker(t, f.env, f.o.userID, 0)
	id := f.run(t)
	f.hold(t, id)
	f.env.exec(`UPDATE runs SET updated_at = now() - interval '3 hours' WHERE id = $1`, id)

	mustNotClaim := func(t *testing.T, workerID uuid.UUID, why string) {
		t.Helper()
		if got, err := f.env.q.ClaimRun(f.env.ctx, claimRunParams(f.worker(t, workerID))); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("%s: ClaimRun err = %v run=%v, want pgx.ErrNoRows", why, err, got.ID)
		}
		if got := statusOf(t, f.env, id); got != "paused" {
			t.Fatalf("%s: status = %q, want paused", why, got)
		}
	}
	mustNotClaim(t, sibling, "sibling before a refused resume")
	f.mustRefuseResume(t, id, "settling hold")
	mustNotClaim(t, sibling, "sibling after a refused resume")
	mustNotClaim(t, f.o.workerID, "holder after a refused resume")

	if _, applied, err := f.report(t, f.o.workerID, id, "credential_switch_hold_settled", f.gen); err != nil || !applied {
		t.Fatalf("settle: applied=%v err=%v", applied, err)
	}
	if err := f.ownerResume(id); err != nil {
		t.Fatalf("resume after the acknowledgement: %v", err)
	}
	if got := statusOf(t, f.env, id); got != "queued" {
		t.Fatalf("status = %q after resume, want queued", got)
	}
	if got, err := f.env.q.ClaimRun(f.env.ctx, claimRunParams(f.worker(t, sibling))); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("sibling inside the affinity window: ClaimRun err = %v run=%v, want pgx.ErrNoRows", err, got.ID)
	}
	got, err := f.env.q.ClaimRun(f.env.ctx, claimRunParams(f.worker(t, f.o.workerID)))
	if err != nil || got.ID != id {
		t.Fatalf("holder ClaimRun = (%v, %v), want the resumed run", got.ID, err)
	}
	if uuid.UUID(got.CredentialOverrideSecretID.Bytes) != f.o.deadTok {
		t.Fatalf("override after the same-worker reclaim = %v, want kept", got.CredentialOverrideSecretID)
	}
}

// TestCredentialSwitchHoldSettleLiveDB: the acknowledgement's fences and its effect.
func TestCredentialSwitchHoldSettleLiveDB(t *testing.T) {
	f := newHoldFx(t)

	t.Run("wrong worker", func(t *testing.T) {
		id := f.run(t)
		f.hold(t, id)
		other := seedOutageWorker(t, f.env, f.o.userID, 0)
		if _, applied, err := f.report(t, other, id, "credential_switch_hold_settled", f.gen); !errors.Is(err, ErrRunNotOwned) || applied {
			t.Fatalf("foreign settle: applied=%v err=%v, want ErrRunNotOwned", applied, err)
		}
		if got := holdReasonOf(t, f.env, id); got != "credential_switch_settling" {
			t.Fatalf("hold_reason = %q, want settling", got)
		}
	})
	t.Run("wrong generation", func(t *testing.T) {
		id := f.run(t)
		f.hold(t, id)
		if _, applied, err := f.report(t, f.o.workerID, id, "credential_switch_hold_settled", f.gen+1); err != nil || applied {
			t.Fatalf("settle at the wrong generation: applied=%v err=%v, want a clean not-applied", applied, err)
		}
		if got := holdReasonOf(t, f.env, id); got != "credential_switch_settling" {
			t.Fatalf("hold_reason = %q, want settling", got)
		}
	})
	t.Run("not holding", func(t *testing.T) {
		id := f.run(t)
		if _, applied, err := f.report(t, f.o.workerID, id, "credential_switch_hold_settled", f.gen); err != nil || applied {
			t.Fatalf("settle on a running run: applied=%v err=%v, want a clean not-applied", applied, err)
		}
		if got := statusOf(t, f.env, id); got != "running" {
			t.Fatalf("status = %q, want running", got)
		}
	})
	t.Run("after re-registration", func(t *testing.T) {
		id := f.run(t)
		f.hold(t, id)
		since := statusSinceOf(t, f.env, id)
		reregister(t, f.env, f.o.workerID)
		if _, applied, err := f.report(t, f.o.workerID, id, "credential_switch_hold_settled", f.gen); err != nil || applied {
			t.Fatalf("settle by a new incarnation: applied=%v err=%v, want a clean not-applied", applied, err)
		}
		if got := holdReasonOf(t, f.env, id); got != "credential_switch_settling" {
			t.Fatalf("hold_reason = %q, want settling (the new incarnation proves nothing)", got)
		}
		if !statusSinceOf(t, f.env, id).Equal(since) {
			t.Fatal("re-registration rewrote status_since")
		}
		// restore the fixture worker's floor for the sub-tests that follow
		f.env.exec(`UPDATE workers SET dind_register_floor = now() - interval '1 hour' WHERE id = $1`, f.o.workerID)
	})
	t.Run("after cancel", func(t *testing.T) {
		id := f.run(t)
		f.hold(t, id)
		if n, err := f.env.q.CancelRunServerSide(f.env.ctx, store.CancelRunServerSideParams{ID: id, UserID: f.o.userID}); err != nil || n != 1 {
			t.Fatalf("cancel: rows=%d err=%v", n, err)
		}
		if _, applied, err := f.report(t, f.o.workerID, id, "credential_switch_hold_settled", f.gen); err != nil || applied {
			t.Fatalf("settle on a cancelled run: applied=%v err=%v, want a clean not-applied", applied, err)
		}
		if got := statusOf(t, f.env, id); got != "cancelled" {
			t.Fatalf("status = %q, want cancelled", got)
		}
	})
	t.Run("genuine acknowledgement opens resume", func(t *testing.T) {
		id := f.run(t)
		f.hold(t, id)
		since := statusSinceOf(t, f.env, id)
		got, applied, err := f.report(t, f.o.workerID, id, "credential_switch_hold_settled", f.gen)
		if err != nil || !applied {
			t.Fatalf("settle: applied=%v err=%v", applied, err)
		}
		if got.Status != "paused" || got.HoldReason.String != "credential_switch_unsettled" {
			t.Fatalf("after settle: %q/%q, want paused/credential_switch_unsettled", got.Status, got.HoldReason.String)
		}
		if !statusSinceOf(t, f.env, id).Equal(since) {
			t.Fatal("the acknowledgement rewrote status_since")
		}
		// Replay is an idempotent applied.
		if _, applied, err := f.report(t, f.o.workerID, id, "credential_switch_hold_settled", f.gen); err != nil || !applied {
			t.Fatalf("settle replay: applied=%v err=%v, want idempotent applied", applied, err)
		}
		// set-token now resumes the run on the new token.
		res, err := f.svc.SetRunCredential(f.env.ctx, f.o.userID, id, CredentialOverrideModePinned, &f.o.altTok)
		if err != nil {
			t.Fatalf("set-token on an unsettled hold: %v", err)
		}
		if res.Run.Status != "queued" || !res.Run.CredentialOverrideSecretID.Valid || uuid.UUID(res.Run.CredentialOverrideSecretID.Bytes) != f.o.altTok {
			t.Fatalf("set-token result status=%q override=%v, want queued on the new token", res.Run.Status, res.Run.CredentialOverrideSecretID)
		}
		if res.Run.HoldReason.Valid {
			t.Fatalf("hold_reason = %q after resume, want cleared", res.Run.HoldReason.String)
		}
	})
}

// TestCredentialSwitchHoldWorkerLossLiftsFenceLiveDB: the fence exists only while the holding worker
// is alive. A stale heartbeat, a deleted worker row and a re-registration each lift it, after which
// the owner's Resume succeeds and clears the hold reason.
func TestCredentialSwitchHoldWorkerLossLiftsFenceLiveDB(t *testing.T) {
	f := newHoldFx(t)

	t.Run("stale heartbeat", func(t *testing.T) {
		id := f.run(t)
		f.hold(t, id)
		f.mustRefuseResume(t, id, "fresh heartbeat")
		f.env.exec(`UPDATE workers SET last_heartbeat_at = now() - interval '5 minutes' WHERE id = $1`, f.o.workerID)
		if err := f.ownerResume(id); err != nil {
			t.Fatalf("resume with a stale holder: %v", err)
		}
		if r := mustRun(t, f.env, id); r.Status != "queued" || r.HoldReason.Valid {
			t.Fatalf("after resume: %q hold=%v, want queued with the hold cleared", r.Status, r.HoldReason)
		}
		f.env.exec(`UPDATE workers SET last_heartbeat_at = now() WHERE id = $1`, f.o.workerID)
	})
	t.Run("worker row deleted", func(t *testing.T) {
		o := seedReevalOwner(t, f.env, BindModeAuto, false)
		f.env.exec(`UPDATE workers SET protocol_capabilities = $2, last_heartbeat_at = now(), dind_register_floor = now() - interval '1 hour' WHERE id = $1`,
			o.workerID, []string{capability.CredentialSwitchV1})
		g := holdFx{env: f.env, svc: f.svc, o: o, gen: 7}
		id := g.run(t)
		g.hold(t, id)
		g.mustRefuseResume(t, id, "worker row present")
		f.env.exec(`DELETE FROM workers WHERE id = $1`, o.workerID) // runs.worker_id is ON DELETE SET NULL
		if err := g.ownerResume(id); err != nil {
			t.Fatalf("resume after the worker row was deleted: %v", err)
		}
		if r := mustRun(t, f.env, id); r.Status != "queued" || r.HoldReason.Valid {
			t.Fatalf("after resume: %q hold=%v, want queued with the hold cleared", r.Status, r.HoldReason)
		}
	})
	t.Run("re-registration", func(t *testing.T) {
		id := f.run(t)
		f.hold(t, id)
		f.mustRefuseResume(t, id, "same incarnation")
		since := statusSinceOf(t, f.env, id)
		reregister(t, f.env, f.o.workerID)
		if !statusSinceOf(t, f.env, id).Equal(since) {
			t.Fatal("re-registration rewrote status_since")
		}
		if err := f.ownerResume(id); err != nil {
			t.Fatalf("resume after re-registration: %v", err)
		}
		if r := mustRun(t, f.env, id); r.Status != "queued" || r.HoldReason.Valid {
			t.Fatalf("after resume: %q hold=%v, want queued with the hold cleared", r.Status, r.HoldReason)
		}
	})
}

// TestCredentialSwitchHoldCancelLiveDB: Cancel is never fenced - a settling hold cancels.
func TestCredentialSwitchHoldCancelLiveDB(t *testing.T) {
	f := newHoldFx(t)
	id := f.run(t)
	f.hold(t, id)
	f.mustRefuseResume(t, id, "settling hold")
	if n, err := f.env.q.CancelRunServerSide(f.env.ctx, store.CancelRunServerSideParams{ID: id, UserID: f.o.userID}); err != nil || n != 1 {
		t.Fatalf("cancel of a settling hold: rows=%d err=%v, want 1", n, err)
	}
	if got := statusOf(t, f.env, id); got != "cancelled" {
		t.Fatalf("status = %q, want cancelled", got)
	}
}

// TestCredentialSwitchHoldNothingMovesStatusSinceLiveDB pins the incarnation fence's premise: after
// the hold report nothing but the owner's Resume/Cancel moves status_since or the hold. A late
// report of every other worker state, the periodic sweeper (run with the clock far enough ahead that
// the holder looks stale and every missing-run window has elapsed), and a re-registration (which only
// lifts the fence) all leave status, hold_reason and status_since untouched.
func TestCredentialSwitchHoldNothingMovesStatusSinceLiveDB(t *testing.T) {
	f := newHoldFx(t)
	id := f.run(t)
	f.hold(t, id)
	since := statusSinceOf(t, f.env, id)
	assertUntouched := func(t *testing.T, step string, wantHold string) {
		t.Helper()
		r := mustRun(t, f.env, id)
		if r.Status != "paused" || r.HoldReason.String != wantHold || !r.StatusSince.Time.Equal(since) {
			t.Fatalf("%s: status=%q hold=%q status_since=%v, want paused/%s/%v", step, r.Status, r.HoldReason.String, r.StatusSince.Time, wantHold, since)
		}
	}

	// Late worker reports of every other state at the held generation: none may move the hold.
	for _, st := range []string{"running", "awaiting_approval", "awaiting_input", "awaiting_followup", "paused", "limit_wait", "recovery_wait", "failed", "completed"} {
		_, _, _ = f.report(t, f.o.workerID, id, st, f.gen) // applied/err vary by arm; the invariant is the row
		assertUntouched(t, "late "+st+" report", "credential_switch_settling")
	}
	// The park statements themselves (the service arms can refuse before reaching them): a late
	// question or follow-up park from the holding worker matches no row on either hold.
	directPark := func(step, wantHold string) {
		t.Helper()
		wid := pgconv.UUID(f.o.workerID)
		if n, err := f.env.q.SetRunAwaitingInput(f.env.ctx, store.SetRunAwaitingInputParams{
			OpenQuestionID: pgconv.TextOrNull("q-1"), ID: id, WorkerID: wid,
		}); err != nil || n != 0 {
			t.Fatalf("%s: SetRunAwaitingInput rows=%d err=%v, want 0 rows", step, n, err)
		}
		if n, err := f.env.q.SetRunAwaitingFollowup(f.env.ctx, store.SetRunAwaitingFollowupParams{
			OpenFollowupID: pgtype.Int8{Int64: 1, Valid: true}, ID: id, WorkerID: wid,
		}); err != nil || n != 0 {
			t.Fatalf("%s: SetRunAwaitingFollowup rows=%d err=%v, want 0 rows", step, n, err)
		}
		assertUntouched(t, step, wantHold)
	}
	directPark("direct park statements on the settling hold", "credential_switch_settling")
	// A pause_failed / credential_switch_failed redelivery withdraws nothing here.
	_, _, _ = f.report(t, f.o.workerID, id, "pause_failed", f.gen)
	_, _, _ = f.report(t, f.o.workerID, id, "credential_switch_failed", f.gen)
	_, _, _ = f.report(t, f.o.workerID, id, "credential_switch", f.gen)
	assertUntouched(t, "withdrawal/release redelivery", "credential_switch_settling")

	// The sweeper with a skewed clock (holder stale, missing windows elapsed).
	sw := New(f.env.q, f.env.box, testParams())
	sw.SetTxBeginner(f.env.pool)
	skew := time.Now().Add(3 * time.Hour)
	sw.now = func() time.Time { return skew }
	if _, err := sw.Sweep(f.env.ctx); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	assertUntouched(t, "sweeper", "credential_switch_settling")

	// The unsettled hold is untouched by the same reports and sweeper.
	if _, applied, err := f.report(t, f.o.workerID, id, "credential_switch_hold_settled", f.gen); err != nil || !applied {
		t.Fatalf("settle: applied=%v err=%v", applied, err)
	}
	for _, st := range []string{"running", "awaiting_input", "awaiting_followup"} {
		_, _, _ = f.report(t, f.o.workerID, id, st, f.gen)
		assertUntouched(t, "late "+st+" report on the unsettled hold", "credential_switch_unsettled")
	}
	if _, err := sw.Sweep(f.env.ctx); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	assertUntouched(t, "sweeper on the unsettled hold", "credential_switch_unsettled")
	directPark("direct park statements on the unsettled hold", "credential_switch_unsettled")
}

// TestCredentialSwitchHoldReregistrationOnlyLiftsFenceLiveDB: re-registration lifts the settling
// fence without touching the row, and the fence stays lifted (a later settle is refused, a later
// Resume is the only mover).
func TestCredentialSwitchHoldReregistrationOnlyLiftsFenceLiveDB(t *testing.T) {
	f := newHoldFx(t)
	id := f.run(t)
	f.hold(t, id)
	since := statusSinceOf(t, f.env, id)
	if settling, err := f.svc.runHoldSettling(f.env.ctx, id); err != nil || !settling {
		t.Fatalf("fence before re-registration = (%v, %v), want up", settling, err)
	}
	reregister(t, f.env, f.o.workerID)
	if settling, err := f.svc.runHoldSettling(f.env.ctx, id); err != nil || settling {
		t.Fatalf("fence after re-registration = (%v, %v), want lifted", settling, err)
	}
	if r := mustRun(t, f.env, id); r.Status != "paused" || r.HoldReason.String != "credential_switch_settling" || !r.StatusSince.Time.Equal(since) {
		t.Fatalf("re-registration touched the run: %q/%q status_since=%v (was %v)", r.Status, r.HoldReason.String, r.StatusSince.Time, since)
	}
	if err := f.ownerResume(id); err != nil {
		t.Fatalf("resume after re-registration: %v", err)
	}
	if r := mustRun(t, f.env, id); r.Status != "queued" || r.HoldReason.Valid {
		t.Fatalf("after resume: %q hold=%v, want queued, hold cleared", r.Status, r.HoldReason)
	}
}

// TestCredentialSwitchHoldResumeBanksOnceLiveDB: the resume after the acknowledgement keeps worker_id,
// banks the paused gap exactly once, clears the hold reason and keeps the override; the holder's
// reclaim then runs the credential-epoch write unharmed.
func TestCredentialSwitchHoldResumeBanksOnceLiveDB(t *testing.T) {
	f := newHoldFx(t)
	id := f.run(t)
	f.hold(t, id)
	// A measurable paused gap that stays after the registration floor.
	f.env.exec(`UPDATE runs SET status_since = now() - interval '90 seconds' WHERE id = $1`, id)
	if _, applied, err := f.report(t, f.o.workerID, id, "credential_switch_hold_settled", f.gen); err != nil || !applied {
		t.Fatalf("settle: applied=%v err=%v", applied, err)
	}
	banked0 := mustRun(t, f.env, id).BudgetPausedSeconds
	if err := f.ownerResume(id); err != nil {
		t.Fatalf("resume: %v", err)
	}
	r := mustRun(t, f.env, id)
	if r.Status != "queued" || r.HoldReason.Valid {
		t.Fatalf("after resume: %q hold=%v, want queued with the hold cleared", r.Status, r.HoldReason)
	}
	if !r.WorkerID.Valid || uuid.UUID(r.WorkerID.Bytes) != f.o.workerID || r.ClaimReleasedAt.Valid {
		t.Fatalf("resume lost the same-worker claim: worker=%v released=%v", r.WorkerID, r.ClaimReleasedAt)
	}
	if gap := r.BudgetPausedSeconds - banked0; gap < 89 || gap > 100 {
		t.Fatalf("banked %ds on resume, want the ~90s paused gap once", gap)
	}
	if r.CredentialOverrideMode.String != CredentialOverrideModePinned || uuid.UUID(r.CredentialOverrideSecretID.Bytes) != f.o.deadTok {
		t.Fatalf("override not kept: %q %v", r.CredentialOverrideMode.String, r.CredentialOverrideSecretID)
	}
	// A second resume is refused and banks nothing more.
	if err := f.ownerResume(id); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("second resume err = %v, want pgx.ErrNoRows", err)
	}
	if again := mustRun(t, f.env, id).BudgetPausedSeconds; again != r.BudgetPausedSeconds {
		t.Fatalf("budget_paused_seconds %d -> %d on a refused second resume", r.BudgetPausedSeconds, again)
	}

	// The holder reclaims; the credential-epoch write for the new generation is unaffected.
	claimed, err := f.env.q.ClaimRun(f.env.ctx, claimRunParams(f.worker(t, f.o.workerID)))
	if err != nil || claimed.ID != id {
		t.Fatalf("holder ClaimRun = (%v, %v), want the resumed run", claimed.ID, err)
	}
	if err := f.env.q.RecordRunCredentialEpoch(f.env.ctx, store.RecordRunCredentialEpochParams{
		RunID: id, ClaimGeneration: claimed.ClaimGeneration,
		SecretID:     pgtype.UUID{Bytes: f.o.deadTok, Valid: true},
		Label:        pgtype.Text{String: "dead", Valid: true},
		SelectReason: pgtype.Text{String: "pinned", Valid: true},
	}); err != nil {
		t.Fatalf("RecordRunCredentialEpoch after the hold resume: %v", err)
	}
	if after := mustRun(t, f.env, id); after.CredentialSwitchRequestedAt.Valid || after.CredentialOverrideMode.String != CredentialOverrideModePinned {
		t.Fatalf("after reclaim: stamp=%v override=%q, want no stamp and the override kept", after.CredentialSwitchRequestedAt, after.CredentialOverrideMode.String)
	}
}

// TestRegisterWorkerAdvancesDindRegisterFloorLiveDB is the precondition the incarnation fence leans
// on: a registration moves workers.dind_register_floor forward to the registration instant (and is
// the only writer that does), so a floor read after a registration is later than any hold written
// before it.
func TestRegisterWorkerAdvancesDindRegisterFloorLiveDB(t *testing.T) {
	f := newHoldFx(t)
	before := f.worker(t, f.o.workerID).DindRegisterFloor.Time
	reregister(t, f.env, f.o.workerID)
	after := f.worker(t, f.o.workerID)
	if !after.DindRegisterFloor.Time.After(before) {
		t.Fatalf("dind_register_floor %v -> %v, want it advanced by RegisterWorker", before, after.DindRegisterFloor.Time)
	}
	if !after.DindRegisterFloor.Time.Equal(after.LastHeartbeatAt.Time) {
		t.Fatalf("floor %v != registration heartbeat %v", after.DindRegisterFloor.Time, after.LastHeartbeatAt.Time)
	}
	// A heartbeat does not move it.
	if _, err := f.svc.Heartbeat(f.env.ctx, after, nil, nil, nil); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if hb := f.worker(t, f.o.workerID); !hb.DindRegisterFloor.Time.Equal(after.DindRegisterFloor.Time) {
		t.Fatalf("a heartbeat moved dind_register_floor %v -> %v", after.DindRegisterFloor.Time, hb.DindRegisterFloor.Time)
	}
}

// TestCredentialSwitchHoldRegisterBeganFirstHoldLockedFirstLiveDB models the ordering that
// transaction-start timestamps get wrong. A re-registration transaction BEGINs first, but the hold
// transaction BEGINs second and takes the worker row lock first; the registration then waits on that
// lock, so it is serialized AFTER the hold. With now() for the floor (the registration's start) it
// would precede the hold's status_since and the new incarnation would be mistaken for the hold's
// writer, leaving the fence up until the heartbeat went stale. RegisterWorker lifts the floor just
// past a settling hold committed under the lock, so after the registration commits the settling fence
// is lifted, the old incarnation's acknowledgement is refused, and the hold row itself (status_since
// included) is untouched. Without a settling hold the floor still equals the registration heartbeat
// (TestRegisterWorkerAdvancesDindRegisterFloorLiveDB and the dind maintenance tests pin that).
func TestCredentialSwitchHoldRegisterBeganFirstHoldLockedFirstLiveDB(t *testing.T) {
	f := newHoldFx(t)
	id := f.run(t)
	ctx := f.env.ctx

	regTx, err := f.env.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin register tx: %v", err)
	}
	defer func() { _ = regTx.Rollback(ctx) }()
	time.Sleep(50 * time.Millisecond) // the registration's transaction start is strictly earlier
	holdTx, err := f.env.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin hold tx: %v", err)
	}
	defer func() { _ = holdTx.Rollback(ctx) }()

	hq := store.New(holdTx)
	if _, err := hq.GetWorkerForUpdate(ctx, f.o.workerID); err != nil {
		t.Fatalf("hold tx lock worker: %v", err)
	}
	rows, err := hq.HoldCredentialSwitchUnsettled(ctx, store.HoldCredentialSwitchUnsettledParams{
		ID: id, WorkerID: pgconv.UUID(f.o.workerID), Generation: f.gen,
	})
	if err != nil || rows != 1 {
		t.Fatalf("hold in tx: rows=%d err=%v, want 1", rows, err)
	}

	regDone := make(chan error, 1)
	go func() {
		rq := store.New(regTx)
		if _, err := rq.GetWorkerForUpdate(ctx, f.o.workerID); err != nil { // blocks on the hold's lock
			regDone <- err
			return
		}
		if _, err := rq.RegisterWorker(ctx, store.RegisterWorkerParams{
			ID: f.o.workerID, ProtocolCapabilities: []string{capability.CredentialSwitchV1},
			SnapshotRegisterNonce: pgconv.TextOrNull(uuid.NewString()),
		}); err != nil {
			regDone <- err
			return
		}
		regDone <- regTx.Commit(ctx)
	}()
	select {
	case err := <-regDone:
		t.Fatalf("the registration finished while the hold transaction held the worker lock (err=%v)", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := holdTx.Commit(ctx); err != nil {
		t.Fatalf("commit hold: %v", err)
	}
	if err := <-regDone; err != nil {
		t.Fatalf("registration after the hold: %v", err)
	}

	since := statusSinceOf(t, f.env, id)
	if floor := f.worker(t, f.o.workerID).DindRegisterFloor.Time; !floor.After(since) {
		t.Fatalf("dind_register_floor %v is not after status_since %v although the registration was serialized after the hold", floor, since)
	}
	if settling, err := f.svc.runHoldSettling(ctx, id); err != nil || settling {
		t.Fatalf("fence after the serialized re-registration = (%v, %v), want lifted", settling, err)
	}
	if _, applied, err := f.report(t, f.o.workerID, id, "credential_switch_hold_settled", f.gen); err != nil || applied {
		t.Fatalf("old incarnation's acknowledgement: applied=%v err=%v, want refused", applied, err)
	}
	if r := mustRun(t, f.env, id); r.Status != "paused" || r.HoldReason.String != "credential_switch_settling" || !r.StatusSince.Time.Equal(since) {
		t.Fatalf("re-registration touched the hold: %q/%q status_since=%v (was %v)", r.Status, r.HoldReason.String, r.StatusSince.Time, since)
	}
	if err := f.ownerResume(id); err != nil {
		t.Fatalf("resume after the serialized re-registration: %v", err)
	}
}

// TestCredentialSwitchHoldNullStaleCutoffKeepsFenceClosedLiveDB: the fence fails CLOSED on an
// unknown stale cutoff. A NULL cutoff must not read as "every heartbeat is stale"; both the read
// and ResumePausedRun keep refusing a settling hold, including a worker whose heartbeat is old, and
// a non-positive WorkerHeartbeatStale window (which yields the NULL cutoff) behaves the same.
func TestCredentialSwitchHoldNullStaleCutoffKeepsFenceClosedLiveDB(t *testing.T) {
	f := newHoldFx(t)
	id := f.run(t)
	f.hold(t, id)
	ctx := f.env.ctx
	null := pgtype.Timestamptz{}

	read := func(cutoff pgtype.Timestamptz) bool {
		t.Helper()
		settling, err := f.env.q.CredentialSwitchHoldSettling(ctx, store.CredentialSwitchHoldSettlingParams{RunID: id, StaleCutoff: cutoff})
		if err != nil {
			t.Fatalf("CredentialSwitchHoldSettling: %v", err)
		}
		return settling
	}
	if !read(null) {
		t.Fatal("CredentialSwitchHoldSettling with a NULL cutoff = false, want the fence kept closed")
	}
	p := f.resumeParams(id)
	p.StaleCutoff = null
	if _, err := f.env.q.ResumePausedRun(ctx, p); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("ResumePausedRun with a NULL cutoff: err = %v, want pgx.ErrNoRows (refused)", err)
	}
	if got := statusOf(t, f.env, id); got != "paused" {
		t.Fatalf("a NULL-cutoff resume left status %q, want paused", got)
	}

	// Even a long-silent holder does not open the fence under an unknown cutoff...
	f.env.exec(`UPDATE workers SET last_heartbeat_at = now() - interval '1 day' WHERE id = $1`, f.o.workerID)
	if !read(null) {
		t.Fatal("a stale heartbeat opened the fence under a NULL cutoff")
	}
	// ...while a known cutoff still lifts it (the stale-worker lift is unchanged).
	if read(pgconv.Time(time.Now().Add(-time.Minute))) {
		t.Fatal("a known cutoff no longer lifts the fence for a stale holder")
	}

	// A non-positive configured window yields the NULL cutoff.
	f.env.exec(`UPDATE workers SET last_heartbeat_at = now() WHERE id = $1`, f.o.workerID)
	zero := testParams()
	zero.WorkerHeartbeatStale = 0
	zsvc := New(f.env.q, f.env.box, zero)
	if c := zsvc.staleWorkerCutoff(); c.Valid {
		t.Fatalf("staleWorkerCutoff with a zero window = %+v, want NULL", c)
	}
	if settling, err := zsvc.runHoldSettling(ctx, id); err != nil || !settling {
		t.Fatalf("runHoldSettling with a zero window = (%v, %v), want the fence closed", settling, err)
	}
}
