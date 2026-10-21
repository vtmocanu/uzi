package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestUsageBaseDivergedAllMappings(t *testing.T) {
	owner, withUsage, withoutUsage := uuid.New(), uuid.New(), uuid.New()
	st := &runsStore{
		selfRunOutcomes: store.SelfRunOutcomesRow{
			LifetimeFinished: 101, LifetimeCompleted: 102, LifetimeCancelled: 103, LifetimePlanRejected: 104, LifetimeBaseDiverged: 105, LifetimeFailed: 106, LifetimeNeedsLanding: 107,
			Last7Finished: 111, Last7Completed: 112, Last7Cancelled: 113, Last7PlanRejected: 114, Last7BaseDiverged: 115, Last7Failed: 116, Last7NeedsLanding: 117,
		},
		adminRunOutcomes: store.AdminRunOutcomesRow{
			LifetimeFinished: 201, LifetimeCompleted: 202, LifetimeCancelled: 203, LifetimePlanRejected: 204, LifetimeBaseDiverged: 205, LifetimeFailed: 206, LifetimeNeedsLanding: 207,
			Last7Finished: 211, Last7Completed: 212, Last7Cancelled: 213, Last7PlanRejected: 214, Last7BaseDiverged: 215, Last7Failed: 216, Last7NeedsLanding: 217,
		},
		adminPerUser: []store.AdminUsagePerUserRow{{UserID: withUsage}},
		adminRunOutcomesPerUser: []store.AdminRunOutcomesPerUserRow{
			{UserID: withUsage, Finished: 301, Completed: 302, Cancelled: 303, PlanRejected: 304, LifetimeBaseDiverged: 305, Failed: 306, NeedsLanding: 307,
				Last7Finished: 311, Last7Completed: 312, Last7Cancelled: 313, Last7PlanRejected: 314, Last7BaseDiverged: 315, Last7Failed: 316, Last7NeedsLanding: 317},
			{UserID: withoutUsage, Finished: 401, Completed: 402, Cancelled: 403, PlanRejected: 404, LifetimeBaseDiverged: 405, Failed: 406, NeedsLanding: 407,
				Last7Finished: 411, Last7Completed: 412, Last7Cancelled: 413, Last7PlanRejected: 414, Last7BaseDiverged: 415, Last7Failed: 416, Last7NeedsLanding: 417},
		},
	}
	h := newRunsHandler(t, st)
	req := func(path string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		return r.WithContext(mw.ContextWithUser(r.Context(), store.User{ID: owner, IsAdmin: true}))
	}
	rec := httptest.NewRecorder()
	h.SelfUsage(rec, req("/api/usage"))
	if rec.Code != http.StatusOK {
		t.Fatalf("self: %d %s", rec.Code, rec.Body.String())
	}
	var self apitypes.SelfUsageDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &self); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	h.AdminUsage(rec, req("/api/admin/usage"))
	if rec.Code != http.StatusOK {
		t.Fatalf("admin: %d %s", rec.Code, rec.Body.String())
	}
	var admin apitypes.AdminUsageDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &admin); err != nil {
		t.Fatal(err)
	}
	if len(admin.Users) != 2 {
		t.Fatalf("users: %+v", admin.Users)
	}
	counts := func(o apitypes.RunOutcomesDTO) []int64 {
		return []int64{o.Finished, o.Completed, o.Cancelled, o.PlanRejected, o.BaseDiverged, o.Failed, o.NeedsLanding}
	}
	for _, tc := range []struct {
		name string
		got  apitypes.RunOutcomesDTO
		want []int64
	}{
		{"self life", self.Outcomes.Lifetime, []int64{101, 102, 103, 104, 105, 106, 107}},
		{"self week", self.Outcomes.Last7Days, []int64{111, 112, 113, 114, 115, 116, 117}},
		{"factory life", admin.Factory.Outcomes.Lifetime, []int64{201, 202, 203, 204, 205, 206, 207}},
		{"factory week", admin.Factory.Outcomes.Last7Days, []int64{211, 212, 213, 214, 215, 216, 217}},
		{"usage life", admin.Users[0].Outcomes, []int64{301, 302, 303, 304, 305, 306, 307}},
		{"usage week", admin.Users[0].Last7Outcomes, []int64{311, 312, 313, 314, 315, 316, 317}},
		{"no usage life", admin.Users[1].Outcomes, []int64{401, 402, 403, 404, 405, 406, 407}},
		{"no usage week", admin.Users[1].Last7Outcomes, []int64{411, 412, 413, 414, 415, 416, 417}},
	} {
		if !reflect.DeepEqual(counts(tc.got), tc.want) {
			t.Errorf("%s: %v want %v", tc.name, counts(tc.got), tc.want)
		}
	}
	raw, err := json.Marshal(apitypes.RunOutcomesDTO{})
	if err != nil {
		t.Fatal(err)
	}
	var zero map[string]any
	if err := json.Unmarshal(raw, &zero); err != nil {
		t.Fatal(err)
	}
	if n, ok := zero["base_diverged"]; !ok || n != float64(0) {
		t.Fatalf("zero field missing: %s", raw)
	}
}
