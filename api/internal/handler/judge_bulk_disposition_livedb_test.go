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
			// DISTINCT rationale per (run, coordinate). It used to be the literal 'because'
			// on every row, which silently bounded every test built on this fixture — the
			// memberRowIn lesson from 136acb53, in this file's neighbour, not carried
			// forward. Specifically it made the rationale_md projection pin UNABLE TO FAIL:
			// folding `rr.rationale_md -> 'because'::text` left the stamped hash and the
			// read-back expectation both sha256("because"), so the test could not tell "SQL
			// projected the column" from "SQL returned a constant that happens to equal the
			// fixture". Measured GREEN under that fold before this change, and RED after it —
			// on a fresh database, at both the per-coordinate hash assertion and the
			// two-hashes-must-differ one in
			// TestBulkDispositionStampsHashOfTheCurrentRationaleLiveDB below.
			//
			// Uniqueness is per ROW, not per coordinate, so a fold to ANY constant collapses
			// a pair that the assertions compare.
			mustExecT(ctx, t, pool,
				`INSERT INTO review_recommendations (review_id, category, target, rationale_md)
				 VALUES ($1, $2, $3, $4)`, reviewID, c[0], c[1],
				fmt.Sprintf("rationale for %s/%s in run %d of %s", c[0], c[1], i, userID))
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

// ---- 3b. a FILED member is not open, and the filed row is COORDINATE-scoped ----------

// TestBulkDispositionFiledMemberIsNotOpenLiveDB pins the one rung of Decision 2's ladder
// that no live exercise of this endpoint reached: `filed_settled` — the boolean
// ListOwnedRecommendationsForCoords computes from its `recommendation_filed_issues` LEFT
// JOIN and BucketOf turns into the `filed` rung.
//
// WHY IT WAS WORTH ADDING, stated as the measurement rather than the conclusion: before
// this test, `grep -c "recommendation_filed_issues\|filed_settled"` over this whole file
// returned 0. No live bulk fixture had ever inserted a filed row, so `f` matched nothing on
// every row of every live run and `filed_settled` was FALSE everywhere. The rung was pinned
// only by fakes — and a fake takes the boolean as a PARAMETER, so it cannot be wrong about
// where the boolean comes from. That is the shape this branch kept finding: the assertion
// existed, the code path did not execute.
//
// The fixture is one review holding TWO coordinates with a filed row on exactly ONE, which
// is what makes the assertion discriminating in BOTH directions. `updated` is the
// load-bearing number:
//
//   - drop `AND f.category = rr.category AND f.target = rr.target` from the join (keeping
//     `f.review_id = rv.id`) and the single filed row cross-matches its SIBLING coordinate
//     too, so BOTH members bucket `filed`, neither is open, and updated = 0;
//   - drop the join or the `filed_at IS NOT NULL` projection and NOTHING is filed, so both
//     members are open and updated = 2.
//
// A one-coordinate fixture would catch only the second. The coordinate half is exactly the
// predicate the PRD's Remaining Work records as asserted-but-unexercised on the M1 read
// query; this pins the bulk query's own copy of it, which is a different query body.
//
// ✅ RE-DERIVED AT `041c5291`, AND THE REASON IS THE POINT. The two folds above were first
// measured at `31080a40`; FIVE migrations from main landed between there and the landing merge
// (six paths appear in the diff — the sixth is PRD #98's own file renumbered 00075 -> 00081 with
// identical content, which `git diff --name-status -M` separates as R100 from the five A lines), so
// by the expiry rule in .claude/agent-team.md they had been measured against DDL the suite no
// longer applies and certified nothing about today's tree. Re-run, one fold per run, each on
// its OWN fresh throwaway Postgres, each confirmed present in both the .sql and the
// regenerated .sql.go by `git diff --numstat`, each compiled (`sqlc generate` + `go vet`)
// before being believed:
//
//	drop `AND f.category = rr.category AND f.target = rr.target`  RED, updated = 0
//	`(f.filed_at IS NOT NULL)::bool` -> `false::bool`             RED, updated = 2
//
// Both still redden, both still with the SAME `updated` signature, and each reddened EXACTLY
// this one test — measured as FAIL=1 against a whole-sweep RUN=141 PASS=140 SKIP=0, with the
// unmutated baseline RUN=141 PASS=141 FAIL=0 SKIP=0 on the same tree. The 141 is `041c5291`'s
// inventory and nothing more: it was 126 at `8c6be2b8`, 128 at `c1fcdfce`, 129 at `31080a40`.
// "each fold reddens exactly one test" is the claim; the tally is only its receipt.
//
// Record the unchanged result rather than deleting the note: "re-derived after six migrations,
// unchanged" is a stronger artifact than the original measurement, and it is the evidence for
// re-folding rather than the excuse to skip it next time. Surviving once is not a licence.
//
// The scope=all leg is not decoration: it proves the skip above was the LADDER refusing an
// already-filed member, not a resolve that silently failed to find it. Same argument as leg
// (c) of the owner-only matrix.
func TestBulkDispositionFiledMemberIsNotOpenLiveDB(t *testing.T) {
	h, pool, _ := bulkDispositionLiveDB(t)
	ctx := context.Background()
	rg, coder := [2]string{"install_worker_tool", "rg"}, [2]string{"improve_agent", "coder"}
	userID := bulkFixture(ctx, t, pool, 1, rg, coder)
	user := store.User{ID: userID}

	// One review, both coordinates. The filed row lands on rg ONLY.
	var reviewID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM run_reviews WHERE user_id = $1`, userID).Scan(&reviewID); err != nil {
		t.Fatalf("pick review: %v", err)
	}
	mustExecT(ctx, t, pool,
		`INSERT INTO recommendation_filed_issues
		     (review_id, category, target, filed_issue_iid, filed_issue_url, filed_at, filing_since)
		 VALUES ($1, 'install_worker_tool', 'rg', 4242, 'https://forge.e2e/i/4242', now(), now())`,
		reviewID)
	// Fixture precondition — without it every assertion below is vacuous, and the whole
	// point of this test is that the precondition was silently absent for the entire file.
	var filedRows int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM recommendation_filed_issues WHERE review_id = $1 AND filed_at IS NOT NULL`,
		reviewID).Scan(&filedRows); err != nil {
		t.Fatalf("count filed rows: %v", err)
	}
	if filedRows != 1 {
		t.Fatalf("fixture broken: %d settled filed rows on the review, want exactly 1 — otherwise this test proves nothing", filedRows)
	}

	body := `{"items":[{"category":"install_worker_tool","target":"rg"},{"category":"improve_agent","target":"coder"}],"status":"done"}`
	rec, got := doBulk(t, h, user, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got.Updated != 1 {
		t.Fatalf("updated = %d, want 1 — scope=open must settle the UNFILED sibling only. "+
			"0 means the filed row cross-matched its sibling coordinate (the join's coordinate half is gone); "+
			"2 means nothing read as filed at all", got.Updated)
	}
	rows := dispositionRowsFor(ctx, t, pool, userID)
	if rows["improve_agent/coder"] != "done" {
		t.Fatalf("the unfiled sibling = %q, want done; rows=%v", rows["improve_agent/coder"], rows)
	}
	if s, ok := rows["install_worker_tool/rg"]; ok {
		t.Fatalf("the FILED coordinate got a %q disposition; scope=open must leave it alone; rows=%v", s, rows)
	}

	// The re-read groups agree with the ladder: rg rolls up `filed`, its sibling `done`.
	// (This half rides the M1 read query, not the bulk resolve, so it does not redden under
	// the folds above — it is here so the response the client renders is checked too.)
	byTarget := map[string]string{}
	for _, g := range got.Groups {
		byTarget[g.Target] = g.Bucket
	}
	if byTarget["rg"] != "filed" || byTarget["coder"] != "done" {
		t.Fatalf("group rollups = %v, want rg=filed and coder=done", byTarget)
	}
	if got.Triage.Filed != 1 || got.Triage.Done != 1 || got.Triage.Todo != 0 {
		t.Fatalf("triage = %+v, want filed 1 / done 1 / todo 0", got.Triage)
	}

	// scope=all reaches the filed member, so the skip above was the ladder and not a
	// resolve that quietly found nothing.
	_, all := doBulk(t, h, user,
		`{"items":[{"category":"install_worker_tool","target":"rg"}],"status":"done","scope":"all"}`)
	if all.Updated != 1 {
		t.Fatalf("scope=all updated %d on the filed coordinate, want 1 — the member resolves fine, "+
			"it was scope=open that declined it", all.Updated)
	}
	if rows := dispositionRowsFor(ctx, t, pool, userID); rows["install_worker_tool/rg"] != "done" {
		t.Fatalf("scope=all left the filed coordinate at %q, want done; rows=%v", rows["install_worker_tool/rg"], rows)
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
		                           'improve_agent','add_agent','improve_uzi','cost_efficiency')`, userID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("%d disposition row(s) carry a category outside the seven-enum set — a request body "+
			"reached the coordinate columns, which is exactly what 00071/00073 assume cannot happen", n)
	}
	// Exactly the resolved coordinate was written, under its OWN category/target.
	rows := dispositionRowsFor(ctx, t, pool, userID)
	if len(rows) != 1 || rows["install_worker_tool/rg"] != "done" {
		t.Fatalf("disposition rows = %v, want exactly the resolved install_worker_tool/rg coordinate", rows)
	}
}

// ---- 5. the settled projection is a WORKING ADDRESS ---------------------------------

// TestBulkDispositionSettledIsAWorkingUndoAddressLiveDB pins the one property of
// `settled` that no fake can hold: that the (run_id, rec_id) pair it returns actually
// RESOLVES — that the query put target_run_id in run_id and rr.id in rec_id, and not the
// other way round.
//
// This test exists because swapping those two aliases in judge_bulk_disposition.sql and
// regenerating leaves the ENTIRE Go suite green (measured, PRD #98 BLK-UNDO audit). Every
// other `settled` test runs against bulkStore, whose rows are `RunID: uuid.New(), RecID:
// uuid.New()` — a fake cannot observe which COLUMN landed in which FIELD, because it never
// ran the query. The file header already made this argument for the owner predicate; it is
// equally true of the projection.
//
// The production failure is a SILENT SUCCESS, which is why a no-op is not harmless here.
// Undo would DELETE /api/runs/<recUUID>/review/recommendations/<runUUID>/disposition;
// resolveOwnedRecommendation finds no run with that id; ErrRunNotFound becomes a 404; the
// web client correctly swallows 404 as "already undone" (the right behaviour for its
// intended case); `failed` stays 0; the honest-partial-failure reporter therefore says
// nothing; load() refreshes — and the user is told the undo succeeded while nothing was
// reverted. The direction is fail-safe (a no-op, never a wrong deletion), but the report is
// a lie either way.
//
// So the assertion is end to end and on the DATABASE: settle a coordinate, feed the pair
// STRAIGHT from res.Settled into the same DeleteDisposition the client calls, and require
// the row to be gone. Nothing here is spelled by hand — a test that reconstructed the pair
// from the fixture would pass under the swap exactly as the fakes do.
func TestBulkDispositionSettledIsAWorkingUndoAddressLiveDB(t *testing.T) {
	h, pool, _ := bulkDispositionLiveDB(t)
	ctx := context.Background()
	rg := [2]string{"install_worker_tool", "rg"}

	userID := bulkFixture(ctx, t, pool, 2, rg)
	_, got := doBulk(t, h, store.User{ID: userID},
		`{"items":[{"category":"install_worker_tool","target":"rg"}],"status":"done"}`)
	if got.Updated != 2 {
		t.Fatalf("fixture: updated = %d, want 2 (one per run)", got.Updated)
	}
	if len(got.Settled) != got.Updated {
		t.Fatalf("settled has %d entries for %d updated coordinates — they must describe the same set",
			len(got.Settled), got.Updated)
	}
	if before := dispositionRowsFor(ctx, t, pool, userID); before["install_worker_tool/rg"] != "done" {
		t.Fatalf("fixture: disposition did not land, rows = %v", before)
	}

	// Undo through EVERY returned address, exactly as the client does. Parsing is part of
	// the contract: these ship as strings and the route parses them as uuids.
	for i, m := range got.Settled {
		runID, err := uuid.Parse(m.RunID)
		if err != nil {
			t.Fatalf("settled[%d].run_id %q does not parse as a uuid: %v", i, m.RunID, err)
		}
		recID, err := uuid.Parse(m.RecID)
		if err != nil {
			t.Fatalf("settled[%d].rec_id %q does not parse as a uuid: %v", i, m.RecID, err)
		}
		if err := h.wsvc.DeleteDisposition(ctx, userID, runID, recID); err != nil {
			// The swapped-alias failure lands exactly here, as ErrRunNotFound → the 404 the
			// client swallows.
			t.Fatalf("settled[%d] (run=%s rec=%s) is not a resolvable undo address: %v — "+
				"if this is ErrRunNotFound, the query's run_id/rec_id projection is swapped and "+
				"every Undo is a silent no-op", i, m.RunID, m.RecID, err)
		}
	}

	// Ground truth: the rows are gone. Scoped to this fixture's user, never table-wide —
	// the LiveDB runner shares one database and sibling fixtures accumulate.
	if after := dispositionRowsFor(ctx, t, pool, userID); len(after) != 0 {
		t.Fatalf("after undoing every settled address, %d disposition row(s) remain: %v", len(after), after)
	}
}

// The pair must address the coordinate the write TOUCHED, not merely some row of the
// caller's. With two runs sharing a coordinate, undoing the first returned address must
// clear exactly one review's disposition and leave the other standing — so a projection that
// returned a constant, or the same run twice, fails here even though every id resolves.
func TestBulkDispositionSettledAddressesAreDistinctPerRunLiveDB(t *testing.T) {
	h, pool, _ := bulkDispositionLiveDB(t)
	ctx := context.Background()
	rg := [2]string{"install_worker_tool", "rg"}

	userID := bulkFixture(ctx, t, pool, 2, rg)
	_, got := doBulk(t, h, store.User{ID: userID},
		`{"items":[{"category":"install_worker_tool","target":"rg"}],"status":"done"}`)
	if len(got.Settled) != 2 {
		t.Fatalf("settled = %d entries, want 2", len(got.Settled))
	}
	if got.Settled[0].RunID == got.Settled[1].RunID {
		t.Fatalf("both settled addresses name the same run (%s) — the fan-out spans two runs",
			got.Settled[0].RunID)
	}
	if got.Settled[0].RecID == got.Settled[1].RecID {
		t.Fatalf("both settled addresses name the same recommendation (%s)", got.Settled[0].RecID)
	}

	runID := uuid.MustParse(got.Settled[0].RunID)
	recID := uuid.MustParse(got.Settled[0].RecID)
	if err := h.wsvc.DeleteDisposition(ctx, userID, runID, recID); err != nil {
		t.Fatalf("undo of the first settled address failed: %v", err)
	}

	// One review's row cleared, the other untouched: count the rows directly, since
	// dispositionRowsFor keys by coordinate and both reviews share one.
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM recommendation_dispositions d
		   JOIN run_reviews rv ON rv.id = d.review_id
		  WHERE rv.user_id = $1`, userID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("after undoing ONE of two settled addresses, %d disposition rows remain, want 1", n)
	}
}

// ---- 6. the bulk write's rationale_md projection ------------------------------------

// TestBulkDispositionStampsHashOfTheCurrentRationaleLiveDB pins the LAST unpinned column on
// this query: `rr.rationale_md AS rationale_md`, which the fan-out hashes into
// recommendation_dispositions.rationale_hash — #94 Decision 3's staleness key.
//
// Blanking it to ” left the entire Go suite green (measured, PRD #98 review). The reason is
// the same fake blindness as everywhere else, in a sharper form:
// judge_bulk_disposition_test.go asserts `got.RationaleHash != workersvc.RationaleHash("because")`,
// and "because" is what memberRow puts in the FIXTURE — so it compares a hash of the fake's
// own input against the fake's own input. The query never runs, so no column mapping is
// observed.
//
// Both drift directions are user-visible and neither errors:
//
//   - a wrong value at write time makes the coordinate read STALE immediately — the run page
//     tells the user "the recommendation changed since you resolved it" the instant they
//     resolve it;
//   - a value that does NOT track the rationale makes a genuinely-changed recommendation read
//     as current, so the user never learns their verdict is out of date. That is the stale
//     flag's entire purpose, silently inverted, and it is the worse direction.
//
// The assertion is the property rather than a literal: the stored hash must equal
// RationaleHash of the recommendation's CURRENT rationale_md, read back from the table.
//
// WHAT ACTUALLY MAKES THAT WORK — and this paragraph replaces a SUPERSEDED criterion that
// stood here and taught the wrong lesson. It used to read: "a test spelling \"because\" here
// would pass under the blanking exactly as the fake one does — the value has to come from
// the database." That is drawn from an experiment whose independent variable could not
// affect the outcome, and it sends the next reader hunting for LITERALS.
//
// THE FIXTURE IS THE PRECONDITION, and it comes first. While bulkFixture wrote 'because' on
// every row, "read the expected value back from the table" and "spell it in the test" were
// LITERALLY THE SAME EXPRESSION — both evaluate to sha256("because") — so no assertion style
// could have rescued it, and the discriminating fold (`rr.rationale_md -> 'because'::text`,
// a value the fixture already contained) passed. Blanking to the empty string would have
// been caught by anything; that is why blanking proves nothing.
//
// So the two things that carry this test are, in order: (1) bulkFixture giving every ROW a
// distinct rationale, and (2) the pairwise assertion below — two coordinates with different
// texts must stamp DIFFERENT hashes, which fails under a fold to ANY constant whichever
// constant is chosen.
//
// The fold is `rr.rationale_md` -> `'because'::text` at
// internal/store/queries/judge_bulk_disposition.sql:111 — the BULK query, not the backlog
// one, which projects a column of the same name. Two results, and they are not equally
// strong: RED after those two changes, at both assertions below, MEASURED 2026-07-21 on a
// fresh database. GREEN before them, INHERITED from the M3 checkpoint's earlier sweep and
// never re-run against the pre-fix tree.
//
// SCOPE OF THAT SWEEP, stated so it is not read as broader than it is: this fixed the
// LIVE-DB half only. memberRowIn in judge_bulk_disposition_test.go still writes
// RationaleMd: "because" on every fake member, and the fake suite still asserts against
// RationaleHash("because"). That is the same uniform-fixture class and it is still live —
// materially weaker, because a fake cannot observe column mapping at all, but not swept.
func TestBulkDispositionStampsHashOfTheCurrentRationaleLiveDB(t *testing.T) {
	h, pool, _ := bulkDispositionLiveDB(t)
	ctx := context.Background()
	rg := [2]string{"install_worker_tool", "rg"}
	docs := [2]string{"improve_uzi", "docs"}

	// TWO coordinates, and bulkFixture now gives them different rationale texts. That is what
	// makes this discriminating: a projection folded to ANY single constant gives both
	// coordinates the SAME stamped hash, which the final assertion catches regardless of
	// which constant was chosen. Asserting only "hash equals the read-back text" was not
	// enough while the fixture made every row identical.
	userID := bulkFixture(ctx, t, pool, 1, rg, docs)
	if _, got := doBulk(t, h, store.User{ID: userID},
		`{"items":[{"category":"install_worker_tool","target":"rg"},{"category":"improve_uzi","target":"docs"}],"status":"done"}`); got.Updated != 2 {
		t.Fatalf("fixture: updated = %d, want 2 (one per coordinate)", got.Updated)
	}

	type stamped struct{ hash, rationale string }
	rows := map[string]stamped{}
	q, err := pool.Query(ctx,
		`SELECT d.target, d.rationale_hash, rr.rationale_md
		   FROM recommendation_dispositions d
		   JOIN run_reviews rv ON rv.id = d.review_id
		   JOIN review_recommendations rr
		     ON rr.review_id = d.review_id AND rr.category = d.category AND rr.target = d.target
		  WHERE rv.user_id = $1`, userID)
	if err != nil {
		t.Fatalf("read back the stamped hashes: %v", err)
	}
	defer q.Close()
	for q.Next() {
		var target, hash, rationale string
		if err := q.Scan(&target, &hash, &rationale); err != nil {
			t.Fatalf("scan: %v", err)
		}
		rows[target] = stamped{hash, rationale}
	}
	if err := q.Err(); err != nil {
		t.Fatalf("iterate: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("read back %d disposition rows, want 2", len(rows))
	}

	// The fixture must actually differ, or everything below is vacuous again.
	if rows[rg[1]].rationale == rows[docs[1]].rationale {
		t.Fatalf("fixture broken: both coordinates carry the same rationale %q — this test cannot "+
			"discriminate a folded projection from a real one", rows[rg[1]].rationale)
	}

	// Each stamped hash is the hash of ITS OWN current text, read back from the table.
	//
	// Reading it back rather than spelling it is worth doing, but do NOT credit it as the
	// defence — that is the superseded criterion the doc comment above corrects. On a fixture
	// where every row said 'because', reading back and spelling it were the same expression.
	// What defends this loop is that bulkFixture now gives every row a DISTINCT text; the
	// pairwise check below is what makes a constant fold unsurvivable.
	for target, got := range rows {
		if want := workersvc.RationaleHash(got.rationale); got.hash != want {
			t.Errorf("%s: stamped rationale_hash = %q, want RationaleHash of its CURRENT text %q = %q — "+
				"the write hashed something other than rr.rationale_md, so this coordinate reads STALE "+
				"the moment it is set", target, got.hash, got.rationale, want)
		}
	}

	// A corroborating assertion, NOT the discriminating one — the credit is corrected here.
	//
	// It used to be labelled "THE DISCRIMINATING ASSERTION", and that was wrong for a reason
	// worth keeping: the Fatalf twenty lines up already establishes the two rationales differ,
	// so two hashes can only collide when at least one has ALREADY diverged from the hash of
	// its own text — which the loop above catches first, as an Errorf, on the same run. The
	// measured fold reddened BOTH together; no fault reddens this one alone. An assertion
	// sitting behind an earlier check is documentation of the property, not the gate on it.
	// (Fifth instance of this class on the branch; the previous four are in the checkpoint.)
	//
	// WHAT ACTUALLY PINS THE PROPERTY, so removing this line is not mistaken for a loss: the
	// expected value is read back through the hand-written query above, which does NOT pass
	// through the sqlc body being folded, and bulkFixture gives every row a distinct text. The
	// loop above then compares each stamped hash against its own coordinate's current text.
	// Keeping this line is still worth it — it states the pairwise property in one place a
	// reader can see — but it is a restatement, not the mechanism.
	if rows[rg[1]].hash == rows[docs[1]].hash {
		t.Errorf("both coordinates stamped the same rationale_hash %q despite different rationale text — "+
			"the projection is returning a constant, and every coordinate disposed through the bulk path "+
			"will read stale (or, worse, never read stale when the text genuinely changes)",
			rows[rg[1]].hash)
	}
}
