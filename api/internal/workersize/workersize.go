// Package workersize is the server-side registry of hosted-worker sizes (PRD #58
// Decision 7, reversed for sizes by PRD #4213). The rows live in the worker_sizes table:
// the four shipped presets are seeded at boot (store.ReconcileBuiltinWorkerSizes) and
// admins may add, edit and delete others. This package is the reader: it answers "which
// sizes exist" from the database on every call.
//
// Uncached by design: the table holds at most 16 rows and a read happens per provision
// request or poll, so a cache would only add a staleness window to a membership check.
//
// Import constraint: this package may import store and the builtin leaf only. It must
// never import config or settings, which depend on it (config -> settings -> workersize
// would cycle); the pre-database checks they need live in the builtin leaf.
package workersize

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersize/builtin"
)

// Names is the shipped (builtin) size names in display order, smallest first. Lowercase
// strings go on the wire verbatim as DesiredWorker.Size and are stored as
// workers.hosted_size. It lists builtins only; admin-added sizes come from List.
var Names = builtin.Names()

// Queries is the narrow slice of the generated store queries this reader needs.
type Queries interface {
	ListWorkerSizes(ctx context.Context) ([]store.WorkerSize, error)
	GetWorkerSize(ctx context.Context, name string) (store.WorkerSize, error)
}

// List returns every size in display order.
func List(ctx context.Context, q Queries) ([]store.WorkerSize, error) {
	return q.ListWorkerSizes(ctx)
}

// Get returns the named size, or (zero, false, nil) when no such size exists.
func Get(ctx context.Context, q Queries, name string) (store.WorkerSize, bool, error) {
	row, err := q.GetWorkerSize(ctx, name)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.WorkerSize{}, false, nil
	}
	if err != nil {
		return store.WorkerSize{}, false, fmt.Errorf("get worker size %q: %w", name, err)
	}
	return row, true, nil
}

// Exists reports whether a size of that name exists. A malformed name is rejected
// without a query: the table can hold no such row.
func Exists(ctx context.Context, q Queries, name string) (bool, error) {
	if !builtin.WellFormed(name) {
		return false, nil
	}
	_, ok, err := Get(ctx, q, name)
	return ok, err
}
