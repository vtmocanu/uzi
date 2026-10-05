package workersvc

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// PromoteVaultLockedRecoveryWaitRuns synchronously queues the owner's vault-locked
// recovery parks after an explicit successful unlock. The timer remains the fallback
// for parks that arrive after this query.
func (s *Service) PromoteVaultLockedRecoveryWaitRuns(ctx context.Context, userID uuid.UUID) error {
	if s.vlt == nil || !s.vlt.Unlocked(userID) {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Lock can race between Unlocked and the UPDATE, queuing a locked run just as
	// the timer does. Service.Claim returns idle until the owner unlocks again.
	queryCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	rows, err := s.q.PromoteVaultLockedRecoveryWaitRuns(queryCtx, userID)
	if err != nil {
		return err
	}
	for _, row := range rows {
		s.publishSwept(row.ID, row.Status)
	}
	return nil
}
