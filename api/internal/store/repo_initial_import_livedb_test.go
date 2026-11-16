package store_test

import (
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4399: the repos.initial_import_* columns, their CHECK constraints, and the
// generated EnableRepoForUser / ClaimRepoInitialImport / FinishRepoInitialImport
// queries, executed against a real Postgres (a green sqlc generate proves none of it).
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres;
// ./e2e/run-store-it.sh provides one and sweeps this package.

type importCols struct {
	state, class pgtype.Text
	gen          int64
	updatedAt    pgtype.Timestamptz
}

func readImportCols(fx *rollAffinityFixture) importCols {
	fx.t.Helper()
	var c importCols
	if err := fx.pool.QueryRow(fx.ctx, `SELECT initial_import_state, initial_import_generation, initial_import_error_class, initial_import_updated_at FROM repos WHERE id=$1`, fx.repoID).
		Scan(&c.state, &c.gen, &c.class, &c.updatedAt); err != nil {
		fx.t.Fatal(err)
	}
	return c
}

func wantImport(t *testing.T, label string, got importCols, state string, gen int64, class string) {
	t.Helper()
	if got.state.String != state || got.state.Valid != (state != "") || got.gen != gen || got.class.String != class || got.class.Valid != (class != "") {
		t.Fatalf("%s: state=%+v gen=%d class=%+v, want state=%q gen=%d class=%q", label, got.state, got.gen, got.class, state, gen, class)
	}
}

func TestRepoInitialImportQueriesLiveDB(t *testing.T) {
	fx := newRollAffinityFixture(t) // an enabled repo with no recorded import: a legacy row
	enable := func(user uuid.UUID) (store.Repo, error) {
		return fx.q.EnableRepoForUser(fx.ctx, store.EnableRepoForUserParams{ID: fx.repoID, UserID: user})
	}
	claim := func(gen int64) int64 {
		n, err := fx.q.ClaimRepoInitialImport(fx.ctx, store.ClaimRepoInitialImportParams{ID: fx.repoID, InitialImportGeneration: gen})
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	finish := func(gen int64, state, class string) int64 {
		arg := store.FinishRepoInitialImportParams{ID: fx.repoID, Generation: gen, State: state}
		if class != "" {
			arg.ErrorClass = pgtype.Text{String: class, Valid: true}
		}
		n, err := fx.q.FinishRepoInitialImport(fx.ctx, arg)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}

	wantImport(t, "legacy row", readImportCols(fx), "", 0, "")

	// Owner-scoped: another user's id matches no row.
	if _, err := enable(uuid.New()); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("enable by a non-owner err = %v, want pgx.ErrNoRows", err)
	}

	// Enabling records a waiting import at a new generation; the returned row carries it.
	repo, err := enable(fx.userID)
	if err != nil {
		t.Fatal(err)
	}
	if !repo.Enabled || repo.InitialImportGeneration != 1 || repo.InitialImportState.String != "waiting" {
		t.Fatalf("enable returned %+v, want enabled, generation 1, waiting", repo)
	}
	wantImport(t, "after enable", readImportCols(fx), "waiting", 1, "")

	// A claim at a stale generation, or after the import is already settled, matches nothing.
	if n := claim(0); n != 0 {
		t.Fatalf("claim at a stale generation wrote %d rows, want 0", n)
	}
	if n := claim(1); n != 1 {
		t.Fatalf("claim at the current generation wrote %d rows, want 1", n)
	}
	wantImport(t, "claimed", readImportCols(fx), "running", 1, "")
	if n := claim(1); n != 1 { // a running import is reclaimed after a restart
		t.Fatalf("re-claim of a running import wrote %d rows, want 1", n)
	}

	// Finish is generation- and state-guarded.
	if n := finish(0, "succeeded", ""); n != 0 {
		t.Fatalf("finish at a stale generation wrote %d rows, want 0", n)
	}
	if n := finish(1, "failed", "timeout"); n != 1 {
		t.Fatalf("finish failed wrote %d rows, want 1", n)
	}
	wantImport(t, "failed", readImportCols(fx), "failed", 1, "timeout")
	if n := finish(1, "succeeded", ""); n != 0 {
		t.Fatalf("finish of a non-running import wrote %d rows, want 0", n)
	}
	// failed -> running (claim clears the class) -> succeeded.
	if n := claim(1); n != 1 {
		t.Fatalf("claim of a failed import wrote %d rows, want 1", n)
	}
	wantImport(t, "retry running", readImportCols(fx), "running", 1, "")
	if n := finish(1, "succeeded", ""); n != 1 {
		t.Fatalf("finish succeeded wrote %d rows, want 1", n)
	}
	done := readImportCols(fx)
	wantImport(t, "succeeded", done, "succeeded", 1, "")
	if n := claim(1); n != 0 {
		t.Fatalf("claim of a succeeded import wrote %d rows, want 0", n)
	}

	// Re-enabling an enabled repo whose import succeeded changes nothing.
	if _, err := enable(fx.userID); err != nil {
		t.Fatal(err)
	}
	again := readImportCols(fx)
	wantImport(t, "idempotent enable", again, "succeeded", 1, "")
	if !again.updatedAt.Time.Equal(done.updatedAt.Time) {
		t.Fatalf("idempotent enable moved updated_at from %v to %v", done.updatedAt.Time, again.updatedAt.Time)
	}

	// A disable keeps the row; the next enable takes a new generation and a waiting import.
	mustExec(fx.ctx, t, fx.pool, `UPDATE repos SET enabled=false WHERE id=$1`, fx.repoID)
	wantImport(t, "after disable", readImportCols(fx), "succeeded", 1, "")
	if _, err := enable(fx.userID); err != nil {
		t.Fatal(err)
	}
	wantImport(t, "enable after disable", readImportCols(fx), "waiting", 2, "")
	// An enable while the import is still owed bumps again (the newest enable owns it).
	if _, err := enable(fx.userID); err != nil {
		t.Fatal(err)
	}
	wantImport(t, "enable while waiting", readImportCols(fx), "waiting", 3, "")
}

func TestRepoInitialImportConstraintsLiveDB(t *testing.T) {
	fx := newRollAffinityFixture(t)
	set := func(sql string) error {
		_, err := fx.pool.Exec(fx.ctx, "UPDATE repos SET "+sql+" WHERE id=$1", fx.repoID)
		return err
	}
	reset := func() {
		mustExec(fx.ctx, t, fx.pool, `UPDATE repos SET initial_import_state=NULL, initial_import_generation=0, initial_import_error_class=NULL, initial_import_updated_at=NULL WHERE id=$1`, fx.repoID)
	}
	for _, tc := range []struct {
		name, set, constraint string // constraint "" means the write must succeed
	}{
		{"legacy NULL state and class", `initial_import_state=NULL, initial_import_error_class=NULL`, ""},
		{"waiting", `initial_import_state='waiting'`, ""},
		{"running", `initial_import_state='running'`, ""},
		{"succeeded", `initial_import_state='succeeded'`, ""},
		{"failed with a class", `initial_import_state='failed', initial_import_error_class='server_error'`, ""},
		{"unknown state", `initial_import_state='importing'`, "repos_initial_import_state_check"},
		{"negative generation", `initial_import_generation=-1`, "repos_initial_import_generation_check"},
		{"unknown class", `initial_import_state='failed', initial_import_error_class='weird'`, "repos_initial_import_error_class_check"},
		{"failed without a class", `initial_import_state='failed', initial_import_error_class=NULL`, "repos_initial_import_error_pairing_check"},
		{"succeeded with a class", `initial_import_state='succeeded', initial_import_error_class='other'`, "repos_initial_import_error_pairing_check"},
		{"class without a state", `initial_import_state=NULL, initial_import_error_class='other'`, "repos_initial_import_error_pairing_check"},
	} {
		reset()
		err := set(tc.set)
		if tc.constraint == "" {
			if err != nil {
				t.Errorf("%s: write rejected: %v", tc.name, err)
			}
			continue
		}
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.ConstraintName != tc.constraint {
			t.Errorf("%s: err = %v, want a violation of %s", tc.name, err, tc.constraint)
		}
	}
	reset()
}
