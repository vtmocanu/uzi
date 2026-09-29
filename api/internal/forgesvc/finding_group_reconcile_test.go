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
	"github.com/jackc/pgx/v5/pgconn"
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
			index, err := indexFindingGroupIssues(tc.issues)
			if err != nil {
				t.Fatal(err)
			}
			match, found := index[id]
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
		ops, finish, err := svc.pendingFindingGroups(context.Background(), repo)
		if err != nil {
			t.Fatal(err)
		}
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
	_, finish, err := svc.pendingFindingGroups(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
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
	index, err := indexFindingGroupIssues([]forge.Issue{{
		IID: 7, WebURL: "https://example.com/issues/7",
		Description: marker(first) + marker(second),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if index[first].ambiguous || index[second].ambiguous ||
		index[first].issue.IID != 7 || index[second].issue.IID != 7 {
		t.Fatalf("distinct markers should each identify the issue: %#v", index)
	}
}

type markerPageDB struct {
	*pendingPageDB
	recorded         uuid.UUID
	settled          uuid.UUID
	iid              int64
	url              string
	candidateQueries int
}

func (db *markerPageDB) Query(ctx context.Context, query string, args ...interface{}) (pgx.Rows, error) {
	if !strings.Contains(query, "o.id=ANY") {
		return db.pendingPageDB.Query(ctx, query, args...)
	}
	db.candidateQueries++
	ids := args[1].([]uuid.UUID)
	var matches []store.FindingGroupClaimOperation
	for _, op := range db.ops {
		for _, id := range ids {
			if op.ID == id && op.Phase == "in_flight" && db.settled != id {
				matches = append(matches, op)
			}
		}
	}
	return &pendingPageRows{ops: matches}, nil
}

func (db *markerPageDB) Exec(_ context.Context, _ string, args ...interface{}) (pgconn.CommandTag, error) {
	db.iid = args[0].(int64)
	db.url = args[1].(string)
	db.recorded = args[2].(uuid.UUID)
	return pgconn.NewCommandTag("UPDATE 1"), nil
}

func (db *markerPageDB) Begin(context.Context) (pgx.Tx, error) {
	return &markerTx{db: db}, nil
}

type markerTx struct {
	pgx.Tx
	db *markerPageDB
}

func (tx *markerTx) QueryRow(_ context.Context, query string, _ ...interface{}) pgx.Row {
	switch {
	case strings.Contains(query, "SELECT repo_id,issue_iid,issue_url"):
		return markerRow{values: []any{tx.db.ops[100].RepoID, tx.db.iid, tx.db.url}}
	case strings.Contains(query, "finding_group_members"):
		return markerRow{values: []any{int64(1)}}
	default:
		return markerRow{values: []any{int64(1)}}
	}
}
func (tx *markerTx) Exec(_ context.Context, _ string, _ ...interface{}) (pgconn.CommandTag, error) {
	return pgconn.NewCommandTag("UPDATE 1"), nil
}
func (tx *markerTx) Commit(context.Context) error {
	tx.db.settled = tx.db.recorded
	return nil
}
func (tx *markerTx) Rollback(context.Context) error { return nil }

type markerRow struct{ values []any }

func (r markerRow) Scan(dest ...interface{}) error {
	for i, value := range r.values {
		switch d := dest[i].(type) {
		case *uuid.UUID:
			*d = value.(uuid.UUID)
		case *int64:
			*d = value.(int64)
		case *string:
			*d = value.(string)
		}
	}
	return nil
}

func TestIncrementalSyncMatchesMarkerBeyondRotatingPage(t *testing.T) {
	repo, user := uuid.New(), uuid.New()
	start := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	db := &markerPageDB{pendingPageDB: &pendingPageDB{}}
	for i := 0; i < 101; i++ {
		db.ops = append(db.ops, store.FindingGroupClaimOperation{
			ID: uuid.New(), UserID: user, RepoID: repo, Phase: "in_flight",
			CreatedAt: start.Add(time.Duration(i) * time.Second),
		})
	}
	target := db.ops[100].ID
	svc := newTestService(&fakeStore{})
	svc.SetFindingGroupDB(db)
	marker := "<!-- uzi-finding-group-operation: " + target.String() + " -->"
	first := &fakeForge{findingIssues: []forge.Issue{{
		IID: 301, WebURL: "https://example.com/301", Description: marker,
		UpdatedAt: start.Add(200 * time.Second),
	}}}
	marks, err := svc.IncrementalSync(context.Background(), repo, 7, first, Marks{})
	if err != nil {
		t.Fatal(err)
	}
	if db.recorded != target || db.settled != target || db.candidateQueries != 1 {
		t.Fatalf("operation 101: recorded=%s settled=%s candidate queries=%d", db.recorded, db.settled, db.candidateQueries)
	}
	if !marks.Finding.Equal(start.Add(200 * time.Second)) {
		t.Fatalf("finding mark = %v", marks.Finding)
	}
	second := &fakeForge{findingIssues: []forge.Issue{{
		IID: 302, WebURL: "https://example.com/302", UpdatedAt: start.Add(201 * time.Second),
	}}}
	next, err := svc.IncrementalSync(context.Background(), repo, 7, second, marks)
	if err != nil {
		t.Fatal(err)
	}
	if !next.Finding.Equal(start.Add(201*time.Second)) || db.settled != target {
		t.Fatalf("second pass: mark=%v settled=%s", next.Finding, db.settled)
	}
}

func TestFindingGroupMarkerLimitFailsBeforeCacheWrites(t *testing.T) {
	repo := uuid.New()
	cache := &fakeStore{}
	svc := newTestService(cache)
	svc.SetFindingGroupDB(&markerPageDB{pendingPageDB: &pendingPageDB{ops: []store.FindingGroupClaimOperation{{
		ID: uuid.New(), UserID: uuid.New(), RepoID: repo, Phase: "in_flight", CreatedAt: time.Now(),
	}}}})
	issue := forge.Issue{Description: strings.Repeat("a", findingGroupDescriptionLimit+1), UpdatedAt: time.Now()}
	start := Marks{Finding: time.Now().Add(-time.Hour)}
	got, err := svc.IncrementalSync(context.Background(), repo, 7, &fakeForge{findingIssues: []forge.Issue{issue}}, start)
	if err == nil || got != start || len(cache.upserts) != 0 {
		t.Fatalf("mark=%v error=%v cache writes=%d", got, err, len(cache.upserts))
	}
}

func TestFindingGroupCandidateLimitHoldsFindingMark(t *testing.T) {
	repo, user := uuid.New(), uuid.New()
	op := store.FindingGroupClaimOperation{ID: uuid.New(), UserID: user, RepoID: repo, Phase: "in_flight", CreatedAt: time.Now()}
	db := &markerPageDB{pendingPageDB: &pendingPageDB{ops: []store.FindingGroupClaimOperation{op}}}
	cache := &fakeStore{}
	svc := newTestService(cache)
	svc.SetFindingGroupDB(db)
	marker := "<!-- uzi-finding-group-operation: " + op.ID.String() + " -->"
	start := Marks{Finding: time.Now().Add(-time.Hour)}
	issue := forge.Issue{IID: 77, Description: strings.Repeat(marker, findingGroupCandidateLimit+1), UpdatedAt: time.Now()}
	got, err := svc.IncrementalSync(context.Background(), repo, 7, &fakeForge{findingIssues: []forge.Issue{issue}}, start)
	if err == nil || got != start || len(cache.upserts) != 0 || db.candidateQueries != 0 || db.recorded != uuid.Nil {
		t.Fatalf("mark=%v error=%v writes=%d queries=%d recorded=%s", got, err, len(cache.upserts), db.candidateQueries, db.recorded)
	}
}
