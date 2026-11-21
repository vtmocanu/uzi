package store_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestAgentTemplateOriginLiveDB pins the PRD #602 (M1) scope-aware `origin`
// provenance column and the boot-reconcile guard it enables. The in-memory
// reconcilerFake cannot model the CHECK constraint, the origin='embedded' WHERE
// guard, or the UpdateAgentTemplate/ResetBuiltinAgentTemplate origin lockstep, so
// this exercises the actual SQL at the query level the reconciler uses
// (InsertBuiltinAgentTemplate + RefreshPristineBuiltin) plus a real
// ReconcileBuiltinTemplates boot to prove a synced/admin row is never clobbered.
//
// See adr/0602-agent-source-repo-sync.md, "Provenance is a scope-aware `origin`
// column". Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.
func TestAgentTemplateOriginLiveDB(t *testing.T) {
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
	admin, userA := uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{admin, userA} {
		mustExec(ctx, t, pool,
			`INSERT INTO users (id, email, password_hash, is_admin) VALUES ($1, $2, 'x', true)`,
			id, fmt.Sprintf("origin-%s-%s@e2e", suffix, id))
	}

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
		return row
	}
	readBack := func(name string) store.AgentTemplate {
		t.Helper()
		row, err := q.GetSharedAgentTemplateByName(ctx, name)
		if err != nil {
			t.Fatalf("read back %q: %v", name, err)
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

	// --- 1. InsertBuiltinAgentTemplate produces origin='embedded' ---------------
	nEmbedded := "coder-" + suffix
	seeded := seedBuiltin(nEmbedded, "d1", "v1\n")
	if seeded.Origin.String != "embedded" || !seeded.Origin.Valid {
		t.Fatalf("a freshly-seeded builtin must have origin='embedded'; got %+v", seeded.Origin)
	}

	// --- 2. Boot-reconcile guard: a synced builtin (origin='synced',
	// customized=false) whose body differs from shipped is NOT clobbered ---------
	nSynced := "synced-" + suffix
	seedBuiltin(nSynced, "d1", "v1\n")
	mustExec(ctx, t, pool,
		`UPDATE agent_templates SET origin='synced', prompt_body='SYNCED BODY', description='synced-desc' WHERE name=$1 AND scope='builtin'`, nSynced)
	if got := refresh(nSynced, "d2", "v2\n"); got != 0 {
		t.Errorf("a synced builtin must be excluded from the boot refresh; got %d rows", got)
	}
	if kept := readBack(nSynced); kept.PromptBody != "SYNCED BODY" || kept.Origin.String != "synced" {
		t.Errorf("synced builtin must keep its body and origin; got body=%q origin=%+v", kept.PromptBody, kept.Origin)
	}

	// --- 3. Boot-reconcile guard: an admin builtin (origin='admin',
	// customized=true) is not clobbered -----------------------------------------
	nAdmin := "admin-" + suffix
	seedBuiltin(nAdmin, "d1", "v1\n")
	mustExec(ctx, t, pool,
		`UPDATE agent_templates SET origin='admin', customized=true, prompt_body='ADMIN BODY' WHERE name=$1 AND scope='builtin'`, nAdmin)
	if got := refresh(nAdmin, "d2", "v2\n"); got != 0 {
		t.Errorf("an admin builtin must be excluded from the boot refresh; got %d rows", got)
	}
	if kept := readBack(nAdmin); kept.PromptBody != "ADMIN BODY" || kept.Origin.String != "admin" {
		t.Errorf("admin builtin must keep its body and origin; got body=%q origin=%+v", kept.PromptBody, kept.Origin)
	}

	// --- 4. A pristine embedded builtin whose shipped body differs IS refreshed --
	nStale := "stale-" + suffix
	seedBuiltin(nStale, "d1", "v1\n")
	if got := refresh(nStale, "d2", "v2\n"); got != 1 {
		t.Fatalf("a pristine embedded builtin must be refreshed; got %d rows", got)
	}
	if after := readBack(nStale); after.PromptBody != "v2\n" || after.Origin.String != "embedded" {
		t.Errorf("refreshed builtin must converge to shipped body with origin unchanged; got body=%q origin=%+v", after.PromptBody, after.Origin)
	}

	// --- 5. UpdateAgentTemplate keeps a builtin's origin in lockstep with
	// customized: an admin edit -> 'admin', an idempotent shipped save -> 'embedded'
	nUpd := "upd-" + suffix
	rowUpd := seedBuiltin(nUpd, "d1", "v1\n")
	edited, err := q.UpdateAgentTemplate(ctx, store.UpdateAgentTemplateParams{
		ID: rowUpd.ID, Description: "e", PromptBody: "edit\n", UpdatedBy: pgUUID(admin), Customized: true,
	})
	if err != nil {
		t.Fatalf("admin edit: %v", err)
	}
	if edited.Origin.String != "admin" {
		t.Errorf("an admin edit (customized=true) must set origin='admin'; got %+v", edited.Origin)
	}
	unedited, err := q.UpdateAgentTemplate(ctx, store.UpdateAgentTemplateParams{
		ID: rowUpd.ID, Description: "d1", PromptBody: "v1\n", UpdatedBy: pgUUID(admin), Customized: false,
	})
	if err != nil {
		t.Fatalf("idempotent shipped save: %v", err)
	}
	if unedited.Origin.String != "embedded" {
		t.Errorf("a shipped-body save (customized=false) must return origin='embedded'; got %+v", unedited.Origin)
	}

	// --- 6. ResetBuiltinAgentTemplate returns origin='embedded' -----------------
	nReset := "reset-" + suffix
	rowReset := seedBuiltin(nReset, "d1", "v1\n")
	if _, err := q.UpdateAgentTemplate(ctx, store.UpdateAgentTemplateParams{
		ID: rowReset.ID, Description: "e", PromptBody: "edit\n", UpdatedBy: pgUUID(admin), Customized: true,
	}); err != nil {
		t.Fatalf("edit before reset: %v", err)
	}
	reset, err := q.ResetBuiltinAgentTemplate(ctx, store.ResetBuiltinAgentTemplateParams{
		ID: rowReset.ID, Description: "d1", PromptBody: "v1\n", UpdatedBy: pgUUID(admin),
	})
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	if reset.Origin.String != "embedded" || reset.Customized {
		t.Errorf("reset must return origin='embedded' and customized=false; got origin=%+v customized=%v", reset.Origin, reset.Customized)
	}

	// --- 7. A scope='global', origin='synced' row inserts and survives a real
	// boot reconcile untouched; plain global (NULL) and user (NULL) are untouched -
	insertRaw := func(name, scope string, isBuiltin bool, origin, userID any) error {
		_, err := pool.Exec(ctx,
			`INSERT INTO agent_templates (name, description, prompt_body, scope, is_builtin, origin, user_id)
			 VALUES ($1, 'd', 'b', $2, $3, $4, $5)`,
			name, scope, isBuiltin, origin, userID)
		return err
	}
	nGlobalSynced := "gsync-" + suffix
	if err := insertRaw(nGlobalSynced, "global", false, "synced", nil); err != nil {
		t.Fatalf("insert global synced row: %v", err)
	}
	nGlobalNull := "gnull-" + suffix
	if err := insertRaw(nGlobalNull, "global", false, nil, nil); err != nil {
		t.Fatalf("insert plain global row: %v", err)
	}
	uRow := mustCreateTemplate(ctx, t, q, "usr-"+suffix, "user", userA)
	if uRow.Origin.Valid {
		t.Errorf("a created user template must have NULL origin; got %+v", uRow.Origin)
	}

	// A real boot: seeds the embedded builtins, must not touch any of the above.
	if _, err := store.ReconcileBuiltinTemplates(ctx, q); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if r := readBack(nGlobalSynced); r.Origin.String != "synced" || r.PromptBody != "b" {
		t.Errorf("global synced row must survive boot untouched; got origin=%+v body=%q", r.Origin, r.PromptBody)
	}
	if r := readBack(nGlobalNull); r.Origin.Valid {
		t.Errorf("plain global row must keep NULL origin across boot; got %+v", r.Origin)
	}
	uAfter, err := q.GetAgentTemplate(ctx, uRow.ID)
	if err != nil {
		t.Fatalf("read back user row: %v", err)
	}
	if uAfter.Origin.Valid {
		t.Errorf("user row must keep NULL origin across boot; got %+v", uAfter.Origin)
	}

	// --- 8. CHECK-constraint negative tests ------------------------------------
	negatives := []struct {
		desc           string
		name, scope    string
		isBuiltin      bool
		origin, userID any
	}{
		{"global origin embedded", "n-gemb-" + suffix, "global", false, "embedded", nil},
		{"global origin admin", "n-gadm-" + suffix, "global", false, "admin", nil},
		{"user origin synced", "n-usync-" + suffix, "user", false, "synced", userA},
		// A builtin MUST carry a concrete provenance. The `origin IS NOT NULL`
		// guard in the builtin branch of the CHECK is what rejects this: without
		// it, a SQL CHECK is SATISFIED when its expression evaluates to NULL, and
		// `origin IN ('embedded','synced','admin')` is NULL — not FALSE — for a
		// NULL origin, so the row would be wrongly accepted. See adr/0602.
		{"builtin origin null", "n-bnull-" + suffix, "builtin", true, nil, nil},
	}
	for _, c := range negatives {
		if err := insertRaw(c.name, c.scope, c.isBuiltin, c.origin, c.userID); err == nil {
			t.Errorf("CHECK must reject %s", c.desc)
		}
	}

	// --- 9. CHECK-constraint positive tests ------------------------------------
	if err := insertRaw("p-bsync-"+suffix, "builtin", true, "synced", nil); err != nil {
		t.Errorf("scope='builtin', origin='synced' must be accepted; got %v", err)
	}
	if err := insertRaw("p-gsync-"+suffix, "global", false, "synced", nil); err != nil {
		t.Errorf("scope='global', origin='synced' must be accepted; got %v", err)
	}
}

// TestAgentTemplateEffortOriginLiveDB pins the PRD #3258 effort pins against the SQL the
// origin/customized lockstep runs on: an effort edit on a builtin is a customization that blocks
// the boot refresh (origin 'admin'), and Reset restores the shipped NULL with customized=false.
func TestAgentTemplateEffortOriginLiveDB(t *testing.T) {
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
	admin := uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash, is_admin) VALUES ($1, $2, 'x', true)`,
		admin, fmt.Sprintf("effort-origin-%s@e2e", suffix))
	text := func(s string) pgtype.Text { return pgtype.Text{String: s, Valid: s != ""} }

	name := "effort-" + suffix
	if _, err := q.InsertBuiltinAgentTemplate(ctx, store.InsertBuiltinAgentTemplateParams{
		Name: name, Description: "d1", PromptBody: "v1\n",
	}); err != nil {
		t.Fatalf("seed builtin: %v", err)
	}
	row, err := q.GetSharedAgentTemplateByName(ctx, name)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if row.Effort.Valid || row.CodexEffort.Valid {
		t.Fatalf("a seeded builtin with no shipped effort must store NULL; got %+v %+v", row.Effort, row.CodexEffort)
	}

	// An effort-only edit (customized=true, as the handler decides via SameContent) sets origin
	// 'admin' and both effort lanes are stored independently.
	edited, err := q.UpdateAgentTemplate(ctx, store.UpdateAgentTemplateParams{
		ID: row.ID, Description: "d1", PromptBody: "v1\n", UpdatedBy: pgUUID(admin), Customized: true,
		Effort: text("high"), CodexEffort: text("xhigh"),
	})
	if err != nil {
		t.Fatalf("effort edit: %v", err)
	}
	if !edited.Customized || edited.Origin.String != "admin" ||
		edited.Effort.String != "high" || edited.CodexEffort.String != "xhigh" {
		t.Fatalf("effort edit: customized=%v origin=%+v effort=%+v codex=%+v", edited.Customized, edited.Origin, edited.Effort, edited.CodexEffort)
	}
	// The boot refresh, even with a shipped body change, leaves the customized row alone.
	n, err := q.RefreshPristineBuiltin(ctx, store.RefreshPristineBuiltinParams{Name: name, Description: "d2", PromptBody: "v2\n"})
	if err != nil || n != 0 {
		t.Fatalf("customized effort row must be excluded from refresh: n=%d err=%v", n, err)
	}
	kept, _ := q.GetAgentTemplate(ctx, row.ID)
	if kept.Effort.String != "high" || kept.CodexEffort.String != "xhigh" || kept.PromptBody != "v1\n" {
		t.Errorf("customized row changed: %+v", kept)
	}

	// The save that restores the shipped values (NULL effort) is customized=false and 'embedded'.
	back, err := q.UpdateAgentTemplate(ctx, store.UpdateAgentTemplateParams{
		ID: row.ID, Description: "d1", PromptBody: "v1\n", UpdatedBy: pgUUID(admin), Customized: false,
	})
	if err != nil || back.Customized || back.Origin.String != "embedded" || back.Effort.Valid || back.CodexEffort.Valid {
		t.Fatalf("shipped-values save: %+v err=%v", back, err)
	}

	// Reset after a customization restores the shipped NULL effort and customized=false.
	if _, err := q.UpdateAgentTemplate(ctx, store.UpdateAgentTemplateParams{
		ID: row.ID, Description: "d1", PromptBody: "v1\n", UpdatedBy: pgUUID(admin), Customized: true,
		Effort: text("max"), CodexEffort: text("low"),
	}); err != nil {
		t.Fatalf("effort edit before reset: %v", err)
	}
	reset, err := q.ResetBuiltinAgentTemplate(ctx, store.ResetBuiltinAgentTemplateParams{
		ID: row.ID, Description: "d1", PromptBody: "v1\n", UpdatedBy: pgUUID(admin),
	})
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	if reset.Customized || reset.Origin.String != "embedded" || reset.Effort.Valid || reset.CodexEffort.Valid {
		t.Errorf("reset must restore NULL efforts, customized=false, origin embedded; got %+v", reset)
	}

	// A global synced row stores the pins through the synced writers too.
	ins, err := q.InsertSyncedGlobalTemplate(ctx, store.InsertSyncedGlobalTemplateParams{
		Name: "gs-" + suffix, Description: "d", PromptBody: "b", UpdatedBy: pgUUID(admin), Effort: text("low"),
	})
	if err != nil || ins.Effort.String != "low" || ins.CodexEffort.Valid {
		t.Fatalf("synced insert: %+v err=%v", ins, err)
	}
	upd, err := q.UpdateSyncedGlobalTemplate(ctx, store.UpdateSyncedGlobalTemplateParams{
		Name: "gs-" + suffix, Description: "d", PromptBody: "b", UpdatedBy: pgUUID(admin), CodexEffort: text("max"),
	})
	if err != nil || upd.Effort.Valid || upd.CodexEffort.String != "max" {
		t.Fatalf("synced update: %+v err=%v", upd, err)
	}
	ov, err := q.ApplySyncedOverrideBuiltin(ctx, store.ApplySyncedOverrideBuiltinParams{
		ID: row.ID, Description: "d1", PromptBody: "v1\n", UpdatedBy: pgUUID(admin), Effort: text("medium"),
	})
	if err != nil || ov.Effort.String != "medium" || ov.CodexEffort.Valid || ov.Origin.String != "synced" {
		t.Fatalf("synced override: %+v err=%v", ov, err)
	}
	// The CHECK rejects an out-of-contract value written through a query (defence in depth).
	if _, err := q.UpdateAgentTemplate(ctx, store.UpdateAgentTemplateParams{
		ID: row.ID, Description: "d1", PromptBody: "v1\n", UpdatedBy: pgUUID(admin), Customized: true, Effort: text("runtime"),
	}); err == nil {
		t.Error("the CHECK must reject effort 'runtime'")
	}
}
