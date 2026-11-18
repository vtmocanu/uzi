package issuetrackersvc

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// fakeRepo is the link-relevant slice of a repos row.
type fakeRepo struct {
	userID        uuid.UUID
	awaiting      bool
	linkedConn    uuid.UUID // zero when unlinked
	projectID     string
	projectShort  string
	tagID         string
	tagName       string
	storesNumbers bool
}

func (f *fakeQuerier) addRepo(user uuid.UUID, awaiting bool) uuid.UUID {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.repos == nil {
		f.repos = map[uuid.UUID]*fakeRepo{}
	}
	id := uuid.New()
	f.repos[id] = &fakeRepo{userID: user, awaiting: awaiting}
	return id
}

func (f *fakeQuerier) repo(id uuid.UUID) *fakeRepo {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.repos[id]
}

func (f *fakeQuerier) ownedRepo(id, user uuid.UUID) (*fakeRepo, error) {
	r := f.repos[id]
	if r == nil || r.userID != user {
		return nil, pgx.ErrNoRows
	}
	return r, nil
}

func (f *fakeQuerier) GetRepoIssueSourceForUser(_ context.Context, a store.GetRepoIssueSourceForUserParams) (store.GetRepoIssueSourceForUserRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.ownedRepo(a.ID, a.UserID)
	if err != nil {
		return store.GetRepoIssueSourceForUserRow{}, err
	}
	return store.GetRepoIssueSourceForUserRow{
		ID: a.ID, AwaitingFirstEnable: r.awaiting,
		IssueTrackerConnectionID: pgtype.UUID{Bytes: r.linkedConn, Valid: r.linkedConn != uuid.Nil},
	}, nil
}

func (f *fakeQuerier) LockRepoForLinkForUser(_ context.Context, a store.LockRepoForLinkForUserParams) (store.LockRepoForLinkForUserRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.ownedRepo(a.ID, a.UserID)
	if err != nil {
		return store.LockRepoForLinkForUserRow{}, err
	}
	return store.LockRepoForLinkForUserRow{
		ID: a.ID, AwaitingFirstEnable: r.awaiting,
		IssueTrackerConnectionID: pgtype.UUID{Bytes: r.linkedConn, Valid: r.linkedConn != uuid.Nil},
	}, nil
}

func (f *fakeQuerier) RepoStoresIssueNumber(_ context.Context, id uuid.UUID) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r := f.repos[id]; r != nil {
		return r.storesNumbers, nil
	}
	return false, nil
}

func (f *fakeQuerier) SetRepoIssueTrackerLink(_ context.Context, a store.SetRepoIssueTrackerLinkParams) (store.Repo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.ownedRepo(a.RepoID, a.UserID)
	if err != nil || !r.awaiting || r.linkedConn != uuid.Nil {
		return store.Repo{}, pgx.ErrNoRows
	}
	r.linkedConn, r.projectID, r.projectShort, r.tagID, r.tagName = a.ConnectionID, a.ProjectID, a.ProjectShort, a.TagID, a.TagName
	now := pgtype.Timestamptz{Valid: true}
	text := func(s string) pgtype.Text { return pgtype.Text{String: s, Valid: true} }
	return store.Repo{
		ID: a.RepoID, IssueTrackerConnectionID: pgtype.UUID{Bytes: a.ConnectionID, Valid: true},
		IssueTrackerProjectID: text(a.ProjectID), IssueTrackerProjectShort: text(a.ProjectShort),
		EligibilityTagID: text(a.TagID), EligibilityTagName: text(a.TagName), IssueTrackerLinkedAt: now,
	}, nil
}

func (f *fakeQuerier) CountReposLinkedToIssueTrackerConnection(_ context.Context, id uuid.UUID) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int64
	for _, r := range f.repos {
		if r.linkedConn == id {
			n++
		}
	}
	return n, nil
}

func (f *fakeQuerier) LockIssueTrackerConnectionByIdentityForUpdate(_ context.Context, a store.LockIssueTrackerConnectionByIdentityForUpdateParams) (store.IssueTrackerConnection, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.rows {
		if r.UserID == a.UserID && r.Kind == a.Kind && r.BaseUrl == a.BaseUrl {
			return r, nil
		}
	}
	return store.IssueTrackerConnection{}, pgx.ErrNoRows
}

func (f *fakeQuerier) LockIssueTrackerConnectionForShareForUser(ctx context.Context, a store.LockIssueTrackerConnectionForShareForUserParams) (store.IssueTrackerConnection, error) {
	return f.GetIssueTrackerConnectionForUser(ctx, store.GetIssueTrackerConnectionForUserParams(a))
}

func (f *fakeQuerier) InsertIssueTrackerConnectionIfAbsent(ctx context.Context, a store.InsertIssueTrackerConnectionIfAbsentParams) (store.IssueTrackerConnection, error) {
	f.mu.Lock()
	for _, r := range f.rows {
		if r.UserID == a.UserID && r.Kind == a.Kind && r.BaseUrl == a.BaseUrl {
			f.mu.Unlock()
			return store.IssueTrackerConnection{}, pgx.ErrNoRows
		}
	}
	f.mu.Unlock()
	return f.UpsertIssueTrackerConnection(ctx, store.UpsertIssueTrackerConnectionParams(a))
}
