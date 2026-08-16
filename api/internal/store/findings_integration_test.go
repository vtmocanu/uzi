package store_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestIncidentalFindingsLiveDB exercises the PRD #333 M1 schema + the FULL query set
// against a REAL Postgres — the two-table store (per-run `findings` evidence + the
// coordinate-keyed `finding_dispositions` lifecycle) that the fake-store unit tests
// cannot cover. It proves the load-bearing M1 properties from the PRD (line 107):
//
//	(1) the coordinate UNIQUE(user_id, repo_id, location) holds, and the SAME location
//	    under two repos stays two distinct coordinates (D3 — repo_id in the coordinate is
//	    the whole repo-differentiation answer);
//	(2) UpsertOpenDisposition is idempotent and does NOT overwrite a filed/dismissed row
//	    (ON CONFLICT DO NOTHING, D6 suppression);
//	(3) a content-hash MISMATCH re-opens a resolved coordinate (1 row) while an identical
//	    hash does not (0 rows) — ReopenDispositionOnHashMismatch (D3);
//	(4) ClaimFindingForFiling moves exactly one open→filing and a second concurrent claim
//	    gets 0 rows affected (the guarded UPDATE, D4 claim-first double-file safety);
//	(5) the status/reason CHECK rejects a reasonless dismissal;
//	(6) the full lifecycle open→filing→filed, filing→open (revert), open→dismissed, and the
//	    disposition-driven backlog dedup / seen_in_runs / bucket / repo+run filters (D7).
//
// Canonicalisation of `location` is the SERVICE's job (M2); this test stores the raw value
// and proves only that UNIQUE(user_id, repo_id, location) behaves — the coordinate math the
// service's dedup rests on.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (e2e/run-store-it.sh).
func TestIncidentalFindingsLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	defer pool.Close()
	q := store.New(pool)

	// A user with a forge connection and two repos, so the cross-repo coordinate split is
	// exercisable. Each repo gets its own run so findings.run_id (FK→runs) resolves.
	userID, connID := uuid.New(), uuid.New()
	repoA, repoB := uuid.New(), uuid.New()
	runA1, runA2, runB1 := uuid.New(), uuid.New(), uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("findings-%s@e2e", userID))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	mkRepo := func(id uuid.UUID, pid int64, path string) {
		mustExec(ctx, t, pool,
			`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
			 VALUES ($1, $2, $3, $4, 'https://forge.e2e/'||$4, 'main', true)`, id, connID, pid, path)
	}
	mkRun := func(id, repoID uuid.UUID, iid int64) {
		mustExec(ctx, t, pool,
			`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind)
			 VALUES ($1, $2, $3, $4, 'Do X', 'desc', 'completed', 'issue')`, id, userID, repoID, iid)
	}
	mkRepo(repoA, 1, "g/a")
	mkRepo(repoB, 2, "g/b")
	mkRun(runA1, repoA, 11)
	mkRun(runA2, repoA, 12)
	mkRun(runB1, repoB, 21)

	// ── (0) migration Down rolls back cleanly, then Up restores the schema ──
	// Run FIRST, before any evidence exists, so dropping and recreating the two tables
	// leaves the rest of this test (and the shared-DB suite) an intact, empty schema. The
	// migration's own Up/Down SQL is replayed verbatim so this cannot drift from what ships.
	t.Run("goose Down drops both tables and Up recreates them", func(t *testing.T) {
		tablesExist := func() (findings, dispositions bool) {
			if err := pool.QueryRow(ctx, `SELECT to_regclass('findings') IS NOT NULL,
				to_regclass('finding_dispositions') IS NOT NULL`).Scan(&findings, &dispositions); err != nil {
				t.Fatalf("regclass probe: %v", err)
			}
			return
		}
		if f, d := tablesExist(); !f || !d {
			t.Fatalf("precondition: both tables should exist after Migrate (findings=%v dispositions=%v)", f, d)
		}
		for _, stmt := range migrationDownStatements(t, "00129_incidental_findings.sql") {
			mustExec(ctx, t, pool, stmt)
		}
		if f, d := tablesExist(); f || d {
			t.Fatalf("after Down both tables should be gone (findings=%v dispositions=%v)", f, d)
		}
		for _, stmt := range migrationUpStatements(t, "00129_incidental_findings.sql") {
			mustExec(ctx, t, pool, stmt)
		}
		if f, d := tablesExist(); !f || !d {
			t.Fatalf("after replaying Up both tables should exist again (findings=%v dispositions=%v)", f, d)
		}
	})

	insFinding := func(runID, repoID uuid.UUID, location, title string) store.IncidentalFinding {
		t.Helper()
		f, err := q.InsertFinding(ctx, store.InsertFindingParams{
			RunID: runID, UserID: userID, RepoID: repoID, Location: location,
			Title: title, DescriptionMd: "does a thing", Labels: []byte(`["bug"]`), Confidence: "high",
		})
		if err != nil {
			t.Fatalf("InsertFinding(%s): %v", location, err)
		}
		return f
	}
	// upsertOpen returns true when it actually inserted (pgx.ErrNoRows ⇒ conflict, no insert).
	upsertOpen := func(repoID uuid.UUID, location, hash, title string) bool {
		t.Helper()
		_, err := q.UpsertOpenDisposition(ctx, store.UpsertOpenDispositionParams{
			UserID: userID, RepoID: repoID, Location: location, ContentHash: hash, LastTitle: title,
		})
		if err == nil {
			return true
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return false
		}
		t.Fatalf("UpsertOpenDisposition(%s): %v", location, err)
		return false
	}

	// ── (1) + (6-dedup) coordinate uniqueness, cross-repo split, seen_in_runs ──
	// Two evidence rows at the SAME location in repoA (from two different runs) collapse to
	// ONE coordinate; the same location in repoB is a SECOND coordinate. The backlog then
	// reads seen_in_runs=2 for repoA's coordinate and 1 for repoB's.
	const loc = "internal/sweep.go#sweeploop"
	t.Run("same location collapses to one coordinate per repo; different repo is distinct", func(t *testing.T) {
		insFinding(runA1, repoA, loc, "leaked ticker in sweepLoop")
		insFinding(runA2, repoA, loc, "leaked ticker in sweepLoop (again)")
		insFinding(runB1, repoB, loc, "leaked ticker in sweepLoop (repo B)")

		if !upsertOpen(repoA, loc, "hash-a-v1", "leaked ticker in sweepLoop") {
			t.Fatal("first upsert on repoA coordinate should INSERT")
		}
		if upsertOpen(repoA, loc, "hash-a-v1", "leaked ticker (again)") {
			t.Fatal("second upsert on the SAME repoA coordinate must be a no-op (ON CONFLICT DO NOTHING)")
		}
		if !upsertOpen(repoB, loc, "hash-b-v1", "leaked ticker in sweepLoop (repo B)") {
			t.Fatal("the SAME location under repoB is a DISTINCT coordinate and should INSERT")
		}
		// Exactly two disposition rows exist for this user+location, one per repo.
		if n := scalarInt(ctx, t, pool,
			`SELECT count(*) FROM finding_dispositions WHERE user_id=$1 AND location=$2`, userID, loc); n != 2 {
			t.Fatalf("want 2 coordinates (one per repo) for the shared location, got %d", n)
		}

		// backlog: repoA coordinate seen in 2 runs, repoB in 1.
		rows := backlog(ctx, t, q, store.ListFindingsBacklogParams{UserID: userID})
		gotA, gotB := int64(-1), int64(-1)
		for _, r := range rows {
			if r.Location != loc {
				continue
			}
			switch r.RepoID {
			case repoA:
				gotA = r.SeenInRuns
			case repoB:
				gotB = r.SeenInRuns
			}
		}
		if gotA != 2 || gotB != 1 {
			t.Fatalf("seen_in_runs: repoA=%d (want 2) repoB=%d (want 1)", gotA, gotB)
		}
	})

	// ── (6) CountFindingsForRun / CountOpenFindingsForUser / GetIncidentalFinding ──
	t.Run("counts and owner-scoped get", func(t *testing.T) {
		if n, err := q.CountFindingsForRun(ctx, runA1); err != nil || n != 1 {
			t.Fatalf("CountFindingsForRun(runA1) = %d, %v; want 1", n, err)
		}
		// Two open coordinates so far (repoA + repoB at loc).
		if n, err := q.CountOpenFindingsForUser(ctx, store.CountOpenFindingsForUserParams{UserID: userID}); err != nil || n != 2 {
			t.Fatalf("CountOpenFindingsForUser = %d, %v; want 2", n, err)
		}
		// ?repo= narrows to one.
		if n, err := q.CountOpenFindingsForUser(ctx, store.CountOpenFindingsForUserParams{
			UserID: userID, RepoID: pgtype.UUID{Bytes: repoA, Valid: true},
		}); err != nil || n != 1 {
			t.Fatalf("CountOpenFindingsForUser(repoA) = %d, %v; want 1", n, err)
		}
		// A finding id resolves for its owner; a foreign user gets ErrNoRows.
		f := insFinding(runA1, repoA, "internal/other.go#f", "another bug")
		got, err := q.GetIncidentalFinding(ctx, store.GetIncidentalFindingParams{ID: f.ID, UserID: userID})
		if err != nil || got.ID != f.ID {
			t.Fatalf("GetIncidentalFinding(owner) = %+v, %v", got, err)
		}
		if _, err := q.GetIncidentalFinding(ctx, store.GetIncidentalFindingParams{ID: f.ID, UserID: uuid.New()}); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("GetIncidentalFinding(foreign user) must be ErrNoRows, got %v", err)
		}
	})

	// ── (4) the claim-first filing state machine, and (6) settle/revert ──
	coord := func(location string) (uuid.UUID, uuid.UUID, string) { return userID, repoA, location }
	claim := func(location string) int64 {
		t.Helper()
		u, r, l := coord(location)
		n, err := q.ClaimFindingForFiling(ctx, store.ClaimFindingForFilingParams{UserID: u, RepoID: r, Location: l})
		if err != nil {
			t.Fatalf("ClaimFindingForFiling(%s): %v", location, err)
		}
		return n
	}
	t.Run("claim moves exactly one open→filing; a second claim gets zero", func(t *testing.T) {
		if got := claim(loc); got != 1 {
			t.Fatalf("first claim should affect 1 row (open→filing), got %d", got)
		}
		if got := claim(loc); got != 0 {
			t.Fatalf("a second claim on the now-filing coordinate must affect 0 rows, got %d", got)
		}
		if s := dispStatus(ctx, t, pool, userID, repoA, loc); s != "filing" {
			t.Fatalf("status after claim = %q, want filing", s)
		}
	})

	t.Run("revert returns filing→open (retryable); a settled row is untouched by revert", func(t *testing.T) {
		u, r, l := coord(loc)
		if n, err := q.RevertFindingFiling(ctx, store.RevertFindingFilingParams{UserID: u, RepoID: r, Location: l}); err != nil || n != 1 {
			t.Fatalf("RevertFindingFiling = %d, %v; want 1", n, err)
		}
		if s := dispStatus(ctx, t, pool, userID, repoA, loc); s != "open" {
			t.Fatalf("status after revert = %q, want open", s)
		}
		// Re-claim then settle.
		if got := claim(loc); got != 1 {
			t.Fatalf("re-claim after revert should affect 1 row, got %d", got)
		}
		if n, err := q.SettleFindingFiled(ctx, store.SettleFindingFiledParams{
			UserID: u, RepoID: r, Location: l, FiledIssueIid: pgtype.Int8{Int64: 4242, Valid: true},
		}); err != nil || n != 1 {
			t.Fatalf("SettleFindingFiled = %d, %v; want 1", n, err)
		}
		if s := dispStatus(ctx, t, pool, userID, repoA, loc); s != "filed" {
			t.Fatalf("status after settle = %q, want filed", s)
		}
		// A revert on a settled row is guarded to status='filing' → 0 rows, no resurrection.
		if n, err := q.RevertFindingFiling(ctx, store.RevertFindingFilingParams{UserID: u, RepoID: r, Location: l}); err != nil || n != 0 {
			t.Fatalf("revert of a filed row must affect 0 rows, got %d, %v", n, err)
		}
	})

	// ── (2) UpsertOpenDisposition never resurrects a filed row ──
	t.Run("upsert does not overwrite a filed coordinate", func(t *testing.T) {
		if upsertOpen(repoA, loc, "hash-a-v1", "re-report while filed") {
			t.Fatal("upsert on a FILED coordinate must be a no-op (must not resurrect it to open)")
		}
		if s := dispStatus(ctx, t, pool, userID, repoA, loc); s != "filed" {
			t.Fatalf("status after upsert-on-filed = %q, want filed (unchanged)", s)
		}
	})

	// ── (3) content-hash mismatch re-opens a resolved coordinate; identical hash does not ──
	t.Run("hash mismatch re-opens a filed coordinate; identical hash is a no-op", func(t *testing.T) {
		u, r, l := coord(loc)
		// Identical hash → 0 rows (stays filed, D6 suppression).
		if n, err := q.ReopenDispositionOnHashMismatch(ctx, store.ReopenDispositionOnHashMismatchParams{
			UserID: u, RepoID: r, Location: l, ContentHash: "hash-a-v1", LastTitle: "same bug",
		}); err != nil || n != 0 {
			t.Fatalf("re-open with the SAME hash must affect 0 rows, got %d, %v", n, err)
		}
		if s := dispStatus(ctx, t, pool, userID, repoA, loc); s != "filed" {
			t.Fatalf("status after identical-hash re-open attempt = %q, want filed", s)
		}
		// Different hash → 1 row, back to open with the resolved state cleared.
		if n, err := q.ReopenDispositionOnHashMismatch(ctx, store.ReopenDispositionOnHashMismatchParams{
			UserID: u, RepoID: r, Location: l, ContentHash: "hash-a-v2", LastTitle: "materially different bug",
		}); err != nil || n != 1 {
			t.Fatalf("re-open with a DIFFERENT hash must affect 1 row, got %d, %v", n, err)
		}
		var status, lastTitle string
		var filedIID pgtype.Int8
		var resolvedAt pgtype.Timestamptz
		if err := pool.QueryRow(ctx,
			`SELECT status, last_title, filed_issue_iid, resolved_at FROM finding_dispositions
			 WHERE user_id=$1 AND repo_id=$2 AND location=$3`, userID, repoA, loc).
			Scan(&status, &lastTitle, &filedIID, &resolvedAt); err != nil {
			t.Fatalf("read re-opened row: %v", err)
		}
		if status != "open" || lastTitle != "materially different bug" || filedIID.Valid || resolvedAt.Valid {
			t.Fatalf("re-opened row = (status=%q last_title=%q filed_iid_valid=%v resolved_valid=%v); "+
				"want open, refreshed title, cleared iid + resolved_at", status, lastTitle, filedIID.Valid, resolvedAt.Valid)
		}
	})

	// ── UpdateDispositionLastTitle keeps an open coordinate current ──
	t.Run("UpdateDispositionLastTitle refreshes an open row and skips a resolved one", func(t *testing.T) {
		u, r, l := coord(loc) // currently open (re-opened above)
		if n, err := q.UpdateDispositionLastTitle(ctx, store.UpdateDispositionLastTitleParams{
			UserID: u, RepoID: r, Location: l, LastTitle: "freshest title", ContentHash: "hash-a-v3",
		}); err != nil || n != 1 {
			t.Fatalf("UpdateDispositionLastTitle on an open row = %d, %v; want 1", n, err)
		}
		if got := scalarText(ctx, t, pool,
			`SELECT last_title FROM finding_dispositions WHERE user_id=$1 AND repo_id=$2 AND location=$3`, userID, repoA, loc); got != "freshest title" {
			t.Fatalf("last_title after refresh = %q, want 'freshest title'", got)
		}
	})

	// ── (5) the status/reason CHECK, and (6) dismissal ──
	t.Run("dismiss requires a reason (CHECK) and moves open→dismissed", func(t *testing.T) {
		u, r, l := coord(loc) // open
		// Reasonless dismissal violates the CHECK.
		if n, err := q.DismissFinding(ctx, store.DismissFindingParams{
			UserID: u, RepoID: r, Location: l, DismissReason: pgtype.Text{Valid: false},
		}); err == nil {
			t.Fatalf("dismiss with a NULL reason must violate the status/reason CHECK, got n=%d", n)
		}
		if s := dispStatus(ctx, t, pool, userID, repoA, loc); s != "open" {
			t.Fatalf("status after failed dismiss = %q, want open (unchanged)", s)
		}
		// With a reason it lands.
		if n, err := q.DismissFinding(ctx, store.DismissFindingParams{
			UserID: u, RepoID: r, Location: l, DismissReason: pgtype.Text{String: "not_an_issue", Valid: true},
		}); err != nil || n != 1 {
			t.Fatalf("DismissFinding with a reason = %d, %v; want 1", n, err)
		}
		if s := dispStatus(ctx, t, pool, userID, repoA, loc); s != "dismissed" {
			t.Fatalf("status after dismiss = %q, want dismissed", s)
		}
	})

	// ── (6) backlog buckets + repo/run filters over the coordinates built above ──
	t.Run("backlog buckets by status and filters by repo and run", func(t *testing.T) {
		// State now: repoA/loc = dismissed; repoB/loc = open; plus repoA/other.go#f has an
		// evidence row but NO disposition (never upserted), so it must NOT appear (the read is
		// disposition-driven, D7).
		all := backlog(ctx, t, q, store.ListFindingsBacklogParams{UserID: userID})
		if locations := coordSet(all); len(all) != 2 {
			t.Fatalf("all-bucket backlog should hold exactly the 2 coordinates that have dispositions, got %d: %v", len(all), locations)
		}
		// bucket=to_file (status='open') → only repoB/loc.
		open := backlog(ctx, t, q, store.ListFindingsBacklogParams{UserID: userID, Status: pgtype.Text{String: "open", Valid: true}})
		if len(open) != 1 || open[0].RepoID != repoB {
			t.Fatalf("to_file bucket = %+v, want exactly repoB's open coordinate", open)
		}
		// bucket=dismissed → only repoA/loc.
		dismissed := backlog(ctx, t, q, store.ListFindingsBacklogParams{UserID: userID, Status: pgtype.Text{String: "dismissed", Valid: true}})
		if len(dismissed) != 1 || dismissed[0].RepoID != repoA {
			t.Fatalf("dismissed bucket = %+v, want exactly repoA's dismissed coordinate", dismissed)
		}
		// ?repo=repoB → only repoB's coordinate.
		byRepo := backlog(ctx, t, q, store.ListFindingsBacklogParams{UserID: userID, RepoID: pgtype.UUID{Bytes: repoB, Valid: true}})
		if len(byRepo) != 1 || byRepo[0].RepoID != repoB {
			t.Fatalf("?repo=repoB backlog = %+v, want exactly repoB's coordinate", byRepo)
		}
		// ?run=runB1 → only coordinates with evidence in runB1 (repoB/loc), and seen_in_runs
		// is the FULL count (the semi-join does not shrink it).
		byRun := backlog(ctx, t, q, store.ListFindingsBacklogParams{UserID: userID, RunID: pgtype.UUID{Bytes: runB1, Valid: true}})
		if len(byRun) != 1 || byRun[0].RepoID != repoB || byRun[0].SeenInRuns != 1 {
			t.Fatalf("?run=runB1 backlog = %+v, want exactly repoB's coordinate with seen_in_runs=1", byRun)
		}
		// A foreign user sees none of these coordinates.
		if got := backlog(ctx, t, q, store.ListFindingsBacklogParams{UserID: uuid.New()}); len(got) != 0 {
			t.Fatalf("a foreign user's backlog must be empty, got %d rows", len(got))
		}
	})
}

func backlog(ctx context.Context, t *testing.T, q *store.Queries, p store.ListFindingsBacklogParams) []store.ListFindingsBacklogRow {
	t.Helper()
	rows, err := q.ListFindingsBacklog(ctx, p)
	if err != nil {
		t.Fatalf("ListFindingsBacklog: %v", err)
	}
	return rows
}

func coordSet(rows []store.ListFindingsBacklogRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, fmt.Sprintf("%s@%s=%s", r.Location, r.RepoID, r.Status))
	}
	return out
}

func dispStatus(ctx context.Context, t *testing.T, pool *pgxpool.Pool, userID, repoID uuid.UUID, location string) string {
	t.Helper()
	return scalarText(ctx, t, pool,
		`SELECT status FROM finding_dispositions WHERE user_id=$1 AND repo_id=$2 AND location=$3`,
		userID, repoID, location)
}

// migrationDownStatements reads the `-- +goose Down` block out of a migration and returns
// its individual statements, so a rollback test executes the migration's EXACT Down SQL
// rather than a re-typed copy that could drift from what ships (mirror of
// migrationUpStatements in recommendation_canonicalize_integration_test.go).
func migrationDownStatements(t *testing.T, name string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("migrations", name))
	if err != nil {
		t.Fatalf("read migration %s: %v", name, err)
	}
	s := string(b)
	i := strings.Index(s, "-- +goose Down")
	if i < 0 {
		t.Fatalf("migration %s has no `-- +goose Down` block", name)
	}
	down := s[i+len("-- +goose Down"):]
	var code strings.Builder
	for _, line := range strings.Split(down, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		code.WriteString(line)
		code.WriteByte('\n')
	}
	var out []string
	for _, stmt := range strings.Split(code.String(), ";") {
		if strings.TrimSpace(stmt) != "" {
			out = append(out, stmt)
		}
	}
	return out
}
