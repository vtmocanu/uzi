package store_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// TestPristineBuiltinRefreshLiveDB pins the PRD #275 (M4b) boot-time delivery
// contract that the in-memory reconcilerFake cannot model — the customized flag,
// the content guard, now()/updated_at, and the FK ON DELETE SET NULL that rules
// out updated_by as the discriminator. It exercises the SQL the reconciler runs
// (InsertBuiltinAgentTemplate + RefreshPristineBuiltin) plus the admin-edit and
// reset queries, at the same query level the reconciler uses.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (the store
// IT runner provides one); `go test ./...` without it SKIPs.
func TestPristineBuiltinRefreshLiveDB(t *testing.T) {
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
	admin, adminDel, userA := uuid.New(), uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{admin, adminDel, userA} {
		mustExec(ctx, t, pool,
			`INSERT INTO users (id, email, password_hash, is_admin) VALUES ($1, $2, 'x', true)`,
			id, fmt.Sprintf("refresh-%s-%s@e2e", suffix, id))
	}

	// seedBuiltin inserts a fresh pristine builtin the way the reconciler does.
	seedBuiltin := func(name, desc, body string) store.AgentTemplate {
		t.Helper()
		if _, err := q.InsertBuiltinAgentTemplate(ctx, store.InsertBuiltinAgentTemplateParams{
			Name: name, Description: desc, PromptBody: body,
		}); err != nil {
			t.Fatalf("seed builtin %q: %v", name, err)
		}
		row, err := q.GetSharedAgentTemplateByName(ctx, name)
		if err != nil {
			t.Fatalf("read back builtin %q: %v", name, err)
		}
		if row.Customized {
			t.Fatalf("a freshly-seeded builtin %q must be pristine (customized=false)", name)
		}
		return row
	}
	refresh := func(name, desc, body string) int64 {
		t.Helper()
		n, err := q.RefreshPristineBuiltin(ctx, store.RefreshPristineBuiltinParams{
			Name: name, Description: desc, PromptBody: body,
		})
		if err != nil {
			t.Fatalf("refresh %q: %v", name, err)
		}
		return n
	}
	readBack := func(name string) store.AgentTemplate {
		t.Helper()
		row, err := q.GetSharedAgentTemplateByName(ctx, name)
		if err != nil {
			t.Fatalf("read back %q: %v", name, err)
		}
		return row
	}

	// --- 1. A pristine row receives a shipped body change on boot ---------------
	n1 := "coder-" + suffix
	seedBuiltin(n1, "d1", "v1\n")
	if got := refresh(n1, "d2", "v2\n"); got != 1 {
		t.Fatalf("pristine refresh must update exactly one row; got %d", got)
	}
	after := readBack(n1)
	if after.PromptBody != "v2\n" || after.Description != "d2" {
		t.Errorf("pristine row must converge to the shipped body; got desc=%q body=%q", after.Description, after.PromptBody)
	}
	if after.Customized {
		t.Error("a refreshed pristine row must stay pristine (customized=false)")
	}

	// --- 2. Content guard: an unchanged builtin is a no-op (no updated_at bump) --
	before := readBack(n1)
	if got := refresh(n1, "d2", "v2\n"); got != 0 {
		t.Errorf("re-refresh with identical content must be a no-op; got %d rows", got)
	}
	if guarded := readBack(n1); !guarded.UpdatedAt.Time.Equal(before.UpdatedAt.Time) {
		t.Errorf("content guard must leave updated_at unchanged; before=%v after=%v", before.UpdatedAt.Time, guarded.UpdatedAt.Time)
	}

	// --- 3. An admin-customized row is never overwritten -----------------------
	n3 := "lead-" + suffix
	row3 := seedBuiltin(n3, "d1", "v1\n")
	if _, err := q.UpdateAgentTemplate(ctx, store.UpdateAgentTemplateParams{
		ID: row3.ID, Description: "admin-desc", PromptBody: "admin edit\n", UpdatedBy: pgUUID(admin),
	}); err != nil {
		t.Fatalf("admin edit: %v", err)
	}
	if edited := readBack(n3); !edited.Customized {
		t.Fatal("an admin edit (UpdateAgentTemplate) must set customized=true")
	}
	if got := refresh(n3, "d2", "v2\n"); got != 0 {
		t.Errorf("a customized row must be excluded from the refresh; got %d rows", got)
	}
	if kept := readBack(n3); kept.PromptBody != "admin edit\n" {
		t.Errorf("a customized row must keep its admin body; got %q", kept.PromptBody)
	}

	// --- 4. THE discriminating case: a customized row whose editing admin was
	// deleted stays preserved. Under `updated_by IS NULL` the ON DELETE SET NULL
	// FK would make this row read as pristine and get silently overwritten.
	n4 := "reviewer-" + suffix
	row4 := seedBuiltin(n4, "d1", "v1\n")
	if _, err := q.UpdateAgentTemplate(ctx, store.UpdateAgentTemplateParams{
		ID: row4.ID, Description: "admin-desc", PromptBody: "admin edit\n", UpdatedBy: pgUUID(adminDel),
	}); err != nil {
		t.Fatalf("admin edit (deletable admin): %v", err)
	}
	mustExec(ctx, t, pool, `DELETE FROM users WHERE id = $1`, adminDel)
	deleted := readBack(n4)
	if deleted.UpdatedBy.Valid {
		t.Fatal("precondition: deleting the editing admin must NULL updated_by (ON DELETE SET NULL)")
	}
	if !deleted.Customized {
		t.Fatal("customized must survive the editing admin's deletion (this is why updated_by is unusable)")
	}
	if got := refresh(n4, "d2", "v2\n"); got != 0 {
		t.Errorf("a customized row with a deleted editor must STILL be preserved; got %d rows", got)
	}
	if kept := readBack(n4); kept.PromptBody != "admin edit\n" {
		t.Errorf("deleted-admin customized row must keep its body; got %q", kept.PromptBody)
	}

	// --- 5. Reset returns to pristine, then tracks the next shipped body (D2) ---
	n5 := "tester-" + suffix
	row5 := seedBuiltin(n5, "d1", "v1\n")
	if _, err := q.UpdateAgentTemplate(ctx, store.UpdateAgentTemplateParams{
		ID: row5.ID, Description: "admin-desc", PromptBody: "admin edit\n", UpdatedBy: pgUUID(admin),
	}); err != nil {
		t.Fatalf("admin edit before reset: %v", err)
	}
	// Reset re-applies the embedded body AND returns the row to pristine.
	reset, err := q.ResetBuiltinAgentTemplate(ctx, store.ResetBuiltinAgentTemplateParams{
		ID: row5.ID, Description: "d1", PromptBody: "v1\n", UpdatedBy: pgUUID(admin),
	})
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	if reset.Customized {
		t.Error("reset must return the row to pristine (customized=false)")
	}
	if reset.PromptBody != "v1\n" {
		t.Errorf("reset must re-apply the embedded body; got %q", reset.PromptBody)
	}
	// A subsequently shipped change now reaches the reset row (it tracks again).
	if got := refresh(n5, "d2", "v2\n"); got != 1 {
		t.Fatalf("a reset row must resume tracking upstream changes; got %d rows", got)
	}
	if tracked := readBack(n5); tracked.PromptBody != "v2\n" {
		t.Errorf("reset row must receive the new shipped body; got %q", tracked.PromptBody)
	}

	// --- 6. A user-scope template of the same name is never touched -------------
	n6 := "coder2-" + suffix
	seedBuiltin(n6, "d1", "v1\n")
	uRow := mustCreateTemplate(ctx, t, q, n6, "user", userA)
	if got := refresh(n6, "d2", "v2\n"); got != 1 {
		t.Fatalf("refresh must update the builtin, not the user row; got %d rows", got)
	}
	uAfter, err := q.GetAgentTemplate(ctx, uRow.ID)
	if err != nil {
		t.Fatalf("read back user row: %v", err)
	}
	if uAfter.PromptBody != uRow.PromptBody || uAfter.Customized {
		t.Errorf("the user-scope same-name row must be untouched; got body=%q customized=%v", uAfter.PromptBody, uAfter.Customized)
	}

	// --- 7. A shadow global row (no builtin of that name) is never touched ------
	n7 := "global-only-" + suffix
	gRow := mustCreateTemplate(ctx, t, q, n7, "global", uuid.Nil)
	if got := refresh(n7, "d2", "v2\n"); got != 0 {
		t.Errorf("refresh keyed on scope='builtin' must not touch a global row; got %d rows", got)
	}
	gAfter, err := q.GetAgentTemplate(ctx, gRow.ID)
	if err != nil {
		t.Fatalf("read back global row: %v", err)
	}
	if gAfter.PromptBody != gRow.PromptBody {
		t.Errorf("the global row must be untouched; got body=%q", gAfter.PromptBody)
	}

	// --- 8. A refresh never re-adds a default allocation an admin removed -------
	// (the reconciler seeds allocations only on a true insert; the refresh's
	// rowcount is discarded and the query itself never touches allocations).
	n8 := "alloc-" + suffix
	row8 := seedBuiltin(n8, "d1", "v1\n")
	if err := q.SeedSharedTemplateAllocationByName(ctx, n8); err != nil {
		t.Fatalf("seed default allocation: %v", err)
	}
	// Admin removes the shared default.
	mustExec(ctx, t, pool,
		`DELETE FROM agent_template_allocations WHERE template_id = $1 AND user_id IS NULL`, row8.ID)
	if got := refresh(n8, "d2", "v2\n"); got != 1 {
		t.Fatalf("pristine refresh should still update the body; got %d rows", got)
	}
	var defaults int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM agent_template_allocations WHERE template_id = $1 AND user_id IS NULL`,
		row8.ID).Scan(&defaults); err != nil {
		t.Fatalf("count shared allocations: %v", err)
	}
	if defaults != 0 {
		t.Errorf("a refresh must not re-add an admin-removed default allocation; got %d", defaults)
	}

	// --- 9. The content guard reacts to the model/tools columns, NULL<->set -----
	// The other cases leave model/tools NULL on both sides; this one flips them
	// from NULL to a value with the description/body held constant, proving the
	// IS DISTINCT FROM guard spans all four columns (and is NULL-safe on the
	// nullable ones), then that an identical re-refresh with non-NULL model/tools
	// is still a no-op.
	n9 := "guard-" + suffix
	seedBuiltin(n9, "d9", "v9\n") // seeded with model + tools NULL
	tools9 := []byte(`["Read","Grep"]`)
	set9 := store.RefreshPristineBuiltinParams{
		Name: n9, Description: "d9", PromptBody: "v9\n",
		Model: pgtype.Text{String: "claude-guard", Valid: true}, Tools: tools9,
	}
	got9, err := q.RefreshPristineBuiltin(ctx, set9)
	if err != nil {
		t.Fatalf("refresh model/tools: %v", err)
	}
	if got9 != 1 {
		t.Fatalf("a model/tools change (NULL->set) with an unchanged body must refresh; got %d rows", got9)
	}
	row9 := readBack(n9)
	if !row9.Model.Valid || row9.Model.String != "claude-guard" || len(row9.Tools) == 0 {
		t.Errorf("model/tools must be applied by the refresh; got model=%+v tools=%s", row9.Model, row9.Tools)
	}
	// Guard still holds when model/tools are non-NULL: an identical re-refresh is a
	// no-op (jsonb normalizes both sides, so whitespace in tools9 is irrelevant).
	if same9, err := q.RefreshPristineBuiltin(ctx, set9); err != nil || same9 != 0 {
		t.Errorf("identical model/tools re-refresh must be a no-op; got %d rows err=%v", same9, err)
	}
}
