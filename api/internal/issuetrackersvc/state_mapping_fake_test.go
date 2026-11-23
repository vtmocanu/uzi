package issuetrackersvc

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

func columnStatesJSON(m map[string]string) []byte {
	if m == nil {
		m = map[string]string{}
	}
	b, _ := json.Marshal(m)
	return b
}

// setBoardColumns replaces the fake repo's board columns.
func (f *fakeQuerier) setBoardColumns(repo uuid.UUID, labels ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.repos[repo].boardColumns = labels
}

func (f *fakeQuerier) LockRepoForStateMappingForUser(_ context.Context, a store.LockRepoForStateMappingForUserParams) (store.LockRepoForStateMappingForUserRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.ownedRepo(a.ID, a.UserID)
	if err != nil {
		return store.LockRepoForStateMappingForUserRow{}, err
	}
	return store.LockRepoForStateMappingForUserRow{
		ID:                       a.ID,
		IssueTrackerConnectionID: pgtype.UUID{Bytes: r.linkedConn, Valid: r.linkedConn != uuid.Nil},
		IssueTrackerProjectID:    pgtype.Text{String: r.projectID, Valid: r.linkedConn != uuid.Nil},
	}, nil
}

func (f *fakeQuerier) ListBoardColumns(_ context.Context, repoID uuid.UUID) ([]store.BoardColumn, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.BoardColumn
	if r := f.repos[repoID]; r != nil {
		for i, l := range r.boardColumns {
			out = append(out, store.BoardColumn{RepoID: repoID, LabelName: l, Position: int32(i)})
		}
	}
	return out, nil
}

func (f *fakeQuerier) UpsertRepoIssueTrackerStates(_ context.Context, a store.UpsertRepoIssueTrackerStatesParams) (store.RepoIssueTrackerState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.ownedRepo(a.RepoID, a.UserID)
	if err != nil || r.linkedConn == uuid.Nil {
		return store.RepoIssueTrackerState{}, pgx.ErrNoRows
	}
	r.openState, r.closedState, r.hasStates = a.OpenState, a.ClosedState, true
	return store.RepoIssueTrackerState{RepoID: a.RepoID, OpenState: a.OpenState, ClosedState: a.ClosedState}, nil
}

func (f *fakeQuerier) DeleteRepoIssueTrackerColumnStates(_ context.Context, repoID uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.repos[repoID].columnStates = nil
	return nil
}

func (f *fakeQuerier) InsertRepoIssueTrackerColumnState(_ context.Context, a store.InsertRepoIssueTrackerColumnStateParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.repos[a.RepoID]
	if r.columnStates == nil {
		r.columnStates = map[string]string{}
	}
	r.columnStates[a.LabelName] = a.StateName
	return nil
}
