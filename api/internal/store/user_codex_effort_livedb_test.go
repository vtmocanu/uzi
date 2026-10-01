package store_test

import (
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestCodexEffortBackfillLiveDB(t *testing.T) {
	ctx, dsn := standIsolatedDB(t, "codex_effort_")
	if err := store.MigrateTo(ctx, dsn, 284); err != nil {
		t.Fatal(err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var columns int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_name='users' AND column_name='default_codex_effort'`).Scan(&columns); err != nil {
		t.Fatal(err)
	}
	if columns != 0 {
		t.Fatal("Codex column already exists before migration")
	}
	cases := []any{nil, "", " xhigh ", "low", "max"}
	ids := make([]uuid.UUID, len(cases))
	for i, value := range cases {
		ids[i] = uuid.New()
		if _, err := pool.Exec(ctx, `INSERT INTO users (id,email,password_hash,default_effort) VALUES ($1,$2,'fixture',$3)`, ids[i], fmt.Sprintf("effort-%s@example.com", ids[i]), value); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.MigrateTo(ctx, dsn, 285); err != nil {
		t.Fatal(err)
	}
	for i, value := range cases {
		var shared, codex pgtype.Text
		if err := pool.QueryRow(ctx, `SELECT default_effort,default_codex_effort FROM users WHERE id=$1`, ids[i]).Scan(&shared, &codex); err != nil {
			t.Fatal(err)
		}
		if shared != codex {
			t.Fatalf("backfill %v changed preference: shared=%+v codex=%+v", value, shared, codex)
		}
		if value == nil {
			if codex.Valid {
				t.Fatal("NULL preference was materialized")
			}
		} else if !codex.Valid || codex.String != value.(string) {
			t.Fatalf("backfill normalized %q to %+v", value, codex)
		}
	}
	if err := store.MigrateDownTo(ctx, dsn, 284); err != nil {
		t.Fatal(err)
	}
	for i, value := range cases {
		var shared pgtype.Text
		if err := pool.QueryRow(ctx, `SELECT default_effort FROM users WHERE id=$1`, ids[i]).Scan(&shared); err != nil {
			t.Fatal(err)
		}
		if value == nil {
			if shared.Valid {
				t.Fatal("Down changed NULL")
			}
		} else if shared.String != value.(string) {
			t.Fatal("Down changed original preference")
		}
	}
}

func TestCodexEffortQueriesIndependentLiveDB(t *testing.T) {
	ctx, dsn := standIsolatedDB(t, "codex_effort_queries_")
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	q := store.New(pool)
	user, err := q.CreateUser(ctx, store.CreateUserParams{Email: fmt.Sprintf("effort-%s@example.com", uuid.New()), PasswordHash: pgtype.Text{String: "fixture", Valid: true}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.SetUserDefaultEffort(ctx, store.SetUserDefaultEffortParams{ID: user.ID, DefaultEffort: pgtype.Text{String: "high", Valid: true}}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.SetUserDefaultCodexEffort(ctx, store.SetUserDefaultCodexEffortParams{ID: user.ID, DefaultCodexEffort: pgtype.Text{String: "low", Valid: true}}); err != nil {
		t.Fatal(err)
	}
	settings, err := q.GetUserSettings(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if settings.DefaultEffort.String != "high" || settings.DefaultCodexEffort.String != "low" {
		t.Fatalf("lanes clobbered: %+v", settings)
	}
	if _, err := q.SetUserDefaultCodexEffort(ctx, store.SetUserDefaultCodexEffortParams{ID: user.ID}); err != nil {
		t.Fatal(err)
	}
	codex, err := q.GetUserDefaultCodexEffort(ctx, user.ID)
	if err != nil || codex.Valid {
		t.Fatalf("clear Codex = %+v, %v", codex, err)
	}
	shared, err := q.GetUserDefaultEffort(ctx, user.ID)
	if err != nil || shared.String != "high" {
		t.Fatalf("clear changed Claude = %+v, %v", shared, err)
	}
}
