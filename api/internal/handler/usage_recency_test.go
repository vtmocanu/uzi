package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestUsageFailureRecencyMapping(t *testing.T) {
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	stamp := pgtype.Timestamptz{Time: at, Valid: true}
	owner, runID := uuid.New(), uuid.New()
	st := &runsStore{
		selfRunOutcomes:         store.SelfRunOutcomesRow{LastFailedAt: stamp, LastFailedRunID: runID, LastFailedOrigin: "unknown", CompletedSinceLastFailure: 4},
		adminRunOutcomes:        store.AdminRunOutcomesRow{LastFailedAt: stamp, LastFailedRunID: runID, LastFailedOrigin: "unknown", LastFailedUserID: owner, CompletedSinceLastFailure: 4},
		adminRunOutcomesPerUser: []store.AdminRunOutcomesPerUserRow{{UserID: owner, Email: "owner@example.com", LastFailedAt: stamp, LastFailedRunID: runID, LastFailedOrigin: "unknown", CompletedSinceLastFailure: 4}},
	}
	h := newRunsHandler(t, st)
	request := func(path string) *http.Request {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		return req.WithContext(mw.ContextWithUser(req.Context(), store.User{ID: owner, IsAdmin: true}))
	}
	assert := func(o apitypes.RunOutcomesDTO, factory bool) {
		t.Helper()
		if o.LastFailedAt == nil || !o.LastFailedAt.Equal(at) || o.LastFailedRunID == nil || *o.LastFailedRunID != runID.String() || o.LastFailedOrigin == nil || *o.LastFailedOrigin != "unknown" || o.CompletedSinceLastFailure == nil || *o.CompletedSinceLastFailure != 4 {
			t.Fatalf("recency missing or wrong: %+v", o)
		}
		if factory {
			if o.LastFailedUserID == nil || *o.LastFailedUserID != owner.String() {
				t.Fatalf("factory owner = %v", o.LastFailedUserID)
			}
		} else if o.LastFailedUserID != nil {
			t.Fatal("non-factory owner should be null")
		}
	}
	assertNull := func(o apitypes.RunOutcomesDTO) {
		t.Helper()
		if o.LastFailedAt != nil || o.LastFailedRunID != nil || o.LastFailedOrigin != nil || o.LastFailedUserID != nil || o.CompletedSinceLastFailure != nil {
			t.Fatalf("invented recency: %+v", o)
		}
	}
	rec := httptest.NewRecorder()
	h.SelfUsage(rec, request("/api/usage"))
	if rec.Code != http.StatusOK {
		t.Fatalf("self: %d %s", rec.Code, rec.Body.String())
	}
	var self apitypes.SelfUsageDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &self); err != nil {
		t.Fatal(err)
	}
	assert(self.Outcomes.Lifetime, false)
	assertNull(self.Outcomes.Last7Days)
	rec = httptest.NewRecorder()
	h.AdminUsage(rec, request("/api/admin/usage"))
	if rec.Code != http.StatusOK {
		t.Fatalf("admin: %d %s", rec.Code, rec.Body.String())
	}
	var admin apitypes.AdminUsageDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &admin); err != nil {
		t.Fatal(err)
	}
	assert(admin.Factory.Outcomes.Lifetime, true)
	assertNull(admin.Factory.Outcomes.Last7Days)
	if len(admin.Users) != 1 {
		t.Fatalf("missing outcome-only user: %+v", admin.Users)
	}
	assert(admin.Users[0].Outcomes, false)
	// Internal SQL defaults must never become an invented zero-valued streak.
	st.selfRunOutcomes.LastFailedAt = pgtype.Timestamptz{}
	rec = httptest.NewRecorder()
	h.SelfUsage(rec, request("/api/usage"))
	if err := json.Unmarshal(rec.Body.Bytes(), &self); err != nil {
		t.Fatal(err)
	}
	assertNull(self.Outcomes.Lifetime)
}
