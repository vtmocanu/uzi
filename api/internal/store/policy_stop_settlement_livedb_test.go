package store

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// This family uses its own database so it can exercise actual goose rollback.
func TestPolicyStopSettlementMigrationLiveDB(t *testing.T) {
	base := os.Getenv("UZI_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	admin, err := OpenPool(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	name := "cdr_stop_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		if _, err := admin.Exec(cleanup, "DROP DATABASE "+name+" WITH (FORCE)"); err != nil {
			t.Error(err)
		}
	}()
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	dsn := u.String()
	holdVersion := migrationVersionByName(t, "policy_hold")
	stopVersion := migrationVersionByName(t, "policy_stop_resolution")
	if err := MigrateTo(ctx, dsn, holdVersion); err != nil {
		t.Fatal(err)
	}
	pool, err := OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	reject := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err == nil {
			t.Fatalf("accepted invalid settlement mutation: %s", sql)
		}
	}
	owner, run, other := uuid.New(), uuid.New(), uuid.New()
	exec("INSERT INTO users(id,email,password_hash) VALUES($1,$2,'x')", owner, owner.String()+"@example.com")
	for _, id := range []uuid.UUID{run, other} {
		exec("INSERT INTO runs(id,user_id,kind,status,harness,issue_title,issue_description) VALUES($1,$2,'chat','paused','codex','fixture','fixture')", id, owner)
	}
	var scope, wrongScope, human int64
	for id, dest := range map[uuid.UUID]*int64{run: &scope, other: &wrongScope} {
		if err := pool.QueryRow(ctx, "INSERT INTO run_user_inputs(run_id,kind,body) VALUES($1,'scope','ceiling') RETURNING id", id).Scan(dest); err != nil {
			t.Fatal(err)
		}
	}
	if err := pool.QueryRow(ctx, "INSERT INTO run_user_inputs(run_id,kind,body,gate_binding,gate_revision,consumed_at,consumed_claim_generation) VALUES($1,'revise_plan',' exact feedback ','bound',7,now(),1) RETURNING id", run).Scan(&human); err != nil {
		t.Fatal(err)
	}
	if err := MigrateTo(ctx, dsn, stopVersion); err != nil {
		t.Fatal(err)
	}
	// New columns must remain absent on historical rows until an explicit settlement.
	var untouched int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM run_user_inputs WHERE id=$1 AND settled_at IS NULL AND superseded_by_stop_input_id IS NULL AND applied_at IS NULL", human).Scan(&untouched); err != nil || untouched != 1 {
		t.Fatalf("additive migration changed historical receipt: count=%d err=%v", untouched, err)
	}
	reject("UPDATE run_user_inputs SET disposition='superseded_by_stop',settled_at=now(),superseded_by_stop_input_id=$2 WHERE id=$1", human, wrongScope)
	reject("UPDATE run_user_inputs SET disposition='superseded_by_stop',settled_at=now(),superseded_by_stop_input_id=$1 WHERE id=$1", human)
	reject("UPDATE run_user_inputs SET disposition='superseded_by_stop' WHERE id=$1", human)
	exec("UPDATE run_user_inputs SET disposition='superseded_by_stop',settled_at=now(),superseded_by_stop_input_id=$2 WHERE id=$1", human, scope)
	var honest int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM run_user_inputs WHERE id=$1 AND body=' exact feedback ' AND gate_revision=7 AND consumed_claim_generation=1 AND consumed_at IS NOT NULL AND applied_at IS NULL AND included_at IS NULL AND settled_at IS NOT NULL AND superseded_by_stop_input_id=$2", human, scope).Scan(&honest); err != nil || honest != 1 {
		t.Fatalf("settlement fabricated receipt or lost evidence: count=%d err=%v", honest, err)
	}
	for _, assignment := range []string{"disposition=NULL", "settled_at=NULL", "body='changed'", "gate_revision=8", "consumed_at=consumed_at+interval '1 second'", "applied_at=now()", "included_at=now()", "consumed_claim_generation=2", "run_id='" + other.String() + "'"} {
		reject("UPDATE run_user_inputs SET "+assignment+" WHERE id=$1", human)
	}
	reject("DELETE FROM run_user_inputs WHERE id=$1", human)
	// A scope disposition may settle normally; its identity and exact body stay fixed.
	exec("UPDATE run_user_inputs SET disposition='applied' WHERE id=$1", scope)
	reject("UPDATE run_user_inputs SET body='changed' WHERE id=$1", scope)
	reject("DELETE FROM run_user_inputs WHERE id=$1", scope)
	cc := uuid.New()
	exec("INSERT INTO cross_checks(id,lead_run_id,stage,round,lead_claim_generation,plan_md,milestones,size_class,base_commit,candidate_digest,verdict,reason_class,findings,deadline_at,checker_harness,checker_model,checker_model_source) VALUES($1,$2,'plan',1,1,'candidate','[]','s','base',$3,'revise','revise','[{\"message\":\"exact automatic feedback\"}]',now(),'codex','fixture','worker default')", cc, run, []byte{1})
	reject("UPDATE cross_checks SET revision_disposition='superseded_by_stop',revision_settled_at=now(),revision_superseded_by_stop_input_id=$2 WHERE id=$1", cc, wrongScope)
	exec("UPDATE cross_checks SET revision_disposition='superseded_by_stop',revision_settled_at=now(),revision_superseded_by_stop_input_id=$2 WHERE id=$1", cc, scope)
	for _, assignment := range []string{"verdict='approve',reason_class='approve'", "findings='[]'", "candidate_digest=decode('02','hex')", "checker_model='different'", "revision_disposition=NULL", "revision_settled_at=revision_settled_at+interval '1 second'", "lead_claim_generation=2"} {
		reject("UPDATE cross_checks SET "+assignment+" WHERE id=$1", cc)
	}
	reject("DELETE FROM cross_checks WHERE id=$1", cc)
	// Resource bookkeeping is independent of the immutable revision evidence.
	exec("UPDATE cross_checks SET wait_credited=true WHERE id=$1", cc)
	if err := MigrateDownTo(ctx, dsn, holdVersion); err == nil {
		t.Fatal("Down erased new settlement semantics")
	}
	// Run deletion remains the ordinary retention boundary and cascades the evidence.
	exec("DELETE FROM runs WHERE id=$1", run)
	exec("UPDATE runs SET policy_revision_source='{\"kind\":\"human\",\"input_id\":1}' WHERE id=$1", other)
	if err := MigrateDownTo(ctx, dsn, holdVersion); err == nil {
		t.Fatal("Down erased source provenance")
	}
	exec("UPDATE runs SET policy_revision_source=NULL WHERE id=$1", other)
	reject("UPDATE runs SET policy_stop_resolution='{}' WHERE id=$1", other)
	exec("UPDATE runs SET policy_stop_resolution=jsonb_build_object('version',1,'scope_input_id',$2::bigint) WHERE id=$1", other, wrongScope)
	reject("UPDATE runs SET policy_stop_resolution=NULL WHERE id=$1", other)
	reject("UPDATE runs SET policy_revision_source='{}' WHERE id=$1", other)
	reject("DELETE FROM run_user_inputs WHERE id=$1", wrongScope)
	if err := MigrateDownTo(ctx, dsn, holdVersion); err == nil {
		t.Fatal("Down erased accepted resolution")
	}
	exec("DELETE FROM runs WHERE id=$1", other)
	if err := MigrateDownTo(ctx, dsn, holdVersion); err != nil {
		t.Fatal(err)
	}
	if err := MigrateTo(ctx, dsn, stopVersion); err != nil {
		t.Fatal(err)
	}
}
