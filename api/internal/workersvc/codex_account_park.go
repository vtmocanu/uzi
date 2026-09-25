package workersvc

import (
	"context"
	"fmt"
	"sync"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// codexAccountPageCap bounds how many runs one tick of either Codex account pass EXAMINES (PRD
// #1590): park_codex_account_unavailable's queued Codex subscription runs (M2, D2) and
// promote_codex_account_available's held runs (M3, D3). The page is taken before any
// alias/account predicate runs, so the cost of a tick does not depend on how many of the
// examined runs turn out to be gated or promotable.
const codexAccountPageCap = 100

// codexAccountParkStore is the narrow store view the pass needs. *store.Queries satisfies it;
// a fake Store that does not is reported as errCodexStoreUnavailable, like the survivor pass.
type codexAccountParkStore interface {
	ParkQueuedCodexAccountUnavailablePage(ctx context.Context, arg store.ParkQueuedCodexAccountUnavailablePageParams) (store.ParkQueuedCodexAccountUnavailablePageRow, error)
}

// codexAccountPageCursor is one Codex account pass's in-memory keyset cursor. after is the last
// run id the previous tick examined (uuid.Nil = the start of the id space). capOverride
// replaces codexAccountPageCap when positive (tests only). The mutex serialises overlapping
// calls, so two passes never advance the cursor from the same starting point.
type codexAccountPageCursor struct {
	mu          sync.Mutex
	after       uuid.UUID
	capOverride int32
}

func (c *codexAccountPageCursor) pageCap() int32 {
	if c.capOverride > 0 {
		return c.capOverride
	}
	return codexAccountPageCap
}

// advance moves the cursor past a page of pageSize examined rows ending at last: a page
// shorter than the cap reached the end of the id space, so the next tick starts again from the
// beginning.
func (c *codexAccountPageCursor) advance(pageSize int64, last uuid.UUID) {
	if pageSize < int64(c.pageCap()) {
		c.after = uuid.Nil
	} else {
		c.after = last
	}
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
	s.codexPark.advance(row.PageSize, row.LastScannedID)
	for _, id := range row.ParkedIds {
		s.publishSwept(id, "recovery_wait")
	}
	return int64(len(row.ParkedIds)), nil
}
