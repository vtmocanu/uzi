package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestLeadTemplatePinSettingsLiveDB(t *testing.T) {
	h, _, pool := cliLiveDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	exercised := 0
	for _, tc := range []struct {
		name, claudePin, codexPin, claudeLane, codexLane, wantClaude, wantCodex string
	}{
		{"both-template-pins", "claude-custom-template", "gpt-6-luna", "", "", "claude-custom-template", "gpt-6-luna"},
		{"claude-lane", "claude-custom-template", "gpt-6-luna", "haiku", "", "haiku", "gpt-6-luna"},
		{"codex-lane", "claude-custom-template", "gpt-6-luna", "", "gpt-6-sol", "claude-custom-template", "gpt-6-sol"},
		{"both-lanes", "claude-custom-template", "gpt-6-luna", "sonnet", "gpt-6-astra", "sonnet", "gpt-6-astra"},
		{"invalid-codex", "claude-custom-template", "not-a-role", "", "", "claude-custom-template", "gpt-6.1-sol"},
		{"claude-alias-in-codex", "claude-custom-template", "opus", "", "", "claude-custom-template", "gpt-6.1-sol"},
		{"empty-codex", "claude-custom-template", "", "", "", "claude-custom-template", "gpt-6.1-sol"},
		{"legacy-claude-luna", "gpt-6-luna", "gpt-6-luna", "", "", "", "gpt-6-luna"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			user := cliSeedUser(t, pool, false)
			defer func() {
				cleanupCtx, done := context.WithTimeout(context.Background(), 3*time.Second)
				defer done()
				if _, err := pool.Exec(cleanupCtx, "DELETE FROM users WHERE id=$1", user); err != nil {
					t.Errorf("cleanup: %v", err)
				}
			}()
			exec := func(sql string, args ...any) {
				t.Helper()
				if _, err := pool.Exec(ctx, sql, args...); err != nil {
					t.Fatal(err)
				}
			}
			exec(`INSERT INTO agent_template_allocations(template_id,user_id,enabled)
    SELECT id,$1,false FROM agent_templates WHERE lower(name) IN ('lead','orchestrator')
    ON CONFLICT DO NOTHING`, user)
			id := uuid.New()
			exec(`INSERT INTO agent_templates(id,name,description,prompt_body,scope,user_id,model,codex_model)
    VALUES($1,'ORCHESTRATOR','d','b','user',$2,$3,$4)`, id, user, tc.claudePin, tc.codexPin)
			exec("INSERT INTO agent_template_allocations(template_id,user_id,enabled) VALUES($1,$2,true)", id, user)
			exec(`UPDATE users SET default_claude_model=NULLIF($2,''),default_codex_model=NULLIF($3,''),
    default_effort='max',default_codex_effort='medium' WHERE id=$1`, user, tc.claudeLane, tc.codexLane)
			assertCells := func(cells []crossCheckPinDTO, pinned bool) {
				t.Helper()
				for _, cell := range cells {
					want := tc.wantClaude
					if cell.Harness == "codex" {
						want = tc.wantCodex
					}
					assertMetadataModel(t, cell.WorkerDefaultModel, want)
					effective, effort, modelSource, effortSource := want, "high", "worker default", "cross-check default"
					if pinned {
						effective, effort, modelSource, effortSource = "sonnet", "low", "pin", "pin"
						if cell.Harness == "codex" {
							effective, effort = "gpt-6-astra", "xhigh"
						}
					}
					assertMetadataModel(t, cell.ResolvedModel, effective)
					if cell.ResolvedEffort != effort || cell.ModelSource != modelSource || cell.EffortSource != effortSource || !cell.Active {
						t.Fatalf("%s/%s metadata=%+v, want effort=%s sources=%s/%s", cell.Stage, cell.Harness, cell, effort, modelSource, effortSource)
					}
					if pinned {
						assertMetadataModel(t, cell.Model, effective)
						assertMetadataModel(t, cell.Effort, effort)
					} else if cell.Model != nil || cell.Effort != nil {
						t.Fatalf("unexpected saved pin: %+v", cell)
					}
				}
			}
			get := func(pinned bool) {
				rec := httptest.NewRecorder()
				h.GetMySettings(rec, userReq(http.MethodGet, "/api/me/settings", "", user, nil))
				assertCells(metadataCells(t, rec), pinned)
			}
			// userReq supplies the authenticated context; retain it for real handler calls.
			get(false)
			rec := httptest.NewRecorder()
			h.PutMySettings(rec, userReq(http.MethodPut, "/api/me/settings", "{}", user, nil))
			assertCells(metadataCells(t, rec), false)
			rec = httptest.NewRecorder()
			h.PutMySettings(rec, userReq(http.MethodPut, "/api/me/settings",
				`{"cross_check_pins":[{"stage":"plan","harness":"claude","model":"sonnet","effort":"low"},{"stage":"plan","harness":"codex","model":"gpt-6-astra","effort":"xhigh"},{"stage":"code","harness":"claude","model":"sonnet","effort":"low"},{"stage":"code","harness":"codex","model":"gpt-6-astra","effort":"xhigh"}]}`, user, nil))
			assertCells(metadataCells(t, rec), true)
			get(true)
			rec = httptest.NewRecorder()
			h.PutMySettings(rec, userReq(http.MethodPut, "/api/me/settings", "{}", user, nil))
			assertCells(metadataCells(t, rec), true)
			t.Logf("case=%s plan/code GET/PUT defaults=(%q,%q) preserved under explicit pins", tc.name, tc.wantClaude, tc.wantCodex)
			exercised++
		})
	}
	if exercised != 8 {
		t.Fatalf("exercised %d settings cases, want eight", exercised)
	}
	t.Logf("exercised %d settings cases, both stages and families", exercised)
}
