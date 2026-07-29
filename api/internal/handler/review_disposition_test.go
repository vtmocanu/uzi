package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/apitypes"
	mw "gitlab.example.com/vtmocanu/uzi/api/internal/middleware"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
	"gitlab.example.com/vtmocanu/uzi/api/internal/workersvc"
)

// dispStore is a minimal workersvc.Store for the PRD #94 disposition handler tests. It
// owns one run + its judged review, answers the owner-scoped resolve path, and captures
// the disposition write-path calls so a test can assert the wire values and whether the
// mutation ran at all. It embeds workersvc.Store (nil), so ANY method the disposition
// path does not use — every enqueue/forge/run-create method — panics if reached: that is
// the structural "no token spend, no forge write" proof (PRD #94 Success Criterion).
type dispStore struct {
	workersvc.Store

	ownerID uuid.UUID
	run     store.Run
	review  store.RunReview
	recs    []store.ReviewRecommendation
	filed   []store.RecommendationFiledIssue
	disps   []store.RecommendationDisposition

	// reviewErr, when set (pgx.ErrNoRows), models an UNJUDGED target — the run resolves
	// but carries no verdict. The zero value keeps the judged behaviour every pre-#119
	// test relies on.
	reviewErr error
	// pendingJudge backs the PRD #119 active-judge read the review READ path now makes.
	// nil means "no judge in flight" (pgx.ErrNoRows), which is what every pre-#119 test
	// expects and what keeps their assertions about the review half unchanged.
	pendingJudge *store.GetActiveJudgeRunForTargetRow

	// triageRows backs the global stats aggregate (ListJudgeTriageRowsForUser).
	triageRows   []store.ListJudgeTriageRowsForUserRow
	statsUserArg uuid.UUID

	// Captured write-path effects.
	upserted   []store.UpsertRecommendationDispositionParams
	deleted    []store.DeleteRecommendationDispositionParams
	deleteRows int64

	calls []string
}

func (s *dispStore) note(name string) { s.calls = append(s.calls, name) }

func (s *dispStore) GetRunByIDForUser(_ context.Context, arg store.GetRunByIDForUserParams) (store.Run, error) {
	s.note("GetRunByIDForUser")
	// Strict caller-ownership: the run resolves only for its owner. A non-owner (incl. a
	// uza_ admin_ro token, which the write path passes as isAdmin=false) gets ErrNoRows,
	// exactly as an unknown id.
	if arg.ID == s.run.ID && arg.UserID == s.ownerID {
		return s.run, nil
	}
	return store.Run{}, pgx.ErrNoRows
}

// GetRunByID models the ADMIN-visible path (owner-or-admin). The disposition write path
// must NEVER reach it (it hardcodes isAdmin=false), so if a regression started consulting
// IsAdmin, an admin caller would resolve the run here and the owner-only test would fail —
// which is exactly the assertion the matrix makes.
func (s *dispStore) GetRunByID(_ context.Context, id uuid.UUID) (store.Run, error) {
	s.note("GetRunByID")
	if id == s.run.ID {
		return s.run, nil
	}
	return store.Run{}, pgx.ErrNoRows
}

func (s *dispStore) GetRunReviewForTarget(_ context.Context, _ uuid.UUID) (store.RunReview, error) {
	s.note("GetRunReviewForTarget")
	if s.reviewErr != nil {
		return store.RunReview{}, s.reviewErr
	}
	return s.review, nil
}
func (s *dispStore) ListRecommendationsForReview(_ context.Context, _ uuid.UUID) ([]store.ReviewRecommendation, error) {
	s.note("ListRecommendationsForReview")
	return s.recs, nil
}
func (s *dispStore) ListFiledIssuesForReview(_ context.Context, _ uuid.UUID) ([]store.RecommendationFiledIssue, error) {
	s.note("ListFiledIssuesForReview")
	return s.filed, nil
}
func (s *dispStore) GetActiveJudgeRunForTarget(_ context.Context, _ pgtype.UUID) (store.GetActiveJudgeRunForTargetRow, error) {
	s.note("GetActiveJudgeRunForTarget")
	if s.pendingJudge == nil {
		return store.GetActiveJudgeRunForTargetRow{}, pgx.ErrNoRows
	}
	return *s.pendingJudge, nil
}
func (s *dispStore) ListDispositionsForReview(_ context.Context, _ uuid.UUID) ([]store.RecommendationDisposition, error) {
	s.note("ListDispositionsForReview")
	return s.disps, nil
}
func (s *dispStore) UpsertRecommendationDisposition(_ context.Context, arg store.UpsertRecommendationDispositionParams) (store.RecommendationDisposition, error) {
	s.note("UpsertRecommendationDisposition")
	s.upserted = append(s.upserted, arg)
	return store.RecommendationDisposition{
		ID: uuid.New(), ReviewID: arg.ReviewID, Category: arg.Category, Target: arg.Target,
		Status: arg.Status, DismissReason: arg.DismissReason, RationaleHash: arg.RationaleHash,
	}, nil
}
func (s *dispStore) DeleteRecommendationDisposition(_ context.Context, arg store.DeleteRecommendationDispositionParams) (int64, error) {
	s.note("DeleteRecommendationDisposition")
	s.deleted = append(s.deleted, arg)
	return s.deleteRows, nil
}
func (s *dispStore) ListJudgeTriageRowsForUser(_ context.Context, userID uuid.UUID) ([]store.ListJudgeTriageRowsForUserRow, error) {
	s.note("ListJudgeTriageRowsForUser")
	s.statsUserArg = userID
	return s.triageRows, nil
}

// ---- request builders (chi {id}+{recID} params, user in context) --------------------

func dispReq(method string, user store.User, runID, recID uuid.UUID, body string) *http.Request {
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, "/api/runs/x/review/recommendations/y/disposition", strings.NewReader(body))
	} else {
		r = httptest.NewRequest(method, "/api/runs/x/review/recommendations/y/disposition", nil)
	}
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", runID.String())
	rctx.URLParams.Add("recID", recID.String())
	ctx := context.WithValue(mw.ContextWithUser(r.Context(), user), chi.RouteCtxKey, rctx)
	return r.WithContext(ctx)
}

// oneRecStore builds a store whose owner has a judged run with a single recommendation
// (improve_uzi/”) the tests dispose. recID is the recommendation the requests address.
func oneRecStore() (*dispStore, uuid.UUID, uuid.UUID) {
	ownerID := uuid.New()
	runID := uuid.New()
	reviewID := uuid.New()
	recID := uuid.New()
	st := &dispStore{
		ownerID: ownerID,
		run:     store.Run{ID: runID, UserID: ownerID, Status: "completed", Kind: "issue"},
		review:  store.RunReview{ID: reviewID, TargetRunID: runID, UserID: ownerID},
		recs: []store.ReviewRecommendation{
			{ID: recID, ReviewID: reviewID, Category: "improve_uzi", Target: "", RationaleMd: "tidy"},
		},
		deleteRows: 1,
	}
	return st, runID, recID
}

// ---- 1. owner-only authz matrix (PUT) -----------------------------------------------

// TestSetDispositionIsOwnerOnly mirrors TestCreateRunInputIsOwnerOnly (runs_test.go): the
// owner writes (204), a non-owner session 404s, and a non-owner ADMIN — modelling a uza_
// admin_ro token, which keeps IsAdmin on RequireUser — ALSO 404s. The write path resolves
// by strict caller-ownership (isAdmin hardcoded false), so the mutation never runs for a
// non-owner and IsAdmin buys no bypass (PRD #94 Decision 5).
func TestSetDispositionIsOwnerOnly(t *testing.T) {
	st, runID, recID := oneRecStore()
	owner := store.User{ID: st.ownerID}
	h := newRunsHandler(t, st)

	rec := httptest.NewRecorder()
	h.SetDisposition(rec, dispReq(http.MethodPut, owner, runID, recID, `{"status":"done"}`))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("owner PUT = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	if len(st.upserted) != 1 {
		t.Fatalf("owner PUT must upsert exactly once, got %d", len(st.upserted))
	}

	// Non-owner session: 404, and the mutation must not have run.
	st.upserted = nil
	rec = httptest.NewRecorder()
	h.SetDisposition(rec, dispReq(http.MethodPut, store.User{ID: uuid.New()}, runID, recID, `{"status":"done"}`))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("non-owner PUT = %d, want 404", rec.Code)
	}
	if len(st.upserted) != 0 {
		t.Fatal("a non-owner PUT must not reach the upsert")
	}

	// Non-owner ADMIN (uza_ admin_ro keeps IsAdmin): STILL 404 — no admin write bypass.
	rec = httptest.NewRecorder()
	h.SetDisposition(rec, dispReq(http.MethodPut, store.User{ID: uuid.New(), IsAdmin: true}, runID, recID, `{"status":"done"}`))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("non-owner admin PUT = %d, want 404 (disposition is owner-only, no IsAdmin bypass)", rec.Code)
	}
	if len(st.upserted) != 0 {
		t.Fatal("a non-owner admin PUT must not reach the upsert (strict caller-ownership, not owner-or-admin)")
	}
}

// ---- 1. owner-only authz matrix (DELETE) --------------------------------------------

func TestDeleteDispositionIsOwnerOnly(t *testing.T) {
	st, runID, recID := oneRecStore()
	st.disps = []store.RecommendationDisposition{
		{ReviewID: st.review.ID, Category: "improve_uzi", Target: "", Status: "done"},
	}
	owner := store.User{ID: st.ownerID}
	h := newRunsHandler(t, st)

	rec := httptest.NewRecorder()
	h.DeleteDisposition(rec, dispReq(http.MethodDelete, owner, runID, recID, ""))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("owner DELETE = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	if len(st.deleted) != 1 {
		t.Fatalf("owner DELETE must delete once, got %d", len(st.deleted))
	}

	st.deleted = nil
	rec = httptest.NewRecorder()
	h.DeleteDisposition(rec, dispReq(http.MethodDelete, store.User{ID: uuid.New()}, runID, recID, ""))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("non-owner DELETE = %d, want 404", rec.Code)
	}
	if len(st.deleted) != 0 {
		t.Fatal("a non-owner DELETE must not reach the delete")
	}

	rec = httptest.NewRecorder()
	h.DeleteDisposition(rec, dispReq(http.MethodDelete, store.User{ID: uuid.New(), IsAdmin: true}, runID, recID, ""))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("non-owner admin DELETE = %d, want 404 (owner-only, no IsAdmin bypass)", rec.Code)
	}
	if len(st.deleted) != 0 {
		t.Fatal("a non-owner admin DELETE must not reach the delete")
	}
}

// ---- 1. owner-only authz matrix (the OTHER two legs the PRD spells out) --------------

// TestSetDispositionAdminWritesOwnRun completes the write matrix (PRD #94 Decision 5,
// Resolved Q "an admin can always triage their OWN judge runs"): an admin caller whose id
// IS the run owner writes normally (204). IsAdmin is never consulted on the write path, so
// it neither grants a cross-user bypass (the tests above) NOR blocks an admin from their
// own run (this one) — the caller==owner check in GetRunByIDForUser is all that matters.
func TestSetDispositionAdminWritesOwnRun(t *testing.T) {
	st, runID, recID := oneRecStore()
	ownerAdmin := store.User{ID: st.ownerID, IsAdmin: true} // the owner, who also happens to be an admin
	h := newRunsHandler(t, st)

	rec := httptest.NewRecorder()
	h.SetDisposition(rec, dispReq(http.MethodPut, ownerAdmin, runID, recID, `{"status":"done"}`))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("owner-admin PUT = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	if len(st.upserted) != 1 {
		t.Fatalf("owner-admin PUT must upsert exactly once, got %d", len(st.upserted))
	}
}

// TestGetRunReviewAdminReadsOthers pins the READ side of the matrix (PRD #94 Decision 5): a
// disposition WRITE is strict-owner-only, but the review READ stays owner-OR-admin (it feeds
// the admin run-view and FileIssue). A non-owner admin reading ANOTHER user's review gets
// 200 via the owner-or-admin GetRunByID path — the deliberate asymmetry the owner-only write
// is layered on top of. Contrast TestSetDispositionIsOwnerOnly, where the same admin's WRITE
// 404s. A plain (non-admin) non-owner would 404 here too, so the 200 is a real admin signal.
func TestGetRunReviewAdminReadsOthers(t *testing.T) {
	st, runID, _ := oneRecStore()
	admin := store.User{ID: uuid.New(), IsAdmin: true} // NOT the owner
	h := newRunsHandler(t, st)

	rec := httptest.NewRecorder()
	h.GetRunReview(rec, runReq(admin, runID))
	if rec.Code != http.StatusOK {
		t.Fatalf("admin read of another user's review = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	sawAdminPath := false
	for _, c := range st.calls {
		if c == "GetRunByID" { // owner-or-admin resolve, not the owner-scoped GetRunByIDForUser
			sawAdminPath = true
		}
	}
	if !sawAdminPath {
		t.Errorf("admin read must resolve via the owner-or-admin GetRunByID, calls=%v", st.calls)
	}
}

// ---- 2. enum validation (PUT) -------------------------------------------------------

// TestSetDispositionEnumValidation exercises the handler's validDisposition gate (PRD #94
// Decision 4): done carries NO reason; dismissed REQUIRES wont_do|not_an_issue. A rejected
// combination is a 400 that never touches the store; a valid one is 204 and forwards the
// exact wire values to the upsert.
func TestSetDispositionEnumValidation(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		wantCode   int
		wantStatus string
		wantReason string // the pgtype.Text the upsert should carry (only checked on 204)
	}{
		{"bad status", `{"status":"resolved"}`, http.StatusBadRequest, "", ""},
		{"dismissed no reason", `{"status":"dismissed"}`, http.StatusBadRequest, "", ""},
		{"dismissed bad reason", `{"status":"dismissed","reason":"because"}`, http.StatusBadRequest, "", ""},
		{"done with a reason", `{"status":"done","reason":"wont_do"}`, http.StatusBadRequest, "", ""},
		{"empty status", `{"status":""}`, http.StatusBadRequest, "", ""},
		{"valid done", `{"status":"done"}`, http.StatusNoContent, "done", ""},
		{"valid dismissed wont_do", `{"status":"dismissed","reason":"wont_do"}`, http.StatusNoContent, "dismissed", "wont_do"},
		{"valid dismissed not_an_issue", `{"status":"dismissed","reason":"not_an_issue"}`, http.StatusNoContent, "dismissed", "not_an_issue"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st, runID, recID := oneRecStore()
			owner := store.User{ID: st.ownerID}
			h := newRunsHandler(t, st)

			rec := httptest.NewRecorder()
			h.SetDisposition(rec, dispReq(http.MethodPut, owner, runID, recID, tc.body))
			if rec.Code != tc.wantCode {
				t.Fatalf("PUT %s = %d, want %d; body=%s", tc.body, rec.Code, tc.wantCode, rec.Body.String())
			}
			if tc.wantCode == http.StatusBadRequest {
				if len(st.upserted) != 0 {
					t.Fatal("a rejected enum must not reach the store")
				}
				return
			}
			if len(st.upserted) != 1 {
				t.Fatalf("valid PUT must upsert once, got %d", len(st.upserted))
			}
			got := st.upserted[0]
			if got.Status != tc.wantStatus {
				t.Errorf("upsert status = %q, want %q", got.Status, tc.wantStatus)
			}
			wantValid := tc.wantReason != ""
			if got.DismissReason.Valid != wantValid || got.DismissReason.String != tc.wantReason {
				t.Errorf("upsert reason = %+v, want {String:%q Valid:%v}", got.DismissReason, tc.wantReason, wantValid)
			}
			// The re-stamped hash is sha256(rationale_md) of the addressed rec (Decision 3).
			if got.RationaleHash != workersvc.RationaleHash("tidy") {
				t.Errorf("upsert rationale_hash = %q, want hash of current rationale_md", got.RationaleHash)
			}
		})
	}
}

// ---- 3. idempotent double-PUT (last-writer-wins) ------------------------------------

// TestSetDispositionIdempotentDoublePUT: two PUTs on the same coordinate both 204; the
// second overwrites the first (the coordinate upsert is last-writer-wins, PRD #94
// Decision 6). Asserted via the fake's captured upsert params.
func TestSetDispositionIdempotentDoublePUT(t *testing.T) {
	st, runID, recID := oneRecStore()
	owner := store.User{ID: st.ownerID}
	h := newRunsHandler(t, st)

	rec := httptest.NewRecorder()
	h.SetDisposition(rec, dispReq(http.MethodPut, owner, runID, recID, `{"status":"done"}`))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("first PUT = %d, want 204", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.SetDisposition(rec, dispReq(http.MethodPut, owner, runID, recID, `{"status":"dismissed","reason":"not_an_issue"}`))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("second PUT = %d, want 204", rec.Code)
	}
	if len(st.upserted) != 2 {
		t.Fatalf("both PUTs must upsert on the same coordinate, got %d", len(st.upserted))
	}
	// Same coordinate both times, and the last write is what stands.
	first, second := st.upserted[0], st.upserted[1]
	if first.Category != second.Category || first.Target != second.Target {
		t.Fatalf("both PUTs must target the same coordinate: %+v vs %+v", first, second)
	}
	if second.Status != "dismissed" || second.DismissReason.String != "not_an_issue" {
		t.Fatalf("last-writer-wins: second upsert = %+v, want dismissed/not_an_issue", second)
	}
}

// ---- 4. undo + double-undo ----------------------------------------------------------

// TestDeleteDispositionUndoAndDoubleUndo: deleting an existing disposition is 204; a second
// delete (rows-affected 0) is 404 — the handler maps 0 rows to ErrRecommendationNotFound
// (PRD #94 Decision 6).
func TestDeleteDispositionUndoAndDoubleUndo(t *testing.T) {
	st, runID, recID := oneRecStore()
	owner := store.User{ID: st.ownerID}
	h := newRunsHandler(t, st)

	// First undo: the coordinate had a disposition → 1 row deleted → 204.
	st.deleteRows = 1
	rec := httptest.NewRecorder()
	h.DeleteDisposition(rec, dispReq(http.MethodDelete, owner, runID, recID, ""))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("first undo = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}

	// Second undo: nothing left → 0 rows deleted → 404.
	st.deleteRows = 0
	rec = httptest.NewRecorder()
	h.DeleteDisposition(rec, dispReq(http.MethodDelete, owner, runID, recID, ""))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("double undo = %d, want 404 (0 rows affected)", rec.Code)
	}
}

// TestSetDispositionUnknownRecIs404: a recID absent from the current review (re-judged
// away, or never existed) 404s — ErrRecommendationNotFound, the same 404 as a foreign run,
// so no existence oracle leaks (PRD #94 Decision 5). The mutation never runs.
func TestSetDispositionUnknownRecIs404(t *testing.T) {
	st, runID, _ := oneRecStore()
	owner := store.User{ID: st.ownerID}
	h := newRunsHandler(t, st)

	rec := httptest.NewRecorder()
	h.SetDisposition(rec, dispReq(http.MethodPut, owner, runID, uuid.New(), `{"status":"done"}`))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown recID PUT = %d, want 404", rec.Code)
	}
	if len(st.upserted) != 0 {
		t.Fatal("an unknown recID must not reach the upsert")
	}
}

// ---- 7. no token spend, no forge write ----------------------------------------------

// TestDispositionTouchesStoreOnly proves the Success Criterion "no token spend, no forge
// write": across a PUT and a DELETE, the disposition path calls ONLY the owner-resolve
// read methods plus the single disposition write — never a run-create/enqueue or forge
// method. The dispStore embeds a nil workersvc.Store, so any such call would additionally
// panic; here we assert the exact call set positively.
func TestDispositionTouchesStoreOnly(t *testing.T) {
	st, runID, recID := oneRecStore()
	st.disps = []store.RecommendationDisposition{
		{ReviewID: st.review.ID, Category: "improve_uzi", Target: "", Status: "done"},
	}
	owner := store.User{ID: st.ownerID}
	h := newRunsHandler(t, st)

	h.SetDisposition(httptest.NewRecorder(), dispReq(http.MethodPut, owner, runID, recID, `{"status":"done"}`))
	h.DeleteDisposition(httptest.NewRecorder(), dispReq(http.MethodDelete, owner, runID, recID, ""))

	allowed := map[string]bool{
		"GetRunByIDForUser":               true, // owner resolve
		"GetRunReviewForTarget":           true,
		"ListRecommendationsForReview":    true,
		"ListFiledIssuesForReview":        true,
		"ListDispositionsForReview":       true,
		"UpsertRecommendationDisposition": true, // the ONLY write
		"DeleteRecommendationDisposition": true, // the ONLY write
	}
	sawUpsert, sawDelete := false, false
	for _, c := range st.calls {
		if !allowed[c] {
			t.Fatalf("disposition path called a non-disposition store method %q (possible spend/forge write); calls=%v", c, st.calls)
		}
		sawUpsert = sawUpsert || c == "UpsertRecommendationDisposition"
		sawDelete = sawDelete || c == "DeleteRecommendationDisposition"
	}
	if !sawUpsert || !sawDelete {
		t.Fatalf("expected exactly one disposition upsert and one delete; calls=%v", st.calls)
	}
}

// ---- 6. stale flag both ways (through GetRunReview / reviewToDTO) --------------------

// TestGetRunReviewStaleFlag drives the server-side stale computation (PRD #94 Decision 3):
// a disposition whose stored rationale_hash equals RationaleHash(current rationale_md) is
// stale=false; one whose stored hash differs (a rationale changed under the disposition) is
// stale=true. Proven both ways in one review via GetRunReview → reviewToDTO.
func TestGetRunReviewStaleFlag(t *testing.T) {
	ownerID := uuid.New()
	runID := uuid.New()
	reviewID := uuid.New()
	st := &dispStore{
		ownerID: ownerID,
		run:     store.Run{ID: runID, UserID: ownerID, Status: "completed", Kind: "issue"},
		review:  store.RunReview{ID: reviewID, TargetRunID: runID, UserID: ownerID},
		recs: []store.ReviewRecommendation{
			{ID: uuid.New(), ReviewID: reviewID, Category: "improve_uzi", Target: "fresh", RationaleMd: "current-fresh"},
			{ID: uuid.New(), ReviewID: reviewID, Category: "improve_agent", Target: "stale", RationaleMd: "changed-since"},
		},
		disps: []store.RecommendationDisposition{
			// matches the CURRENT rationale → not stale.
			{ReviewID: reviewID, Category: "improve_uzi", Target: "fresh", Status: "done",
				RationaleHash: workersvc.RationaleHash("current-fresh")},
			// stored against an OLD rationale → stale.
			{ReviewID: reviewID, Category: "improve_agent", Target: "stale", Status: "dismissed",
				DismissReason: pgtype.Text{String: "wont_do", Valid: true},
				RationaleHash: workersvc.RationaleHash("old-rationale")},
		},
	}
	owner := store.User{ID: ownerID}
	h := newRunsHandler(t, st)

	rec := httptest.NewRecorder()
	h.GetRunReview(rec, runReq(owner, runID))
	if rec.Code != http.StatusOK {
		t.Fatalf("GetRunReview = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Review apitypes.ReviewDTO `json:"review"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	staleByTarget := map[string]bool{}
	for _, d := range body.Review.Dispositions {
		staleByTarget[d.Target] = d.Stale
	}
	if len(body.Review.Dispositions) != 2 {
		t.Fatalf("want 2 dispositions in the DTO, got %d: %+v", len(body.Review.Dispositions), body.Review.Dispositions)
	}
	if staleByTarget["fresh"] {
		t.Error("a disposition matching the current rationale hash must be stale=false")
	}
	if !staleByTarget["stale"] {
		t.Error("a disposition whose stored hash differs from the current rationale must be stale=true")
	}
}

// TestGetRunReviewPerReviewTriage pins the PER-REVIEW triage tally on the review DTO —
// the counterpart of TestJudgeStatsAggregateLadder, which only covers the GLOBAL strip
// fed by the flat store join. reviewToDTO assembles its OWN triage rows from the current
// recommendations + dispositions + settled filed links (dispStatus/dispReason and the
// filed_at-valid filter), so the ladder being right does not by itself make this right.
// Added by PRD #97 M4: the e2e phase that used to assert `.review.triage.false_positives`
// over the wire was dropped, and this was the one leg no lower layer covered.
//
// Five current recommendations: a not_an_issue dismissal (the ONLY false positive), a
// wont_do dismissal (dismissed but NOT a false positive), a done, a SETTLED filed link,
// and an UNSETTLED claim (must count todo, never filed). A disposition on a coordinate
// with no current recommendation must not count at all.
func TestGetRunReviewPerReviewTriage(t *testing.T) {
	ownerID, runID, reviewID := uuid.New(), uuid.New(), uuid.New()
	rec := func(cat, tgt string) store.ReviewRecommendation {
		return store.ReviewRecommendation{ID: uuid.New(), ReviewID: reviewID, Category: cat, Target: tgt}
	}
	st := &dispStore{
		ownerID: ownerID,
		run:     store.Run{ID: runID, UserID: ownerID, Status: "completed", Kind: "issue"},
		review:  store.RunReview{ID: reviewID, TargetRunID: runID, UserID: ownerID},
		recs: []store.ReviewRecommendation{
			rec("improve_uzi", "fp"),
			rec("improve_uzi", "wont"),
			rec("improve_agent", "done"),
			rec("install_worker_tool", "settled"),
			rec("install_worker_tool", "claimed"),
		},
		disps: []store.RecommendationDisposition{
			{ReviewID: reviewID, Category: "improve_uzi", Target: "fp", Status: "dismissed",
				DismissReason: pgtype.Text{String: "not_an_issue", Valid: true}},
			{ReviewID: reviewID, Category: "improve_uzi", Target: "wont", Status: "dismissed",
				DismissReason: pgtype.Text{String: "wont_do", Valid: true}},
			{ReviewID: reviewID, Category: "improve_agent", Target: "done", Status: "done"},
			// Stale coordinate: no current recommendation carries it, so it must not count.
			{ReviewID: reviewID, Category: "add_agent", Target: "gone", Status: "done"},
		},
		filed: []store.RecommendationFiledIssue{
			{ReviewID: reviewID, Category: "install_worker_tool", Target: "settled",
				FiledAt: pgtype.Timestamptz{Time: time.Now(), Valid: true}},
			// An UNSETTLED claim (filed_at NULL) is NOT filed — it stays todo.
			{ReviewID: reviewID, Category: "install_worker_tool", Target: "claimed"},
		},
	}
	h := newRunsHandler(t, st)

	w := httptest.NewRecorder()
	h.GetRunReview(w, runReq(store.User{ID: ownerID}, runID))
	if w.Code != http.StatusOK {
		t.Fatalf("GetRunReview = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var body struct {
		Review apitypes.ReviewDTO `json:"review"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := apitypes.TriageDTO{Total: 5, Todo: 1, Filed: 1, Done: 1, Dismissed: 2, FalsePositives: 1}
	if body.Review.Triage != want {
		t.Fatalf("per-review triage = %+v, want %+v", body.Review.Triage, want)
	}
}

// ---- 5 + 1(stats). global stats aggregate (owner-scoped) ----------------------------

// TestJudgeStatsAggregateLadder feeds ListJudgeTriageRowsForUser a mix of flat rows and
// asserts the TriageDTO the handler returns (PRD #94 Decision 8 — the single-ladder proof):
// the ladder precedence (dismissed > done > filed > todo), an UNSETTLED claim counting as
// todo (filed_settled=false), and false_positives counting ONLY not_an_issue. It also
// confirms the handler is owner-scoped: the store query is called with the caller's id.
func TestJudgeStatsAggregateLadder(t *testing.T) {
	text := func(s string) pgtype.Text {
		if s == "" {
			return pgtype.Text{}
		}
		return pgtype.Text{String: s, Valid: true}
	}
	caller := store.User{ID: uuid.New()}
	st := &dispStore{
		ownerID: caller.ID,
		triageRows: []store.ListJudgeTriageRowsForUserRow{
			// dismissed beats filed even when settled (precedence).
			{DispositionStatus: text("dismissed"), DismissReason: text("not_an_issue"), FiledSettled: true},
			{DispositionStatus: text("dismissed"), DismissReason: text("wont_do"), FiledSettled: false},
			// done beats filed.
			{DispositionStatus: text("done"), FiledSettled: true},
			// a settled filed link, no disposition → filed.
			{DispositionStatus: text(""), FiledSettled: true},
			// an UNSETTLED claim (filed_settled=false), no disposition → todo, NOT filed.
			{DispositionStatus: text(""), FiledSettled: false},
			// a plain open rec → todo.
			{DispositionStatus: text(""), FiledSettled: false},
		},
	}
	h := newRunsHandler(t, st)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/me/judge/stats", nil)
	h.JudgeStats(rec, req.WithContext(mw.ContextWithUser(req.Context(), caller)))
	if rec.Code != http.StatusOK {
		t.Fatalf("JudgeStats = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if st.statsUserArg != caller.ID {
		t.Fatalf("stats query scoped to %v, want the caller %v (owner-scoped)", st.statsUserArg, caller.ID)
	}
	var got apitypes.TriageDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := apitypes.TriageDTO{Total: 6, Todo: 2, Filed: 1, Done: 1, Dismissed: 2, FalsePositives: 1}
	if got != want {
		t.Fatalf("triage aggregate = %+v, want %+v", got, want)
	}
}
