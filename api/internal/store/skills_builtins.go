package store

import (
	"context"
	"fmt"
	"log/slog"

	"gitlab.example.com/vtmocanu/uzi/api/internal/skilltmpl"
)

// builtinSkillReconcilerQueries is the query subset ReconcileBuiltinSkills needs.
// Narrowing to an interface (satisfied by *Queries) keeps the default-allocation
// seed path — in particular the zero-row warning Decision 9 requires — unit
// testable without a live database, exactly as builtinReconcilerQueries does for
// the template side's collision warning.
type builtinSkillReconcilerQueries interface {
	InsertBuiltinSkill(ctx context.Context, arg InsertBuiltinSkillParams) (int64, error)
	// SeedSharedSkillAllocationByName seeds ONE default allocation row, called
	// once per target template so a miss can name which template was absent.
	SeedSharedSkillAllocationByName(ctx context.Context, arg SeedSharedSkillAllocationByNameParams) (int64, error)
}

// TemplatesReconciled is proof that ReconcileBuiltinTemplates has already run.
//
// It exists because PRD #72 M2 makes the boot ordering LOAD-BEARING and Decision 9
// requires it asserted rather than assumed. A builtin skill's default allocation
// is seeded BY TEMPLATE NAME, and only on the boot that first inserts the skill.
// Run the skills reconciler first and the agent_templates rows do not exist yet:
// every seed inserts nothing, warns, and — because the skill row now exists, so
// the next boot's insert is a no-op — never retries. The builtin ships permanently
// unallocated, which is the precise failure this milestone was written to remove.
//
// Of the three ways to hold that ordering (a test that reads main.go's source, a
// comment plus a runtime guard, a structural dependency) this is the structural
// one, chosen because it is the only one that cannot regress silently: the
// ordering becomes a COMPILE-TIME dependency at the call site in
// api/cmd/server/main.go, self-documenting and immune to the helper-extraction
// refactor that would defeat a source-scanning test. The unexported field means
// no package outside store can forge a valid token, and ReconcileBuiltinSkills
// rejects the zero value, so the one remaining hole (passing TemplatesReconciled{})
// is closed at runtime too.
type TemplatesReconciled struct{ done bool }

// ReconcileBuiltinSkills seeds the Go-embedded builtin skills into the database.
// It is idempotent and edit-preserving: a missing builtin is inserted with
// scope='builtin', an existing row (builtin or admin-edited) is never
// overwritten. It runs at startup AFTER ReconcileBuiltinTemplates, which the
// TemplatesReconciled argument enforces rather than documents.
//
// Deliberate divergence from agent templates: builtin-ness is the `scope` value,
// not an `is_builtin` flag, and the reconciler keys on (name, scope='builtin')
// via uq_skills_shared_name. A builtin and a global therefore can never share a
// name — intended, not a bug to "fix".
func ReconcileBuiltinSkills(ctx context.Context, q builtinSkillReconcilerQueries, templates TemplatesReconciled) error {
	if !templates.done {
		return fmt.Errorf("reconcile builtin skills: agent templates have not been reconciled yet " +
			"(default skill allocations resolve agent templates by name and would silently seed nothing)")
	}
	var inserted, allocated int
	for _, def := range skilltmpl.Builtins() {
		n, err := q.InsertBuiltinSkill(ctx, InsertBuiltinSkillParams{
			Name:        def.Name,
			Description: def.Description,
			Body:        def.Body,
		})
		if err != nil {
			return fmt.Errorf("insert builtin skill %q: %w", def.Name, err)
		}
		if n > 0 {
			// A newly-inserted builtin gets its default allocations (PRD #72 M2).
			// Seeded HERE, not on every boot, so a default an admin later removes
			// stays removed — the same gate ReconcileBuiltinTemplates applies, and
			// the reason `ci-cd-norms` needs migration 00083 instead: its row
			// already exists on every live instance, so n is 0 there forever.
			for _, tmpl := range skilltmpl.DefaultAllocationsFor(def.Name) {
				rows, err := q.SeedSharedSkillAllocationByName(ctx, SeedSharedSkillAllocationByNameParams{
					SkillName:    def.Name,
					TemplateName: tmpl,
				})
				if err != nil {
					return fmt.Errorf("seed default allocation of skill %q to template %q: %w", def.Name, tmpl, err)
				}
				if rows == 0 {
					// The seed targets a row it did not insert, so unlike the
					// template analogue it CAN miss without erroring. No allocation
					// for this skill can pre-exist (its row was just created, and
					// skill_id is an FK), so 0 is never "already there" — the named
					// agent template is absent, and this builtin is now shipping
					// without the default it declares.
					slog.Warn("builtin skill default allocation seeded no row; the agent template is missing",
						"skill", def.Name, "template", tmpl)
					continue
				}
				allocated += int(rows)
			}
		}
		inserted += int(n)
	}
	slog.Info("reconciled builtin skills",
		"builtins", len(skilltmpl.Builtins()), "inserted", inserted, "allocations_seeded", allocated)
	return nil
}
