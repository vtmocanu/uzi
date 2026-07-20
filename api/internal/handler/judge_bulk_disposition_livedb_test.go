package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"gitlab.example.com/vtmocanu/uzi/api/internal/apitypes"
	"gitlab.example.com/vtmocanu/uzi/api/internal/config"
	mw "gitlab.example.com/vtmocanu/uzi/api/internal/middleware"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
	"gitlab.example.com/vtmocanu/uzi/api/internal/workersvc"
)

// The live-DB half of PRD #98 M2. The bulk fan-out's whole security story lives in ONE
// SQL predicate — `WHERE rv.user_id = @user_id` in ListOwnedRecommendationsForCoords — and
// a fake store cannot defend it: a fake returns its fixture regardless of the user id it is
// handed, so deleting that predicate would leave every fake-backed test green. Worse, the
// authz case the PRD calls for (a uza_ admin_ro token aimed at ANOTHER user's rows) has no
// id to 404 on: coordinates are not ids, so the response is a 200 with zero members either
// way. A status-only assertion is therefore VACUOUS — the only honest assertion is at the
// DB level, on whether the victim's row changed. That is what these tests do.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; ./e2e/run-store-it.sh
// provides one and sweeps this package for the LiveDB suffix.

func bulkDispositionLiveDB(t *testing.T) (*Handler, *pgxpool.Pool, *store.Queries) {
	t.Helper()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	q := store.New(pool)
	box := newHandlerTestBox(t)
	return &Handler{pool: pool, q: q, box: box, cfg: config.Config{}, wsvc: workersvc.New(q, box, workersvc.Params{})}, pool, q
}

// bulkFixture seeds one user with `runs` judged runs, each carrying every coordinate in
// coords, and returns the user id. Fresh uuids per call — the LiveDB runner shares one
// database across the whole suite, so nothing may collide with a sibling test.
func bulkFixture(ctx context.Context, t *testing.T, pool *pgxpool.Pool, runs int, coords ...[2]string) uuid.UUID {
	t.Helper()
	userID, connID, repoID := uuid.New(), uuid.New(), uuid.New()
	mustExecT(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("bulkdisp-%s@e2e", userID))
	mustExecT(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	mustExecT(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, repoID, connID)
	for i := 0; i < runs; i++ {
		runID, reviewID := uuid.New(), uuid.New()
		mustExecT(ctx, t, pool,
			`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status)
			 VALUES ($1, $2, $3, $4, $5, 'd', 'completed')`, runID, userID, repoID, i+1, fmt.Sprintf("run %d", i))
		mustExecT(ctx, t, pool,
			`INSERT INTO run_reviews (id, target_run_id, user_id, verdict) VALUES ($1, $2, $3, 'issues')`,
			reviewID, runID, userID)
		for _, c := range coords {
			mustExecT(ctx, t, pool,
				`INSERT INTO review_recommendations (review_id, category, target, rationale_md)
				 VALUES ($1, $2, $3, 'because')`, reviewID, c[0], c[1])
		}
	}
	return userID
}

// dispositionRowsFor reads the disposition rows a user actually holds, straight from the
// table — the ground truth every assertion below is made against.
func dispositionRowsFor(ctx context.Context, t *testing.T, pool *pgxpool.Pool, userID uuid.UUID) map[string]string {
	t.Helper()
	rows, err := pool.Query(ctx,
		`SELECT d.category, d.target, d.status
		   FROM recommendation_dispositions d
		   JOIN run_reviews rv ON rv.id = d.review_id
		  WHERE rv.user_id = $1`, userID)
	if err != nil {
		t.Fatalf("read dispositions: %v", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var cat, tgt, status string
		if err := rows.Scan(&cat, &tgt, &status); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out[cat+"/"+tgt] = status
	}
	return out
}

func bulkReq(t *testing.T, user store.User, body string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPut, "/api/me/judge/recommendations/disposition", strings.NewReader(body))
	return r.WithContext(mw.ContextWithUser(r.Context(), user))
}

func doBulk(t *testing.T, h *Handler, user store.User, body string) (*httptest.ResponseRecorder, apitypes.JudgeDispositionResultDTO) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.BulkSetDispositions(rec, bulkReq(t, user, body))
	var got apitypes.JudgeDispositionResultDTO
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
		}
	}
	return rec, got
}

// ---- 1. the fan-out itself ----------------------------------------------------------

// TestBulkDispositionFansOutAcrossRunsLiveDB: ONE call on ONE coordinate writes a
// disposition on EVERY run that coordinate recurs in (PRD #98 Decision 3's whole point —
// the same idea triaged once instead of N times), and the returned group re-renders at its
// new rollup with a fresh triage tally.
func TestBulkDispositionFansOutAcrossRunsLiveDB(t *testing.T) {
	h, pool, _ := bulkDispositionLiveDB(t)
	ctx := context.Background()
	rg, coder := [2]string{"install_worker_tool", "rg"}, [2]string{"improve_agent", "coder"}
	userID := bulkFixture(ctx, t, pool, 3, rg, coder)
	user := store.User{ID: userID}

	rec, got := doBulk(t, h, user, `{"items":[{"category":"install_worker_tool","target":"rg"}],"status":"dismissed","reason":"wont_do"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got.Updated != 3 {
		t.Fatalf("updated = %d, want 3 (one per run the coordinate recurs in)", got.Updated)
	}
	rows := dispositionRowsFor(ctx, t, pool, userID)
	if rows["install_worker_tool/rg"] != "dismissed" {
		t.Fatalf("rg disposition = %q, want dismissed; rows=%v", rows["install_worker_tool/rg"], rows)
	}
	// The untouched coordinate must be untouched — the fan-out is per requested item.
	if _, ok := rows["improve_agent/coder"]; ok {
		t.Fatalf("a coordinate that was not requested must not be disposed; rows=%v", rows)
	}
	// The response re-renders the acted-on group only, at its new rollup.
	if len(got.Groups) != 1 || got.Groups[0].Target != "rg" || got.Groups[0].Bucket != "dismissed" {
		t.Fatalf("groups = %+v, want just the rg group at its new dismissed rollup", got.Groups)
	}
	if got.Groups[0].OpenCount != 0 {
		t.Errorf("open_count = %d, want 0 after dismissing every member", got.Groups[0].OpenCount)
	}
	// triage is the recomputed canonical tally: 6 recs, 3 now dismissed, 3 still to do.
	if got.Triage.Total != 6 || got.Triage.Dismissed != 3 || got.Triage.Todo != 3 {
		t.Fatalf("triage = %+v, want total 6 / dismissed 3 / todo 3", got.Triage)
	}
}

// ---- 2. the authz matrix, asserted where it is meaningful ---------------------------

// TestBulkDispositionOwnerOnlyLiveDB is the PRD's owner-only matrix. Every leg asserts on
// the DATABASE, not on the status code: with coordinates there is no id to 404 on, so all
// four legs return 200 and only the rows tell the truth.
//
//   - a plain non-owner writes nothing on the victim's coordinate;
//   - a non-owner ADMIN (modelling a uza_ admin_ro token, which keeps IsAdmin on
//     RequireUser) ALSO writes nothing — IsAdmin buys no bypass because it is never
//     consulted;
//   - that same admin CAN dispose its own identical coordinate, proving the refusal above
//     is ownership and not some unrelated failure;
//   - and the victim's pre-existing disposition is byte-for-byte unchanged throughout.
func TestBulkDispositionOwnerOnlyLiveDB(t *testing.T) {
	h, pool, _ := bulkDispositionLiveDB(t)
	ctx := context.Background()
	rg := [2]string{"install_worker_tool", "rg"}

	victim := bulkFixture(ctx, t, pool, 1, rg)
	// The victim settles their own coordinate first, so there is a row to clobber.
	if _, got := doBulk(t, h, store.User{ID: victim},
		`{"items":[{"category":"install_worker_tool","target":"rg"}],"status":"done"}`); got.Updated != 1 {
		t.Fatalf("victim's own disposition did not land: updated=%d", got.Updated)
	}
	before := dispositionRowsFor(ctx, t, pool, victim)
	if before["install_worker_tool/rg"] != "done" {
		t.Fatalf("fixture: victim's row = %q, want done", before["install_worker_tool/rg"])
	}

	body := `{"items":[{"category":"install_worker_tool","target":"rg"}],"status":"dismissed","reason":"not_an_issue","scope":"all"}`

	// (a) a plain non-owner, holding no rows of their own.
	stranger := bulkFixture(ctx, t, pool, 0)
	rec, got := doBulk(t, h, store.User{ID: stranger}, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("non-owner PUT = %d, want 200 (no oracle: it simply matches nothing)", rec.Code)
	}
	if got.Updated != 0 || len(got.Groups) != 0 {
		t.Fatalf("non-owner result = %+v, want zero members and no groups", got)
	}
	if now := dispositionRowsFor(ctx, t, pool, victim); now["install_worker_tool/rg"] != "done" {
		t.Fatalf("a non-owner CHANGED the victim's row to %q — the resolve is not owner-scoped", now["install_worker_tool/rg"])
	}

	// (b) a non-owner ADMIN — the uza_ admin_ro case. Same silence, same untouched row.
	adminOther := bulkFixture(ctx, t, pool, 1, rg) // holds its OWN copy of the coordinate
	rec, got = doBulk(t, h, store.User{ID: adminOther, IsAdmin: true}, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("non-owner admin PUT = %d, want 200", rec.Code)
	}
	if now := dispositionRowsFor(ctx, t, pool, victim); now["install_worker_tool/rg"] != "done" {
		t.Fatalf("a non-owner ADMIN overwrote the victim's row (%q) — IsAdmin must buy no bypass",
			now["install_worker_tool/rg"])
	}
	// (c) ...and the very same call DID settle the admin's OWN identical coordinate, so
	// (b) is ownership scoping and not a silently broken request.
	if got.Updated != 1 {
		t.Fatalf("the admin's own coordinate must still be disposed: updated=%d", got.Updated)
	}
	if own := dispositionRowsFor(ctx, t, pool, adminOther); own["install_worker_tool/rg"] != "dismissed" {
		t.Fatalf("admin's own row = %q, want dismissed", own["install_worker_tool/rg"])
	}
}

// ---- 3. scope ------------------------------------------------------------------------

// TestBulkDispositionScopeLiveDB: scope=open (the default) settles only members the shared
// ladder buckets as todo, leaving an already-settled member's verdict alone; scope=all
// re-asserts over it. Proven on a PARTIAL group — some members settled, some open — which
// is the case the PRD calls out.
func TestBulkDispositionScopeLiveDB(t *testing.T) {
	h, pool, _ := bulkDispositionLiveDB(t)
	ctx := context.Background()
	rg := [2]string{"install_worker_tool", "rg"}
	userID := bulkFixture(ctx, t, pool, 2, rg)
	user := store.User{ID: userID}

	// Settle ONE of the two members by hand, so the group is partial.
	var reviewID uuid.UUID
	if err := pool.QueryRow(ctx,
		`SELECT rv.id FROM run_reviews rv WHERE rv.user_id = $1 ORDER BY rv.created_at ASC LIMIT 1`,
		userID).Scan(&reviewID); err != nil {
		t.Fatalf("pick a review: %v", err)
	}
	mustExecT(ctx, t, pool,
		`INSERT INTO recommendation_dispositions (review_id, category, target, status, dismiss_reason, rationale_hash)
		 VALUES ($1, 'install_worker_tool', 'rg', 'dismissed', 'wont_do', 'stale-hash')`, reviewID)

	// scope=open: the dismissed member is NOT open, so only the other one is marked done.
	_, got := doBulk(t, h, user, `{"items":[{"category":"install_worker_tool","target":"rg"}],"status":"done"}`)
	if got.Updated != 1 {
		t.Fatalf("scope=open updated %d members, want 1 (the settled member is not open)", got.Updated)
	}
	var dismissedStill int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM recommendation_dispositions d JOIN run_reviews rv ON rv.id = d.review_id
		  WHERE rv.user_id = $1 AND d.status = 'dismissed'`, userID).Scan(&dismissedStill); err != nil {
		t.Fatalf("count: %v", err)
	}
	if dismissedStill != 1 {
		t.Fatalf("scope=open overwrote a settled member's verdict (dismissed rows now %d, want 1)", dismissedStill)
	}

	// scope=all re-asserts across every member, including the dismissed one.
	_, got = doBulk(t, h, user, `{"items":[{"category":"install_worker_tool","target":"rg"}],"status":"done","scope":"all"}`)
	if got.Updated != 2 {
		t.Fatalf("scope=all updated %d members, want 2 (it re-asserts)", got.Updated)
	}
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM recommendation_dispositions d JOIN run_reviews rv ON rv.id = d.review_id
		  WHERE rv.user_id = $1 AND d.status = 'dismissed'`, userID).Scan(&dismissedStill); err != nil {
		t.Fatalf("count: %v", err)
	}
	if dismissedStill != 0 {
		t.Fatalf("scope=all left %d dismissed rows, want 0", dismissedStill)
	}
}

// ---- 4. idempotence ------------------------------------------------------------------

// TestBulkDispositionIdempotentLiveDB: calling twice with scope=all converges — the second
// call rewrites the same coordinates rather than inserting duplicates (#94 Decision 6's
// upsert). Row COUNT is the assertion, because a broken upsert would show up as growth.
func TestBulkDispositionIdempotentLiveDB(t *testing.T) {
	h, pool, _ := bulkDispositionLiveDB(t)
	ctx := context.Background()
	rg := [2]string{"install_worker_tool", "rg"}
	userID := bulkFixture(ctx, t, pool, 2, rg)
	user := store.User{ID: userID}
	body := `{"items":[{"category":"install_worker_tool","target":"rg"}],"status":"done","scope":"all"}`

	_, first := doBulk(t, h, user, body)
	_, second := doBulk(t, h, user, body)
	if first.Updated != 2 || second.Updated != 2 {
		t.Fatalf("updated = %d then %d, want 2 both times", first.Updated, second.Updated)
	}
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM recommendation_dispositions d JOIN run_reviews rv ON rv.id = d.review_id
		  WHERE rv.user_id = $1`, userID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Fatalf("disposition rows = %d after two identical calls, want 2 (idempotent upsert)", n)
	}
	if first.Triage != second.Triage {
		t.Fatalf("triage moved on a repeat call: %+v then %+v", first.Triage, second.Triage)
	}
}

// ---- 4b. a human write clears the sync's provenance -----------------------------------

// TestBulkDispositionClearsIssueCloseProvenanceLiveDB covers an interaction M6 introduced
// and #94's upsert did not anticipate: once `set_via` exists, a DO UPDATE that does not
// touch it CARRIES IT OVER. So a coordinate auto-resolved by the Filed→Done sync
// (set_via='issue_close') would keep that provenance after a human overrode it, and the
// panel would label the user's own verdict "done via #IID" — attributing their decision to
// the system.
//
// Both human write paths (this bulk one and #94's single-coordinate route) now reset
// set_via, so a person's verdict always reads as a person's.
func TestBulkDispositionClearsIssueCloseProvenanceLiveDB(t *testing.T) {
	h, pool, _ := bulkDispositionLiveDB(t)
	ctx := context.Background()
	rg := [2]string{"install_worker_tool", "rg"}
	userID := bulkFixture(ctx, t, pool, 1, rg)
	user := store.User{ID: userID}

	// Model what the M6 sync writes: a system 'done' with issue_close provenance.
	var reviewID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM run_reviews WHERE user_id = $1`, userID).Scan(&reviewID); err != nil {
		t.Fatalf("pick review: %v", err)
	}
	mustExecT(ctx, t, pool,
		`INSERT INTO recommendation_dispositions
		     (review_id, category, target, status, rationale_hash, set_by_user_id, set_via)
		 VALUES ($1, 'install_worker_tool', 'rg', 'done', 'h', NULL, 'issue_close')`, reviewID)

	// The user disagrees and dismisses it. scope=all, since the member is already settled.
	if _, got := doBulk(t, h, user,
		`{"items":[{"category":"install_worker_tool","target":"rg"}],"status":"dismissed",`+
			`"reason":"not_an_issue","scope":"all"}`); got.Updated != 1 {
		t.Fatalf("the human override did not land: updated=%d", got.Updated)
	}

	var status string
	var setVia *string
	var setBy *uuid.UUID
	if err := pool.QueryRow(ctx,
		`SELECT status, set_via, set_by_user_id FROM recommendation_dispositions
		  WHERE review_id = $1 AND category = 'install_worker_tool' AND target = 'rg'`,
		reviewID).Scan(&status, &setVia, &setBy); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if status != "dismissed" {
		t.Fatalf("status = %q, want dismissed", status)
	}
	if setVia != nil {
		t.Fatalf("set_via = %q, want NULL — a human write must clear the sync's provenance, "+
			"or the panel labels the user's own verdict \"done via #IID\"", *setVia)
	}
	if setBy == nil || *setBy != userID {
		t.Fatalf("set_by_user_id = %v, want the user who made the call", setBy)
	}
}

// ---- 4c. a duplicated coordinate does not crash the single-statement write ------------

// TestBulkDispositionHandlesDuplicateCoordinateLiveDB guards a runtime crash that the
// single-statement rewrite introduced and that NO fake can surface.
//
// review_recommendations has no unique constraint on (review_id, category, target), so a
// judge may legitimately emit the same coordinate twice in ONE review. The per-member loop
// was immune — each upsert was its own statement — but feeding both rows into one multi-row
// `ON CONFLICT DO UPDATE` makes Postgres raise SQLSTATE 21000, "ON CONFLICT DO UPDATE
// command cannot affect row a second time". It would have fired only for users whose judge
// happened to duplicate a coordinate: rare, data-dependent, and invisible to every unit
// test. dedupeCoords does NOT protect against it — that dedupes the REQUEST, while the
// duplication arises inside the RESOLVED member set.
//
// The resolve's DISTINCT ON is the fix, and this is the test that would have caught it.
func TestBulkDispositionHandlesDuplicateCoordinateLiveDB(t *testing.T) {
	h, pool, _ := bulkDispositionLiveDB(t)
	ctx := context.Background()
	userID := bulkFixture(ctx, t, pool, 1, [2]string{"install_worker_tool", "rg"})
	user := store.User{ID: userID}

	// The SAME coordinate a second time in the SAME review — what a repetitive judge emits.
	var reviewID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM run_reviews WHERE user_id = $1`, userID).Scan(&reviewID); err != nil {
		t.Fatalf("pick review: %v", err)
	}
	mustExecT(ctx, t, pool,
		`INSERT INTO review_recommendations (review_id, category, target, rationale_md)
		 VALUES ($1, 'install_worker_tool', 'rg', 'said it twice')`, reviewID)
	var recs int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM review_recommendations
		  WHERE review_id = $1 AND category = 'install_worker_tool' AND target = 'rg'`,
		reviewID).Scan(&recs); err != nil {
		t.Fatalf("count recs: %v", err)
	}
	if recs != 2 {
		t.Fatalf("fixture: want 2 recommendations on one coordinate, got %d", recs)
	}

	rec, got := doBulk(t, h, user, `{"items":[{"category":"install_worker_tool","target":"rg"}],"status":"done"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d, want 200 — a duplicated coordinate must not blow up the write; body=%s",
			rec.Code, rec.Body.String())
	}
	// ONE disposition, because the coordinate IS the disposition's key: two recommendations
	// sharing a coordinate share one verdict.
	if got.Updated != 1 {
		t.Fatalf("updated = %d, want 1 — the duplicate must collapse to a single member", got.Updated)
	}
	if rows := dispositionRowsFor(ctx, t, pool, userID); len(rows) != 1 || rows["install_worker_tool/rg"] != "done" {
		t.Fatalf("disposition rows = %v, want exactly one done row", rows)
	}
}

// ---- 5. the body never becomes a coordinate ------------------------------------------

// TestBulkDispositionRejectsBodyCoordinateLiveDB is the live-DB proof of the invariant
// migrations 00071/00073 rely on: "the handler never accepts a category from the request
// body — it reads it off the resolved recommendation". Neither table has a category CHECK,
// on those exact grounds, so a body-echo bug would land free text in the coordinate columns
// with nothing to stop it.
//
// THE REQUEST MUST CARRY A RESOLVABLE ITEM, and that is not incidental. An earlier version
// sent only bogus items — so zero members resolved, the upsert loop body never executed,
// and the test stayed GREEN against an endpoint deliberately rewired to echo the body
// (found by mutation, PRD #98 M2 review). With a real coordinate alongside, the loop runs
// at least once, so a body-echo bug writes the bogus category under the resolvable member
// and the count below catches it. A test that cannot reach the code it names proves
// nothing.
func TestBulkDispositionRejectsBodyCoordinateLiveDB(t *testing.T) {
	h, pool, _ := bulkDispositionLiveDB(t)
	ctx := context.Background()
	userID := bulkFixture(ctx, t, pool, 1, [2]string{"install_worker_tool", "rg"})
	user := store.User{ID: userID}

	rec, got := doBulk(t, h, user,
		`{"items":[{"category":"'; DROP TABLE runs; --","target":"anything"},`+
			`{"category":"not_a_real_category","target":"rg"},`+
			// The resolvable one: it makes the upsert loop actually execute.
			`{"category":"install_worker_tool","target":"rg"}],"status":"done"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d, want 200 (a bogus coordinate is not an error, it just matches nothing)", rec.Code)
	}
	if got.Updated != 1 {
		t.Fatalf("updated = %d, want exactly 1 — only the resolvable coordinate may write, and it MUST "+
			"write, or this test cannot observe a body-echo bug at all", got.Updated)
	}
	// Scoped to THIS caller's reviews, not the whole table. The store-IT runner shares one
	// database across the entire suite, so an unscoped count is assertion-by-coincidence:
	// any sibling test — or, as happened here, a leftover row from a mutation run against
	// the same database — turns it red for reasons that have nothing to do with this code.
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM recommendation_dispositions d
		   JOIN run_reviews rv ON rv.id = d.review_id
		  WHERE rv.user_id = $1
		    AND d.category NOT IN ('enable_tool','install_worker_tool','adjust_template',
		                           'improve_agent','add_agent','improve_uzi')`, userID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("%d disposition row(s) carry a category outside the six-enum set — a request body "+
			"reached the coordinate columns, which is exactly what 00071/00073 assume cannot happen", n)
	}
	// Exactly the resolved coordinate was written, under its OWN category/target.
	rows := dispositionRowsFor(ctx, t, pool, userID)
	if len(rows) != 1 || rows["install_worker_tool/rg"] != "done" {
		t.Fatalf("disposition rows = %v, want exactly the resolved install_worker_tool/rg coordinate", rows)
	}
}
