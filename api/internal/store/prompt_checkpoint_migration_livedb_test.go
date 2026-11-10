package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestPromptCheckpointMigrationLifecycleLiveDB pins migrations 00422/00423 (issue #4116):
// the fn_checkpoint_coordinates prompt arm, the widened run_salvage branch CHECK, and the
// guarded Down.
func TestPromptCheckpointMigrationLifecycleLiveDB(t *testing.T) {
	ctx, dsn := standIsolatedDB(t, "prompt_checkpoint_migration_")
	if err := store.MigrateTo(ctx, dsn, 421); err != nil {
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
	mustExec(ctx, t, pool, `INSERT INTO repos(id,connection_id,forge_project_id,path_with_namespace,web_url,default_branch) VALUES($1,$2,1,'example/prompt','https://example.com/prompt','main')`, repo, conn)

	const tip = "0123456789abcdef0123456789abcdef01234567"
	insertPrompt := func(id uuid.UUID) {
		t.Helper()
		mustExec(ctx, t, pool, `INSERT INTO runs(id,user_id,repo_id,kind,issue_title,issue_description,status) VALUES($1,$2,$3,'prompt','t','d','failed')`, id, owner, repo)
	}
	insertSalvage := func(runID uuid.UUID, branch string) error {
		_, err := pool.Exec(ctx, `INSERT INTO run_salvage(run_id,user_id,repo_id,forge_type,branch,tip,state) VALUES($1,$2,$3,'github',$4,$5,'unavailable')`,
			runID, owner, repo, branch, tip)
		return err
	}
	// coords returns the resolver's jsonb as text, or "" when it is NULL.
	coords := func(id uuid.UUID) string {
		t.Helper()
		var s *string
		if err := pool.QueryRow(ctx, `SELECT fn_checkpoint_coordinates(r)::text FROM runs r WHERE id=$1`, id).Scan(&s); err != nil {
			t.Fatal(err)
		}
		if s == nil {
			return ""
		}
		return *s
	}
	wantPgCode := func(err error, code string) *pgconn.PgError {
		t.Helper()
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != code {
			t.Fatalf("want SQLSTATE %s, got: %v", code, err)
		}
		return pgErr
	}

	run1, run2, noRepo := uuid.New(), uuid.New(), uuid.New()
	insertPrompt(run1)
	insertPrompt(run2)

	t.Run("version 421 has no prompt arm and rejects the prompt salvage branch", func(t *testing.T) {
		if got := coords(run1); got != "" {
			t.Fatalf("421 prompt coordinates = %s, want NULL", got)
		}
		pgErr := wantPgCode(insertSalvage(run2, "uzi/prompt-"+run2.String()), "23514")
		if pgErr.ConstraintName != "run_salvage_branch_format_check" {
			t.Fatalf("constraint = %q", pgErr.ConstraintName)
		}
	})

	// The definitions Down must restore exactly (function body and constraint, incl. validation).
	defs421 := schemaDefs(ctx, t, pool)

	if err := store.MigrateTo(ctx, dsn, 423); err != nil {
		t.Fatal(err)
	}

	t.Run("version 423 still resolves no coordinates for non-checkpoint kinds", func(t *testing.T) {
		// Synthetic rows bypass runs_kind_shape: only the resolver's kind dispatch is under test.
		// issue_iid is set so widening the issue arm to another kind is visible too.
		for _, kind := range []string{"ci_fix", "mr_rework", "chat", "judge", "job", "cross_check"} {
			var isNull bool
			if err := pool.QueryRow(ctx, `SELECT fn_checkpoint_coordinates(jsonb_populate_record(NULL::runs,
				jsonb_build_object('id',gen_random_uuid(),'user_id',r.user_id,'repo_id',r.repo_id,'kind',$2::text,'issue_iid',1,
				  'branch','uzi/prompt-'||r.id::text))) IS NULL FROM runs r WHERE r.id=$1`, run1, kind).Scan(&isNull); err != nil {
				t.Fatal(err)
			}
			if !isNull {
				t.Fatalf("kind %s resolved checkpoint coordinates, want NULL", kind)
			}
		}
	})

	t.Run("version 423 resolves prompt coordinates", func(t *testing.T) {
		var ok bool
		err := pool.QueryRow(ctx, `SELECT fn_checkpoint_coordinates(r) = jsonb_build_object(
			'version',1,'run_id',r.id,'user_id',r.user_id,'repo_id',r.repo_id,'kind','prompt','issue_iid',NULL::int,
			'work_branch','uzi/prompt-'||r.id::text,'checkpoint_key','uzi/prompt-'||r.id::text,
			'provenance',jsonb_build_array(r.id)) FROM runs r WHERE id=$1`, run1).Scan(&ok)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			t.Fatalf("coordinates = %s", coords(run1))
		}
		var iid *string
		if err := pool.QueryRow(ctx, `SELECT fn_checkpoint_coordinates(r)->>'issue_iid' FROM runs r WHERE id=$1`, run1).Scan(&iid); err != nil {
			t.Fatal(err)
		}
		if iid != nil {
			t.Fatalf("issue_iid = %q, want JSON null", *iid)
		}
	})

	t.Run("a prompt run without a repo is rejected and has no coordinates", func(t *testing.T) {
		_, err := pool.Exec(ctx, `INSERT INTO runs(id,user_id,repo_id,kind,issue_title,issue_description,status) VALUES($1,$2,NULL,'prompt','t','d','failed')`, noRepo, owner)
		pgErr := wantPgCode(err, "23514")
		if pgErr.ConstraintName != "runs_kind_shape" {
			t.Fatalf("constraint = %q", pgErr.ConstraintName)
		}
		var isNull bool
		err = pool.QueryRow(ctx, `SELECT fn_checkpoint_coordinates(jsonb_populate_record(NULL::runs, to_jsonb(r) || '{"repo_id":null}'::jsonb)) IS NULL FROM runs r WHERE id=$1`, run1).Scan(&isNull)
		if err != nil {
			t.Fatal(err)
		}
		if !isNull {
			t.Fatal("repo-less prompt row resolved coordinates")
		}
	})

	t.Run("version 423 admits the prompt salvage branch", func(t *testing.T) {
		if err := insertSalvage(run2, "uzi/prompt-"+run2.String()); err != nil {
			t.Fatal(err)
		}
		var validated bool
		if err := pool.QueryRow(ctx, `SELECT convalidated FROM pg_constraint WHERE conrelid='run_salvage'::regclass AND conname='run_salvage_branch_format_check'`).Scan(&validated); err != nil {
			t.Fatal(err)
		}
		if !validated {
			t.Fatal("00423 left the salvage branch constraint NOT VALID")
		}
		_ = wantPgCode(insertSalvage(run1, "uzi/prompt-not-a-uuid"), "23514")
	})

	// MigrateDownTo commits one migration per step, so a refused Down to 421 has already
	// committed 00423's no-op Down. Step to 422 first so the snapshot compares exactly the
	// state the refusing migration (00422) saw.
	if err := store.MigrateDownTo(ctx, dsn, 422); err != nil {
		t.Fatal(err)
	}

	snapshot := func() string {
		t.Helper()
		var s string
		if err := pool.QueryRow(ctx, `SELECT jsonb_build_object(
   'versions',(SELECT jsonb_agg(to_jsonb(v) ORDER BY id) FROM goose_db_version v),
   'function',(SELECT pg_get_functiondef('fn_checkpoint_coordinates(runs)'::regprocedure)),
   'constraint',(SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conrelid='run_salvage'::regclass AND conname='run_salvage_branch_format_check'))::text`).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		mustExec(ctx, t, pool, sql, args...)
	}
	clearAll := func(c context.Context, p *pgxpool.Pool) {
		t.Helper()
		for _, q := range []string{
			`UPDATE runs SET checkpoint_tip=NULL WHERE kind='prompt'`,
			`DELETE FROM checkpoint_publish_attempts`,
			`DELETE FROM checkpoint_retentions`,
			`DELETE FROM run_salvage`,
		} {
			if _, err := p.Exec(c, q); err != nil {
				t.Fatal(err)
			}
		}
	}

	cases := []struct {
		name  string
		setup func()
		want  string
	}{
		{"published checkpoint tip", func() {
			exec(`UPDATE runs SET checkpoint_tip=$2 WHERE id=$1`, run1, tip)
		}, "cannot downgrade with a published prompt checkpoint"},
		{"unresolved publish attempt with run row", func() {
			// The run has no checkpoint_tip and no retention or salvage row.
			exec(`INSERT INTO checkpoint_publish_attempts(run_id,branch,ref,tip) VALUES($1,$2,$3,$4)`,
				run1, "uzi/prompt-"+run1.String(), "refs/uzi-checkpoints/uzi/prompt-"+run1.String(), tip)
		}, "cannot downgrade with an unresolved prompt checkpoint publication"},
		{"retention row", func() {
			exec(`INSERT INTO checkpoint_retentions(run_id,user_id,repo_id,branch,tip,ref,state) VALUES($1,$2,$3,$4,$5,$6,'deleted')`,
				uuid.New(), owner, repo, "uzi/prompt-"+run1.String(), tip, "refs/uzi-checkpoints/uzi/prompt-"+run1.String())
		}, "cannot downgrade with a prompt checkpoint retention"},
		{"salvage row", func() {
			if err := insertSalvage(run2, "uzi/prompt-"+run2.String()); err != nil {
				t.Fatal(err)
			}
		}, "cannot downgrade with a prompt salvage record"},
		{"publish attempt with no run row", func() {
			// 00267 deliberately has no run FK: deleting the run cannot erase a push
			// that may still reach the forge. Down must protect this orphan directly.
			missingRun := uuid.New()
			exec(`INSERT INTO checkpoint_publish_attempts(run_id,branch,ref,tip) VALUES($1,$2,$3,$4)`,
				missingRun, "uzi/prompt-"+missingRun.String(), "refs/uzi-checkpoints/uzi/prompt-"+missingRun.String(), tip)
		}, "cannot downgrade with an unresolved prompt checkpoint publication"},
		{"missing run with prompt branch alone", func() {
			missingRun := uuid.New()
			exec(`INSERT INTO checkpoint_publish_attempts(run_id,branch,ref,tip) VALUES($1,$2,$3,$4)`,
				missingRun, "uzi/prompt-"+missingRun.String(), "refs/uzi-checkpoints/agent/issue-1", tip)
		}, "cannot downgrade with an unresolved prompt checkpoint publication"},
		{"missing run with prompt ref alone", func() {
			missingRun := uuid.New()
			exec(`INSERT INTO checkpoint_publish_attempts(run_id,branch,ref,tip) VALUES($1,$2,$3,$4)`,
				missingRun, "agent/issue-1", "refs/uzi-checkpoints/uzi/prompt-"+missingRun.String(), tip)
		}, "cannot downgrade with an unresolved prompt checkpoint publication"},
	}
	for _, tc := range cases {
		t.Run("Down refuses: "+tc.name, func(t *testing.T) {
			clearAll(ctx, pool)
			tc.setup()
			state := snapshot()
			err := store.MigrateDownTo(ctx, dsn, 421)
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || !strings.Contains(pgErr.Message, tc.want) {
				t.Fatalf("Down error = %v, want message containing %q", err, tc.want)
			}
			if snapshot() != state {
				t.Fatal("refused Down changed Goose history, function, or constraint")
			}
		})
	}

	t.Run("Down succeeds with no prompt checkpoint state", func(t *testing.T) {
		clearAll(ctx, pool)
		if err := store.MigrateDownTo(ctx, dsn, 421); err != nil {
			t.Fatal(err)
		}
		if got := coords(run1); got != "" {
			t.Fatalf("421 prompt coordinates after Down = %s, want NULL", got)
		}
		var def string
		if err := pool.QueryRow(ctx, `SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conrelid='run_salvage'::regclass AND conname='run_salvage_branch_format_check'`).Scan(&def); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(def, "uzi/prompt-") {
			t.Fatalf("constraint still admits prompt branches: %s", def)
		}
		_ = wantPgCode(insertSalvage(run2, "uzi/prompt-"+run2.String()), "23514")
		if got := schemaDefs(ctx, t, pool); got != defs421 {
			t.Fatalf("Down did not restore the 421 definitions:\n got  %s\n want %s", got, defs421)
		}
	})
}

// schemaDefs renders fn_checkpoint_coordinates and run_salvage_branch_format_check (with its
// validation flag) so a Down that restores a different body or constraint is caught.
func schemaDefs(ctx context.Context, t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(ctx, `SELECT pg_get_functiondef('fn_checkpoint_coordinates(runs)'::regprocedure)
		|| E'\n' || pg_get_constraintdef(c.oid) || ' validated=' || c.convalidated::text
		FROM pg_constraint c WHERE c.conrelid='run_salvage'::regclass AND c.conname='run_salvage_branch_format_check'`).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}
