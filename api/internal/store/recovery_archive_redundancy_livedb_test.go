package store_test

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #2625: Down restores the ADR-2417 capture guard exactly, drops the proof columns, and
// leaves the table able to take the migration again even when a capture carries the reason.
func TestRecoveryArchiveRedundancyMigrationRoundTripLiveDB(t *testing.T) {
	ctx, dsn := standIsolatedDB(t, "archive_redundancy_")
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	user, conn, repo, worker, run, hold := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users(id,email,password_hash) VALUES($1,$2,'x')`, user, fmt.Sprintf("redundancy-%s@example.com", user))
	mustExec(ctx, t, pool, `INSERT INTO forge_connections(id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext)
		VALUES($1,$2,'github','https://example.com','bot',1,$3)`, conn, user, []byte{1})
	mustExec(ctx, t, pool, `INSERT INTO repos(id,connection_id,forge_project_id,path_with_namespace,web_url,default_branch,enabled)
		VALUES($1,$2,1,'g/r','https://example.com/g/r','main',true)`, repo, conn)
	mustExec(ctx, t, pool, `INSERT INTO workers(id,user_id,name,token_hash,status) VALUES($1,$2,'w',$3,'online')`, worker, user, worker[:])
	mustExec(ctx, t, pool, `INSERT INTO runs(id,user_id,repo_id,worker_id,issue_iid,issue_title,issue_description,status,claim_generation)
		VALUES($1,$2,$3,$4,1,'t','d','completed',1)`, run, user, repo, worker)
	mustExec(ctx, t, pool, `INSERT INTO recovery_custody_holds(id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,inventory_guarded)
		VALUES($1,$2,$3,$4,1,'released',$5,'ident',true)`, hold, user, repo, run, worker)
	proofCaptures := []uuid.UUID{uuid.New(), uuid.New()}
	for i, state := range []string{"expired", "discarded"} {
		mustExec(ctx, t, pool, `INSERT INTO recovery_captures(id,hold_id,run_id,user_id,original_worker_id,original_worker_identity,source_sha,idempotency_key,state,reason,redundancy_proof)
			VALUES($1,$2,$3,$4,$5,'ident',$6,$7,$8,'published_redundant','{"anchor_head":"x"}'::jsonb)`,
			proofCaptures[i], hold, run, user, worker, strings.Repeat("a", 40), fmt.Sprintf("k%d", i), state)
	}

	columns := func() int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_name='recovery_captures'
			AND column_name IN ('redundancy_proof','redundancy_refused_at','redundancy_refusal')`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	guard := func() string {
		var def string
		if err := pool.QueryRow(ctx, `SELECT pg_get_functiondef('recovery_inventory_capture_guard'::regproc)`).Scan(&def); err != nil {
			t.Fatal(err)
		}
		return def
	}
	if columns() != 3 || !strings.Contains(guard(), "redundancy") {
		t.Fatalf("migrated shape: columns=%d", columns())
	}
	if err := store.MigrateDownTo(ctx, dsn, 320); err != nil {
		t.Fatalf("Down: %v", err)
	}
	if n := columns(); n != 0 {
		t.Fatalf("Down left %d proof columns", n)
	}
	// The restored body is byte-for-byte the one migration 00305 created.
	orig, err := os.ReadFile("migrations/00305_recovery_inventory.sql")
	if err != nil {
		t.Fatal(err)
	}
	_, afterHeader, ok := strings.Cut(string(orig), "CREATE FUNCTION recovery_inventory_capture_guard() RETURNS trigger LANGUAGE plpgsql AS $$")
	wantBody, _, ok2 := strings.Cut(afterHeader, "$$;")
	if !ok || !ok2 {
		t.Fatal("00305 capture guard not found")
	}
	if def := guard(); !strings.Contains(def, "$function$"+wantBody+"$function$") {
		t.Fatalf("Down did not restore the 00305 guard body:\n%s", def)
	}
	var constraints int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_constraint WHERE conname LIKE 'recovery_captures_redundancy_%'`).Scan(&constraints); err != nil || constraints != 0 {
		t.Fatalf("Down left constraints: %d %v", constraints, err)
	}
	for _, id := range proofCaptures {
		var reason string
		if err := pool.QueryRow(ctx, `SELECT reason FROM recovery_captures WHERE id=$1`, id).Scan(&reason); err != nil || reason != "published_redundant_rolled_back" {
			t.Fatalf("capture %s reason after Down: %q %v", id, reason, err)
		}
	}
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("Up after Down: %v", err)
	}
	if columns() != 3 || !strings.Contains(guard(), "redundancy") {
		t.Fatalf("re-applied shape: columns=%d", columns())
	}
}
