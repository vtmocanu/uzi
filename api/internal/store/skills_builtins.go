package store

import (
	"context"
	"fmt"
	"log/slog"

	"gitlab.example.com/vtmocanu/uzi/api/internal/skilltmpl"
)

// ReconcileBuiltinSkills seeds the Go-embedded builtin skills into the database.
// It is idempotent and edit-preserving: a missing builtin is inserted with
// scope='builtin', an existing row (builtin or admin-edited) is never
// overwritten. It runs at startup alongside ReconcileBuiltinTemplates.
//
// Deliberate divergence from agent templates: builtin-ness is the `scope` value,
// not an `is_builtin` flag, and the reconciler keys on (name, scope='builtin')
// via uq_skills_shared_name. A builtin and a global therefore can never share a
// name — intended, not a bug to "fix".
func ReconcileBuiltinSkills(ctx context.Context, q *Queries) error {
	var inserted int
	for _, def := range skilltmpl.Builtins() {
		n, err := q.InsertBuiltinSkill(ctx, InsertBuiltinSkillParams{
			Name:        def.Name,
			Description: def.Description,
			Body:        def.Body,
		})
		if err != nil {
			return fmt.Errorf("insert builtin skill %q: %w", def.Name, err)
		}
		inserted += int(n)
	}
	slog.Info("reconciled builtin skills", "builtins", len(skilltmpl.Builtins()), "inserted", inserted)
	return nil
}
