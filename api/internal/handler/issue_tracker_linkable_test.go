package handler

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/config"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestLinkableFor(t *testing.T) {
	linked := store.Repo{AwaitingFirstEnable: true, IssueTrackerConnectionID: pgtype.UUID{Bytes: uuid.New(), Valid: true}}
	tests := []struct {
		name    string
		repo    store.Repo
		offered bool
		want    bool
	}{
		{"linked", linked, true, false},
		{"never-enabled, link offered", store.Repo{AwaitingFirstEnable: true}, true, true},
		{"never-enabled, nothing configured", store.Repo{AwaitingFirstEnable: true}, false, false},
		{"once enabled is not awaiting", store.Repo{AwaitingFirstEnable: false, Enabled: true}, true, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := linkableFor(tc.repo, tc.offered); got != tc.want {
				t.Errorf("linkableFor = %v, want %v", got, tc.want)
			}
		})
	}
}

// linkOfferDB answers ListIssueTrackerConnectionsByUser with rows one-row-or-none, or an
// error, and counts the queries it receives.
type linkOfferDB struct {
	rows    int
	err     error
	queries int
}

func (*linkOfferDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}

func (f *linkOfferDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	f.queries++
	if f.err != nil {
		return nil, f.err
	}
	return &linkOfferRows{left: f.rows}, nil
}

func (*linkOfferDB) QueryRow(context.Context, string, ...any) pgx.Row { return nil }

// linkOfferRows yields left rows of an issue_tracker_connections list. Scan leaves the
// destinations zero: the caller only counts rows.
type linkOfferRows struct {
	emptyViewerRows
	left int
}

func (r *linkOfferRows) Next() bool {
	if r.left == 0 {
		return false
	}
	r.left--
	return true
}

func TestIssueTrackerLinkOffered(t *testing.T) {
	tests := []struct {
		name        string
		allowlist   []string
		db          linkOfferDB
		want        bool
		wantQueries int
	}{
		{"allowlist set asks nothing", []string{"https://issues.example.com"}, linkOfferDB{err: errors.New("unused")}, true, 0},
		{"empty allowlist, one connection", nil, linkOfferDB{rows: 1}, true, 1},
		{"empty allowlist, no connection", nil, linkOfferDB{}, false, 1},
		{"store error reads as not offered", nil, linkOfferDB{err: errors.New("db down")}, false, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db := tc.db
			h := &Handler{q: store.New(&db), cfg: config.Config{IssueTrackerAllowedBaseURLs: tc.allowlist}}
			if got := h.issueTrackerLinkOffered(context.Background(), uuid.New()); got != tc.want {
				t.Errorf("offered = %v, want %v", got, tc.want)
			}
			if db.queries != tc.wantQueries {
				t.Errorf("queries = %d, want %d", db.queries, tc.wantQueries)
			}
		})
	}
}

func TestSetLinkableAsksOnce(t *testing.T) {
	linkedID := pgtype.UUID{Bytes: uuid.New(), Valid: true}
	awaiting := store.Repo{AwaitingFirstEnable: true}
	tests := []struct {
		name        string
		repos       []store.Repo
		db          linkOfferDB
		want        []bool
		wantQueries int
	}{
		{
			"several awaiting repos share one connections query",
			[]store.Repo{awaiting, {AwaitingFirstEnable: true, IssueTrackerConnectionID: linkedID}, awaiting, {Enabled: true}, awaiting},
			linkOfferDB{rows: 1}, []bool{true, false, true, false, true}, 1,
		},
		{
			"no connection reads as not linkable for all",
			[]store.Repo{awaiting, awaiting},
			linkOfferDB{}, []bool{false, false}, 1,
		},
		{
			"no awaiting repo asks nothing",
			[]store.Repo{{AwaitingFirstEnable: true, IssueTrackerConnectionID: linkedID}, {Enabled: true}},
			linkOfferDB{rows: 1}, []bool{false, false}, 0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db := tc.db
			h := &Handler{q: store.New(&db)}
			dtos := make([]apitypes.RepoDTO, len(tc.repos))
			h.setLinkable(context.Background(), uuid.New(), tc.repos, dtos)
			for i, d := range dtos {
				if d.IssueTrackerLinkable != tc.want[i] {
					t.Errorf("repo %d linkable = %v, want %v", i, d.IssueTrackerLinkable, tc.want[i])
				}
			}
			if db.queries != tc.wantQueries {
				t.Errorf("queries = %d, want %d", db.queries, tc.wantQueries)
			}
		})
	}
}
