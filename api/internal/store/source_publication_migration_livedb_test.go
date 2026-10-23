package store_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestSourcePublicationMigrationLiveDB(t *testing.T) {
	ctx, dsn := standIsolatedDB(t, "source_publication_migration_")
	if err := store.MigrateTo(ctx, dsn, 348); err != nil {
		t.Fatal(err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	owner, old := uuid.New(), uuid.New()
	mustExec(ctx, t, pool, "INSERT INTO users(id,email,password_hash) VALUES($1,$2,'x')", owner, owner.String()+"@example.com")
	conn := uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO forge_connections(id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext) VALUES($1,$2,'github','https://example.com','bot',1,'x')`, conn, owner)
	repo := seedRepo(ctx, t, pool, conn, "source/publication", []string{})
	mustExec(ctx, t, pool, "INSERT INTO runs(id,user_id,repo_id,issue_iid,kind,status,issue_title,issue_description) VALUES($1,$2,$3,1,'issue','completed','old','')", old, owner, repo)
	if err := store.MigrateTo(ctx, dsn, 349); err != nil {
		t.Fatal(err)
	}
	validated := func() bool {
		t.Helper()
		var v bool
		if err := pool.QueryRow(ctx, "SELECT convalidated FROM pg_constraint WHERE conrelid='runs'::regclass AND conname='runs_source_publication_intent_check'").Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	if validated() {
		t.Fatal("additive transaction scanned historical runs")
	}
	var absent bool
	if err := pool.QueryRow(ctx, "SELECT source_publication_intent IS NULL FROM runs WHERE id=$1", old).Scan(&absent); err != nil || !absent {
		t.Fatalf("historical stamp backfilled: %t %v", absent, err)
	}
	for _, phase := range []string{"not_valid", "validated"} {
		if phase == "validated" {
			if err := store.MigrateTo(ctx, dsn, 350); err != nil {
				t.Fatal(err)
			}
			if !validated() {
				t.Fatal("separate validation missing")
			}
		}
		for _, defect := range []string{"valid", "intent_null", "intent_number", "intent_missing", "branch_null", "reported_null", "version_null", "extra", "wrong_intent"} {
			t.Run(phase+"/"+defect, func(t *testing.T) {
				value := map[string]any{"version": 1, "claim_generation": 1, "branch": "agent/issue-1", "mr_iid": 7, "published_head": strings.Repeat("a", 40), "reported_non_closing": false, "intent": "closing"}
				switch defect {
				case "intent_null":
					value["intent"] = nil
				case "intent_number":
					value["intent"] = 1
				case "intent_missing":
					delete(value, "intent")
				case "branch_null":
					value["branch"] = nil
				case "reported_null":
					value["reported_non_closing"] = nil
				case "version_null":
					value["version"] = nil
				case "extra":
					value["extra"] = true
				case "wrong_intent":
					value["intent"] = "other"
				}
				raw, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				// INSERT exercises the CHECK independently of the UPDATE identity guard.
				_, err = pool.Exec(ctx, `INSERT INTO runs(id,user_id,repo_id,issue_iid,kind,status,issue_title,issue_description,source_publication_intent)
 VALUES($1,$2,$3,1,'issue','completed','schema','',$4)`, uuid.New(), owner, repo, raw)
				if defect == "valid" {
					if err != nil {
						t.Fatal(err)
					}
					return
				}
				var pgErr *pgconn.PgError
				if !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != "runs_source_publication_intent_check" {
					t.Fatalf("CHECK failed to reject %s: %v", raw, err)
				}
			})
		}
	}
}
