package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// issue_tracker_state_mapping_livedb_test.go proves migration 00508 (PRD #4173 M5) and its
// queries against a real Postgres on an isolated database: the name and pair CHECKs, the
// one-column-per-State uniqueness, the cascades, the owner-and-linked scoping of the upsert
// and the lock, the resolver row's new columns, the prune and the issues column.
// Skipped without UZI_TEST_DATABASE_URL.

const checkViolation = "23514"
const uniqueViolation = "23505"
const foreignKeyViolation = "23503"

type stateMappingFixture struct {
	ctx            context.Context
	q              *store.Queries
	pool           *pgxpool.Pool
	user, stranger uuid.UUID
	linked, plain  uuid.UUID
}

func newStateMappingFixture(t *testing.T) (*stateMappingFixture, *store.Queries, func(sql string, args ...any)) {
	t.Helper()
	ctx, dsn := standIsolatedDB(t, "state_mapping_")
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	exec := func(sql string, args ...any) {
		t.Helper()
		itlExec(t, pool, sql, args...)
	}
	f := &stateMappingFixture{ctx: ctx, q: store.New(pool), pool: pool,
		user: uuid.New(), stranger: uuid.New(), linked: uuid.New(), plain: uuid.New()}
	forgeConn, itc := uuid.New(), uuid.New()
	for _, u := range []uuid.UUID{f.user, f.stranger} {
		exec(`INSERT INTO users (id,email,password_hash) VALUES ($1,$2,'x')`, u, u.String()+"@example.test")
	}
	exec(`INSERT INTO forge_connections (id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext)
		VALUES ($1,$2,'github','https://example.com','bot',1,'x')`, forgeConn, f.user)
	exec(`INSERT INTO issue_tracker_connections (id,user_id,kind,base_url,account_id,account_login,token_ciphertext)
		VALUES ($1,$2,'youtrack','https://yt.example.com','a','al','x')`, itc, f.user)
	exec(`INSERT INTO repos (id,connection_id,forge_project_id,path_with_namespace,web_url,enabled,awaiting_first_enable,
	        issue_tracker_connection_id,issue_tracker_project_id,issue_tracker_project_short,eligibility_tag_id,eligibility_tag_name,issue_tracker_linked_at)
	      VALUES ($1,$2,1,'o/linked','https://example.com/o/linked',true,false,$3,'0-1','UZI','5-1','uzi',now())`, f.linked, forgeConn, itc)
	exec(`INSERT INTO repos (id,connection_id,forge_project_id,path_with_namespace,web_url,enabled)
		VALUES ($1,$2,2,'o/plain','https://example.com/o/plain',true)`, f.plain, forgeConn)
	return f, f.q, exec
}

func (f *stateMappingFixture) upsert(t *testing.T, repo, user uuid.UUID, open, closed string) error {
	t.Helper()
	_, err := f.q.UpsertRepoIssueTrackerStates(f.ctx, store.UpsertRepoIssueTrackerStatesParams{OpenState: open, ClosedState: closed, RepoID: repo, UserID: user})
	return err
}

func TestIssueTrackerStateMappingChecksLiveDB(t *testing.T) {
	f, q, exec := newStateMappingFixture(t)

	bad := []struct{ name, open, closed string }{
		{"empty open", "", "Fixed"},
		{"empty closed", "Open", ""},
		{"too long", strings.Repeat("x", 201), "Fixed"},
		{"control character", "Op\ten", "Fixed"},
		{"newline", "Open", "Fi\nxed"},
		{"same pair", "Open", "Open"},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			if err := f.upsert(t, f.linked, f.user, tc.open, tc.closed); sqlState(err) != checkViolation {
				t.Fatalf("err = %v (state %q), want a check violation", err, sqlState(err))
			}
		})
	}
	// The bounds are inclusive: 200 characters, counted as characters and not bytes.
	if err := f.upsert(t, f.linked, f.user, strings.Repeat("é", 200), "Fixed"); err != nil {
		t.Fatalf("a 200-character name: %v", err)
	}
	if err := f.upsert(t, f.linked, f.user, "Open", "Fixed"); err != nil {
		t.Fatal(err)
	}

	col := func(label, state string) error {
		return q.InsertRepoIssueTrackerColumnState(f.ctx, store.InsertRepoIssueTrackerColumnStateParams{RepoID: f.linked, LabelName: label, StateName: state})
	}
	if err := col("In Progress", "Doing"); err != nil {
		t.Fatal(err)
	}
	if err := col("Review", "Doing"); sqlState(err) != uniqueViolation {
		t.Errorf("a State mapped to two columns: %v, want a unique violation", err)
	}
	if err := col("In Progress", "Other"); sqlState(err) != uniqueViolation {
		t.Errorf("a column mapped twice: %v, want a unique violation", err)
	}
	if err := col("Review", "Do\x07ing"); sqlState(err) != checkViolation {
		t.Errorf("a control character in a column State: %v, want a check violation", err)
	}
	if err := col("Review", strings.Repeat("y", 201)); sqlState(err) != checkViolation {
		t.Errorf("an overlong column State: %v, want a check violation", err)
	}
	if err := col("Re\tview", "Reviewing"); sqlState(err) != checkViolation {
		t.Errorf("a control character in a column label: %v, want a check violation", err)
	}
	if err := col(strings.Repeat("y", 201), "Reviewing"); sqlState(err) != checkViolation {
		t.Errorf("an overlong column label: %v, want a check violation", err)
	}
	if err := col("", "Reviewing"); sqlState(err) != checkViolation {
		t.Errorf("an empty column label: %v, want a check violation", err)
	}
	// A column row needs the states row.
	if err := q.InsertRepoIssueTrackerColumnState(f.ctx, store.InsertRepoIssueTrackerColumnStateParams{RepoID: uuid.New(), LabelName: "a", StateName: "b"}); sqlState(err) != foreignKeyViolation {
		t.Errorf("a column row with no states row: %v, want a foreign key violation", err)
	}

	// issues.issue_tracker_column is nullable and bounded.
	exec(`INSERT INTO issues (repo_id, forge_issue_iid, title, state, labels, web_url, forge_updated_at, synced_at)
	      VALUES ($1, 1, 't', 'opened', '[]', 'https://example.com/i', now(), now())`, f.linked)
	var isNull bool
	if err := f.pool.QueryRow(f.ctx, `SELECT issue_tracker_column IS NULL FROM issues WHERE repo_id = $1`, f.linked).Scan(&isNull); err != nil || !isNull {
		t.Fatalf("issue_tracker_column null = %v (%v), want a NULL default", isNull, err)
	}
}

func TestIssueTrackerStateMappingQueriesLiveDB(t *testing.T) {
	f, q, exec := newStateMappingFixture(t)
	exec(`INSERT INTO board_columns (repo_id,label_name,position) VALUES ($1,'In Progress',0),($1,'Review',1),($1,'Done',2)`, f.linked)

	read := func() store.GetRepoIssueSourceForUserRow {
		t.Helper()
		row, err := q.GetRepoIssueSourceForUser(f.ctx, store.GetRepoIssueSourceForUserParams{ID: f.linked, UserID: f.user})
		if err != nil {
			t.Fatal(err)
		}
		return row
	}
	// No mapping yet: the optional values are NULL and the column map is empty.
	if row := read(); row.OpenState.Valid || row.ClosedState.Valid || string(row.ColumnStates) != "{}" {
		t.Fatalf("resolver row before a mapping = %+v", row)
	}

	// The upsert is owner-scoped and needs a linked repo.
	if err := f.upsert(t, f.linked, f.stranger, "Open", "Fixed"); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("upsert as a stranger: %v, want no rows", err)
	}
	if err := f.upsert(t, f.plain, f.user, "Open", "Fixed"); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("upsert on an unlinked repo: %v, want no rows", err)
	}
	if _, err := q.LockRepoForStateMappingForUser(f.ctx, store.LockRepoForStateMappingForUserParams{ID: f.linked, UserID: f.stranger}); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("lock as a stranger: %v, want no rows", err)
	}
	if lock, err := q.LockRepoForStateMappingForUser(f.ctx, store.LockRepoForStateMappingForUserParams{ID: f.plain, UserID: f.user}); err != nil || lock.IssueTrackerConnectionID.Valid {
		t.Errorf("lock of an unlinked repo = %+v (%v), want the row, unlinked", lock, err)
	}

	if err := f.upsert(t, f.linked, f.user, "Open", "Fixed"); err != nil {
		t.Fatal(err)
	}
	for label, state := range map[string]string{"In Progress": "Doing", "Review": "In Review"} {
		if err := q.InsertRepoIssueTrackerColumnState(f.ctx, store.InsertRepoIssueTrackerColumnStateParams{RepoID: f.linked, LabelName: label, StateName: state}); err != nil {
			t.Fatal(err)
		}
	}
	row := read()
	var cols map[string]string
	if err := json.Unmarshal(row.ColumnStates, &cols); err != nil {
		t.Fatal(err)
	}
	if row.OpenState.String != "Open" || row.ClosedState.String != "Fixed" || len(cols) != 2 || cols["In Progress"] != "Doing" || cols["Review"] != "In Review" {
		t.Fatalf("resolver row = open %q closed %q columns %v", row.OpenState.String, row.ClosedState.String, cols)
	}
	// The stranger still sees nothing of it.
	if _, err := q.GetRepoIssueSourceForUser(f.ctx, store.GetRepoIssueSourceForUserParams{ID: f.linked, UserID: f.stranger}); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("resolver row as a stranger: %v, want no rows", err)
	}

	// A second upsert replaces the pair and keeps the column rows.
	if err := f.upsert(t, f.linked, f.user, "Doing", "Done"); err != nil {
		t.Fatal(err)
	}
	if row := read(); row.OpenState.String != "Doing" || row.ClosedState.String != "Done" || !strings.Contains(string(row.ColumnStates), "Review") {
		t.Fatalf("after a second upsert: %+v", row)
	}

	// Prune keeps the listed labels only; an empty keep list removes every row.
	if err := q.PruneRepoIssueTrackerColumnStates(f.ctx, store.PruneRepoIssueTrackerColumnStatesParams{RepoID: f.linked, Keep: []string{"Review", "Backlog"}}); err != nil {
		t.Fatal(err)
	}
	if got := string(read().ColumnStates); got != `{"Review": "In Review"}` {
		t.Fatalf("after pruning to Review: %s", got)
	}
	if err := q.PruneRepoIssueTrackerColumnStates(f.ctx, store.PruneRepoIssueTrackerColumnStatesParams{RepoID: f.linked, Keep: []string{}}); err != nil {
		t.Fatal(err)
	}
	if got := string(read().ColumnStates); got != "{}" {
		t.Fatalf("after pruning to nothing: %s", got)
	}

	// Delete then insert is the replace the route does; the states row survives it.
	if err := q.InsertRepoIssueTrackerColumnState(f.ctx, store.InsertRepoIssueTrackerColumnStateParams{RepoID: f.linked, LabelName: "Done", StateName: "Shipped"}); err != nil {
		t.Fatal(err)
	}
	if err := q.DeleteRepoIssueTrackerColumnStates(f.ctx, f.linked); err != nil {
		t.Fatal(err)
	}
	if row := read(); string(row.ColumnStates) != "{}" || !row.OpenState.Valid {
		t.Fatalf("after deleting the column rows: %+v", row)
	}
}

func TestIssueTrackerStateMappingCascadesLiveDB(t *testing.T) {
	f, q, exec := newStateMappingFixture(t)
	if err := f.upsert(t, f.linked, f.user, "Open", "Fixed"); err != nil {
		t.Fatal(err)
	}
	if err := q.InsertRepoIssueTrackerColumnState(f.ctx, store.InsertRepoIssueTrackerColumnStateParams{RepoID: f.linked, LabelName: "a", StateName: "b"}); err != nil {
		t.Fatal(err)
	}
	count := func(table string) int {
		t.Helper()
		var n int
		if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// Deleting the states row takes its column rows with it.
	exec(`DELETE FROM repo_issue_tracker_states WHERE repo_id = $1`, f.linked)
	if n := count("repo_issue_tracker_column_states"); n != 0 {
		t.Fatalf("column rows after the states row is gone = %d, want 0", n)
	}
	// Deleting the repo takes the whole mapping.
	if err := f.upsert(t, f.linked, f.user, "Open", "Fixed"); err != nil {
		t.Fatal(err)
	}
	if err := q.InsertRepoIssueTrackerColumnState(f.ctx, store.InsertRepoIssueTrackerColumnStateParams{RepoID: f.linked, LabelName: "a", StateName: "b"}); err != nil {
		t.Fatal(err)
	}
	exec(`DELETE FROM repos WHERE id = $1`, f.linked)
	if s, c := count("repo_issue_tracker_states"), count("repo_issue_tracker_column_states"); s != 0 || c != 0 {
		t.Fatalf("after the repo is deleted: %d states rows, %d column rows, want none", s, c)
	}
}
