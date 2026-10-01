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

func resolveHarnessEffortPtr(harness Harness, value pgtype.Text) *string {
	if harness == HarnessCodex {
		effort := agenttmpl.ResolveDefaultCodexEffort(textPtr(value))
		return &effort
	}
	return resolveEffortPtr(value)
}
