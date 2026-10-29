package store_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func assertForgeCheckerAdmission(t *testing.T, ctx context.Context, pool *pgxpool.Pool, worker, child uuid.UUID, stage string) {
	t.Helper()
	check := func(want bool) {
		t.Helper()
		var static, available bool
		err := pool.QueryRow(ctx, `SELECT
            COALESCE(fn_cross_check_child_eligible(w,r,false,'any',$3,now(),now()-interval '1 day'),false),
            COALESCE(fn_cross_check_child_eligible(w,r,true,'any',$3,now(),now()-interval '1 day'),false)
            FROM workers w CROSS JOIN runs r WHERE w.id=$1 AND r.id=$2`, worker, child, stage).Scan(&static, &available)
		if err != nil || !static || available != want {
			t.Fatalf("%s static=%v available=%v want=%v err=%v", stage, static, available, want, err)
		}
	}
	check(true)
	mustExec(ctx, t, pool, `INSERT INTO forge_origin_cooldowns(user_id,worker_id,origin,cooled_until,last_reason,observed_at)
        SELECT r.user_id,$1,a.origin,now()+interval '1 day','transient_clone_fetch_exhausted',now()
        FROM runs r JOIN authorized_forge_origins a ON a.repo_id=r.repo_id AND a.user_id=r.user_id WHERE r.id=$2`, worker, child)
	check(false)
	mustExec(ctx, t, pool, "UPDATE forge_origin_cooldowns SET cooled_until=now()-interval '1 day' WHERE worker_id=$1", worker)
	check(true)
	mustExec(ctx, t, pool, `UPDATE forge_origin_cooldowns SET reservation_run_id=$2,reservation_generation=1,
        reservation_incarnation=incarnation,reservation_version=observation_version,
        reservation_observation_version=observation_version WHERE worker_id=$1`, worker, child)
	check(false)
}

func TestForgeOriginCheckerAdmissionLiveDB(t *testing.T) {
	t.Run("plan", func(t *testing.T) {
		fx := newFleetFixture(t)
		worker := fx.worker("checker", capOf(4), false)
		mustExec(fx.ctx, t, fx.pool, "UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", worker, claudeCheckerProtocols(true, 1))
		child, _ := seedClaudeChild(fx, fx.worker("lead", nil, false), 1)
		assertForgeCheckerAdmission(t, fx.ctx, fx.pool, worker, child, "plan")
	})
	t.Run("code", func(t *testing.T) {
		fx := codeCrossCheckFixture(t)
		if err := insertCodeFixture(t, fx, codeFixtureHead, codeFixtureBase, "pending", nil); err != nil {
			t.Fatal(err)
		}
		assertForgeCheckerAdmission(t, context.Background(), fx.f.pool, fx.f.workerID, fx.checkerID, "code")
	})
}
