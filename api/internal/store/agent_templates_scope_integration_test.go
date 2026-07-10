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

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// TestAgentTemplateScopesLiveDB pins the PRD #18 M6 SQL guarantees the fake-store
// unit tests cannot cover: the visibility WHERE clauses in
// ListAgentTemplatesForViewer / GetAgentTemplateForViewer, and — critically — the
// reconciler's new partial-unique conflict target. After the 00048 migration the
// builtin seed's ON CONFLICT keys on (name) WHERE scope <> 'user'; a user-scoped
// template of the SAME name must therefore NOT block a builtin seed (boot would
// otherwise break), while a shared-namespace row of that name is a normal no-op.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (the store
// IT runner provides one); `go test ./...` without it SKIPs.
func TestAgentTemplateScopesLiveDB(t *testing.T) {
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
	userA, userB, adminID := uuid.New(), uuid.New(), uuid.New()
	for _, u := range []struct {
		id      uuid.UUID
		isAdmin bool
	}{{userA, false}, {userB, false}, {adminID, true}} {
		mustExec(ctx, t, pool,
			`INSERT INTO users (id, email, password_hash, is_admin) VALUES ($1, $2, 'x', $3)`,
			u.id, fmt.Sprintf("tmpl-%s-%s@e2e", suffix, u.id), u.isAdmin)
	}

	globalName := "g-" + suffix
	aName := "priv-a-" + suffix
	bName := "priv-b-" + suffix
	globalT := mustCreateTemplate(ctx, t, q, globalName, "global", uuid.Nil)
	aT := mustCreateTemplate(ctx, t, q, aName, "user", userA)
	_ = mustCreateTemplate(ctx, t, q, bName, "user", userB)

	// --- List visibility ------------------------------------------------------
	names := func(who uuid.UUID, admin bool) map[string]bool {
		rows, err := q.ListAgentTemplatesForViewer(ctx, store.ListAgentTemplatesForViewerParams{IsAdmin: admin, ViewerID: pgUUID(who)})
		if err != nil {
			t.Fatalf("list for viewer: %v", err)
		}
		out := map[string]bool{}
		for _, tmpl := range rows {
			out[tmpl.Name] = true
		}
		return out
	}

	bView := names(userB, false)
	if !bView[globalName] || !bView[bName] {
		t.Errorf("user B must see global + own template; got %v", bView)
	}
	if bView[aName] {
		t.Error("user B must NOT see user A's private template in the listing")
	}
	if names(userA, false)[bName] {
		t.Error("user A must NOT see user B's private template")
	}
	adminView := names(adminID, true)
	if !adminView[aName] || !adminView[bName] || !adminView[globalName] {
		t.Errorf("admin must see all scopes; got %v", adminView)
	}

	// --- Single-get visibility ------------------------------------------------
	if _, err := q.GetAgentTemplateForViewer(ctx, store.GetAgentTemplateForViewerParams{ID: aT.ID, IsAdmin: false, ViewerID: pgUUID(userB)}); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("user B GET of user A's template: want ErrNoRows, got %v", err)
	}
	if _, err := q.GetAgentTemplateForViewer(ctx, store.GetAgentTemplateForViewerParams{ID: aT.ID, IsAdmin: false, ViewerID: pgUUID(userA)}); err != nil {
		t.Errorf("owner GET of own template failed: %v", err)
	}
	if _, err := q.GetAgentTemplateForViewer(ctx, store.GetAgentTemplateForViewerParams{ID: globalT.ID, IsAdmin: false, ViewerID: pgUUID(userB)}); err != nil {
		t.Errorf("any user GET of a global template failed: %v", err)
	}

	// --- Reconciler conflict target (the boot-safety guarantee) ----------------
	// A user-scoped template named like a builtin must NOT block the builtin seed:
	// the shared-namespace partial unique excludes scope='user', so the seed inserts.
	collideName := "coder-" + suffix
	mustCreateTemplate(ctx, t, q, collideName, "user", userA)
	n, err := q.InsertBuiltinAgentTemplate(ctx, store.InsertBuiltinAgentTemplateParams{
		Name: collideName, Description: "b.", PromptBody: "body\n",
	})
	if err != nil || n != 1 {
		t.Fatalf("builtin seed must insert despite a same-named user template: n=%d err=%v", n, err)
	}
	// Re-seeding the same builtin is a no-op (the builtin row now occupies the
	// shared namespace).
	n, err = q.InsertBuiltinAgentTemplate(ctx, store.InsertBuiltinAgentTemplateParams{
		Name: collideName, Description: "edited.", PromptBody: "changed\n",
	})
	if err != nil || n != 0 {
		t.Fatalf("second builtin seed must be a no-op: n=%d err=%v", n, err)
	}
	// A global template already in the shared namespace also makes the seed a
	// no-op (the reconciler's shadow-warning case).
	n, err = q.InsertBuiltinAgentTemplate(ctx, store.InsertBuiltinAgentTemplateParams{
		Name: globalName, Description: "b.", PromptBody: "body\n",
	})
	if err != nil || n != 0 {
		t.Fatalf("builtin seed over an existing global must be a no-op: n=%d err=%v", n, err)
	}
}

// TestTemplateAllocationsLiveDB pins the PRD #18 M7 allocation resolution the
// claim depends on: a user overlay (enabled) wins over the global default; absent
// an overlay the global default decides; a user template rides only its owner's
// claim (and only when the owner allocated it); no empty-means-all cliff.
func TestTemplateAllocationsLiveDB(t *testing.T) {
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
	userA, userB := uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{userA, userB} {
		mustExec(ctx, t, pool,
			`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
			id, fmt.Sprintf("alloc-%s-%s@e2e", suffix, id))
	}

	// g1: a global default (shared row seeded). g2: a global with its default
	// removed (no shared row). ua: userA's private template.
	g1 := mustCreateTemplate(ctx, t, q, "g1-"+suffix, "global", uuid.Nil)
	g2 := mustCreateTemplate(ctx, t, q, "g2-"+suffix, "global", uuid.Nil)
	ua := mustCreateTemplate(ctx, t, q, "ua-"+suffix, "user", userA)

	if err := q.InsertSharedTemplateAllocation(ctx, g1.ID); err != nil {
		t.Fatalf("seed g1 default: %v", err)
	}
	// userA overlay: force g1 OFF, force g2 ON, enable own ua.
	for _, o := range []struct {
		id      uuid.UUID
		enabled bool
	}{{g1.ID, false}, {g2.ID, true}, {ua.ID, true}} {
		if err := q.InsertUserTemplateAllocation(ctx, store.InsertUserTemplateAllocationParams{
			TemplateID: o.id, UserID: pgUUID(userA), Enabled: o.enabled,
		}); err != nil {
			t.Fatalf("insert userA overlay: %v", err)
		}
	}

	claimNames := func(who uuid.UUID) map[string]bool {
		rows, err := q.ListClaimAgentTemplates(ctx, pgUUID(who))
		if err != nil {
			t.Fatalf("list claim templates: %v", err)
		}
		out := map[string]bool{}
		for _, tmpl := range rows {
			out[tmpl.Name] = true
		}
		return out
	}

	// userA: overlay wins — g2 + ua delivered, g1 suppressed.
	a := claimNames(userA)
	if a[g1.Name] {
		t.Error("userA overlay force-off must suppress the global default g1")
	}
	if !a[g2.Name] || !a[ua.Name] {
		t.Errorf("userA overlay force-on must deliver g2 + own ua; got %v", a)
	}

	// userB: no overlay — only the global default g1. g2 (no default) absent; ua
	// (userA's private) never visible.
	b := claimNames(userB)
	if !b[g1.Name] {
		t.Errorf("userB must receive the global default g1; got %v", b)
	}
	if b[g2.Name] {
		t.Error("userB must NOT receive g2 (no global default, no overlay) — the no-cliff guarantee")
	}
	if b[ua.Name] {
		t.Error("userB must NOT receive userA's private template")
	}

	// Allocation view: userA sees the resolved state; userB never sees ua.
	viewA := map[string]store.ListTemplateAllocationsForViewerRow{}
	rowsA, err := q.ListTemplateAllocationsForViewer(ctx, store.ListTemplateAllocationsForViewerParams{IsAdmin: false, ViewerID: pgUUID(userA)})
	if err != nil {
		t.Fatalf("alloc view A: %v", err)
	}
	for _, r := range rowsA {
		viewA[r.Name] = r
	}
	if !viewA[g1.Name].GlobalDefault || !viewA[g1.Name].MyOverride.Valid || viewA[g1.Name].MyOverride.Bool {
		t.Errorf("g1 for userA: want global_default=true, my_override=false; got %+v", viewA[g1.Name])
	}
	if viewA[g2.Name].GlobalDefault || !viewA[g2.Name].MyOverride.Bool {
		t.Errorf("g2 for userA: want global_default=false, my_override=true; got %+v", viewA[g2.Name])
	}
	rowsB, err := q.ListTemplateAllocationsForViewer(ctx, store.ListTemplateAllocationsForViewerParams{IsAdmin: false, ViewerID: pgUUID(userB)})
	if err != nil {
		t.Fatalf("alloc view B: %v", err)
	}
	for _, r := range rowsB {
		if r.Name == ua.Name {
			t.Error("userB allocation view must NOT include userA's private template")
		}
	}
}

// TestClaimSharedPrecedenceLiveDB pins the audit acceptance criterion: a user
// template whose name collides with a builtin/global is dropped from the owner's
// claim (SHARED wins), so the worker — which keys subagents by name with no scope
// tiebreak — can never have its curated builtin displaced by a user's same-named
// template. Also pins the reconciler's shadow-warning read-back to the shared row.
func TestClaimSharedPrecedenceLiveDB(t *testing.T) {
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
	userA := uuid.New()
	mustExec(ctx, t, pool,
		`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userA, fmt.Sprintf("shadow-%s@e2e", suffix))

	// A builtin "coder-<suffix>" (seeded like the reconciler does: insert + default
	// allocation), and a user template of the SAME name allocated by userA.
	name := "coder-" + suffix
	if _, err := q.InsertBuiltinAgentTemplate(ctx, store.InsertBuiltinAgentTemplateParams{
		Name: name, Description: "the curated coder.", PromptBody: "builtin body\n",
	}); err != nil {
		t.Fatalf("insert builtin: %v", err)
	}
	if err := q.SeedSharedTemplateAllocationByName(ctx, name); err != nil {
		t.Fatalf("seed builtin default: %v", err)
	}
	uc := mustCreateTemplate(ctx, t, q, name, "user", userA)
	if err := q.InsertUserTemplateAllocation(ctx, store.InsertUserTemplateAllocationParams{
		TemplateID: uc.ID, UserID: pgUUID(userA), Enabled: true,
	}); err != nil {
		t.Fatalf("allocate user coder: %v", err)
	}

	rows, err := q.ListClaimAgentTemplates(ctx, pgUUID(userA))
	if err != nil {
		t.Fatalf("list claim templates: %v", err)
	}
	var seen []store.AgentTemplate
	for _, r := range rows {
		if r.Name == name {
			seen = append(seen, r)
		}
	}
	if len(seen) != 1 {
		t.Fatalf("claim must carry exactly one %q (shared wins, user dropped); got %d", name, len(seen))
	}
	if seen[0].Scope != "builtin" {
		t.Errorf("the surviving %q must be the builtin, not the user template; got scope %q", name, seen[0].Scope)
	}
	if seen[0].ID == uc.ID {
		t.Error("the user template must be dropped from the claim, not delivered")
	}

	// Shadow-warning read-back resolves to the shared row, not the user's same-name.
	got, err := q.GetSharedAgentTemplateByName(ctx, name)
	if err != nil {
		t.Fatalf("shared read-back: %v", err)
	}
	if got.Scope != "builtin" || got.ID == uc.ID {
		t.Errorf("GetSharedAgentTemplateByName must return the builtin, not the user row; got scope %q", got.Scope)
	}
}

func mustCreateTemplate(ctx context.Context, t *testing.T, q *store.Queries, name, scope string, owner uuid.UUID) store.AgentTemplate {
	t.Helper()
	var uid pgtype.UUID
	if owner != uuid.Nil {
		uid = pgUUID(owner)
	}
	tmpl, err := q.CreateAgentTemplate(ctx, store.CreateAgentTemplateParams{
		Name:        name,
		Description: name + " does a thing.",
		PromptBody:  "# " + name + "\n\nbody\n",
		Scope:       scope,
		UserID:      uid,
		UpdatedBy:   uid,
	})
	if err != nil {
		t.Fatalf("create template %q: %v", name, err)
	}
	return tmpl
}
