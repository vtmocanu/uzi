package store_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// The SQL half of PRD #72 M2. The reconciler's unit tests use a fake and so
// cannot see what the query actually MATCHES — and the sharpest bug in this
// milestone is a matching bug, not a control-flow one.
//
// uq_skills_shared_name is a PARTIAL unique index (`ON skills (name) WHERE scope
// <> 'user'`), so a skill name is unique only across the shared scopes. `WHERE
// name = @skill_name` alone therefore matches every private user skill of that
// name too, and the INSERT … SELECT would create one SHARED (user_id NULL)
// allocation per matching row — publishing a private body to every user's runs.
// The user-scoped rows in these fixtures are the whole point: without them the
// scope guard is unpinned and an implementation that dropped it still passes.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (the e2e
// runner provides one); `go test ./...` without it SKIPs.
func TestSeedSharedSkillAllocationScopeGuardsLiveDB(t *testing.T) {
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
	defer pool.Close()
	q := store.New(pool)

	// Unique per run so repeated runs against the same database never collide on
	// the shared-name uniques.
	suffix := uuid.NewString()[:8]
	skillName := "seed-skill-" + suffix
	tmplName := "seed-tmpl-" + suffix

	userA, userB := uuid.New(), uuid.New()
	for _, u := range []uuid.UUID{userA, userB} {
		mustExec(ctx, t, pool,
			`INSERT INTO users (id, email, password_hash, is_admin) VALUES ($1, $2, 'x', false)`,
			u, fmt.Sprintf("seed-%s-%s@e2e", suffix, u))
	}

	// The shared skill the seed must find...
	shared := mustCreateSkill(ctx, t, q, skillName, "shared one.", "global", uuid.Nil)
	// ...and TWO private skills of the SAME name. Legal: uq_skills_user_name is
	// (user_id, name). An unguarded lookup returns three rows, not one.
	privA := mustCreateSkill(ctx, t, q, skillName, "a private.", "user", userA)
	privB := mustCreateSkill(ctx, t, q, skillName, "b private.", "user", userB)

	// The shared template the seed must find, plus a user-owned template of the
	// same name (legal per uq_agent_templates_user_name).
	sharedTmpl, userTmpl := uuid.New(), uuid.New()
	mustExec(ctx, t, pool,
		`INSERT INTO agent_templates (id, name, description, prompt_body, scope) VALUES ($1, $2, 'd', 'b', 'global')`,
		sharedTmpl, tmplName)
	mustExec(ctx, t, pool,
		`INSERT INTO agent_templates (id, name, description, prompt_body, scope, user_id) VALUES ($1, $2, 'd', 'b', 'user', $3)`,
		userTmpl, tmplName, userA)

	seed := func() int64 {
		t.Helper()
		n, err := q.SeedSharedSkillAllocationByName(ctx, store.SeedSharedSkillAllocationByNameParams{
			SkillName:    skillName,
			TemplateName: tmplName,
		})
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
		return n
	}

	// --- One row, and the RIGHT one -------------------------------------------
	if n := seed(); n != 1 {
		t.Fatalf("expected exactly 1 seeded row (3 same-name skills x 2 same-name templates exist); got %d", n)
	}

	type alloc struct {
		templateID uuid.UUID
		skillID    uuid.UUID
	}
	var got []alloc
	rows, err := pool.Query(ctx,
		`SELECT template_id, skill_id FROM agent_skill_allocations
		  WHERE user_id IS NULL AND skill_id = ANY($1::uuid[])`,
		[]uuid.UUID{shared.ID, privA.ID, privB.ID})
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	for rows.Next() {
		var a alloc
		if err := rows.Scan(&a.templateID, &a.skillID); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, a)
	}
	rows.Close()

	if len(got) != 1 {
		t.Fatalf("expected exactly one shared allocation across the three same-name skills; got %d (%v)", len(got), got)
	}
	if got[0].skillID != shared.ID {
		switch got[0].skillID {
		case privA.ID, privB.ID:
			t.Fatal("the seed allocated a USER-SCOPED skill as a SHARED allocation — the s.scope guard is missing")
		default:
			t.Fatalf("unexpected skill_id %v", got[0].skillID)
		}
	}
	if got[0].templateID != sharedTmpl {
		t.Fatalf("the seed targeted the user-owned template — the t.scope guard is missing (got %v, want %v)",
			got[0].templateID, sharedTmpl)
	}

	// --- Idempotent -----------------------------------------------------------
	// A second seed conflicts on uq_allocations and inserts nothing. This is why
	// the reconciler's zero-row WARNING is only correct where it is called: after
	// an insert that just created the skill, no allocation can pre-exist, so a 0
	// there means "no such template" and never "already seeded".
	if n := seed(); n != 0 {
		t.Errorf("a repeated seed must insert nothing; got %d", n)
	}

	// --- A missing template reports zero, not an error ------------------------
	n, err := q.SeedSharedSkillAllocationByName(ctx, store.SeedSharedSkillAllocationByNameParams{
		SkillName:    skillName,
		TemplateName: "no-such-template-" + suffix,
	})
	if err != nil {
		t.Fatalf("a missing template must not error: %v", err)
	}
	if n != 0 {
		t.Errorf("a missing template must report 0 rows; got %d", n)
	}
}

// migrationFile is the backfill this test exercises. Named as a constant so a
// renumber at landing (goose numbers are drafts until merge, per CLAUDE.md
// §Conventions) fails loudly here rather than silently skipping the test.
const migrationFile = "migrations/00084_seed_builtin_skill_allocations.sql"

// prdDonePathMigrationFile is M4's. Named here so the non-LiveDB guard below
// covers BOTH migrations this branch adds — they renumber together.
const prdDonePathMigrationFile = "migrations/00085_run_prd_done_path.sql"

// upStatement returns the backfill's `-- +goose Up` body, read from the REAL file.
//
// This indirection is the point of the test. The first draft embedded a
// hand-copied constant, which meant editing the migration — dropping ON CONFLICT,
// dropping a scope guard — left this test green while the shipped statement was
// broken: it asserted on a copy, so nothing in production could ever fail it.
// Reading the file binds the assertions to the artifact that actually runs.
func upStatement(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(migrationFile)
	if err != nil {
		t.Fatalf("read %s (renumbered at landing?): %v", migrationFile, err)
	}
	body := string(raw)
	up := strings.Index(body, "-- +goose Up")
	down := strings.Index(body, "-- +goose Down")
	if up < 0 || down < 0 || down < up {
		t.Fatalf("%s has no Up/Down sections", migrationFile)
	}
	return body[up:down]
}

// The migration half: the backfill must be idempotent against an instance
// that already has the allocation, and must apply the same scope guards. Goose
// records the version so re-running Migrate cannot exercise this — the statement
// is therefore executed directly, which is what "the migration is idempotent"
// actually means for a data seed.
//
// The fixture substitutes unique names for the shipped literals so the assertions
// cannot collide with real seeded rows or with another test's fixtures in the
// shared throwaway database. Each substitution is checked to have actually
// happened, so a rename in the migration reddens here instead of quietly turning
// the test into a no-op.
func TestBuiltinSkillAllocationBackfillIsIdempotentLiveDB(t *testing.T) {
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
	defer pool.Close()
	q := store.New(pool)

	suffix := uuid.NewString()[:8]
	skillName := "backfill-skill-" + suffix
	tmplA, tmplB := "backfill-a-"+suffix, "backfill-b-"+suffix

	owner := uuid.New()
	mustExec(ctx, t, pool,
		`INSERT INTO users (id, email, password_hash, is_admin) VALUES ($1, $2, 'x', false)`,
		owner, fmt.Sprintf("backfill-%s@e2e", suffix))

	shared := mustCreateSkill(ctx, t, q, skillName, "shared.", "global", uuid.Nil)
	priv := mustCreateSkill(ctx, t, q, skillName, "private.", "user", owner)
	for _, name := range []string{tmplA, tmplB} {
		mustExec(ctx, t, pool,
			`INSERT INTO agent_templates (id, name, description, prompt_body, scope) VALUES ($1, $2, 'd', 'b', 'global')`,
			uuid.New(), name)
	}
	// The adversarial row for the TEMPLATE axis. Without it this fixture holds only
	// `global` templates, so `t.name IN (…)` alone already selects every row and the
	// `t.scope <> 'user'` guard is UNOBSERVABLE — deleting it from the migration
	// leaves this test green (measured by the auditor).
	//
	// The asymmetry is the lesson, not the patch: the same test DID pin the sibling
	// s.scope guard, because the fixture above carries a private same-name skill.
	// One axis got the adversarial row and the other did not, and nothing in the
	// test's name, shape, or green result distinguished them. A compound predicate
	// needs one hostile row PER conjunct.
	mustExec(ctx, t, pool,
		`INSERT INTO agent_templates (id, name, description, prompt_body, scope, user_id) VALUES ($1, $2, 'd', 'b', 'user', $3)`,
		uuid.New(), tmplA, owner)

	// The REAL statement, with the shipped literals swapped for this fixture's
	// names. Every substitution is verified, so a rename in the migration fails
	// here loudly instead of leaving a test that exercises nothing.
	backfill := upStatement(t)
	for _, sub := range []struct{ from, to string }{
		{"'ci-cd-norms'", "'" + skillName + "'"},
		{"'coder'", "'" + tmplA + "'"},
		{"'reviewer'", "'" + tmplB + "'"},
	} {
		if !strings.Contains(backfill, sub.from) {
			t.Fatalf("%s no longer contains %s; this test would assert nothing", migrationFile, sub.from)
		}
		backfill = strings.ReplaceAll(backfill, sub.from, sub.to)
	}

	count := func() int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM agent_skill_allocations WHERE skill_id = ANY($1::uuid[])`,
			[]uuid.UUID{shared.ID, priv.ID}).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		return n
	}

	mustExec(ctx, t, pool, backfill)
	if got := count(); got != 2 {
		t.Fatalf("expected 2 allocations (one per target template, private skill excluded); got %d", got)
	}
	mustExec(ctx, t, pool, backfill)
	if got := count(); got != 2 {
		t.Errorf("re-running the backfill must be a no-op; got %d", got)
	}

	// The private same-name skill received nothing (the s.scope guard).
	var onPrivate int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM agent_skill_allocations WHERE skill_id = $1`, priv.ID).Scan(&onPrivate); err != nil {
		t.Fatalf("count private: %v", err)
	}
	if onPrivate != 0 {
		t.Errorf("the backfill allocated a user-scoped skill; got %d rows", onPrivate)
	}
}

// TestMigrationFileConstantsResolve is LANDING-CRITICAL and deliberately NOT a
// LiveDB test.
//
// The renumber guard inside TestBuiltinSkillAllocationBackfillIsIdempotentLiveDB
// is silent on the gate a person actually runs. Measured: renaming the file
// leaves `go test -count=1 ./internal/store/...` GREEN with UZI_TEST_DATABASE_URL
// unset, because the t.Skip fires before upStatement ever opens the file. Only the
// live sweep catches it.
//
// That matters because this branch adds TWO migrations, goose numbers are assigned
// at MERGE time (CLAUDE.md §Conventions), and renumbering is a landing-rebase
// activity — done by someone running the ordinary gate, not the sweep. So the guard
// has to hold without a database. It uses the SAME constant, so the two cannot
// drift.
func TestMigrationFileConstantsResolve(t *testing.T) {
	for _, f := range []string{migrationFile, prdDonePathMigrationFile} {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("migration %s is missing: %v\n"+
				"If this branch was rebased and the migrations renumbered, update the constant "+
				"in this file to match. Renumber BOTH together — renumbering one reorders them.", f, err)
		}
	}
}
