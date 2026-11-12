package workersvc

import (
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// requireRunOwnerHeld asserts err is the set-token owner-hold refusal carrying label, matches the
// ErrRunOwnerHeld sentinel and is NOT the generic raced refusal.
func requireRunOwnerHeld(t *testing.T, err error, label string) {
	t.Helper()
	var held *RunOwnerHeldError
	if !errors.As(err, &held) {
		t.Fatalf("err = %v, want *RunOwnerHeldError", err)
	}
	if held.Label != label {
		t.Fatalf("label = %q, want %q", held.Label, label)
	}
	if !errors.Is(err, ErrRunOwnerHeld) {
		t.Fatalf("err = %v does not match ErrRunOwnerHeld", err)
	}
	if errors.Is(err, ErrCredentialSwitchRaced) {
		t.Fatalf("err = %v unexpectedly matches ErrCredentialSwitchRaced", err)
	}
}

type ownerHeldFx struct {
	env    codexTestEnv
	user   uuid.UUID
	repo   uuid.UUID
	worker uuid.UUID
}

func newOwnerHeldFx(t *testing.T) ownerHeldFx {
	t.Helper()
	env := setupCodexLiveDB(t)
	user, _, repo := env.seedCodexInfra(t)
	memoryFixtureCleanup(t, env, user)
	return ownerHeldFx{env: env, user: user, repo: repo, worker: seedSnapshotWorker(t, env, user, "owner-held")}
}

// parked seeds an ordinary recovery_wait run with an open custody hold, then applies setSQL.
func (f ownerHeldFx) parked(t *testing.T, setSQL string) uuid.UUID {
	t.Helper()
	run := seedOutageRun(t, f.env, f.user, f.repo, f.worker, "recovery_wait", "issue", 2, 0)
	f.env.exec("INSERT INTO recovery_custody_holds(id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,live_worker_id,live_run_id) VALUES($1,$2,$3,$4,2,'open',$5,'owner-held',$5,$4)", uuid.New(), f.user, f.repo, run, f.worker)
	if setSQL != "" {
		f.env.exec("UPDATE runs SET "+setSQL+" WHERE id=$1", run)
	}
	return run
}

// snapshot is every observable the refusal must leave untouched.
func (f ownerHeldFx) snapshot(t *testing.T, run uuid.UUID) [3]string {
	t.Helper()
	var s [3]string
	q := func(sql string, dst *string) {
		if err := f.env.pool.QueryRow(f.env.ctx, sql, run).Scan(dst); err != nil {
			t.Fatal(err)
		}
	}
	q("SELECT row_to_json(r)::text FROM runs r WHERE id=$1", &s[0])
	q("SELECT coalesce(jsonb_agg(to_jsonb(h) ORDER BY h.id)::text,'[]') FROM recovery_custody_holds h WHERE run_id=$1", &s[1])
	q("SELECT coalesce(jsonb_agg(to_jsonb(i) ORDER BY i.id)::text,'[]') FROM run_user_inputs i WHERE i.run_id=$1", &s[2])
	return s
}

const ownerHeldPendingOutcome = `policy_outcome_id=gen_random_uuid(), policy_continuation_mode='same_conversation',
	policy_applying_worker_id=worker_id, policy_source_generation=1, policy_applying_generation=1,
	policy_preserve_required=true, policy_resolved_at=NULL`

func TestSetTokenOwnerHeldRefusalLiveDB(t *testing.T) {
	f := newOwnerHeldFx(t)
	for _, tc := range []struct{ name, set, label string }{
		{"worker_requeue_exhausted", "recovery_wait_cause='worker_requeue_exhausted'", "worker requeue exhausted"},
		{"worker_memory_pressure", "recovery_wait_cause='worker_memory_pressure'", "worker memory pressure"},
		{"retained_recovery_adoption_failed", "recovery_wait_cause='retained_recovery_adoption_failed'", "retained recovery adoption failed"},
		{"retained_continuation_unwitnessed", "recovery_wait_cause='retained_continuation_unwitnessed'", "retained continuation unwitnessed"},
		{"retained_recovery_exhausted", "recovery_wait_cause='retained_recovery_exhausted'", "retained recovery exhausted"},
		{"codex_account_unavailable", "harness='claude', recovery_wait_cause='codex_account_unavailable'", "codex account unavailable"},
		{"pending policy outcome", "harness='claude', recovery_wait_cause='provider_outage', " + ownerHeldPendingOutcome, "pending policy outcome"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := f.parked(t, tc.set)
			before := f.snapshot(t, run)
			_, err := snapshotSvc(f.env, testParams()).SetRunCredential(f.env.ctx, f.user, run, CredentialOverrideModeAuto, nil)
			requireRunOwnerHeld(t, err, tc.label)
			if after := f.snapshot(t, run); after != before {
				t.Fatalf("refused switch wrote: before=%v after=%v", before, after)
			}
		})
	}
}

func TestSetTokenPromotableRecoveryWaitControlsLiveDB(t *testing.T) {
	f := newOwnerHeldFx(t)
	for _, cause := range []string{"provider_outage", "vault_locked"} {
		t.Run(cause, func(t *testing.T) {
			run := f.parked(t, "recovery_wait_cause='"+cause+"', recovery_retry_not_before=now()+interval '1 hour'")
			if _, err := snapshotSvc(f.env, testParams()).SetRunCredential(f.env.ctx, f.user, run, CredentialOverrideModeAuto, nil); err != nil {
				t.Fatalf("SetRunCredential: %v", err)
			}
			if got := mustRun(t, f.env, run); got.Status != "queued" {
				t.Fatalf("status = %q, want queued", got.Status)
			}
		})
	}
}

func TestSetTokenPerCauseSentinelsUnchangedLiveDB(t *testing.T) {
	f := newOwnerHeldFx(t)
	for _, tc := range []struct {
		cause string
		want  error
	}{
		{"data_volume_full", ErrDataVolumeFullHeld},
		{"forge_unreachable", ErrForgeUnreachableHeld},
		{"codex_idle_stall", ErrCodexIdleStallHeld},
	} {
		t.Run(tc.cause, func(t *testing.T) {
			run := f.parked(t, "recovery_wait_cause='"+tc.cause+"', recovery_retry_not_before=NULL")
			_, err := snapshotSvc(f.env, testParams()).SetRunCredential(f.env.ctx, f.user, run, CredentialOverrideModeAuto, nil)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			var held *RunOwnerHeldError
			if errors.As(err, &held) {
				t.Fatalf("sentinel masked by RunOwnerHeldError: %v", err)
			}
		})
	}
}

// TestSetTokenOwnerHoldRaceOrderingLiveDB pins the deliberate order in setCredentialAndPromote:
// the observed-row comparison runs before the owner-hold check, so a run that moved (status_since
// restamped) is the generic race, while a cause-only change on the same observed row is the
// specific owner-hold refusal. Both write nothing beyond the hook's own change.
func TestSetTokenOwnerHoldRaceOrderingLiveDB(t *testing.T) {
	f := newOwnerHeldFx(t)
	for _, tc := range []struct {
		name string
		move string
		want string // "" = raced
	}{
		{"cause and status_since moved", "recovery_wait_cause='worker_memory_pressure', status_since=status_since+interval '1 minute'", ""},
		{"cause only", "recovery_wait_cause='worker_memory_pressure'", "worker memory pressure"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := f.parked(t, "recovery_wait_cause='provider_outage', recovery_retry_not_before=now()+interval '1 hour'")
			svc := snapshotSvc(f.env, testParams())
			var before [3]string
			svc.beforeCredentialSwitchTx = func() {
				f.env.exec("UPDATE runs SET "+tc.move+" WHERE id=$1", run)
				before = f.snapshot(t, run)
			}
			_, err := svc.SetRunCredential(f.env.ctx, f.user, run, CredentialOverrideModeAuto, nil)
			if tc.want == "" {
				if !errors.Is(err, ErrCredentialSwitchRaced) {
					t.Fatalf("err = %v, want ErrCredentialSwitchRaced", err)
				}
				var held *RunOwnerHeldError
				if errors.As(err, &held) {
					t.Fatalf("race reported as owner hold: %v", err)
				}
			} else {
				requireRunOwnerHeld(t, err, tc.want)
			}
			if after := f.snapshot(t, run); after != before {
				t.Fatalf("refused switch wrote: before=%v after=%v", before, after)
			}
		})
	}
}

// TestPolicyHoldRecoveryWaitUnreachableLiveDB pins why the codex_policy_hold branch of
// recoveryWaitOwnerHold is defensive: the schema forbids that hold on a recovery_wait run.
func TestPolicyHoldRecoveryWaitUnreachableLiveDB(t *testing.T) {
	f := newOwnerHeldFx(t)
	run := f.parked(t, "")
	_, err := f.env.pool.Exec(f.env.ctx, `UPDATE runs SET hold_reason='codex_policy_hold', `+ownerHeldPendingOutcome+` WHERE id=$1`, run)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != "runs_policy_hold_shape" {
		t.Fatalf("err = %v, want check violation runs_policy_hold_shape", err)
	}
}
