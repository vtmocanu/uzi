package store_test

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestCodexProviderSchemaLiveDB(t *testing.T) {
	ctx, pool, q := productLiveDB(t)
	owner, _ := newProductUser(ctx, t, pool)
	other, _ := newProductUser(ctx, t, pool)
	empty, err := q.GetUserProviderSource(ctx, store.GetUserProviderSourceParams{ID: other, Engine: "codex"})
	if err != nil || empty.ProviderID.Valid {
		t.Fatalf("empty source %+v: %v", empty, err)
	}
	p, err := q.InsertModelProvider(ctx, store.InsertModelProviderParams{OwnerUserID: owner, Name: "responses", Engine: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	rev := store.InsertModelProviderRevisionParams{ProviderID: p.ID, Revision: 1, Protocol: "openai_responses", BaseUrl: "https://example.test/v1", AuthStyle: "bearer", ModelMap: []byte(`{"default":{"model":"gateway"}}`)}
	if err := q.InsertModelProviderRevision(ctx, rev); err != nil {
		t.Fatal(err)
	}
	rev.Revision = 2
	rev.AuthStyle = "x_api_key"
	if err := q.InsertModelProviderRevision(ctx, rev); err == nil {
		t.Fatal("accepted non-Bearer Responses")
	}
	rev.AuthStyle = "bearer"
	rev.Protocol = "anthropic_messages"
	if err := q.InsertModelProviderRevision(ctx, rev); err == nil {
		t.Fatal("accepted mismatched engine/protocol")
	}
	k, err := insertProviderKey(ctx, q, owner, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if k.Kind != store.KindOpenAICompatibleKey || k.IsDefault || k.AutoEligible {
		t.Fatalf("provider key flags: %+v", k)
	}
	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM codex_credential_state WHERE user_secret_id=$1", k.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("vendor state count=%d: %v", count, err)
	}
	if _, err := insertProviderKey(ctx, q, other, p.ID); err == nil {
		t.Fatal("inserted foreign owner key")
	}
	src := store.SetUserProviderSourceParams{ID: owner, Engine: "codex", ProviderID: pgtype.UUID{Bytes: p.ID, Valid: true}}
	if _, err := q.SetUserProviderSource(ctx, src); err != nil {
		t.Fatal(err)
	}
	lanes, err := q.GetUserProviderLaneSources(ctx, owner)
	if err != nil || lanes.ClaudeProviderID.Valid || !lanes.CodexProviderID.Valid || uuid.UUID(lanes.CodexProviderID.Bytes) != p.ID {
		t.Fatalf("sources %+v: %v", lanes, err)
	}
	row, err := q.GetUserProviderSource(ctx, store.GetUserProviderSourceParams{ID: owner, Engine: "codex"})
	if err != nil || !row.KeyKind.Valid || row.KeyKind.String != store.KindOpenAICompatibleKey {
		t.Fatalf("resolved %+v: %v", row, err)
	}
	if _, err := q.DeleteModelProvider(ctx, store.DeleteModelProviderParams{ID: p.ID, OwnerUserID: owner}); err == nil {
		t.Fatal("deleted selected source")
	}

	// Replaying guarded Downs in rolled-back transactions must preserve each dependency.
	for _, migration := range []string{"00514_codex_provider_profiles.sql", "00515_openai_compatible_provider_keys.sql", "00517_users_codex_provider_source.sql"} {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var refusal error
		for _, stmt := range migrationDownStatements(t, migration) {
			if _, refusal = tx.Exec(ctx, stmt); refusal != nil {
				break
			}
		}
		_ = tx.Rollback(ctx)
		if refusal == nil {
			t.Fatalf("%s Down accepted live dependency", migration)
		}
	}
	// Cross-owner Codex source assignment is also refused at the database boundary.
	// The scope names the other user's own id, so the scope check passes and the composite foreign
	// key (scope, provider) is what refuses the owner's definition.
	_, err = pool.Exec(ctx, "UPDATE users SET codex_provider_id=$1, codex_provider_scope_id=$2 WHERE id=$2", p.ID, other)
	if code, c := pgCodeC(err); code != "23503" || c != "users_codex_provider_fkey" {
		t.Fatalf("cross-owner Codex source = %v (%s %s), want users_codex_provider_fkey", err, code, c)
	}

	if _, err := q.GetProviderKeyForUpdate(ctx, store.GetProviderKeyForUpdateParams{UserID: owner, ProviderID: src.ProviderID}); err != nil {
		t.Fatal(err)
	}
	if n, err := q.DeleteProviderKeySecret(ctx, store.DeleteProviderKeySecretParams{UserID: owner, ProviderID: src.ProviderID}); err != nil || n != 1 {
		t.Fatalf("delete key %d: %v", n, err)
	}
}

func TestCodexProviderRunSchemaLiveDB(t *testing.T) {
	ctx, dsn := standIsolatedDB(t, "codex_provider_run_schema_")
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	q := store.New(pool)
	owner, _ := newProductUser(ctx, t, pool)
	conn, repo := uuid.New(), uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO forge_connections(id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext) VALUES($1,$2,'github','https://example.com','bot',1,'x')`, conn, owner)
	mustExec(ctx, t, pool, `INSERT INTO repos(id,connection_id,forge_project_id,path_with_namespace,web_url,default_branch) VALUES($1,$2,1,'example/codex','https://example.com/codex','main')`, repo, conn)
	p, err := q.InsertModelProvider(ctx, store.InsertModelProviderParams{OwnerUserID: owner, Name: "run-schema", Engine: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	key, err := insertProviderKey(ctx, q, owner, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(ctx, t, pool, `UPDATE users SET codex_provider_id=$1, codex_provider_scope_id=$2 WHERE id=$2`, p.ID, owner)
	insert := `INSERT INTO runs(user_id,repo_id,kind,branch,issue_title,issue_description,harness,model_provider_id,model_provider_revision,model_provider_key_mode,model_provider_key_secret_id,model_provider_key_rev) VALUES($1,$2,$3,'schema-branch','schema','schema','codex',$4,3,'user_key',$5,7) RETURNING id`
	var runs []uuid.UUID
	for _, kind := range []string{"task", "prompt"} {
		t.Run(kind, func(t *testing.T) {
			var id uuid.UUID
			if err := pool.QueryRow(ctx, insert, owner, repo, kind, p.ID, key.ID).Scan(&id); err != nil {
				t.Fatal(err)
			}
			runs = append(runs, id)
			var provider, secret uuid.UUID
			var revision, keyRevision int
			var mode string
			if err := pool.QueryRow(ctx, `SELECT model_provider_id,model_provider_revision,model_provider_key_mode,model_provider_key_secret_id,model_provider_key_rev FROM runs WHERE id=$1`, id).Scan(&provider, &revision, &mode, &secret, &keyRevision); err != nil {
				t.Fatal(err)
			}
			if provider != p.ID || revision != 3 || mode != "user_key" || secret != key.ID || keyRevision != 7 {
				t.Fatalf("binding changed: %s %d %s %s %d", provider, revision, mode, secret, keyRevision)
			}
			for _, column := range []string{"anthropic_secret_id", "codex_secret_id", "codex_auth_mode", "credential_override_secret_id"} {
				t.Run(column, func(t *testing.T) {
					value := any(key.ID)
					if column == "codex_auth_mode" {
						value = "api_key"
					}
					_, err := pool.Exec(ctx, `UPDATE runs SET `+column+`=$1 WHERE id=$2`, value, id)
					if code, c := pgCodeC(err); code != "23514" || c != "runs_model_provider_exclusive_check" {
						t.Errorf("provider run with %s = %v (%s %s), want runs_model_provider_exclusive_check", column, err, code, c)
					}
				})
			}
			for _, column := range []string{"model_provider_id", "model_provider_revision", "model_provider_key_mode", "model_provider_key_secret_id", "model_provider_key_rev"} {
				t.Run("partial-"+column, func(t *testing.T) {
					_, err := pool.Exec(ctx, `UPDATE runs SET `+column+`=NULL WHERE id=$1`, id)
					if code, c := pgCodeC(err); code != "23514" || c != "runs_model_provider_binding_check" {
						t.Errorf("partial tuple %s = %v (%s %s)", column, err, code, c)
					}
				})
			}
		})
	}
	t.Run("down-refuses-under-lock", func(t *testing.T) {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		stmts := migrationDownStatements(t, "00519_runs_codex_provider_exclusive.sql")
		for i, stmt := range stmts {
			_, err = tx.Exec(ctx, stmt)
			if i == 1 && err == nil {
				var locked bool
				if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE pid=pg_backend_pid() AND relation='runs'::regclass AND mode='AccessExclusiveLock' AND granted)`).Scan(&locked); err != nil || !locked {
					t.Fatalf("Down lock=%v: %v", locked, err)
				}
			}
			if err != nil {
				break
			}
		}
		if err == nil || !strings.Contains(err.Error(), "refusing to roll back 00519: Codex provider runs still exist") {
			t.Fatalf("Down refusal: %v", err)
		}
	})
	t.Run("cleared-dependencies-allow-down", func(t *testing.T) {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		for _, id := range runs {
			if _, err := tx.Exec(ctx, `DELETE FROM runs WHERE id=$1`, id); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE users SET codex_provider_id=NULL, codex_provider_scope_id=NULL WHERE id=$1`, owner); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM user_secrets WHERE id=$1`, key.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM model_providers WHERE id=$1`, p.ID); err != nil {
			t.Fatal(err)
		}
		for _, migration := range []string{"00519_runs_codex_provider_exclusive.sql", "00517_users_codex_provider_source.sql", "00515_openai_compatible_provider_keys.sql", "00514_codex_provider_profiles.sql"} {
			for _, stmt := range migrationDownStatements(t, migration) {
				if _, err := tx.Exec(ctx, stmt); err != nil {
					t.Fatalf("%s cleared Down: %v", migration, err)
				}
			}
		}
	})
}
