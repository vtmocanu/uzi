package workersvc

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #3859, bounded witness wait. A retained-custody continuation (an open hold of an EARLIER
// generation) whose first running report was refused stays claimed. While its worker is online
// and shows presence under the current registration the sweep DEFERS it; a departure stamp makes
// it a charge; the window passing (twice the claim grace) parks it unwitnessed. Everything runs
// through the real Service entry points; the window is crossed by backdating claimed_at.

const (
	cnwGrace      = time.Minute
	cnwInWindow   = 90 * time.Second // past the claim grace, inside twice the grace
	cnwPastWindow = 10 * time.Minute // past twice the claim grace
)

func newWitnessFixture(t *testing.T, env codexTestEnv, maxRequeues int) *cncFixture {
	t.Helper()
	return newCNCFixture(t, env, maxRequeues, func(p *Params) { p.ClaimGrace = cnwGrace })
}

func (f *cncFixture) ageClaim(run uuid.UUID, age time.Duration) {
	f.env.exec(`UPDATE runs SET claimed_at = now() - make_interval(secs => $2) WHERE id = $1`, run, age.Seconds())
}

// claimAged claims the run, asserts the generation and sets claimed_at to now minus age.
func (f *cncFixture) claimAged(t *testing.T, run uuid.UUID, wantGen int64, age time.Duration) {
	t.Helper()
	got, gen := f.claim(t)
	if got != run || gen != wantGen {
		t.Fatalf("claim = (%s, gen %d), want (%s, gen %d)", got, gen, run, wantGen)
	}
	f.ageClaim(run, age)
}

// firstEver puts a fresh guarded run through generation 1: claimed, past the window, swept to
// queued uncharged (its only hold is the current generation's, so it is not a continuation).
// The next claim is generation 2 with an open hold of generation 1.
func (f *cncFixture) firstEver(t *testing.T, run uuid.UUID) {
	t.Helper()
	f.claimAged(t, run, 1, cnwPastWindow)
	f.evidenceReport(t, run, 1, f.nonce, f.next())
	f.sweep(t)
	f.assertQueued(t, run, 0, "first-ever claim")
}

// continuation returns a run queued at generation 1 with an earlier open hold.
func (f *cncFixture) continuation(t *testing.T) uuid.UUID {
	t.Helper()
	run := f.seedGuardedRun(t)
	f.firstEver(t, run)
	return run
}

func (f *cncFixture) listEntry(run uuid.UUID, gen int64) ActiveRunEntry {
	return ActiveRunEntry{RunID: run.String(), ClaimGeneration: gen, Phase: "running"}
}

func (f *cncFixture) assertDeferred(t *testing.T, run uuid.UUID, count int32, sw SweepResult, what string) {
	t.Helper()
	r := f.run(t, run)
	if r.Status != "claimed" || r.RequeueCount != count || r.ClaimReleasedAt.Valid || r.RecoveryWaitCause.Valid {
		t.Fatalf("%s: status=%s count=%d released=%v cause=%v, want deferred (claimed, count %d, claim kept)", what, r.Status, r.RequeueCount, r.ClaimReleasedAt.Valid, r.RecoveryWaitCause, count)
	}
	if sw.ClaimedReset != 0 || sw.ClaimedCharged != 0 || sw.ClaimedExhausted != 0 || sw.ClaimedUnwitnessed != 0 {
		t.Fatalf("%s: sweep = %+v, want the run deferred and nothing else touched", what, sw)
	}
}

// assertUnwitnessedPark checks the park's contract (not the byte-unchanged custody rows).
func (f *cncFixture) assertUnwitnessedPark(t *testing.T, run uuid.UUID, wantCount int32, what string) map[string]json.RawMessage {
	t.Helper()
	r := f.run(t, run)
	if r.Status != "recovery_wait" || !r.RecoveryWaitCause.Valid || r.RecoveryWaitCause.String != "retained_continuation_unwitnessed" ||
		r.FailOrigin.Valid || r.FinishedAt.Valid || r.FailureReason.Valid {
		t.Fatalf("%s: status=%s cause=%v fail_origin=%v finished=%v, want recovery_wait/retained_continuation_unwitnessed, never failed", what, r.Status, r.RecoveryWaitCause, r.FailOrigin, r.FinishedAt.Valid)
	}
	if r.RequeueCount != wantCount {
		t.Fatalf("%s: requeue_count = %d, want %d (the unwitnessed park is not a charge)", what, r.RequeueCount, wantCount)
	}
	if !r.ClaimReleasedAt.Valid || r.ReleasedWorkerID != pgconv.UUID(f.workerID) || !r.ReleasedWorkerNonce.Valid || r.ReleasedWorkerNonce.String != f.nonce {
		t.Fatalf("%s: claim release = %v worker=%v nonce=%v, want released to this worker and incarnation", what, r.ClaimReleasedAt.Valid, r.ReleasedWorkerID, r.ReleasedWorkerNonce)
	}
	if r.CodexCapHash != nil {
		t.Fatalf("%s: codex capability %v survived the park", what, r.CodexCapHash)
	}
	if r.RecoveryRetryNotBefore.Valid || r.StaleRequeueGeneration.Valid {
		t.Fatalf("%s: retry markers survived the park: %v %v", what, r.RecoveryRetryNotBefore, r.StaleRequeueGeneration)
	}
	if r.Health != "ok" || r.HealthReason.Valid || r.HealthSince.Valid {
		t.Fatalf("%s: health = %q/%v/%v, want ok and clear", what, r.Health, r.HealthReason, r.HealthSince)
	}
	if r.SnapshotDepartedGeneration.Valid && r.SnapshotDepartedGeneration.Int64 == r.ClaimGeneration {
		t.Fatalf("%s: the park wrote a departure stamp", what)
	}
	if r.MilestonesInProgress != nil || r.PauseRequestedAt.Valid || r.CredentialSwitchRequestedAt.Valid || r.PlanCrossCheckGateReason.Valid {
		t.Fatalf("%s: pause/credential/milestone/plan-check fields survived the park", what)
	}
	if !r.WorkerID.Valid || r.WorkerID != pgconv.UUID(f.workerID) {
		t.Fatalf("%s: worker_id = %v, want kept", what, r.WorkerID)
	}
	var ev map[string]json.RawMessage
	if err := json.Unmarshal(r.WorkerRecoveryEvidence, &ev); err != nil {
		t.Fatalf("%s: evidence %s: %v", what, r.WorkerRecoveryEvidence, err)
	}
	var presence string
	if err := json.Unmarshal(ev["presence"], &presence); err != nil || (presence != "p1" && presence != "p2") {
		t.Fatalf("%s: evidence presence = %s, want p1 or p2", what, ev["presence"])
	}
	var deadline time.Time
	if err := json.Unmarshal(ev["deadline"], &deadline); err != nil || deadline.Sub(r.ClaimedAt.Time.Add(2*f.svc.p.ClaimGrace)).Abs() > time.Millisecond {
		t.Fatalf("%s: evidence deadline = %s (%v), want claimed_at + 2x grace = %s", what, ev["deadline"], err, r.ClaimedAt.Time.Add(2*f.svc.p.ClaimGrace))
	}
	for _, key := range []string{"registration_evidence_epoch", "worker_snapshot_epoch", "custody_uncertain", "recorded_at"} {
		if _, ok := ev[key]; !ok {
			t.Fatalf("%s: evidence lacks %q: %s", what, key, r.WorkerRecoveryEvidence)
		}
	}
	return ev
}

// custodyRows snapshots, as text, every custody/source/journal row this run owns.
func (f *cncFixture) custodyRows(t *testing.T, run uuid.UUID) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, table := range []string{"recovery_custody_holds", "recovery_captures", "run_messages", "agent_source_staged", "run_forge_origins"} {
		var has bool
		if err := f.env.pool.QueryRow(f.env.ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema = 'public' AND table_name = $1 AND column_name = 'run_id')`, table).Scan(&has); err != nil {
			t.Fatalf("probe %s: %v", table, err)
		}
		if !has {
			continue
		}
		var rows string
		q := fmt.Sprintf(`SELECT COALESCE(jsonb_agg(to_jsonb(t) ORDER BY to_jsonb(t)::text), '[]'::jsonb)::text FROM %s t WHERE t.run_id = $1`, table)
		if err := f.env.pool.QueryRow(f.env.ctx, q, run).Scan(&rows); err != nil {
			t.Fatalf("snapshot %s: %v", table, err)
		}
		out[table] = rows
	}
	if out["recovery_custody_holds"] == "" || out["recovery_custody_holds"] == "[]" {
		t.Fatalf("precondition: run %s has no custody hold rows", run)
	}
	return out
}

func (f *cncFixture) rowJSON(t *testing.T, run uuid.UUID) map[string]any {
	t.Helper()
	var raw string
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT to_jsonb(r)::text FROM runs r WHERE r.id = $1`, run).Scan(&raw); err != nil {
		t.Fatalf("row json: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("row json decode: %v", err)
	}
	return m
}

// sameExcept fails unless both rows agree on every column except the named ones.
func sameExcept(t *testing.T, what string, before, after map[string]any, skip ...string) {
	t.Helper()
	skipped := map[string]bool{}
	for _, k := range skip {
		skipped[k] = true
	}
	for k, b := range before {
		if skipped[k] {
			continue
		}
		if fmt.Sprint(b) != fmt.Sprint(after[k]) {
			t.Fatalf("%s: column %s changed: %v -> %v", what, k, b, after[k])
		}
	}
}

// resetColumns are the only columns the uncharged reset writes.
var resetColumns = []string{"status", "status_since", "updated_at", "health", "health_reason", "health_since"}

// TestClaimedNeverStartedWitnessStampRacingSweepLiveDB (i): a departure stamp racing the sweep
// always ends in exactly one charge, never an uncharged reset and never a park.
func TestClaimedNeverStartedWitnessStampRacingSweepLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	for round := 0; round < 6; round++ {
		f := newWitnessFixture(t, env, 5)
		run := f.continuation(t)
		f.claimAged(t, run, 2, cnwInWindow)
		f.evidenceReport(t, run, 2, f.nonce, f.next())
		f.plainHeartbeat(t)
		epoch := f.next()
		var wg sync.WaitGroup
		var hbErr, sweepErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, hbErr = f.svc.Heartbeat(f.env.ctx, store.Worker{ID: f.workerID, UserID: f.userID}, nil, nil,
				&ActiveSnapshot{SnapshotEpoch: epoch, RegisterNonce: f.nonce, Active: []ActiveRunEntry{}})
		}()
		go func() {
			defer wg.Done()
			_, sweepErr = f.svc.Sweep(f.env.ctx)
		}()
		wg.Wait()
		if hbErr != nil || sweepErr != nil {
			t.Fatalf("round %d: heartbeat=%v sweep=%v", round, hbErr, sweepErr)
		}
		f.sweep(t)
		f.assertQueued(t, run, 1, fmt.Sprintf("round %d: racing stamp", round))
		if d := f.departure(t, run); !d.is(2, f.nonce, epoch) {
			t.Fatalf("round %d: departure = %+v, want the stamp at epoch %d", round, d, epoch)
		}
	}
}

// TestClaimedNeverStartedWitnessDeferralLiveDB (ii)/(iii): the sweep defers while presence holds
// and the window is open, and charges once the omission stamps the departure.
func TestClaimedNeverStartedWitnessDeferralLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	t.Run("sweep before the first post-evidence heartbeat", func(t *testing.T) {
		f := newWitnessFixture(t, env, 5)
		run := f.continuation(t)
		f.claimAged(t, run, 2, cnwInWindow)
		f.evidenceReport(t, run, 2, f.nonce, f.next())
		f.assertDeferred(t, run, 0, f.sweep(t), "before the first heartbeat")
		f.omit(t)
		sw := f.sweep(t)
		f.assertQueued(t, run, 1, "after the stamp")
		if sw.ClaimedCharged != 1 {
			t.Fatalf("sweep = %+v, want one charge", sw)
		}
	})
	t.Run("listing, sweep, omission, charge", func(t *testing.T) {
		f := newWitnessFixture(t, env, 5)
		run := f.continuation(t)
		f.claimAged(t, run, 2, cnwInWindow)
		f.heartbeat(t, f.next(), f.nonce, f.listEntry(run, 2))
		f.assertDeferred(t, run, 0, f.sweep(t), "while listed")
		f.assertDeferred(t, run, 0, f.sweep(t), "while listed, second sweep")
		f.omit(t)
		sw := f.sweep(t)
		f.assertQueued(t, run, 1, "after the omission")
		if sw.ClaimedCharged != 1 {
			t.Fatalf("sweep = %+v, want one charge", sw)
		}
	})
}

// TestClaimedNeverStartedWitnessBoundedRefusedClaimsLiveDB (iv): repeated cycles of refused
// claim, departure and sweep never exceed k+1 refused claims before the park, and the requeue
// count moves exactly once per charge.
func TestClaimedNeverStartedWitnessBoundedRefusedClaimsLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	for _, extra := range []bool{false, true} {
		t.Run(fmt.Sprintf("extra claim attempts=%v", extra), func(t *testing.T) {
			const max = 3
			f := newWitnessFixture(t, env, max)
			run := f.continuation(t)
			base := f.run(t, run)
			k := max - int(base.RequeueCount-base.RequeueEpisodeBaseline)
			claims, charges := 0, 0
			parked := false
			for gen := int64(2); gen < int64(k)+8; gen++ {
				got, g := f.claim(t)
				if got == uuid.Nil {
					break
				}
				if got != run || g != gen {
					t.Fatalf("claim = (%s, %d), want (%s, %d)", got, g, run, gen)
				}
				claims++
				f.ageClaim(run, cnwInWindow)
				if res := f.evidenceReport(t, run, gen, f.nonce, f.next()); res.Applied {
					t.Fatalf("gen %d: the guard accepted the running report", gen)
				}
				f.omit(t)
				if extra {
					if again, _ := f.claim(t); again != uuid.Nil {
						t.Fatalf("gen %d: a claimed run was claimed again", gen)
					}
				}
				before := f.run(t, run).RequeueCount
				f.sweep(t)
				if extra {
					f.sweep(t)
				}
				r := f.run(t, run)
				if r.Status == "recovery_wait" {
					if r.RecoveryWaitCause.String != "worker_requeue_exhausted" || r.RequeueCount != before {
						t.Fatalf("gen %d: park cause=%v count=%d (was %d), want worker_requeue_exhausted with no charge", gen, r.RecoveryWaitCause, r.RequeueCount, before)
					}
					parked = true
					break
				}
				if r.Status != "queued" || r.RequeueCount != before+1 {
					t.Fatalf("gen %d: status=%s count=%d (was %d), want exactly one charge", gen, r.Status, r.RequeueCount, before)
				}
				charges++
			}
			if !parked || claims != k+1 || charges != k {
				t.Fatalf("parked=%v claims=%d charges=%d, want a park after %d refused claims and %d charges", parked, claims, charges, k+1, k)
			}
			for i := 0; i < 3; i++ {
				f.sweep(t)
				if got, _ := f.claim(t); got != uuid.Nil {
					t.Fatalf("claimed %s after the park", got)
				}
			}
		})
	}
}

// TestClaimedNeverStartedWitnessWindowExpiryLiveDB (v)-(vii): without a stamp the run is deferred
// inside the window and parked unwitnessed once the window passes.
func TestClaimedNeverStartedWitnessWindowExpiryLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	expire := func(t *testing.T, wantPresence string, prep func(f *cncFixture, run uuid.UUID)) {
		f := newWitnessFixture(t, env, 5)
		run := f.continuation(t)
		f.claimAged(t, run, 2, cnwInWindow)
		prep(f, run)
		f.assertDeferred(t, run, 0, f.sweep(t), "inside the window")
		f.ageClaim(run, cnwPastWindow)
		sw := f.sweep(t)
		if sw.ClaimedUnwitnessed != 1 || sw.ClaimedCharged != 0 || sw.ClaimedExhausted != 0 {
			t.Fatalf("sweep = %+v, want one unwitnessed park", sw)
		}
		ev := f.assertUnwitnessedPark(t, run, 0, "past the window")
		if string(ev["presence"]) != `"`+wantPresence+`"` {
			t.Fatalf("presence = %s, want %s", ev["presence"], wantPresence)
		}
		if got := f.openHolds(t); got < 1 {
			t.Fatalf("open holds = %d, want custody untouched", got)
		}
	}
	t.Run("snapshot-less heartbeats only", func(t *testing.T) {
		expire(t, "p1", func(f *cncFixture, run uuid.UUID) {
			f.evidenceReport(t, run, 2, f.nonce, f.next())
			f.plainHeartbeat(t)
			f.plainHeartbeat(t)
		})
	})
	t.Run("rejected and invalid snapshots", func(t *testing.T) {
		expire(t, "p1", func(f *cncFixture, run uuid.UUID) {
			f.evidenceReport(t, run, 2, f.nonce, f.next())
			accepted := f.acceptedEpoch(t)
			// Wrong nonce, stale epoch, older epoch and a malformed entry: each is rejected or
			// invalid, and none may stamp a departure or advance the accepted epoch.
			_, _ = f.svc.Heartbeat(f.env.ctx, store.Worker{ID: f.workerID, UserID: f.userID}, nil, nil, &ActiveSnapshot{SnapshotEpoch: accepted + 100, RegisterNonce: "wrong-nonce", Active: []ActiveRunEntry{}})
			_, _ = f.svc.Heartbeat(f.env.ctx, store.Worker{ID: f.workerID, UserID: f.userID}, nil, nil, &ActiveSnapshot{SnapshotEpoch: accepted, RegisterNonce: f.nonce, Active: []ActiveRunEntry{}})
			_, _ = f.svc.Heartbeat(f.env.ctx, store.Worker{ID: f.workerID, UserID: f.userID}, nil, nil, &ActiveSnapshot{SnapshotEpoch: accepted - 1, RegisterNonce: f.nonce, Active: []ActiveRunEntry{}})
			_, _ = f.svc.Heartbeat(f.env.ctx, store.Worker{ID: f.workerID, UserID: f.userID}, nil, nil, &ActiveSnapshot{SnapshotEpoch: accepted + 5, RegisterNonce: f.nonce, Active: []ActiveRunEntry{{RunID: "not-a-uuid", ClaimGeneration: 2, Phase: "running"}}})
			if got := f.acceptedEpoch(t); got != accepted {
				t.Fatalf("a rejected snapshot advanced the epoch to %d", got)
			}
			if d := f.departure(t, run); d.gen.Valid && d.gen.Int64 == 2 {
				t.Fatalf("a rejected snapshot stamped %+v", d)
			}
		})
	})
	t.Run("repeated listings past the window", func(t *testing.T) {
		expire(t, "p2", func(f *cncFixture, run uuid.UUID) {
			for i := 0; i < 3; i++ {
				f.heartbeat(t, f.next(), f.nonce, f.listEntry(run, 2))
			}
		})
	})
}

// TestClaimedNeverStartedWitnessResetWhenPresenceEndsLiveDB (viii): an offline worker or a
// re-registration (a new nonce) removes the deferral, inside the window and past it.
func TestClaimedNeverStartedWitnessResetWhenPresenceEndsLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	for _, age := range []time.Duration{cnwInWindow, cnwPastWindow} {
		for _, mode := range []string{"worker offline", "re-registration"} {
			t.Run(fmt.Sprintf("%s age %s", mode, age), func(t *testing.T) {
				f := newWitnessFixture(t, env, 5)
				run := f.continuation(t)
				f.claimAged(t, run, 2, age)
				f.evidenceReport(t, run, 2, f.nonce, f.next())
				f.heartbeat(t, f.next(), f.nonce, f.listEntry(run, 2))
				before := f.rowJSON(t, run)
				if mode == "worker offline" {
					f.env.exec(`UPDATE workers SET status = 'offline' WHERE id = $1`, f.workerID)
					if _, err := f.svc.Sweep(f.env.ctx); err != nil {
						t.Fatalf("Sweep: %v", err)
					}
				} else {
					f.env.exec(`UPDATE workers SET snapshot_register_nonce = $2, snapshot_epoch = 0 WHERE id = $1`, f.workerID, "re-registered-"+uuid.NewString())
					f.env.exec(`DELETE FROM worker_active_runs WHERE worker_id = $1`, f.workerID)
					f.sweep(t)
				}
				f.assertQueued(t, run, 0, mode)
				sameExcept(t, mode, before, f.rowJSON(t, run), resetColumns...)
			})
		}
	}
}

// TestClaimedNeverStartedWitnessOverBudgetLiveDB (ix): a witnessed departure past the episode
// budget parks at worker_requeue_exhausted (never failed, never charged), with the capability
// revoked; the budget is the episode's, not the lifetime's.
func TestClaimedNeverStartedWitnessOverBudgetLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	const max = 3
	for _, c := range []struct {
		name            string
		count, baseline int32
		parks           bool
	}{
		{"at the cap", max, 0, true},
		{"over the cap", max + 2, 0, true},
		{"cap measured from the episode baseline", max + 2, 3, false},
		{"baseline episode spent", max + 3, 3, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newWitnessFixture(t, env, max)
			run := f.continuation(t)
			f.claimAged(t, run, 2, cnwInWindow)
			f.evidenceReport(t, run, 2, f.nonce, f.next())
			f.omit(t)
			capHash := []byte("cap-" + uuid.NewString())
			f.env.exec(`UPDATE runs SET requeue_count = $2, requeue_episode_baseline = $3, codex_cap_hash = $4 WHERE id = $1`, run, c.count, c.baseline, capHash)
			epoch := f.run(t, run).CodexClaimEpoch
			holds := f.openHolds(t)
			f.sweep(t)
			r := f.run(t, run)
			if c.parks {
				f.assertParked(t, run, c.count, capHash, epoch, holds)
				return
			}
			if r.Status != "queued" || r.RequeueCount != c.count+1 || r.ClaimReleasedAt.Valid || r.CodexCapHash != nil || r.CodexClaimEpoch != epoch+1 {
				t.Fatalf("status=%s count=%d released=%v cap=%v epoch=%d, want a charge with the capability revoked", r.Status, r.RequeueCount, r.ClaimReleasedAt.Valid, r.CodexCapHash, r.CodexClaimEpoch)
			}
		})
	}
}

// parkedUnwitnessed drives a continuation to the unwitnessed park and returns the pre-sweep
// snapshot of its custody rows with the codex epoch before the park.
func parkedUnwitnessed(t *testing.T, env codexTestEnv) (*cncFixture, uuid.UUID, map[string]string, int64) {
	t.Helper()
	f := newWitnessFixture(t, env, 5)
	run := f.continuation(t)
	f.claimAged(t, run, 2, cnwPastWindow)
	f.evidenceReport(t, run, 2, f.nonce, f.next())
	f.plainHeartbeat(t)
	f.env.exec(`UPDATE runs SET requeue_count = 1, codex_cap_hash = $2 WHERE id = $1`, run, []byte("cap-"+uuid.NewString()))
	f.env.exec(`INSERT INTO run_messages (run_id, seq, kind, payload) VALUES ($1, 1, 'status', '{"journal":"before"}')`, run)
	before := f.custodyRows(t, run)
	epoch := f.run(t, run).CodexClaimEpoch
	if sw := f.sweep(t); sw.ClaimedUnwitnessed != 1 {
		t.Fatalf("sweep = %+v, want one unwitnessed park", sw)
	}
	return f, run, before, epoch
}

// TestClaimedNeverStartedWitnessReleaseContractLiveDB: every write of the park, and nothing else.
func TestClaimedNeverStartedWitnessReleaseContractLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	f, run, before, epoch := parkedUnwitnessed(t, env)
	f.assertUnwitnessedPark(t, run, 1, "release contract")
	r := f.run(t, run)
	if r.CodexClaimEpoch != epoch+1 {
		t.Fatalf("codex epoch = %d, want %d", r.CodexClaimEpoch, epoch+1)
	}
	if r.SnapshotDepartedGeneration.Valid || r.SnapshotDepartedNonce.Valid || r.SnapshotDepartedEpoch.Valid {
		t.Fatalf("a departure stamp was written: %+v", f.departure(t, run))
	}
	after := f.custodyRows(t, run)
	for table, rows := range before {
		if after[table] != rows {
			t.Fatalf("%s rows changed by the park:\n before %s\n after  %s", table, rows, after[table])
		}
	}
	if len(before) < 3 {
		t.Fatalf("snapshotted tables = %d, want holds, journal and more", len(before))
	}
}

// TestClaimedNeverStartedWitnessLateWritesLiveDB: stragglers from the departed flight leave the
// parked run, its release and its witness columns alone.
func TestClaimedNeverStartedWitnessLateWritesLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	type snap struct {
		status, cause    string
		released         time.Time
		count            int32
		evG, evE, stampG any
		evN, stampN      any
	}
	read := func(f *cncFixture, run uuid.UUID) snap {
		r := f.run(t, run)
		return snap{r.Status, r.RecoveryWaitCause.String, r.ClaimReleasedAt.Time, r.RequeueCount,
			r.RegistrationEvidenceGeneration, r.RegistrationEvidenceEpoch, r.SnapshotDepartedGeneration,
			r.RegistrationEvidenceNonce, r.SnapshotDepartedNonce}
	}
	late := map[string]func(f *cncFixture, run uuid.UUID){
		"running report without evidence": func(f *cncFixture, run uuid.UUID) {
			g := int64(2)
			_, _ = f.svc.SetStateReportWithReconciliation(f.env.ctx, f.wkr, run, StateRequest{State: "running", ClaimGeneration: &g})
		},
		"running report with evidence": func(f *cncFixture, run uuid.UUID) {
			g, e := int64(2), f.next()
			_, _ = f.svc.SetStateReportWithReconciliation(f.env.ctx, f.wkr, run, StateRequest{
				State: "running", ClaimGeneration: &g,
				RegistrationEvidence: &RegistrationEvidence{RegisterNonce: f.nonce, ClaimGeneration: 2, RegistrationEpoch: &e},
			})
		},
		"recovery_wait report": func(f *cncFixture, run uuid.UUID) {
			g := int64(2)
			_, _ = f.svc.SetStateReportWithReconciliation(f.env.ctx, f.wkr, run, StateRequest{State: "recovery_wait", ClaimGeneration: &g})
		},
		"listing snapshot": func(f *cncFixture, run uuid.UUID) {
			_, _ = f.svc.Heartbeat(f.env.ctx, store.Worker{ID: f.workerID, UserID: f.userID}, nil, nil,
				&ActiveSnapshot{SnapshotEpoch: f.next(), RegisterNonce: f.nonce, Active: []ActiveRunEntry{f.listEntry(run, 2)}})
		},
		"omitting snapshot": func(f *cncFixture, run uuid.UUID) {
			_, _ = f.svc.Heartbeat(f.env.ctx, store.Worker{ID: f.workerID, UserID: f.userID}, nil, nil,
				&ActiveSnapshot{SnapshotEpoch: f.next(), RegisterNonce: f.nonce, Active: []ActiveRunEntry{}})
		},
	}
	for name, write := range late {
		t.Run(name, func(t *testing.T) {
			f, run, _, _ := parkedUnwitnessed(t, env)
			before := read(f, run)
			write(f, run)
			f.plainHeartbeat(t)
			if after := read(f, run); after != before {
				t.Fatalf("a late %s changed the parked run: %+v -> %+v", name, before, after)
			}
			f.assertUnwitnessedPark(t, run, 1, "after the late write")
			if f.sweep(t).ClaimedUnwitnessed != 0 {
				t.Fatal("a later sweep re-parked the run")
			}
		})
	}
}

// TestClaimedNeverStartedWitnessControlsLiveDB: every run the cohort must leave to today's
// behaviour, and the NULL-witness case the draft stranded.
func TestClaimedNeverStartedWitnessControlsLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	resetsIdentically := func(t *testing.T, f *cncFixture, run uuid.UUID, what string) {
		t.Helper()
		before := f.rowJSON(t, run)
		sw := f.sweep(t)
		if sw.ClaimedReset < 1 || sw.ClaimedCharged != 0 || sw.ClaimedExhausted != 0 || sw.ClaimedUnwitnessed != 0 {
			t.Fatalf("%s: sweep = %+v, want an uncharged reset only", what, sw)
		}
		f.assertQueued(t, run, f.run(t, run).RequeueCount, what)
		sameExcept(t, what, before, f.rowJSON(t, run), resetColumns...)
	}
	t.Run("hold with a NULL witness and no presence is reset, not stranded", func(t *testing.T) {
		for _, age := range []time.Duration{cnwInWindow, cnwPastWindow} {
			f := newWitnessFixture(t, env, 5)
			run := f.continuation(t)
			f.claimAged(t, run, 2, age)
			f.env.exec(`UPDATE runs SET codex_cap_hash = $2 WHERE id = $1`, run, []byte("cap"))
			if r := f.run(t, run); r.SnapshotDepartedGeneration.Valid || r.RegistrationEvidenceGeneration.Int64 == 2 {
				t.Fatal("precondition: the run must have no witness at generation 2")
			}
			resetsIdentically(t, f, run, fmt.Sprintf("no witness, age %s", age))
		}
	})
	t.Run("non-custody claimed run", func(t *testing.T) {
		f := newWitnessFixture(t, env, 5)
		run := f.seedRun("claimed", 1)
		f.env.exec(`UPDATE runs SET worker_id = $2, claimed_at = now() - interval '10 minutes', codex_cap_hash = $3 WHERE id = $1`, run, f.workerID, []byte("cap"))
		resetsIdentically(t, f, run, "no hold")
	})
	t.Run("first-ever claim with presence", func(t *testing.T) {
		f := newWitnessFixture(t, env, 5)
		run := f.seedGuardedRun(t)
		f.claimAged(t, run, 1, cnwPastWindow)
		f.evidenceReport(t, run, 1, f.nonce, f.next())
		f.heartbeat(t, f.next(), f.nonce, f.listEntry(run, 1))
		resetsIdentically(t, f, run, "current-generation hold with presence")
	})
	t.Run("current-generation-only hold on a later generation", func(t *testing.T) {
		f := newWitnessFixture(t, env, 5)
		run := f.continuation(t)
		f.claimAged(t, run, 2, cnwPastWindow)
		f.evidenceReport(t, run, 2, f.nonce, f.next())
		f.env.exec(`DELETE FROM recovery_custody_holds WHERE run_id = $1 AND generation < 2`, run)
		resetsIdentically(t, f, run, "current-generation-only hold")
	})
	for _, stamped := range []bool{false, true} {
		t.Run(fmt.Sprintf("policy-unresolved (stamped=%v)", stamped), func(t *testing.T) {
			f := newWitnessFixture(t, env, 5)
			run := f.continuation(t)
			f.claimAged(t, run, 2, cnwPastWindow)
			f.evidenceReport(t, run, 2, f.nonce, f.next())
			if stamped {
				f.omit(t)
			}
			// The shape a Codex policy refusal leaves on a run whose outcome is not yet resolved.
			f.env.exec(`UPDATE runs SET policy_outcome_id = gen_random_uuid(), policy_continuation_mode = 'same_conversation',
			    policy_applying_worker_id = $2, policy_source_generation = 1, policy_applying_generation = 2,
			    policy_preserve_required = true WHERE id = $1`, run, f.workerID)
			resetsIdentically(t, f, run, "policy-unresolved")
		})
	}
	t.Run("cross_check run", func(t *testing.T) {
		f := newWitnessFixture(t, env, 5)
		lead := f.continuation(t)
		checker := uuid.New()
		f.env.exec(`INSERT INTO runs (id, user_id, repo_id, issue_title, issue_description, status, kind, trigger_source, harness, report_only, budget_wall_seconds, target_run_id, worker_id, claim_generation, claimed_at,
		                            registration_evidence_generation, registration_evidence_nonce, registration_evidence_epoch)
		            VALUES ($1, $2, $3, 'checker', 'candidate', 'claimed', 'cross_check', 'cross_check', 'codex', true, 300, $4, $5, 2, now() - interval '10 minutes', 2, $6, 1)`,
			checker, f.userID, f.repoID, lead, f.workerID, f.nonce)
		f.seedHold(f.userID, checker, 1, "open")
		before := f.rowJSON(t, checker)
		f.sweep(t)
		r := f.run(t, checker)
		if r.Status == "recovery_wait" || r.RecoveryWaitCause.Valid || r.ClaimReleasedAt.Valid || r.RequeueCount != 0 {
			t.Fatalf("the charge or park touched a cross_check run: status=%s cause=%v released=%v", r.Status, r.RecoveryWaitCause, r.ClaimReleasedAt.Valid)
		}
		sameExcept(t, "cross_check", before, f.rowJSON(t, checker))
	})
	for _, name := range []string{"terminal_pending", "pending_overflow"} {
		t.Run(name+" is protected from the park", func(t *testing.T) {
			f := newWitnessFixture(t, env, 5)
			run := f.continuation(t)
			f.claimAged(t, run, 2, cnwPastWindow)
			f.evidenceReport(t, run, 2, f.nonce, f.next())
			if name == "terminal_pending" {
				f.env.exec(`INSERT INTO worker_active_runs (worker_id, run_id, claim_generation, phase, terminal_pending, terminal_pending_until, snapshot_epoch, reported_at)
				            VALUES ($1, $2, 2, 'running', true, now() + interval '1 hour', 1, now())`, f.workerID, run)
			} else {
				f.env.exec(`UPDATE workers SET pending_overflow = true, pending_overflow_until = now() + interval '1 hour' WHERE id = $1`, f.workerID)
			}
			before := f.rowJSON(t, run)
			f.sweep(t)
			sameExcept(t, name, before, f.rowJSON(t, run))
			if r := f.run(t, run); r.Status != "claimed" {
				t.Fatalf("status = %s, want the protected run left claimed", r.Status)
			}
		})
	}
	t.Run("unrelated live runs and holds are untouched", func(t *testing.T) {
		f := newWitnessFixture(t, env, 5)
		bystander := f.seedRun("running", 3)
		f.env.exec(`UPDATE runs SET worker_id = $2, started_at = now(), claimed_at = now() - interval '10 minutes' WHERE id = $1`, bystander, f.workerID)
		f.seedHold(f.userID, bystander, 1, "open")
		bystanderRow, bystanderHolds := f.rowJSON(t, bystander), f.custodyRows(t, bystander)
		run := f.continuation(t)
		f.claimAged(t, run, 2, cnwPastWindow)
		f.evidenceReport(t, run, 2, f.nonce, f.next())
		f.heartbeat(t, f.next(), f.nonce, f.listEntry(bystander, 3), f.listEntry(run, 2))
		f.assertDeferred(t, run, 0, SweepResult{}, "setup") // still claimed, nothing swept yet
		if sw := f.sweep(t); sw.ClaimedUnwitnessed != 1 {
			t.Fatalf("sweep = %+v, want the one park", sw)
		}
		sameExcept(t, "bystander", bystanderRow, f.rowJSON(t, bystander), "last_activity_at", "updated_at")
		for table, rows := range f.custodyRows(t, bystander) {
			if bystanderHolds[table] != rows {
				t.Fatalf("bystander %s rows changed", table)
			}
		}
	})
	t.Run("the park settles a pending plan check through the lead-exit trigger", func(t *testing.T) {
		f := newWitnessFixture(t, env, 5)
		caps := append(append([]string{}, f.wkr.ProtocolCapabilities...), "cross_check_v1")
		f.wkr.ProtocolCapabilities = caps
		f.env.exec(`UPDATE workers SET protocol_capabilities = $2 WHERE id = $1`, f.workerID, caps)
		run := f.seedGuardedRun(t)
		f.env.exec(`UPDATE runs SET plan_cross_check_required = true WHERE id = $1`, run)
		f.firstEver(t, run)
		f.claimAged(t, run, 2, cnwPastWindow)
		f.evidenceReport(t, run, 2, f.nonce, f.next())
		checker, check := uuid.New(), uuid.New()
		f.env.exec(`INSERT INTO runs (id, user_id, repo_id, issue_title, issue_description, status, kind, trigger_source, harness, report_only, budget_wall_seconds, target_run_id)
		            VALUES ($1, $2, $3, 'checker', 'candidate', 'queued', 'cross_check', 'cross_check', 'codex', true, 300, $4)`, checker, f.userID, f.repoID, run)
		f.env.exec(`INSERT INTO cross_checks (id, lead_run_id, checker_run_id, stage, round, lead_claim_generation, plan_md, milestones, size_class, base_commit, candidate_digest, checker_harness, deadline_at)
		            VALUES ($1, $2, $3, 'plan', 1, 2, 'plan', '[]'::jsonb, 's', repeat('a', 40), $4, 'codex', now() + interval '5 minutes')`, check, run, checker, []byte("digest"))
		f.sweep(t)
		f.assertUnwitnessedPark(t, run, 0, "lead with a pending plan check")
		var verdict string
		if err := f.env.pool.QueryRow(f.env.ctx, `SELECT verdict FROM cross_checks WHERE id = $1`, check).Scan(&verdict); err != nil {
			t.Fatalf("read verdict: %v", err)
		}
		if verdict == "pending" || f.run(t, checker).Status == "queued" {
			t.Fatalf("verdict=%s checker=%s, want the plan check settled by the lead exit", verdict, f.run(t, checker).Status)
		}
	})
}
