package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// These tests execute the generated ClaimRun through the same fixture seam as
// the affinity tests. Regenerate sqlc before running them after query edits.
func forgeAdmissionClaim(fx *rollAffinityFixture, q *store.Queries, worker uuid.UUID) (store.Run, error) {
	ctx, cancel := context.WithTimeout(fx.ctx, 15*time.Second)
	defer cancel()
	params := store.ClaimRunParams{
		WorkerID:         pgtype.UUID{Bytes: worker, Valid: true},
		UserID:           fx.userID,
		AffinityCutoff:   pgtype.Timestamptz{Time: time.Now().Add(-30 * time.Minute), Valid: true},
		SpreadCutoff:     pgtype.Timestamptz{Time: time.Now().Add(-9 * time.Second), Valid: true},
		HeartbeatCutoff:  pgtype.Timestamptz{Time: time.Now().Add(-45 * time.Second), Valid: true},
		ClaimantDraining: false,
		RecoveryCapable:  true,
		WorkerIdentity:   "forge-admission-test",
	}
	// ANY over an empty, non-NULL array is false for inventory_guarded.
	params.WorkerProtocolCaps = []string{}
	return q.ClaimRun(ctx, params)
}

func forgeAdmissionCooldown(fx *rollAffinityFixture, worker uuid.UUID, due bool) {
	fx.t.Helper()
	mustExec(fx.ctx, fx.t, fx.pool, `INSERT INTO forge_origin_cooldowns
		(user_id,worker_id,origin,cooled_until,last_reason,observed_at)
		VALUES ($1,$2,'https://forge.e2e',
			now() + CASE WHEN $3 THEN interval '-1 day' ELSE interval '1 day' END,
			'transient_clone_fetch_exhausted',now())`, fx.userID, worker, due)
}

func TestForgeOriginAdmissionHardExclusionLiveDB(t *testing.T) {
	fx := newRollAffinityFixture(t)
	w := fx.worker("claimant", 0, false)
	first := fx.pinnedRun(w, time.Hour)
	second := fx.unclaimedRun()
	mustExec(fx.ctx, t, fx.pool, "UPDATE runs SET updated_at=now()-interval '1 hour' WHERE id=$1", second)
	// A distinct repository on the same origin shares the exclusion key.
	otherRepo := uuid.New()
	mustExec(fx.ctx, t, fx.pool, `INSERT INTO repos
		(id,connection_id,forge_project_id,path_with_namespace,web_url,default_branch,enabled)
		SELECT $1,connection_id,2,'g/second','https://forge.e2e/g/second','main',true
		FROM repos WHERE id=$2`, otherRepo, fx.repoID)
	mustExec(fx.ctx, t, fx.pool, "UPDATE runs SET repo_id=$1 WHERE id=$2", otherRepo, second)
	forgeAdmissionCooldown(fx, w, false)
	if _, err := forgeAdmissionClaim(fx, fx.q, w); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("lone worker claimed before due: %v", err)
	}
	var untouched bool
	if err := fx.pool.QueryRow(fx.ctx, `SELECT
		(SELECT count(*) FROM runs WHERE id IN ($1,$2) AND status='queued' AND claim_generation=0)=2
		AND NOT EXISTS (SELECT 1 FROM recovery_custody_holds WHERE run_id IN ($1,$2))`,
		first, second).Scan(&untouched); err != nil || !untouched {
		t.Fatalf("refusal changed G or custody: %v, %v", untouched, err)
	}
	// Hard exclusion also holds when every worker is down for this origin.
	peer := fx.worker("also-cooled", 0, false)
	forgeAdmissionCooldown(fx, peer, false)
	if _, err := forgeAdmissionClaim(fx, fx.q, peer); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("all-cooled fleet admitted peer: %v", err)
	}
	var atEquality bool
	if err := fx.pool.QueryRow(fx.ctx, `SELECT fn_forge_origin_eligible(w,r,c.cooled_until)
		FROM workers w JOIN forge_origin_cooldowns c ON c.worker_id=w.id
		JOIN runs r ON r.id=$2 WHERE w.id=$1`, w, first).Scan(&atEquality); err != nil || !atEquality {
		t.Fatalf("exact due boundary: %v, %v", atEquality, err)
	}
	// Other origins remain available without waiting for the first origin.
	mustExec(fx.ctx, t, fx.pool, "UPDATE repos SET web_url='https://other.example/g/second' WHERE id=$1", otherRepo)
	got, err := forgeAdmissionClaim(fx, fx.q, w)
	if err != nil || got.ID != second {
		t.Fatalf("other origin claim: %s, %v", got.ID, err)
	}
	// The snapshot helper also leaves noncloning lanes alone.
	var exempt bool
	if err := fx.pool.QueryRow(fx.ctx, `SELECT
		fn_forge_origin_eligible(w, jsonb_populate_record(NULL::runs,
			to_jsonb(r) || '{"repo_id":null,"kind":"job"}'::jsonb), now())
		AND fn_forge_origin_eligible(w, jsonb_populate_record(NULL::runs,
			to_jsonb(r) || '{"repo_id":null,"kind":"chat"}'::jsonb), now())
		AND fn_forge_origin_eligible(w, jsonb_populate_record(NULL::runs,
			to_jsonb(r) || jsonb_build_object('egress_profile_id',gen_random_uuid())), now())
		FROM workers w CROSS JOIN runs r WHERE w.id=$1 AND r.id=$2`, w, first).Scan(&exempt); err != nil || !exempt {
		t.Fatalf("noncloning snapshot exemptions: %v, %v", exempt, err)
	}
}

func TestForgeOriginAdmissionDueOnceLiveDB(t *testing.T) {
	fx := newRollAffinityFixture(t)
	w := fx.worker("claimant", 0, false)
	first, second := fx.unclaimedRun(), fx.unclaimedRun()
	forgeAdmissionCooldown(fx, w, true)
	type result struct {
		run store.Run
		err error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	// Exactly two attempts, independent except for the worker lock; both finish
	// before this test returns. One refusal does not block its sibling.
	for range 2 {
		go func() {
			<-start
			run, err := forgeAdmissionClaim(fx, fx.q, w)
			results <- result{run, err}
		}()
	}
	close(start)
	// Drain both bounded calls before any assertion can terminate the test.
	both := [2]result{<-results, <-results}
	var winner uuid.UUID
	for _, got := range both {
		if got.err == nil {
			if winner != uuid.Nil {
				t.Fatalf("two due claims: %s and %s", winner, got.run.ID)
			}
			winner = got.run.ID
		} else if !errors.Is(got.err, pgx.ErrNoRows) {
			t.Fatalf("claim: %v", got.err)
		}
	}
	if winner != first && winner != second {
		t.Fatalf("no due winner: %s", winner)
	}
	var exact bool
	if err := fx.pool.QueryRow(fx.ctx, `SELECT
		c.reservation_run_id=$1 AND c.reservation_generation=1
		AND c.reservation_incarnation=c.incarnation
		AND c.reservation_observation_version=c.observation_version
		AND m.reservation_incarnation=c.reservation_incarnation
		AND m.reservation_version=c.reservation_version
		AND m.generation=c.reservation_generation
		AND m.connection_id=a.connection_id AND m.origin=a.origin
		AND (SELECT sum(claim_generation) FROM runs WHERE id IN ($2,$3))=1
		AND (SELECT count(*) FROM recovery_custody_holds WHERE run_id IN ($2,$3))=1
		FROM forge_origin_cooldowns c
		JOIN run_forge_origins m ON m.run_id=c.reservation_run_id
		JOIN authorized_forge_origins a ON a.repo_id=$4
		WHERE c.worker_id=$5`, winner, first, second, fx.repoID, w).Scan(&exact); err != nil || !exact {
		t.Fatalf("reservation/metadata/G/hold mismatch: %v, %v", exact, err)
	}
}

func TestForgeOriginAdmissionLifecycleLiveDB(t *testing.T) {
	for _, release := range []string{"requeue", "generation", "delete"} {
		t.Run(release, func(t *testing.T) {
			fx := newRollAffinityFixture(t)
			w := fx.worker("claimant", 0, false)
			id := fx.unclaimedRun()
			forgeAdmissionCooldown(fx, w, true)
			got, err := forgeAdmissionClaim(fx, fx.q, w)
			if err != nil || got.ID != id {
				t.Fatalf("due claim: %s, %v", got.ID, err)
			}
			// Neither running nor human waits, even after another failure
			// advances the current observation, releases the admitted probe.
			mustExec(fx.ctx, t, fx.pool, `UPDATE forge_origin_cooldowns
				SET observation_version=observation_version+1, cooled_until=now()+interval '1 day'
				WHERE worker_id=$1`, w)
			if _, err := fx.q.GetRunClaimContext(fx.ctx, id); err != nil {
				t.Fatalf("new cooldown revoked an admitted claim: %v", err)
			}
			for _, status := range []string{"running", "awaiting_approval", "awaiting_input", "awaiting_followup"} {
				mustExec(fx.ctx, t, fx.pool, "UPDATE runs SET status=$1 WHERE id=$2", status, id)
				var retained bool
				if err := fx.pool.QueryRow(fx.ctx, `SELECT reservation_run_id=$1
					AND reservation_observation_version=1 AND observation_version=2
					FROM forge_origin_cooldowns WHERE worker_id=$2`, id, w).Scan(&retained); err != nil || !retained {
					t.Fatalf("%s lost exact reservation: %v, %v", status, retained, err)
				}
			}
			switch release {
			case "requeue":
				mustExec(fx.ctx, t, fx.pool, "UPDATE runs SET status='queued',claim_released_at=now() WHERE id=$1", id)
			case "generation":
				mustExec(fx.ctx, t, fx.pool, "UPDATE runs SET status='claimed',claim_generation=claim_generation+1 WHERE id=$1", id)
			case "delete":
				// A real delete must first dispose of the independent custody FK.
				mustExec(fx.ctx, t, fx.pool, "DELETE FROM recovery_custody_holds WHERE run_id=$1", id)
				mustExec(fx.ctx, t, fx.pool, "DELETE FROM runs WHERE id=$1", id)
			}
			var released bool
			if err := fx.pool.QueryRow(fx.ctx, `SELECT reservation_run_id IS NULL
				AND reservation_generation IS NULL AND reservation_incarnation IS NULL
				AND reservation_version IS NULL AND reservation_observation_version IS NULL
				AND observation_version=2 AND cooled_until>now()
				FROM forge_origin_cooldowns WHERE worker_id=$1`, w).Scan(&released); err != nil || !released {
				t.Fatalf("cleanup lost evidence or retained reservation: %v, %v", released, err)
			}
			if release == "generation" {
				var replaced bool
				if err := fx.pool.QueryRow(fx.ctx, `SELECT generation=2 AND reservation_incarnation IS NULL
					AND reservation_version IS NULL FROM run_forge_origins WHERE run_id=$1`, id).Scan(&replaced); err != nil || !replaced {
					t.Fatalf("replacement overwrote OLD before cleanup: %v, %v", replaced, err)
				}
			}
		})
	}
}

func TestForgeOriginAdmissionSpreadLiveDB(t *testing.T) {
	fx := newRollAffinityFixture(t)
	busy := fx.worker("busy", 0, false)
	idle := fx.worker("idle", 0, false)
	active := fx.unclaimedRun()
	mustExec(fx.ctx, t, fx.pool, "UPDATE runs SET worker_id=$1,status='running',claim_generation=1 WHERE id=$2", busy, active)
	target := fx.unclaimedRun()
	forgeAdmissionCooldown(fx, idle, false)
	got, err := forgeAdmissionClaim(fx, fx.q, busy)
	if err != nil || got.ID != target {
		t.Fatalf("deferred to cooled idle peer: %s, %v", got.ID, err)
	}
	// Positive control: with an eligible idle peer the existing spread fence
	// still defers a fresh run. Origin admission does not weaken that fence.
	mustExec(fx.ctx, t, fx.pool, "DELETE FROM forge_origin_cooldowns WHERE worker_id=$1", idle)
	next := fx.unclaimedRun()
	if _, err := forgeAdmissionClaim(fx, fx.q, busy); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("did not defer to eligible idle peer: %v", err)
	}
	if got, err := forgeAdmissionClaim(fx, fx.q, idle); err != nil || got.ID != next {
		t.Fatalf("eligible idle peer failed to claim: %s, %v", got.ID, err)
	}
}

func TestForgeOriginAdmissionExactCleanupLiveDB(t *testing.T) {
	fx := newRollAffinityFixture(t)
	w := fx.worker("claimant", 0, false)
	id := fx.unclaimedRun()
	forgeAdmissionCooldown(fx, w, true)
	if _, err := forgeAdmissionClaim(fx, fx.q, w); err != nil {
		t.Fatal(err)
	}
	// An obsolete snapshot must not clear a newer reservation version even
	// when run, generation and incarnation happen to match.
	mustExec(fx.ctx, t, fx.pool, "UPDATE forge_origin_cooldowns SET reservation_version=reservation_version+1 WHERE worker_id=$1", w)
	mustExec(fx.ctx, t, fx.pool, "UPDATE runs SET status='queued' WHERE id=$1", id)
	var retained bool
	if err := fx.pool.QueryRow(fx.ctx, `SELECT reservation_run_id=$1
		AND reservation_generation=1 AND reservation_version=2
		FROM forge_origin_cooldowns WHERE worker_id=$2`, id, w).Scan(&retained); err != nil || !retained {
		t.Fatalf("obsolete snapshot cleared a different tuple: %v, %v", retained, err)
	}
}

func TestForgeOriginAdmissionRollbackLiveDB(t *testing.T) {
	fx := newRollAffinityFixture(t)
	w := fx.worker("claimant", 0, false)
	id := fx.unclaimedRun()
	forgeAdmissionCooldown(fx, w, true)
	tx, err := fx.pool.Begin(fx.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(fx.ctx) }()
	if _, err := forgeAdmissionClaim(fx, store.New(tx), w); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(fx.ctx); err != nil {
		t.Fatal(err)
	}
	var reverted bool
	if err := fx.pool.QueryRow(fx.ctx, `SELECT r.status='queued' AND r.claim_generation=0
		AND c.reservation_run_id IS NULL
		AND NOT EXISTS (SELECT 1 FROM run_forge_origins WHERE run_id=r.id)
		AND NOT EXISTS (SELECT 1 FROM recovery_custody_holds WHERE run_id=r.id)
		FROM runs r JOIN forge_origin_cooldowns c ON c.worker_id=$2 WHERE r.id=$1`, id, w).Scan(&reverted); err != nil || !reverted {
		t.Fatalf("claim rollback was not atomic: %v, %v", reverted, err)
	}
	if got, err := forgeAdmissionClaim(fx, fx.q, w); err != nil || got.ID != id {
		t.Fatalf("rolled-back reservation blocked retry: %s, %v", got.ID, err)
	}
}
