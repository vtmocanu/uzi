package workersvc

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/capability"
)

// Issue #4201 m3a: a worker whose CURRENT registration negotiated in-place apply never writes a v1
// release (executed against a real Postgres, skipped unless UZI_TEST_DATABASE_URL is set).

var applyNegotiatedCaps = []string{capability.CredentialSwitchV1, capability.CredentialSwitchInPlaceV1, capability.InputReceiptsV1}

// register runs the real Service.Register for the fixture worker and returns its nonce.
func (f applyFx) register(t *testing.T, caps []string) string {
	t.Helper()
	_, nonce, err := f.svc.Register(f.env.ctx, f.worker(t), "v-test", "", nil, nil, caps, nil)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	return nonce
}

// oldReplicaRegister is what an api replica that predates this change does: it rotates the snapshot
// nonce and never touches the apply nonce.
func (f applyFx) oldReplicaRegister(t *testing.T, nonce string) {
	t.Helper()
	f.env.exec(`UPDATE workers SET snapshot_register_nonce = $2 WHERE id = $1`, f.o.workerID, nonce)
}

func (f applyFx) applyNonce(t *testing.T) (apply, snap string) {
	t.Helper()
	w := f.worker(t)
	return w.CredentialSwitchApplyNonce.String, w.SnapshotRegisterNonce.String
}

func (f applyFx) release(t *testing.T, run uuid.UUID) (applied bool, err error) {
	t.Helper()
	_, applied, err = f.report(t, f.worker(t), run, "credential_switch", f.gen, "", nil)
	return applied, err
}

// heldV1Run is a running run with a v1 held-state switch stamp pending (the shape a v1 release acts on).
func (f applyFx) heldV1Run(t *testing.T) uuid.UUID {
	t.Helper()
	id, _ := f.run(t, "running")
	return id
}

func TestRegisterStampsApplyNonceLiveDB(t *testing.T) {
	f := newApplyFx(t)
	if apply, _ := f.applyNonce(t); apply != "" {
		t.Fatalf("fixture apply nonce = %q, want unset", apply)
	}
	nonce := f.register(t, applyNegotiatedCaps)
	if apply, snap := f.applyNonce(t); apply != nonce || snap != nonce {
		t.Fatalf("after an in-place registration apply=%q snapshot=%q, want both %q", apply, snap, nonce)
	}
	// A registration that does not advertise the capability leaves the stamp stale (never equal).
	f.register(t, []string{capability.CredentialSwitchV1})
	if apply, snap := f.applyNonce(t); apply != nonce || apply == snap {
		t.Fatalf("after a v1 registration apply=%q snapshot=%q, want a stale apply nonce %q", apply, snap, nonce)
	}
}

func TestV1ReleaseRetiredForApplyNegotiatedWorkerLiveDB(t *testing.T) {
	f := newApplyFx(t)
	nonce := f.register(t, applyNegotiatedCaps)

	t.Run("new registration then a v1 release is refused and nothing is written", func(t *testing.T) {
		run := f.heldV1Run(t)
		f.noWrite(t, run, func() {
			applied, err := f.release(t, run)
			if applied || !errors.Is(err, ErrCredentialSwitchReleaseRetired) {
				t.Fatalf("release: applied=%v err=%v, want ErrCredentialSwitchReleaseRetired", applied, err)
			}
		})
		if got := mustRun(t, f.env, run); got.ClaimReleasedAt.Valid || got.Status != "running" {
			t.Fatalf("status=%s claim_released_at=%v, want running and unreleased", got.Status, got.ClaimReleasedAt)
		}
	})
	t.Run("a release with no claim_generation is an invalid report before it is a retired one", func(t *testing.T) {
		run := f.heldV1Run(t)
		_, applied, err := f.svc.SetState(f.env.ctx, f.worker(t), run, StateRequest{State: "credential_switch"})
		if applied || !errors.Is(err, ErrInvalidState) || errors.Is(err, ErrCredentialSwitchReleaseRetired) {
			t.Fatalf("release without a generation: applied=%v err=%v, want ErrInvalidState and not ErrCredentialSwitchReleaseRetired", applied, err)
		}
	})
	t.Run("an un-fenced v1 hold is refused and nothing is written", func(t *testing.T) {
		run := f.heldV1Run(t)
		f.noWrite(t, run, func() {
			_, applied, err := f.report(t, f.worker(t), run, "credential_switch_held", f.gen, nonce, nil)
			if applied || !errors.Is(err, ErrCredentialSwitchReleaseRetired) {
				t.Fatalf("v1 hold: applied=%v err=%v, want ErrCredentialSwitchReleaseRetired", applied, err)
			}
		})
	})
	t.Run("an op-fenced hold, its replay and the settle ack stay accepted", func(t *testing.T) {
		run, op := f.run(t, "running")
		w := f.worker(t)
		if _, applied, err := f.report(t, w, run, "credential_switch_held", f.gen, nonce, &op); err != nil || !applied {
			t.Fatalf("op-fenced hold: applied=%v err=%v", applied, err)
		}
		if _, applied, err := f.report(t, w, run, "credential_switch_held", f.gen, nonce, &op); err != nil || !applied {
			t.Fatalf("hold replay: applied=%v err=%v, want accepted", applied, err)
		}
		if _, applied, err := f.report(t, w, run, "credential_switch_hold_settled", f.gen, nonce, nil); err != nil || !applied {
			t.Fatalf("hold_settled: applied=%v err=%v, want accepted", applied, err)
		}
	})
	t.Run("an op-fenced hold for another operation is refused", func(t *testing.T) {
		run, _ := f.run(t, "running")
		other := uuid.New()
		f.noWrite(t, run, func() {
			if _, applied, err := f.report(t, f.worker(t), run, "credential_switch_held", f.gen, nonce, &other); err != nil || applied {
				t.Fatalf("held with a foreign op: applied=%v err=%v, want refused", applied, err)
			}
		})
	})
}

func TestV1ReleaseAcceptedMidRollLiveDB(t *testing.T) {
	f := newApplyFx(t)
	f.register(t, applyNegotiatedCaps)
	// An older replica's re-registration rotates the snapshot nonce without stamping the apply nonce.
	f.oldReplicaRegister(t, "nonce-old-replica")
	if apply, snap := f.applyNonce(t); apply == snap {
		t.Fatalf("apply nonce %q still equals the snapshot nonce after an old-replica registration", apply)
	}
	run := f.heldV1Run(t)
	got, applied, err := f.report(t, f.worker(t), run, "credential_switch", f.gen, "", nil)
	if err != nil || !applied || got.Status != "queued" || !got.ClaimReleasedAt.Valid {
		t.Fatalf("mid-roll v1 release: status=%s released=%v applied=%v err=%v, want accepted", got.Status, got.ClaimReleasedAt, applied, err)
	}
	// The v1 hold stays accepted too.
	run2 := f.heldV1Run(t)
	if _, applied, err := f.report(t, f.worker(t), run2, "credential_switch_held", f.gen, "nonce-old-replica", nil); err != nil || !applied {
		t.Fatalf("mid-roll v1 hold: applied=%v err=%v, want accepted", applied, err)
	}
}

func TestOldRegistrationThenNewRetiresV1ReleaseLiveDB(t *testing.T) {
	f := newApplyFx(t)
	f.oldReplicaRegister(t, "nonce-old-replica")
	if applied, err := f.release(t, f.heldV1Run(t)); err != nil || !applied {
		t.Fatalf("release under an old registration: applied=%v err=%v, want accepted", applied, err)
	}
	f.register(t, applyNegotiatedCaps)
	run := f.heldV1Run(t)
	f.noWrite(t, run, func() {
		if applied, err := f.release(t, run); applied || !errors.Is(err, ErrCredentialSwitchReleaseRetired) {
			t.Fatalf("release after the new registration: applied=%v err=%v, want ErrCredentialSwitchReleaseRetired", applied, err)
		}
	})
}

// Issue #4177 cross-reference: an in-place apply keeps the claim, so it opens no new claim generation
// and no recovery adoption (custody hold), the way the release+reclaim path it replaces would.
func TestInPlaceApplyOpensNoClaimGenerationOrAdoptionLiveDB(t *testing.T) {
	f := newApplyFx(t)
	f.register(t, applyNegotiatedCaps)
	run, op := f.run(t, "running")
	before := mustRun(t, f.env, run)
	holds := func() (n int64) {
		if err := f.env.pool.QueryRow(f.env.ctx, `SELECT count(*) FROM recovery_custody_holds WHERE run_id = $1`, run).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	h0 := holds()
	f.want(t, f.apply(t, run, f.reqWithNonce(t, op)), ApplyOutcomeApplied, "")
	after := mustRun(t, f.env, run)
	if after.ClaimGeneration != before.ClaimGeneration || after.ClaimReleasedAt.Valid || after.WorkerID != before.WorkerID || after.Status != before.Status {
		t.Fatalf("claim moved: generation %d->%d released=%v worker %v->%v status %s->%s",
			before.ClaimGeneration, after.ClaimGeneration, after.ClaimReleasedAt, before.WorkerID, after.WorkerID, before.Status, after.Status)
	}
	if h1 := holds(); h1 != h0 {
		t.Fatalf("recovery_custody_holds %d -> %d, want no adoption", h0, h1)
	}
}

// reqWithNonce is f.req with the worker's current (post-Register) nonce.
func (f applyFx) reqWithNonce(t *testing.T, op uuid.UUID) CredentialSwitchApplyRequest {
	t.Helper()
	r := f.req(op)
	r.RegisterNonce = f.worker(t).SnapshotRegisterNonce.String
	return r
}
