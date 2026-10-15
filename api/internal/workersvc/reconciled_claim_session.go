package workersvc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// reconciledClaimSession leaves the durable session in runs for custody/history,
// but never offers the latest reconciliation's predecessor transcript to a lead.
// Recovery claims remain fresh until a different session is durably recorded.
func (s *Service) reconciledClaimSession(ctx context.Context, run store.Run) (*string, error) {
	session := textPtr(run.SessionID)
	if session == nil || run.GateRevision == 0 {
		return session, nil
	}
	reader, ok := s.q.(interface {
		LatestRunMilestoneReconciliationSession(context.Context, uuid.UUID) ([][]byte, error)
	})
	if !ok {
		return nil, errors.New("reconciliation session reader unavailable")
	}
	rows, err := reader.LatestRunMilestoneReconciliationSession(ctx, run.ID)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return session, nil
	}
	if len(rows) != 1 || len(bytes.TrimSpace(rows[0])) == 0 {
		return nil, errors.New("invalid reconciliation predecessor session")
	}
	var predecessor *string
	if err := json.Unmarshal(rows[0], &predecessor); err != nil {
		return nil, err
	}
	if predecessor != nil && *predecessor == *session {
		return nil, nil
	}
	return session, nil
}
