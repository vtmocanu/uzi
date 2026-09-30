package store_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/skilltmpl"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #1909 M6: the product skill scope at the SQL layer. Skipped unless UZI_TEST_DATABASE_URL
// points at a throwaway Postgres (./e2e/run-store-it.sh provides one).

func productSkillsDB(t *testing.T) (context.Context, *store.Queries, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return ctx, store.New(pool), pool
}

func seedProduct(ctx context.Context, t *testing.T, pool *pgxpool.Pool, name string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO products (id, name) VALUES ($1, $2)`, id, name+"-"+id.String())
	return id
}

func seedProductSkill(ctx context.Context, t *testing.T, pool *pgxpool.Pool, product uuid.UUID, name string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	mustExec(ctx, t, pool,
		`INSERT INTO skills (id, name, description, body, scope, product_id) VALUES ($1, $2, 'd', 'body', 'product', $3)`,
		id, name, product)
	return id
}

// TestSkillsScopeConstraintNameLiveDB pins the migration's assumption about the constraint it
// drops and re-adds: 00040_skills.sql created the scope CHECK unnamed, so PostgreSQL named it
// skills_scope_check, and the migration that widens it drops that exact name. The check's
// definition must list EXACTLY the Go scope vocabulary (skilltmpl.Scopes), so a scope added to
// one side without the other is red here.
func TestSkillsScopeConstraintNameLiveDB(t *testing.T) {
	ctx, _, pool := productSkillsDB(t)
	var def string
	if err := pool.QueryRow(ctx,
		`SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conrelid = 'skills'::regclass AND conname = 'skills_scope_check'`).Scan(&def); err != nil {
		t.Fatalf("skills_scope_check is not a constraint of skills (the migration drops it by that name): %v", err)
	}
	got := regexp.MustCompile(`'([a-z]+)'::text`).FindAllStringSubmatch(def, -1)
	var scopes []string
	for _, m := range got {
		scopes = append(scopes, m[1])
	}
	sort.Strings(scopes)
	want := skilltmpl.Scopes()
	sort.Strings(want)
	if fmt.Sprint(scopes) != fmt.Sprint(want) {
		t.Fatalf("skills_scope_check allows %v, skilltmpl.Scopes is %v (%s)", scopes, want, def)
	}
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_constraint WHERE conrelid = 'skills'::regclass AND conname IN ('skills_product_scope_check')`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("skills_product_scope_check present = %d, %v; want 1", n, err)
	}
}

// TestProductSkillsSchemaInvariantsLiveDB: a product row needs a product id and only a product row
// may carry one; names are unique per product but not across products, and a product skill name
// does not collide with (or hide) a builtin, global or user skill of the same name.
func TestProductSkillsSchemaInvariantsLiveDB(t *testing.T) {
	ctx, q, pool := productSkillsDB(t)
	suffix := uuid.NewString()[:8]
	p1 := seedProduct(ctx, t, pool, "inv-a")
	p2 := seedProduct(ctx, t, pool, "inv-b")

	if _, err := pool.Exec(ctx, `INSERT INTO skills (name, description, body, scope) VALUES ($1, 'd', 'b', 'product')`, "np-"+suffix); err == nil {
		t.Error("a product-scope skill with no product_id was accepted")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO skills (name, description, body, scope, product_id) VALUES ($1, 'd', 'b', 'global', $2)`, "gp-"+suffix, p1); err == nil {
		t.Error("a global skill carrying a product_id was accepted")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO skills (name, description, body, scope) VALUES ($1, 'd', 'b', 'other')`, "os-"+suffix); err == nil {
		t.Error("an unknown scope was accepted")
	}

	name := "shared-" + suffix
	seedProductSkill(ctx, t, pool, p1, name)
	seedProductSkill(ctx, t, pool, p2, name) // the same name in another product is fine
	if _, err := pool.Exec(ctx, `INSERT INTO skills (name, description, body, scope, product_id) VALUES ($1, 'd', 'b', 'product', $2)`, name, p1); err == nil {
		t.Error("a duplicate skill name inside one product was accepted")
	}

	// A product skill's name is free in the shared namespace and in every user's...
	mustCreateSkill(ctx, t, q, name, "a global of the same name.", "global", uuid.Nil)
	if n, err := q.InsertBuiltinSkill(ctx, store.InsertBuiltinSkillParams{Name: "b-" + name, Description: "d", Body: "b"}); err != nil || n != 1 {
		t.Fatalf("builtin insert: n=%d err=%v", n, err)
	}
	// ...and a builtin / global still cannot share a name with each other (the rebuilt index).
	if _, err := q.CreateSkill(ctx, store.CreateSkillParams{Name: "b-" + name, Description: "d", Body: "b", Scope: "global"}); err == nil {
		t.Error("a global skill with a builtin's name was accepted; uq_skills_shared_name no longer covers builtin+global")
	}
	// The idempotent builtin seed still no-ops on its own name.
	if n, err := q.InsertBuiltinSkill(ctx, store.InsertBuiltinSkillParams{Name: "b-" + name, Description: "other", Body: "other"}); err != nil || n != 0 {
		t.Fatalf("second builtin insert must be a no-op: n=%d err=%v", n, err)
	}

	// Deleting a product cascades to its skills (a hard delete; products are soft-deleted in practice).
	if _, err := pool.Exec(ctx, `DELETE FROM products WHERE id = $1`, p2); err != nil {
		t.Fatalf("delete product: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM skills WHERE product_id = $1`, p2).Scan(&n); err != nil || n != 0 {
		t.Fatalf("skills of a deleted product = %d, %v; want 0", n, err)
	}
}

// TestProductSkillsNeverLeakLiveDB is the leak-closure test (PRD #1909 clarification 10). A product
// skill exists, and corrupted allocation rows (inserted DIRECTLY, bypassing the handler) point at
// it as a shared row and as an overlay. No scope-filtering query may surface it to anyone.
//
// MUTATION CHECK: reverting ListRunSkillAllocations' scope predicates from IN ('builtin','global')
// to `<> 'user'` makes the run-allocation leg red.
func TestProductSkillsNeverLeakLiveDB(t *testing.T) {
	ctx, q, pool := productSkillsDB(t)
	suffix := uuid.NewString()[:8]
	owner, admin := uuid.New(), uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, owner, fmt.Sprintf("leak-%s-o@e2e", suffix))
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash, is_admin) VALUES ($1, $2, 'x', true)`, admin, fmt.Sprintf("leak-%s-a@e2e", suffix))

	product := seedProduct(ctx, t, pool, "leak")
	psName := "prod-only-" + suffix
	ps := seedProductSkill(ctx, t, pool, product, psName)
	global := mustCreateSkill(ctx, t, q, "g-"+suffix, "global.", "global", uuid.Nil)

	templateID := uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO agent_templates (id, name, description, prompt_body) VALUES ($1, $2, 'd', 'b')`, templateID, "tmpl-"+suffix)
	mustExec(ctx, t, pool, `INSERT INTO agent_skill_allocations (template_id, skill_id, user_id) VALUES ($1, $2, NULL)`, templateID, global.ID)
	mustExec(ctx, t, pool, `INSERT INTO agent_skill_allocations (template_id, skill_id, user_id) VALUES ($1, $2, NULL)`, templateID, ps)      // CORRUPT shared
	mustExec(ctx, t, pool, `INSERT INTO agent_skill_allocations (template_id, skill_id, user_id) VALUES ($1, $2, $3)`, templateID, ps, owner) // CORRUPT overlay

	t.Run("a run's allocation union never carries it (positive control: the global one is carried)", func(t *testing.T) {
		rows, err := q.ListRunSkillAllocations(ctx, pgUUID(owner))
		if err != nil {
			t.Fatalf("ListRunSkillAllocations: %v", err)
		}
		var sawGlobal bool
		for _, r := range rows {
			if r.SkillID == ps || r.Scope == skilltmpl.ScopeProduct {
				t.Fatalf("ListRunSkillAllocations returned a product skill: %+v", r)
			}
			if r.SkillID == global.ID {
				sawGlobal = true
			}
		}
		if !sawGlobal {
			t.Fatal("the positive control (the allocated global skill) was not returned: the query is not delivering anything, so this leg proves nothing")
		}
	})

	t.Run("no viewer listing or read returns it, admins included", func(t *testing.T) {
		for _, who := range []struct {
			name   string
			admin  bool
			viewer uuid.UUID
		}{{"member", false, owner}, {"admin", true, admin}} {
			rows, err := q.ListSkillsForViewer(ctx, store.ListSkillsForViewerParams{IsAdmin: who.admin, ViewerID: pgUUID(who.viewer)})
			if err != nil {
				t.Fatalf("ListSkillsForViewer(%s): %v", who.name, err)
			}
			var sawGlobal bool
			for _, r := range rows {
				if r.ID == ps || r.Scope == skilltmpl.ScopeProduct {
					t.Errorf("ListSkillsForViewer(%s) returned a product skill: %+v", who.name, r)
				}
				if r.ID == global.ID {
					sawGlobal = true
				}
			}
			if !sawGlobal {
				t.Errorf("ListSkillsForViewer(%s) lost the global skill (positive control)", who.name)
			}
			if _, err := q.GetSkillForViewer(ctx, store.GetSkillForViewerParams{ID: ps, IsAdmin: who.admin, ViewerID: pgUUID(who.viewer)}); !errors.Is(err, pgx.ErrNoRows) {
				t.Errorf("GetSkillForViewer(%s) of a product skill = %v, want no rows", who.name, err)
			}
		}
	})

	t.Run("the template allocation view never lists it", func(t *testing.T) {
		rows, err := q.ListAllocationsForTemplateForViewer(ctx, store.ListAllocationsForTemplateForViewerParams{TemplateID: templateID, ViewerID: pgUUID(owner)})
		if err != nil {
			t.Fatalf("ListAllocationsForTemplateForViewer: %v", err)
		}
		var sawGlobal bool
		for _, r := range rows {
			if r.SkillID == ps || r.SkillScope == skilltmpl.ScopeProduct {
				t.Errorf("the allocation view returned a product skill: %+v", r)
			}
			if r.SkillID == global.ID {
				sawGlobal = true
			}
		}
		if !sawGlobal {
			t.Error("the allocation view lost the global skill (positive control)")
		}
	})

	t.Run("the default-allocation seed never targets it, even by its name", func(t *testing.T) {
		n, err := q.SeedSharedSkillAllocationByName(ctx, store.SeedSharedSkillAllocationByNameParams{SkillName: psName, TemplateName: "tmpl-" + suffix})
		if err != nil {
			t.Fatalf("SeedSharedSkillAllocationByName: %v", err)
		}
		if n != 0 {
			t.Errorf("the seed inserted %d allocation(s) for a product skill's name", n)
		}
	})

	t.Run("no skill-id write query can change or remove it", func(t *testing.T) {
		if n, err := q.DeleteSkill(ctx, ps); err != nil || n != 0 {
			t.Errorf("DeleteSkill of a product skill = %d, %v; want 0 rows", n, err)
		}
		if _, err := q.UpdateSkill(ctx, store.UpdateSkillParams{ID: ps, Description: "x", Body: "x"}); !errors.Is(err, pgx.ErrNoRows) {
			t.Errorf("UpdateSkill of a product skill = %v, want no rows", err)
		}
		var body string
		if err := pool.QueryRow(ctx, `SELECT body FROM skills WHERE id = $1`, ps).Scan(&body); err != nil || body != "body" {
			t.Errorf("the product skill after the refused writes = %q, %v; want it untouched", body, err)
		}
	})
}

// TestListProductSkillsForRunLiveDB: the claim-time read joins through job_origins, so it returns
// exactly the origin product's skills for a product job and nothing for a uzc_ job, a run with no
// origin row, a disabled product or a soft-deleted product.
func TestListProductSkillsForRunLiveDB(t *testing.T) {
	ctx, q, pool := productSkillsDB(t)
	suffix := uuid.NewString()[:8]
	user := uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, user, fmt.Sprintf("lpr-%s@e2e", suffix))

	mine := seedProduct(ctx, t, pool, "mine")
	other := seedProduct(ctx, t, pool, "other")
	seedProductSkill(ctx, t, pool, mine, "zeta")
	seedProductSkill(ctx, t, pool, mine, "alpha")
	seedProductSkill(ctx, t, pool, other, "foreign")
	mustCreateSkill(ctx, t, q, "g-"+suffix, "a global skill.", "global", uuid.Nil)

	job := func(product *uuid.UUID) uuid.UUID {
		id := uuid.New()
		mustExec(ctx, t, pool,
			`INSERT INTO runs (id, user_id, kind, job_type, issue_title, issue_description, status) VALUES ($1, $2, 'job', 'research', 't', 'p', 'queued')`, id, user)
		if product != nil {
			mustExec(ctx, t, pool, `INSERT INTO job_origins (run_id, product_id) VALUES ($1, $2)`, id, *product)
		} else {
			mustExec(ctx, t, pool, `INSERT INTO job_origins (run_id) VALUES ($1)`, id)
		}
		return id
	}
	names := func(run uuid.UUID) []string {
		rows, err := q.ListProductSkillsForRun(ctx, run)
		if err != nil {
			t.Fatalf("ListProductSkillsForRun: %v", err)
		}
		out := make([]string, 0, len(rows))
		for _, r := range rows {
			out = append(out, r.Name)
		}
		return out
	}

	productJob := job(&mine)
	if got := names(productJob); fmt.Sprint(got) != "[alpha zeta]" {
		t.Fatalf("a product job's skills = %v, want exactly its own product's [alpha zeta] by name", got)
	}
	if got := names(job(nil)); len(got) != 0 {
		t.Errorf("a uzc_ job (no origin product) got %v, want none", got)
	}
	if got := names(uuid.New()); len(got) != 0 {
		t.Errorf("a run with no origin row got %v, want none", got)
	}
	mustExec(ctx, t, pool, `UPDATE products SET enabled = false WHERE id = $1`, mine)
	if got := names(productJob); len(got) != 0 {
		t.Errorf("a disabled product's job got %v, want none", got)
	}
	mustExec(ctx, t, pool, `UPDATE products SET enabled = true WHERE id = $1`, mine)
	if got := names(productJob); len(got) != 2 {
		t.Errorf("re-enabled product's job got %v, want both skills back", got)
	}
	mustExec(ctx, t, pool, `UPDATE products SET enabled = false, deleted_at = now() WHERE id = $1`, mine)
	if got := names(productJob); len(got) != 0 {
		t.Errorf("a soft-deleted product's job got %v, want none", got)
	}
}
