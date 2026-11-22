package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// issue_tracker_repo_link_livedb_test.go proves migration 00474 (PRD #4173 M4) against a
// real Postgres on an isolated database: the never-enabled marker and its trigger, the
// issue-number guard on every table that stores an issue number, the issue_key and link
// CHECKs, the connection FK, and the link queries. Skipped without UZI_TEST_DATABASE_URL.

const issueNumberGuardSQLState = "UZ413"

func sqlState(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

func itlExec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

func itlLinked(t *testing.T, pool *pgxpool.Pool, repo uuid.UUID) bool {
	t.Helper()
	var b bool
	if err := pool.QueryRow(context.Background(), `SELECT issue_tracker_connection_id IS NOT NULL FROM repos WHERE id=$1`, repo).Scan(&b); err != nil {
		t.Fatal(err)
	}
	return b
}

func itlAwaiting(t *testing.T, pool *pgxpool.Pool, repo uuid.UUID) bool {
	t.Helper()
	var b bool
	if err := pool.QueryRow(context.Background(), `SELECT awaiting_first_enable FROM repos WHERE id=$1`, repo).Scan(&b); err != nil {
		t.Fatal(err)
	}
	return b
}

// itlFixture is a user with a forge connection, an issue tracker connection, a legacy repo
// (inserted before 00474) and the ids a test needs.
type itlFixture struct {
	user, forgeConn, itc, legacy uuid.UUID
}

func itlMigrateWithLegacyRepo(t *testing.T) (context.Context, string, *pgxpool.Pool, itlFixture) {
	t.Helper()
	ctx, dsn := standIsolatedDB(t, "issue_tracker_link_")
	if err := store.MigrateTo(ctx, dsn, 473); err != nil {
		t.Fatalf("migrate to 473: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	f := itlFixture{user: uuid.New(), forgeConn: uuid.New(), itc: uuid.New(), legacy: uuid.New()}
	itlExec(t, pool, `INSERT INTO users (id,email,password_hash) VALUES ($1,$2,'x')`, f.user, f.user.String()+"@example.test")
	itlExec(t, pool, `INSERT INTO forge_connections (id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext)
		VALUES ($1,$2,'github','https://example.com','bot',1,'x')`, f.forgeConn, f.user)
	itlExec(t, pool, `INSERT INTO issue_tracker_connections (id,user_id,kind,base_url,account_id,account_login,token_ciphertext)
		VALUES ($1,$2,'youtrack','https://yt.example.com','a','al','x')`, f.itc, f.user)
	itlExec(t, pool, `INSERT INTO repos (id,connection_id,forge_project_id,path_with_namespace,web_url,enabled)
		VALUES ($1,$2,1,'o/legacy','https://example.com/o/legacy',false)`, f.legacy, f.forgeConn)
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate to head: %v", err)
	}
	return ctx, dsn, pool, f
}

func TestIssueTrackerRepoLinkMigrationLiveDB(t *testing.T) {
	ctx, _, pool, f := itlMigrateWithLegacyRepo(t)
	q := store.New(pool)

	t.Run("marker", func(t *testing.T) {
		if itlAwaiting(t, pool, f.legacy) {
			t.Fatal("a repo that existed before the migration must count as previously enabled")
		}
		repo, err := q.UpsertRepo(ctx, store.UpsertRepoParams{ConnectionID: f.forgeConn, ForgeProjectID: 2, PathWithNamespace: "o/new", WebUrl: "https://example.com/o/new"})
		if err != nil {
			t.Fatal(err)
		}
		if !repo.AwaitingFirstEnable || !itlAwaiting(t, pool, repo.ID) {
			t.Fatal("UpsertRepo must mark a newly discovered repo awaiting_first_enable")
		}
		again, err := q.UpsertRepo(ctx, store.UpsertRepoParams{ConnectionID: f.forgeConn, ForgeProjectID: 2, PathWithNamespace: "o/new2", WebUrl: "https://example.com/o/new"})
		if err != nil || again.ID != repo.ID {
			t.Fatalf("conflict upsert: %v", err)
		}
		if !itlAwaiting(t, pool, repo.ID) {
			t.Fatal("the ON CONFLICT branch must not touch awaiting_first_enable")
		}
		itlExec(t, pool, `UPDATE repos SET enabled=true WHERE id=$1`, repo.ID)
		if itlAwaiting(t, pool, repo.ID) {
			t.Fatal("enabling must clear the marker in the same statement")
		}
		again, err = q.UpsertRepo(ctx, store.UpsertRepoParams{ConnectionID: f.forgeConn, ForgeProjectID: 2, PathWithNamespace: "o/new3", WebUrl: "https://example.com/o/new"})
		if err != nil || again.AwaitingFirstEnable {
			t.Fatalf("a re-listing must not re-mark an enabled repo: %v", err)
		}
		itlExec(t, pool, `UPDATE repos SET enabled=false WHERE id=$1`, repo.ID)
		if itlAwaiting(t, pool, repo.ID) {
			t.Fatal("disabling must not re-mark")
		}
		_, err = pool.Exec(ctx, `UPDATE repos SET awaiting_first_enable=true WHERE id=$1`, repo.ID)
		var pe *pgconn.PgError
		if !errors.As(err, &pe) || !strings.Contains(pe.Message, "is never set back once cleared") {
			t.Fatalf("setting the marker back must raise the trigger's refusal, got %v", err)
		}
		// Enabling and setting it in one statement still ends cleared, never raises.
		itlExec(t, pool, `UPDATE repos SET enabled=true, awaiting_first_enable=true WHERE id=$1`, repo.ID)
		if itlAwaiting(t, pool, repo.ID) {
			t.Fatal("an enabled row must never keep the marker")
		}
		// A plain insert with the marker and enabled both true is cleared too.
		var ins bool
		if err := pool.QueryRow(ctx, `INSERT INTO repos (connection_id,forge_project_id,path_with_namespace,web_url,enabled,awaiting_first_enable)
			VALUES ($1,99,'o/x','https://example.com/o/x',true,true) RETURNING awaiting_first_enable`, f.forgeConn).Scan(&ins); err != nil || ins {
			t.Fatalf("insert enabled+awaiting: awaiting=%v err=%v", ins, err)
		}
	})

	t.Run("guard", func(t *testing.T) {
		aw, err := q.UpsertRepo(ctx, store.UpsertRepoParams{ConnectionID: f.forgeConn, ForgeProjectID: 10, PathWithNamespace: "o/aw", WebUrl: "https://example.com/o/aw"})
		if err != nil {
			t.Fatal(err)
		}
		// A run on the (enabled-once) legacy repo supplies the FK parents.
		runID, reviewID := uuid.New(), uuid.New()
		itlExec(t, pool, `INSERT INTO runs (id,user_id,repo_id,issue_iid,issue_title,issue_description) VALUES ($1,$2,$3,1,'t','d')`, runID, f.user, f.legacy)
		itlExec(t, pool, `INSERT INTO run_reviews (id,target_run_id,user_id,verdict) VALUES ($1,$2,$3,'issues')`, reviewID, runID, f.user)

		inserts := []struct {
			table string
			sql   string
			args  func(repo uuid.UUID) []any
		}{
			{"issues", `INSERT INTO issues (repo_id,forge_issue_iid,title,state,web_url,forge_updated_at,synced_at) VALUES ($1,5,'t','opened','u',now(),now())`, func(r uuid.UUID) []any { return []any{r} }},
			{"runs", `INSERT INTO runs (user_id,repo_id,issue_iid,issue_title,issue_description) VALUES ($1,$2,5,'t','d')`, func(r uuid.UUID) []any { return []any{f.user, r} }},
			{"run_schedules", `INSERT INTO run_schedules (user_id,repo_id,target,issue_iid,timing,run_at) VALUES ($1,$2,'issue',5,'once',now())`, func(r uuid.UUID) []any { return []any{f.user, r} }},
			{"autopilot_triggers", `INSERT INTO autopilot_triggers (repo_id,issue_iid,last_event_id) VALUES ($1,5,1)`, func(r uuid.UUID) []any { return []any{r} }},
			{"github_project_items", `INSERT INTO github_project_items (repo_id,forge_issue_iid,item_node_id) VALUES ($1,5,'PVTI_x')`, func(r uuid.UUID) []any { return []any{r} }},
			{"issue_proposals", `INSERT INTO issue_proposals (run_id,repo_id,title,description,created_issue_iid) VALUES ($1,$2,'t','d',5)`, func(r uuid.UUID) []any { return []any{runID, r} }},
			{"recommendation_filed_issues", `INSERT INTO recommendation_filed_issues (review_id,category,filed_repo_id,filed_issue_iid) VALUES ($1,'c',$2,5)`, func(r uuid.UUID) []any { return []any{reviewID, r} }},
			{"finding_dispositions", `INSERT INTO finding_dispositions (user_id,repo_id,location,filed_issue_iid) VALUES ($1,$2,'loc',5)`, func(r uuid.UUID) []any { return []any{f.user, r} }},
			{"finding_group_operations", `INSERT INTO finding_group_operations (user_id,repo_id,deadline_at,issue_iid,issue_url) VALUES ($1,$2,now(),5,'u')`, func(r uuid.UUID) []any { return []any{f.user, r} }},
			// Issue #4512: the one-time fire ledger. Each execution makes its own schedule and header,
			// so the list can run twice (before and after the repo is enabled).
			{"schedule_once_fire_runs", `WITH s AS (INSERT INTO run_schedules (user_id,repo_id,target,prompt,timing,run_at) VALUES ($1,$2,'prompt','p','once',now()) RETURNING id),
				h AS (INSERT INTO schedule_once_fires (schedule_id,fire_epoch) SELECT id,0 FROM s RETURNING schedule_id)
				INSERT INTO schedule_once_fire_runs (schedule_id,fire_epoch,repo_id,issue_iid,issue_title,web_url,run_uuid) SELECT schedule_id,0,$2,5,'t','u',gen_random_uuid() FROM h`, func(r uuid.UUID) []any { return []any{f.user, r} }},
		}
		for _, in := range inserts {
			if _, err := pool.Exec(ctx, in.sql, in.args(aw.ID)...); sqlState(err) != issueNumberGuardSQLState {
				t.Errorf("%s: insert on an awaiting repo: want SQLSTATE %s, got %v", in.table, issueNumberGuardSQLState, err)
			}
		}
		// A row with no issue number is not guarded: a prompt schedule and a NULL-iid finding.
		itlExec(t, pool, `INSERT INTO run_schedules (user_id,repo_id,target,prompt,timing,run_at) VALUES ($1,$2,'prompt','p','once',now())`, f.user, aw.ID)
		itlExec(t, pool, `INSERT INTO finding_dispositions (user_id,repo_id,location) VALUES ($1,$2,'nonum')`, f.user, aw.ID)
		// A NULL filed_repo_id is skipped.
		itlExec(t, pool, `INSERT INTO recommendation_filed_issues (review_id,category,filed_issue_iid) VALUES ($1,'norepo',5)`, reviewID)

		stores, err := q.RepoStoresIssueNumber(ctx, aw.ID)
		if err != nil || stores {
			t.Fatalf("RepoStoresIssueNumber on an awaiting repo: %v %v", stores, err)
		}

		// UPDATE OF the repo id column: moving an issue onto the awaiting repo is refused.
		itlExec(t, pool, `INSERT INTO issues (repo_id,forge_issue_iid,title,state,web_url,forge_updated_at,synced_at) VALUES ($1,7,'t','opened','u',now(),now())`, f.legacy)
		if _, err := pool.Exec(ctx, `UPDATE issues SET repo_id=$1 WHERE repo_id=$2 AND forge_issue_iid=7`, aw.ID, f.legacy); sqlState(err) != issueNumberGuardSQLState {
			t.Errorf("moving an issue to an awaiting repo: want %s, got %v", issueNumberGuardSQLState, err)
		}
		// UPDATE OF repo_id on a run that carries an issue number.
		if _, err := pool.Exec(ctx, `UPDATE runs SET repo_id=$1 WHERE id=$2`, aw.ID, runID); sqlState(err) != issueNumberGuardSQLState {
			t.Errorf("moving a run to an awaiting repo: want %s, got %v", issueNumberGuardSQLState, err)
		}

		itlExec(t, pool, `UPDATE repos SET enabled=true WHERE id=$1`, aw.ID)
		for _, in := range inserts {
			if _, err := pool.Exec(ctx, in.sql, in.args(aw.ID)...); err != nil {
				t.Errorf("%s: insert after the repo was enabled: %v", in.table, err)
			}
		}
		stores, err = q.RepoStoresIssueNumber(ctx, aw.ID)
		if err != nil || !stores {
			t.Fatalf("RepoStoresIssueNumber after inserts: %v %v", stores, err)
		}
	})

	t.Run("once_fire_ledger_only", func(t *testing.T) {
		// Pins the schedule_once_fire_runs clause of RepoStoresIssueNumber on its own: the only
		// stored issue number for this repo is one ledger row. Its parent schedule is a prompt
		// schedule (no issue_iid) and the repo has no issues or runs rows, so no other clause
		// can report true.
		repo, err := q.UpsertRepo(ctx, store.UpsertRepoParams{ConnectionID: f.forgeConn, ForgeProjectID: 50, PathWithNamespace: "o/ledger", WebUrl: "https://example.com/o/ledger"})
		if err != nil {
			t.Fatal(err)
		}
		if stores, err := q.RepoStoresIssueNumber(ctx, repo.ID); err != nil || stores {
			t.Fatalf("RepoStoresIssueNumber on an empty repo: %v %v", stores, err)
		}
		// The guard refuses a ledger insert while the repo awaits its first enable.
		itlExec(t, pool, `UPDATE repos SET enabled=true WHERE id=$1`, repo.ID)
		itlExec(t, pool, `WITH s AS (INSERT INTO run_schedules (user_id,repo_id,target,prompt,timing,run_at) VALUES ($1,$2,'prompt','p','once',now()) RETURNING id),
			h AS (INSERT INTO schedule_once_fires (schedule_id,fire_epoch) SELECT id,0 FROM s RETURNING schedule_id)
			INSERT INTO schedule_once_fire_runs (schedule_id,fire_epoch,repo_id,issue_iid,issue_title,web_url,run_uuid) SELECT schedule_id,0,$2,5,'t','u',gen_random_uuid() FROM h`, f.user, repo.ID)
		stores, err := q.RepoStoresIssueNumber(ctx, repo.ID)
		if err != nil || !stores {
			t.Fatalf("RepoStoresIssueNumber with only a once-fire ledger row: %v %v", stores, err)
		}
	})

	t.Run("continued_issue_iid", func(t *testing.T) {
		// A task run continues an issue through continued_issue_iid while issue_iid stays
		// NULL (runs_continued_issue_shape), so this column alone carries the number.
		repo, err := q.UpsertRepo(ctx, store.UpsertRepoParams{ConnectionID: f.forgeConn, ForgeProjectID: 40, PathWithNamespace: "o/cont", WebUrl: "https://example.com/o/cont"})
		if err != nil {
			t.Fatal(err)
		}
		const ins = `INSERT INTO runs (user_id,repo_id,kind,branch,continued_issue_iid,issue_title,issue_description) VALUES ($1,$2,'task','b',5,'t','d')`
		if _, err := pool.Exec(ctx, ins, f.user, repo.ID); sqlState(err) != issueNumberGuardSQLState {
			t.Fatalf("a task run continuing an issue on an awaiting repo: want %s, got %v", issueNumberGuardSQLState, err)
		}
		itlExec(t, pool, `UPDATE repos SET enabled=true WHERE id=$1`, repo.ID)
		itlExec(t, pool, ins, f.user, repo.ID)
	})

	t.Run("number_set_by_update", func(t *testing.T) {
		// Insert with a NULL number (allowed), then UPDATE the number onto the awaiting repo.
		aw, err := q.UpsertRepo(ctx, store.UpsertRepoParams{ConnectionID: f.forgeConn, ForgeProjectID: 41, PathWithNamespace: "o/upd", WebUrl: "https://example.com/o/upd"})
		if err != nil {
			t.Fatal(err)
		}
		runID := uuid.New()
		itlExec(t, pool, `INSERT INTO runs (id,user_id,repo_id,issue_iid,issue_title,issue_description) VALUES ($1,$2,$3,2,'t','d')`, runID, f.user, f.legacy)
		reviewID := uuid.New()
		itlExec(t, pool, `INSERT INTO run_reviews (id,target_run_id,user_id,verdict) VALUES ($1,$2,$3,'issues')`, reviewID, runID, f.user)
		for _, c := range []struct {
			table, insert, update string
			args                  []any
		}{
			{"issue_proposals", `INSERT INTO issue_proposals (id,run_id,repo_id,title,description) VALUES ($1,$2,$3,'t','d')`, `UPDATE issue_proposals SET created_issue_iid=5 WHERE id=$1`, []any{runID, aw.ID}},
			{"finding_dispositions", `INSERT INTO finding_dispositions (id,user_id,repo_id,location) VALUES ($1,$2,$3,'upd')`, `UPDATE finding_dispositions SET filed_issue_iid=5 WHERE id=$1`, []any{f.user, aw.ID}},
			{"finding_group_operations", `INSERT INTO finding_group_operations (id,user_id,repo_id,deadline_at) VALUES ($1,$2,$3,now())`, `UPDATE finding_group_operations SET issue_iid=5, issue_url='u' WHERE id=$1`, []any{f.user, aw.ID}},
			{"recommendation_filed_issues", `INSERT INTO recommendation_filed_issues (id,review_id,category,filed_repo_id) VALUES ($1,$2,'upd',$3)`, `UPDATE recommendation_filed_issues SET filed_issue_iid=5 WHERE id=$1`, []any{reviewID, aw.ID}},
		} {
			id := uuid.New()
			itlExec(t, pool, c.insert, append([]any{id}, c.args...)...)
			if _, err := pool.Exec(ctx, c.update, id); sqlState(err) != issueNumberGuardSQLState {
				t.Errorf("%s: UPDATE setting a number on an awaiting repo: want %s, got %v", c.table, issueNumberGuardSQLState, err)
			}
		}
	})

	t.Run("guard_fails_closed_on_missing_column", func(t *testing.T) {
		// A trigger argument naming no column of the row must raise, never pass silently.
		itlExec(t, pool, `CREATE TABLE itl_guard_probe (id int, repo_id uuid, num bigint)`)
		itlExec(t, pool, `CREATE TRIGGER itl_probe_bad_num BEFORE INSERT ON itl_guard_probe
			FOR EACH ROW EXECUTE FUNCTION issue_number_awaiting_first_enable_guard('repo_id', 'no_such_num')`)
		_, err := pool.Exec(ctx, `INSERT INTO itl_guard_probe (id) VALUES (1)`)
		var pe *pgconn.PgError
		if !errors.As(err, &pe) || !strings.Contains(pe.Message, "no column no_such_num") {
			t.Errorf("missing number column: want the guard's refusal, got %v", err)
		}
		itlExec(t, pool, `DROP TRIGGER itl_probe_bad_num ON itl_guard_probe`)
		itlExec(t, pool, `CREATE TRIGGER itl_probe_bad_repo BEFORE INSERT ON itl_guard_probe
			FOR EACH ROW EXECUTE FUNCTION issue_number_awaiting_first_enable_guard('no_such_repo', 'num')`)
		_, err = pool.Exec(ctx, `INSERT INTO itl_guard_probe (id) VALUES (1)`)
		if !errors.As(err, &pe) || !strings.Contains(pe.Message, "no column no_such_repo") {
			t.Errorf("missing repo column: want the guard's refusal, got %v", err)
		}
		itlExec(t, pool, `DROP TABLE itl_guard_probe`)
	})

	t.Run("issue_key", func(t *testing.T) {
		repo := f.legacy
		ins := func(iid int64, key any) error {
			_, err := pool.Exec(ctx, `INSERT INTO issues (repo_id,forge_issue_iid,issue_key,title,state,web_url,forge_updated_at,synced_at) VALUES ($1,$2,$3,'t','opened','u',now(),now())`, repo, iid, key)
			return err
		}
		if err := ins(2, "ABC-12"); sqlState(err) != "23514" {
			t.Errorf("ABC-12 on iid 2 must be refused by the CHECK: %v", err)
		}
		if err := ins(12, "ABC-12"); err != nil {
			t.Errorf("ABC-12 on iid 12: %v", err)
		}
		if err := ins(13, nil); err != nil {
			t.Errorf("NULL key: %v", err)
		}
		for i, bad := range []string{"abc12", "ABC-0", "ABC-012", "1BC-14", "ABC-14 ", "A-B-14"} {
			if err := ins(14, bad); sqlState(err) != "23514" {
				t.Errorf("key %q (%d) must be refused: %v", bad, i, err)
			}
		}
		if err := ins(20, "A_1-20"); err != nil {
			t.Errorf("A_1-20: %v", err)
		}
	})

	t.Run("link_shape_and_fk", func(t *testing.T) {
		repo, err := q.UpsertRepo(ctx, store.UpsertRepoParams{ConnectionID: f.forgeConn, ForgeProjectID: 30, PathWithNamespace: "o/lk", WebUrl: "https://example.com/o/lk"})
		if err != nil {
			t.Fatal(err)
		}
		// Partial link refused.
		if _, err := pool.Exec(ctx, `UPDATE repos SET issue_tracker_connection_id=$2 WHERE id=$1`, repo.ID, f.itc); sqlState(err) != "23514" {
			t.Errorf("partial link must violate the all-or-none CHECK: %v", err)
		}
		full := `UPDATE repos SET issue_tracker_connection_id=$2, issue_tracker_project_id=$3, issue_tracker_project_short=$4,
			eligibility_tag_id=$5, eligibility_tag_name=$6, issue_tracker_linked_at=now() WHERE id=$1`
		for name, args := range map[string][]any{
			"control char in tag name": {repo.ID, f.itc, "0-1", "ABC", "9-1", "bad\x01name"},
			"bad short":                {repo.ID, f.itc, "0-1", "1ABC", "9-1", "tag"},
			"short with dash":          {repo.ID, f.itc, "0-1", "A-B", "9-1", "tag"},
			"empty project id":         {repo.ID, f.itc, "", "ABC", "9-1", "tag"},
			"long tag name":            {repo.ID, f.itc, "0-1", "ABC", "9-1", strings.Repeat("x", 257)},
			"long project id":          {repo.ID, f.itc, strings.Repeat("x", 129), "ABC", "9-1", "tag"},
		} {
			if _, err := pool.Exec(ctx, full, args...); sqlState(err) != "23514" {
				t.Errorf("%s must be refused by the CHECK: %v", name, err)
			}
		}

		// issue_key is 64 wide (checked on the legacy repo, which the guard lets store a number): a 44-character short plus "-" plus 19 digits is the widest key.
		bound := func(short string) error {
			_, err := pool.Exec(ctx, full, repo.ID, f.itc, "0-1", short, "9-1", "tag")
			return err
		}
		if err := bound("A" + strings.Repeat("b", 45)); sqlState(err) != "23514" {
			t.Errorf("a 46-character short must be refused: %v", err)
		}
		if err := bound("A" + strings.Repeat("b", 44)); sqlState(err) != "23514" {
			t.Errorf("a 45-character short must be refused: %v", err)
		}
		long := "A" + strings.Repeat("b", 43)
		if err := bound(long); err != nil {
			t.Fatalf("a 44-character short must link: %v", err)
		}
		itlExec(t, pool, `INSERT INTO issues (repo_id,forge_issue_iid,issue_key,title,state,web_url,forge_updated_at,synced_at) VALUES ($1,9223372036854775807,$2,'t','opened','u',now(),now())`, f.legacy, long+"-9223372036854775807")
		itlExec(t, pool, `DELETE FROM issues WHERE issue_key=$1`, long+"-9223372036854775807")
		itlExec(t, pool, `UPDATE repos SET issue_tracker_connection_id=NULL, issue_tracker_project_id=NULL, issue_tracker_project_short=NULL,
			eligibility_tag_id=NULL, eligibility_tag_name=NULL, issue_tracker_linked_at=NULL WHERE id=$1`, repo.ID)

		// Link queries.
		src, err := q.GetRepoIssueSourceForUser(ctx, store.GetRepoIssueSourceForUserParams{ID: repo.ID, UserID: f.user})
		if err != nil || src.LinkedConnectionID.Valid {
			t.Fatalf("source before link: %+v %v", src, err)
		}
		other := uuid.New()
		itlExec(t, pool, `INSERT INTO users (id,email,password_hash) VALUES ($1,$2,'x')`, other, other.String()+"@example.test")
		if _, err := q.GetRepoIssueSourceForUser(ctx, store.GetRepoIssueSourceForUserParams{ID: repo.ID, UserID: other}); !errors.Is(err, pgx.ErrNoRows) {
			t.Errorf("GetRepoIssueSourceForUser must be owner-scoped: %v", err)
		}
		if _, err := q.LockRepoForLinkForUser(ctx, store.LockRepoForLinkForUserParams{ID: repo.ID, UserID: other}); !errors.Is(err, pgx.ErrNoRows) {
			t.Errorf("LockRepoForLinkForUser must be owner-scoped: %v", err)
		}
		// A cross-user connection or a foreign repo yields no row and writes nothing.
		otherITC := uuid.New()
		itlExec(t, pool, `INSERT INTO issue_tracker_connections (id,user_id,kind,base_url,account_id,account_login,token_ciphertext)
			VALUES ($1,$2,'youtrack','https://yt2.example.com','b','bl','x')`, otherITC, other)
		if _, err := q.SetRepoIssueTrackerLink(ctx, store.SetRepoIssueTrackerLinkParams{
			ConnectionID: otherITC, ProjectID: "0-1", ProjectShort: "ABC", TagID: "9-1", TagName: "uzi", RepoID: repo.ID, UserID: f.user}); !errors.Is(err, pgx.ErrNoRows) {
			t.Errorf("linking to another user's connection must be refused: %v", err)
		}
		if _, err := q.SetRepoIssueTrackerLink(ctx, store.SetRepoIssueTrackerLinkParams{
			ConnectionID: otherITC, ProjectID: "0-1", ProjectShort: "ABC", TagID: "9-1", TagName: "uzi", RepoID: repo.ID, UserID: other}); !errors.Is(err, pgx.ErrNoRows) {
			t.Errorf("another user linking a repo they do not own must be refused: %v", err)
		}
		if _, err := q.SetRepoIssueTrackerLink(ctx, store.SetRepoIssueTrackerLinkParams{
			ConnectionID: f.itc, ProjectID: "0-1", ProjectShort: "ABC", TagID: "9-1", TagName: "uzi", RepoID: repo.ID, UserID: other}); !errors.Is(err, pgx.ErrNoRows) {
			t.Errorf("a foreign repo with the foreign user's own connection id must be refused: %v", err)
		}
		if itlLinked(t, pool, repo.ID) {
			t.Fatal("a refused link must write nothing")
		}
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		qt := q.WithTx(tx)
		lock, err := qt.LockRepoForLinkForUser(ctx, store.LockRepoForLinkForUserParams{ID: repo.ID, UserID: f.user})
		if err != nil || !lock.AwaitingFirstEnable || lock.IssueTrackerConnectionID.Valid {
			t.Fatalf("lock: %+v %v", lock, err)
		}
		linked, err := qt.SetRepoIssueTrackerLink(ctx, store.SetRepoIssueTrackerLinkParams{
			ConnectionID: f.itc, ProjectID: "0-1", ProjectShort: "ABC", TagID: "9-1", TagName: "uzi", RepoID: repo.ID, UserID: f.user})
		if err != nil || !linked.IssueTrackerLinkedAt.Valid {
			t.Fatalf("SetRepoIssueTrackerLink: %+v %v", linked, err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		src, err = q.GetRepoIssueSourceForUser(ctx, store.GetRepoIssueSourceForUserParams{ID: repo.ID, UserID: f.user})
		if err != nil || !src.LinkedConnectionID.Valid || src.LinkedConnectionID.Bytes != f.itc || src.IssueTrackerProjectShort.String != "ABC" {
			t.Fatalf("source after link: %+v %v", src, err)
		}
		n, err := q.CountReposLinkedToIssueTrackerConnection(ctx, f.itc)
		if err != nil || n != 1 {
			t.Fatalf("count linked: %d %v", n, err)
		}
		// Already linked: refused (no row).
		if _, err := q.SetRepoIssueTrackerLink(ctx, store.SetRepoIssueTrackerLinkParams{
			ConnectionID: f.itc, ProjectID: "0-2", ProjectShort: "XYZ", TagID: "9-2", TagName: "t2", RepoID: repo.ID, UserID: f.user}); !errors.Is(err, pgx.ErrNoRows) {
			t.Errorf("relinking must be refused: %v", err)
		}
		// FK: deleting a referenced connection fails.
		if _, err := pool.Exec(ctx, `DELETE FROM issue_tracker_connections WHERE id=$1`, f.itc); sqlState(err) != "23503" {
			t.Errorf("deleting a linked connection must fail on the FK: %v", err)
		}
		// Owner binding of the read: a link to another user's connection, forced past the
		// query by a raw UPDATE, must not read back as a linked connection.
		itlExec(t, pool, `UPDATE repos SET issue_tracker_connection_id=$2 WHERE id=$1`, repo.ID, otherITC)
		src, err = q.GetRepoIssueSourceForUser(ctx, store.GetRepoIssueSourceForUserParams{ID: repo.ID, UserID: f.user})
		if err != nil || src.LinkedConnectionID.Valid {
			t.Errorf("a connection of another user must not read as linked: %+v %v", src, err)
		}
		itlExec(t, pool, `UPDATE repos SET issue_tracker_connection_id=$2 WHERE id=$1`, repo.ID, f.itc)
		// A repo that existed before the migration counts as previously enabled and refuses the link.
		if _, err := q.SetRepoIssueTrackerLink(ctx, store.SetRepoIssueTrackerLinkParams{
			ConnectionID: f.itc, ProjectID: "0-1", ProjectShort: "ABC", TagID: "9-1", TagName: "uzi", RepoID: f.legacy, UserID: f.user}); !errors.Is(err, pgx.ErrNoRows) {
			t.Errorf("an enabled-once repo must refuse the link: %v", err)
		}
		// Unlinking (all six to NULL) satisfies the CHECK.
		itlExec(t, pool, `UPDATE repos SET issue_tracker_connection_id=NULL, issue_tracker_project_id=NULL, issue_tracker_project_short=NULL,
			eligibility_tag_id=NULL, eligibility_tag_name=NULL, issue_tracker_linked_at=NULL WHERE id=$1`, repo.ID)
		if n, _ := q.CountReposLinkedToIssueTrackerConnection(ctx, f.itc); n != 0 {
			t.Errorf("count after unlink: %d", n)
		}
	})
}

// TestIssueTrackerRepoLinkDownUpLiveDB runs 00474's Down and Up again.
func TestIssueTrackerRepoLinkDownUpLiveDB(t *testing.T) {
	ctx, dsn, pool, f := itlMigrateWithLegacyRepo(t)
	if err := store.MigrateDownTo(ctx, dsn, 473); err != nil {
		t.Fatalf("down: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_name='repos' AND column_name IN ('awaiting_first_enable','issue_tracker_connection_id','eligibility_tag_name')`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("columns left after Down: %d %v", n, err)
	}
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM pg_trigger WHERE tgname LIKE '%awaiting_first_enable_guard') + (SELECT count(*) FROM pg_proc WHERE proname LIKE '%awaiting_first_enable_guard') + (SELECT count(*) FROM information_schema.columns WHERE table_name='issues' AND column_name='issue_key')`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("objects left after Down: %d %v", n, err)
	}
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if itlAwaiting(t, pool, f.legacy) {
		t.Fatal("legacy repo must be previously enabled after a re-Up")
	}
}
