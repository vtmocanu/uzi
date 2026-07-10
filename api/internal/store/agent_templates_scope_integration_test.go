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
// reconciler's new partial-unique conflict target. After the 00047 migration the
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
