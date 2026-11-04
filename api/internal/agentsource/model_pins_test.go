package agentsource

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/agenttmpl"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Exercise both raw entry points through reconcile's persisted preview and the
// staged decode/apply plan, including a second preview of the resulting row.
func TestReconcileModelPinsPreviewAndApplyPlan(t *testing.T) {
	for _, tc := range []struct {
		name, raw, claude, codex string
	}{
		{"codex", "gpt-6-sol", "", "gpt-6-sol"},
		{"luna", "gpt-6-luna", "", "gpt-6-luna"},
		{"custom-claude", "claude-custom-v2", "claude-custom-v2", ""},
		{"empty", "", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, scope := range []string{"global", "builtin"} {
				t.Run(scope, func(t *testing.T) {
					st := newFakeStore()
					r := newRec(st, &fakeSettings{enabled: true, url: "https://ok.test/a.git"}, true)
					f := SourceFile{Name: "coder.md", Data: []byte("---\nname: coder\ndescription: source coder\nmodel: " + tc.raw + "\ntools: Read\n---\nsource body\n")}
					r.fetch = stubFetch("first", []SourceFile{f}, nil)
					res, err := r.Reconcile(context.Background())
					if err != nil || res.Changed != 1 || len(st.upserts) != 1 {
						t.Fatalf("initial reconcile: %+v, %v", res, err)
					}
					snapshot := st.upserts[0]
					var rawRoles []StagedRole
					if err := json.Unmarshal(snapshot.Roles, &rawRoles); err != nil {
						t.Fatal(err)
					}
					if len(rawRoles) != 1 || rawRoles[0].Model != tc.raw {
						t.Fatalf("staging changed raw model: %+v", rawRoles)
					}
					defs, skipped, err := decodeStagedRoles(snapshot.Roles)
					if err != nil || len(skipped) != 0 || len(defs) != 1 {
						t.Fatalf("decode: %+v, %+v, %v", defs, skipped, err)
					}
					if defs[0].Model != tc.claude || defs[0].CodexModel != tc.codex {
						t.Fatalf("pins: %+v; want Claude=%q Codex=%q", defs[0], tc.claude, tc.codex)
					}
					row := tmpl("coder", scope, "synced", "source coder", "source body\n")
					// Existing pins must both be replaced, including clearing the
					// opposite family (or both when the source is empty).
					row.Model = pgtype.Text{String: "opus", Valid: true}
					row.CodexModel = pgtype.Text{String: "gpt-6-astra", Valid: true}
					st.templates = []store.AgentTemplate{row}
					r.fetch = stubFetch("changed", []SourceFile{f}, nil)
					res, err = r.Reconcile(context.Background())
					if err != nil || res.Changed != 1 {
						t.Fatalf("changed preview: %+v, %v", res, err)
					}
					op, ok := opFor(planApply(defs, st.templates), "coder")
					wantAction := ActionUpdateGlobal
					if scope == "builtin" {
						wantAction = ActionOverrideBuiltin
					}
					if !ok || op.Action != wantAction || op.Def.Model != tc.claude || op.Def.CodexModel != tc.codex {
						t.Fatalf("replacement plan: %+v", op)
					}
					// Model the applied content, including builtin product tools.
					row.Model = pgtype.Text{String: op.Def.Model, Valid: op.Def.Model != ""}
					row.CodexModel = pgtype.Text{String: op.Def.CodexModel, Valid: op.Def.CodexModel != ""}
					row.Tools = mustJSON(t, op.Def.Tools)
					st.templates = []store.AgentTemplate{row}
					r.fetch = stubFetch("second", []SourceFile{f}, nil)
					res, err = r.Reconcile(context.Background())
					if err != nil || res.Changed != 0 {
						t.Fatalf("second preview must be unchanged: %+v, %v", res, err)
					}
					if op, _ := opFor(planApply(defs, st.templates), "coder"); op.Action != ActionUnchanged {
						t.Fatalf("second plan: %+v", op)
					}
				})
			}
		})
	}
}

func TestLegacyStagedModelPins(t *testing.T) {
	// Pre-upgrade snapshots have only the raw model, with no derived fields.
	raw := []byte(`[{"name":"coder","ok":true,"description":"d","model":"gpt-6-luna","tools":["Read"],"prompt_body":"body\n"}]`)
	defs, skipped, err := decodeStagedRoles(raw)
	if err != nil || len(skipped) != 0 || len(defs) != 1 {
		t.Fatalf("decode legacy snapshot: %+v, %+v, %v", defs, skipped, err)
	}
	op, ok := opFor(planApply(defs, nil), "coder")
	if !ok || op.Action != ActionAddGlobal || op.Def.Model != "" || op.Def.CodexModel != "gpt-6-luna" {
		t.Fatalf("legacy apply plan: %+v", op)
	}
	if op.Def.PromptBody != "body\n" || len(op.Def.Tools) != 1 || op.Def.Tools[0] != "Read" {
		t.Fatalf("legacy content changed: %+v", op.Def)
	}
	// Already normalized Codex-only definitions must survive planApply intact.
	if !agenttmpl.SameContent(defs[0], op.Def) {
		t.Fatalf("plan changed normalized content: %+v", op.Def)
	}
}
