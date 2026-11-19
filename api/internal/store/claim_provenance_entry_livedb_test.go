package store_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4476: entry-point regressions for the Codex claim provenance gate, against a REAL Postgres
// (skipped unless UZI_TEST_DATABASE_URL points at a throwaway one).
//
// Every test seeds a Codex run through the REAL ClaimRun (claimant A, generation 0 -> 1), brings it
// to a re-claimable state through the production writer named in the test, and then asserts that a
// different live worker B (and, where a writer hands the run to a third worker, that worker too)
// gets no row while A, the last Codex claimant of that generation, claims it. Before #4476 each of
// these entry points released the run to B, so the file references no #4476 symbol: it compiles and
// runs against the pre-change schema and goes red there.
//
// Serial: RequeueRunsOfStaleWorkers is a global sweep and the fixture helpers are shared; never add
// t.Parallel here.

// entrySeedCodex inserts a queued Codex issue run and claims it as a (generation 1) through the
// real ClaimRun, returning the run id. The run is left 'claimed' on a.
func entrySeedCodex(fx *rollAffinityFixture, a uuid.UUID) uuid.UUID {
	fx.t.Helper()
	id := uuid.New()
	mustExec(fx.ctx, fx.t, fx.pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, harness)
		 VALUES ($1, $2, $3, $4, 't', 'd', 'queued', 'codex')`, id, fx.userID, fx.repoID, fx.nextIID())
	fx.mustClaim(a, id, 0, staleCeiling, "seed: first Codex claim")
	return id
}

// entryQueue returns a claimed run to the queue the way a requeue/park leaves it: queued, worker_id
// kept, clocks aged by the given amounts.
func entryQueue(fx *rollAffinityFixture, run uuid.UUID, age time.Duration) {
	fx.t.Helper()
	mustExec(fx.ctx, fx.t, fx.pool, `UPDATE runs SET status = 'queued' WHERE id = $1`, run)
	fx.age(run, age, age)
}

func entryHeartbeatStale(fx *rollAffinityFixture, w uuid.UUID) {
	fx.t.Helper()
	mustExec(fx.ctx, fx.t, fx.pool, `UPDATE workers SET last_heartbeat_at = now() - interval '2 minutes' WHERE id = $1`, w)
}

// entryReleased marks a claimed run the way a server-side park does: claim released, the released
// incarnation recorded with a nonce the claimant does not currently carry (so A may re-claim).
func entryReleased(fx *rollAffinityFixture, run, a uuid.UUID) {
	fx.t.Helper()
	mustExec(fx.ctx, fx.t, fx.pool,
		`UPDATE runs SET claim_released_at = now(), released_worker_id = $2, released_worker_nonce = 'old-incarnation' WHERE id = $1`, run, a)
}

func entryWorkerNull(fx *rollAffinityFixture, run uuid.UUID) {
	fx.t.Helper()
	var w pgtype.UUID
	if err := fx.pool.QueryRow(fx.ctx, `SELECT worker_id FROM runs WHERE id = $1`, run).Scan(&w); err != nil || w.Valid {
		fx.t.Fatalf("worker_id after the writer = %v (err %v), want NULL", w, err)
	}
}

func entryStatus(fx *rollAffinityFixture, run uuid.UUID, want string) {
	fx.t.Helper()
	var s string
	if err := fx.pool.QueryRow(fx.ctx, `SELECT status FROM runs WHERE id = $1`, run).Scan(&s); err != nil || s != want {
		fx.t.Fatalf("status = %q (err %v), want %q", s, err, want)
	}
}

// (1) The stale-worker sweep: A goes heartbeat-stale, RequeueRunsOfStaleWorkers requeues the run
// keeping worker_id. Before #4476 the pin fell open when the grace was disabled, when it expired,
// or when the ceiling lapsed; a live peer then claimed the Codex run cold.
func TestCodexProvenanceEntryStaleSweepLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name       string
		grace      time.Duration
		statusAge  time.Duration
		updatedAge time.Duration
	}{
		{"grace_disabled", 0, 0, 0},
		{"grace_expired", staleGrace, 11 * time.Minute, 11 * time.Minute},
		{"ceiling_lapsed", staleGrace, time.Minute, 40 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newRollAffinityFixture(t)
			a := fx.worker("A", 0, false)
			b := fx.worker("B", 0, false)
			run := entrySeedCodex(fx, a)
			entryHeartbeatStale(fx, a)
			rows, err := fx.q.RequeueRunsOfStaleWorkers(fx.ctx, store.RequeueRunsOfStaleWorkersParams{
				MaxRequeues: 3, Cutoff: pgtype.Timestamptz{Time: time.Now().Add(-45 * time.Second), Valid: true},
			})
			if err != nil {
				t.Fatalf("RequeueRunsOfStaleWorkers: %v", err)
			}
			found := false
			for _, r := range rows {
				found = found || r.ID == run
			}
			if !found {
				t.Fatalf("the stale sweep did not requeue run %s", run)
			}
			if tc.updatedAge > 0 {
				fx.age(run, tc.statusAge, tc.updatedAge)
			}
			fx.mustBlock(b, tc.grace, staleCeiling, "live peer B after the stale sweep ("+tc.name+")")
			fx.mustClaim(a, run, tc.grace, staleCeiling, "recorded claimant A after the stale sweep")
		})
	}
}

// (2) The affinity ceiling lapsing on a plain queued Codex run: with A fresh, with A heartbeat-stale,
// and with A draining (a roll cordon). Past the ceiling the pin released the run to B before #4476.
func TestCodexProvenanceEntryAffinityCeilingLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(fx *rollAffinityFixture, a uuid.UUID)
	}{
		{"owner_fresh", func(*rollAffinityFixture, uuid.UUID) {}},
		{"owner_stale", entryHeartbeatStale},
		{"owner_draining", func(fx *rollAffinityFixture, a uuid.UUID) {
			mustExec(fx.ctx, fx.t, fx.pool, `UPDATE workers SET draining_since = now() WHERE id = $1`, a)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newRollAffinityFixture(t)
			a := fx.worker("A", 0, false)
			b := fx.worker("B", 0, false)
			run := entrySeedCodex(fx, a)
			tc.setup(fx, a)
			entryQueue(fx, run, 40*time.Minute) // past the 30m ceiling
			fx.mustBlock(b, 0, staleCeiling, "live peer B past the affinity ceiling ("+tc.name+")")
			fx.mustClaim(a, run, 0, staleCeiling, "recorded claimant A past the affinity ceiling")
		})
	}
}

// (3) Writers that null worker_id (a server-parked run resuming without its worker, D19) hand the run
// to any worker before #4476: ResumePausedRun, the wall-park Extend, the wall-park Stop (finalize)
// and the exhausted-recovery Resume.
func TestCodexProvenanceEntryWorkerNulledLiveDB(t *testing.T) {
	const userTimeout = 3600
	for _, tc := range []struct {
		name  string
		setup string // applied to the claimed run; $1 = run, $2 = A
		do    func(fx *rollAffinityFixture, run uuid.UUID)
	}{
		{
			"resume_paused",
			`UPDATE runs SET status = 'paused', claim_released_at = now(), released_worker_id = $2, released_worker_nonce = 'old-incarnation' WHERE id = $1`,
			func(fx *rollAffinityFixture, run uuid.UUID) {
				if _, err := fx.q.ResumePausedRun(fx.ctx, store.ResumePausedRunParams{ID: run, UserID: fx.userID, GlobalTimeoutSeconds: userTimeout}); err != nil {
					fx.t.Fatalf("ResumePausedRun: %v", err)
				}
			},
		},
		{
			"wall_extend",
			`UPDATE runs SET status = 'paused', hold_reason = 'budget_exhausted', claim_released_at = now(), released_worker_id = $2,
			        released_worker_nonce = 'old-incarnation', started_at = now() - interval '5 hours',
			        budget_wall_seconds = 3600, status_since = now() - interval '1 minute' WHERE id = $1`,
			func(fx *rollAffinityFixture, run uuid.UUID) {
				if _, err := fx.q.ExtendAndResumeWallPark(fx.ctx, store.ExtendAndResumeWallParkParams{
					ID: run, UserID: fx.userID, Secs: 3600, Cap: 57600, BaseWallUpperSeconds: extendTestBaseWallUpper,
					FinalizeAllowanceSeconds: extendTestFinalizeAllowance, MaxWallSeconds: extendTestMaxWall,
					GlobalTimeoutSeconds: 7200, Body: pgtype.Text{String: "extend+resume", Valid: true},
				}); err != nil {
					fx.t.Fatalf("ExtendAndResumeWallPark: %v", err)
				}
			},
		},
		{
			"wall_finalize",
			`UPDATE runs SET status = 'paused', hold_reason = 'budget_exhausted', claim_released_at = now(), released_worker_id = $2,
			        released_worker_nonce = 'old-incarnation', started_at = now() - interval '5 hours',
			        budget_wall_seconds = 3600, status_since = now() - interval '1 minute',
			        milestones_completed = '["m1"]'::jsonb WHERE id = $1`,
			func(fx *rollAffinityFixture, run uuid.UUID) {
				if _, err := fx.q.StopWallPark(fx.ctx, store.StopWallParkParams{
					ScopeCeiling: 1, GlobalTimeoutSeconds: 7200, ID: run, UserID: fx.userID,
					Body: pgtype.Text{String: "stop", Valid: true},
				}); err != nil {
					fx.t.Fatalf("StopWallPark: %v", err)
				}
			},
		},
		{
			"recovery_resume",
			`UPDATE runs SET status = 'recovery_wait', recovery_wait_cause = 'worker_requeue_exhausted', claim_released_at = now(),
			        released_worker_id = $2, released_worker_nonce = 'old-incarnation' WHERE id = $1`,
			func(fx *rollAffinityFixture, run uuid.UUID) {
				if _, err := fx.q.ResumeWorkerRecoveryEpisode(fx.ctx, store.ResumeWorkerRecoveryEpisodeParams{
					ID: run, UserID: fx.userID, GlobalTimeoutSeconds: userTimeout,
				}); err != nil {
					fx.t.Fatalf("ResumeWorkerRecoveryEpisode: %v", err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newRollAffinityFixture(t)
			a := fx.worker("A", 0, false)
			b := fx.worker("B", 0, false)
			run := entrySeedCodex(fx, a)
			mustExec(fx.ctx, t, fx.pool, tc.setup, run, a)
			tc.do(fx, run)
			entryStatus(fx, run, "queued")
			entryWorkerNull(fx, run)
			fx.mustBlock(b, 0, staleCeiling, "live peer B on a run resumed with worker_id NULL ("+tc.name+")")
			fx.mustClaim(a, run, 0, staleCeiling, "recorded claimant A on a run resumed with worker_id NULL")
		})
	}
}

// (4) ParkRunCodexAccountUnavailable rewrites worker_id to the newest open custody holder C. The run
// is then queued under C's pin until the ceiling lapses; neither B nor C (the pin holder itself) may
// claim it, only the recorded claimant A.
func TestCodexProvenanceEntryAccountUnavailableForeignHolderLiveDB(t *testing.T) {
	fx := newRollAffinityFixture(t)
	a := fx.worker("A", 0, false)
	b := fx.worker("B", 0, false)
	c := fx.worker("C", 0, false)
	run := entrySeedCodex(fx, a)
	mustExec(fx.ctx, t, fx.pool,
		`INSERT INTO recovery_custody_holds (id, user_id, repo_id, run_id, generation, state, original_worker_id, original_worker_identity,
		        live_worker_id, live_run_id, inventory_guarded)
		 VALUES ($1, $2, $3, $4, 1, 'open', $5, 'provenance-entry', $5, $4, false)`,
		uuid.New(), fx.userID, fx.repoID, run, c)
	// The custody holder is C; the park rewrites worker_id from A to C.
	parked, err := fx.q.ParkRunCodexAccountUnavailable(fx.ctx, store.ParkRunCodexAccountUnavailableParams{
		ClaimGeneration: 1, ID: run, WorkerID: pgtype.UUID{Bytes: a, Valid: true},
	})
	if err != nil {
		t.Fatalf("ParkRunCodexAccountUnavailable: %v", err)
	}
	if !parked.WorkerID.Valid || parked.WorkerID.Bytes != c {
		t.Fatalf("park left worker_id=%v, want the foreign custody holder %s", parked.WorkerID, c)
	}
	// The account recovers; the run is queued, pinned to C, and the ceiling has lapsed.
	mustExec(fx.ctx, t, fx.pool, `UPDATE runs SET status = 'queued', recovery_wait_cause = NULL WHERE id = $1`, run)
	fx.age(run, 40*time.Minute, 40*time.Minute)
	fx.mustBlock(b, 0, staleCeiling, "unrelated peer B")
	fx.mustBlock(c, 0, staleCeiling, "the foreign custody holder C that the park wrote into worker_id")
	fx.mustClaim(a, run, 0, staleCeiling, "recorded claimant A")
}

// (5) The forge-origin owner release (fn_forge_origin_owner_pins): the owner A is cooled at the
// run's origin and the source history is provably safe, so the pin releases the run to any peer.
// Provenance still holds it for A, which becomes eligible again once its cooldown is due.
func TestCodexProvenanceEntryForgeOriginOwnerReleaseLiveDB(t *testing.T) {
	fx := newRollAffinityFixture(t)
	a := fx.worker("A", 0, false)
	b := fx.worker("B", 0, false)
	run := entrySeedCodex(fx, a)
	f := &forgePlacementFixture{rollAffinityFixture: fx, owner: a, peer: b, run: run}
	forgeAdmissionCooldown(fx, a, false)
	f.history("no_adopted_source")
	f.safe(true)
	fx.mustBlock(b, 0, staleCeiling, "peer B after the forge-origin owner release")
	mustExec(fx.ctx, t, fx.pool, `UPDATE forge_origin_cooldowns SET cooled_until = now() - interval '1 day' WHERE worker_id = $1`, a)
	fx.mustClaim(a, run, 0, staleCeiling, "recorded claimant A once its cooldown is due")
}

// (6) The cross_check bypass: a checker child skips the worker_id pin entirely (kind = 'cross_check'),
// so any live checker-capable worker took a re-queued Codex checker before #4476.
func TestCodexProvenanceEntryCrossCheckBypassLiveDB(t *testing.T) {
	fx := newRollAffinityFixture(t)
	a := fx.worker("A", 0, false)
	b := fx.worker("B", 0, false)
	caps := []string{capability.CrossCheckV1, capability.CodexHarnessV1, capability.CodexRuntimeV2}
	mustExec(fx.ctx, t, fx.pool, `UPDATE workers SET protocol_capabilities = $2 WHERE id = ANY($1)`, []uuid.UUID{a, b}, caps)

	lead, child := fx.unclaimedRun(), fx.unclaimedRun()
	mustExec(fx.ctx, t, fx.pool, `UPDATE runs SET status = 'running', worker_id = $2, claim_generation = 1 WHERE id = $1`, lead, a)
	mustExec(fx.ctx, t, fx.pool,
		`UPDATE runs SET kind = 'cross_check', issue_iid = NULL, target_run_id = $2, harness = 'codex', report_only = true,
		        budget_wall_seconds = 1800, required_capabilities = '{}' WHERE id = $1`, child, lead)
	mustExec(fx.ctx, t, fx.pool,
		`INSERT INTO cross_checks (lead_run_id, checker_run_id, stage, round, lead_claim_generation, plan_md, milestones, size_class,
		        base_commit, candidate_digest, checker_harness, deadline_at, automatic_rounds_enabled, automatic_revision_limit)
		 VALUES ($1, $2, 'plan', 1, 1, 'plan', '[]', 's', repeat('a', 40), $3, 'codex', now() + interval '1 hour', true, 2)`,
		lead, child, []byte("digest"))

	claim := func(w uuid.UUID) (store.Run, error) {
		now := time.Now()
		return fx.q.ClaimRun(fx.ctx, store.ClaimRunParams{
			WorkerID: pgtype.UUID{Bytes: w, Valid: true}, UserID: fx.userID,
			CrossCheckEvaluatedAt: pgtype.Timestamptz{Time: now, Valid: true},
			// An affinity cutoff in the future has lapsed the checker's own affinity window, so only the
			// provenance gate can hold the run for A.
			CrossCheckAffinityCutoff: pgtype.Timestamptz{Time: now.Add(time.Hour), Valid: true},
			AffinityCutoff:           pgtype.Timestamptz{Time: now.Add(-staleCeiling), Valid: true},
			// A spread cutoff in the future bypasses fleet spread: A already carries the lead run.
			SpreadCutoff:       pgtype.Timestamptz{Time: now.Add(time.Hour), Valid: true},
			HeartbeatCutoff:    pgtype.Timestamptz{Time: now.Add(-45 * time.Second), Valid: true},
			WorkerCaps:         []string{},
			WorkerProtocolCaps: caps,
		})
	}
	got, err := claim(a)
	if err != nil || got.ID != child {
		t.Fatalf("seed: A's first claim of the Codex checker: run=%s err=%v", got.ID, err)
	}
	mustExec(fx.ctx, t, fx.pool, `UPDATE runs SET status = 'queued' WHERE id = $1`, child)
	if _, err := claim(b); err != pgx.ErrNoRows {
		t.Fatalf("peer B claiming a re-queued Codex checker recorded for A: got %v, want pgx.ErrNoRows", err)
	}
	got, err = claim(a)
	if err != nil || got.ID != child {
		t.Fatalf("recorded claimant A re-claiming its checker: run=%s err=%v", got.ID, err)
	}
}

// (7) A Codex judge run: queued again after its first claim, it stays queued for B and A claims it.
func TestCodexProvenanceEntryJudgeLiveDB(t *testing.T) {
	fx := newRollAffinityFixture(t)
	a := fx.worker("A", 0, false)
	b := fx.worker("B", 0, false)
	target := fx.unclaimedRun()
	mustExec(fx.ctx, t, fx.pool, `UPDATE runs SET status = 'completed', finished_at = now() WHERE id = $1`, target)
	judge := uuid.New()
	mustExec(fx.ctx, t, fx.pool,
		`INSERT INTO runs (id, user_id, kind, target_run_id, issue_title, issue_description, status, trigger_source, harness)
		 VALUES ($1, $2, 'judge', $3, 'judge', 'd', 'queued', 'judge', 'codex')`, judge, fx.userID, target)
	fx.mustClaim(a, judge, 0, staleCeiling, "seed: first Codex judge claim")
	entryQueue(fx, judge, 40*time.Minute)
	fx.mustBlock(b, 0, staleCeiling, "peer B on a re-queued Codex judge run")
	fx.mustClaim(a, judge, 0, staleCeiling, "recorded claimant A on its judge run")
}
