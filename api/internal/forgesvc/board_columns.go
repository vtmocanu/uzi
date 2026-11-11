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
	InsertDefaultBoardColumns(ctx context.Context, arg store.InsertDefaultBoardColumnsParams) error
}

// LabelEnsurer is the slice of forge.IssueTracker EnsureBoardColumns needs.
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
// column (custom columns included) is left untouched. The rows go in with one
// statement (InsertDefaultBoardColumns), so a failure leaves no columns and a
// retry starts over rather than reading a partial board as complete. It is
// idempotent and does no issue import.
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
	names := make([]string, len(DefaultColumns))
	for i, col := range DefaultColumns {
		names[i] = col.Name
	}
	if err := q.InsertDefaultBoardColumns(ctx, store.InsertDefaultBoardColumnsParams{RepoID: repoID, LabelNames: names}); err != nil {
		return fmt.Errorf("insert default board columns: %w", err)
	}
	return nil
}
