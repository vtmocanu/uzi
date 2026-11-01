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
	return effortResolutionPtr(agenttmpl.ResolveHarnessEffort(string(harness), textPtr(value)))
}

// effortResolutionPtr is the one mapping from a resolution to the claim's default_effort: nil
// (key omitted) for the runtime default, else the resolved level. resolveHarnessEffortPtr and
// resolveLeadEffort both go through it.
func effortResolutionPtr(r agenttmpl.EffortResolution) *string {
	if r.Mode == agenttmpl.EffortModeRuntimeDefault {
		return nil
	}
	level := r.Level
	return &level
}
