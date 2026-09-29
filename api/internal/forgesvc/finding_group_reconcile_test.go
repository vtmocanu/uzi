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
		{"unwanted marker repeated", []forge.Issue{valid, {Description: strings.Repeat("<!-- uzi-finding-group-operation: "+uuid.NewString()+" -->", 2)}}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			index := indexFindingGroupIssues(tc.issues, map[uuid.UUID]struct{}{id: {}})
			if len(index) > 1 {
				t.Fatalf("indexed %d ids, want only the wanted one", len(index))
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
		if op.Phase == "settled" {
			continue
		}
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
	row := pendingStatsRow{count: int64(len(db.ops))}
	if len(db.ops) > 0 {
		oldest := db.ops[0].CreatedAt
		row.oldest = &oldest
	}
	return row
}

// pendingStatsRow mirrors count(*),min(created_at): a NULL minimum for no rows.
type pendingStatsRow struct {
	count  int64
	oldest *time.Time
}

func (r pendingStatsRow) Scan(dest ...interface{}) error {
	*dest[0].(*int64) = r.count
	*dest[1].(**time.Time) = r.oldest
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
		ops, finish, advance, err := svc.pendingFindingGroups(context.Background(), repo)
		if err != nil {
			t.Fatal(err)
		}
		if len(ops) != want.count || ops[0].ID != db.ops[want.first].ID || ops[len(ops)-1].ID != db.ops[want.last].ID {
			t.Fatalf("pass %d: selected %d ops, wanted indices %d..%d", pass, len(ops), want.first, want.last)
		}
		advance()
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
	_, finish, _, err := svc.pendingFindingGroups(ctx, repo)
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
	index := indexFindingGroupIssues([]forge.Issue{{
		IID: 7, WebURL: "https://example.com/issues/7",
		Description: marker(first) + marker(second),
	}}, map[uuid.UUID]struct{}{first: {}, second: {}})
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
	beginErr         error
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
			if op.ID == id && (op.Phase == "in_flight" || op.Phase == "returned_uncertain") && db.settled != id {
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
	for i := range db.ops {
		if db.ops[i].ID == db.recorded {
			db.ops[i].Phase = "issue_recorded"
			db.ops[i].IssueIID = &db.iid
			db.ops[i].IssueURL = db.url
		}
	}
	return pgconn.NewCommandTag("UPDATE 1"), nil
}

// recordedOp is the operation RecordFindingGroupIssue last touched, so the settlement
// transaction fake answers for the op under test whatever the fixture size.
func (db *markerPageDB) recordedOp() store.FindingGroupClaimOperation {
	for _, op := range db.ops {
		if op.ID == db.recorded {
			return op
		}
	}
	return store.FindingGroupClaimOperation{}
}

func (db *markerPageDB) Begin(context.Context) (pgx.Tx, error) {
	if db.beginErr != nil {
		return nil, db.beginErr
	}
	return &markerTx{db: db}, nil
}

type markerTx struct {
	pgx.Tx
	db *markerPageDB
}

func (tx *markerTx) QueryRow(_ context.Context, query string, _ ...interface{}) pgx.Row {
	switch {
	case strings.Contains(query, "SELECT repo_id,issue_iid,issue_url"):
		return markerRow{values: []any{tx.db.recordedOp().RepoID, tx.db.iid, tx.db.url}}
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
	for i := range tx.db.ops {
		if tx.db.ops[i].ID == tx.db.recorded {
			tx.db.ops[i].Phase = "settled"
		}
	}
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

// Only the current page's operations are matched. An operation on a later page is not
// matched while another page is current, and settles once FullSync rotates to it.
func TestFullSyncMatchesMarkerOnLaterPageAfterRotation(t *testing.T) {
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
	carrier := forge.Issue{
		IID: 301, WebURL: "https://example.com/301",
		Description: "<!-- uzi-finding-group-operation: " + target.String() + " -->",
		UpdatedAt:   start.Add(200 * time.Second),
	}
	first := &fakeForge{allIssues: []forge.Issue{carrier}}
	if _, err := svc.FullSync(context.Background(), repo, 7, first); err != nil {
		t.Fatal(err)
	}
	if db.recorded != uuid.Nil || db.candidateQueries != 0 || len(first.unfilteredListCalls()) != 1 {
		t.Fatalf("first page matched operation 101: recorded=%s candidate queries=%d unfiltered=%d",
			db.recorded, db.candidateQueries, len(first.unfilteredListCalls()))
	}
	second := &fakeForge{allIssues: []forge.Issue{carrier}}
	if _, err := svc.FullSync(context.Background(), repo, 7, second); err != nil {
		t.Fatal(err)
	}
	if db.recorded != target || db.settled != target || db.candidateQueries != 1 {
		t.Fatalf("second page: recorded=%s settled=%s candidate queries=%d", db.recorded, db.settled, db.candidateQueries)
	}
}

func TestIncrementalSyncSettlementFailureHoldsMarks(t *testing.T) {
	repo, user := uuid.New(), uuid.New()
	iid := int64(31)
	db := &markerPageDB{pendingPageDB: &pendingPageDB{ops: []store.FindingGroupClaimOperation{{
		ID: uuid.New(), UserID: user, RepoID: repo, Phase: "issue_recorded", IssueIID: &iid,
		IssueURL: "https://example.com/issues/31", CreatedAt: time.Now().Add(-time.Minute),
	}}}, beginErr: fmt.Errorf("settlement database unavailable")}
	for i := 0; i < 100; i++ {
		db.ops = append(db.ops, store.FindingGroupClaimOperation{
			ID: uuid.New(), UserID: user, RepoID: repo, Phase: "in_flight",
			CreatedAt: db.ops[0].CreatedAt.Add(time.Duration(i+1) * time.Second),
		})
	}
	cache := &fakeStore{}
	svc := newTestService(cache)
	svc.SetFindingGroupDB(db)
	start := Marks{Finding: time.Now().Add(-time.Hour)}
	for pass := 0; pass < 2; pass++ {
		got, err := svc.IncrementalSync(context.Background(), repo, 7, &fakeForge{findingIssues: []forge.Issue{{
			IID: 32, UpdatedAt: time.Now(),
		}}}, start)
		if err == nil || got != start || len(cache.upserts) != 0 {
			t.Fatalf("pass %d: mark=%v error=%v cache writes=%d", pass, got, err, len(cache.upserts))
		}
	}
	if _, advanced := svc.groupCursors[repo]; advanced {
		t.Fatal("failed settlement advanced the group cursor past the recorded issue")
	}
}

func TestFullSyncMarkerSettlementFailureRetainsPage(t *testing.T) {
	repo, user := uuid.New(), uuid.New()
	db := &markerPageDB{pendingPageDB: &pendingPageDB{}}
	startTime := time.Now().Add(-time.Hour)
	for i := 0; i < 101; i++ {
		db.ops = append(db.ops, store.FindingGroupClaimOperation{
			ID: uuid.New(), UserID: user, RepoID: repo, Phase: "in_flight",
			CreatedAt: startTime.Add(time.Duration(i) * time.Second),
		})
	}
	db.beginErr = fmt.Errorf("settlement unavailable after marker record")
	cache := &fakeStore{}
	svc := newTestService(cache)
	svc.SetFindingGroupDB(db)
	marker := "<!-- uzi-finding-group-operation: " + db.ops[0].ID.String() + " -->"
	for pass := 0; pass < 2; pass++ {
		got, err := svc.FullSync(context.Background(), repo, 7, &fakeForge{allIssues: []forge.Issue{{
			IID: 44, WebURL: "https://example.com/issues/44", Description: marker,
			UpdatedAt: startTime.Add(200 * time.Second),
		}}})
		if err == nil || got != (Marks{}) || len(cache.upserts) != 0 || len(cache.deleteCalls) != 0 {
			t.Fatalf("pass %d: marks=%v error=%v writes=%d evictions=%d", pass, got, err, len(cache.upserts), len(cache.deleteCalls))
		}
	}
	if db.ops[0].Phase != "issue_recorded" || db.candidateQueries != 1 {
		t.Fatalf("second pass did not retry the durable record: phase=%s candidate queries=%d", db.ops[0].Phase, db.candidateQueries)
	}
	if _, advanced := svc.groupCursors[repo]; advanced {
		t.Fatal("marker settlement failure advanced past the recorded operation")
	}
}

// requireScanSkipped asserts a FullSync whose marker scan could not see the complete
// issue set: the issue sync still succeeds (marks reported, cache written and
// evicted), the claim stays untouched, and the group cursor rotates.
func requireScanSkipped(t *testing.T, svc *Service, cache *fakeStore, db *markerPageDB, repo uuid.UUID, marks Marks, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("FullSync failed on an incomplete marker scan: %v", err)
	}
	if marks.PRD.IsZero() || len(cache.upserts) == 0 || len(cache.deleteCalls) == 0 {
		t.Fatalf("issue sync did not run: marks=%v writes=%d evictions=%d", marks, len(cache.upserts), len(cache.deleteCalls))
	}
	if db.candidateQueries != 0 || db.recorded != uuid.Nil || db.ops[0].Phase != "in_flight" || db.ops[0].IssueIID != nil {
		t.Fatalf("claim changed: queries=%d recorded=%s phase=%s", db.candidateQueries, db.recorded, db.ops[0].Phase)
	}
	if _, advanced := svc.groupCursors[repo]; !advanced {
		t.Fatal("incomplete scan did not rotate the group cursor")
	}
}

func newHeldFixture() (*Service, *fakeStore, *markerPageDB, store.FindingGroupClaimOperation, uuid.UUID) {
	repo := uuid.New()
	op := store.FindingGroupClaimOperation{ID: uuid.New(), UserID: uuid.New(), RepoID: repo, Phase: "in_flight", CreatedAt: time.Now()}
	db := &markerPageDB{pendingPageDB: &pendingPageDB{ops: []store.FindingGroupClaimOperation{op}}}
	cache := &fakeStore{}
	svc := newTestService(cache)
	svc.SetFindingGroupDB(db)
	return svc, cache, db, op, repo
}

// requireSyncedAndSettled asserts a FullSync ran the issue sync and settled op on iid.
func requireSyncedAndSettled(t *testing.T, cache *fakeStore, db *markerPageDB, op uuid.UUID, iid int64, marks Marks, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("FullSync: %v", err)
	}
	if marks.PRD.IsZero() || len(cache.upserts) == 0 || len(cache.deleteCalls) == 0 {
		t.Fatalf("issue sync did not run: marks=%v writes=%d evictions=%d", marks, len(cache.upserts), len(cache.deleteCalls))
	}
	if db.recorded != op || db.settled != op || db.iid != iid {
		t.Fatalf("recorded=%s settled=%s iid=%d, want op settled on #%d", db.recorded, db.settled, db.iid, iid)
	}
}

// An oversized description in an unrelated issue neither fails the sync nor stops the
// pending operation from settling on its own issue.
func TestFindingGroupOversizedDescriptionDoesNotBlockSettlement(t *testing.T) {
	svc, cache, db, op, repo := newHeldFixture()
	big := forge.Issue{IID: 76, Description: strings.Repeat("a", 2<<20), UpdatedAt: time.Now()}
	genuine := forge.Issue{IID: 78, WebURL: "https://example.com/78", Description: "<!-- uzi-finding-group-operation: " + op.ID.String() + " -->", UpdatedAt: time.Now()}
	f := &fakeForge{allIssues: []forge.Issue{big, genuine}, issues: []forge.Issue{issueAt(1, time.Now())}}
	marks, err := svc.FullSync(context.Background(), repo, 7, f)
	requireSyncedAndSettled(t, cache, db, op.ID, 78, marks, err)
}

// A wanted marker repeated many times is ambiguous: the op stays claimed, and the sync
// still runs.
func TestFindingGroupRepeatedWantedMarkerStaysClaimedAndSyncs(t *testing.T) {
	svc, cache, db, op, repo := newHeldFixture()
	marker := "<!-- uzi-finding-group-operation: " + op.ID.String() + " -->"
	issue := forge.Issue{IID: 77, WebURL: "https://example.com/77", Description: strings.Repeat(marker, 1001), UpdatedAt: time.Now()}
	f := &fakeForge{allIssues: []forge.Issue{issue}, issues: []forge.Issue{issueAt(1, time.Now())}}
	marks, err := svc.FullSync(context.Background(), repo, 7, f)
	if err != nil {
		t.Fatalf("FullSync: %v", err)
	}
	if marks.PRD.IsZero() || len(cache.upserts) == 0 {
		t.Fatalf("issue sync did not run: marks=%v writes=%d", marks, len(cache.upserts))
	}
	if db.recorded != uuid.Nil || db.ops[0].Phase != "in_flight" || db.ops[0].IssueIID != nil {
		t.Fatalf("ambiguous marker moved the op: recorded=%s phase=%s", db.recorded, db.ops[0].Phase)
	}
}

// Markers naming operations that are not pending on this page (settled long ago, or
// planted in an unrelated issue by anyone who can open one) are ignored: however many
// there are, the pending operation still settles on its own issue.
func TestFindingGroupUnwantedMarkersAreIgnored(t *testing.T) {
	svc, cache, db, op, repo := newHeldFixture()
	var planted strings.Builder
	for i := 0; i <= 1000; i++ {
		planted.WriteString("<!-- uzi-finding-group-operation: " + uuid.NewString() + " -->")
	}
	genuine := forge.Issue{IID: 78, WebURL: "https://example.com/78", Description: "<!-- uzi-finding-group-operation: " + op.ID.String() + " -->", UpdatedAt: time.Now()}
	f := &fakeForge{
		allIssues: []forge.Issue{{IID: 77, WebURL: "https://example.com/77", Description: planted.String(), UpdatedAt: time.Now()}, genuine},
		issues:    []forge.Issue{issueAt(1, time.Now())},
	}
	marks, err := svc.FullSync(context.Background(), repo, 7, f)
	requireSyncedAndSettled(t, cache, db, op.ID, 78, marks, err)
}

func TestFullSyncUnfilteredFetchErrorSkipsScanNotSync(t *testing.T) {
	svc, cache, db, _, repo := newHeldFixture()
	f := &fakeForge{allErr: fmt.Errorf("forge pagination cap"), issues: []forge.Issue{issueAt(1, time.Now())}}
	marks, err := svc.FullSync(context.Background(), repo, 7, f)
	requireScanSkipped(t, svc, cache, db, repo, marks, err)
	if len(f.unfilteredListCalls()) != 1 {
		t.Fatalf("unfiltered calls = %d, want 1", len(f.unfilteredListCalls()))
	}
}

// ── regression: markers are matched over the COMPLETE issue list only ─────────────────────

// The marker sits on two issues with different updated_at. An incremental pass whose
// watermark returns only the newer one must not settle the op, and neither may a later
// FullSync whose label-filtered fetches also show only one carrier: the unfiltered
// complete set exposes the duplicate.
func TestDuplicateMarkerHiddenFromFilteredFetchesNeverSettles(t *testing.T) {
	svc, _, db, op, repo := newHeldFixture()
	marker := "<!-- uzi-finding-group-operation: " + op.ID.String() + " -->"
	now := time.Now().UTC().Truncate(time.Second)
	older := forge.Issue{IID: 501, WebURL: "https://example.com/501", Description: marker, UpdatedAt: now.Add(-time.Hour)}
	newer := forge.Issue{IID: 502, WebURL: "https://example.com/502", Description: marker, UpdatedAt: now}
	watermark := Marks{Finding: now.Add(-time.Minute)}

	inc := &fakeForge{findingIssues: []forge.Issue{newer}}
	if _, err := svc.IncrementalSync(context.Background(), repo, 7, inc, watermark); err != nil {
		t.Fatalf("IncrementalSync: %v", err)
	}
	if len(inc.unfilteredListCalls()) != 0 {
		t.Fatalf("IncrementalSync made %d unfiltered calls", len(inc.unfilteredListCalls()))
	}
	full := &fakeForge{findingIssues: []forge.Issue{newer}, allIssues: []forge.Issue{older, newer}}
	if _, err := svc.FullSync(context.Background(), repo, 7, full); err != nil {
		t.Fatalf("FullSync: %v", err)
	}
	if db.recorded != uuid.Nil || db.settled != uuid.Nil || db.ops[0].Phase != "in_flight" || db.ops[0].IssueIID != nil {
		t.Fatalf("ambiguous marker moved the op: recorded=%s settled=%s phase=%s", db.recorded, db.settled, db.ops[0].Phase)
	}
}

// A created issue that lost (or never had) the finding label is invisible to the
// label-filtered fetches; the complete set still finds it, open or closed.
func TestFullSyncSettlesCreatedIssueWithoutFindingLabel(t *testing.T) {
	for _, state := range []string{"opened", "closed"} {
		t.Run(state, func(t *testing.T) {
			svc, _, db, op, repo := newHeldFixture()
			marker := "<!-- uzi-finding-group-operation: " + op.ID.String() + " -->"
			created := forge.Issue{
				IID: 601, State: state, WebURL: "https://example.com/601", Description: marker,
				UpdatedAt: time.Now(),
			}
			f := &fakeForge{allIssues: []forge.Issue{created}}
			if _, err := svc.FullSync(context.Background(), repo, 7, f); err != nil {
				t.Fatalf("FullSync: %v", err)
			}
			if db.recorded != op.ID || db.settled != op.ID || db.iid != 601 {
				t.Fatalf("recorded=%s settled=%s iid=%d, want op settled on #601", db.recorded, db.settled, db.iid)
			}
		})
	}
}

// The complete-set fetch is made only when an operation could actually be matched.
func TestFullSyncUnfilteredFetchOnlyWhenMatchableOpPending(t *testing.T) {
	iid := int64(31)
	mk := func(phase string, recorded bool) *markerPageDB {
		repoOps := []store.FindingGroupClaimOperation{}
		if phase != "" {
			op := store.FindingGroupClaimOperation{ID: uuid.New(), UserID: uuid.New(), RepoID: uuid.New(), Phase: phase, CreatedAt: time.Now().Add(-time.Minute)}
			if recorded {
				op.IssueIID, op.IssueURL = &iid, "https://example.com/issues/31"
			}
			repoOps = append(repoOps, op)
		}
		return &markerPageDB{pendingPageDB: &pendingPageDB{ops: repoOps}}
	}
	tests := []struct {
		name string
		db   *markerPageDB
		want int
	}{
		{"no pending op", mk("", false), 0},
		{"only a recorded op", mk("issue_recorded", true), 0},
		{"only a pre_call op", mk("pre_call", false), 0},
		{"an in_flight unrecorded op", mk("in_flight", false), 1},
		{"a returned_uncertain unrecorded op", mk("returned_uncertain", false), 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc := newTestService(&fakeStore{})
			svc.SetFindingGroupDB(tc.db)
			f := &fakeForge{}
			if _, err := svc.FullSync(context.Background(), uuid.New(), 7, f); err != nil {
				t.Fatalf("FullSync: %v", err)
			}
			if got := len(f.unfilteredListCalls()); got != tc.want {
				t.Fatalf("unfiltered calls = %d, want %d", got, tc.want)
			}
		})
	}
	t.Run("incremental never fetches the complete set", func(t *testing.T) {
		db := mk("in_flight", false)
		svc := newTestService(&fakeStore{})
		svc.SetFindingGroupDB(db)
		f := &fakeForge{}
		if _, err := svc.IncrementalSync(context.Background(), uuid.New(), 7, f, Marks{}); err != nil {
			t.Fatalf("IncrementalSync: %v", err)
		}
		if got := len(f.unfilteredListCalls()); got != 0 {
			t.Fatalf("unfiltered calls = %d, want 0", got)
		}
	})
}

// IncrementalSync must not rotate the group page: with 1 FullSync + 9
// IncrementalSync per reconcile cycle, incremental rotation left every FullSync on
// the same page, so an in_flight op on page 2 behind a page of pre_call ops was
// never examined.
func TestIncrementalSyncDoesNotRotateGroupPage(t *testing.T) {
	repo, user := uuid.New(), uuid.New()
	start := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	db := &markerPageDB{pendingPageDB: &pendingPageDB{}}
	for i := 0; i < 101; i++ {
		phase := "pre_call"
		if i == 100 {
			phase = "in_flight"
		}
		db.ops = append(db.ops, store.FindingGroupClaimOperation{
			ID: uuid.New(), UserID: user, RepoID: repo, Phase: phase,
			CreatedAt: start.Add(time.Duration(i) * time.Second),
		})
	}
	target := db.ops[100].ID
	svc := newTestService(&fakeStore{})
	svc.SetFindingGroupDB(db)
	marker := "<!-- uzi-finding-group-operation: " + target.String() + " -->"
	f := &fakeForge{allIssues: []forge.Issue{{
		IID: 301, WebURL: "https://example.com/301", Description: marker,
		UpdatedAt: start.Add(200 * time.Second),
	}}}
	ctx := context.Background()
	if _, err := svc.FullSync(ctx, repo, 7, f); err != nil {
		t.Fatal(err)
	}
	if db.settled != uuid.Nil {
		t.Fatal("page 1 holds only pre_call ops; nothing may settle on the first FullSync")
	}
	for i := 0; i < 9; i++ {
		if _, err := svc.IncrementalSync(ctx, repo, 7, f, Marks{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := svc.FullSync(ctx, repo, 7, f); err != nil {
		t.Fatal(err)
	}
	if db.recorded != target || db.settled != target {
		t.Fatalf("in_flight op on page 2 not settled by the second FullSync: recorded=%s settled=%s", db.recorded, db.settled)
	}
}

// A returned_uncertain unrecorded op is matchable exactly like in_flight.
func TestFullSyncMatchesReturnedUncertainOp(t *testing.T) {
	repo := uuid.New()
	op := store.FindingGroupClaimOperation{ID: uuid.New(), UserID: uuid.New(), RepoID: repo, Phase: "returned_uncertain", CreatedAt: time.Now().Add(-time.Minute)}
	db := &markerPageDB{pendingPageDB: &pendingPageDB{ops: []store.FindingGroupClaimOperation{op}}}
	svc := newTestService(&fakeStore{})
	svc.SetFindingGroupDB(db)
	f := &fakeForge{allIssues: []forge.Issue{{
		IID: 41, WebURL: "https://example.com/41", UpdatedAt: time.Now(),
		Description: "<!-- uzi-finding-group-operation: " + op.ID.String() + " -->",
	}}}
	if _, err := svc.FullSync(context.Background(), repo, 7, f); err != nil {
		t.Fatal(err)
	}
	if db.recorded != op.ID || db.settled != op.ID || db.candidateQueries != 1 {
		t.Fatalf("returned_uncertain op: recorded=%s settled=%s candidate queries=%d", db.recorded, db.settled, db.candidateQueries)
	}
}
