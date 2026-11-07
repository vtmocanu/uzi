package workersvc

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4058, accounting evidence for a successor that parks immediately at
// recovery_wait / retained_recovery_adoption_failed because a cancelled predecessor's source
// carries an active remediation record. The agent-side classifier is not under test here: this
// file proves, through the real ClaimRun, the real state-report writer, the real snapshot
// heartbeat and the real claimed-never-started sweep, how the EXISTING accounting treats such a
// run. Nothing writes a departure witness, a listing or registration evidence by hand; open
// custody holds are counted after every step so the no-release premise is asserted, not assumed.
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.

const rhCause = recoveryCauseRetainedRecoveryAdoptionFailed

// newRHFixture is one owner with one recovery-capable worker, RUN_MAX_REQUEUES = max and a
// custody_hold_limit of limit.
func newRHFixture(t *testing.T, env codexTestEnv, max int, limit int32) *cncFixture {
	t.Helper()
	f := newCNCFixture(t, env, max, func(p *Params) { p.CustodyHoldLimit = &fakeCustodyLimit{n: limit} })
	return f.rhInventory(t)
}

// rhInventory makes the fixture worker advertise recovery_inventory_v1, so every claim it takes
// opens an inventory-guarded hold (the production shape this issue is about).
func (f *cncFixture) rhInventory(t *testing.T) *cncFixture {
	t.Helper()
	f.wkr.ProtocolCapabilities = append(f.wkr.ProtocolCapabilities, capability.RecoveryInventoryV1)
	f.env.exec(`UPDATE workers SET protocol_capabilities = $2 WHERE id = $1`, f.workerID, f.wkr.ProtocolCapabilities)
	return f
}

// rhAssertAllGuarded fails unless every open hold of the run is inventory-guarded.
func (f *cncFixture) rhAssertAllGuarded(t *testing.T, run uuid.UUID) {
	t.Helper()
	var n int64
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT count(*) FROM recovery_custody_holds WHERE run_id = $1 AND state = 'open' AND NOT inventory_guarded`, run).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d open holds of %s are not inventory-guarded; the claims were not taken on a recovery_inventory_v1 worker", n, run)
	}
}

// rhReregister is a worker restart: a new registration nonce, protocol capabilities restored the
// way newCNCFixture does after Register.
func (f *cncFixture) rhReregister(t *testing.T) {
	t.Helper()
	_, nonce, err := f.svc.Register(f.env.ctx, store.Worker{ID: f.workerID, UserID: f.userID}, "v1", "base", nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("re-Register: %v", err)
	}
	f.nonce = nonce
	f.env.exec(`UPDATE workers SET protocol_capabilities = $2 WHERE id = $1`, f.workerID, f.wkr.ProtocolCapabilities)
}

func (f *cncFixture) rhRunning(t *testing.T, run uuid.UUID, gen int64) {
	t.Helper()
	g := gen
	res, err := f.svc.SetStateReportWithReconciliation(f.env.ctx, f.wkr, run, StateRequest{State: "running", ClaimGeneration: &g})
	if err != nil || !res.Applied {
		t.Fatalf("gen %d: running applied=%v err=%v", gen, res.Applied, err)
	}
}

func (f *cncFixture) rhPark(t *testing.T, run uuid.UUID, gen int64) (StateReportResult, error) {
	t.Helper()
	g := gen
	return f.svc.SetStateReportWithReconciliation(f.env.ctx, f.wkr, run, StateRequest{State: "recovery_wait", RecoveryCause: strPtr(rhCause), ClaimGeneration: &g})
}

// rhAssertImmediatePark is arm (a): recovery_wait/cause, NULL retry stamp, counters unchanged,
// claim released, affinity kept.
func (f *cncFixture) rhAssertImmediatePark(t *testing.T, run uuid.UUID, wantRequeue, wantRetainedParks int32, what string) store.Run {
	t.Helper()
	r := f.run(t, run)
	if r.Status != "recovery_wait" || r.RecoveryWaitCause.String != rhCause || r.FailOrigin.Valid || r.FinishedAt.Valid {
		t.Fatalf("%s: status=%s cause=%v fail_origin=%v finished=%v, want recovery_wait/%s", what, r.Status, r.RecoveryWaitCause, r.FailOrigin, r.FinishedAt.Valid, rhCause)
	}
	if r.RecoveryRetryNotBefore.Valid {
		t.Fatalf("%s: recovery_retry_not_before = %v, want NULL (owner hold, no timer)", what, r.RecoveryRetryNotBefore)
	}
	if r.RetainedParkCount != wantRetainedParks || r.RequeueCount != wantRequeue {
		t.Fatalf("%s: retained_park_count=%d requeue_count=%d, want %d/%d", what, r.RetainedParkCount, r.RequeueCount, wantRetainedParks, wantRequeue)
	}
	if !r.ClaimReleasedAt.Valid || !r.WorkerID.Valid || r.WorkerID != pgconv.UUID(f.workerID) {
		t.Fatalf("%s: claim_released=%v worker_id=%v, want released with affinity kept", what, r.ClaimReleasedAt.Valid, r.WorkerID)
	}
	return r
}

func (f *cncFixture) rhHoldRows(t *testing.T, run uuid.UUID) string {
	t.Helper()
	var s string
	if err := f.env.pool.QueryRow(f.env.ctx,
		`SELECT COALESCE(jsonb_agg(to_jsonb(h) ORDER BY h.generation, h.id)::text, '[]') FROM recovery_custody_holds h WHERE h.run_id = $1`, run).Scan(&s); err != nil {
		t.Fatalf("hold rows: %v", err)
	}
	return s
}

// rhPredecessor seeds the cancelled predecessor's open custody hold (an owner row only; the
// predecessor's worker-side source is outside the api) and returns its run id and hold id.
func (f *cncFixture) rhPredecessor(t *testing.T) (uuid.UUID, uuid.UUID) {
	t.Helper()
	pred := uuid.New()
	f.seedHold(f.userID, pred, 1, "open")
	var holdID uuid.UUID
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT id FROM recovery_custody_holds WHERE run_id = $1`, pred).Scan(&holdID); err != nil {
		t.Fatalf("read predecessor hold: %v", err)
	}
	return pred, holdID
}

// rhDrainLostClaims drives arm (c), the omit-only cycle: each cycle is a real ClaimRun whose
// flight never reports running and is never listed; an accepted snapshot omits the run (no
// Register, so the worker is not restarted) and the real claimed-never-started sweep resets it.
// It continues until ClaimRun refuses, and returns the number of claims it made. Every cycle
// asserts: no departure stamp, an uncharged reset (requeue_count unchanged), no park, and exactly
// one more open own hold. A restart-driven loss (Register with a new nonce) is a CHARGED path and
// is measured by TestRemediationHoldRestartLossChargedLiveDB.
func (f *cncFixture) rhDrainLostClaims(t *testing.T, run uuid.UUID, limit int, firstGen int64, wantRequeue int32, earlierHolds int64) int {
	t.Helper()
	claims := 0
	for gen := firstGen; ; gen++ {
		if claims > limit+3 {
			t.Fatalf("runaway: %d claims at a custody limit of %d", claims, limit)
		}
		got, g := f.claim(t)
		if got == uuid.Nil {
			return claims
		}
		if got != run || g != gen {
			t.Fatalf("claim = (%s, gen %d), want (%s, gen %d)", got, g, run, gen)
		}
		claims++
		f.rhAssertAllGuarded(t, run)
		f.backdate(run)
		if own := f.ownOpenHolds(t, run); own != int64(claims)+earlierHolds {
			t.Fatalf("gen %d: own open holds = %d, want %d (every claim opens one hold; nothing released one)", gen, own, int64(claims)+earlierHolds)
		}
		f.omit(t)
		if d := f.departure(t, run); d.gen.Valid {
			t.Fatalf("gen %d: a departure stamp %+v was produced for a flight that never reported or listed", gen, d)
		}
		if st := f.run(t, run); st.Status != "claimed" || st.RegistrationEvidenceGeneration.Valid {
			t.Fatalf("gen %d: status=%s evidence=%v before the sweep, want claimed with no evidence", gen, st.Status, st.RegistrationEvidenceGeneration)
		}
		holdsBefore := f.ownOpenHolds(t, run)
		sw := f.sweep(t)
		r := f.run(t, run)
		if r.Status != "queued" || r.RequeueCount != wantRequeue || r.ClaimReleasedAt.Valid || r.RecoveryWaitCause.Valid || r.FailOrigin.Valid {
			t.Fatalf("gen %d: status=%s requeue_count=%d claim_released=%v cause=%v fail_origin=%v (sweep %+v), want an uncharged reset to queued (count %d)",
				gen, r.Status, r.RequeueCount, r.ClaimReleasedAt.Valid, r.RecoveryWaitCause, r.FailOrigin, sw, wantRequeue)
		}
		if sw.ClaimedCharged != 0 || sw.ClaimedExhausted != 0 || sw.ClaimedUnwitnessed != 0 || sw.ClaimedReset < 1 {
			t.Fatalf("gen %d: sweep = %+v, want an uncharged reset by the sweep and no charge, exhaustion or unwitnessed park", gen, sw)
		}
		if got := f.ownOpenHolds(t, run); got != holdsBefore {
			t.Fatalf("gen %d: the sweep changed own open holds %d -> %d", gen, holdsBefore, got)
		}
		t.Logf("lost claim: gen=%d claims=%d own_open_holds=%d requeue_count=%d", gen, claims, f.ownOpenHolds(t, run), r.RequeueCount)
	}
}

// rhAssertRefusedQueued asserts the end state of arm (c): ClaimRun refuses at own holds = limit,
// the run stays a plain queued run (not a park, not failed) and health names the custody cap.
func (f *cncFixture) rhAssertRefusedQueued(t *testing.T, run uuid.UUID, limit int32, wantRequeue int32, predRows string, pred uuid.UUID) {
	t.Helper()
	if got := f.ownOpenHolds(t, run); got != int64(limit) {
		t.Fatalf("own open holds = %d at refusal, want exactly the limit %d", got, limit)
	}
	if got, _ := f.claim(t); got != uuid.Nil {
		t.Fatalf("claimed %s with %d own open holds at a limit of %d, want idle", got, limit, limit)
	}
	r := f.run(t, run)
	if r.Status != "queued" || r.RecoveryWaitCause.Valid || r.FailOrigin.Valid || r.FinishedAt.Valid || r.RequeueCount != wantRequeue {
		t.Fatalf("status=%s cause=%v fail_origin=%v requeue_count=%d, want a plain queued run (not recovery_wait, not failed), count %d", r.Status, r.RecoveryWaitCause, r.FailOrigin, r.RequeueCount, wantRequeue)
	}
	adm, err := f.env.q.GetCustodyAdmissionForRun(f.env.ctx, store.GetCustodyAdmissionForRunParams{
		UserID: f.userID, RunID: run, CustodyHoldLimit: limit, HeartbeatCutoff: pgconv.Time(time.Now().Add(-time.Hour)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if adm.ContinuationExempt || adm.AdmissionCountedHolds < int64(limit) {
		t.Fatalf("admission = %+v, want the exemption exhausted and the owner count at/over %d", adm, limit)
	}
	row := store.ListActiveRunsForHealthRow{ID: run, UserID: f.userID, Status: "queued", Kind: "issue"}
	if got := f.svc.queuedReason(f.env.ctx, time.Now(), row, thWithLimit(limit)); got != reasonCustodyLimit {
		t.Fatalf("health reason = %q, want %q", got, reasonCustodyLimit)
	}
	if got := f.rhHoldRows(t, pred); got != predRows {
		t.Fatalf("the predecessor's custody rows changed:\n before %s\n after  %s", predRows, got)
	}
}

// TestRemediationHoldImmediateParkLiveDB: arm (a), the lost park ACK, and the delayed-report
// fences (worker identity plus claim generation).
func TestRemediationHoldImmediateParkLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)

	t.Run("positively ACKed park ends the claim", func(t *testing.T) {
		f := newRHFixture(t, env, 3, testCustodyLimit)
		pred, _ := f.rhPredecessor(t)
		predRows := f.rhHoldRows(t, pred)
		run := f.seedRun("queued", 0)
		f.claimRun(t, run, 1)
		f.rhRunning(t, run, 1)
		before := f.run(t, run)
		res, err := f.rhPark(t, run, 1)
		if err != nil || !res.Applied {
			t.Fatalf("park applied=%v err=%v", res.Applied, err)
		}
		if res.Run.Status != "recovery_wait" || res.Run.RecoveryWaitCause.String != rhCause {
			t.Fatalf("park ACK = %s/%v, want recovery_wait/%s (the worker confirms a hold only from this)", res.Run.Status, res.Run.RecoveryWaitCause, rhCause)
		}
		r := f.rhAssertImmediatePark(t, run, before.RequeueCount, before.RetainedParkCount, "after park")
		if got := f.ownOpenHolds(t, run); got != 1 {
			t.Fatalf("own open holds = %d, want 1 (one claim, one hold)", got)
		}
		f.rhAssertAllGuarded(t, run)
		if got := f.rhHoldRows(t, pred); got != predRows {
			t.Fatalf("the park changed the predecessor's custody rows")
		}
		t.Logf("(a) immediate park: claims=1 own_open_holds=1 requeue_count=%d retained_park_count=%d retry=%v released=%v worker_kept=%v",
			r.RequeueCount, r.RetainedParkCount, r.RecoveryRetryNotBefore.Valid, r.ClaimReleasedAt.Valid, r.WorkerID.Valid)

		// A sweep and a worker restart neither move nor re-claim the held run.
		f.rhReregister(t)
		f.sweep(t)
		f.rhAssertImmediatePark(t, run, before.RequeueCount, before.RetainedParkCount, "after sweep and re-register")
		if got, _ := f.claim(t); got != uuid.Nil {
			t.Fatalf("claimed %s while held, want idle", got)
		}

		// Lost park ACK: the worker replays the same-generation report; the claim is already
		// released, so it is stale, writes nothing and counts nothing twice.
		frozen := f.rowJSON(t, run)
		if _, err := f.rhPark(t, run, 1); !errors.Is(err, ErrStaleClaim) {
			t.Fatalf("duplicate same-generation park: err=%v, want ErrStaleClaim", err)
		}
		sameExcept(t, "duplicate park", frozen, f.rowJSON(t, run))
		if got := f.ownOpenHolds(t, run); got != 1 {
			t.Fatalf("own open holds = %d after the duplicate, want 1", got)
		}
	})

	t.Run("a delayed report from an earlier generation is stale after a re-claim", func(t *testing.T) {
		f := newRHFixture(t, env, 3, testCustodyLimit)
		run := f.seedRun("queued", 0)
		f.claimRun(t, run, 1)
		f.rhRunning(t, run, 1)
		f.heartbeat(t, f.next(), f.nonce, ActiveRunEntry{RunID: run.String(), ClaimGeneration: 1, Phase: "running"})
		f.rhReregister(t)
		f.env.exec(`UPDATE runs SET status_since = now() - interval '2 hours' WHERE id = $1`, run)
		f.omit(t)
		if r := f.run(t, run); r.Status != "queued" {
			t.Fatalf("after re-register and an omitting snapshot status=%s requeue_count=%d, want the run requeued", r.Status, r.RequeueCount)
		}
		f.claimRun(t, run, 2)
		frozen := f.rowJSON(t, run)
		holds := f.rhHoldRows(t, run)
		if _, err := f.rhPark(t, run, 1); !errors.Is(err, ErrStaleClaim) {
			t.Fatalf("delayed generation-1 park: err=%v, want ErrStaleClaim", err)
		}
		sameExcept(t, "delayed old-generation park", frozen, f.rowJSON(t, run))
		if got := f.rhHoldRows(t, run); got != holds {
			t.Fatalf("the delayed park changed the custody rows")
		}
		if r := f.run(t, run); r.Status != "claimed" || r.ClaimReleasedAt.Valid {
			t.Fatalf("status=%s released=%v, want the generation-2 claim untouched", r.Status, r.ClaimReleasedAt.Valid)
		}
	})

	t.Run("without a re-claim a same-generation report applies only this hold", func(t *testing.T) {
		f := newRHFixture(t, env, 3, testCustodyLimit)
		bystander := f.seedRun("queued", 0)
		run := f.seedRun("queued", 0)
		got, gen := f.claim(t)
		if got != bystander || gen != 1 {
			t.Fatalf("first claim = (%s, %d), want the bystander", got, gen)
		}
		f.rhRunning(t, bystander, 1)
		f.claimRun(t, run, 1)
		f.rhRunning(t, run, 1)
		byRow, byHolds := f.rowJSON(t, bystander), f.rhHoldRows(t, bystander)
		res, err := f.rhPark(t, run, 1)
		if err != nil || !res.Applied {
			t.Fatalf("same-generation park: applied=%v err=%v", res.Applied, err)
		}
		f.rhAssertImmediatePark(t, run, 0, 0, "delayed same-generation park")
		sameExcept(t, "bystander", byRow, f.rowJSON(t, bystander), "updated_at")
		if got := f.rhHoldRows(t, bystander); got != byHolds {
			t.Fatalf("the park changed another run's custody rows")
		}
		if got := f.ownOpenHolds(t, run); got != 1 {
			t.Fatalf("own open holds = %d, want 1", got)
		}
	})
}

// TestRemediationHoldUnchargedResetsBoundedByLimitLiveDB is the counterexample to a per-episode
// claim bound: claims lost before running, listing or registration evidence are reset uncharged
// by the sweep and the only bound is the run's own open-hold count against custody_hold_limit.
func TestRemediationHoldUnchargedResetsBoundedByLimitLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	const max = 3

	for _, limit := range []int32{8, 3} {
		t.Run(map[int32]string{8: "limit 8 (default)", 3: "limit 3 (low-limit control)"}[limit], func(t *testing.T) {
			f := newRHFixture(t, env, max, limit)
			pred, predHold := f.rhPredecessor(t)
			predRows := f.rhHoldRows(t, pred)
			run := f.seedRun("queued", 0)
			claims := f.rhDrainLostClaims(t, run, int(limit), 1, 0, 0)
			if claims != int(limit) {
				t.Fatalf("claims = %d, want %d (the run's own open holds reach the limit and then ClaimRun refuses)", claims, limit)
			}
			if limit == 8 && claims <= max+2 {
				t.Fatalf("claims = %d, want more than RUN_MAX_REQUEUES+2 = %d uncharged claims", claims, max+2)
			}
			f.rhAssertRefusedQueued(t, run, limit, 0, predRows, pred)
			t.Logf("(c) first episode, limit=%d RUN_MAX_REQUEUES=%d: uncharged claims=%d own_open_holds=%d requeue_count=0 final=queued", limit, max, claims, f.ownOpenHolds(t, run))

			// Owner discard of the predecessor's hold changes only that custody row.
			frozen := f.rowJSON(t, run)
			runHolds := f.rhHoldRows(t, run)
			n, err := f.env.q.DiscardCustodyHoldForOwner(f.env.ctx, store.DiscardCustodyHoldForOwnerParams{
				HoldID: predHold, RunID: pred, UserID: f.userID, ReleaseEvidence: pgconv.TextOrNull("owner_discard"),
			})
			if err != nil || n != 1 {
				t.Fatalf("discard predecessor hold: n=%d err=%v", n, err)
			}
			sameExcept(t, "predecessor discard", frozen, f.rowJSON(t, run))
			if got := f.rhHoldRows(t, run); got != runHolds {
				t.Fatalf("discarding the predecessor's hold changed the successor's custody rows")
			}
			if got := f.ownOpenHolds(t, run); got != int64(limit) {
				t.Fatalf("successor own open holds = %d after the predecessor discard, want %d unchanged", got, limit)
			}
		})
	}

	t.Run("later owner episode after a confirmed park and Resume", func(t *testing.T) {
		const limit = testCustodyLimit
		f := newRHFixture(t, env, max, limit)
		pred, _ := f.rhPredecessor(t)
		predRows := f.rhHoldRows(t, pred)
		run := f.seedRun("queued", 0)
		f.claimRun(t, run, 1)
		f.rhRunning(t, run, 1)
		if res, err := f.rhPark(t, run, 1); err != nil || !res.Applied {
			t.Fatalf("park applied=%v err=%v", res.Applied, err)
		}
		f.rhAssertImmediatePark(t, run, 0, 0, "first episode park")
		row, err := f.env.q.ResumeWorkerRecoveryEpisode(f.env.ctx, store.ResumeWorkerRecoveryEpisodeParams{ID: run, UserID: f.userID, GlobalTimeoutSeconds: 3600})
		if err != nil || row.Status != "queued" {
			t.Fatalf("Resume: status=%q err=%v, want queued", row.Status, err)
		}
		if r := f.run(t, run); r.Status != "queued" || r.RecoveryWaitCause.Valid || r.WorkerID != pgconv.UUID(f.workerID) {
			t.Fatalf("after Resume status=%s cause=%v worker=%v, want queued, cause cleared, affinity kept", r.Status, r.RecoveryWaitCause, r.WorkerID)
		}
		claims := f.rhDrainLostClaims(t, run, limit, 2, 0, 1) // the confirmed park's own claim already holds one
		if claims != limit-1 {
			t.Fatalf("claims after Resume = %d, want %d (own holds start at 1 and stop at the limit %d)", claims, limit-1, limit)
		}
		f.rhAssertRefusedQueued(t, run, limit, 0, predRows, pred)
		t.Logf("(c) later episode: park claims=1, after Resume uncharged claims=%d, own_open_holds=%d, requeue_count=0 final=queued", claims, f.ownOpenHolds(t, run))
	})
}

// TestRemediationHoldRestartLossChargedLiveDB measures the restart-driven loss: a real ClaimRun
// whose flight never reports running or is listed, then a real Register with a new nonce. Register
// charges the run (requeue_count + 1) and, over RUN_MAX_REQUEUES, must park it for its owner at
// recovery_wait / worker_requeue_exhausted (unresolved custody is recorded), never fail it.
func TestRemediationHoldRestartLossChargedLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	const max = 3
	const limit = int32(8)
	f := newRHFixture(t, env, max, limit)
	run := f.seedRun("queued", 0)
	for gen := int64(1); ; gen++ {
		if gen > int64(limit)+2 {
			t.Fatalf("runaway: %d claims without a disposition", gen-1)
		}
		got, g := f.claim(t)
		if got != run || g != gen {
			t.Fatalf("claim = (%s, gen %d), want (%s, gen %d)", got, g, run, gen)
		}
		f.rhAssertAllGuarded(t, run)
		if own := f.ownOpenHolds(t, run); own != gen {
			t.Fatalf("gen %d: own open holds = %d after the claim, want %d", gen, own, gen)
		}
		f.backdate(run)
		f.rhReregister(t)
		r := f.run(t, run)
		if gen <= max {
			if r.Status != "queued" || r.RequeueCount != int32(gen) || r.FailOrigin.Valid || r.RecoveryWaitCause.Valid {
				t.Fatalf("gen %d: status=%s requeue_count=%d fail_origin=%v cause=%v, want a charged requeue to queued (count %d)", gen, r.Status, r.RequeueCount, r.FailOrigin, r.RecoveryWaitCause, gen)
			}
			if own := f.ownOpenHolds(t, run); own != gen {
				t.Fatalf("gen %d: own open holds = %d after Register, want %d (nothing released)", gen, own, gen)
			}
			t.Logf("restart loss: gen=%d own_open_holds=%d requeue_count=%d status=%s", gen, f.ownOpenHolds(t, run), r.RequeueCount, r.Status)
			continue
		}
		if r.Status == "failed" || r.FailOrigin.Valid {
			t.Fatalf("MEASURED FAILED: gen %d status=%s fail_origin=%v cause=%v requeue_count=%d own_open_holds=%d; expected an owner-held recovery_wait", gen, r.Status, r.FailOrigin, r.RecoveryWaitCause, r.RequeueCount, f.ownOpenHolds(t, run))
		}
		if r.Status != "recovery_wait" || r.RecoveryWaitCause.String != "worker_requeue_exhausted" || r.FinishedAt.Valid || r.RecoveryRetryNotBefore.Valid {
			t.Fatalf("gen %d: status=%s cause=%v retry=%v finished=%v requeue_count=%d, want recovery_wait/worker_requeue_exhausted with no timer", gen, r.Status, r.RecoveryWaitCause, r.RecoveryRetryNotBefore, r.FinishedAt.Valid, r.RequeueCount)
		}
		if got := f.ownOpenHolds(t, run); got != gen {
			t.Fatalf("own open holds = %d at exhaustion, want %d (nothing released)", got, gen)
		}
		if got, _ := f.claim(t); got != uuid.Nil {
			t.Fatalf("claimed %s while held, want idle", got)
		}
		t.Logf("(c') restart loss: RUN_MAX_REQUEUES=%d limit=%d claims=%d own_open_holds=%d requeue_count=%d final=%s/%s", max, limit, gen, f.ownOpenHolds(t, run), r.RequeueCount, r.Status, r.RecoveryWaitCause.String)
		return
	}
}

// TestRemediationHoldWitnessedExhaustionLiveDB: arm (b). Claims that did reach running (or whose
// departure the real snapshot path witnessed) are charged and end at the owner-held exhaustion
// park, never failed.
func TestRemediationHoldWitnessedExhaustionLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)

	t.Run("running omissions", func(t *testing.T) {
		const max = 3
		f := newRHFixture(t, env, max, testCustodyLimit)
		run := f.seedRun("queued", 0)
		for gen := int64(1); gen <= max+1; gen++ {
			f.claimRun(t, run, gen)
			f.rhRunning(t, run, gen)
			if own := f.ownOpenHolds(t, run); own != gen {
				t.Fatalf("gen %d: own open holds = %d, want %d", gen, own, gen)
			}
			f.heartbeat(t, f.next(), f.nonce, ActiveRunEntry{RunID: run.String(), ClaimGeneration: gen, Phase: "running"})
			f.env.exec(`UPDATE runs SET status_since = now() - interval '2 hours' WHERE id = $1`, run)
			f.omit(t)
			r := f.run(t, run)
			if gen <= max {
				if r.Status != "queued" || r.RequeueCount != int32(gen) {
					t.Fatalf("gen %d: status=%s requeue_count=%d, want the charged requeue %d", gen, r.Status, r.RequeueCount, gen)
				}
				continue
			}
			if r.Status != "recovery_wait" || r.RecoveryWaitCause.String != "worker_requeue_exhausted" || r.FailOrigin.Valid || r.FinishedAt.Valid || !r.ClaimReleasedAt.Valid {
				t.Fatalf("gen %d: status=%s cause=%v fail_origin=%v released=%v, want recovery_wait/worker_requeue_exhausted, never failed", gen, r.Status, r.RecoveryWaitCause, r.FailOrigin, r.ClaimReleasedAt.Valid)
			}
			if got := f.ownOpenHolds(t, run); got != gen {
				t.Fatalf("own open holds = %d at exhaustion, want %d (nothing released)", got, gen)
			}
			t.Logf("(b) running omissions: RUN_MAX_REQUEUES=%d claims=%d own_open_holds=%d requeue_count=%d final=%s/%s", max, gen, f.ownOpenHolds(t, run), r.RequeueCount, r.Status, r.RecoveryWaitCause.String)
		}
	})

	t.Run("departure stamp through the real snapshot path", func(t *testing.T) {
		const max = 2
		f := newCNCFixture(t, env, max).rhInventory(t)
		run := f.seedGuardedRun(t) // the first running report is refused by a real SetRunRunning guard
		for gen := int64(1); gen <= max+2; gen++ {
			e2 := f.cycle(t, run, gen)
			if d := f.departure(t, run); !d.is(gen, f.nonce, e2) {
				t.Fatalf("gen %d: departure = %+v, want the real snapshot path's stamp (%d, nonce, %d)", gen, d, gen, e2)
			}
			sw := f.sweep(t)
			r := f.run(t, run)
			switch {
			case gen == 1:
				f.assertQueued(t, run, 0, "first-ever claim is not a continuation")
			case gen <= max+1:
				f.assertQueued(t, run, int32(gen-1), "charged")
				if sw.ClaimedCharged < 1 {
					t.Fatalf("gen %d sweep = %+v, want a charge", gen, sw)
				}
			default:
				if r.Status != "recovery_wait" || r.RecoveryWaitCause.String != "worker_requeue_exhausted" || r.FailOrigin.Valid || r.RequeueCount != max {
					t.Fatalf("gen %d: status=%s cause=%v fail_origin=%v count=%d, want recovery_wait/worker_requeue_exhausted at count %d", gen, r.Status, r.RecoveryWaitCause, r.FailOrigin, r.RequeueCount, max)
				}
				t.Logf("(b) witnessed departures: RUN_MAX_REQUEUES=%d claims=%d own_open_holds=%d requeue_count=%d final=%s/%s", max, gen, f.ownOpenHolds(t, run), r.RequeueCount, r.Status, r.RecoveryWaitCause.String)
			}
			if got := f.ownOpenHolds(t, run); got != gen {
				t.Fatalf("gen %d: own open holds = %d, want %d", gen, got, gen)
			}
		}
	})
}

// TestRemediationHoldUnwitnessedParkControlLiveDB: a claimed continuation whose worker stays
// online with current registration evidence and no departure stamp is parked at
// retained_continuation_unwitnessed after the window: uncharged, custody untouched.
func TestRemediationHoldUnwitnessedParkControlLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	f := newWitnessFixture(t, env, 5).rhInventory(t)
	run := f.continuation(t)
	f.claimAged(t, run, 2, cnwPastWindow)
	f.evidenceReport(t, run, 2, f.nonce, f.next())
	f.plainHeartbeat(t)
	holds, rows := f.ownOpenHolds(t, run), f.rhHoldRows(t, run)
	if holds != 2 {
		t.Fatalf("own open holds = %d before the sweep, want 2", holds)
	}
	if sw := f.sweep(t); sw.ClaimedUnwitnessed != 1 || sw.ClaimedCharged != 0 {
		t.Fatalf("sweep = %+v, want one unwitnessed park and no charge", sw)
	}
	f.assertUnwitnessedPark(t, run, 0, "presence control")
	if got := f.ownOpenHolds(t, run); got != holds {
		t.Fatalf("own open holds = %d after the park, want %d", got, holds)
	}
	if got := f.rhHoldRows(t, run); got != rows {
		t.Fatalf("the unwitnessed park changed the custody rows")
	}
	t.Logf("unwitnessed park: claims=2 own_open_holds=%d requeue_count=0 final=recovery_wait/retained_continuation_unwitnessed", holds)
}

// TestRemediationHoldOwnerCancelLiveDB: an owner Cancel of a held successor is terminal through
// the existing route; the number of its open guarded holds afterwards is measured and pinned.
func TestRemediationHoldOwnerCancelLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	f := newRHFixture(t, env, 3, testCustodyLimit)
	pred, _ := f.rhPredecessor(t)
	predRows := f.rhHoldRows(t, pred)
	run := f.seedRun("queued", 0)
	f.claimRun(t, run, 1)
	f.rhRunning(t, run, 1)
	if res, err := f.rhPark(t, run, 1); err != nil || !res.Applied {
		t.Fatalf("park applied=%v err=%v", res.Applied, err)
	}
	f.rhAssertAllGuarded(t, run)
	before := f.ownOpenHolds(t, run)
	if _, err := f.svc.SubmitInputWithOptions(f.env.ctx, f.userID, run, "cancel", "", nil, SubmitInputOptions{}); err != nil {
		t.Fatalf("owner cancel: %v", err)
	}
	r := f.run(t, run)
	if r.Status != "cancelled" || !r.FinishedAt.Valid {
		t.Fatalf("after Cancel status=%s finished=%v, want terminal cancelled", r.Status, r.FinishedAt.Valid)
	}
	after := f.ownOpenHolds(t, run)
	var states []byte
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT COALESCE(jsonb_agg(jsonb_build_object('generation', generation, 'state', state, 'inventory_guarded', inventory_guarded) ORDER BY generation), '[]')::text FROM recovery_custody_holds WHERE run_id = $1`, run).Scan(&states); err != nil {
		t.Fatal(err)
	}
	var shape []map[string]any
	if err := json.Unmarshal(states, &shape); err != nil {
		t.Fatal(err)
	}
	t.Logf("owner Cancel of a held successor: open own holds before=%d after=%d; holds=%s", before, after, states)
	if after != before {
		t.Fatalf("open own holds %d -> %d across Cancel; the plan expects Cancel to leave guarded holds open for the owner to discard", before, after)
	}
	if got := f.rhHoldRows(t, pred); got != predRows {
		t.Fatalf("Cancel changed the predecessor's custody rows")
	}
}
