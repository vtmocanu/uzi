package store_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #4205 M1 follow-up: the hibernation fences (hibernation_state = 'none') in
// ListUnplaceableQueuedRunsForEphemeral and ListSaturationQueuedRunsForEphemeral. A queued
// cross-check child pinned to its own worker is placeable by that worker through the
// own-worker arm ONLY while the worker is awake: asleep, the arm must not count it, so the run
// has no capable online worker and is surfaced. Skipped unless UZI_TEST_DATABASE_URL is set; run
// via e2e/run-store-it.sh.

// seedPinnedPlanChecker seeds an online hosted worker W advertising the plan cross-check
// protocol and a queued plan-stage Claude checker pinned to W (runs.worker_id), whose lead is
// running on another worker. It returns W and the checker run.
func seedPinnedPlanChecker(fx *fleetFixture) (worker, checker uuid.UUID) {
	fx.t.Helper()
	leadWorker := fx.worker("lead-host", nil, false)
	worker = uuid.New()
	mustExec(fx.ctx, fx.t, fx.pool,
		`INSERT INTO workers (id, user_id, name, token_hash, status, last_heartbeat_at, kind, hosted_size, template_declared, protocol_capabilities)
		 VALUES ($1, $2, $3, $4, 'online', now(), 'hosted', 'm', 'base', $5)`,
		worker, fx.userID, "pinned-"+worker.String(), tokenHash(), []string{"cross_check_v1", "cross_check_codex_lead_v1"})
	lead, checker := fx.queuedRun(), fx.queuedRun()
	mustExec(fx.ctx, fx.t, fx.pool, `UPDATE runs SET status='running', worker_id=$2, claim_generation=1 WHERE id=$1`, lead, leadWorker)
	mustExec(fx.ctx, fx.t, fx.pool,
		`UPDATE runs SET kind='cross_check', issue_iid=NULL, target_run_id=$2, harness='claude', report_only=true,
		        budget_wall_seconds=1800, required_capabilities='{}', worker_id=$3, status_since=now() - interval '1 hour' WHERE id=$1`,
		checker, lead, worker)
	mustExec(fx.ctx, fx.t, fx.pool, `INSERT INTO cross_checks
		(lead_run_id, checker_run_id, stage, round, lead_claim_generation, plan_md, milestones, size_class, base_commit,
		 candidate_digest, checker_harness, deadline_at, automatic_rounds_enabled, automatic_revision_limit)
		VALUES ($1, $2, 'plan', 1, 1, 'plan', '[]', 's', repeat('a', 40), $3, 'claude', now() + interval '5 minutes', true, 2)`,
		lead, checker, []byte("digest"))
	return worker, checker
}

// gapRunIDsAt is unplaceableRunIDs with the cross-check evaluation clock set: the plain helper
// leaves it NULL, which makes every checker-eligibility comparison NULL and a checker never placeable.
func gapRunIDsAt(fx *fleetFixture) map[uuid.UUID]bool {
	fx.t.Helper()
	now := time.Now()
	rows, err := fx.q.ListUnplaceableQueuedRunsForEphemeral(fx.ctx, store.ListUnplaceableQueuedRunsForEphemeralParams{
		BackgroundGraceCutoff:    pgtype.Timestamptz{Time: now.Add(-15 * time.Minute), Valid: true},
		CrossCheckEvaluatedAt:    pgtype.Timestamptz{Time: now, Valid: true},
		CrossCheckAffinityCutoff: pgtype.Timestamptz{Time: now.Add(-time.Hour), Valid: true},
		EphemeralLease:           pgtype.Interval{Valid: true},
		MaxRows:                  1000,
		MaxPerUser:               1000,
	})
	if err != nil {
		fx.t.Fatalf("ListUnplaceableQueuedRunsForEphemeral: %v", err)
	}
	out := make(map[uuid.UUID]bool, len(rows))
	for _, r := range rows {
		if r.UserID == fx.userID {
			out[r.ID] = true
		}
	}
	return out
}

func setHibernated(fx *fleetFixture, worker uuid.UUID, state string) {
	fx.t.Helper()
	mustExec(fx.ctx, fx.t, fx.pool,
		`UPDATE workers SET hibernation_state = $2, hibernation_op_id = $3, hibernated_at = now(), wake_requested_at = now() WHERE id = $1`,
		worker, state, uuid.New())
}

func TestHibernationFencesEphemeralTriggersLiveDB(t *testing.T) {
	t.Run("gap trigger: the own-worker arm counts an awake worker only", func(t *testing.T) {
		fx := newFleetFixture(t)
		optInEphemeral(fx)
		worker, checker := seedPinnedPlanChecker(fx)

		if got := gapRunIDsAt(fx); got[checker] {
			t.Fatalf("checker %s surfaced although its own awake worker can claim it", checker)
		}
		setHibernated(fx, worker, "hibernated")
		if got := gapRunIDsAt(fx); !got[checker] {
			t.Fatalf("checker %s not surfaced while its own worker is hibernated; the own-worker arm must not count a sleeper", checker)
		}
		setHibernated(fx, worker, "waking")
		if got := gapRunIDsAt(fx); !got[checker] {
			t.Fatalf("checker %s not surfaced while its own worker is waking (claims stay fenced until settlement)", checker)
		}
		mustExec(fx.ctx, t, fx.pool, `UPDATE workers SET hibernation_state = 'none', hibernation_op_id = NULL, hibernated_at = NULL, wake_requested_at = NULL WHERE id = $1`, worker)
		if got := gapRunIDsAt(fx); got[checker] {
			t.Fatalf("checker %s surfaced after the worker settled awake", checker)
		}
	})

	t.Run("a hibernated worker never counts as a capable online worker for a capability run", func(t *testing.T) {
		fx := newFleetFixture(t)
		optInEphemeral(fx)
		w := uuid.New()
		mustExec(fx.ctx, t, fx.pool,
			`INSERT INTO workers (id, user_id, name, token_hash, status, last_heartbeat_at, kind, hosted_size, template_declared, docker_enabled)
			 VALUES ($1, $2, $3, $4, 'online', now(), 'hosted', 'm', 'base', true)`, w, fx.userID, "docker-"+w.String(), tokenHash())
		run := queuedRunWithCaps(fx, []string{"docker"})
		if got := unplaceableRunIDs(fx); got[run] {
			t.Fatalf("run %s surfaced although an awake docker worker satisfies it", run)
		}
		setHibernated(fx, w, "hibernated")
		if got := unplaceableRunIDs(fx); !got[run] {
			t.Fatalf("run %s not surfaced while the only docker worker is asleep", run)
		}
	})

	t.Run("saturation trigger: a hibernated worker is not a free-slot satisfier", func(t *testing.T) {
		fx := newFleetFixture(t)
		optInEphemeral(fx)
		full := fx.worker("full", capOf(1), false)
		fx.holdActive(full, 1)
		sleeper := uuid.New()
		mustExec(fx.ctx, t, fx.pool,
			`INSERT INTO workers (id, user_id, name, token_hash, status, last_heartbeat_at, kind, hosted_size, template_declared, max_concurrent_runs)
			 VALUES ($1, $2, $3, $4, 'online', now(), 'hosted', 'm', 'base', 4)`, sleeper, fx.userID, "sleeper-"+sleeper.String(), tokenHash())
		run := fx.queuedRun()
		mustExec(fx.ctx, t, fx.pool, `UPDATE runs SET status_since = now() - interval '1 hour' WHERE id = $1`, run)
		if got := saturationRunIDs(fx); got[run] {
			t.Fatalf("run %s surfaced as saturated although an awake worker has free slots", run)
		}
		setHibernated(fx, sleeper, "hibernated")
		if got := saturationRunIDs(fx); !got[run] {
			t.Fatalf("run %s not surfaced; a hibernated worker's free slots must not block the saturation trigger", run)
		}
	})
}
