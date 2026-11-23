package workersvc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// A Codex provider-bound run's frozen binding (provider, revision, key mode, key secret, key rev) must
// survive every worker-loss recovery path unchanged: the sweep resets, requeues and parks the run, a
// reclaim by the same claimant delivers the identical binding, and a key rotated in between is refused
// as provider_binding_stale before any key is opened. Every case drives the production Service.Sweep and
// a real Service.Claim of the real creation fixture (newCodexActualClaimFix); the sweep's clock is the
// service clock (SetClock), and worker liveness is the workers.last_heartbeat_at column the sweep reads.

// sweepRecoveryFix wraps the actual-claim fixture with the frozen binding captured at creation.
type sweepRecoveryFix struct {
	pf     *pcFix
	id     uuid.UUID
	frozen string // the five binding columns as one string, read before the first claim
}

func newSweepRecoveryFix(t *testing.T) *sweepRecoveryFix {
	t.Helper()
	pf, id := newCodexActualClaimFix(t)
	restartCodexActualClaimService(pf)
	f := &sweepRecoveryFix{pf: pf, id: id}
	f.frozen = f.bindingColumns(t)
	return f
}

// bindingColumns reads the frozen model_provider_* columns as one composite text value, so
// "byte-identical" is one string comparison independent of the Go struct mapping. A NULL column
// would render as an empty field, so the five NOT NULL facts are read alongside and enforced.
func (f *sweepRecoveryFix) bindingColumns(t *testing.T) string {
	t.Helper()
	var s string
	var allSet bool
	err := f.pf.env.pool.QueryRow(f.pf.env.ctx, `SELECT row(model_provider_id, model_provider_revision,
	    model_provider_key_mode, model_provider_key_secret_id, model_provider_key_rev)::text,
	    (model_provider_id IS NOT NULL AND model_provider_revision IS NOT NULL AND model_provider_key_mode IS NOT NULL
	     AND model_provider_key_secret_id IS NOT NULL AND model_provider_key_rev IS NOT NULL)
	    FROM runs WHERE id = $1`, f.id).Scan(&s, &allSet)
	if err != nil {
		t.Fatalf("read binding columns: %v", err)
	}
	if !allSet {
		t.Fatalf("binding columns %s: a frozen binding column is NULL", s)
	}
	return s
}

func (f *sweepRecoveryFix) run(t *testing.T) store.Run { return mustRun(t, f.pf.env, f.id) }

// claim runs the real Claim and asserts the provider-A payload and the generation it advanced to.
func (f *sweepRecoveryFix) claim(t *testing.T, wantGen int64) store.Run {
	t.Helper()
	// A worker the sweep marked offline is brought back as a heartbeat would.
	f.pf.env.exec(`UPDATE workers SET status = 'online', last_heartbeat_at = now() WHERE id = $1`, f.pf.workerID)
	r := wantCodexActualProviderClaim(t, f.pf, f.id)
	if r.ClaimGeneration != wantGen || r.Status != "claimed" {
		t.Fatalf("claim: status=%s generation=%d, want claimed at %d", r.Status, r.ClaimGeneration, wantGen)
	}
	f.wantFrozen(t, "after claim")
	return r
}

func (f *sweepRecoveryFix) wantFrozen(t *testing.T, when string) {
	t.Helper()
	if got := f.bindingColumns(t); got != f.frozen {
		t.Fatalf("%s: binding columns = %q, want the frozen %q", when, got, f.frozen)
	}
}

// sweepAt runs the production Sweep with the service clock at `at` and the worker's last heartbeat at
// `heartbeat`, then restores the real clock.
func (f *sweepRecoveryFix) sweepAt(t *testing.T, at, heartbeat time.Time) SweepResult {
	t.Helper()
	f.pf.env.exec(`UPDATE workers SET status = 'online', last_heartbeat_at = $2 WHERE id = $1`, f.pf.workerID, heartbeat)
	f.pf.svc.SetClock(func() time.Time { return at })
	defer f.pf.svc.SetClock(nil)
	res, err := f.pf.svc.Sweep(f.pf.env.ctx)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	return res
}

// wantArm asserts only that the expected sweep arm fired at least once. The production Sweep is
// database-wide, so rows left by other tests in a shared database can add to any counter; the
// per-run assertions (wantRun, wantWorkerKept) are the proof that this fixture's run took the arm.
func wantArm(t *testing.T, when, arm string, got int64) {
	t.Helper()
	if got < 1 {
		t.Fatalf("%s: sweep counter %s = %d, want at least 1", when, arm, got)
	}
}

// wantWorkerStatus asserts the fixture worker's persisted liveness status.
func (f *sweepRecoveryFix) wantWorkerStatus(t *testing.T, when, want string) {
	t.Helper()
	var got string
	if err := f.pf.env.pool.QueryRow(f.pf.env.ctx, `SELECT status FROM workers WHERE id = $1`, f.pf.workerID).Scan(&got); err != nil {
		t.Fatalf("read worker status: %v", err)
	}
	if got != want {
		t.Fatalf("%s: worker status = %q, want %q", when, got, want)
	}
}

// wantRun asserts the run's status, requeue_count, claim_generation and worker affinity.
func (f *sweepRecoveryFix) wantRun(t *testing.T, when, status string, requeues int32, gen int64) store.Run {
	t.Helper()
	r := f.run(t)
	if r.Status != status || r.RequeueCount != requeues || r.ClaimGeneration != gen {
		t.Fatalf("%s: status=%s requeue_count=%d claim_generation=%d, want %s/%d/%d", when, r.Status, r.RequeueCount, r.ClaimGeneration, status, requeues, gen)
	}
	f.wantFrozen(t, when)
	wantCodexActualNoVendorWrites(t, f.pf, r)
	return r
}

func (f *sweepRecoveryFix) wantWorkerKept(t *testing.T, when string, r store.Run) {
	t.Helper()
	if !r.WorkerID.Valid || uuid.UUID(r.WorkerID.Bytes) != f.pf.workerID {
		t.Fatalf("%s: worker_id = %v, want kept as %s", when, r.WorkerID, f.pf.workerID)
	}
}

func TestCodexProviderSweepRecoveryLiveDB(t *testing.T) {
	const grace = 5 * time.Minute // testParams ClaimGrace

	t.Run("live heartbeat and an aborted finisher is reset and reclaimed with the same binding", func(t *testing.T) {
		f := newSweepRecoveryFix(t)
		// Abort after assembly, before the exact-claim finisher commits: ClaimRun has committed
		// generation 1 and the worker never learned of it.
		ctx, cancel := context.WithCancel(f.pf.env.ctx)
		defer cancel()
		f.pf.svc.claimHooks = &claimTestHooks{afterAssembly: func(_ context.Context, _ store.Run, p *ClaimPayload, err error) {
			if err != nil || p == nil || p.Secrets.Provider == nil {
				t.Errorf("abort hook needs a successful provider assembly (payload=%v err=%v)", p != nil, err)
			}
			cancel()
		}}
		if p, err := f.pf.svc.Claim(ctx, f.pf.worker(t), nil); err == nil && p != nil {
			t.Fatal("the aborted claim delivered a payload")
		}
		f.pf.svc.claimHooks = nil
		claimed := f.wantRun(t, "after the aborted claim", "claimed", 0, 1)

		at := claimed.ClaimedAt.Time.Add(grace + time.Minute)
		res := f.sweepAt(t, at, at) // the worker keeps heartbeating
		wantArm(t, "live heartbeat", "ClaimedReset", res.ClaimedReset)
		r := f.wantRun(t, "after the sweep", "queued", 0, 1)
		f.wantWorkerKept(t, "after the sweep", r)

		f.claim(t, 2) // the same worker reclaims; wantCodexActualProviderClaim proved the identical binding
	})

	t.Run("stale heartbeat on a claimed run past grace is reset not requeued", func(t *testing.T) {
		f := newSweepRecoveryFix(t)
		claimed := f.claim(t, 1)
		at := claimed.ClaimedAt.Time.Add(grace + time.Minute)
		res := f.sweepAt(t, at, at.Add(-10*time.Minute))
		wantArm(t, "stale heartbeat", "ClaimedReset", res.ClaimedReset)
		wantArm(t, "stale heartbeat", "WorkersOffline", res.WorkersOffline)
		f.wantWorkerStatus(t, "after the sweep", "offline")
		// The reset arm, not the stale-requeue arm: requeue_count stays 0 (the stale arm would make it 1).
		r := f.wantRun(t, "after the sweep", "queued", 0, 1)
		f.wantWorkerKept(t, "after the sweep", r)
		f.claim(t, 2)
	})

	t.Run("retained custody continuation is deferred inside the witness window then parked unwitnessed", func(t *testing.T) {
		f := newSweepRecoveryFix(t)
		first := f.claim(t, 1)
		at := first.ClaimedAt.Time.Add(grace + time.Minute)
		wantArm(t, "generation 1 reset", "ClaimedReset", f.sweepAt(t, at, at).ClaimedReset)
		f.wantRun(t, "after the first reset", "queued", 0, 1)

		// Generation 2 is a continuation: the generation 1 custody hold stays open.
		second := f.claim(t, 2)
		var heldGen int64
		if err := f.pf.env.pool.QueryRow(f.pf.env.ctx,
			`SELECT min(generation) FROM recovery_custody_holds WHERE run_id = $1 AND state = 'open'`, f.id).Scan(&heldGen); err != nil || heldGen >= second.ClaimGeneration {
			t.Fatalf("open hold generation = %d (%v), want an earlier generation than %d", heldGen, err, second.ClaimGeneration)
		}
		// Presence: the live worker's accepted snapshot lists the generation-2 flight.
		var nonce string
		var epoch int64
		if err := f.pf.env.pool.QueryRow(f.pf.env.ctx, `SELECT snapshot_register_nonce, snapshot_epoch FROM workers WHERE id = $1`, f.pf.workerID).Scan(&nonce, &epoch); err != nil {
			t.Fatal(err)
		}
		if _, err := f.pf.svc.Heartbeat(f.pf.env.ctx, f.pf.worker(t), nil, nil, &ActiveSnapshot{
			SnapshotEpoch: epoch + 1, RegisterNonce: nonce,
			Active: []ActiveRunEntry{{RunID: f.id.String(), ClaimGeneration: 2, Phase: "running"}},
		}); err != nil {
			t.Fatalf("Heartbeat with a listing snapshot: %v", err)
		}

		// Past the claim grace but inside 2*grace: deferred, still claimed.
		inside := second.ClaimedAt.Time.Add(grace + time.Minute)
		f.sweepAt(t, inside, inside)
		f.wantRun(t, "deferred", "claimed", 0, 2) // neither reset, charged nor parked: still claimed at generation 2

		// Past the window: parked unwitnessed, never a charge.
		past := second.ClaimedAt.Time.Add(2*grace + time.Minute)
		wantArm(t, "past the witness window", "ClaimedUnwitnessed", f.sweepAt(t, past, past).ClaimedUnwitnessed)
		r := f.wantRun(t, "parked", "recovery_wait", 0, 2)
		if r.RecoveryWaitCause.String != "retained_continuation_unwitnessed" {
			t.Fatalf("recovery_wait_cause = %q, want retained_continuation_unwitnessed", r.RecoveryWaitCause.String)
		}
	})

	t.Run("a running run on a worker that went stale is requeued and reclaimed by the same claimant", func(t *testing.T) {
		f := newSweepRecoveryFix(t)
		claimed := f.claim(t, 1)
		g := claimed.ClaimGeneration
		res, err := f.pf.svc.SetStateReportWithReconciliation(f.pf.env.ctx, f.pf.worker(t), f.id, StateRequest{State: "running", ClaimGeneration: &g})
		if err != nil || !res.Applied {
			t.Fatalf("running report applied=%v err=%v", res.Applied, err)
		}
		f.wantRun(t, "after the running report", "running", 0, 1)

		at := time.Now().Add(time.Minute)
		sw := f.sweepAt(t, at, at.Add(-10*time.Minute))
		wantArm(t, "stale running run", "StaleRequeued", sw.StaleRequeued)
		r := f.wantRun(t, "after the sweep", "queued", 1, 1)
		f.wantWorkerKept(t, "after the sweep", r)

		f.claim(t, 2) // ADR-4476: the same claimant takes it back with the identical binding
	})

	// This covers the pre-open providerBindingRefusal arm of Claim (the persisted binding is already
	// stale when the run is picked); lockedProviderRefusal's stale arm is covered by
	// codex_provider_binding_livedb_test.go.
	t.Run("a key rotated between the reset and the reclaim is refused before the key is opened", func(t *testing.T) {
		f := newSweepRecoveryFix(t)
		claimed := f.claim(t, 1)
		at := claimed.ClaimedAt.Time.Add(grace + time.Minute)
		wantArm(t, "reset", "ClaimedReset", f.sweepAt(t, at, at).ClaimedReset)
		f.wantRun(t, "after the reset", "queued", 0, 1)

		// A real rotation: new ciphertext (deliberately undecryptable, so an attempted open would
		// surface as a different error) and an enablement_rev bump.
		if _, err := f.pf.env.q.RotateUserSecret(f.pf.env.ctx, store.RotateUserSecretParams{
			ID: f.pf.key, UserID: f.pf.userID, Ciphertext: []byte("not-a-sealed-blob"), SealedWith: "master",
		}); err != nil {
			t.Fatalf("RotateUserSecret: %v", err)
		}

		// Narrow seam: the binding assembly on the persisted run reads no ciphertext.
		reads := &codexBindingReads{Queries: f.pf.env.q}
		probe := New(reads, f.pf.env.box, f.pf.svc.p)
		probe.SetVault(f.pf.env.vlt)
		pb, err := probe.assembleProviderBinding(f.pf.env.ctx, f.pf.worker(t), f.run(t))
		if pb != nil || !errors.Is(err, ErrProviderBindingStale) {
			t.Fatalf("assembleProviderBinding present=%t err=%v, want ErrProviderBindingStale", pb != nil, err)
		}
		if reads.cipherReads != 0 {
			t.Fatalf("the stale binding read ciphertext %d time(s)", reads.cipherReads)
		}

		// End to end through the real claim: parked, typed, nothing delivered, binding intact.
		f.pf.env.exec(`UPDATE workers SET status = 'online', last_heartbeat_at = now() WHERE id = $1`, f.pf.workerID)
		p, err := f.pf.svc.Claim(f.pf.env.ctx, f.pf.worker(t), nil)
		if err != nil || p != nil {
			t.Fatalf("Claim payload present=%t err=%v, want a refusal that delivers nothing", p != nil, err)
		}
		parked := f.pf.wantParked(t, f.id, holdProviderBindingStale)
		wantCodexActualNoVendorWrites(t, f.pf, parked)
		f.wantFrozen(t, "after the refused claim")
	})
}
