package workersvc

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

var errProviderNameStoreUnavailable = errors.New("provider name query surface unavailable")

// runProviderNameStore is the provider-name lookup's one batched read. *store.Queries satisfies it.
type runProviderNameStore interface {
	ListRunProviderNames(ctx context.Context, runIds []uuid.UUID) ([]store.ListRunProviderNamesRow, error)
}

// ProviderNamesForRuns returns the current name of the model provider each run was bound to
// (PRD #4320), keyed by run id, from ONE batched read. The name is matched under the run's own
// owner under visibility (a provider the owner holds or an admin-shared one), so it never crosses
// users. A run with no binding, or whose provider has since been deleted or is not visible to its
// owner, is absent from the map (null on the wire). Callers pass only runs that carry a
// binding, so a page of vendor runs costs no query. The name is user- or admin-authored text:
// every renderer must treat it as plain text.
func (s *Service) ProviderNamesForRuns(ctx context.Context, runIDs []uuid.UUID) (map[uuid.UUID]string, error) {
	if len(runIDs) == 0 {
		return map[uuid.UUID]string{}, nil
	}
	q, ok := s.q.(runProviderNameStore)
	if !ok {
		return nil, errProviderNameStoreUnavailable
	}
	rows, err := q.ListRunProviderNames(ctx, runIDs)
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]string, len(rows))
	for _, row := range rows {
		out[row.RunID] = row.Name
	}
	return out, nil
}
