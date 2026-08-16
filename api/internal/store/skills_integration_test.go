package store_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestSkillsVisibilityLiveDB pins the read-authz guarantee the fake-store unit
// tests cannot cover: the visibility WHERE clauses in ListSkillsForViewer /
// GetSkillForViewer / ListAllocationsForTemplateForViewer. It is the SQL side of
// the Success Criterion "a user's private skill never appears in another user's
// listing or allocation view".
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (the e2e
// runner provides one); `go test ./...` without it SKIPs.
func TestSkillsVisibilityLiveDB(t *testing.T) {
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

	// Unique-per-run ids and names so re-runs against the same DB never collide
	// on uq_skills_shared_name (name is globally unique for shared scopes).
	suffix := uuid.NewString()[:8]
	userA, userB, adminID := uuid.New(), uuid.New(), uuid.New()
	for _, u := range []struct {
		id      uuid.UUID
		isAdmin bool
	}{{userA, false}, {userB, false}, {adminID, true}} {
		mustExec(ctx, t, pool,
			`INSERT INTO users (id, email, password_hash, is_admin) VALUES ($1, $2, 'x', $3)`,
			u.id, fmt.Sprintf("skill-%s-%s@e2e", suffix, u.id), u.isAdmin)
	}

	globalName := "g-" + suffix
	aName := "priv-a-" + suffix
	bName := "priv-b-" + suffix
	global := mustCreateSkill(ctx, t, q, globalName, "global one.", "global", uuid.Nil)
	a1 := mustCreateSkill(ctx, t, q, aName, "a private.", "user", userA)
	b1 := mustCreateSkill(ctx, t, q, bName, "b private.", "user", userB)

	// --- List visibility ------------------------------------------------------
	names := func(who uuid.UUID, admin bool) map[string]bool {
		rows, err := q.ListSkillsForViewer(ctx, store.ListSkillsForViewerParams{IsAdmin: admin, ViewerID: pgUUID(who)})
		if err != nil {
			t.Fatalf("list for viewer: %v", err)
		}
		out := map[string]bool{}
		for _, s := range rows {
			out[s.Name] = true
		}
		return out
	}

	bView := names(userB, false)
	if !bView[globalName] || !bView[bName] {
		t.Errorf("user B must see global + own skill; got %v", bView)
	}
	if bView[aName] {
		t.Error("user B must NOT see user A's private skill in the listing")
	}
	aView := names(userA, false)
	if aView[bName] {
		t.Error("user A must NOT see user B's private skill")
	}
	adminView := names(adminID, true)
	if !adminView[aName] || !adminView[bName] || !adminView[globalName] {
		t.Errorf("admin must see all scopes; got %v", adminView)
	}

	// --- Single-get visibility ------------------------------------------------
	if _, err := q.GetSkillForViewer(ctx, store.GetSkillForViewerParams{ID: a1.ID, IsAdmin: false, ViewerID: pgUUID(userB)}); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("user B GET of user A's skill: want ErrNoRows, got %v", err)
	}
	if _, err := q.GetSkillForViewer(ctx, store.GetSkillForViewerParams{ID: a1.ID, IsAdmin: false, ViewerID: pgUUID(userA)}); err != nil {
		t.Errorf("owner GET of own skill failed: %v", err)
	}

	// --- Allocation overlay isolation -----------------------------------------
	templateID := uuid.New()
	mustExec(ctx, t, pool,
		`INSERT INTO agent_templates (id, name, description, prompt_body) VALUES ($1, $2, 'd', 'b')`,
		templateID, "tmpl-"+suffix)
	if err := q.InsertSharedAllocation(ctx, store.InsertSharedAllocationParams{TemplateID: templateID, SkillID: global.ID}); err != nil {
		t.Fatalf("insert shared allocation: %v", err)
	}
	if err := q.InsertUserAllocation(ctx, store.InsertUserAllocationParams{TemplateID: templateID, SkillID: a1.ID, UserID: pgUUID(userA)}); err != nil {
		t.Fatalf("insert user A allocation: %v", err)
	}
	if err := q.InsertUserAllocation(ctx, store.InsertUserAllocationParams{TemplateID: templateID, SkillID: b1.ID, UserID: pgUUID(userB)}); err != nil {
		t.Fatalf("insert user B allocation: %v", err)
	}

	allocs, err := q.ListAllocationsForTemplateForViewer(ctx, store.ListAllocationsForTemplateForViewerParams{TemplateID: templateID, ViewerID: pgUUID(userB)})
	if err != nil {
		t.Fatalf("list allocations for B: %v", err)
	}
	var sawShared, sawMine, sawOther bool
	for _, r := range allocs {
		switch r.SkillName {
		case globalName:
			sawShared = !r.UserID.Valid // the shared row carries a NULL user_id
		case bName:
			sawMine = r.UserID.Valid
		case aName:
			sawOther = true
		}
	}
	if !sawShared {
		t.Error("user B allocation view must include the shared row")
	}
	if !sawMine {
		t.Error("user B allocation view must include B's own overlay")
	}
	if sawOther {
		t.Error("user B allocation view must NOT include user A's overlay row")
	}

	// --- Builtin reconcile idempotency ---------------------------------------
	biName := "builtin-" + suffix
	n, err := q.InsertBuiltinSkill(ctx, store.InsertBuiltinSkillParams{Name: biName, Description: "b.", Body: "body\n"})
	if err != nil || n != 1 {
		t.Fatalf("first builtin insert: n=%d err=%v", n, err)
	}
	n, err = q.InsertBuiltinSkill(ctx, store.InsertBuiltinSkillParams{Name: biName, Description: "edited.", Body: "changed\n"})
	if err != nil || n != 0 {
		t.Fatalf("second builtin insert must be a no-op: n=%d err=%v", n, err)
	}
}

func mustCreateSkill(ctx context.Context, t *testing.T, q *store.Queries, name, desc, scope string, owner uuid.UUID) store.Skill {
	t.Helper()
	var uid pgtype.UUID
	if owner != uuid.Nil {
		uid = pgUUID(owner)
	}
	s, err := q.CreateSkill(ctx, store.CreateSkillParams{
		Name:        name,
		Description: desc,
		Body:        "# " + name + "\n\nbody\n",
		Scope:       scope,
		UserID:      uid,
		UpdatedBy:   uid,
	})
	if err != nil {
		t.Fatalf("create skill %q: %v", name, err)
	}
	return s
}

// TestListRunSkillAllocationsHardeningLiveDB pins the auditor's M3 Low
// defense-in-depth: even if a future handler bug wrote a bad allocation row, the
// claim-assembly query must never ship a private skill body. It inserts corrupted
// rows DIRECTLY (bypassing the handler): a shared row (user_id NULL) pointing at a
// user-scope skill, and one user's overlay pointing at ANOTHER user's private
// skill — and asserts neither is delivered.
func TestListRunSkillAllocationsHardeningLiveDB(t *testing.T) {
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
	userA, userB, userC := uuid.New(), uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{userA, userB, userC} {
		mustExec(ctx, t, pool,
			`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
			id, fmt.Sprintf("hard-%s-%s@e2e", suffix, id))
	}
	global := mustCreateSkill(ctx, t, q, "g-"+suffix, "global.", "global", uuid.Nil)
	a1 := mustCreateSkill(ctx, t, q, "a-"+suffix, "a private.", "user", userA)
	b1 := mustCreateSkill(ctx, t, q, "b-"+suffix, "b private.", "user", userB)

	templateID := uuid.New()
	mustExec(ctx, t, pool,
		`INSERT INTO agent_templates (id, name, description, prompt_body) VALUES ($1, $2, 'd', 'b')`,
		templateID, "tmpl-"+suffix)
	alloc := func(skillID uuid.UUID, userID *uuid.UUID) {
		if userID == nil {
			mustExec(ctx, t, pool, `INSERT INTO agent_skill_allocations (template_id, skill_id, user_id) VALUES ($1, $2, NULL)`, templateID, skillID)
		} else {
			mustExec(ctx, t, pool, `INSERT INTO agent_skill_allocations (template_id, skill_id, user_id) VALUES ($1, $2, $3)`, templateID, skillID, *userID)
		}
	}
	alloc(global.ID, nil) // legit shared
	alloc(a1.ID, nil)     // CORRUPTED shared → points at userA's private skill
	alloc(a1.ID, &userA)  // legit overlay (userA's own skill)
	alloc(b1.ID, &userA)  // CORRUPTED overlay → userA overlay pointing at userB's private

	namesFor := func(who uuid.UUID) map[string]bool {
		rows, err := q.ListRunSkillAllocations(ctx, pgUUID(who))
		if err != nil {
			t.Fatalf("list run skill allocations: %v", err)
		}
		out := map[string]bool{}
		for _, r := range rows {
			out[r.SkillName] = true
		}
		return out
	}

	// A neutral user's run: the legit global shared row is delivered, but the
	// corrupted shared row pointing at userA's private skill is NOT.
	c := namesFor(userC)
	if !c[global.Name] {
		t.Errorf("userC run must receive the global shared skill; got %v", c)
	}
	if c[a1.Name] {
		t.Error("a shared allocation pointing at a user-scope skill must NOT ship its private body")
	}

	// userA's run: the global shared row and userA's own overlay are delivered, but
	// the corrupted overlay pointing at userB's private skill is NOT.
	a := namesFor(userA)
	if !a[global.Name] || !a[a1.Name] {
		t.Errorf("userA run must receive global + own overlay skill; got %v", a)
	}
	if a[b1.Name] {
		t.Error("a user's overlay pointing at ANOTHER user's private skill must NOT ship its body")
	}
}

func pgUUID(id uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: id, Valid: true} }
