package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/apitypes"
	mw "gitlab.example.com/vtmocanu/uzi/api/internal/middleware"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
	"gitlab.example.com/vtmocanu/uzi/api/internal/workersvc"
)

// categoryStatsStore is a fake store for the PRD #270 chip-count handler. It serves the
// whole-backlog recommendation rows VERBATIM for the owner and returns nothing for anyone
// else — standing in for the query's `WHERE rv.user_id = @user_id` owner scope so the handler
// test can prove the endpoint never counts a second user's recommendations. It embeds
// workersvc.Store (nil), so any OTHER method the handler path reaches would panic: the
// structural "reads only its one row load" proof.
//
// The rows go through the REAL service (workersvc.JudgeCategoryStats runs the shared
// GroupJudgeRecommendations rollup and tallies the bucket→category matrix), so this fake does
// NOT re-implement grouping or bucketing — it only feeds rows and records the params.
type categoryStatsStore struct {
	workersvc.Store

	owner uuid.UUID
	rows  []store.ListJudgeRecommendationRowsForUserRow

	gotUserArg   uuid.UUID
	gotRunAnchor pgtype.UUID
	gotLim       int32
	calls        []string
}

func (s *categoryStatsStore) ListJudgeRecommendationRowsForUser(_ context.Context, arg store.ListJudgeRecommendationRowsForUserParams) ([]store.ListJudgeRecommendationRowsForUserRow, error) {
	s.calls = append(s.calls, "ListJudgeRecommendationRowsForUser")
	s.gotUserArg = arg.UserID
	s.gotRunAnchor = arg.RunAnchor
	s.gotLim = arg.Lim
	if arg.UserID != s.owner {
		// Mirror the owner-scoped WHERE: a different caller sees none of the owner's rows.
		return nil, nil
	}
	return s.rows, nil
}

// catRow builds one whole-backlog recommendation row for the chip-count fixture. Each
// distinct (category, target) is its own group; disposition/filedSettled decide the group's
// rollup bucket via the shared BucketOf ladder (dismissed > done > filed > todo).
func catRow(category, target, disposition string, filedSettled bool) store.ListJudgeRecommendationRowsForUserRow {
	txt := func(s string) pgtype.Text {
		if s == "" {
			return pgtype.Text{}
		}
		return pgtype.Text{String: s, Valid: true}
	}
	return store.ListJudgeRecommendationRowsForUserRow{
		ReviewID:          uuid.New(),
		RunID:             uuid.New(),
		RecID:             uuid.New(),
		Category:          category,
		Target:            target,
		DispositionStatus: txt(disposition),
		FiledSettled:      filedSettled,
	}
}

func categoryStatsReq(user store.User) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/api/me/judge/category-stats", nil)
	return req.WithContext(mw.ContextWithUser(req.Context(), user))
}

func categoryStatsReqRun(user store.User, run string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/api/me/judge/category-stats?run="+run, nil)
	return req.WithContext(mw.ContextWithUser(req.Context(), user))
}

// ownerCategoryRows is the shared fixture: two categories across four bucket rollups, so the
// matrix has real per-bucket structure.
//
//	install_worker_tool: t1 todo, t2 todo, t3 done                    -> all 3, todo 2, done 1
//	improve_uzi:         u1 todo, u2 dismissed, u3 filed(settled)     -> all 3, todo 1, dismissed 1, filed 1
func ownerCategoryRows() []store.ListJudgeRecommendationRowsForUserRow {
	return []store.ListJudgeRecommendationRowsForUserRow{
		catRow("install_worker_tool", "t1", "", false),
		catRow("install_worker_tool", "t2", "", false),
		catRow("install_worker_tool", "t3", "done", false),
		catRow("improve_uzi", "u1", "", false),
		catRow("improve_uzi", "u2", "dismissed", false),
		catRow("improve_uzi", "u3", "", true),
	}
}

// TestJudgeCategoryStatsResponseShape: the endpoint returns the bucket→category count matrix
// under `counts_by_bucket`, keyed by bucket then category, computed through the shared rollup,
// scopes the query to the caller, and loads UNCAPPED (Lim: 0).
func TestJudgeCategoryStatsResponseShape(t *testing.T) {
	owner := store.User{ID: uuid.New()}
	st := &categoryStatsStore{owner: owner.ID, rows: ownerCategoryRows()}
	h := newRunsHandler(t, st)

	rec := httptest.NewRecorder()
	h.JudgeCategoryStats(rec, categoryStatsReq(owner))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if st.gotUserArg != owner.ID {
		t.Fatalf("query arg = %v, want the caller %v (owner-scoped)", st.gotUserArg, owner.ID)
	}
	// UNCAPPED: the chip-count rollup must load the whole backlog, or a group whose only open
	// member fell past a cap would mis-roll (PRD #270 / #244).
	if st.gotLim != 0 {
		t.Fatalf("Lim = %d, want 0 (uncapped whole-backlog load)", st.gotLim)
	}
	// No ?run= → the anchor param is a SQL NULL (no-op).
	if st.gotRunAnchor.Valid {
		t.Fatalf("run anchor = %+v, want an invalid (NULL) anchor when no ?run= is set", st.gotRunAnchor)
	}

	var got apitypes.JudgeCategoryStatsDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}

	// The wire envelope carries the `counts_by_bucket` key (the contract the web reads as
	// counts_by_bucket[tab][cat] ?? 0).
	if _, ok := rawKeys(t, rec.Body.Bytes())["counts_by_bucket"]; !ok {
		t.Fatalf("response missing the `counts_by_bucket` key: %s", rec.Body.String())
	}

	// All five bucket keys are present (an absent bucket serializes as {} not null).
	for _, b := range []string{"todo", "filed", "done", "dismissed", "all"} {
		if got.CountsByBucket[b] == nil {
			t.Fatalf("bucket %q missing from the matrix: %+v", b, got.CountsByBucket)
		}
	}

	// Per-bucket, per-category tallies.
	want := map[string]map[string]int{
		"todo":      {"install_worker_tool": 2, "improve_uzi": 1},
		"done":      {"install_worker_tool": 1},
		"dismissed": {"improve_uzi": 1},
		"filed":     {"improve_uzi": 1},
		"all":       {"install_worker_tool": 3, "improve_uzi": 3},
	}
	for bucket, cats := range want {
		for cat, n := range cats {
			if got.CountsByBucket[bucket][cat] != n {
				t.Errorf("counts_by_bucket[%q][%q] = %d, want %d (full matrix: %+v)",
					bucket, cat, got.CountsByBucket[bucket][cat], n, got.CountsByBucket)
			}
		}
	}

	// The tab-partition invariant: per category, todo+filed+done+dismissed == all.
	for _, cat := range []string{"install_worker_tool", "improve_uzi"} {
		sum := got.CountsByBucket["todo"][cat] + got.CountsByBucket["filed"][cat] +
			got.CountsByBucket["done"][cat] + got.CountsByBucket["dismissed"][cat]
		if sum != got.CountsByBucket["all"][cat] {
			t.Errorf("category %q: todo+filed+done+dismissed = %d, want == all = %d",
				cat, sum, got.CountsByBucket["all"][cat])
		}
	}
}

// TestJudgeCategoryStatsThreadsRunAnchor: a well-formed ?run= is parsed and pushed down as the
// query's owner-scoped anchor; a malformed one is a 400 that never reaches the store.
func TestJudgeCategoryStatsThreadsRunAnchor(t *testing.T) {
	owner := store.User{ID: uuid.New()}
	run := uuid.New()
	st := &categoryStatsStore{owner: owner.ID, rows: ownerCategoryRows()}
	h := newRunsHandler(t, st)

	rec := httptest.NewRecorder()
	h.JudgeCategoryStats(rec, categoryStatsReqRun(owner, run.String()))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET ?run= = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !st.gotRunAnchor.Valid || uuid.UUID(st.gotRunAnchor.Bytes) != run {
		t.Fatalf("run anchor = %+v, want the parsed run %v pushed down", st.gotRunAnchor, run)
	}

	// A malformed ?run= is a 400 and never loads rows.
	bad := &categoryStatsStore{owner: owner.ID, rows: ownerCategoryRows()}
	hBad := newRunsHandler(t, bad)
	recBad := httptest.NewRecorder()
	hBad.JudgeCategoryStats(recBad, categoryStatsReqRun(owner, "not-a-uuid"))
	if recBad.Code != http.StatusBadRequest {
		t.Fatalf("malformed ?run= = %d, want 400", recBad.Code)
	}
	if len(bad.calls) != 0 {
		t.Fatalf("a malformed ?run= must not reach the store, calls=%v", bad.calls)
	}
}

// TestJudgeCategoryStatsIsOwnerScoped: a SECOND user's recommendations are never counted. The
// fake keys its rows on the owner, so a different caller reaching the same endpoint gets an
// empty (but present) matrix — proving the handler threads the caller's own id into the query
// and never returns another tenant's categories.
func TestJudgeCategoryStatsIsOwnerScoped(t *testing.T) {
	owner := store.User{ID: uuid.New()}
	other := store.User{ID: uuid.New()}
	st := &categoryStatsStore{owner: owner.ID, rows: ownerCategoryRows()}
	h := newRunsHandler(t, st)

	rec := httptest.NewRecorder()
	h.JudgeCategoryStats(rec, categoryStatsReq(other))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if st.gotUserArg != other.ID {
		t.Fatalf("query arg = %v, want the second caller %v — the endpoint must scope to whoever calls it", st.gotUserArg, other.ID)
	}
	var got apitypes.JudgeCategoryStatsDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	// Every bucket is present but empty — no category was counted for the non-owner.
	for bucket, cats := range got.CountsByBucket {
		if len(cats) != 0 {
			t.Fatalf("a second user's request counted the owner's categories in bucket %q: %+v", bucket, cats)
		}
	}
}

// TestJudgeCategoryStatsUnauthenticatedIs401: no user in context is a 401 and never reaches
// the store (RequireUser mounts this endpoint, mirroring /me/judge/stats).
func TestJudgeCategoryStatsUnauthenticatedIs401(t *testing.T) {
	st := &categoryStatsStore{owner: uuid.New()}
	h := newRunsHandler(t, st)

	rec := httptest.NewRecorder()
	h.JudgeCategoryStats(rec, httptest.NewRequest(http.MethodGet, "/api/me/judge/category-stats", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated GET = %d, want 401", rec.Code)
	}
	if len(st.calls) != 0 {
		t.Fatalf("an unauthenticated request must not reach the store, calls=%v", st.calls)
	}
}

// rawKeys returns the top-level JSON object keys of b, so a test can assert the wire
// envelope carries a key (and not just that a typed decode succeeded).
func rawKeys(t *testing.T, b []byte) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal to object: %v (json=%s)", err, b)
	}
	return m
}
