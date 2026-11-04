package workersvc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #3859, custody accounting and lock-order probes for the owner-held parks the claimed
// never-started sweep writes: retained_continuation_unwitnessed (the witness window passed) and
// worker_requeue_exhausted (the P1 charge ran out of budget). Everything goes through the real
// Sweep, Heartbeat and DecidePlanCrossCheck entry points and the real custody readers.

// cnuParkedExhausted drives a never-started run at the requeue cap to the charged park: a
// departure stamp from an accepted omitting snapshot is the witness that makes it a charge.
func cnuParkedExhausted(t *testing.T, env codexTestEnv) (*cncFixture, uuid.UUID) {
	t.Helper()
	const max = 2
	f := newCNCFixture(t, env, max)
	run, _ := f.armedAtMax(t, max)
	f.evidenceReport(t, run, 2, f.nonce, f.next())
	f.omit(t)
	if sw := f.sweep(t); sw.ClaimedExhausted < 1 {
		t.Fatalf("sweep = %+v, want the charged park", sw)
	}
	r := f.run(t, run)
	if r.Status != "recovery_wait" || r.RecoveryWaitCause.String != "worker_requeue_exhausted" {
		t.Fatalf("status=%s cause=%v, want recovery_wait/worker_requeue_exhausted", r.Status, r.RecoveryWaitCause)
	}
	return f, run
}

// cnuSetCaptures replaces the captures of every open hold of the run. kind is "none",
// "available" or "needs_action"; guarded sets the holds' inventory_guarded.
func (f *cncFixture) cnuSetCaptures(t *testing.T, run uuid.UUID, kind string, guarded bool) {
	t.Helper()
	f.env.exec(`DELETE FROM recovery_captures WHERE run_id = $1`, run)
	f.env.exec(`UPDATE recovery_custody_holds SET inventory_guarded = $2 WHERE run_id = $1 AND state = 'open'`, run, guarded)
	rows, err := f.env.pool.Query(f.env.ctx, `SELECT id FROM recovery_custody_holds WHERE run_id = $1 AND state = 'open'`, run)
	if err != nil {
		t.Fatal(err)
	}
	var holds []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		holds = append(holds, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for _, hold := range holds {
		capture := uuid.New()
		switch kind {
		case "none":
		case "available":
			f.env.exec(`INSERT INTO recovery_captures
                (id, hold_id, run_id, user_id, original_worker_id, original_worker_identity, source_sha, idempotency_key, state, manifest_bound, byte_size, checksum)
                VALUES ($1, $2, $3, $4, $5, 'cnu-ident', 'H0', $6, 'available', true, 3, 'deadbeef')`,
				capture, hold, run, f.userID, f.workerID, "k-"+capture.String())
			f.env.exec(`INSERT INTO recovery_capture_chunks (capture_id, chunk_index, length, sealed) VALUES ($1, 0, 3, $2)`, capture, []byte("abc"))
		case "needs_action":
			f.env.exec(`INSERT INTO recovery_captures
                (id, hold_id, run_id, user_id, original_worker_id, original_worker_identity, source_sha, idempotency_key, state)
                VALUES ($1, $2, $3, $4, $5, 'cnu-ident', 'H0', $6, 'needs_action')`,
				capture, hold, run, f.userID, f.workerID, "k-"+capture.String())
		default:
			t.Fatalf("unknown capture kind %q", kind)
		}
	}
}

// cnuAssertDecisionCustody reads the run's open holds through the three real readers (the facts
// view behind the custody listings, fn_custody_admission_members and fn_custody_admission_count)
// and through ClaimRun's admission gate: every hold is decision-needed, never archive_ready or
// active, and every hold counts against the owner's limit.
func (f *cncFixture) cnuAssertDecisionCustody(t *testing.T, run uuid.UUID, wantAttention, what string) {
	t.Helper()
	ctx := f.env.ctx
	cutoff := time.Now().Add(-45 * time.Second)
	rows, err := f.env.pool.Query(ctx, `SELECT id, attention, decision_needed FROM recovery_custody_hold_facts WHERE run_id = $1 AND state = 'open'`, run)
	if err != nil {
		t.Fatal(err)
	}
	holds := map[uuid.UUID]bool{}
	for rows.Next() {
		var id uuid.UUID
		var attention string
		var decision bool
		if err := rows.Scan(&id, &attention, &decision); err != nil {
			t.Fatal(err)
		}
		if attention != wantAttention || !decision {
			t.Errorf("%s: hold %s attention=%q decision_needed=%v, want %q and decision_needed", what, id, attention, decision, wantAttention)
		}
		holds[id] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(holds) < 2 {
		t.Fatalf("%s: run has %d open holds, want at least 2 (an earlier generation and the current one)", what, len(holds))
	}
	mrows, err := f.env.pool.Query(ctx, `SELECT hold_id, counted, reason FROM fn_custody_admission_members($1, $2)`, f.userID, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for mrows.Next() {
		var id uuid.UUID
		var counted bool
		var reason string
		if err := mrows.Scan(&id, &counted, &reason); err != nil {
			t.Fatal(err)
		}
		if !holds[id] {
			continue
		}
		seen++
		if !counted || reason != "decision_needed" {
			t.Errorf("%s: hold %s counted=%v reason=%q, want counted with reason decision_needed", what, id, counted, reason)
		}
	}
	mrows.Close()
	if err := mrows.Err(); err != nil {
		t.Fatal(err)
	}
	if seen != len(holds) {
		t.Fatalf("%s: fn_custody_admission_members listed %d of the run's %d holds", what, seen, len(holds))
	}
	var count int64
	if err := f.env.pool.QueryRow(ctx, `SELECT fn_custody_admission_count($1, $2)`, f.userID, cutoff).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if want := f.openHolds(t); count != want {
		t.Fatalf("%s: fn_custody_admission_count = %d, want every open hold (%d) counted", what, count, want)
	}

	// ClaimRun admission: a fresh run of the same owner is refused at a limit equal to the held
	// count and admitted one above it, so the parked holds are what gate it.
	fresh := f.seedRun("queued", 0)
	f.plainHeartbeat(t)
	origLimit := f.svc.p.CustodyHoldLimit
	f.svc.p.CustodyHoldLimit = &fakeCustodyLimit{n: int32(count)}
	if got, _ := f.claim(t); got != uuid.Nil {
		t.Fatalf("%s: claimed %s at custody_hold_limit=%d with %d parked holds, want the parked holds to count against it", what, got, count, count)
	}
	f.svc.p.CustodyHoldLimit = &fakeCustodyLimit{n: int32(count) + 1}
	if got, _ := f.claim(t); got != fresh {
		t.Fatalf("%s: claimed %s at custody_hold_limit=%d, want the fresh run %s admitted one above the held count", what, got, count+1, fresh)
	}
	// Put the fixture back for the next scenario.
	f.env.exec(`UPDATE runs SET status = 'cancelled', finished_at = now() WHERE id = $1`, fresh)
	f.env.exec(`DELETE FROM recovery_custody_holds WHERE run_id = $1`, fresh)
	f.svc.p.CustodyHoldLimit = origLimit
}

// TestUnwitnessedParkCustodyIsDecisionNeededLiveDB: both owner-held parks keep every open hold
// in the decision-needed bucket whatever the capture says, and each hold counts.
func TestUnwitnessedParkCustodyIsDecisionNeededLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	parks := map[string]func(t *testing.T) (*cncFixture, uuid.UUID){
		"retained_continuation_unwitnessed": func(t *testing.T) (*cncFixture, uuid.UUID) {
			f, run, _, _ := parkedUnwitnessed(t, env)
			return f, run
		},
		"worker_requeue_exhausted": func(t *testing.T) (*cncFixture, uuid.UUID) { return cnuParkedExhausted(t, env) },
	}
	for name, park := range parks {
		t.Run(name, func(t *testing.T) {
			f, run := park(t)
			t.Cleanup(func() { f.env.exec(`DELETE FROM recovery_captures WHERE user_id = $1`, f.userID) })
			for _, guarded := range []bool{false, true} {
				f.cnuSetCaptures(t, run, "none", guarded)
				f.cnuAssertDecisionCustody(t, run, "source_only", fmt.Sprintf("no capture, guarded=%v", guarded))
			}
			// An available capture that is not guarded would be archive_ready under any other
			// cause: the owner-held park must still demand a decision.
			f.cnuSetCaptures(t, run, "available", false)
			f.cnuAssertDecisionCustody(t, run, "source_only", "available capture, not guarded")
			f.cnuSetCaptures(t, run, "available", true)
			f.cnuAssertDecisionCustody(t, run, "source_only", "available capture, guarded")
			f.cnuSetCaptures(t, run, "needs_action", false)
			f.cnuAssertDecisionCustody(t, run, "needs_action", "needs_action capture")
			// Control: the same capture on a run that is not parked reads archive_ready, so the
			// assertions above can fail.
			f.cnuSetCaptures(t, run, "available", false)
			var attention string
			if err := f.env.pool.QueryRow(f.env.ctx, `SELECT fn_custody_attention('open', true, false, 'available', 'recovery_wait', 'provider_outage')`).Scan(&attention); err != nil || attention != "archive_ready" {
				t.Fatalf("control attention = %q (%v), want archive_ready", attention, err)
			}
		})
	}
}

func cnuDeadlock(err error) bool {
	if err == nil {
		return false
	}
	var pg *pgconn.PgError
	return (errors.As(err, &pg) && pg.Code == "40P01") || strings.Contains(err.Error(), "40P01") || strings.Contains(err.Error(), "deadlock detected")
}

// cnuRace runs a and b together, released by a barrier, and returns their errors.
func cnuRace(a, b func() error) (errA, errB error) {
	var wg sync.WaitGroup
	start := make(chan struct{})
	wg.Add(2)
	go func() { defer wg.Done(); <-start; errA = a() }()
	go func() { defer wg.Done(); <-start; errB = b() }()
	close(start)
	wg.Wait()
	return errA, errB
}

// cnuWindowPassed puts a continuation in the state the sweep parks: generation 2 claimed, past
// the witness window, first running report carrying registration evidence, no departure stamp.
func cnuWindowPassed(t *testing.T, f *cncFixture) uuid.UUID {
	t.Helper()
	run := f.continuation(t)
	f.claimAged(t, run, 2, cnwPastWindow)
	f.evidenceReport(t, run, 2, f.nonce, f.next())
	f.plainHeartbeat(t)
	f.env.exec(`UPDATE runs SET requeue_count = 1 WHERE id = $1`, run)
	return run
}

// TestUnwitnessedParkSweepVsSnapshotApplyDeadlockLiveDB: Sweep (which parks or charges the run)
// racing the heartbeat's active-run snapshot apply (ReplaceWorkerActiveRuns) for the same worker
// and run, many rounds. No deadlock, no error, and one consistent outcome: the park (the snapshot
// lost the race and left no stamp before the sweep read) or exactly one charge (the stamp landed
// first).
func TestUnwitnessedParkSweepVsSnapshotApplyDeadlockLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	const rounds = 20
	parked, charged := 0, 0
	for round := 0; round < rounds; round++ {
		f := newWitnessFixture(t, env, 5)
		run := cnuWindowPassed(t, f)
		ctx, cancel := context.WithTimeout(f.env.ctx, 60*time.Second)
		sweepErr, beatErr := cnuRace(func() error {
			for i := 0; i < 3; i++ {
				if _, err := f.svc.Sweep(ctx); err != nil {
					return err
				}
			}
			return nil
		}, func() error {
			for i := 0; i < 3; i++ {
				// Accepted snapshots that never list the flight: the first one with an earlier
				// witnessed listing (the registration evidence) stamps the departure.
				if _, err := f.svc.Heartbeat(ctx, store.Worker{ID: f.workerID, UserID: f.userID}, nil, nil,
					&ActiveSnapshot{SnapshotEpoch: f.next(), RegisterNonce: f.nonce, Active: []ActiveRunEntry{}}); err != nil {
					return err
				}
			}
			return nil
		})
		cancel()
		for who, err := range map[string]error{"Sweep": sweepErr, "Heartbeat": beatErr} {
			if cnuDeadlock(err) {
				t.Fatalf("round %d: %s deadlocked: %v", round, who, err)
			}
			if err != nil {
				t.Fatalf("round %d: %s: %v", round, who, err)
			}
		}
		r := f.run(t, run)
		switch {
		case r.Status == "recovery_wait" && r.RecoveryWaitCause.String == "retained_continuation_unwitnessed":
			f.assertUnwitnessedPark(t, run, 1, fmt.Sprintf("round %d", round))
			parked++
		case r.Status == "queued" && r.RequeueCount == 2 && !r.ClaimReleasedAt.Valid:
			charged++
		default:
			t.Fatalf("round %d: status=%s count=%d cause=%v released=%v, want a park or exactly one charge",
				round, r.Status, r.RequeueCount, r.RecoveryWaitCause, r.ClaimReleasedAt.Valid)
		}
		if got := f.ownOpenHolds(t, run); got != 2 {
			t.Fatalf("round %d: open holds = %d, want 2 untouched", round, got)
		}
	}
	t.Logf("sweep vs snapshot apply: %d rounds, %d parked, %d charged", rounds, parked, charged)
}

// TestUnwitnessedParkSweepVsCheckerSettlementDeadlockLiveDB: the sweep parks a lead that has a
// pending plan cross-check while the checker run reports its verdict (DecidePlanCrossCheck takes
// the lead lock, then the check). The sweep locks the same pair; whichever lands first, there is
// no deadlock, the check is never left pending and the lead ends parked.
func TestUnwitnessedParkSweepVsCheckerSettlementDeadlockLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	const rounds = 20
	decided, refused := 0, 0
	for round := 0; round < rounds; round++ {
		f := newWitnessFixture(t, env, 5)
		caps := append(append([]string{}, f.wkr.ProtocolCapabilities...), "cross_check_v1")
		f.wkr.ProtocolCapabilities = caps
		f.env.exec(`UPDATE workers SET protocol_capabilities = $2 WHERE id = $1`, f.workerID, caps)
		run := f.continuation(t)
		f.env.exec(`UPDATE runs SET plan_cross_check_required = true WHERE id = $1`, run)
		f.claimAged(t, run, 2, cnwPastWindow)
		f.evidenceReport(t, run, 2, f.nonce, f.next())
		f.plainHeartbeat(t)
		f.env.exec(`UPDATE runs SET requeue_count = 1 WHERE id = $1`, run)
		checker, check := uuid.New(), uuid.New()
		f.env.exec(`INSERT INTO runs (id, user_id, repo_id, issue_title, issue_description, status, kind, trigger_source, harness, report_only, budget_wall_seconds, target_run_id, worker_id, claim_generation)
		            VALUES ($1, $2, $3, 'checker', 'candidate', 'running', 'cross_check', 'cross_check', 'codex', true, 300, $4, $5, 1)`, checker, f.userID, f.repoID, run, f.workerID)
		f.env.exec(`INSERT INTO cross_checks (id, lead_run_id, checker_run_id, stage, round, lead_claim_generation, plan_md, milestones, size_class, base_commit, candidate_digest, checker_harness, deadline_at)
		            VALUES ($1, $2, $3, 'plan', 1, 2, 'plan', '[]'::jsonb, 's', repeat('a', 40), $4, 'codex', now() + interval '5 minutes')`, check, run, checker, []byte("digest"))
		ctx, cancel := context.WithTimeout(f.env.ctx, 60*time.Second)
		sweepErr, decideErr := cnuRace(func() error {
			_, err := f.svc.Sweep(ctx)
			return err
		}, func() error {
			_, err := f.svc.DecidePlanCrossCheck(ctx, f.wkr, checker, 1, "approve", "approve", []byte(`{"summary":"ok","items":[]}`))
			return err
		})
		cancel()
		if cnuDeadlock(sweepErr) || cnuDeadlock(decideErr) {
			t.Fatalf("round %d: deadlock: sweep=%v decide=%v", round, sweepErr, decideErr)
		}
		if sweepErr != nil {
			t.Fatalf("round %d: Sweep: %v", round, sweepErr)
		}
		switch {
		case decideErr == nil:
			decided++
		case errors.Is(decideErr, ErrCrossCheckRefused):
			refused++
		default:
			t.Fatalf("round %d: DecidePlanCrossCheck: %v", round, decideErr)
		}
		f.assertUnwitnessedPark(t, run, 1, fmt.Sprintf("round %d", round))
		var verdict string
		if err := f.env.pool.QueryRow(f.env.ctx, `SELECT verdict FROM cross_checks WHERE id = $1`, check).Scan(&verdict); err != nil {
			t.Fatal(err)
		}
		if verdict == "pending" {
			t.Fatalf("round %d: the plan check was left pending (decide err=%v)", round, decideErr)
		}
		if decideErr == nil && verdict != "approve" {
			t.Fatalf("round %d: decide succeeded but verdict = %q", round, verdict)
		}
		if got := f.ownOpenHolds(t, run); got != 2 {
			t.Fatalf("round %d: open holds = %d, want 2 untouched", round, got)
		}
	}
	t.Logf("sweep vs checker settlement: %d rounds, %d decided before the sweep, %d refused after it", rounds, decided, refused)
}
