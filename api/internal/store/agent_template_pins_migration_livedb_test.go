package store

import (
	"context"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/agenttmpl"
)

// Uses a separate database: never rolls back the shared store-IT database.
func TestAgentTemplatePinMigrationUpDownLiveDB(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	version := migrationVersionByName(t, "agent_template_codex_model")
	pool, dsn := codexJudgeMigrationDB(t, version-1)
	var before int64
	if err := pool.QueryRow(ctx, "SELECT max(version_id) FROM goose_db_version WHERE is_applied").Scan(&before); err != nil || before != version-1 {
		t.Fatalf("historical version=%d want=%d: %v", before, version-1, err)
	}
	user := uuid.New()
	cjExec(t, pool, "INSERT INTO users (id,email,password_hash) VALUES ($1,$2,'x')", user, user.String()+"@example.test")
	stamp := time.Date(2025, 2, 3, 4, 5, 6, 0, time.UTC)
	type fixture struct {
		id              uuid.UUID
		scope           string
		original, moved *string
		origin          *string
		customized      bool
	}
	ptr := func(s string) *string { return &s }
	ids := make([]string, 0, len(agenttmpl.CodexRoleModels))
	for id := range agenttmpl.CodexRoleModels {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	if len(ids) == 0 || !agenttmpl.CodexRoleModels["gpt-6-luna"] {
		t.Fatal("role registry must include luna")
	}
	var fixtures []fixture
	for _, scope := range []string{"builtin", "global", "user"} {
		cases := []struct{ input, moved *string }{
			{ptr("opus"), nil}, {ptr("custom-provider-model"), nil}, {nil, nil},
			{ptr("vgpt-6-sol"), nil}, {ptr("gpt-6-solv"), nil},
			{ptr(" \t" + "gpt-6-sol" + "\n "), ptr("gpt-6-sol")},
			{ptr("\vgpt-6-luna\v"), ptr("gpt-6-luna")},
		}
		for _, id := range ids {
			cases = append(cases, struct{ input, moved *string }{ptr(id), ptr(id)})
		}
		for i, c := range cases {
			f := fixture{id: uuid.New(), scope: scope, original: c.input, moved: c.moved, customized: i%2 == 0}
			var owner any
			if scope == "user" {
				owner = user
			}
			if scope == "builtin" {
				f.origin = ptr([]string{"embedded", "admin", "synced"}[i%3])
			}
			if scope == "global" && i%2 == 0 {
				f.origin = ptr("synced")
			}
			// The padded role in shared scopes also exercises synced provenance.
			if scope != "user" && i == 5 {
				f.origin = ptr("synced")
			}
			cjExec(t, pool, `INSERT INTO agent_templates
    (id,name,description,prompt_body,scope,is_builtin,user_id,model,customized,origin,updated_at,updated_by)
    VALUES ($1,$2,'migration fixture','fixture body',$3,$4,$5,$6,$7,$8,$9,$10)`,
				f.id, "pin-"+f.id.String(), scope, scope == "builtin", owner, c.input, f.customized, f.origin, stamp, user)
			fixtures = append(fixtures, f)
		}
	}
	if err := MigrateTo(ctx, dsn, version); err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, f := range fixtures {
		var model, codex, origin *string
		var customized bool
		var updated time.Time
		var updatedBy uuid.UUID
		if err := pool.QueryRow(ctx, `SELECT model,codex_model,origin,customized,updated_at,updated_by FROM agent_templates WHERE id=$1`, f.id).
			Scan(&model, &codex, &origin, &customized, &updated, &updatedBy); err != nil {
			t.Fatal(err)
		}
		wantModel := f.original
		if f.moved != nil {
			wantModel = nil
			counts[f.scope]++
		}
		if !reflect.DeepEqual(model, wantModel) || !reflect.DeepEqual(codex, f.moved) ||
			!reflect.DeepEqual(origin, f.origin) || customized != f.customized || !updated.Equal(stamp) || updatedBy != user {
			t.Errorf("Up scope=%s original=%v: model=%v codex=%v origin=%v customized=%v updated=%s by=%s", f.scope, f.original, model, codex, origin, customized, updated, updatedBy)
		}
	}
	for _, scope := range []string{"builtin", "global", "user"} {
		if counts[scope] != len(ids)+2 {
			t.Fatalf("%s moved %d want %d", scope, counts[scope], len(ids)+2)
		}
		t.Logf("%s: verified %d recognized moves and 5 unchanged cases", scope, counts[scope])
	}
	// Post-upgrade dual pins deliberately lose the Codex pin on Down; a Codex-only
	// pin created after upgrade is restored to the legacy column.
	dual, only := uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{dual, only} {
		cjExec(t, pool, `INSERT INTO agent_templates (id,name,description,prompt_body,scope,is_builtin,codex_model)
   VALUES ($1,$2,'rollback fixture','body','global',false,'gpt-6-luna')`, id, "pin-"+id.String())
	}
	cjExec(t, pool, "UPDATE agent_templates SET model='sonnet' WHERE id=$1", dual)
	if err := MigrateDownTo(ctx, dsn, version-1); err != nil {
		t.Fatal(err)
	}
	for _, f := range fixtures {
		var model, origin *string
		var customized bool
		var updated time.Time
		want := f.original
		if f.moved != nil {
			want = f.moved
		}
		if err := pool.QueryRow(ctx, "SELECT model,origin,customized,updated_at FROM agent_templates WHERE id=$1", f.id).
			Scan(&model, &origin, &customized, &updated); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(model, want) || !reflect.DeepEqual(origin, f.origin) || customized != f.customized || !updated.Equal(stamp) {
			t.Errorf("Down changed model/metadata for %s (%s)", f.id, f.scope)
		}
	}
	for id, want := range map[uuid.UUID]string{dual: "sonnet", only: "gpt-6-luna"} {
		var got string
		if err := pool.QueryRow(ctx, "SELECT model FROM agent_templates WHERE id=$1", id).Scan(&got); err != nil || got != want {
			t.Fatalf("Down model=%q want=%q: %v", got, want, err)
		}
	}
	var columns int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_name='agent_templates' AND column_name='codex_model'`).Scan(&columns); err != nil || columns != 0 {
		t.Fatalf("Down codex columns=%d: %v", columns, err)
	}
}
