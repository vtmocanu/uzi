package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"gitlab.example.com/vtmocanu/uzi/api/internal/apitypes"
	mw "gitlab.example.com/vtmocanu/uzi/api/internal/middleware"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
	"gitlab.example.com/vtmocanu/uzi/api/internal/workersvc"
)

// categoryStatsStore is a fake store for the PRD #244 chip-count handler. It serves the
// per-category aggregate rows VERBATIM for the owner and returns nothing for anyone else —
// standing in for the query's `WHERE rv.user_id = @user_id` owner scope so the handler test
// can prove the endpoint never counts a second user's recommendations. It embeds
// workersvc.Store (nil), so any OTHER method the handler path reaches would panic: the
// structural "reads only its one aggregate" proof.
//
// It deliberately does NOT re-implement COUNT(DISTINCT) — the rows it returns are already
// the aggregate's output. Proving the SQL dedupe/uncapped behaviour is the live-DB test's
// job (judge_category_stats_integration_test.go); this fake would make that assertion
// vacuous.
type categoryStatsStore struct {
	workersvc.Store

	owner uuid.UUID
	rows  []store.CountJudgeGroupsByCategoryForUserRow

	gotUserArg uuid.UUID
	calls      []string
}

func (s *categoryStatsStore) CountJudgeGroupsByCategoryForUser(_ context.Context, userID uuid.UUID) ([]store.CountJudgeGroupsByCategoryForUserRow, error) {
	s.calls = append(s.calls, "CountJudgeGroupsByCategoryForUser")
	s.gotUserArg = userID
	if userID != s.owner {
		// Mirror the owner-scoped WHERE: a different caller sees none of the owner's rows.
		return nil, nil
	}
	return s.rows, nil
}

func categoryStatsReq(user store.User) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/api/me/judge/category-stats", nil)
	return req.WithContext(mw.ContextWithUser(req.Context(), user))
}

// TestJudgeCategoryStatsResponseShape: the endpoint returns the per-category counts folded
// into the `counts` map, keyed by category, and scopes the query to the caller.
func TestJudgeCategoryStatsResponseShape(t *testing.T) {
	owner := store.User{ID: uuid.New()}
	st := &categoryStatsStore{
		owner: owner.ID,
		rows: []store.CountJudgeGroupsByCategoryForUserRow{
			{Category: "install_worker_tool", GroupCount: 3},
			{Category: "improve_uzi", GroupCount: 6},
		},
	}
	h := newRunsHandler(t, st)

	rec := httptest.NewRecorder()
	h.JudgeCategoryStats(rec, categoryStatsReq(owner))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if st.gotUserArg != owner.ID {
		t.Fatalf("query arg = %v, want the caller %v (owner-scoped)", st.gotUserArg, owner.ID)
	}
	var got apitypes.JudgeCategoryStatsDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	want := map[string]int{"install_worker_tool": 3, "improve_uzi": 6}
	if len(got.Counts) != len(want) {
		t.Fatalf("counts = %+v, want %+v", got.Counts, want)
	}
	for k, v := range want {
		if got.Counts[k] != v {
			t.Errorf("counts[%q] = %d, want %d", k, got.Counts[k], v)
		}
	}
	// The response envelope carries the `counts` key (the wire contract the web reads).
	if _, ok := rawKeys(t, rec.Body.Bytes())["counts"]; !ok {
		t.Fatalf("response missing the `counts` key: %s", rec.Body.String())
	}
}

// TestJudgeCategoryStatsIsOwnerScoped: a SECOND user's recommendations are never counted.
// The fake keys its rows on the owner, so a different caller reaching the same endpoint gets
// an empty (but present) counts map — proving the handler threads the caller's own id into
// the aggregate and never returns another tenant's categories.
func TestJudgeCategoryStatsIsOwnerScoped(t *testing.T) {
	owner := store.User{ID: uuid.New()}
	other := store.User{ID: uuid.New()}
	st := &categoryStatsStore{
		owner: owner.ID,
		rows: []store.CountJudgeGroupsByCategoryForUserRow{
			{Category: "install_worker_tool", GroupCount: 3},
		},
	}
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
	if len(got.Counts) != 0 {
		t.Fatalf("a second user's request counted the owner's categories: %+v", got.Counts)
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
