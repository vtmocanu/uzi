package forgesvc

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// BoardColumnStore is the slice of *store.Queries EnsureBoardColumns needs.
type BoardColumnStore interface {
	CountBoardColumns(ctx context.Context, repoID uuid.UUID) (int64, error)
	InsertBoardColumn(ctx context.Context, arg store.InsertBoardColumnParams) error
}

// LabelEnsurer is the slice of forge.Forge EnsureBoardColumns needs.
type LabelEnsurer interface {
	EnsureLabels(ctx context.Context, projectID int64, labels []forge.Label) error
}

// LabelSeedError reports that the forge refused to create the default column
// labels. Callers map it to a 502; any other error from EnsureBoardColumns is
// a database failure. Err is already PAT-redacted by the forge driver.
type LabelSeedError struct{ Err error }

func (e *LabelSeedError) Error() string { return e.Err.Error() }
func (e *LabelSeedError) Unwrap() error { return e.Err }

// EnsureBoardColumns gives a repo its board columns when it has none: it
// ensures the DefaultColumns labels exist on the forge, then records them as
// board_columns rows in DefaultColumns order. A repo that already has any
// column (custom columns included) is left untouched. It is idempotent
// (InsertBoardColumn is ON CONFLICT DO NOTHING) and does no issue import:
// the poller imports issues after a repo is enabled.
//
// Without these rows board.ResolveColumn returns "" for every card, so the
// run lifecycle reads an automated move as a person's and clears it.
func EnsureBoardColumns(ctx context.Context, q BoardColumnStore, f LabelEnsurer, repoID uuid.UUID, projectID int64) error {
	count, err := q.CountBoardColumns(ctx, repoID)
	if err != nil {
		return fmt.Errorf("count board columns: %w", err)
	}
	if count > 0 {
		return nil
	}
	if err := f.EnsureLabels(ctx, projectID, DefaultColumns); err != nil {
		return &LabelSeedError{Err: err}
	}
	for i, col := range DefaultColumns {
		if err := q.InsertBoardColumn(ctx, store.InsertBoardColumnParams{
			RepoID:    repoID,
			LabelName: col.Name,
			Position:  int32(i), //nolint:gosec // G115: i indexes the four-entry DefaultColumns
		}); err != nil {
			return fmt.Errorf("insert board column %q: %w", col.Name, err)
		}
	}
	return nil
}
