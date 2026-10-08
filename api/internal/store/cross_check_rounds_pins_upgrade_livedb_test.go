package store_test

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// A separate database is necessary: head-schema fixtures cannot exercise backfill.
func TestCrossCheckRoundsPinsUpgradeLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; disposable Postgres required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	name := "rounds_pins_upgrade_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	admin, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	admin.Close()
	var pool *pgxpool.Pool
	t.Cleanup(func() {
		if pool != nil {
			pool.Close()
		}
		cleanupCtx, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		a, e := store.OpenPool(cleanupCtx, dsn)
		if e != nil {
			t.Errorf("cleanup admin: %v", e)
			return
		}
		defer a.Close()
		if _, e = a.Exec(cleanupCtx, "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); e != nil {
			t.Errorf("cleanup database: %v", e)
		}
	})
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	if err = store.MigrateTo(ctx, u.String(), 311); err != nil {
		t.Fatal(err)
	}
	pool, err = store.OpenPool(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	assert := func(label, sql string, args ...any) {
		t.Helper()
		var ok bool
		if e := pool.QueryRow(ctx, sql, args...).Scan(&ok); e != nil || !ok {
			t.Fatalf("%s: ok=%v err=%v", label, ok, e)
		}
	}
	assert("311 pins present rounds absent", `SELECT
  EXISTS(SELECT 1 FROM information_schema.columns WHERE table_name='cross_checks' AND column_name='checker_model_source')
  AND EXISTS(SELECT 1 FROM information_schema.columns WHERE table_name='cross_checks' AND column_name='checker_effort_source')
  AND NOT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_name='cross_checks' AND column_name='wait_credited')
  AND NOT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_name='cross_checks' AND column_name='automatic_rounds_enabled')
  AND to_regclass('public.user_cross_check_pins') IS NOT NULL`)
	user, connection, repo := uuid.New(), uuid.New(), uuid.New()
	mustExec(ctx, t, pool, "INSERT INTO users(id,email,password_hash) VALUES($1,$2,'x')", user, fmt.Sprintf("%s@e2e", user))
	mustExec(ctx, t, pool, `INSERT INTO forge_connections(id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext)
 VALUES($1,$2,'gitlab','https://gitlab.example','bot',1,$3)`, connection, user, []byte("fixture"))
	mustExec(ctx, t, pool, `INSERT INTO repos(id,connection_id,forge_project_id,path_with_namespace,web_url)
 VALUES($1,$2,1,'upgrade/repo','https://gitlab.example/upgrade/repo')`, repo, connection)
	mustExec(ctx, t, pool, "INSERT INTO user_cross_check_pins(user_id,stage,harness,model,effort) VALUES($1,'plan','codex','gpt-6-astra','xhigh')", user)
	type seeded struct {
		lead, child           uuid.UUID
		decided               bool
		model, effort, source any
	}
	var rows []seeded
	for _, decided := range []bool{false, true} {
		for _, values := range [][3]any{{"gpt-6-astra", "xhigh", "pin"}, {"gpt-6-sol", "high", "worker default"}, {nil, nil, nil}} {
			s := seeded{uuid.New(), uuid.New(), decided, values[0], values[1], values[2]}
			verdict, reason, childStatus := "pending", any(nil), "running"
			if decided {
				verdict, reason, childStatus = "revise", "revise", "completed"
			}
			mustExec(ctx, t, pool, `INSERT INTO runs(id,user_id,repo_id,issue_iid,issue_title,issue_description,status,claim_generation,budget_paused_seconds)
   VALUES($1,$2,$3,abs(hashtext($4)::bigint)+1,'lead','body','running',1,73)`, s.lead, user, repo, s.lead.String())
			mustExec(ctx, t, pool, `INSERT INTO runs(id,user_id,repo_id,kind,target_run_id,harness,report_only,budget_wall_seconds,issue_title,issue_description,status)
   VALUES($1,$2,$3,'cross_check',$4,'codex',true,1800,'checker','body',$5)`, s.child, user, repo, s.lead, childStatus)
			mustExec(ctx, t, pool, `INSERT INTO cross_checks(lead_run_id,stage,round,lead_claim_generation,plan_md,milestones,size_class,base_commit,
   candidate_digest,checker_run_id,checker_harness,checker_model,checker_effort,checker_model_source,checker_effort_source,
   verdict,reason_class,created_at,deadline_at,decided_at)
   VALUES($1,'plan',1,1,'historical plan','[]','s',repeat('a',40),$2,$3,'codex',$4,$5,$6,$6,$7,$8,
   now()-interval '20 seconds',now()+interval '10 minutes',CASE WHEN $7='pending' THEN NULL ELSE now() END)`,
				s.lead, []byte("historical digest"), s.child, s.model, s.effort, s.source, verdict, reason)
			rows = append(rows, s)
		}
	}
	if err = store.MigrateTo(ctx, u.String(), 312); err != nil {
		t.Fatal(err)
	}
	q := store.New(pool)
	for _, s := range rows {
		t.Run(fmt.Sprintf("decided=%v/source=%v", s.decided, s.source), func(t *testing.T) {
			cc, e := q.GetPlanCrossCheck(ctx, s.lead)
			if e != nil {
				t.Fatal(e)
			}
			if cc.WaitCredited != s.decided || cc.AutomaticRoundsEnabled || cc.AutomaticRevisionLimit != 0 ||
				cc.PlanMd.String != "historical plan" || string(cc.CandidateDigest) != "historical digest" {
				t.Fatalf("backfill changed history: %+v", cc)
			}
			assert("provenance preserved", `SELECT checker_model IS NOT DISTINCT FROM $2::text AND checker_effort IS NOT DISTINCT FROM $3::text
   AND checker_model_source IS NOT DISTINCT FROM $4::text AND checker_effort_source IS NOT DISTINCT FROM $4::text
   FROM cross_checks WHERE lead_run_id=$1`, s.lead, s.model, s.effort, s.source)
			assert("baseline credit preserved", "SELECT budget_paused_seconds=73 FROM runs WHERE id=$1", s.lead)
			if _, e = pool.Exec(ctx, "UPDATE cross_checks SET automatic_rounds_enabled=true,automatic_revision_limit=2 WHERE lead_run_id=$1", s.lead); e == nil {
				t.Fatal("historical snapshot widened")
			}
			mustExec(ctx, t, pool, "UPDATE runs SET claim_generation=2 WHERE id=$1", s.lead)
			settled, e := q.GetPlanCrossCheck(ctx, s.lead)
			if e != nil {
				t.Fatal(e)
			}
			var credit int32
			var status string
			if e = pool.QueryRow(ctx, "SELECT budget_paused_seconds FROM runs WHERE id=$1", s.lead).Scan(&credit); e != nil {
				t.Fatal(e)
			}
			if e = pool.QueryRow(ctx, "SELECT status FROM runs WHERE id=$1", s.child).Scan(&status); e != nil {
				t.Fatal(e)
			}
			if s.decided {
				if credit != 73 || status != "completed" || settled.Verdict != "revise" {
					t.Fatal("decided row credited or cancelled twice")
				}
			} else {
				assert("one pending credit", `SELECT r.budget_paused_seconds=73+CEIL(EXTRACT(EPOCH FROM (c.decided_at-c.created_at)))::int
    AND c.wait_credited AND c.verdict='failed' AND c.reason_class='superseded'
    FROM runs r JOIN cross_checks c ON c.lead_run_id=r.id WHERE r.id=$1`, s.lead)
				if status != "cancelled" {
					t.Fatalf("child status=%s", status)
				}
			}
			mustExec(ctx, t, pool, "UPDATE runs SET claim_generation=3 WHERE id=$1", s.lead)
			after, e := q.GetPlanCrossCheck(ctx, s.lead)
			if e != nil || !reflect.DeepEqual(settled, after) {
				t.Fatalf("second settlement changed evidence: %v", e)
			}
			assert("second settlement credit unchanged", "SELECT budget_paused_seconds=$2 FROM runs WHERE id=$1", s.lead, credit)
		})
	}
	// Historical false/zero budgets cannot be widened. A new lead gets an
	// independent eligible snapshot and may carry pinned evidence in round 2.
	lead, child := uuid.New(), uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO runs(id,user_id,repo_id,issue_iid,issue_title,issue_description,status,claim_generation,budget_paused_seconds,auto_approve,harness,plan_cross_check_required)
 VALUES($1,$2,$3,999999,'new lead','body','running',1,73,true,'claude',true)`, lead, user, repo)
	mustExec(ctx, t, pool, `INSERT INTO runs(id,user_id,repo_id,kind,target_run_id,harness,report_only,budget_wall_seconds,issue_title,issue_description,status)
 VALUES($1,$2,$3,'cross_check',$4,'codex',true,1800,'new checker','body','running')`, child, user, repo, lead)
	mustExec(ctx, t, pool, `INSERT INTO cross_checks(lead_run_id,stage,round,lead_claim_generation,plan_md,milestones,size_class,base_commit,candidate_digest,
 verdict,reason_class,decided_at,deadline_at,wait_credited,automatic_rounds_enabled,automatic_revision_limit)
 VALUES($1,'plan',1,1,'first','[]','s',repeat('a',40),$2,'revise','revise',now(),now()+interval '10 minutes',true,true,2)`, lead, []byte("first digest"))
	mustExec(ctx, t, pool, `INSERT INTO cross_checks(lead_run_id,stage,round,lead_claim_generation,plan_md,milestones,size_class,base_commit,candidate_digest,
 checker_run_id,checker_harness,checker_model,checker_effort,checker_model_source,checker_effort_source,created_at,deadline_at,
 automatic_rounds_enabled,automatic_revision_limit)
 VALUES($1,'plan',2,1,'second','[]','s',repeat('a',40),$2,$3,'codex','gpt-6-astra','xhigh','pin','pin',
 now()-interval '20 seconds',now()+interval '10 minutes',true,2)`, lead, []byte("second digest"), child)
	mustExec(ctx, t, pool, "UPDATE runs SET claim_generation=2 WHERE id=$1", lead)
	settled, e := q.GetPlanCrossCheck(ctx, lead)
	if e != nil || settled.Round != 2 || !settled.WaitCredited || settled.CheckerModelSource.String != "pin" ||
		settled.CheckerEffortSource.String != "pin" || settled.Verdict != "failed" {
		t.Fatalf("new round 2 settlement: %+v err=%v", settled, e)
	}
	assert("round 2 credits once", `SELECT r.budget_paused_seconds=73+CEIL(EXTRACT(EPOCH FROM (c.decided_at-c.created_at)))::int
 AND child.status='cancelled' FROM runs r JOIN cross_checks c ON c.lead_run_id=r.id
 JOIN runs child ON child.id=c.checker_run_id WHERE r.id=$1 AND c.round=2`, lead)
	var credit int32
	if e = pool.QueryRow(ctx, "SELECT budget_paused_seconds FROM runs WHERE id=$1", lead).Scan(&credit); e != nil {
		t.Fatal(e)
	}
	mustExec(ctx, t, pool, "UPDATE runs SET claim_generation=3 WHERE id=$1", lead)
	after, e := q.GetPlanCrossCheck(ctx, lead)
	if e != nil || !reflect.DeepEqual(settled, after) {
		t.Fatalf("round 2 resettlement changed evidence: %v", e)
	}
	assert("round 2 resettlement credit unchanged", "SELECT budget_paused_seconds=$2 FROM runs WHERE id=$1", lead, credit)
	assert("round 1 revise retained", "SELECT verdict='revise' AND reason_class='revise' AND wait_credited AND interrupted_at IS NULL FROM cross_checks WHERE lead_run_id=$1 AND round=1", lead)
	assert("settings retained", "SELECT model='gpt-6-astra' AND effort='xhigh' FROM user_cross_check_pins WHERE user_id=$1", user)
}
