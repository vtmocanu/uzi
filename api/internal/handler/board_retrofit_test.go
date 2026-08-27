package handler

import (
	"context"
	"reflect"
	"sort"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/board"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func col(name string, pos int32) store.BoardColumn {
	return store.BoardColumn{LabelName: name, Position: pos}
}

func TestHumanReviewPlacement(t *testing.T) {
	tests := []struct {
		name       string
		cols       []store.BoardColumn
		wantPos    int
		wantNeeded bool
	}{
		{
			name:       "old board inserts right after In Progress",
			cols:       []store.BoardColumn{col("In Progress", 0), col("Upcoming", 1), col("Later", 2)},
			wantPos:    1,
			wantNeeded: true,
		},
		{
			name:       "already present is a no-op (idempotent)",
			cols:       []store.BoardColumn{col("In Progress", 0), col("Human Review", 1), col("Upcoming", 2), col("Later", 3)},
			wantPos:    0,
			wantNeeded: false,
		},
		{
			name:       "In Progress absent appends at the end",
			cols:       []store.BoardColumn{col("Upcoming", 0), col("Later", 1)},
			wantPos:    2,
			wantNeeded: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pos, needed := humanReviewPlacement(tc.cols)
			if pos != tc.wantPos || needed != tc.wantNeeded {
				t.Errorf("humanReviewPlacement = (%d,%v), want (%d,%v)", pos, needed, tc.wantPos, tc.wantNeeded)
			}
		})
	}
}

// fakeColumnStore models board_columns faithfully to the two mutation queries the
// retrofit uses, so the end-to-end ordering can be asserted without a database.
type fakeColumnStore struct {
	cols []store.BoardColumn
}

func (f *fakeColumnStore) ListBoardColumns(context.Context, uuid.UUID) ([]store.BoardColumn, error) {
	out := append([]store.BoardColumn(nil), f.cols...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Position < out[j].Position })
	return out, nil
}

// ShiftBoardColumnsFrom mirrors `UPDATE ... SET position = position + 1 WHERE
// position >= from`.
func (f *fakeColumnStore) ShiftBoardColumnsFrom(_ context.Context, arg store.ShiftBoardColumnsFromParams) error {
	for i := range f.cols {
		if f.cols[i].Position >= arg.FromPosition {
			f.cols[i].Position++
		}
	}
	return nil
}

// InsertBoardColumn mirrors the ON CONFLICT (repo_id, label_name) DO NOTHING
// insert.
func (f *fakeColumnStore) InsertBoardColumn(_ context.Context, arg store.InsertBoardColumnParams) error {
	for _, c := range f.cols {
		if c.LabelName == arg.LabelName {
			return nil // DO NOTHING
		}
	}
	f.cols = append(f.cols, store.BoardColumn{LabelName: arg.LabelName, Position: arg.Position})
	return nil
}

func (f *fakeColumnStore) ordered() []string {
	cols, _ := f.ListBoardColumns(context.Background(), uuid.UUID{})
	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = c.LabelName
	}
	return names
}

// columnRetrofitStore is the narrow store the retrofit ordering exercises (the
// forge-label ensure is orthogonal and lives in the handler method).
type columnRetrofitStore interface {
	ListBoardColumns(context.Context, uuid.UUID) ([]store.BoardColumn, error)
	ShiftBoardColumnsFrom(context.Context, store.ShiftBoardColumnsFromParams) error
	InsertBoardColumn(context.Context, store.InsertBoardColumnParams) error
}

// applyRetrofit performs the exact DB sequence ensureHumanReviewColumn runs after
// the forge label is ensured — the same humanReviewPlacement, shift, then insert.
func applyRetrofit(ctx context.Context, q columnRetrofitStore, repoID uuid.UUID) error {
	cols, err := q.ListBoardColumns(ctx, repoID)
	if err != nil {
		return err
	}
	pos, needed := humanReviewPlacement(cols)
	if !needed {
		return nil
	}
	if err := q.ShiftBoardColumnsFrom(ctx, store.ShiftBoardColumnsFromParams{RepoID: repoID, FromPosition: int32(pos)}); err != nil {
		return err
	}
	return q.InsertBoardColumn(ctx, store.InsertBoardColumnParams{RepoID: repoID, LabelName: board.ColumnHumanReview, Position: int32(pos)})
}

func TestHumanReviewRetrofitOrderAndIdempotency(t *testing.T) {
	ctx := context.Background()
	repoID := uuid.New()
	fake := &fakeColumnStore{cols: []store.BoardColumn{col("In Progress", 0), col("Upcoming", 1), col("Later", 2)}}

	if err := applyRetrofit(ctx, fake, repoID); err != nil {
		t.Fatalf("retrofit: %v", err)
	}
	want := []string{"In Progress", "Human Review", "Upcoming", "Later"}
	if got := fake.ordered(); !reflect.DeepEqual(got, want) {
		t.Fatalf("retrofit order = %v, want %v", got, want)
	}

	// Idempotent: a second load must detect Human Review and leave the board — no
	// double shift, no duplicate row.
	if err := applyRetrofit(ctx, fake, repoID); err != nil {
		t.Fatalf("retrofit re-run: %v", err)
	}
	if got := fake.ordered(); !reflect.DeepEqual(got, want) {
		t.Fatalf("re-run changed the board: got %v, want %v", got, want)
	}
}
