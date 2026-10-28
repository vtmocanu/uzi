package store_test

import (
	"context"
	"fmt"
	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/store"
	"os"
	"testing"
	"time"
)

func TestRetainedParkRollbackLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("fixture migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("fixture open pool: %v", err)
	}
	defer pool.Close()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		if err := tx.Rollback(cleanup); err != nil {
			t.Errorf("fixture rollback: %v", err)
		}
	}()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := tx.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("fixture SQL: %v", err)
		}
	}
	assertSQL := func(name, sql string, args ...any) {
		t.Helper()
		var ok bool
		if err := tx.QueryRow(ctx, sql, args...).Scan(&ok); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !ok {
			t.Fatalf("%s", name)
		}
	}
	snapshot := func(sql string, id uuid.UUID) string {
		t.Helper()
		var value string
		if err := tx.QueryRow(ctx, sql, id).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}

	user, conn, repo, worker := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	exec("INSERT INTO users(id,email,password_hash) VALUES($1,$2,'x')", user, fmt.Sprintf("rollback-%s@e2e", user))
	exec(`INSERT INTO forge_connections(id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext)
		VALUES($1,$2,'gitlab','https://forge.e2e','bot',1,$3)`, conn, user, []byte{1})
	exec(`INSERT INTO repos(id,connection_id,forge_project_id,path_with_namespace,web_url,default_branch,enabled)
		VALUES($1,$2,1,'g/r','https://forge.e2e/g/r','main',true)`, repo, conn)
	nonce := "rollback-incarnation-" + worker.String()
	exec(`INSERT INTO workers(id,user_id,name,token_hash,max_concurrent_runs,snapshot_register_nonce)
		VALUES($1,$2,'rollback-worker',$3,4,$4)`, worker, user, "hash-"+worker.String(), nonce)

	ids := []uuid.UUID{uuid.New(), uuid.New(), uuid.New()}
	for i, id := range ids {
		exec(`INSERT INTO runs(id,user_id,repo_id,worker_id,kind,issue_iid,issue_title,issue_description,status,
   retained_park_count,claim_generation,claim_released_at,recovery_wait_cause,recovery_retry_not_before)
   VALUES($1,$2,$3,$4,'issue',$5,'rollback','fixture',$6,3,7,now(),'retained_recovery_exhausted',
   CASE WHEN $5::bigint=1 THEN now()+interval '1 hour' END)`, id, user, repo, worker, i+1, []string{"recovery_wait", "recovery_wait", "queued"}[i])
	}
	const preserved = `SELECT (to_jsonb(r)-ARRAY['retained_park_count','status','fail_origin','failure_reason','finished_at','status_since','updated_at','recovery_wait_cause','recovery_retry_not_before'])::text FROM runs r WHERE id=$1`
	before := map[uuid.UUID]string{}
	for _, id := range ids {
		before[id] = snapshot(preserved, id)
	}
	// Roll back the additive cause first: earlier LiveDB tests may leave its rows.
	for _, stmt := range migrationDownStatements(t, "00372_retained_recovery_adoption_failed.sql") {
		exec(stmt)
	}
	for _, stmt := range migrationDownStatements(t, "00362_retained_recovery_parks.sql") {
		exec(stmt)
	}
	assertSQL("column removed", `SELECT NOT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='runs' AND column_name='retained_park_count')`)
	for i, id := range ids {
		assertSQL("obsolete cause and retry removed", `SELECT recovery_wait_cause IS NULL AND recovery_retry_not_before IS NULL FROM runs WHERE id=$1`, id)
		if i < 2 {
			assertSQL("park terminalized before narrowing check", `SELECT status='failed' AND fail_origin='worker_lost' AND finished_at IS NOT NULL FROM runs WHERE id=$1`, id)
		}
		if snapshot(preserved, id) != before[id] {
			t.Fatal("rollback changed affinity, execution fence or unrelated data")
		}
	}
	for _, stmt := range migrationUpStatements(t, "00362_retained_recovery_parks.sql") {
		exec(stmt)
	}
	for _, id := range ids {
		assertSQL("re-up defaults to zero", `SELECT retained_park_count=0 FROM runs WHERE id=$1`, id)
	}
	exec("SAVEPOINT negative_counter")
	_, err = tx.Exec(ctx, "UPDATE runs SET retained_park_count=-1 WHERE id=$1", ids[0])
	if err == nil {
		t.Fatal("negative retained count admitted")
	}
	exec("ROLLBACK TO SAVEPOINT negative_counter")
	exec("UPDATE runs SET status='recovery_wait',recovery_wait_cause='retained_recovery_exhausted' WHERE id=$1", ids[0])
	assertSQL("attention protects retained park", `SELECT fn_custody_attention('open',true,false,'available','recovery_wait','retained_recovery_exhausted')='source_only'`)
	assertSQL("discarded takes precedence", `SELECT fn_custody_attention('discarded',true,false,'available','recovery_wait','retained_recovery_exhausted')='discarded'`)
	assertSQL("released takes precedence", `SELECT fn_custody_attention('released',true,false,'available','recovery_wait','retained_recovery_exhausted')='released'`)
}
