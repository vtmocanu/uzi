package agentsource

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #3258 N8: a source role's `effort:` stages as its Claude-family pin, previews as a
// change against a row without it, plans into the apply definition, and re-previews
// unchanged once the row carries it. A stored Codex effort is cleared by the sync, like
// codex_model, because the source carries no Codex pin.
func TestReconcileEffortPreviewAndApplyPlan(t *testing.T) {
	for _, scope := range []string{"global", "builtin"} {
		t.Run(scope, func(t *testing.T) {
			st := newFakeStore()
			r := newRec(st, &fakeSettings{enabled: true, url: "https://ok.test/a.git"}, true)
			f := SourceFile{Name: "coder.md", Data: []byte("---\nname: coder\ndescription: source coder\neffort: high\ntools: Read\n---\nsource body\n")}
			r.fetch = stubFetch("first", []SourceFile{f}, nil)
			if res, err := r.Reconcile(context.Background()); err != nil || res.Changed != 1 || len(st.upserts) != 1 {
				t.Fatalf("initial reconcile: %+v, %v", res, err)
			}
			var staged []StagedRole
			if err := json.Unmarshal(st.upserts[0].Roles, &staged); err != nil {
				t.Fatal(err)
			}
			if len(staged) != 1 || staged[0].Effort != "high" || len(staged[0].Notes) != 0 {
				t.Fatalf("staged role: %+v", staged)
			}
			defs, skipped, err := decodeStagedRoles(st.upserts[0].Roles)
			if err != nil || len(skipped) != 0 || len(defs) != 1 || defs[0].Effort != "high" || defs[0].CodexEffort != "" {
				t.Fatalf("decode: %+v, %+v, %v", defs, skipped, err)
			}

			row := tmpl("coder", scope, "synced", "source coder", "source body\n")
			row.CodexEffort = pgtype.Text{String: "low", Valid: true}
			st.templates = []store.AgentTemplate{row}
			r.fetch = stubFetch("changed", []SourceFile{f}, nil)
			if res, err := r.Reconcile(context.Background()); err != nil || res.Changed != 1 {
				t.Fatalf("changed preview: %+v, %v", res, err)
			}
			op, ok := opFor(planApply(defs, st.templates), "coder")
			if !ok || op.Action == ActionUnchanged || op.Def.Effort != "high" || op.Def.CodexEffort != "" {
				t.Fatalf("plan: %+v", op)
			}

			row.Effort = pgtype.Text{String: op.Def.Effort, Valid: op.Def.Effort != ""}
			row.CodexEffort = pgtype.Text{String: op.Def.CodexEffort, Valid: op.Def.CodexEffort != ""}
			row.Tools = mustJSON(t, op.Def.Tools)
			st.templates = []store.AgentTemplate{row}
			r.fetch = stubFetch("second", []SourceFile{f}, nil)
			if res, err := r.Reconcile(context.Background()); err != nil || res.Changed != 0 {
				t.Fatalf("second preview must be unchanged: %+v, %v", res, err)
			}
			if op, _ := opFor(planApply(defs, st.templates), "coder"); op.Action != ActionUnchanged {
				t.Fatalf("second plan: %+v", op)
			}
		})
	}
}

// An invalid `effort:` keeps the role, stages no pin and carries the effort_ignored clamp
// note (never the value) that the admin card shows.
func TestReconcileInvalidEffortStagesNoPin(t *testing.T) {
	st := newFakeStore()
	r := newRec(st, &fakeSettings{enabled: true, url: "https://ok.test/a.git"}, true)
	f := SourceFile{Name: "coder.md", Data: []byte("---\nname: coder\ndescription: source coder\neffort: SECRET\n---\nsource body\n")}
	r.fetch = stubFetch("first", []SourceFile{f}, nil)
	if res, err := r.Reconcile(context.Background()); err != nil || res.Failed != 0 {
		t.Fatalf("reconcile: %+v, %v", res, err)
	}
	var staged []StagedRole
	if err := json.Unmarshal(st.upserts[0].Roles, &staged); err != nil {
		t.Fatal(err)
	}
	if len(staged) != 1 || !staged[0].OK || staged[0].Effort != "" || len(staged[0].Notes) != 1 || staged[0].Notes[0] != string(NoteEffortIgnored) {
		t.Fatalf("staged role: %+v", staged)
	}
	if bytes.Contains(st.upserts[0].Roles, []byte("SECRET")) {
		t.Fatalf("the invalid value reached the snapshot: %s", st.upserts[0].Roles)
	}
}

// A snapshot staged before effort existed decodes with no pin.
func TestLegacyStagedEffort(t *testing.T) {
	raw := []byte(`[{"name":"coder","ok":true,"description":"d","tools":["Read"],"prompt_body":"body\n"}]`)
	defs, _, err := decodeStagedRoles(raw)
	if err != nil || len(defs) != 1 || defs[0].Effort != "" || defs[0].CodexEffort != "" {
		t.Fatalf("legacy decode: %+v, %v", defs, err)
	}
	if op, ok := opFor(planApply(defs, nil), "coder"); !ok || op.Def.Effort != "" {
		t.Fatalf("legacy plan: %+v", op)
	}
}
