package store

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/agenttmpl"
)

// ReconcileBuiltinTemplates seeds the Go-embedded builtin agent templates into
// the database. It is idempotent and edit-preserving: a missing builtin is
// inserted with is_builtin=true, an existing row (builtin or admin-edited) is
// never overwritten. It runs at startup alongside Migrate so future releases
// can add or improve builtins without a re-run-unsafe SQL seed.
func ReconcileBuiltinTemplates(ctx context.Context, q *Queries) error {
	var inserted int
	for _, def := range agenttmpl.Builtins() {
		model, tools, err := builtinColumns(def)
		if err != nil {
			return fmt.Errorf("encode builtin %q: %w", def.Name, err)
		}
		n, err := q.InsertBuiltinAgentTemplate(ctx, InsertBuiltinAgentTemplateParams{
			Name:        def.Name,
			Description: def.Description,
			Model:       model,
			Tools:       tools,
			PromptBody:  def.PromptBody,
		})
		if err != nil {
			return fmt.Errorf("insert builtin %q: %w", def.Name, err)
		}
		if n == 0 {
			// ON CONFLICT (name) DO NOTHING: an existing row of the same name
			// kept the seed out. That is normal when a prior boot already
			// inserted this builtin, but on an upgrade a pre-existing admin
			// template (is_builtin=false) can shadow a new builtin and never
			// receive it — the worker still routes it by name, but it is not
			// resettable to the shipped definition. Warn so an operator can
			// rename or delete the custom row if they want the builtin.
			if existing, gErr := q.GetAgentTemplateByName(ctx, def.Name); gErr == nil && !existing.IsBuiltin {
				slog.Warn("builtin agent template shadowed by a custom row; skipping seed",
					"name", def.Name)
			}
		}
		inserted += int(n)
	}
	slog.Info("reconciled builtin agent templates", "builtins", len(agenttmpl.Builtins()), "inserted", inserted)
	return nil
}

// builtinColumns converts a definition's model/tools into their DB column types:
// an empty model is a NULL model (inherit), an empty tools list is a NULL tools
// column (inherit all), and a non-empty tools list is a JSON array.
func builtinColumns(def agenttmpl.Definition) (pgtype.Text, []byte, error) {
	var model pgtype.Text
	if def.Model != "" {
		model = pgtype.Text{String: def.Model, Valid: true}
	}
	var tools []byte
	if len(def.Tools) > 0 {
		b, err := json.Marshal(def.Tools)
		if err != nil {
			return pgtype.Text{}, nil, err
		}
		tools = b
	}
	return model, tools, nil
}
