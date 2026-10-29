package workersvc

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/agenttmpl"
)

// Read only the preference matching the run's frozen harness. Callers retain
// their existing strict (ordinary) or best-effort (judge) lookup error posture.
func (s *Service) readUserDefaultEffort(ctx context.Context, userID uuid.UUID, harness Harness) (pgtype.Text, error) {
	if harness == HarnessCodex {
		return s.q.GetUserDefaultCodexEffort(ctx, userID)
	}
	return s.q.GetUserDefaultEffort(ctx, userID)
}

// resolveHarnessEffortPtr resolves the ordinary per-harness effort preference to
// the claim's default_effort: nil (key omitted) exactly when the owner chose
// Runtime default, else the resolved level.
func resolveHarnessEffortPtr(harness Harness, value pgtype.Text) *string {
	r := agenttmpl.ResolveHarnessEffort(string(harness), textPtr(value))
	if r.Mode == agenttmpl.EffortModeRuntimeDefault {
		return nil
	}
	return &r.Level
}
