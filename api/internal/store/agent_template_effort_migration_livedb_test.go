package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// Uses a separate database: never rolls back the shared store-IT database.
func TestAgentTemplateEffortMigrationUpDownLiveDB(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	version := migrationVersionByName(t, "agent_template_effort")
	pool, dsn := codexJudgeMigrationDB(t, version-1)

	user := uuid.New()
	cjExec(t, pool, "INSERT INTO users (id,email,password_hash) VALUES ($1,$2,'x')", user, user.String()+"@example.test")
	stamp := time.Date(2025, 2, 3, 4, 5, 6, 0, time.UTC)
	type fixture struct {
		id         uuid.UUID
		scope      string
		origin     *string
		customized bool
	}
	embedded, admin := "embedded", "admin"
	var fixtures []fixture
	for _, f := range []fixture{
		{scope: "builtin", origin: &embedded},
		{scope: "builtin", origin: &admin, customized: true},
		{scope: "global"},
		{scope: "user"},
	} {
		f.id = uuid.New()
		var owner any
		if f.scope == "user" {
			owner = user
		}
		cjExec(t, pool, `INSERT INTO agent_templates
    (id,name,description,prompt_body,scope,is_builtin,user_id,model,customized,origin,updated_at,updated_by)
    VALUES ($1,$2,'migration fixture','fixture body',$3,$4,$5,'opus',$6,$7,$8,$9)`,
			f.id, "effort-"+f.id.String(), f.scope, f.scope == "builtin", owner, f.customized, f.origin, stamp, user)
		fixtures = append(fixtures, f)
	}

	if err := MigrateTo(ctx, dsn, version); err != nil {
		t.Fatal(err)
	}

	// Both columns exist, are nullable text, and both named constraints exist and are validated.
	for _, col := range []string{"effort", "codex_effort"} {
		var dataType, nullable string
		if err := pool.QueryRow(ctx, `SELECT data_type, is_nullable FROM information_schema.columns
			WHERE table_name='agent_templates' AND column_name=$1`, col).Scan(&dataType, &nullable); err != nil {
			t.Fatalf("%s column: %v", col, err)
		}
		if dataType != "text" || nullable != "YES" {
			t.Errorf("%s: type=%s nullable=%s", col, dataType, nullable)
		}
	}
	for _, con := range []string{"agent_templates_effort_ck", "agent_templates_codex_effort_ck"} {
		var validated bool
		if err := pool.QueryRow(ctx, `SELECT convalidated FROM pg_constraint
			WHERE conname=$1 AND conrelid='agent_templates'::regclass`, con).Scan(&validated); err != nil {
			t.Fatalf("constraint %s: %v", con, err)
		}
		if !validated {
			t.Errorf("constraint %s is NOT VALID", con)
		}
	}

	// Existing rows are unchanged: effort NULL, and customized, origin and updated_at untouched.
	for _, f := range fixtures {
		var effort, codex, origin *string
		var customized bool
		var updated time.Time
		if err := pool.QueryRow(ctx, `SELECT effort,codex_effort,origin,customized,updated_at FROM agent_templates WHERE id=$1`, f.id).
			Scan(&effort, &codex, &origin, &customized, &updated); err != nil {
			t.Fatal(err)
		}
		sameOrigin := (origin == nil) == (f.origin == nil) && (origin == nil || *origin == *f.origin)
		if effort != nil || codex != nil || !sameOrigin || customized != f.customized || !updated.Equal(stamp) {
			t.Errorf("scope=%s: effort=%v codex=%v origin=%v customized=%v updated=%s", f.scope, effort, codex, origin, customized, updated)
		}
	}

	// NULL and the five levels are accepted on both columns; '', 'High' and 'runtime' are rejected
	// with a check violation (23514) naming that column's constraint.
	insert := func(col string, value any) error {
		id := uuid.New()
		_, err := pool.Exec(ctx, `INSERT INTO agent_templates (id,name,description,prompt_body,scope,is_builtin,`+col+`)
			VALUES ($1,$2,'d','b','global',false,$3)`, id, "eff-"+id.String(), value)
		return err
	}
	for _, col := range []string{"effort", "codex_effort"} {
		if err := insert(col, nil); err != nil {
			t.Errorf("%s NULL rejected: %v", col, err)
		}
		for _, level := range []string{"low", "medium", "high", "xhigh", "max"} {
			if err := insert(col, level); err != nil {
				t.Errorf("%s=%q rejected: %v", col, level, err)
			}
		}
		for _, bad := range []string{"", "High", "runtime", " high"} {
			err := insert(col, bad)
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != "agent_templates_"+col+"_ck" {
				t.Errorf("%s=%q: err=%v, want check violation 23514 on agent_templates_%s_ck", col, bad, err, col)
			}
		}
	}

	// Down removes both columns and both constraints.
	if err := MigrateDownTo(ctx, dsn, version-1); err != nil {
		t.Fatal(err)
	}
	var columns, constraints int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
		WHERE table_name='agent_templates' AND column_name IN ('effort','codex_effort')`).Scan(&columns); err != nil || columns != 0 {
		t.Fatalf("Down left %d effort columns: %v", columns, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_constraint
		WHERE conrelid='agent_templates'::regclass AND conname IN ('agent_templates_effort_ck','agent_templates_codex_effort_ck')`).Scan(&constraints); err != nil || constraints != 0 {
		t.Fatalf("Down left %d effort constraints: %v", constraints, err)
	}
	var customized bool
	var updated time.Time
	if err := pool.QueryRow(ctx, `SELECT customized, updated_at FROM agent_templates WHERE id=$1`, fixtures[1].id).Scan(&customized, &updated); err != nil || !customized || !updated.Equal(stamp) {
		t.Fatalf("Down changed an existing row: customized=%v updated=%s err=%v", customized, updated, err)
	}
}
