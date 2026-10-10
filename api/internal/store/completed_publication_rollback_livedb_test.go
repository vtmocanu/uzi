package store_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestCompletedPublicationRollbackRoundTripLiveDB(t *testing.T) {
	ctx, dsn := standIsolatedDB(t, "publication_rollback_")
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	user, conn, repo, worker := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users(id,email,password_hash) VALUES($1,$2,'x')`,
		user, fmt.Sprintf("rollback-%s@example.com", user))
	mustExec(ctx, t, pool, `INSERT INTO forge_connections
		(id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext)
		VALUES($1,$2,'github','https://example.com','bot',1,$3)`, conn, user, []byte{1})
	mustExec(ctx, t, pool, `INSERT INTO repos
		(id,connection_id,forge_project_id,path_with_namespace,web_url,default_branch,enabled)
		VALUES($1,$2,1,'g/r','https://example.com/g/r','main',true)`, repo, conn)
	mustExec(ctx, t, pool, `INSERT INTO workers(id,user_id,name,token_hash,status,protocol_capabilities)
		VALUES($1,$2,'rollback',$3,'online',ARRAY['recovery_completed_publication_v1'])`,
		worker, user, worker[:])

	// All holds start open. Completion stamps and releases run through the installed
	// triggers; the archive fixture supplies the available, manifest-bound capture.
	holds := map[string]uuid.UUID{}
	for i, name := range []string{"published", "stamped", "archive", "settled", "no_adopted_source"} {
		run, hold := uuid.New(), uuid.New()
		holds[name] = hold
		mustExec(ctx, t, pool, `INSERT INTO runs
			(id,user_id,repo_id,worker_id,kind,issue_iid,issue_title,issue_description,status,claim_generation,mr_iid)
			VALUES($1,$2,$3,$4,'issue',$5,'rollback','body','claimed',1,42)`,
			run, user, repo, worker, i+1)
		mustExec(ctx, t, pool, `INSERT INTO recovery_custody_holds
			(id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,
			 live_worker_id,live_run_id,inventory_guarded)
			VALUES($1,$2,$3,$4,1,'open',$5,'rollback-worker',$5,$4,true)`,
			hold, user, repo, run, worker)
		execOne := func(sql string, args ...any) {
			t.Helper()
			tag, err := pool.Exec(ctx, sql, args...)
			if err != nil || tag.RowsAffected() != 1 {
				t.Fatalf("%s fixture transition: rows=%d err=%v", name, tag.RowsAffected(), err)
			}
		}
		if name == "published" || name == "stamped" {
			execOne(`UPDATE runs SET status='completed',completion_final_head=repeat('a',40) WHERE id=$1`, run)
			var stamped bool
			if err := pool.QueryRow(ctx, `SELECT completion_identity IS NOT NULL FROM recovery_custody_holds WHERE id=$1`,
				hold).Scan(&stamped); err != nil || !stamped {
				t.Fatalf("%s completion was not stamped: %v", name, err)
			}
			if name == "published" {
				execOne(`UPDATE recovery_custody_holds SET state='released',live_worker_id=NULL,live_run_id=NULL,
					final_disposition='completed_publication',release_evidence='completed_publication',
					completed_publication_receipt=completion_identity || jsonb_build_object('observed_branch_head',repeat('b',40)),
					released_at='2026-01-02 03:04:05+00' WHERE id=$1`, hold)
			} else {
				execOne(`UPDATE recovery_custody_holds SET completed_publication_reason='forge_timeout' WHERE id=$1`, hold)
			}
			continue
		}
		switch name {
		case "archive":
			execOne(`UPDATE runs SET status='failed' WHERE id=$1`, run)
			capture := uuid.New()
			mustExec(ctx, t, pool, `INSERT INTO recovery_captures
				(id,hold_id,run_id,user_id,original_worker_id,original_worker_identity,source_sha,idempotency_key,
				 state,manifest_bound,coverage_digest,local_replica_worker_id,ready_retention_seconds,expires_at)
				VALUES($1,$2,$3,$4,$5,'rollback-worker',repeat('c',40),'archive','available',true,
				 repeat('d',64),$5,3600,now()+interval '1 hour')`, capture, hold, run, user, worker)
			execOne(`UPDATE recovery_custody_holds SET state='released',live_worker_id=NULL,live_run_id=NULL,
				final_disposition='archive',release_evidence='archive',final_capture_id=$2,
				final_source_sha=repeat('c',40),final_coverage_digest=repeat('d',64),
				released_at='2026-01-02 03:04:05+00' WHERE id=$1`, hold, capture)
		case "settled":
			execOne(`UPDATE runs SET status='completed' WHERE id=$1`, run)
			execOne(`UPDATE recovery_custody_holds SET state='released',live_worker_id=NULL,live_run_id=NULL,
				final_disposition='settled',release_evidence='publication',
				final_coverage_digest='e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855',
				released_at='2026-01-02 03:04:05+00' WHERE id=$1`, hold)
		case "no_adopted_source":
			execOne(`UPDATE recovery_custody_holds SET state='released',live_worker_id=NULL,live_run_id=NULL,
				final_disposition='no_adopted_source',release_evidence='no_adopted_source',
				released_at='2026-01-02 03:04:05+00' WHERE id=$1`, hold)
		}
	}
	// Snapshot every retained field, including timestamps, live IDs and final capture
	// proof. Only the publication classifications are expected to become NULL.
	expected := map[string]string{}
	for name, hold := range holds {
		var snapshot string
		if err := pool.QueryRow(ctx, `SELECT ((to_jsonb(h) - ARRAY[
			'completion_identity','completed_publication_receipt','completed_publication_reason']) ||
			jsonb_build_object('final_disposition',NULLIF(final_disposition,'completed_publication'),
				'release_evidence',NULLIF(release_evidence,'completed_publication')))::text
			FROM recovery_custody_holds h WHERE id=$1`, hold).Scan(&snapshot); err != nil {
			t.Fatal(err)
		}
		expected[name] = snapshot
	}
	for _, version := range []int64{319, 318, 317, 312} {
		if err := store.MigrateDownTo(ctx, dsn, version); err != nil {
			t.Fatalf("Down to %d: %v", version, err)
		}
		if version == 318 {
			for _, function := range []string{"completed_publication_stamp", "recovery_inventory_hold_guard"} {
				var body string
				if err := pool.QueryRow(ctx, `SELECT prosrc FROM pg_proc WHERE oid=$1::regproc`,
					function).Scan(&body); err != nil || strings.Contains(body, "self_improve") {
					t.Fatalf("Down319 %s still supports self_improve: %v", function, err)
				}
			}
		}
		if version > 317 {
			continue
		}
		var columns, objects int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_schema='public'
			AND ((table_name='runs' AND column_name='completion_final_head') OR
				(table_name='recovery_custody_holds' AND column_name IN
				 ('completion_identity','completed_publication_receipt','completed_publication_reason')))`).Scan(&columns); err != nil || columns != 0 {
			t.Fatalf("Down%d publication columns remain: %d err=%v", version, columns, err)
		}
		if err := pool.QueryRow(ctx, `SELECT
			(SELECT count(*) FROM pg_trigger WHERE NOT tgisinternal AND tgname LIKE 'completed_publication_%') +
			(SELECT count(*) FROM pg_proc WHERE pronamespace='public'::regnamespace AND proname LIKE 'completed_publication_%') +
			(SELECT count(*) FROM pg_constraint WHERE conname='completed_publication_receipt_check')`).Scan(&objects); err != nil || objects != 0 {
			t.Fatalf("Down%d publication objects remain: %d err=%v", version, objects, err)
		}
		var enabled string
		if err := pool.QueryRow(ctx, `SELECT tgenabled::text FROM pg_trigger
			WHERE tgrelid='recovery_custody_holds'::regclass AND tgname='recovery_inventory_hold_guard'`).
			Scan(&enabled); err != nil || enabled != "O" {
			t.Fatalf("Down%d inventory guard disabled: %q err=%v", version, enabled, err)
		}
		// A harmless UPDATE executes the restored guard, including its open-hold arm.
		tag, err := pool.Exec(ctx, `UPDATE recovery_custody_holds SET updated_at=updated_at`)
		if err != nil || tag.RowsAffected() != int64(len(holds)) {
			t.Fatalf("Down%d restored guard not executable: rows=%d err=%v", version, tag.RowsAffected(), err)
		}
		for name, hold := range holds {
			var snapshot string
			if err := pool.QueryRow(ctx, `SELECT to_jsonb(h)::text FROM recovery_custody_holds h WHERE id=$1`,
				hold).Scan(&snapshot); err != nil || snapshot != expected[name] {
				t.Fatalf("Down%d changed retained %s data: got=%s want=%s err=%v",
					version, name, snapshot, expected[name], err)
			}
		}
		// The legacy constraints reject each new classification independently.
		for _, column := range []string{"final_disposition", "release_evidence"} {
			_, err := pool.Exec(ctx, `INSERT INTO recovery_custody_holds
				(user_id,run_id,generation,state,original_worker_id,original_worker_identity,`+column+`)
				VALUES($1,$2,1,'released',$3,'rollback-worker','completed_publication')`,
				user, uuid.New(), worker)
			if err == nil || !strings.Contains(err.Error(), "recovery_custody_holds_"+column+"_check") {
				t.Fatalf("Down%d legacy %s constraint: %v", version, column, err)
			}
		}
	}
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("re-up to head: %v", err)
	}
	for name, hold := range holds {
		var snapshot string
		if err := pool.QueryRow(ctx, `SELECT (to_jsonb(h) - ARRAY[
			'completion_identity','completed_publication_receipt','completed_publication_reason'])::text
			FROM recovery_custody_holds h WHERE id=$1`, hold).Scan(&snapshot); err != nil || snapshot != expected[name] {
			t.Fatalf("re-up changed retained %s data: %s err=%v", name, snapshot, err)
		}
	}
}
