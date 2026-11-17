package store_test

import (
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Migration 00467 (PRD #4173 D13) against a real Postgres: a delete still reports its row,
// a second removal keeps a linked flag, and Down removes the table and its triggers so a
// delete and an enable behave as before. Skipped without UZI_TEST_DATABASE_URL.
func TestRepoRemovalMarkersMigrationLiveDB(t *testing.T) {
	ctx, dsn, pool, f := itlMigrateWithLegacyRepo(t)
	q := store.New(pool)

	count := func(sql string, args ...any) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// The legacy repo was enabled before the markers existed, so its removal writes one.
	n, err := q.DeleteRepoForUser(ctx, store.DeleteRepoForUserParams{ID: f.legacy, UserID: f.user})
	if err != nil || n != 1 {
		t.Fatalf("DeleteRepoForUser = %d, %v; want 1 row", n, err)
	}
	if got := count(`SELECT count(*) FROM repo_removal_markers WHERE user_id=$1 AND forge_type='github' AND base_url='https://example.com' AND forge_project_id=1 AND NOT was_linked`, f.user); got != 1 {
		t.Fatalf("markers after the removal = %d, want 1", got)
	}

	// A linked removal, then an unlinked one of the same project: the flag stays set.
	itlExec(t, pool, `UPDATE repo_removal_markers SET was_linked = true WHERE user_id=$1`, f.user)
	id := uuid.New()
	itlExec(t, pool, `INSERT INTO repos (id,connection_id,forge_project_id,path_with_namespace,web_url) VALUES ($1,$2,1,'o/again','https://example.com/o/again')`, id, f.forgeConn)
	if itlAwaiting(t, pool, id) {
		t.Fatal("a raw insert of a project with a marker must not be awaiting its first enable")
	}
	itlExec(t, pool, `DELETE FROM repos WHERE id=$1`, id)
	if got := count(`SELECT count(*) FROM repo_removal_markers WHERE user_id=$1 AND was_linked`, f.user); got != 1 {
		t.Fatalf("a second removal cleared the linked flag (rows still linked = %d)", got)
	}

	// A re-inserted project with a linked marker cannot be enabled unlinked, until Down.
	again := uuid.New()
	itlExec(t, pool, `INSERT INTO repos (id,connection_id,forge_project_id,path_with_namespace,web_url) VALUES ($1,$2,1,'o/again','https://example.com/o/again')`, again, f.forgeConn)
	var pgErr *pgconn.PgError
	if _, err := pool.Exec(ctx, `UPDATE repos SET enabled = true WHERE id=$1`, again); !errors.As(err, &pgErr) || pgErr.Code != "UZ414" {
		t.Fatalf("enable before Down err = %v, want SQLSTATE UZ414", err)
	}

	// Down: the table and triggers are gone, and the enable that was refused goes through.
	if err := store.MigrateDownTo(ctx, dsn, 466); err != nil {
		t.Fatalf("down: %v", err)
	}
	if got := count(`SELECT count(*) FROM pg_tables WHERE tablename='repo_removal_markers'`); got != 0 {
		t.Fatal("Down left the table")
	}
	if got := count(`SELECT count(*) FROM pg_trigger WHERE tgname LIKE 'repos_removal_marker_%'`); got != 0 {
		t.Fatalf("Down left %d triggers", got)
	}
	if got := count(`SELECT count(*) FROM pg_proc WHERE proname LIKE 'repos_removal_marker_%'`); got != 0 {
		t.Fatalf("Down left %d functions", got)
	}
	if _, err := pool.Exec(ctx, `UPDATE repos SET enabled = true WHERE id=$1`, again); err != nil {
		t.Fatalf("enable after Down: %v", err)
	}
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("up again: %v", err)
	}
}
