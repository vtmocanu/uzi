package agentsource

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/agenttmpl"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// planApply is the pure classifier the transactional executor runs; these tests pin
// each branch (the four apply cases + de-provisioning) without a database.

func tmpl(name, scope, origin, desc, body string) store.AgentTemplate {
	row := store.AgentTemplate{ID: uuid.New(), Name: name, Scope: scope, Description: desc, PromptBody: body}
	if origin != "" {
		row.Origin = pgtype.Text{String: origin, Valid: true}
	}
	if scope == "builtin" {
		row.IsBuiltin = true
	}
	return row
}

func synDef(name, desc, body string) agenttmpl.Definition {
	return agenttmpl.Definition{Name: name, Description: desc, PromptBody: body}
}

func opFor(ops []plannedOp, name string) (plannedOp, bool) {
	for _, o := range ops {
		if o.Name == name {
			return o, true
		}
	}
	return plannedOp{}, false
}

func TestPlanApplyClassification(t *testing.T) {
	current := []store.AgentTemplate{
		tmpl("coder", "builtin", "embedded", "coder desc", "old\n"),              // override
		tmpl("admin-builtin", "builtin", "admin", "ab desc", "ab body\n"),        // override (admin builtin still overridden)
		tmpl("changed-global", "global", "synced", "cg desc", "cg old\n"),        // update-synced-global
		tmpl("kept-global", "global", "synced", "kg desc", "kg body\n"),          // unchanged
		tmpl("kept-builtin", "builtin", "synced", "kb desc", "kb body\n"),        // unchanged
		tmpl("admin-global", "global", "", "adm desc", "adm body\n"),             // conflict
		tmpl("gone-global", "global", "synced", "gg desc", "gg body\n"),          // deprovision (absent)
		tmpl("gone-builtin", "builtin", "synced", "gb desc", "gb body\n"),        // reset (absent)
		tmpl("admin-gone-builtin", "builtin", "admin", "agb desc", "agb body\n"), // NOT de-provisioned (origin admin)
		tmpl("plain-gone-global", "global", "", "pgg desc", "pgg body\n"),        // NOT de-provisioned (origin NULL)
		tmpl("user-same-name", "user", "", "u desc", "u body\n"),                 // ignored (user scope)
	}
	defs := []agenttmpl.Definition{
		synDef("coder", "coder desc", "new\n"),
		synDef("admin-builtin", "ab desc2", "ab body2\n"),
		synDef("brand-new", "bn desc", "bn body\n"), // add
		synDef("changed-global", "cg desc", "cg new\n"),
		synDef("kept-global", "kg desc", "kg body\n"),  // identical → unchanged
		synDef("kept-builtin", "kb desc", "kb body\n"), // identical → unchanged
		synDef("admin-global", "x desc", "x body\n"),   // collides with admin global → conflict
	}

	ops := planApply(defs, current)

	want := map[string]ApplyAction{
		"coder":          ActionOverrideBuiltin,
		"admin-builtin":  ActionOverrideBuiltin,
		"brand-new":      ActionAddGlobal,
		"changed-global": ActionUpdateGlobal,
		"kept-global":    ActionUnchanged,
		"kept-builtin":   ActionUnchanged,
		"admin-global":   ActionConflict,
		"gone-global":    ActionDeprovisionGlobal,
		"gone-builtin":   ActionResetBuiltin,
	}
	for name, action := range want {
		op, ok := opFor(ops, name)
		if !ok {
			t.Errorf("no planned op for %q (want %s)", name, action)
			continue
		}
		if op.Action != action {
			t.Errorf("op %q action = %s, want %s", name, op.Action, action)
		}
	}

	// Rows that must NOT be de-provisioned (origin admin / NULL) and the user-scope row
	// produce no op at all.
	for _, name := range []string{"admin-gone-builtin", "plain-gone-global", "user-same-name"} {
		if op, ok := opFor(ops, name); ok {
			t.Errorf("unexpected op for %q: %s (must never be touched)", name, op.Action)
		}
	}

	// The add op carries the synced def so the executor can insert it.
	if op, _ := opFor(ops, "brand-new"); op.Def.PromptBody != "bn body\n" {
		t.Errorf("add op lost its def body: %q", op.Def.PromptBody)
	}
	// The override op carries the target row so the executor has its id.
	if op, _ := opFor(ops, "coder"); op.Row.Name != "coder" {
		t.Errorf("override op lost its target row: %+v", op.Row)
	}
}

// decodeStagedRoles must split OK roles into defs and !OK roles into parse-skip
// outcomes, dropping the failed ones from the apply candidates.
func TestDecodeStagedRoles(t *testing.T) {
	raw := []byte(`[
		{"name":"good","ok":true,"description":"d","prompt_body":"b\n","tools":["Read"],"model":"opus"},
		{"name":"bad","ok":false,"reason":"invalid"}
	]`)
	defs, skipped, err := decodeStagedRoles(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(defs) != 1 || defs[0].Name != "good" || defs[0].Model != "opus" || len(defs[0].Tools) != 1 {
		t.Fatalf("defs = %+v", defs)
	}
	if len(skipped) != 1 || skipped[0].Name != "bad" || skipped[0].Action != ActionSkippedParse || skipped[0].Detail != "invalid" {
		t.Fatalf("skipped = %+v", skipped)
	}
}

// An UNDECODABLE roles blob must return an error (NOT decode to empty defs). An empty
// def set would classify every origin='synced' row as a removal, so a corrupt snapshot
// would de-provision every synced template — the destructive misread this guards.
func TestDecodeStagedRolesInvalidJSONErrors(t *testing.T) {
	// Both a syntactically malformed blob and a well-formed-but-wrong-shape one (the
	// jsonb-column reality: Postgres accepts only valid JSON, so a corrupt snapshot
	// surfaces as an object where an array is expected) must error, never decode to empty.
	for _, raw := range [][]byte{[]byte(`{not valid json`), []byte(`{"not":"an array"}`)} {
		defs, skipped, err := decodeStagedRoles(raw)
		if err == nil {
			t.Fatalf("want an error on %q, got defs=%+v skipped=%+v", raw, defs, skipped)
		}
		if defs != nil || skipped != nil {
			t.Fatalf("on error, defs/skipped must be nil: defs=%+v skipped=%+v", defs, skipped)
		}
	}
}
