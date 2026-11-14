package store_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #3250 M1: migrations 00442 (effort, model_source, effort_source, the two NOT VALID CHECKs and
// the schedule_pin backfill) and 00443 (VALIDATE both). Skipped unless UZI_TEST_DATABASE_URL points
// at a throwaway Postgres.
func TestRunStartOptionsMigrationLifecycleLiveDB(t *testing.T) {
	ctx, dsn := standIsolatedDB(t, "run_start_options_migration_")
	if err := store.MigrateTo(ctx, dsn, 441); err != nil {
		t.Fatal(err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	owner, conn, repo := uuid.New(), uuid.New(), uuid.New()
	mustExec(ctx, t, pool, "INSERT INTO users(id,email,password_hash) VALUES($1,$2,'x')", owner, owner.String()+"@example.com")
	mustExec(ctx, t, pool, `INSERT INTO forge_connections(id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext) VALUES($1,$2,'github','https://example.com','bot',1,'x')`, conn, owner)
	mustExec(ctx, t, pool, `INSERT INTO repos(id,connection_id,forge_project_id,path_with_namespace,web_url,default_branch) VALUES($1,$2,1,'example/rso','https://example.com/rso','main')`, repo, conn)

	type seeded struct {
		id        uuid.UUID
		name      string
		wantModel bool
	}
	var rows []seeded
	head := strings.Repeat("a", 40)
	// The runs INSERT guard refuses a completion_final_head at INSERT, so seed under replica mode
	// (the same bypass a pre-existing production row effectively has).
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	mustExecTx(ctx, t, tx, "SET LOCAL session_replication_role = replica")
	iid := 0
	insert := func(name, status string, model *string, extra string) uuid.UUID {
		t.Helper()
		iid++
		id := uuid.New()
		cols, vals := "", ""
		if extra != "" {
			parts := strings.SplitN(extra, "|", 2)
			cols, vals = ","+parts[0], ","+strings.ReplaceAll(parts[1], "$ID", "'"+id.String()+"'")
		}
		mustExecTx(ctx, t, tx, fmt.Sprintf(
			`INSERT INTO runs(id,user_id,repo_id,issue_iid,issue_title,issue_description,status,model%s) VALUES($1,$2,$3,$4,'t','d',$5,$6%s)`, cols, vals),
			id, owner, repo, iid, status, model)
		rows = append(rows, seeded{id: id, name: name, wantModel: model != nil})
		return id
	}
	opus := "opus"
	insert("completed with head and contract", "completed", &opus, "completion_final_head,completion_contract_version|'"+head+"',1")
	insert("completed without head", "completed", &opus, "")
	insert("queued with model", "queued", &opus, "")
	insert("failed with model", "failed", &opus, "fail_origin|'agent_failure'")
	insert("queued without model", "queued", nil, "")
	insert("queued with checkpoint coordinates", "queued", &opus,
		fmt.Sprintf(`checkpoint_coordinates|jsonb_build_object('run_id',$ID::text,'user_id','%s','repo_id','%s','kind','issue','issue_iid',%d,'work_branch','agent/issue-%d')`, owner, repo, iid+1, iid+1))
	terminal := insert("terminal with forge origin", "completed", &opus, "")
	mustExecTx(ctx, t, tx, `INSERT INTO run_forge_origins(run_id,user_id,worker_id,generation,connection_id,origin) VALUES($1,$2,$3,1,$4,'https://example.com')`,
		terminal, owner, uuid.New(), conn)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	snapshot := func() map[uuid.UUID]string {
		t.Helper()
		out := map[uuid.UUID]string{}
		rs, err := pool.Query(ctx, `SELECT id, to_jsonb(r)::text FROM runs r`)
		if err != nil {
			t.Fatal(err)
		}
		defer rs.Close()
		for rs.Next() {
			var id uuid.UUID
			var s string
			if err := rs.Scan(&id, &s); err != nil {
				t.Fatal(err)
			}
			out[id] = s
		}
		return out
	}
	before := snapshot()
	var forgeOrigins int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM run_forge_origins`).Scan(&forgeOrigins); err != nil || forgeOrigins != 1 {
		t.Fatalf("seeded forge origins = %d (%v), want 1", forgeOrigins, err)
	}

	if err := store.MigrateTo(ctx, dsn, 442); err != nil {
		t.Fatal(err)
	}
	// Up changes only the three new columns: every pre-existing column of every row, including the
	// completed row's head, is byte-identical.
	after := map[uuid.UUID]string{}
	rs, err := pool.Query(ctx, `SELECT id, (to_jsonb(r) - 'effort' - 'model_source' - 'effort_source')::text FROM runs r`)
	if err != nil {
		t.Fatal(err)
	}
	for rs.Next() {
		var id uuid.UUID
		var s string
		if err := rs.Scan(&id, &s); err != nil {
			t.Fatal(err)
		}
		after[id] = s
	}
	rs.Close()
	for _, r := range rows {
		if after[r.id] != before[r.id] {
			t.Errorf("%s: pre-existing columns changed by Up", r.name)
		}
		var src, effort, effortSrc *string
		if err := pool.QueryRow(ctx, `SELECT model_source, effort, effort_source FROM runs WHERE id=$1`, r.id).Scan(&src, &effort, &effortSrc); err != nil {
			t.Fatal(err)
		}
		if r.wantModel != (src != nil && *src == "schedule_pin") || (!r.wantModel && src != nil) {
			t.Errorf("%s: model_source=%v, want schedule_pin=%v", r.name, src, r.wantModel)
		}
		if effort != nil || effortSrc != nil {
			t.Errorf("%s: effort pair = %v/%v, want NULL", r.name, effort, effortSrc)
		}
	}
	var gotHead string
	if err := pool.QueryRow(ctx, `SELECT completion_final_head FROM runs WHERE id=$1`, rows[0].id).Scan(&gotHead); err != nil || gotHead != head {
		t.Errorf("completed head = %q (%v), want %q", gotHead, err, head)
	}
	var coords string
	if err := pool.QueryRow(ctx, `SELECT checkpoint_coordinates::text FROM runs WHERE id=$1`, rows[5].id).Scan(&coords); err != nil || coords == "" {
		t.Errorf("checkpoint coordinates = %q (%v), want preserved", coords, err)
	}
	var originAfter int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM run_forge_origins`).Scan(&originAfter); err != nil || originAfter != 1 {
		t.Errorf("forge origins after Up = %d (%v), want 1", originAfter, err)
	}

	validated := func() map[string]bool {
		t.Helper()
		out := map[string]bool{}
		rs, err := pool.Query(ctx, `SELECT conname, convalidated FROM pg_constraint WHERE conrelid='runs'::regclass AND conname IN ('runs_model_source_check','runs_effort_source_check')`)
		if err != nil {
			t.Fatal(err)
		}
		defer rs.Close()
		for rs.Next() {
			var n string
			var v bool
			if err := rs.Scan(&n, &v); err != nil {
				t.Fatal(err)
			}
			out[n] = v
		}
		return out
	}
	refused := func(col, val, constraint string) {
		t.Helper()
		_, err := pool.Exec(ctx, fmt.Sprintf(`UPDATE runs SET %s=$1 WHERE id=$2`, col), val, rows[2].id)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != constraint {
			t.Fatalf("UPDATE %s=%q: %v, want 23514 on %s", col, val, err, constraint)
		}
	}
	got := validated()
	if len(got) != 2 || got["runs_model_source_check"] || got["runs_effort_source_check"] {
		t.Fatalf("after 00442 constraints = %v, want both present and NOT VALID", got)
	}
	refused("model_source", "bogus", "runs_model_source_check")
	refused("effort_source", "schedule_pin", "runs_effort_source_check")

	if err := store.MigrateTo(ctx, dsn, 443); err != nil {
		t.Fatal(err)
	}
	got = validated()
	if len(got) != 2 || !got["runs_model_source_check"] || !got["runs_effort_source_check"] {
		t.Fatalf("after 00443 constraints = %v, want both validated", got)
	}
	refused("model_source", "bogus", "runs_model_source_check")
	refused("effort_source", "schedule_pin", "runs_effort_source_check")

	// 00443's Down is a no-op: the validated constraints stay.
	if err := store.MigrateDownTo(ctx, dsn, 442); err != nil {
		t.Fatal(err)
	}
	if got = validated(); !got["runs_model_source_check"] || !got["runs_effort_source_check"] {
		t.Fatalf("00443 Down changed the constraints: %v", got)
	}
	// 00442's Down drops the constraints and the three columns cleanly.
	if err := store.MigrateDownTo(ctx, dsn, 441); err != nil {
		t.Fatal(err)
	}
	if got = validated(); len(got) != 0 {
		t.Fatalf("constraints survive Down: %v", got)
	}
	assertNoRunStartColumns(ctx, t, pool)
}

func assertNoRunStartColumns(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_name='runs' AND column_name IN ('effort','model_source','effort_source')`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("columns surviving Down = %d (%v), want 0", n, err)
	}
}
