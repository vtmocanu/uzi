package forgesvc

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestMatchFindingGroupIssue(t *testing.T) {
	id := uuid.New()
	marker := "<!-- uzi-finding-group-operation: " + id.String() + " -->"
	valid := forge.Issue{IID: 42, WebURL: "https://example.com/issues/42", Description: "body\n" + marker}
	tests := []struct {
		name   string
		issues []forge.Issue
		want   bool
	}{
		{"unique", []forge.Issue{{Description: "<!-- uzi-finding-group-operation: " + uuid.NewString() + " -->"}, valid}, true},
		{"absent", []forge.Issue{{IID: 42, WebURL: valid.WebURL}}, false},
		{"altered marker", []forge.Issue{{IID: 42, WebURL: valid.WebURL, Description: "<!-- uzi-finding-group-operation:" + id.String() + " -->"}}, false},
		{"two issues", []forge.Issue{valid, valid}, false},
		{"repeated in issue", []forge.Issue{{IID: 42, WebURL: valid.WebURL, Description: marker + marker}}, false},
		{"invalid identity", []forge.Issue{{Description: marker}}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			match, found := indexFindingGroupIssues(tc.issues)[id]
			ok := found && !match.ambiguous && match.issue.IID > 0 && match.issue.WebURL != ""
			if ok != tc.want {
				t.Fatalf("match = %v, want %v", ok, tc.want)
			}
			if ok && match.issue.IID != 42 {
				t.Fatalf("matched iid = %d", match.issue.IID)
			}
		})
	}
}

type pendingPageDB struct {
	store.DBTX
	store.FindingGroupDB
	ops         []store.FindingGroupClaimOperation
	statsCtxErr error
}

func (db *pendingPageDB) Query(_ context.Context, _ string, args ...interface{}) (pgx.Rows, error) {
	var after *time.Time
	if len(args) > 1 {
		cursor := args[1].(time.Time)
		after = &cursor
	}
	var page []store.FindingGroupClaimOperation
	for _, op := range db.ops {
		if after != nil && !op.CreatedAt.After(*after) {
			continue
		}
		page = append(page, op)
		if len(page) == 100 {
			break
		}
	}
	return &pendingPageRows{ops: page}, nil
}

func (db *pendingPageDB) QueryRow(ctx context.Context, _ string, _ ...interface{}) pgx.Row {
	db.statsCtxErr = ctx.Err()
	return pendingStatsRow{count: int64(len(db.ops)), oldest: db.ops[0].CreatedAt}
}

type pendingStatsRow struct {
	count  int64
	oldest time.Time
}

func (r pendingStatsRow) Scan(dest ...interface{}) error {
	*dest[0].(*int64) = r.count
	*dest[1].(**time.Time) = &r.oldest
	return nil
}

type pendingPageRows struct {
	pgx.Rows
	ops []store.FindingGroupClaimOperation
	pos int
}

func (r *pendingPageRows) Next() bool { return r.pos < len(r.ops) }
func (r *pendingPageRows) Close()     {}
func (r *pendingPageRows) Err() error { return nil }
func (r *pendingPageRows) Scan(dest ...interface{}) error {
	op := r.ops[r.pos]
	r.pos++
	*dest[0].(*uuid.UUID) = op.ID
	*dest[1].(*uuid.UUID) = op.UserID
	*dest[2].(*uuid.UUID) = op.RepoID
	*dest[3].(*string) = op.Phase
	*dest[4].(*time.Time) = op.Deadline
	*dest[5].(*time.Time) = op.CreatedAt
	*dest[6].(**int64) = op.IssueIID
	*dest[7].(*string) = op.IssueURL
	return nil
}

func TestPendingFindingGroupsCursorWrapsPastUnmatched(t *testing.T) {
	repo, user := uuid.New(), uuid.New()
	db := &pendingPageDB{}
	start := time.Now().Add(-time.Hour)
	for i := 0; i < 101; i++ {
		db.ops = append(db.ops, store.FindingGroupClaimOperation{
			ID: uuid.New(), UserID: user, RepoID: repo, Phase: "in_flight",
			CreatedAt: start.Add(time.Duration(i) * time.Second),
		})
	}
	svc := &Service{groupDB: db}
	for pass, want := range []struct {
		count int
		first int
		last  int
	}{{100, 0, 99}, {1, 100, 100}, {100, 0, 99}} {
		ops, finish := svc.pendingFindingGroups(context.Background(), repo)
		if len(ops) != want.count || ops[0].ID != db.ops[want.first].ID || ops[len(ops)-1].ID != db.ops[want.last].ID {
			t.Fatalf("pass %d: selected %d ops, wanted indices %d..%d", pass, len(ops), want.first, want.last)
		}
		finish()
	}
}

func TestPendingFindingGroupsLogsStatsAfterSyncCancellation(t *testing.T) {
	repo := uuid.New()
	db := &pendingPageDB{ops: []store.FindingGroupClaimOperation{{
		ID: uuid.New(), UserID: uuid.New(), RepoID: repo,
		Phase: "in_flight", CreatedAt: time.Now().Add(-time.Minute),
	}}}
	var output bytes.Buffer
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&output, nil)))
	t.Cleanup(func() { slog.SetDefault(oldLogger) })

	svc := &Service{groupDB: db}
	ctx, cancel := context.WithCancel(context.Background())
	_, finish := svc.pendingFindingGroups(ctx, repo)
	cancel()
	finish()
	log := output.String()
	if db.statsCtxErr != nil || strings.Count(log, "finding group reconciliation pending") != 1 ||
		!strings.Contains(log, "pending_group_operations=1") || !strings.Contains(log, "oldest_age=") {
		t.Fatalf("stats context error = %v; warning = %q", db.statsCtxErr, log)
	}
}

func TestFindingGroupIndexKeepsDistinctMarkers(t *testing.T) {
	first, second := uuid.New(), uuid.New()
	marker := func(id uuid.UUID) string {
		return fmt.Sprintf("<!-- uzi-finding-group-operation: %s -->", id)
	}
	index := indexFindingGroupIssues([]forge.Issue{{
		IID: 7, WebURL: "https://example.com/issues/7",
		Description: marker(first) + marker(second),
	}})
	if index[first].ambiguous || index[second].ambiguous ||
		index[first].issue.IID != 7 || index[second].issue.IID != 7 {
		t.Fatalf("distinct markers should each identify the issue: %#v", index)
	}
}
