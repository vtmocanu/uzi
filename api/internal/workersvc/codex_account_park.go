package workersvc

import (
	"context"
	"fmt"
	"sync"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// codexAccountParkPageCap bounds how many queued Codex subscription runs one
// park_codex_account_unavailable tick EXAMINES (PRD #1590 M2, D2). The page is taken before
// the alias/account predicate runs, so the cost of a tick does not depend on how many of the
// examined runs turn out to be gated.
const codexAccountParkPageCap = 100

// codexAccountParkStore is the narrow store view the pass needs. *store.Queries satisfies it;
// a fake Store that does not is reported as errCodexStoreUnavailable, like the survivor pass.
type codexAccountParkStore interface {
	ParkQueuedCodexAccountUnavailablePage(ctx context.Context, arg store.ParkQueuedCodexAccountUnavailablePageParams) (store.ParkQueuedCodexAccountUnavailablePageRow, error)
}

// codexAccountParkCursor is the pass's in-memory keyset cursor. after is the last run id the
// previous tick examined (uuid.Nil = the start of the id space). capOverride replaces
// codexAccountParkPageCap when positive (tests only). The mutex serialises overlapping calls,
// so two passes never advance the cursor from the same starting point.
type codexAccountParkCursor struct {
	mu          sync.Mutex
	after       uuid.UUID
	capOverride int32
}

func (c *codexAccountParkCursor) pageCap() int32 {
	if c.capOverride > 0 {
		return c.capOverride
	}
	return codexAccountParkPageCap
}

// parkCodexAccountUnavailable runs one page of the park_codex_account_unavailable pass: it
// moves the gated queued Codex subscription runs among the next page of queued Codex
// subscription runs to recovery_wait (cause codex_account_unavailable) and publishes each
// transition. The cursor advances to the page's last examined id; a page shorter than the cap
// reached the end of the queue, so the next tick starts again from the beginning. On a store
// error the cursor is left in place and the error returned; Sweep logs it and continues.
func (s *Service) parkCodexAccountUnavailable(ctx context.Context) (int64, error) {
	q, ok := s.q.(codexAccountParkStore)
	if !ok {
		return 0, errCodexStoreUnavailable
	}
	s.codexPark.mu.Lock()
	defer s.codexPark.mu.Unlock()
	pageCap := s.codexPark.pageCap()
	row, err := q.ParkQueuedCodexAccountUnavailablePage(ctx, store.ParkQueuedCodexAccountUnavailablePageParams{
		AfterID: s.codexPark.after,
		PageCap: pageCap,
	})
	if err != nil {
		return 0, fmt.Errorf("park codex account unavailable: %w", err)
	}
	if row.PageSize < int64(pageCap) {
		s.codexPark.after = uuid.Nil
	} else {
		s.codexPark.after = row.LastScannedID
	}
	for _, id := range row.ParkedIds {
		s.publishSwept(id, "recovery_wait")
	}
	return int64(len(row.ParkedIds)), nil
}
