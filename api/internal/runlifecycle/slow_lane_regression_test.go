package runlifecycle

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/board"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// This file is self-contained on purpose (its own store, mover and forge; it uses
// only New and Reconcile): it must compile and run against the commit before the
// slow lane existed, where it fails because the 30-minute window never lists a
// marker that is 3.5 hours old.

const slowRegAge = 210 * time.Minute // 3.5 hours

type slowRegStore struct {
	runID   uuid.UUID
	repoID  uuid.UUID
	pending time.Time
	marked  bool
}

func (s *slowRegStore) GetRunMoveContext(context.Context, uuid.UUID) (store.GetRunMoveContextRow, error) {
	row := store.GetRunMoveContextRow{
		Status:          "failed",
		IssueIid:        pgtype.Int8{Int64: 4, Valid: true},
		RepoID:          pgtype.UUID{Bytes: s.repoID, Valid: true},
		OriginColumn:    pgtype.Text{String: "Upcoming", Valid: true},
		BoardColumn:     pgtype.Text{String: board.ColumnInProgress, Valid: true},
		ForgeProjectID:  77,
		ForgeType:       "gitlab",
		BaseUrl:         "https://gitlab.example.com",
		TokenCiphertext: []byte("sealed"),
	}
	if s.marked {
		row.MovePendingSince = pgtype.Timestamptz{Time: s.pending, Valid: true}
	}
	return row, nil
}

func (s *slowRegStore) GetIssueByIID(_ context.Context, arg store.GetIssueByIIDParams) (store.Issue, error) {
	labels, _ := json.Marshal([]string{board.ColumnInProgress})
	return store.Issue{RepoID: arg.RepoID, ForgeIssueIid: arg.ForgeIssueIid, State: "opened", Labels: labels}, nil
}

func (s *slowRegStore) ListBoardColumns(context.Context, uuid.UUID) ([]store.BoardColumn, error) {
	names := []string{board.ColumnInProgress, "Upcoming", board.ColumnHumanReview}
	cols := make([]store.BoardColumn, len(names))
	for i, n := range names {
		cols[i] = store.BoardColumn{LabelName: n, Position: int32(i)}
	}
	return cols, nil
}

func (s *slowRegStore) RecordRunColumnMove(context.Context, store.RecordRunColumnMoveParams) (int64, error) {
	s.marked = false
	return 1, nil
}

func (s *slowRegStore) ClearRunMovePending(context.Context, uuid.UUID) (int64, error) {
	s.marked = false
	return 1, nil
}

// ListPendingColumnMoves has the SQL semantics: grace_cutoff >= pending > giveup_cutoff.
func (s *slowRegStore) ListPendingColumnMoves(_ context.Context, arg store.ListPendingColumnMovesParams) ([]uuid.UUID, error) {
	if s.marked && !s.pending.After(arg.GraceCutoff.Time) && s.pending.After(arg.GiveupCutoff.Time) {
		return []uuid.UUID{s.runID}, nil
	}
	return nil, nil
}

func (s *slowRegStore) ListGaveUpColumnMoves(context.Context, store.ListGaveUpColumnMovesParams) ([]store.ListGaveUpColumnMovesRow, error) {
	return nil, nil
}

func (s *slowRegStore) ClaimAutopilotTerminalComment(context.Context, uuid.UUID) (int64, error) {
	return 0, nil
}

type slowRegForge struct{ forge.Forge }

type slowRegMover struct{ moves []string }

func (m *slowRegMover) ForgeForConnection(string, string, []byte) (forge.Forge, error) {
	return &slowRegForge{}, nil
}

func (m *slowRegMover) AutoMove(_ context.Context, _ forge.IssueTracker, _ int64, issue store.Issue, _ []store.BoardColumn, target string) (store.Issue, error) {
	m.moves = append(m.moves, target)
	return issue, nil
}

func TestReconcileRetriesAMovePendingThreeAndAHalfHours(t *testing.T) {
	s := &slowRegStore{
		runID:   uuid.New(),
		repoID:  uuid.New(),
		pending: time.Now().Add(-slowRegAge),
		marked:  true,
	}
	m := &slowRegMover{}
	l := New(s, m, "")

	l.Reconcile(context.Background())

	if len(m.moves) == 0 {
		t.Fatalf("move not attempted: a 3.5h-pending marker must be retried by the slow lane")
	}
	if len(m.moves) != 1 || m.moves[0] != "Upcoming" {
		t.Fatalf("moves = %v, want one move to Upcoming", m.moves)
	}
	if s.marked {
		t.Fatal("the marker must be cleared after the successful move")
	}
}
