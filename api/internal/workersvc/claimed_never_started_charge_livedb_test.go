package workersvc

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #3859: a retained-custody run whose first running report is refused by a SetRunRunning
// guard stays claimed; its flight ends without any report. These tests drive the whole loop
// through the real entry points (svc.Claim, SetStateReportWithReconciliation, Heartbeat with
// active snapshots, Sweep) and never inject the departure witness by hand, except in the
// fail-closed controls that need an inconsistent state to prove a conjunct.

// cncFixture is one owner with one registered, recovery-capable worker and a service whose
// RUN_MAX_REQUEUES is the test's own.
type cncFixture struct {
	*custodyContinuationFixture
	nonce   string
	counter int64 // the worker-side snapshot counter (the agent's epoch), advanced by next()
}

func newCNCFixture(t *testing.T, env codexTestEnv, maxRequeues int, tweak ...func(*Params)) *cncFixture {
	t.Helper()
	base := newCustodyContinuationFixture(t, env)
	p := testParams()
	p.RunMaxRequeues = maxRequeues
	for _, fn := range tweak {
		fn(&p)
	}
	svc := New(env.q, env.box, p)
	svc.SetVault(env.vlt)
	svc.SetTxBeginner(env.pool)
	base.svc = svc
	// The semantic-enrolled guard fixture below is only claimable by a worker that speaks the
	// semantic publication protocol.
	base.wkr.ProtocolCapabilities = append(base.wkr.ProtocolCapabilities, capability.CompletionSemanticPublicationV1)
	f := &cncFixture{custodyContinuationFixture: base}
	_, nonce, err := svc.Register(env.ctx, store.Worker{ID: f.workerID, UserID: f.userID}, "v1", "base", nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	f.nonce = nonce
	env.exec(`UPDATE workers SET protocol_capabilities = $2 WHERE id = $1`, f.workerID, base.wkr.ProtocolCapabilities)
	return f
}

// next advances the worker-side counter and returns the new value.
func (f *cncFixture) next() int64 {
	f.counter++
	return f.counter
}

// seedGuardedRun inserts a queued issue run whose first running report a real SetRunRunning
// guard refuses: a semantic_planning row exists while the run carries a plan but no semantic
// completion contract, so none of the guard's three alternatives holds.
func (f *cncFixture) seedGuardedRun(t *testing.T) uuid.UUID {
	t.Helper()
	run := f.seedRun("queued", 0)
	f.env.exec(`INSERT INTO semantic_planning(run_id, enrollment) VALUES ($1, '{"profile":"semantic","policy":{"version":1,"model":"sonnet","max_reviews":3,"max_invocations":2,"max_tokens":120000,"max_seconds":900}}')`, run)
	f.env.exec(`UPDATE runs SET plan_md = 'planned' WHERE id = $1`, run)
	return run
}

func (f *cncFixture) run(t *testing.T, id uuid.UUID) store.Run {
	t.Helper()
	r, err := f.env.q.GetRunByID(f.env.ctx, id)
	if err != nil {
		t.Fatalf("GetRunByID: %v", err)
	}
	return r
}

// backdate makes the claimed run older than the claim grace so the sweep considers it.
func (f *cncFixture) backdate(runID uuid.UUID) {
	f.env.exec(`UPDATE runs SET claimed_at = now() - interval '2 hours' WHERE id = $1`, runID)
}

// evidenceReport sends the first running report WITH registration evidence through the real
// service path. It returns the result; the guard refuses the status write, so Applied is false.
func (f *cncFixture) evidenceReport(t *testing.T, runID uuid.UUID, gen int64, nonce string, epoch int64) StateReportResult {
	t.Helper()
	g, e := gen, epoch
	res, err := f.svc.SetStateReportWithReconciliation(f.env.ctx, f.wkr, runID, StateRequest{
		State: "running", ClaimGeneration: &g,
		RegistrationEvidence: &RegistrationEvidence{RegisterNonce: nonce, ClaimGeneration: gen, RegistrationEpoch: &e},
	})
	if err != nil {
		t.Fatalf("running report: %v", err)
	}
	return res
}

// heartbeat sends a snapshot-carrying heartbeat and returns the worker's accepted epoch after it.
func (f *cncFixture) heartbeat(t *testing.T, epoch int64, nonce string, listed ...ActiveRunEntry) int64 {
	t.Helper()
	if listed == nil {
		listed = []ActiveRunEntry{}
	}
	_, err := f.svc.Heartbeat(f.env.ctx, store.Worker{ID: f.workerID, UserID: f.userID}, nil, nil,
		&ActiveSnapshot{SnapshotEpoch: epoch, RegisterNonce: nonce, Active: listed})
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	return f.acceptedEpoch(t)
}

func (f *cncFixture) acceptedEpoch(t *testing.T) int64 {
	t.Helper()
	var e int64
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT snapshot_epoch FROM workers WHERE id = $1`, f.workerID).Scan(&e); err != nil {
		t.Fatalf("read epoch: %v", err)
	}
	return e
}

// omit applies an accepted snapshot at a fresh epoch that does not list the flight.
func (f *cncFixture) omit(t *testing.T) int64 {
	t.Helper()
	e := f.next()
	if got := f.heartbeat(t, e, f.nonce); got != e {
		t.Fatalf("omitting snapshot at epoch %d was not accepted (worker epoch %d)", e, got)
	}
	return e
}

func (f *cncFixture) plainHeartbeat(t *testing.T) {
	t.Helper()
	if _, err := f.svc.Heartbeat(f.env.ctx, store.Worker{ID: f.workerID, UserID: f.userID}, nil, nil, nil); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
}

// sweep refreshes the worker's liveness (so the stale-worker passes leave it alone) and runs the
// real Sweep.
func (f *cncFixture) sweep(t *testing.T) SweepResult {
	t.Helper()
	f.plainHeartbeat(t)
	res, err := f.svc.Sweep(f.env.ctx)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	return res
}

// claimAt claims the run, asserting the generation, backdates claimed_at and returns it.
func (f *cncFixture) claimRun(t *testing.T, run uuid.UUID, wantGen int64) {
	t.Helper()
	got, gen := f.claim(t)
	if got != run || gen != wantGen {
		t.Fatalf("claim = (%s, gen %d), want (%s, gen %d)", got, gen, run, wantGen)
	}
	f.backdate(run)
}

type departure struct {
	gen, epoch pgtype.Int8
	nonce      pgtype.Text
}

func (f *cncFixture) departure(t *testing.T, run uuid.UUID) departure {
	t.Helper()
	r := f.run(t, run)
	return departure{gen: r.SnapshotDepartedGeneration, nonce: r.SnapshotDepartedNonce, epoch: r.SnapshotDepartedEpoch}
}

func (d departure) is(gen int64, nonce string, epoch int64) bool {
	return d.gen.Valid && d.gen.Int64 == gen && d.nonce.Valid && d.nonce.String == nonce && d.epoch.Valid && d.epoch.Int64 == epoch
}

func (f *cncFixture) assertQueued(t *testing.T, run uuid.UUID, wantCount int32, what string) {
	t.Helper()
	r := f.run(t, run)
	if r.Status != "queued" || r.RequeueCount != wantCount || r.ClaimReleasedAt.Valid {
		t.Fatalf("%s: status=%s requeue_count=%d claim_released=%v, want queued/%d/claim kept", what, r.Status, r.RequeueCount, r.ClaimReleasedAt.Valid, wantCount)
	}
}

// uncharged reports a reset that left the run queued WITHOUT a charge (the old behaviour).
func (f *cncFixture) assertUnchargedReset(t *testing.T, run uuid.UUID, count int32, what string) {
	t.Helper()
	f.assertQueued(t, run, count, what)
}

// parkAssert checks the full exhausted-arm contract.
func (f *cncFixture) assertParked(t *testing.T, run uuid.UUID, wantCount int32, capBefore []byte, epochBefore int64, holdsBefore int64) {
	t.Helper()
	r := f.run(t, run)
	if r.Status != "recovery_wait" || !r.RecoveryWaitCause.Valid || r.RecoveryWaitCause.String != "worker_requeue_exhausted" || r.FailOrigin.Valid || r.FinishedAt.Valid {
		t.Fatalf("status=%s cause=%v fail_origin=%v finished=%v, want recovery_wait/worker_requeue_exhausted, never failed", r.Status, r.RecoveryWaitCause, r.FailOrigin, r.FinishedAt.Valid)
	}
	if r.RequeueCount != wantCount {
		t.Fatalf("requeue_count = %d, want %d (the park is not a charge)", r.RequeueCount, wantCount)
	}
	if !r.ClaimReleasedAt.Valid || !r.ReleasedWorkerID.Valid || r.ReleasedWorkerID != pgconv.UUID(f.workerID) ||
		!r.ReleasedWorkerNonce.Valid || r.ReleasedWorkerNonce.String != f.nonce {
		t.Fatalf("claim release = %v released_worker=%v nonce=%v, want released to this worker and incarnation", r.ClaimReleasedAt.Valid, r.ReleasedWorkerID, r.ReleasedWorkerNonce)
	}
	if r.CodexCapHash != nil || r.CodexClaimEpoch != epochBefore+1 {
		t.Fatalf("codex cap=%v epoch=%d, want cap revoked and epoch %d", r.CodexCapHash, r.CodexClaimEpoch, epochBefore+1)
	}
	if !r.WorkerID.Valid || r.WorkerID != pgconv.UUID(f.workerID) {
		t.Fatalf("worker_id = %v, want kept", r.WorkerID)
	}
	var evidence store.WorkerRecoveryEvidence
	if err := json.Unmarshal(r.WorkerRecoveryEvidence, &evidence); err != nil || !evidence.CustodyUncertain {
		t.Fatalf("evidence = %s (%v), want custody_uncertain", r.WorkerRecoveryEvidence, err)
	}
	if r.RecoveryRetryNotBefore.Valid || r.StaleRequeueGeneration.Valid || r.MovePendingSince.Valid {
		t.Fatalf("retry/stale/move markers survived the park: %+v", r)
	}
	if got := f.openHolds(t); got != holdsBefore {
		t.Fatalf("open holds = %d, want %d (a park leaves custody untouched)", got, holdsBefore)
	}
}

// TestClaimedNeverStartedChargeShortLivedLoopLiveDB is the issue #3859 reproduction with no
// injected witness: each generation is claimed, its first running report (with registration
// evidence) is refused by a real guard, no snapshot ever lists the flight, and an accepted
// snapshot at a later epoch omits it. The sweep charges until the budget is spent, then parks.
func TestClaimedNeverStartedChargeShortLivedLoopLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	const max = 3
	f := newCNCFixture(t, env, max)
	run := f.seedGuardedRun(t)
	for gen := int64(1); gen <= max+2; gen++ {
		f.claimRun(t, run, gen)
		res := f.evidenceReport(t, run, gen, f.nonce, f.next())
		if res.Applied || res.RegistrationEvidence != RegistrationEvidenceRecorded {
			t.Fatalf("gen %d: applied=%v evidence=%q, want refused status write and recorded evidence", gen, res.Applied, res.RegistrationEvidence)
		}
		if st := f.run(t, run); st.Status != "claimed" || !st.RegistrationEvidenceGeneration.Valid || st.RegistrationEvidenceGeneration.Int64 != gen {
			t.Fatalf("gen %d: status=%s evidence gen=%v, want claimed with the evidence stored", gen, st.Status, st.RegistrationEvidenceGeneration)
		}
		e2 := f.omit(t)
		if d := f.departure(t, run); !d.is(gen, f.nonce, e2) {
			t.Fatalf("gen %d: departure = %+v, want (%d, nonce, %d)", gen, d, gen, e2)
		}
		capBefore := []byte("cap-" + uuid.NewString())
		f.env.exec(`UPDATE runs SET codex_cap_hash = $2 WHERE id = $1`, run, capBefore)
		epochBefore := f.run(t, run).CodexClaimEpoch
		holds := f.openHolds(t)
		sw := f.sweep(t)
		switch {
		case gen == 1:
			// A first-ever claim has no earlier hold: the old uncharged reset, untouched.
			f.assertUnchargedReset(t, run, 0, "first-ever claim")
			if sw.ClaimedCharged != 0 || sw.ClaimedExhausted != 0 || sw.ClaimedReset < 1 {
				t.Fatalf("gen 1 sweep = %+v, want an uncharged reset only", sw)
			}
		case gen <= max+1:
			f.assertQueued(t, run, int32(gen-1), "charged")
			// The charged arm revokes the Codex capability like FrozenRequeue: the flight that held
			// it is gone, so a stale capability must not stay replayable.
			if r := f.run(t, run); r.CodexCapHash != nil || r.CodexClaimEpoch != epochBefore+1 {
				t.Fatalf("gen %d: charged arm cap=%v epoch=%d, want the capability revoked and epoch %d", gen, r.CodexCapHash, r.CodexClaimEpoch, epochBefore+1)
			}
			if sw.ClaimedCharged < 1 {
				t.Fatalf("gen %d sweep = %+v, want a charge", gen, sw)
			}
		default:
			f.assertParked(t, run, max, capBefore, epochBefore, holds)
			if sw.ClaimedExhausted < 1 {
				t.Fatalf("park sweep = %+v, want ClaimedExhausted", sw)
			}
		}
	}
	if got, _ := f.claim(t); got != uuid.Nil {
		t.Fatalf("claimed %s after the park, want idle", got)
	}
}

// cycle runs one never-started generation up to (not including) the sweep: claim, the refused
// first running report WITH evidence, and an accepted snapshot that omits the flight.
func (f *cncFixture) cycle(t *testing.T, run uuid.UUID, gen int64) (e2 int64) {
	t.Helper()
	f.claimRun(t, run, gen)
	if res := f.evidenceReport(t, run, gen, f.nonce, f.next()); res.Applied || res.RegistrationEvidence != RegistrationEvidenceRecorded {
		t.Fatalf("gen %d: applied=%v evidence=%q, want refused write and recorded evidence", gen, res.Applied, res.RegistrationEvidence)
	}
	return f.omit(t)
}

// qualifying builds a run at generation 2 in the fully qualifying state, reaching it through
// the real flow (generation 1 is a first-ever claim: swept uncharged), then sets the episode
// count the test wants to probe. It returns the run and the omitting epoch.
func (f *cncFixture) qualifying(t *testing.T, count int32) (uuid.UUID, int64) {
	t.Helper()
	run := f.seedGuardedRun(t)
	f.cycle(t, run, 1)
	f.sweep(t)
	f.assertQueued(t, run, 0, "first-ever claim")
	e2 := f.cycle(t, run, 2)
	f.env.exec(`UPDATE runs SET requeue_count = $2 WHERE id = $1`, run, count)
	return run, e2
}

// charge calls the store statement directly (no other sweep pass) at an always-old cutoff.
func (f *cncFixture) charge(t *testing.T, max int32) []store.ChargeClaimedNeverStartedContinuationsRow {
	t.Helper()
	rows, err := f.env.q.ChargeClaimedNeverStartedContinuations(f.env.ctx, store.ChargeClaimedNeverStartedContinuationsParams{
		Cutoff: pgconv.Time(time.Now().Add(-time.Minute)), MaxRequeues: max,
	})
	if err != nil {
		t.Fatalf("ChargeClaimedNeverStartedContinuations: %v", err)
	}
	return rows
}

func (f *cncFixture) mine(rows []store.ChargeClaimedNeverStartedContinuationsRow, run uuid.UUID) (store.ChargeClaimedNeverStartedContinuationsRow, bool) {
	for _, r := range rows {
		if r.ID == run {
			return r, true
		}
	}
	return store.ChargeClaimedNeverStartedContinuationsRow{}, false
}

// TestClaimedNeverStartedChargeDelayedHeartbeatLiveDB: the snapshot the worker built at epoch R
// has not reached the api when the first running report (carrying R) does. The evidence is still
// recorded. The delayed snapshot at R, applied without the flight, must not stamp a departure
// (R is not before itself); a later accepted E2 > R does, and the sweep runs to the park.
func TestClaimedNeverStartedChargeDelayedHeartbeatLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	const max = 2
	f := newCNCFixture(t, env, max)
	run := f.seedGuardedRun(t)
	f.cycle(t, run, 1)
	f.sweep(t)
	for gen := int64(2); gen <= max+2; gen++ {
		f.claimRun(t, run, gen)
		accepted := f.acceptedEpoch(t)
		r := f.next()
		if r <= accepted {
			t.Fatalf("worker counter %d must run ahead of the accepted epoch %d", r, accepted)
		}
		if res := f.evidenceReport(t, run, gen, f.nonce, r); res.RegistrationEvidence != RegistrationEvidenceRecorded {
			t.Fatalf("gen %d: evidence = %q, want recorded although R=%d > accepted epoch %d", gen, res.RegistrationEvidence, r, accepted)
		}
		// The delayed snapshot built at R now arrives, without the flight.
		if got := f.heartbeat(t, r, f.nonce); got != r {
			t.Fatalf("delayed snapshot at %d not accepted (worker epoch %d)", r, got)
		}
		if d := f.departure(t, run); d.gen.Valid && d.gen.Int64 == gen {
			t.Fatalf("gen %d: departure %+v stamped by a snapshot at the evidence epoch", gen, d)
		}
		e2 := f.omit(t)
		if d := f.departure(t, run); !d.is(gen, f.nonce, e2) {
			t.Fatalf("gen %d: departure = %+v, want (%d, nonce, %d)", gen, d, gen, e2)
		}
		f.sweep(t)
		if gen <= max {
			f.assertQueued(t, run, int32(gen-1), "charged")
		}
	}
	r := f.run(t, run)
	if r.Status != "recovery_wait" || r.RecoveryWaitCause.String != "worker_requeue_exhausted" || r.RequeueCount != max {
		t.Fatalf("final status=%s cause=%v count=%d, want the charged exhaustion park at count %d", r.Status, r.RecoveryWaitCause, r.RequeueCount, max)
	}
}

// TestClaimedNeverStartedChargeListedWitnessLoopLiveDB: with no registration evidence at all, the
// earlier witnessed epoch (E1) is a LISTED snapshot of the flight; a later accepted snapshot that
// omits it stamps the departure and the loop is charged exactly as in the short-lived case.
func TestClaimedNeverStartedChargeListedWitnessLoopLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	const max = 2
	f := newCNCFixture(t, env, max)
	run := f.seedGuardedRun(t)
	for gen := int64(1); gen <= max+2; gen++ {
		f.claimRun(t, run, gen)
		listed := f.next()
		if got := f.heartbeat(t, listed, f.nonce, ActiveRunEntry{RunID: run.String(), ClaimGeneration: gen, Phase: "running"}); got != listed {
			t.Fatalf("listing snapshot at %d not accepted (%d)", listed, got)
		}
		if d := f.departure(t, run); d.gen.Valid && d.gen.Int64 == gen {
			t.Fatalf("gen %d: a snapshot LISTING the flight stamped a departure %+v", gen, d)
		}
		e2 := f.omit(t)
		if d := f.departure(t, run); !d.is(gen, f.nonce, e2) {
			t.Fatalf("gen %d: departure = %+v, want (%d, nonce, %d)", gen, d, gen, e2)
		}
		if r := f.run(t, run); r.RegistrationEvidenceGeneration.Valid {
			t.Fatalf("gen %d: registration evidence recorded without a report: %+v", gen, r.RegistrationEvidenceGeneration)
		}
		f.sweep(t)
		switch {
		case gen == 1:
			f.assertQueued(t, run, 0, "first-ever claim")
		case gen <= max+1:
			f.assertQueued(t, run, int32(gen-1), "charged")
		default:
			if r := f.run(t, run); r.Status != "recovery_wait" || r.RequeueCount != max {
				t.Fatalf("status=%s count=%d, want the park at count %d", r.Status, r.RequeueCount, max)
			}
		}
	}
}

// TestClaimedNeverStartedChargeBoundariesLiveDB pins the budget comparison: strictly below the
// cap charges, at the cap parks, and a zero budget parks the first qualifying run.
func TestClaimedNeverStartedChargeBoundariesLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	t.Run("max zero parks at once", func(t *testing.T) {
		f := newCNCFixture(t, env, 0)
		run, _ := f.qualifying(t, 0)
		capBefore := []byte("cap-" + uuid.NewString())
		f.env.exec(`UPDATE runs SET codex_cap_hash = $2 WHERE id = $1`, run, capBefore)
		epoch, holds := f.run(t, run).CodexClaimEpoch, f.openHolds(t)
		if res := f.sweep(t); res.ClaimedExhausted < 1 || res.ClaimedCharged != 0 {
			t.Fatalf("sweep = %+v, want an exhausted park", res)
		}
		f.assertParked(t, run, 0, capBefore, epoch, holds)
	})
	t.Run("count max-1 charges", func(t *testing.T) {
		f := newCNCFixture(t, env, 3)
		run, _ := f.qualifying(t, 2)
		f.sweep(t)
		f.assertQueued(t, run, 3, "count max-1")
	})
	t.Run("count max parks", func(t *testing.T) {
		f := newCNCFixture(t, env, 3)
		run, _ := f.qualifying(t, 3)
		capBefore := []byte("cap-" + uuid.NewString())
		f.env.exec(`UPDATE runs SET codex_cap_hash = $2 WHERE id = $1`, run, capBefore)
		epoch, holds := f.run(t, run).CodexClaimEpoch, f.openHolds(t)
		f.sweep(t)
		f.assertParked(t, run, 3, capBefore, epoch, holds)
	})
	t.Run("episode baseline offsets the count", func(t *testing.T) {
		f := newCNCFixture(t, env, 3)
		run, _ := f.qualifying(t, 5)
		f.env.exec(`UPDATE runs SET requeue_episode_baseline = 4 WHERE id = $1`, run)
		f.sweep(t)
		f.assertQueued(t, run, 6, "count 5 above baseline 4 charges")
	})
}

// TestClaimedNeverStartedChargeEvidenceRefusedLiveDB: registration evidence is recorded only for
// this worker's live claim under its current nonce; every other shape is refused, and an
// unapplied status write that carries no evidence records nothing.
func TestClaimedNeverStartedChargeEvidenceRefusedLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	cases := []struct {
		name  string
		mod   func(f *cncFixture, run uuid.UUID, gen int64) (worker store.Worker, nonce string, g int64)
		fatal bool
	}{
		{"wrong nonce", func(f *cncFixture, _ uuid.UUID, g int64) (store.Worker, string, int64) {
			return f.wkr, "not-the-nonce", g
		}, false},
		{"wrong generation", func(f *cncFixture, _ uuid.UUID, g int64) (store.Worker, string, int64) { return f.wkr, f.nonce, g + 1 }, false},
		{"released claim", func(f *cncFixture, run uuid.UUID, g int64) (store.Worker, string, int64) {
			f.env.exec(`UPDATE runs SET claim_released_at = now() WHERE id = $1`, run)
			return f.wkr, f.nonce, g
		}, false},
		{"another worker", func(f *cncFixture, _ uuid.UUID, g int64) (store.Worker, string, int64) {
			other := uuid.New()
			f.env.exec(`INSERT INTO workers (id, user_id, name, token_hash, status, snapshot_register_nonce) VALUES ($1, $2, $3, $4, 'online', $5)`,
				other, f.userID, "w-"+other.String(), other[:], f.nonce)
			return store.Worker{ID: other, UserID: f.userID}, f.nonce, g
		}, false},
		{"status outside claimed and running", func(f *cncFixture, run uuid.UUID, g int64) (store.Worker, string, int64) {
			f.env.exec(`UPDATE runs SET status = 'recovery_wait' WHERE id = $1`, run)
			return f.wkr, f.nonce, g
		}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newCNCFixture(t, env, 3)
			run := f.seedGuardedRun(t)
			f.claimRun(t, run, 1)
			worker, nonce, gen := c.mod(f, run, 1)
			epoch := f.next()
			sent := gen
			res, _ := f.svc.SetStateReportWithReconciliation(f.env.ctx, worker, run, StateRequest{
				State: "running", ClaimGeneration: &sent,
				RegistrationEvidence: &RegistrationEvidence{RegisterNonce: nonce, ClaimGeneration: gen, RegistrationEpoch: &epoch},
			})
			if res.RegistrationEvidence != RegistrationEvidenceRefused {
				t.Fatalf("evidence = %q, want refused", res.RegistrationEvidence)
			}
			if r := f.run(t, run); r.RegistrationEvidenceGeneration.Valid || r.RegistrationEvidenceNonce.Valid || r.RegistrationEvidenceEpoch.Valid {
				t.Fatalf("a refused report stored evidence: %+v", r)
			}
		})
	}
	t.Run("unapplied write without evidence records nothing", func(t *testing.T) {
		f := newCNCFixture(t, env, 3)
		run := f.seedGuardedRun(t)
		f.claimRun(t, run, 1)
		gen := int64(1)
		res, err := f.svc.SetStateReportWithReconciliation(f.env.ctx, f.wkr, run, StateRequest{State: "running", ClaimGeneration: &gen})
		if err != nil || res.Applied || res.RegistrationEvidence != "" {
			t.Fatalf("applied=%v evidence=%q err=%v, want a refused write with no evidence ACK", res.Applied, res.RegistrationEvidence, err)
		}
		if r := f.run(t, run); r.Status != "claimed" || r.RegistrationEvidenceGeneration.Valid {
			t.Fatalf("status=%s evidence=%v, want claimed and nothing recorded", r.Status, r.RegistrationEvidenceGeneration)
		}
	})
	t.Run("invalid objects are a 400 and write nothing", func(t *testing.T) {
		f := newCNCFixture(t, env, 3)
		run := f.seedGuardedRun(t)
		f.claimRun(t, run, 1)
		gen, neg, ok := int64(1), int64(-1), int64(0)
		for name, req := range map[string]StateRequest{
			"empty nonce":        {State: "running", RegistrationEvidence: &RegistrationEvidence{ClaimGeneration: 1, RegistrationEpoch: &ok}},
			"generation zero":    {State: "running", RegistrationEvidence: &RegistrationEvidence{RegisterNonce: f.nonce, ClaimGeneration: 0, RegistrationEpoch: &ok}},
			"negative epoch":     {State: "running", RegistrationEvidence: &RegistrationEvidence{RegisterNonce: f.nonce, ClaimGeneration: 1, RegistrationEpoch: &neg}},
			"missing epoch":      {State: "running", RegistrationEvidence: &RegistrationEvidence{RegisterNonce: f.nonce, ClaimGeneration: 1}},
			"not a running body": {State: "awaiting_approval", RegistrationEvidence: &RegistrationEvidence{RegisterNonce: f.nonce, ClaimGeneration: 1, RegistrationEpoch: &ok}},
		} {
			req.ClaimGeneration = &gen
			if _, err := f.svc.SetStateReportWithReconciliation(f.env.ctx, f.wkr, run, req); !errors.Is(err, ErrInvalidRegistrationEvidence) {
				t.Fatalf("%s: err = %v, want ErrInvalidRegistrationEvidence", name, err)
			}
		}
		if r := f.run(t, run); r.RegistrationEvidenceGeneration.Valid {
			t.Fatalf("an invalid object stored evidence")
		}
	})
}

// TestClaimedNeverStartedChargeRecordedBeforeDepartureLiveDB: evidence whose epoch R is ahead of
// the accepted snapshot epoch is still recorded (the epoch is deliberately not bounded), but no
// departure exists until an accepted E2 > R, and a snapshot at or below R stamps nothing.
func TestClaimedNeverStartedChargeRecordedBeforeDepartureLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	f := newCNCFixture(t, env, 3)
	run, _ := f.qualifying(t, 0)
	f.sweep(t) // the qualifying run is charged and requeued; re-claim for a clean probe
	f.assertQueued(t, run, 1, "charged")
	f.claimRun(t, run, 3)
	accepted := f.acceptedEpoch(t)
	r := accepted + 5
	f.counter = r
	if res := f.evidenceReport(t, run, 3, f.nonce, r); res.RegistrationEvidence != RegistrationEvidenceRecorded {
		t.Fatalf("evidence = %q, want recorded although R=%d is ahead of the accepted epoch %d", res.RegistrationEvidence, r, accepted)
	}
	for _, e := range []int64{accepted + 2, r} { // E2 <= R
		f.heartbeat(t, e, f.nonce)
		if d := f.departure(t, run); d.gen.Valid && d.gen.Int64 == 3 {
			t.Fatalf("snapshot at %d (<= R=%d) stamped %+v", e, r, d)
		}
	}
	f.counter = r + 1
	f.heartbeat(t, r+1, f.nonce)
	if d := f.departure(t, run); !d.is(3, f.nonce, r+1) {
		t.Fatalf("departure = %+v, want (3, nonce, %d)", d, r+1)
	}
}

// TestClaimedNeverStartedChargePreClaimSnapshotLiveDB is the causal regression: a snapshot built
// BEFORE the claim can never list the flight, so applying it late (after claimed_at plus an
// interval) proves nothing about the flight and must neither charge nor park.
func TestClaimedNeverStartedChargePreClaimSnapshotLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	const max = 3
	for _, order := range []string{"snapshot before evidence", "evidence before late snapshot"} {
		t.Run(order, func(t *testing.T) {
			f := newCNCFixture(t, env, max)
			run, _ := f.qualifying(t, max)
			// Re-do generation 2 as a fresh claim: requeue (keeping the claim) and claim again.
			f.env.exec(`UPDATE runs SET status = 'queued', worker_id = NULL WHERE id = $1`, run)
			pre := f.next() // built before the claim
			f.claimRun(t, run, 3)
			capBefore := []byte("cap-" + uuid.NewString())
			f.env.exec(`UPDATE runs SET codex_cap_hash = $2, requeue_count = $3 WHERE id = $1`, run, capBefore, max)
			if order == "snapshot before evidence" {
				f.heartbeat(t, pre, f.nonce)
				f.evidenceReport(t, run, 3, f.nonce, f.next())
			} else {
				f.counter = pre + 1
				f.evidenceReport(t, run, 3, f.nonce, pre+1)
				f.heartbeat(t, pre, f.nonce)
			}
			if d := f.departure(t, run); d.gen.Valid && d.gen.Int64 == 3 {
				t.Fatalf("a pre-claim snapshot stamped %+v", d)
			}
			f.sweep(t)
			// Presence under the current registration, no stamp, window long passed: the bounded
			// witness wait ends in the unwitnessed park, never a charge and never an exhaustion park.
			f.assertUnwitnessedPark(t, run, max, "pre-claim snapshot")
		})
	}
}

// armedAtMax claims the run's generation 2 (generation 1 having left the earlier open hold) at
// the episode cap, with a codex capability installed, and no witness of any kind yet.
func (f *cncFixture) armedAtMax(t *testing.T, max int32) (run uuid.UUID, capHash []byte) {
	t.Helper()
	run = f.seedGuardedRun(t)
	f.cycle(t, run, 1)
	f.sweep(t)
	f.claimRun(t, run, 2)
	capHash = []byte("cap-" + uuid.NewString())
	f.env.exec(`UPDATE runs SET requeue_count = $2, codex_cap_hash = $3 WHERE id = $1`, run, max, capHash)
	return run, capHash
}

// assertFailClosed: after the sweep the run was neither charged nor parked and kept its claim and
// capability (the old uncharged reset may still return it to queued).
func (f *cncFixture) assertFailClosed(t *testing.T, run uuid.UUID, max int32, capHash []byte, what string) {
	t.Helper()
	r := f.run(t, run)
	if r.Status == "recovery_wait" || r.RequeueCount != max || r.ClaimReleasedAt.Valid || string(r.CodexCapHash) != string(capHash) ||
		r.RecoveryWaitCause.Valid {
		t.Fatalf("%s: status=%s count=%d released=%v cap kept=%v cause=%v, want no charge, no park, claim and capability kept",
			what, r.Status, r.RequeueCount, r.ClaimReleasedAt.Valid, string(r.CodexCapHash) == string(capHash), r.RecoveryWaitCause)
	}
}

// TestClaimedNeverStartedChargeFailClosedControlsLiveDB: each way the departure evidence can be
// missing, forged by the wrong incarnation or never applied leaves a run AT THE CAP unparked.
func TestClaimedNeverStartedChargeFailClosedControlsLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	const max = 2
	run := func(t *testing.T) (*cncFixture, uuid.UUID, []byte) {
		f := newCNCFixture(t, env, max)
		id, capHash := f.armedAtMax(t, max)
		return f, id, capHash
	}
	t.Run("the flight is listed", func(t *testing.T) {
		f, id, _ := run(t)
		f.heartbeat(t, f.next(), f.nonce, ActiveRunEntry{RunID: id.String(), ClaimGeneration: 2, Phase: "running"})
		f.sweep(t)
		f.assertUnwitnessedPark(t, id, max, "listed")
	})
	t.Run("no snapshot ever applied", func(t *testing.T) {
		f, id, _ := run(t)
		f.evidenceReport(t, id, 2, f.nonce, f.next())
		f.sweep(t)
		f.assertUnwitnessedPark(t, id, max, "evidence only")
	})
	t.Run("only rejected snapshots", func(t *testing.T) {
		f, id, _ := run(t)
		f.evidenceReport(t, id, 2, f.nonce, f.next())
		accepted := f.acceptedEpoch(t)
		f.heartbeat(t, f.next()+100, "wrong-nonce") // rejected: another incarnation
		f.heartbeat(t, accepted, f.nonce)           // rejected: epoch not advancing
		f.heartbeat(t, accepted-1, f.nonce)         // rejected: older epoch
		if got := f.acceptedEpoch(t); got != accepted {
			t.Fatalf("a rejected snapshot advanced the epoch to %d", got)
		}
		if d := f.departure(t, id); d.gen.Valid && d.gen.Int64 == 2 {
			t.Fatalf("a rejected snapshot stamped %+v", d)
		}
		f.sweep(t)
		f.assertUnwitnessedPark(t, id, max, "rejected snapshots")
	})
	t.Run("snapshot-less heartbeats", func(t *testing.T) {
		f, id, _ := run(t)
		f.evidenceReport(t, id, 2, f.nonce, f.next())
		f.plainHeartbeat(t)
		f.plainHeartbeat(t)
		f.sweep(t)
		f.assertUnwitnessedPark(t, id, max, "snapshot-less heartbeats")
	})
	t.Run("unwitnessed: no evidence and never listed", func(t *testing.T) {
		f, id, capHash := run(t)
		f.omit(t)
		if d := f.departure(t, id); d.gen.Valid && d.gen.Int64 == 2 {
			t.Fatalf("an accepted snapshot with no earlier witness stamped %+v", d)
		}
		f.sweep(t)
		f.assertFailClosed(t, id, max, capHash, "unwitnessed")
	})
	// A real Service.Register would run its own orphan pass and park a claimed run that sits at the
	// cap, which would mask what these two controls prove. The re-registration is therefore the
	// pre-change register statement itself: rotate the nonce, reset the epoch, touch nothing else.
	rotate := func(f *cncFixture) string {
		nonce := "re-registered-" + uuid.NewString()
		f.env.exec(`UPDATE workers SET snapshot_register_nonce = $2, snapshot_epoch = 0 WHERE id = $1`, f.workerID, nonce)
		return nonce
	}
	t.Run("the nonce was rotated by a re-registration", func(t *testing.T) {
		f, id, capHash := run(t)
		f.evidenceReport(t, id, 2, f.nonce, f.next())
		f.omit(t)
		if d := f.departure(t, id); !d.is(2, f.nonce, f.counter) {
			t.Fatalf("precondition: departure = %+v", d)
		}
		rotate(f)
		f.sweep(t)
		f.assertFailClosed(t, id, max, capHash, "rotated nonce")
	})
	t.Run("a prior row survived a registration", func(t *testing.T) {
		f, id, capHash := run(t)
		f.heartbeat(t, f.next(), f.nonce, ActiveRunEntry{RunID: id.String(), ClaimGeneration: 2, Phase: "running"})
		priorEpoch := f.counter
		f.nonce, f.counter = rotate(f), priorEpoch+6 // the omitting epoch exceeds the surviving row's
		var rows int
		if err := f.env.pool.QueryRow(f.env.ctx, `SELECT count(*) FROM worker_active_runs WHERE run_id = $1`, id).Scan(&rows); err != nil || rows != 1 {
			t.Fatalf("precondition: surviving rows = %d (%v), want the prior row to survive the registration", rows, err)
		}
		f.omit(t) // accepted under the NEW nonce at an epoch above the reset one and above the row's
		if d := f.departure(t, id); d.gen.Valid && d.gen.Int64 == 2 {
			t.Fatalf("a row from before the registration served as E1: %+v", d)
		}
		f.sweep(t)
		f.assertFailClosed(t, id, max, capHash, "prior row survived a registration")
	})
}

// TestClaimedNeverStartedChargeMixedVersionsLiveDB: an old api reads and writes the same rows
// without knowing the witness columns. Each of its statements must leave the new api failing
// closed, and the new api's own witness must stay valid only while its nonce stays current.
func TestClaimedNeverStartedChargeMixedVersionsLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	const max = 2
	t.Run("old-api registration does not qualify", func(t *testing.T) {
		f := newCNCFixture(t, env, max)
		run, _ := f.qualifying(t, max)
		capHash := []byte("cap-" + uuid.NewString())
		f.env.exec(`UPDATE runs SET codex_cap_hash = $2 WHERE id = $1`, run, capHash)
		// The pre-change register statement: rotate the nonce, reset the epoch, witness columns untouched.
		f.env.exec(`UPDATE workers SET snapshot_register_nonce = $2, snapshot_epoch = 0 WHERE id = $1`, f.workerID, "old-api-"+uuid.NewString())
		f.sweep(t)
		f.assertFailClosed(t, run, max, capHash, "old-api registration")
	})
	t.Run("old-api snapshot application does not qualify", func(t *testing.T) {
		f := newCNCFixture(t, env, max)
		run, capHash := f.armedAtMax(t, max)
		r := f.next()
		f.evidenceReport(t, run, 2, f.nonce, r)
		// The pre-change replacement: drop the rows, advance the epoch, stamp nothing.
		f.env.exec(`DELETE FROM worker_active_runs WHERE worker_id = $1`, f.workerID)
		f.env.exec(`UPDATE workers SET snapshot_epoch = $2 WHERE id = $1`, f.workerID, r+3)
		f.sweep(t)
		f.assertFailClosed(t, run, max, capHash, "old-api snapshot application")
	})
	t.Run("new-api witness survives old-api applications under the same nonce", func(t *testing.T) {
		f := newCNCFixture(t, env, max)
		run, e2 := f.qualifying(t, max)
		f.env.exec(`DELETE FROM worker_active_runs WHERE worker_id = $1`, f.workerID)
		f.env.exec(`UPDATE workers SET snapshot_epoch = $2 WHERE id = $1`, f.workerID, e2+10)
		capBefore, epoch, holds := []byte("cap-"+uuid.NewString()), f.run(t, run).CodexClaimEpoch, f.openHolds(t)
		f.env.exec(`UPDATE runs SET codex_cap_hash = $2 WHERE id = $1`, run, capBefore)
		f.sweep(t)
		f.assertParked(t, run, max, capBefore, epoch, holds)
	})
	t.Run("an old worker's running report is accepted unchanged", func(t *testing.T) {
		f := newCNCFixture(t, env, max)
		run := f.seedRun("queued", 0)
		f.claimRun(t, run, 1)
		gen := int64(1)
		res, err := f.svc.SetStateReportWithReconciliation(f.env.ctx, f.wkr, run, StateRequest{State: "running", ClaimGeneration: &gen})
		if err != nil || !res.Applied || res.RegistrationEvidence != "" {
			t.Fatalf("applied=%v evidence=%q err=%v, want an applied report with no evidence ACK", res.Applied, res.RegistrationEvidence, err)
		}
		if r := f.run(t, run); r.Status != "running" || r.RegistrationEvidenceGeneration.Valid {
			t.Fatalf("status=%s evidence=%v", r.Status, r.RegistrationEvidenceGeneration)
		}
	})
}

// TestClaimedNeverStartedChargeNegativesLiveDB: the custody half of the predicate. Each shape
// leaves a perfectly witnessed run to the old uncharged reset.
func TestClaimedNeverStartedChargeNegativesLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	const max = 2
	check := func(t *testing.T, mutate func(f *cncFixture, run uuid.UUID)) {
		f := newCNCFixture(t, env, max)
		run, _ := f.qualifying(t, max)
		capHash := []byte("cap-" + uuid.NewString())
		f.env.exec(`UPDATE runs SET codex_cap_hash = $2 WHERE id = $1`, run, capHash)
		mutate(f, run)
		f.sweep(t)
		f.assertFailClosed(t, run, max, capHash, "negative")
	}
	dropOwnLower := func(f *cncFixture, run uuid.UUID) {
		f.env.exec(`DELETE FROM recovery_custody_holds WHERE run_id = $1 AND generation < 2`, run)
	}
	t.Run("same-generation-only hold", func(t *testing.T) {
		check(t, func(f *cncFixture, run uuid.UUID) { dropOwnLower(f, run) })
	})
	t.Run("open lower-generation hold on a sibling run of the same owner", func(t *testing.T) {
		check(t, func(f *cncFixture, run uuid.UUID) {
			dropOwnLower(f, run)
			f.seedHold(f.userID, f.seedRun("queued", 1), 1, "open")
		})
	})
	t.Run("foreign-owner hold", func(t *testing.T) {
		check(t, func(f *cncFixture, run uuid.UUID) {
			dropOwnLower(f, run)
			f.seedHold(f.env.seedUser(t), run, 1, "open")
		})
	})
	for _, state := range []string{"released", "discarded"} {
		t.Run(state+" earlier hold", func(t *testing.T) {
			check(t, func(f *cncFixture, run uuid.UUID) {
				f.env.exec(`UPDATE recovery_custody_holds SET state = $2 WHERE run_id = $1 AND generation < 2`, run, state)
			})
		})
	}
	t.Run("witness and evidence from an earlier generation", func(t *testing.T) {
		f := newCNCFixture(t, env, max)
		run, _ := f.qualifying(t, max) // witnessed at generation 2
		capHash := []byte("cap-" + uuid.NewString())
		f.env.exec(`UPDATE runs SET codex_cap_hash = $2 WHERE id = $1`, run, capHash)
		f.requeue(run)
		f.claimRun(t, run, 3) // a new flight: no evidence, no snapshot of its own
		f.env.exec(`UPDATE runs SET requeue_count = $2, codex_cap_hash = $3 WHERE id = $1`, run, max, capHash)
		f.sweep(t)
		f.assertFailClosed(t, run, max, capHash, "stale generation witness")
	})
}

// TestClaimedNeverStartedChargeProtectionsLiveDB: the D11 protections hold under BOTH statements.
func TestClaimedNeverStartedChargeProtectionsLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	const max = 2
	for _, name := range []string{"terminal_pending", "pending_overflow"} {
		t.Run(name, func(t *testing.T) {
			f := newCNCFixture(t, env, max)
			run, _ := f.qualifying(t, max)
			if name == "terminal_pending" {
				f.env.exec(`INSERT INTO worker_active_runs (worker_id, run_id, claim_generation, phase, terminal_pending, terminal_pending_until, snapshot_epoch, reported_at)
				            VALUES ($1, $2, 2, 'running', true, now() + interval '1 hour', 1, now())`, f.workerID, run)
			} else {
				f.env.exec(`UPDATE workers SET pending_overflow = true, pending_overflow_until = now() + interval '1 hour' WHERE id = $1`, f.workerID)
			}
			if rows := f.charge(t, max); len(rows) != 0 {
				if _, ok := f.mine(rows, run); ok {
					t.Fatalf("charge touched a protected run")
				}
			}
			if _, err := f.env.q.SweepClaimedNeverStarted(f.env.ctx, store.SweepClaimedNeverStartedParams{Cutoff: pgconv.Time(time.Now().Add(-time.Minute))}); err != nil {
				t.Fatalf("SweepClaimedNeverStarted: %v", err)
			}
			if r := f.run(t, run); r.Status != "claimed" || r.RequeueCount != max || r.ClaimReleasedAt.Valid {
				t.Fatalf("status=%s count=%d released=%v, want the protected run left claimed by both statements", r.Status, r.RequeueCount, r.ClaimReleasedAt.Valid)
			}
		})
	}
}

// TestClaimedNeverStartedChargeParkReleasesForgeReservationLiveDB: the park rides the same
// release triggers as the missing-flight park (migration 00392): the run's forge cooldown
// reservation is released with its claim.
func TestClaimedNeverStartedChargeParkReleasesForgeReservationLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	const max = 2
	f := newCNCFixture(t, env, max)
	run, _ := f.qualifying(t, max)
	incarnation := uuid.New()
	f.env.exec(`INSERT INTO forge_origin_cooldowns
	    (user_id, worker_id, origin, incarnation, cooled_until, last_reason, observed_at,
	     reservation_run_id, reservation_generation, reservation_incarnation, reservation_version, reservation_observation_version)
	    VALUES ($1, $2, 'https://forge.e2e', $3, now(), 'transient_clone_fetch_exhausted', now(), $4, 2, $3, 7, 1)`,
		f.userID, f.workerID, incarnation, run)
	// The claim stamped the run's origin mapping; bind it to the reservation as the claim would.
	f.env.exec(`UPDATE run_forge_origins SET reservation_incarnation = $2, reservation_version = 7 WHERE run_id = $1`, run, incarnation)
	reserved := func() bool {
		var valid bool
		if err := f.env.pool.QueryRow(f.env.ctx, `SELECT reservation_run_id IS NOT NULL FROM forge_origin_cooldowns WHERE user_id = $1 AND worker_id = $2 AND origin = 'https://forge.e2e'`,
			f.userID, f.workerID).Scan(&valid); err != nil {
			t.Fatalf("read reservation: %v", err)
		}
		return valid
	}
	if !reserved() {
		t.Fatal("precondition: the seeded reservation is not held")
	}
	f.sweep(t)
	if r := f.run(t, run); r.Status != "recovery_wait" {
		t.Fatalf("status = %s, want the park", r.Status)
	}
	if reserved() {
		t.Fatal("the park left the run's forge reservation held")
	}
}

// TestClaimedNeverStartedChargeCrossCheckLiveDB: a qualifying lead with a pending plan
// cross-check settles it exactly as the uncharged queued reset does (migrations 00312/00324).
func TestClaimedNeverStartedChargeCrossCheckLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	outcome := func(t *testing.T, qualifies bool, max int, count int32) (leadStatus, checkerStatus, verdict string) {
		f := newCNCFixture(t, env, max)
		caps := append([]string{}, f.wkr.ProtocolCapabilities...)
		caps = append(caps, "cross_check_v1")
		f.wkr.ProtocolCapabilities = caps
		f.env.exec(`UPDATE workers SET protocol_capabilities = $2 WHERE id = $1`, f.workerID, caps)
		run := f.seedGuardedRun(t)
		f.env.exec(`UPDATE runs SET plan_cross_check_required = true WHERE id = $1`, run)
		f.cycle(t, run, 1)
		f.sweep(t)
		f.claimRun(t, run, 2)
		if qualifies {
			f.evidenceReport(t, run, 2, f.nonce, f.next())
		}
		f.omit(t)
		f.env.exec(`UPDATE runs SET requeue_count = $2 WHERE id = $1`, run, count)
		checker, check := uuid.New(), uuid.New()
		f.env.exec(`INSERT INTO runs (id, user_id, repo_id, issue_title, issue_description, status, kind, trigger_source, harness, report_only, budget_wall_seconds, target_run_id)
		            VALUES ($1, $2, $3, 'checker', 'candidate', 'queued', 'cross_check', 'cross_check', 'codex', true, 300, $4)`, checker, f.userID, f.repoID, run)
		f.env.exec(`INSERT INTO cross_checks (id, lead_run_id, checker_run_id, stage, round, lead_claim_generation, plan_md, milestones, size_class, base_commit, candidate_digest, checker_harness, deadline_at)
		            VALUES ($1, $2, $3, 'plan', 1, 2, 'plan', '[]'::jsonb, 's', repeat('a', 40), $4, 'codex', now() + interval '5 minutes')`, check, run, checker, []byte("digest"))
		f.sweep(t)
		leadStatus = f.run(t, run).Status
		checkerStatus = f.run(t, checker).Status
		if err := env.pool.QueryRow(env.ctx, `SELECT verdict FROM cross_checks WHERE id = $1`, check).Scan(&verdict); err != nil {
			t.Fatalf("read verdict: %v", err)
		}
		return
	}
	t.Run("charged requeue settles like the queued path", func(t *testing.T) {
		wantLead, wantChecker, wantVerdict := outcome(t, false, 3, 0)
		gotLead, gotChecker, gotVerdict := outcome(t, true, 3, 0)
		if wantLead != "queued" || gotLead != "queued" || gotChecker != wantChecker || gotVerdict != wantVerdict {
			t.Fatalf("qualifying lead=%s checker=%s verdict=%s; queued path lead=%s checker=%s verdict=%s", gotLead, gotChecker, gotVerdict, wantLead, wantChecker, wantVerdict)
		}
		if gotVerdict == "pending" {
			t.Fatalf("the plan check was left pending")
		}
	})
	t.Run("the park settles the plan check", func(t *testing.T) {
		lead, checker, verdict := outcome(t, true, 2, 2)
		if lead != "recovery_wait" || verdict == "pending" || checker == "queued" {
			t.Fatalf("lead=%s checker=%s verdict=%s, want a parked lead and a settled check", lead, checker, verdict)
		}
	})
}

// TestClaimedNeverStartedChargeAfterParkLiveDB: what the owner and a straggler can do once the
// run is parked.
func TestClaimedNeverStartedChargeAfterParkLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	const max = 2
	park := func(t *testing.T) (*cncFixture, uuid.UUID, int64) {
		f := newCNCFixture(t, env, max)
		run, _ := f.qualifying(t, max)
		f.sweep(t)
		if r := f.run(t, run); r.Status != "recovery_wait" {
			t.Fatalf("status = %s, want parked", r.Status)
		}
		return f, run, f.openHolds(t)
	}
	t.Run("a late running write affects no row", func(t *testing.T) {
		f, run, _ := park(t)
		rows, err := f.env.q.SetRunRunning(f.env.ctx, store.SetRunRunningParams{ID: run, WorkerID: pgconv.UUID(f.workerID)})
		if err != nil || rows != 0 {
			t.Fatalf("SetRunRunning rows=%d err=%v, want 0", rows, err)
		}
		gen := int64(2)
		res, err := f.svc.SetStateReportWithReconciliation(f.env.ctx, f.wkr, run, StateRequest{State: "running", ClaimGeneration: &gen})
		if res.Applied || (err != nil && !errors.Is(err, ErrStaleClaim)) {
			t.Fatalf("service running report applied=%v err=%v, want it refused", res.Applied, err)
		}
		if r := f.run(t, run); r.Status != "recovery_wait" {
			t.Fatalf("status = %s after the late report", r.Status)
		}
	})
	t.Run("cancel keeps the holds open", func(t *testing.T) {
		f, run, holds := park(t)
		if _, err := f.svc.SubmitInput(f.env.ctx, f.userID, run, "cancel", "owner cancel", nil); err != nil {
			t.Fatalf("cancel: %v", err)
		}
		if r := f.run(t, run); r.Status != "cancelled" {
			t.Fatalf("status = %s, want cancelled", r.Status)
		}
		if got := f.openHolds(t); got != holds {
			t.Fatalf("open holds = %d after cancel, want %d (custody outlives the run)", got, holds)
		}
	})
	t.Run("resume re-baselines and a new cycle follows", func(t *testing.T) {
		f, run, _ := park(t)
		if _, err := f.env.q.ResumeWorkerRecoveryEpisode(f.env.ctx, store.ResumeWorkerRecoveryEpisodeParams{ID: run, UserID: f.userID, GlobalTimeoutSeconds: 3600}); err != nil {
			t.Fatalf("resume: %v", err)
		}
		if r := f.run(t, run); r.Status != "queued" || r.RequeueEpisodeBaseline != max || r.RequeueCount != max {
			t.Fatalf("status=%s baseline=%d count=%d, want queued with the episode re-baselined to %d", r.Status, r.RequeueEpisodeBaseline, r.RequeueCount, max)
		}
		// The released incarnation may not re-claim; the worker restarts (a new nonce) first.
		_, nonce, err := f.svc.Register(f.env.ctx, store.Worker{ID: f.workerID, UserID: f.userID}, "v1", "base", nil, nil, nil, nil)
		if err != nil {
			t.Fatalf("re-register: %v", err)
		}
		f.env.exec(`UPDATE workers SET protocol_capabilities = $2 WHERE id = $1`, f.workerID, f.wkr.ProtocolCapabilities)
		f.nonce, f.counter = nonce, 0
		f.cycle(t, run, 3)
		f.sweep(t)
		f.assertQueued(t, run, max+1, "first charge of the new episode")
	})
}

// TestClaimedNeverStartedChargeCustodyCapFirstLiveDB: the per-run custody bound still ends the
// loop first when it is lower than the requeue budget; the run waits queued and health says why.
func TestClaimedNeverStartedChargeCustodyCapFirstLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	f := newCNCFixture(t, env, 3, func(p *Params) { p.CustodyHoldLimit = &fakeCustodyLimit{n: 2} })
	run := f.seedGuardedRun(t)
	f.cycle(t, run, 1)
	f.sweep(t)
	f.cycle(t, run, 2)
	f.sweep(t)
	f.assertQueued(t, run, 1, "charged once")
	holds := f.ownOpenHolds(t, run)
	if got, _ := f.claim(t); got != uuid.Nil {
		t.Fatalf("claimed %s with %d own open holds at a limit of 2, want idle", got, holds)
	}
	if got := f.ownOpenHolds(t, run); got != holds || got != 2 {
		t.Fatalf("own open holds = %d, want %d unchanged and exactly 2", got, holds)
	}
	f.assertQueued(t, run, 1, "waiting on the custody cap")
	row := store.ListActiveRunsForHealthRow{ID: run, UserID: f.userID, Status: "queued", Kind: "issue"}
	if got := f.svc.queuedReason(env.ctx, time.Now(), row, thWithLimit(2)); got != reasonCustodyLimit {
		t.Fatalf("health reason = %q, want %q", got, reasonCustodyLimit)
	}
}

// TestClaimedNeverStartedChargeRunningPathLiveDB: the existing running-flight loss path is
// untouched: a run that did report running and then vanishes from the snapshot is requeued
// through the charged FrozenRequeueRunsMissingFromSnapshot, and parked once over the cap.
func TestClaimedNeverStartedChargeRunningPathLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	const max = 2
	f := newCNCFixture(t, env, max)
	run := f.seedRun("queued", 0)
	for gen := int64(1); gen <= max+1; gen++ {
		f.claimRun(t, run, gen)
		g := gen
		if res, err := f.svc.SetStateReportWithReconciliation(f.env.ctx, f.wkr, run, StateRequest{State: "running", ClaimGeneration: &g}); err != nil || !res.Applied {
			t.Fatalf("gen %d: running applied=%v err=%v", gen, res.Applied, err)
		}
		f.heartbeat(t, f.next(), f.nonce, ActiveRunEntry{RunID: run.String(), ClaimGeneration: gen, Phase: "running"})
		f.env.exec(`UPDATE runs SET status_since = now() - interval '2 hours' WHERE id = $1`, run)
		f.omit(t) // the reconciliation inside this heartbeat is the missing-flight path
		r := f.run(t, run)
		if gen <= max {
			if r.Status != "queued" || r.RequeueCount != int32(gen) {
				t.Fatalf("gen %d: status=%s count=%d, want the charged requeue", gen, r.Status, r.RequeueCount)
			}
			continue
		}
		if r.Status != "recovery_wait" || r.RecoveryWaitCause.String != "worker_requeue_exhausted" || r.FailOrigin.Valid || !r.ClaimReleasedAt.Valid {
			t.Fatalf("gen %d: status=%s cause=%v fail_origin=%v released=%v, want the over-cap park", gen, r.Status, r.RecoveryWaitCause, r.FailOrigin, r.ClaimReleasedAt.Valid)
		}
	}
}

// holdRows lists the run's custody holds as "generation/state", ordered, so a test can compare
// the exact set before and after a step.
func (f *cncFixture) holdRows(t *testing.T, run uuid.UUID) []string {
	t.Helper()
	rows, err := f.env.pool.Query(f.env.ctx,
		`SELECT generation, state FROM recovery_custody_holds WHERE user_id = $1 AND run_id = $2 ORDER BY generation, id`, f.userID, run)
	if err != nil {
		t.Fatalf("list holds: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var gen int64
		var state string
		if err := rows.Scan(&gen, &state); err != nil {
			t.Fatalf("scan hold: %v", err)
		}
		out = append(out, fmt.Sprintf("%d/%s", gen, state))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("hold rows: %v", err)
	}
	return out
}

// TestClaimedNeverStartedChargeRetainedCustodyRunningPathLiveDB (issue #3644, plan 3.5 running
// path): a run holding a retained OPEN generation-1 recovery custody hold reaches running on a
// worker, whose active-run snapshot then omits it (the worker refused the restore nonterminally
// and dropped it). The existing server paths run unchanged: FrozenRequeueRunsMissingFromSnapshot
// charges a requeue below RUN_MAX_REQUEUES, and at the cap frozenFailRunsMissingFromSnapshotLocked
// parks the run at recovery_wait / worker_requeue_exhausted with custody_uncertain=true. Neither
// path touches the holds. SetRunRunning has no custody guard, so the running report is applied.
// Every subtest also seeds testCustodyLimit-1 unrelated open holds for the owner. The retained
// subtests (which seed the predecessor hold too) therefore sit at custody_hold_limit; the hold-free
// control sits one below it. The run still claims, which is all that is pinned about the limit.
//
// svc.Claim opens one hold per claimed generation, so three subtests isolate which hold matters:
//   - "retained predecessor hold" keeps every hold: it ends in the park, custody_uncertain=true.
//   - "only the generation-1 hold" deletes the holds Claim opens for generations above 1 right
//     after each claim, leaving only the retained generation-1 hold; it must still park with
//     custody_uncertain=true, so the predecessor-generation hold alone is sufficient (a
//     custody_uncertain EXISTS narrowed to the run's current claim_generation reddens it).
//   - "hold-free control" deletes every hold of the run after each claim: the same sequence ends
//     failed/worker_lost (no park) with custody_uncertain=false.
func TestClaimedNeverStartedChargeRetainedCustodyRunningPathLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	const max = 2

	// holdMode says which of the run's holds are deleted right after each claim.
	type holdMode int
	const (
		keepAll holdMode = iota
		dropAbove1
		dropAll
	)

	runCycle := func(t *testing.T, f *cncFixture, run uuid.UUID, wantGen int64, wantCount int32, mode holdMode) store.Run {
		t.Helper()
		f.claimRun(t, run, wantGen)
		switch mode {
		case dropAll:
			f.env.exec(`DELETE FROM recovery_custody_holds WHERE user_id = $1 AND run_id = $2`, f.userID, run)
		case dropAbove1:
			f.env.exec(`DELETE FROM recovery_custody_holds WHERE user_id = $1 AND run_id = $2 AND generation > 1`, f.userID, run)
		}
		g := wantGen
		res, err := f.svc.SetStateReportWithReconciliation(f.env.ctx, f.wkr, run, StateRequest{State: "running", ClaimGeneration: &g})
		if err != nil || !res.Applied {
			t.Fatalf("gen %d: running applied=%v err=%v (SetRunRunning has no custody guard)", wantGen, res.Applied, err)
		}
		if r := f.run(t, run); r.Status != "running" || r.RequeueCount != wantCount {
			t.Fatalf("gen %d: after running report status=%s requeue_count=%d, want running/%d", wantGen, r.Status, r.RequeueCount, wantCount)
		}
		f.heartbeat(t, f.next(), f.nonce, ActiveRunEntry{RunID: run.String(), ClaimGeneration: wantGen, Phase: "running"})
		f.env.exec(`UPDATE runs SET status_since = now() - interval '2 hours' WHERE id = $1`, run)
		holdsBefore := f.holdRows(t, run)
		f.omit(t)
		if got := f.holdRows(t, run); !slices.Equal(got, holdsBefore) {
			t.Fatalf("gen %d: holds = %v after the omitting snapshot, want %v unchanged", wantGen, got, holdsBefore)
		}
		return f.run(t, run)
	}
	evidence := func(t *testing.T, r store.Run) store.WorkerRecoveryEvidence {
		t.Helper()
		var ev store.WorkerRecoveryEvidence
		if err := json.Unmarshal(r.WorkerRecoveryEvidence, &ev); err != nil {
			t.Fatalf("evidence = %s (%v)", r.WorkerRecoveryEvidence, err)
		}
		return ev
	}

	// drive runs the whole sequence from a run seeded at claim_generation 1 (generation 1 is the
	// retained predecessor; the first claim here is generation 2) and returns the parked run.
	drive := func(t *testing.T, f *cncFixture, run uuid.UUID, mode holdMode) store.Run {
		t.Helper()
		for step := int32(0); step <= max; step++ {
			r := runCycle(t, f, run, int64(step)+2, step, mode)
			held := f.holdRows(t, run)
			switch {
			case mode == dropAll && len(held) != 0:
				t.Fatalf("step %d: control holds = %v, want none", step, held)
			case mode == dropAbove1 && !slices.Equal(held, []string{"1/open"}):
				t.Fatalf("step %d: holds = %v, want only the retained 1/open", step, held)
			case mode == keepAll && (len(held) != int(step)+2 || held[0] != "1/open"):
				t.Fatalf("step %d: holds = %v, want the retained 1/open plus one per claimed generation", step, held)
			}
			for _, h := range held {
				if !strings.HasSuffix(h, "/open") {
					t.Fatalf("step %d: hold %q is not open", step, h)
				}
			}
			if step < max {
				if r.Status != "queued" || r.RequeueCount != step+1 || r.RecoveryWaitCause.Valid || r.FailOrigin.Valid {
					t.Fatalf("step %d: status=%s requeue_count=%d cause=%v fail_origin=%v, want the charged requeue %d", step, r.Status, r.RequeueCount, r.RecoveryWaitCause, r.FailOrigin, step+1)
				}
				continue
			}
			if mode == dropAll {
				// Current code: with no custody evidence the over-cap run is failed, not parked.
				if r.Status != "failed" || !r.FailOrigin.Valid || r.FailOrigin.String != "worker_lost" || !r.FinishedAt.Valid || r.RecoveryWaitCause.Valid {
					t.Fatalf("step %d: control status=%s fail_origin=%v finished=%v cause=%v, want failed/worker_lost", step, r.Status, r.FailOrigin, r.FinishedAt.Valid, r.RecoveryWaitCause)
				}
				return r
			}
			if r.Status != "recovery_wait" || !r.RecoveryWaitCause.Valid || r.RecoveryWaitCause.String != "worker_requeue_exhausted" ||
				r.FailOrigin.Valid || r.FinishedAt.Valid || !r.ClaimReleasedAt.Valid {
				t.Fatalf("step %d: status=%s cause=%v fail_origin=%v finished=%v released=%v, want the over-cap park, never failed", step, r.Status, r.RecoveryWaitCause, r.FailOrigin, r.FinishedAt.Valid, r.ClaimReleasedAt.Valid)
			}
			if r.RequeueCount != max {
				t.Fatalf("step %d: requeue_count = %d, want %d (the park is not a charge)", step, r.RequeueCount, max)
			}
			return r
		}
		t.Fatal("unreachable")
		return store.Run{}
	}

	seeded := func(t *testing.T, retained bool) (*cncFixture, uuid.UUID) {
		t.Helper()
		f := newCNCFixture(t, env, max)
		run := f.seedRun("queued", 1)
		if retained {
			f.seedHold(f.userID, run, 1, "open")
		}
		f.seedUnrelatedOpenHolds(testCustodyLimit - 1) // retained: owner at custody_hold_limit; hold-free control: one below
		return f, run
	}
	parks := func(t *testing.T, mode holdMode) {
		f, run := seeded(t, true)
		if got := f.holdRows(t, run); !slices.Equal(got, []string{"1/open"}) {
			t.Fatalf("seeded holds = %v, want [1/open]", got)
		}
		r := drive(t, f, run, mode)
		if !evidence(t, r).CustodyUncertain {
			t.Fatalf("evidence = %s, want custody_uncertain=true for a run holding open custody", r.WorkerRecoveryEvidence)
		}
	}

	t.Run("retained predecessor hold: charged requeues then custody-uncertain park", func(t *testing.T) {
		parks(t, keepAll)
	})

	t.Run("only the generation-1 hold: still the custody-uncertain park", func(t *testing.T) {
		parks(t, dropAbove1)
	})

	t.Run("hold-free control: same sequence fails with custody_uncertain false", func(t *testing.T) {
		f, run := seeded(t, false)
		r := drive(t, f, run, dropAll)
		if evidence(t, r).CustodyUncertain {
			t.Fatalf("evidence = %s, want custody_uncertain=false without holds", r.WorkerRecoveryEvidence)
		}
	})
}
