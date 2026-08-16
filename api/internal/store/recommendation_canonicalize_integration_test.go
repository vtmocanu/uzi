package store_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// canonicalTargetSQL mirrors the 00097 backfill expression AND handler.canonicalizeTarget
// (issue #232): lowercase, collapse whitespace/ASCII-punctuation runs to one space, trim.
// Used as the test's own oracle for the fixtures below. It carries the SAME COLLATE "C" the
// migration does, so the fold is ASCII-only and locale-independent (see 00097's header): on
// the ASCII fixtures here that changes nothing, but keeping it identical to what ships stops
// this oracle from silently drifting from the migration if a non-ASCII fixture is ever added.
func canonicalTargetSQL(ctx context.Context, t *testing.T, pool *pgxpool.Pool, raw string) string {
	t.Helper()
	var got string
	if err := pool.QueryRow(ctx,
		`SELECT lower(btrim(regexp_replace($1 COLLATE "C", '[[:space:][:punct:]]+', ' ', 'g')))`, raw).Scan(&got); err != nil {
		t.Fatalf("canonical SQL for %q: %v", raw, err)
	}
	return got
}

// migrationUpStatements reads the goose `-- +goose Up` block out of a migration file and
// returns its individual SQL statements, so the test executes the MIGRATION'S EXACT SQL
// against pre-canonical rows rather than a re-typed copy that could silently drift from what
// ships. Comment lines are stripped BEFORE splitting on `;` — the migration's own prose
// contains a semicolon (e.g. "...canonicalizes at write (handler.canonicalizeTarget); this
// pass..."), so splitting first would tear a comment mid-line into a bogus "statement".
func migrationUpStatements(t *testing.T, name string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("migrations", name))
	if err != nil {
		t.Fatalf("read migration %s: %v", name, err)
	}
	up := string(b)
	if i := strings.Index(up, "-- +goose Up"); i >= 0 {
		up = up[i+len("-- +goose Up"):]
	}
	if i := strings.Index(up, "-- +goose Down"); i >= 0 {
		up = up[:i]
	}
	// Drop whole `--` comment lines first (every comment in this migration is a full-line
	// comment; no SQL statement carries a trailing inline comment), then split on `;`.
	var code strings.Builder
	for _, line := range strings.Split(up, "\n") {
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

// TestCanonicalizeRecommendationTargetsMigrationLiveDB proves the 00097 backfill (issue
// #232) on a REAL Postgres: it folds the coordinate `target` to its canonical form across
// ALL THREE tables that carry it, keeps the disposition/filed LEFT JOINs LINKED across the
// fold (a previously-triaged item must not resurface as todo), and resolves a unique-key
// collision collision-safely by deleting the deterministic loser before the UPDATE.
//
// store.Migrate runs 00097 at test start, so the tables are already canonical; the test then
// inserts PRE-CANONICAL rows directly (simulating historical, pre-ingest data) and REPLAYS
// the migration's exact Up SQL against them — the only way to exercise the transform at the
// store layer, since ingest canonicalization is handler-layer.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (e2e/run-store-it.sh).
func TestCanonicalizeRecommendationTargetsMigrationLiveDB(t *testing.T) {
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

	userID, connID, repoID := uuid.New(), uuid.New(), uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("canon-%s@e2e", userID))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, repoID, connID)

	// Each review needs its own reviewed run (run_reviews.target_run_id is UNIQUE).
	var reviewIDs []uuid.UUID
	newReview := func(iid int64) uuid.UUID {
		runID, reviewID := uuid.New(), uuid.New()
		mustExec(ctx, t, pool,
			`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind)
			 VALUES ($1, $2, $3, $4, 'do', 'd', 'completed', 'issue')`, runID, userID, repoID, iid)
		mustExec(ctx, t, pool,
			`INSERT INTO run_reviews (id, target_run_id, user_id, verdict) VALUES ($1, $2, $3, 'issues')`,
			reviewID, runID, userID)
		reviewIDs = append(reviewIDs, reviewID)
		return reviewID
	}
	// Isolation: cascade-delete every seeded review at the end (LIFO, and on Fatalf). These
	// use improve_agent so they never touch the instance-wide improve_uzi backlog, but the
	// cleanup is kept regardless — a leaked review row is a cross-test hazard by itself.
	defer func() {
		for _, id := range reviewIDs {
			mustExec(ctx, t, pool, `DELETE FROM run_reviews WHERE id = $1`, id)
		}
	}()

	// ── R1: join consistency across the fold ──
	// One rec + one disposition + one filed link, all on the SAME raw non-canonical
	// coordinate. After the fold all three targets must become the canonical value and the
	// coordinate JOINs (d.target = rr.target, f.target = rr.target) must still link.
	const rawJoin = "Worker Git-Identity Setup"
	r1 := newReview(101)
	mustExec(ctx, t, pool,
		`INSERT INTO review_recommendations (review_id, category, target, rationale_md, confidence)
		 VALUES ($1, 'improve_agent', $2, 'r', 'high')`, r1, rawJoin)
	mustExec(ctx, t, pool,
		`INSERT INTO recommendation_dispositions (review_id, category, target, status, rationale_hash, set_by_user_id)
		 VALUES ($1, 'improve_agent', $2, 'done', 'h', $3)`, r1, rawJoin, userID)
	mustExec(ctx, t, pool,
		`INSERT INTO recommendation_filed_issues
		   (id, review_id, category, target, filed_repo_id, filed_issue_iid, filed_issue_url, filed_by_user_id, filed_at)
		 VALUES ($1, $2, 'improve_agent', $3, $4, 5, 'https://forge.e2e/g/r/-/issues/5', $5, now())`,
		uuid.New(), r1, rawJoin, repoID, userID)

	// ── R2: recommendation_dispositions collision ──
	// Two dispositions on cosmetic variants of ONE coordinate → they fold to the same
	// (review_id, category, target) unique key. The DELETE must keep the freshest
	// (updated_at DESC) — the dismissed 'foo  bar' — and drop the older done 'Foo Bar'.
	r2 := newReview(102)
	mustExec(ctx, t, pool,
		`INSERT INTO recommendation_dispositions (review_id, category, target, status, rationale_hash, set_by_user_id, set_at, updated_at)
		 VALUES ($1, 'improve_agent', 'Foo Bar', 'done', 'h', $2, now() - interval '2 hours', now() - interval '2 hours')`,
		r2, userID)
	mustExec(ctx, t, pool,
		`INSERT INTO recommendation_dispositions (review_id, category, target, status, dismiss_reason, rationale_hash, set_by_user_id, set_at, updated_at)
		 VALUES ($1, 'improve_agent', 'foo  bar', 'dismissed', 'not_an_issue', 'h', $2, now(), now())`,
		r2, userID)

	// ── R3: recommendation_filed_issues collision ──
	// Two filed rows on cosmetic variants of ONE coordinate. Winner is the SETTLED row even
	// though the unsettled claim has a NEWER filing_since — (filed_at IS NOT NULL) DESC leads
	// the winner ordering, proving settled beats mere recency.
	r3 := newReview(103)
	settledID := uuid.New()
	mustExec(ctx, t, pool,
		`INSERT INTO recommendation_filed_issues
		   (id, review_id, category, target, filed_repo_id, filed_issue_iid, filed_issue_url, filed_by_user_id, filed_at)
		 VALUES ($1, $2, 'improve_agent', 'Bar Baz', $3, 9, 'https://forge.e2e/g/r/-/issues/9', $4, now() - interval '1 hour')`,
		settledID, r3, repoID, userID)
	mustExec(ctx, t, pool,
		`INSERT INTO recommendation_filed_issues (id, review_id, category, target, filing_since)
		 VALUES ($1, $2, 'improve_agent', 'bar--baz', now())`, uuid.New(), r3)

	// ── R4: non-ASCII discrimination — COLLATE "C" in 00097 is load-bearing ──
	// The raw target carries a curly apostrophe U+2019 (a NON-ASCII byte run). Both
	// handler.canonicalizeTarget (RE2's ASCII-only [:punct:]) and the migration (via
	// COLLATE "C") treat that byte run as opaque and KEEP it, folding only the ASCII casing,
	// whitespace and ASCII punctuation around it. Drop COLLATE "C" from the migration and
	// Postgres's locale-aware POSIX [:punct:] would fold U+2019 → space under en_US.utf8,
	// yielding "worker s git identity" — a value ingest never produces, so a pre-migration
	// disposition would de-link and resurface as todo. One rec + one disposition on the SAME
	// raw coordinate so the join-consistency check exercises that de-link path. This fixture
	// is the ONLY one here that discriminates COLLATE "C"; the ASCII fixtures above are no-ops
	// for it (see 00097's header).
	const rawCurly = "Worker’s  Git-Identity"
	r4 := newReview(104)
	mustExec(ctx, t, pool,
		`INSERT INTO review_recommendations (review_id, category, target, rationale_md, confidence)
		 VALUES ($1, 'improve_agent', $2, 'r', 'high')`, r4, rawCurly)
	mustExec(ctx, t, pool,
		`INSERT INTO recommendation_dispositions (review_id, category, target, status, rationale_hash, set_by_user_id)
		 VALUES ($1, 'improve_agent', $2, 'done', 'h', $3)`, r4, rawCurly, userID)

	// Sanity: the fixtures fold to what we expect, computed via the migration's own SQL
	// expression (the test's oracle) — so a fixture typo can't quietly pass the asserts.
	wantJoin := canonicalTargetSQL(ctx, t, pool, rawJoin)
	if wantJoin != "worker git identity setup" {
		t.Fatalf("fixture oracle drift: %q folded to %q", rawJoin, wantJoin)
	}
	// The oracle (canonicalTargetSQL) carries its OWN COLLATE "C" independent of 00097, so
	// wantCurly stays "worker’s git identity" (U+2019 KEPT) even if the migration's COLLATE "C"
	// is later dropped — that is precisely what turns such a drop into a test failure below.
	wantCurly := canonicalTargetSQL(ctx, t, pool, rawCurly)
	if wantCurly != "worker’s git identity" {
		t.Fatalf("fixture oracle drift: %q folded to %q, want the U+2019 KEPT (\"worker’s git identity\")", rawCurly, wantCurly)
	}

	// ── replay the migration's EXACT Up SQL against the pre-canonical rows ──
	stmts := migrationUpStatements(t, "00097_canonicalize_recommendation_targets.sql")
	for _, stmt := range stmts {
		mustExec(ctx, t, pool, stmt)
	}

	// R1: rec folded, and the disposition + filed link still JOIN on the coordinate.
	if got := scalarText(ctx, t, pool,
		`SELECT target FROM review_recommendations WHERE review_id = $1`, r1); got != wantJoin {
		t.Errorf("R1 rec target = %q, want canonical %q", got, wantJoin)
	}
	if n := scalarInt(ctx, t, pool,
		`SELECT count(*) FROM recommendation_dispositions d
		 JOIN review_recommendations rr
		   ON rr.review_id = d.review_id AND rr.category = d.category AND rr.target = d.target
		 WHERE d.review_id = $1`, r1); n != 1 {
		t.Errorf("R1 disposition must stay LINKED to its rec after the fold, joined rows = %d", n)
	}
	if n := scalarInt(ctx, t, pool,
		`SELECT count(*) FROM recommendation_filed_issues f
		 JOIN review_recommendations rr
		   ON rr.review_id = f.review_id AND rr.category = f.category AND rr.target = f.target
		 WHERE f.review_id = $1`, r1); n != 1 {
		t.Errorf("R1 filed link must stay LINKED to its rec after the fold, joined rows = %d", n)
	}

	// R4: the curly apostrophe (U+2019) SURVIVES the fold — proving COLLATE "C" kept the
	// transform ASCII-only — while the ASCII casing/whitespace/punctuation around it folds.
	// Comparing against wantCurly (oracle, which keeps its own COLLATE "C") is the discriminator:
	// drop COLLATE "C" from 00097 and the migration folds this rec to "worker s git identity",
	// mismatching the expected "worker’s git identity", so this assertion FAILS.
	if got := scalarText(ctx, t, pool,
		`SELECT target FROM review_recommendations WHERE review_id = $1`, r4); got != wantCurly {
		t.Errorf("R4 rec target = %q, want canonical %q (U+2019 preserved, ASCII folded/lowercased)", got, wantCurly)
	}
	// And the disposition on that coordinate stays LINKED to its rec — no de-link across the fold.
	if n := scalarInt(ctx, t, pool,
		`SELECT count(*) FROM recommendation_dispositions d
		 JOIN review_recommendations rr
		   ON rr.review_id = d.review_id AND rr.category = d.category AND rr.target = d.target
		 WHERE d.review_id = $1 AND rr.target = $2`, r4, wantCurly); n != 1 {
		t.Errorf("R4 disposition must stay LINKED to its rec on the canonical (U+2019-preserving) target, joined rows = %d", n)
	}

	// R2: exactly one disposition survives the collision, and it is the deterministic winner
	// (the freshest — dismissed/not_an_issue), now on the canonical target.
	if n := scalarInt(ctx, t, pool,
		`SELECT count(*) FROM recommendation_dispositions WHERE review_id = $1`, r2); n != 1 {
		t.Fatalf("R2 must collapse two colliding dispositions to one, got %d", n)
	}
	var status, reason, tgt string
	if err := pool.QueryRow(ctx,
		`SELECT status, coalesce(dismiss_reason, ''), target FROM recommendation_dispositions WHERE review_id = $1`, r2).
		Scan(&status, &reason, &tgt); err != nil {
		t.Fatalf("R2 read survivor: %v", err)
	}
	if status != "dismissed" || reason != "not_an_issue" || tgt != "foo bar" {
		t.Errorf("R2 winner = (%q,%q,%q), want the freshest (dismissed,not_an_issue,\"foo bar\")", status, reason, tgt)
	}

	// R3: exactly one filed row survives, and it is the SETTLED one (filed_at set), now
	// canonical — settled beats the newer unsettled claim.
	if n := scalarInt(ctx, t, pool,
		`SELECT count(*) FROM recommendation_filed_issues WHERE review_id = $1`, r3); n != 1 {
		t.Fatalf("R3 must collapse two colliding filed rows to one, got %d", n)
	}
	var survivorID uuid.UUID
	var filedNotNull bool
	if err := pool.QueryRow(ctx,
		`SELECT id, (filed_at IS NOT NULL), target FROM recommendation_filed_issues WHERE review_id = $1`, r3).
		Scan(&survivorID, &filedNotNull, &tgt); err != nil {
		t.Fatalf("R3 read survivor: %v", err)
	}
	if survivorID != settledID || !filedNotNull || tgt != "bar baz" {
		t.Errorf("R3 winner = (id=%s settled=%v target=%q), want the settled row on \"bar baz\"", survivorID, filedNotNull, tgt)
	}

	// Re-running the migration SQL is a NO-OP now (every row already canonical): the
	// WHERE target <> canonical guard matches nothing, so idempotence holds at the SQL layer.
	for _, stmt := range stmts {
		mustExec(ctx, t, pool, stmt)
	}
	if n := scalarInt(ctx, t, pool,
		`SELECT count(*) FROM recommendation_dispositions WHERE review_id = $1`, r2); n != 1 {
		t.Errorf("second migration pass must be a no-op, R2 disposition count = %d", n)
	}
}

// TestListKnownImproveUziTargetsForUserLiveDB proves the write-side menu query (issue #232,
// Part B) against real SQL: it returns the owner's improve_uzi targets frequency-ranked,
// deduped by the (already-canonical) target, EXCLUDING empty targets and other categories,
// scoped to the owner (never another user's targets), and capped by @lim.
func TestListKnownImproveUziTargetsForUserLiveDB(t *testing.T) {
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

	var reviewIDs []uuid.UUID
	// A review-with-recs for one user; recs is a list of (category, target). Direct INSERTs
	// simulate post-ingest (already-canonical) rows.
	seed := func(iid int64, recs [][2]string) uuid.UUID {
		userID, connID, repoID := uuid.New(), uuid.New(), uuid.New()
		mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
			userID, fmt.Sprintf("knownmenu-%s@e2e", userID))
		mustExec(ctx, t, pool,
			`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
			 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
		mustExec(ctx, t, pool,
			`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
			 VALUES ($1, $2, $3, $4, 'https://forge.e2e/g/r', 'main', true)`, repoID, connID, iid, fmt.Sprintf("g/r%d", iid))
		runID, reviewID := uuid.New(), uuid.New()
		mustExec(ctx, t, pool,
			`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind)
			 VALUES ($1, $2, $3, $4, 'do', 'd', 'completed', 'issue')`, runID, userID, repoID, iid)
		mustExec(ctx, t, pool,
			`INSERT INTO run_reviews (id, target_run_id, user_id, verdict) VALUES ($1, $2, $3, 'issues')`,
			reviewID, runID, userID)
		for _, rc := range recs {
			mustExec(ctx, t, pool,
				`INSERT INTO review_recommendations (review_id, category, target, rationale_md, confidence)
				 VALUES ($1, $2, $3, 'r', 'high')`, reviewID, rc[0], rc[1])
		}
		reviewIDs = append(reviewIDs, reviewID)
		return reviewID
	}
	// Isolation: these seed improve_uzi recs, which ListOpenImproveUziRecommendations reads
	// INSTANCE-WIDE — leave them and a later test's global backlog assertion breaks. Cascade
	// them all away at the end.
	defer func() {
		for _, id := range reviewIDs {
			mustExec(ctx, t, pool, `DELETE FROM run_reviews WHERE id = $1`, id)
		}
	}()

	// U1: a frequency gradient — alpha×3, beta×2, gamma×1 — plus an empty target (excluded)
	// and an improve_agent rec (wrong category, excluded). Owner is a single user, so read
	// the user id back off the seeded run_reviews row.
	r1 := seed(201, [][2]string{
		{"improve_uzi", "alpha"}, {"improve_uzi", "alpha"}, {"improve_uzi", "alpha"},
		{"improve_uzi", "beta"}, {"improve_uzi", "beta"},
		{"improve_uzi", "gamma"},
		{"improve_uzi", ""},          // empty target — excluded
		{"improve_agent", "notmenu"}, // wrong category — excluded
	})
	u1 := scalarUUID(ctx, t, pool, `SELECT user_id FROM run_reviews WHERE id = $1`, r1)

	// U2: a distinct owner with its own target that must never appear in U1's menu.
	r2 := seed(202, [][2]string{{"improve_uzi", "secret-u2"}})
	u2 := scalarUUID(ctx, t, pool, `SELECT user_id FROM run_reviews WHERE id = $1`, r2)

	// Full menu for U1: frequency desc, target asc tiebreak, empty + wrong-category excluded.
	got, err := q.ListKnownImproveUziTargetsForUser(ctx, store.ListKnownImproveUziTargetsForUserParams{UserID: u1, Lim: 50})
	if err != nil {
		t.Fatalf("ListKnownImproveUziTargetsForUser(u1): %v", err)
	}
	want := []string{"alpha", "beta", "gamma"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("u1 menu = %v, want %v (freq desc; empty + wrong-category excluded)", got, want)
	}

	// Owner scoping: U2 sees only its own target; U1's targets never leak in.
	gotU2, err := q.ListKnownImproveUziTargetsForUser(ctx, store.ListKnownImproveUziTargetsForUserParams{UserID: u2, Lim: 50})
	if err != nil {
		t.Fatalf("ListKnownImproveUziTargetsForUser(u2): %v", err)
	}
	if len(gotU2) != 1 || gotU2[0] != "secret-u2" {
		t.Fatalf("u2 menu = %v, want exactly [secret-u2]", gotU2)
	}
	if contains(got, "secret-u2") {
		t.Errorf("u1 menu must NOT contain another user's target, got %v", got)
	}

	// The @lim cap truncates deterministically to the top of the frequency ranking.
	capped, err := q.ListKnownImproveUziTargetsForUser(ctx, store.ListKnownImproveUziTargetsForUserParams{UserID: u1, Lim: 2})
	if err != nil {
		t.Fatalf("ListKnownImproveUziTargetsForUser(u1, cap 2): %v", err)
	}
	if strings.Join(capped, ",") != "alpha,beta" {
		t.Fatalf("capped menu = %v, want [alpha beta]", capped)
	}
}

func scalarText(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sql string, args ...any) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(ctx, sql, args...).Scan(&s); err != nil {
		t.Fatalf("scalarText %q: %v", sql, err)
	}
	return s
}

func scalarInt(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		t.Fatalf("scalarInt %q: %v", sql, err)
	}
	return n
}

func scalarUUID(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sql string, args ...any) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
		t.Fatalf("scalarUUID %q: %v", sql, err)
	}
	return id
}
