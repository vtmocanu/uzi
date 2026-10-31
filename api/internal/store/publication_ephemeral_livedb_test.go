package store_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// An enrolled queued run with no handoff is claimable only by its custodian, so a new ephemeral
// worker could never serve it: ClaimRun bars it and the reaper removes the worker, which would
// loop. The three provisioning triggers must not list it (issue #1324 D5).
func TestPublicationEnrolledRunNotListedForEphemeralLiveDB(t *testing.T) {
	fx := newFleetFixture(t)
	optInEphemeral(fx)
	at := time.Now().UTC().Truncate(time.Microsecond)
	capped := uuid.New()
	mustExec(fx.ctx, t, fx.pool, `INSERT INTO workers
		(id,user_id,name,token_hash,status,last_heartbeat_at,max_concurrent_runs,created_at,updated_at)
		VALUES ($1,$2,'capable-at-cap',$3,'online',$4,1,$4,$4)`, capped, fx.userID, capped[:], at)
	active := uuid.New()
	mustExec(fx.ctx, t, fx.pool, `INSERT INTO runs
		(id,user_id,repo_id,issue_iid,issue_title,issue_description,status,worker_id,status_since,created_at)
		VALUES ($1,$2,$3,$4,'t','d','running',$5,$6,$6)`, active, fx.userID, fx.repoID, fx.nextIID(), capped, at.Add(-time.Hour))
	gap := queuedRunWithCaps(fx, []string{"gh"})
	sat := queuedRunWithCaps(fx, nil)
	profile := uuid.New()
	mustExec(fx.ctx, t, fx.pool, `INSERT INTO egress_profiles (id, name, hosts) VALUES ($1, $2, '{docs.example.com}')`,
		profile, "pub-"+profile.String()[:20])
	iso := uuid.New()
	mustExec(fx.ctx, t, fx.pool, `INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, egress_profile_id)
		VALUES ($1, $2, $3, 'issue', $4, 't', 'd', 'queued', $5)`, iso, fx.userID, fx.repoID, fx.nextIID(), profile)
	t.Cleanup(func() {
		_, _ = fx.pool.Exec(fx.ctx, "DELETE FROM runs WHERE user_id=$1", fx.userID)
		_, _ = fx.pool.Exec(fx.ctx, "DELETE FROM egress_profiles WHERE id=$1", profile)
		_, _ = fx.pool.Exec(fx.ctx, "DELETE FROM workers WHERE user_id=$1", fx.userID)
		_, _ = fx.pool.Exec(fx.ctx, "UPDATE users SET ephemeral_workers_enabled=false WHERE id=$1", fx.userID)
	})
	iv := leaseInterval(0)
	lists := func() (map[uuid.UUID]bool, map[uuid.UUID]bool, map[uuid.UUID]bool) {
		return listedUnplaceable(fx, 1000, iv), listedSaturation(fx, 1000, iv), listedIsolated(fx, 1000)
	}
	u, s, i := lists()
	if !u[gap] || !s[sat] || !i[iso] {
		t.Fatalf("control: unplaceable=%v saturation=%v isolated=%v, want all three listed", u[gap], s[sat], i[iso])
	}
	enroll := func(run uuid.UUID, handoff any) {
		tx, err := fx.pool.Begin(fx.ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(fx.ctx) }()
		if _, err := tx.Exec(fx.ctx, `SELECT set_config('uzi.publication_resume_api','v1',true)`); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(fx.ctx, `INSERT INTO run_publication_resume (run_id, enrollment_seq, claim_generation, worker_id, intent, state, handoff)
			VALUES ($1, 1, 1, $2, json_build_object('run_id', $1::text), 'active', $3::json)`, run, capped, handoff); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(fx.ctx); err != nil {
			t.Fatal(err)
		}
	}
	enroll(gap, nil)
	enroll(sat, nil)
	enroll(iso, nil)
	u, s, i = lists()
	if u[gap] || s[sat] || i[iso] {
		t.Fatalf("enrolled no-handoff runs listed: unplaceable=%v saturation=%v isolated=%v", u[gap], s[sat], i[iso])
	}
	// With a recorded handoff a peer may claim, so the run is demand again.
	mustExec(fx.ctx, t, fx.pool, `UPDATE run_publication_resume SET handoff='{"checkpoint_tip":"cccccccccccccccccccccccccccccccccccccccc"}'::json WHERE run_id = ANY($1)`, []uuid.UUID{gap, sat, iso})
	u, s, i = lists()
	if !u[gap] || !s[sat] || !i[iso] {
		t.Fatalf("handoff runs not listed: unplaceable=%v saturation=%v isolated=%v", u[gap], s[sat], i[iso])
	}
}
