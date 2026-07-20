package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/apitypes"
	mw "gitlab.example.com/vtmocanu/uzi/api/internal/middleware"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
	"gitlab.example.com/vtmocanu/uzi/api/internal/workersvc"
)

// Fake-backed unit tests for the PRD #98 M2 bulk fan-out. Scope is deliberate: everything
// that is TRUE OF THE REQUEST — the enum gate, the item cap, de-duplication, what reaches
// the store and what never does — lives here, where it runs on every `go test ./...`.
// Everything that is true of the DATA — owner scoping, the fan-out, idempotence, scope
// semantics — lives in judge_bulk_disposition_livedb_test.go instead, because a fake store
// ignores the `user_id` predicate those guarantees rest on and would report success either
// way. Splitting them this way keeps each assertion in the only place it can actually fail.

// bulkStore is a workersvc.Store that records the resolve params and the upserts. It
// embeds workersvc.Store (nil), so any method outside the bulk path panics — the same
// structural "no spend, no forge write" proof the #94 tests use.
type bulkStore struct {
	workersvc.Store

	// members is what the (owner-scoped) resolve returns, keyed "category/target".
	members map[string][]store.ListOwnedRecommendationsForCoordsRow
	// backlogRows backs the post-write re-read.
	backlogRows []store.ListJudgeRecommendationRowsForUserRow

	resolveArgs []store.ListOwnedRecommendationsForCoordsParams
	upserted    []store.UpsertRecommendationDispositionParams
	calls       []string

	// writeArgs captures the multi-row write's params; failUpsert makes that one
	// statement fail.
	writeArgs  []store.UpsertDispositionsForResolvedCoordsParams
	failUpsert bool
}

func (s *bulkStore) ListOwnedRecommendationsForCoords(_ context.Context, arg store.ListOwnedRecommendationsForCoordsParams) ([]store.ListOwnedRecommendationsForCoordsRow, error) {
	s.calls = append(s.calls, "ListOwnedRecommendationsForCoords")
	s.resolveArgs = append(s.resolveArgs, arg)
	out := []store.ListOwnedRecommendationsForCoordsRow{}
	// Model the query's pairwise zip: row i is (categories[i], targets[i]).
	for i := range arg.Categories {
		out = append(out, s.members[arg.Categories[i]+"/"+arg.Targets[i]]...)
	}
	return out, nil
}

// UpsertDispositionsForResolvedCoords models the ONE multi-row write. It records the
// arrays as individual coordinates so the existing assertions read unchanged, and
// failUpsert makes the whole statement fail — which is the only failure mode there is now
// that the write is a single statement.
func (s *bulkStore) UpsertDispositionsForResolvedCoords(_ context.Context, arg store.UpsertDispositionsForResolvedCoordsParams) (int64, error) {
	s.calls = append(s.calls, "UpsertDispositionsForResolvedCoords")
	s.writeArgs = append(s.writeArgs, arg)
	if s.failUpsert {
		// Nothing is recorded: a single statement that errors wrote NOTHING.
		return 0, errors.New("upsert exploded")
	}
	for i := range arg.ReviewIds {
		s.upserted = append(s.upserted, store.UpsertRecommendationDispositionParams{
			ReviewID: arg.ReviewIds[i], Category: arg.Categories[i], Target: arg.Targets[i],
			Status: arg.Status, DismissReason: arg.DismissReason,
			RationaleHash: arg.RationaleHashes[i], SetByUserID: arg.SetByUserID,
		})
	}
	return int64(len(arg.ReviewIds)), nil
}

func (s *bulkStore) ListJudgeRecommendationRowsForUser(_ context.Context, _ store.ListJudgeRecommendationRowsForUserParams) ([]store.ListJudgeRecommendationRowsForUserRow, error) {
	s.calls = append(s.calls, "ListJudgeRecommendationRowsForUser")
	return s.backlogRows, nil
}

func (s *bulkStore) ListJudgeTriageRowsForUser(_ context.Context, _ uuid.UUID) ([]store.ListJudgeTriageRowsForUserRow, error) {
	s.calls = append(s.calls, "ListJudgeTriageRowsForUser")
	return []store.ListJudgeTriageRowsForUserRow{}, nil
}

// memberRow builds one resolved member of a coordinate, on its OWN review.
//
// Read memberRowIn before adding tests: this helper mints a fresh ReviewID per call, so
// members built with it can never share a (review_id, category, target) triple. That is a
// REAL limit, not a detail — the duplicate-coordinate crash (SQLSTATE 21000) was invisible
// to every fake-backed test precisely because this helper cannot construct the shape that
// breaks the single-statement write. A helper that cannot build the failing input silently
// bounds every test written on top of it.
func memberRow(category, target, disposition string, filedSettled bool) store.ListOwnedRecommendationsForCoordsRow {
	return memberRowIn(uuid.New(), category, target, disposition, filedSettled)
}

// memberRowIn builds a member on a CALLER-CHOSEN review, so a test can put two members of
// the same coordinate on ONE review — the duplicate the schema permits and the judge emits.
func memberRowIn(reviewID uuid.UUID, category, target, disposition string, filedSettled bool) store.ListOwnedRecommendationsForCoordsRow {
	row := store.ListOwnedRecommendationsForCoordsRow{
		ReviewID: reviewID, RecID: uuid.New(),
		Category: category, Target: target, RationaleMd: "because",
		FiledSettled: filedSettled,
	}
	if disposition != "" {
		row.DispositionStatus = pgtype.Text{String: disposition, Valid: true}
	}
	return row
}

// oneOpenMemberStore resolves a single open member of (install_worker_tool, rg).
func oneOpenMemberStore() *bulkStore {
	return &bulkStore{members: map[string][]store.ListOwnedRecommendationsForCoordsRow{
		"install_worker_tool/rg": {memberRow("install_worker_tool", "rg", "", false)},
	}}
}

func bulkPut(user store.User, body string) *http.Request {
	r := httptest.NewRequest(http.MethodPut, "/api/me/judge/recommendations/disposition", strings.NewReader(body))
	return r.WithContext(mw.ContextWithUser(r.Context(), user))
}

// ---- the request gate ----------------------------------------------------------------

// TestBulkDispositionValidation walks the request contract. Every rejection must happen
// BEFORE the store is touched: a 400 that already wrote half a fan-out would be worse than
// no validation at all.
func TestBulkDispositionValidation(t *testing.T) {
	item := `{"category":"install_worker_tool","target":"rg"}`
	cases := []struct {
		name string
		body string
		want int
	}{
		{"no items", `{"items":[],"status":"done"}`, http.StatusBadRequest},
		{"missing items", `{"status":"done"}`, http.StatusBadRequest},
		{"bad status", `{"items":[` + item + `],"status":"resolved"}`, http.StatusBadRequest},
		{"dismissed without a reason", `{"items":[` + item + `],"status":"dismissed"}`, http.StatusBadRequest},
		{"dismissed with a bogus reason", `{"items":[` + item + `],"status":"dismissed","reason":"because"}`, http.StatusBadRequest},
		{"done carrying a reason", `{"items":[` + item + `],"status":"done","reason":"wont_do"}`, http.StatusBadRequest},
		{"bad scope", `{"items":[` + item + `],"status":"done","scope":"everything"}`, http.StatusBadRequest},
		{"unknown field", `{"items":[` + item + `],"status":"done","extra":1}`, http.StatusBadRequest},
		{"valid done", `{"items":[` + item + `],"status":"done"}`, http.StatusOK},
		{"valid dismissed", `{"items":[` + item + `],"status":"dismissed","reason":"not_an_issue"}`, http.StatusOK},
		{"valid explicit scope", `{"items":[` + item + `],"status":"done","scope":"all"}`, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := oneOpenMemberStore()
			h := newRunsHandler(t, st)
			rec := httptest.NewRecorder()
			h.BulkSetDispositions(rec, bulkPut(store.User{ID: uuid.New()}, tc.body))
			if rec.Code != tc.want {
				t.Fatalf("PUT %s = %d, want %d; body=%s", tc.body, rec.Code, tc.want, rec.Body.String())
			}
			if tc.want == http.StatusBadRequest && len(st.calls) != 0 {
				t.Fatalf("a rejected request must not reach the store, calls=%v", st.calls)
			}
		})
	}
}

// TestBulkDispositionItemCap: over JudgeDispositionMaxItems is a 400 and never reaches the
// store — this endpoint is N resolves + N upserts on the no-CSRF token path, so the work is
// bounded explicitly rather than by the body limit. Exactly at the cap is allowed, so the
// boundary is not off by one.
func TestBulkDispositionItemCap(t *testing.T) {
	body := func(n int) string {
		items := make([]string, 0, n)
		for i := 0; i < n; i++ {
			items = append(items, fmt.Sprintf(`{"category":"improve_uzi","target":"t%d"}`, i))
		}
		return `{"items":[` + strings.Join(items, ",") + `],"status":"done"}`
	}
	t.Run("at the cap is allowed", func(t *testing.T) {
		st := oneOpenMemberStore()
		h := newRunsHandler(t, st)
		rec := httptest.NewRecorder()
		h.BulkSetDispositions(rec, bulkPut(store.User{ID: uuid.New()}, body(workersvc.JudgeDispositionMaxItems)))
		if rec.Code != http.StatusOK {
			t.Fatalf("exactly the cap = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
	})
	// The cap counts DISTINCT work, not body length — which is the whole point of PF-2 and
	// is only true because the cap check runs AFTER dedupeCoords. Moving the check before
	// the dedup leaves every other test in the suite green (they all use distinct targets),
	// so without this subtest nothing pins the ordering.
	t.Run("over-cap raw items that dedupe to within the cap are accepted", func(t *testing.T) {
		st := oneOpenMemberStore()
		h := newRunsHandler(t, st)
		items := make([]string, 0, workersvc.JudgeDispositionMaxItems*2)
		for i := 0; i < workersvc.JudgeDispositionMaxItems*2; i++ {
			// Two copies each of exactly `cap` distinct coordinates.
			items = append(items, fmt.Sprintf(`{"category":"improve_uzi","target":"t%d"}`, i%workersvc.JudgeDispositionMaxItems))
		}
		rec := httptest.NewRecorder()
		h.BulkSetDispositions(rec, bulkPut(store.User{ID: uuid.New()},
			`{"items":[`+strings.Join(items, ",")+`],"status":"done"}`))
		if rec.Code != http.StatusOK {
			t.Fatalf("%d raw items deduping to %d = %d, want 200 — the cap must count distinct work",
				len(items), workersvc.JudgeDispositionMaxItems, rec.Code)
		}
		if n := len(st.resolveArgs[0].Categories); n != workersvc.JudgeDispositionMaxItems {
			t.Fatalf("resolve received %d coordinates, want the %d distinct ones", n, workersvc.JudgeDispositionMaxItems)
		}
	})
	t.Run("over the cap is 400 and writes nothing", func(t *testing.T) {
		st := oneOpenMemberStore()
		h := newRunsHandler(t, st)
		rec := httptest.NewRecorder()
		h.BulkSetDispositions(rec, bulkPut(store.User{ID: uuid.New()}, body(workersvc.JudgeDispositionMaxItems+1)))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("over the cap = %d, want 400", rec.Code)
		}
		for _, c := range st.calls {
			if c == "UpsertRecommendationDisposition" {
				t.Fatalf("an over-cap request must write nothing, calls=%v", st.calls)
			}
		}
	})
}

// TestBulkDispositionServiceRejectsUnknownScope pins the SERVICE-side gate independently of
// the handler's (PRD #98 review A8). The handler already rejects an unknown scope, so this
// calls the service directly — the only way to observe the second layer.
//
// It matters because scope has no safe default: the service selects members, so a plain
// `scope == ScopeOpen` comparison treats anything unrecognised as ScopeAll, which
// RE-ASSERTS over settled verdicts. That was found by widening the handler's validator in a
// mutation and watching the request come back `{"updated":1,…}` — an unknown scope silently
// writing. Contrast `bucket`, which fails closed naturally (it matches no group). Both
// layers exist so neither is load-bearing alone.
func TestBulkDispositionServiceRejectsUnknownScope(t *testing.T) {
	st := oneOpenMemberStore()
	h := newRunsHandler(t, st)
	items := []workersvc.JudgeDispositionCoord{{Category: "install_worker_tool", Target: "rg"}}

	_, err := h.wsvc.BulkSetDispositions(context.Background(), uuid.New(), items, "done", "", "everything")
	if !errors.Is(err, workersvc.ErrInvalidScope) {
		t.Fatalf("service err = %v, want ErrInvalidScope — an unknown scope must NOT fall through "+
			"to `all` semantics and re-assert over settled verdicts", err)
	}
	if len(st.calls) != 0 {
		t.Fatalf("a rejected scope must not reach the store at all, calls=%v", st.calls)
	}
	// Positive control: the two accepted values do proceed, so the refusal above is the
	// scope check and not a broken fixture.
	for _, ok := range []string{workersvc.ScopeOpen, workersvc.ScopeAll} {
		if _, err := h.wsvc.BulkSetDispositions(context.Background(), uuid.New(), items, "done", "", ok); err != nil {
			t.Fatalf("scope %q must be accepted, got %v", ok, err)
		}
	}
}

// ---- what reaches the store ----------------------------------------------------------

// TestBulkDispositionWritesResolvedCoordinateNotBody is the unit-level guard on the
// invariant migrations 00071/00073 depend on: neither table has a category CHECK, on the
// stated grounds that the handler "never accepts a category from the request body — it
// reads it off the resolved recommendation". Here the resolve deliberately returns a
// member whose category/target differ from what the body asked for; the upsert must carry
// the RESOLVED values. (The live-DB sibling proves the same thing end to end against real
// constraints.)
func TestBulkDispositionWritesResolvedCoordinateNotBody(t *testing.T) {
	st := &bulkStore{members: map[string][]store.ListOwnedRecommendationsForCoordsRow{
		// The body will ask for this key, but the row carries canonical values.
		"install_worker_tool/rg": {memberRow("install_worker_tool", "ripgrep", "", false)},
	}}
	h := newRunsHandler(t, st)

	rec := httptest.NewRecorder()
	h.BulkSetDispositions(rec, bulkPut(store.User{ID: uuid.New()},
		`{"items":[{"category":"install_worker_tool","target":"rg"}],"status":"done"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(st.upserted) != 1 {
		t.Fatalf("want one upsert, got %d", len(st.upserted))
	}
	got := st.upserted[0]
	if got.Target != "ripgrep" {
		t.Fatalf("upsert target = %q, want the RESOLVED row's %q — the request body must never "+
			"reach the coordinate columns", got.Target, "ripgrep")
	}
	// The hash is re-stamped from the resolved rec's CURRENT rationale (#94 Decision 3).
	if got.RationaleHash != workersvc.RationaleHash("because") {
		t.Errorf("rationale_hash = %q, want the hash of the resolved rationale_md", got.RationaleHash)
	}
	if got.Status != "done" || got.DismissReason.Valid {
		t.Errorf("a done must carry a NULL reason, got %+v", got)
	}
}

// TestBulkDispositionResolveIsOwnerScopedAndDeduped: the resolve is called ONCE, with the
// caller's own id and with the coordinates de-duplicated. De-duplication matters twice
// over — a repeated coordinate must not resolve (and therefore upsert) twice, and the cap
// must count distinct work rather than body length.
func TestBulkDispositionResolveIsOwnerScopedAndDeduped(t *testing.T) {
	st := oneOpenMemberStore()
	caller := store.User{ID: uuid.New()}
	h := newRunsHandler(t, st)

	rec := httptest.NewRecorder()
	h.BulkSetDispositions(rec, bulkPut(caller,
		`{"items":[{"category":"install_worker_tool","target":"rg"},`+
			`{"category":"install_worker_tool","target":"rg"}],"status":"done"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(st.resolveArgs) != 1 {
		t.Fatalf("want a single resolve round-trip, got %d", len(st.resolveArgs))
	}
	arg := st.resolveArgs[0]
	if arg.UserID != caller.ID {
		t.Fatalf("resolve scoped to %v, want the caller %v", arg.UserID, caller.ID)
	}
	if len(arg.Categories) != 1 || len(arg.Targets) != 1 {
		t.Fatalf("a repeated coordinate must be de-duplicated before the resolve, got %v/%v", arg.Categories, arg.Targets)
	}
	if len(st.upserted) != 1 {
		t.Fatalf("a repeated coordinate must not double the fan-out, got %d upserts", len(st.upserted))
	}
}

// TestBulkDispositionScopeSkipsSettledMembers: scope=open (the default) writes only members
// the SHARED ladder buckets as todo — an unsettled filed claim is still todo, a SETTLED
// filed link is not, and an already-disposed member is not. scope=all re-asserts over all
// of them. This is the ladder being reused rather than re-decided in the fan-out.
func TestBulkDispositionScopeSkipsSettledMembers(t *testing.T) {
	newStore := func() *bulkStore {
		return &bulkStore{members: map[string][]store.ListOwnedRecommendationsForCoordsRow{
			"improve_uzi/docs": {
				memberRow("improve_uzi", "docs", "", false),          // todo
				memberRow("improve_uzi", "docs", "", true),           // settled filed → not open
				memberRow("improve_uzi", "docs", "dismissed", false), // settled → not open
			},
		}}
	}
	body := `{"items":[{"category":"improve_uzi","target":"docs"}],"status":"done"`

	st := newStore()
	h := newRunsHandler(t, st)
	rec := httptest.NewRecorder()
	h.BulkSetDispositions(rec, bulkPut(store.User{ID: uuid.New()}, body+`}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("scope=open PUT = %d, want 200", rec.Code)
	}
	if len(st.upserted) != 1 {
		t.Fatalf("scope=open wrote %d members, want 1 (only the todo one)", len(st.upserted))
	}

	st = newStore()
	h = newRunsHandler(t, st)
	rec = httptest.NewRecorder()
	h.BulkSetDispositions(rec, bulkPut(store.User{ID: uuid.New()}, body+`,"scope":"all"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("scope=all PUT = %d, want 200", rec.Code)
	}
	if len(st.upserted) != 3 {
		t.Fatalf("scope=all wrote %d members, want all 3", len(st.upserted))
	}
}

// ---- partial failure ---------------------------------------------------------------

// TestBulkDispositionFailedWriteChangesNothing pins the all-or-nothing contract the
// single-statement write buys (PRD #98 M2, audit NB-A superseding review finding N). The
// fan-out is ONE multi-row upsert, so a failure cannot leave some members written: the
// endpoint answers 500 and the database is untouched.
//
// The earlier loop could half-apply, and the best that could be said was "a 500 claims no
// success". This asserts the stronger property outright — zero rows changed — which is
// what makes the response's silence honest rather than merely non-committal.
func TestBulkDispositionFailedWriteChangesNothing(t *testing.T) {
	st := &bulkStore{members: map[string][]store.ListOwnedRecommendationsForCoordsRow{
		"improve_uzi/docs": {
			memberRow("improve_uzi", "docs", "", false),
			memberRow("improve_uzi", "docs", "", false),
			memberRow("improve_uzi", "docs", "", false),
		},
	}}
	st.failUpsert = true
	h := newRunsHandler(t, st)

	rec := httptest.NewRecorder()
	h.BulkSetDispositions(rec, bulkPut(store.User{ID: uuid.New()},
		`{"items":[{"category":"improve_uzi","target":"docs"}],"status":"done"}`))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("PUT with a failing write = %d, want 500; body=%s", rec.Code, rec.Body.String())
	}
	// Exactly ONE statement was attempted for all three members — not three.
	if len(st.writeArgs) != 1 {
		t.Fatalf("want a single write statement, got %d", len(st.writeArgs))
	}
	if n := len(st.writeArgs[0].ReviewIds); n != 3 {
		t.Fatalf("the one statement must carry all 3 members, got %d", n)
	}
	// And nothing landed.
	if len(st.upserted) != 0 {
		t.Fatalf("a failed single statement must write NOTHING, got %d rows", len(st.upserted))
	}
	if body := rec.Body.String(); !strings.Contains(body, "internal error") {
		t.Fatalf("body = %s, want the generic internal error", body)
	}
	var got apitypes.JudgeDispositionResultDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err == nil && got.Updated != 0 {
		t.Fatalf("a failed request must not report updated=%d", got.Updated)
	}
}

// TestBulkDispositionIsOneRoundTrip is audit NB-A's headline: the number of STATEMENTS is
// independent of the number of members. The item cap bounds coordinates, but one
// coordinate can match every occurrence across every review the caller owns, so a
// per-member loop was unbounded round-trips from a ~4 KB body.
func TestBulkDispositionIsOneRoundTrip(t *testing.T) {
	members := make([]store.ListOwnedRecommendationsForCoordsRow, 0, 500)
	for i := 0; i < 500; i++ {
		members = append(members, memberRow("improve_uzi", "docs", "", false))
	}
	st := &bulkStore{members: map[string][]store.ListOwnedRecommendationsForCoordsRow{"improve_uzi/docs": members}}
	h := newRunsHandler(t, st)

	rec := httptest.NewRecorder()
	h.BulkSetDispositions(rec, bulkPut(store.User{ID: uuid.New()},
		`{"items":[{"category":"improve_uzi","target":"docs"}],"status":"done"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	writes := 0
	for _, c := range st.calls {
		if c == "UpsertDispositionsForResolvedCoords" {
			writes++
		}
	}
	if writes != 1 {
		t.Fatalf("500 members took %d write statements, want exactly 1 — the fan-out must not "+
			"scale round-trips with member count", writes)
	}
	if got := decodeBulk(t, rec).Updated; got != 500 {
		t.Fatalf("updated = %d, want 500 (rows-affected from the one statement)", got)
	}
}

// TestBulkDispositionDedupesMembersWithinAReview is the fake-backed half of the
// duplicate-coordinate guard, and it exists because the fixture used to make it
// unwritable. review_recommendations has no unique constraint on the coordinate, so one
// review can carry (improve_uzi, docs) twice; feeding both into the single multi-row
// ON CONFLICT DO UPDATE raises SQLSTATE 21000 and the endpoint 500s.
//
// The resolve's DISTINCT ON is the first layer and only a live DB can exercise it. This
// asserts the SECOND layer — the Go dedup — which is what holds if the query is ever
// relaxed or a second caller appears.
//
// The negative control matters as much as the positive: the SAME coordinate on DIFFERENT
// reviews must NOT be collapsed, because that recurrence is the cross-run fan-out this
// whole PRD exists for. A dedup keyed on (category, target) instead of the full triple
// would pass the first assertion and silently destroy the feature.
func TestBulkDispositionDedupesMembersWithinAReview(t *testing.T) {
	sharedReview := uuid.New()
	st := &bulkStore{members: map[string][]store.ListOwnedRecommendationsForCoordsRow{
		"improve_uzi/docs": {
			// Same coordinate, SAME review — the illegal-to-write duplicate.
			memberRowIn(sharedReview, "improve_uzi", "docs", "", false),
			memberRowIn(sharedReview, "improve_uzi", "docs", "", false),
			// Same coordinate, DIFFERENT review — a legitimate second occurrence.
			memberRowIn(uuid.New(), "improve_uzi", "docs", "", false),
		},
	}}
	h := newRunsHandler(t, st)

	rec := httptest.NewRecorder()
	h.BulkSetDispositions(rec, bulkPut(store.User{ID: uuid.New()},
		`{"items":[{"category":"improve_uzi","target":"docs"}],"status":"done"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(st.writeArgs) != 1 {
		t.Fatalf("want one write statement, got %d", len(st.writeArgs))
	}
	arg := st.writeArgs[0]
	if len(arg.ReviewIds) != 2 {
		t.Fatalf("wrote %d members, want 2 — the same-review duplicate must collapse and the "+
			"other review's occurrence must survive; got review ids %v", len(arg.ReviewIds), arg.ReviewIds)
	}
	// No triple may repeat — that is what SQLSTATE 21000 punishes.
	seen := map[string]bool{}
	for i := range arg.ReviewIds {
		k := arg.ReviewIds[i].String() + "/" + arg.Categories[i] + "/" + arg.Targets[i]
		if seen[k] {
			t.Fatalf("duplicate (review_id, category, target) reached the write: %s", k)
		}
		seen[k] = true
	}
	// And the cross-run occurrence is still there.
	distinctReviews := map[uuid.UUID]bool{}
	for _, id := range arg.ReviewIds {
		distinctReviews[id] = true
	}
	if len(distinctReviews) != 2 {
		t.Fatalf("want both reviews represented, got %d — deduping on (category, target) "+
			"instead of the full triple would collapse the cross-run fan-out", len(distinctReviews))
	}
}

// ---- the response leaks nothing (helper) ---------------------------------------------

func decodeBulk(t *testing.T, rec *httptest.ResponseRecorder) apitypes.JudgeDispositionResultDTO {
	t.Helper()
	var got apitypes.JudgeDispositionResultDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	return got
}

// ---- the response leaks nothing --------------------------------------------------------

// TestBulkDispositionNoExistenceOracle: a coordinate that resolves to nothing — absent, or
// another user's — produces a 200 whose body is indistinguishable from a request that
// asked only for coordinates that do not exist. In particular there is no per-item status
// array (#94 Decision 5's one-404 rule: with coordinates there is no id to 404 on, so a
// per-item breakdown WOULD be the oracle).
func TestBulkDispositionNoExistenceOracle(t *testing.T) {
	st := &bulkStore{members: map[string][]store.ListOwnedRecommendationsForCoordsRow{}}
	h := newRunsHandler(t, st)

	rec := httptest.NewRecorder()
	h.BulkSetDispositions(rec, bulkPut(store.User{ID: uuid.New()},
		`{"items":[{"category":"improve_uzi","target":"absent"}],"status":"done"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("unresolvable coordinate = %d, want 200 (not a 404 — that would be the oracle)", rec.Code)
	}
	var got apitypes.JudgeDispositionResultDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Updated != 0 || len(got.Groups) != 0 {
		t.Fatalf("result = %+v, want zero members and no groups", got)
	}
	if len(st.upserted) != 0 {
		t.Fatal("an unresolvable coordinate must write nothing")
	}
	// The wire shape carries no per-item detail at all.
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode raw: %v", err)
	}
	for _, k := range []string{"items", "results", "errors", "not_found", "skipped"} {
		if _, present := raw[k]; present {
			t.Fatalf("response carries a per-item field %q — that reconstructs the existence oracle", k)
		}
	}
}

// ---- no spend, no forge write ----------------------------------------------------------

// TestBulkDispositionTouchesStoreOnly: the whole fan-out calls only the resolve, the
// disposition upsert, and the two reads that rebuild the response. The nil embedded Store
// would panic on anything else; here the call set is asserted positively, which is the
// Success Criterion "no Anthropic token is spent by any disposition action" in unit form.
func TestBulkDispositionTouchesStoreOnly(t *testing.T) {
	st := oneOpenMemberStore()
	h := newRunsHandler(t, st)

	rec := httptest.NewRecorder()
	h.BulkSetDispositions(rec, bulkPut(store.User{ID: uuid.New()},
		`{"items":[{"category":"install_worker_tool","target":"rg"}],"status":"dismissed","reason":"wont_do"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	allowed := map[string]bool{
		"ListOwnedRecommendationsForCoords":   true, // the owner-scoped resolve
		"UpsertDispositionsForResolvedCoords": true, // the ONLY write, and it is ONE statement
		"ListJudgeRecommendationRowsForUser":  true, // response groups
		"ListJudgeTriageRowsForUser":          true, // response triage
	}
	for _, c := range st.calls {
		if !allowed[c] {
			t.Fatalf("the fan-out called %q — no run-create, enqueue or forge method may be reachable; calls=%v", c, st.calls)
		}
	}
}
