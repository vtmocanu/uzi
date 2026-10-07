package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestAdminUsageSevenDayUnion(t *testing.T) {
	both, usageOnly, lifetimeOnly, outcomesOnly := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	runID := uuid.New()
	st := &runsStore{
		adminPerUser: []store.AdminUsagePerUserRow{
			{UserID: both, Email: "both@example.com", InputTokens: 101, RunCount: 9,
				Last7InputTokens: 11, Last7CacheReadTokens: 12, Last7CacheCreationTokens: 13,
				Last7OutputTokens: 14, Last7CostUsd: numericFor(0.125), Last7RunCount: 3,
				Last7SubscriptionRunCount: 1, Last7UnreportedRunCount: 2},
			{UserID: usageOnly, Email: "usage@example.com", Last7InputTokens: 20, Last7RunCount: 1},
			{UserID: lifetimeOnly, Email: "old@example.com", InputTokens: 50, RunCount: 1},
		},
		adminRunOutcomesPerUser: []store.AdminRunOutcomesPerUserRow{
			{UserID: both, Finished: 10, Failed: 7, FailOrigins: []byte(`{"unknown":7}`),
				LastFailedAt: pgtype.Timestamptz{Time: at, Valid: true}, LastFailedRunID: runID,
				LastFailedOrigin: "unknown", CompletedSinceLastFailure: 3,
				Last7Finished: 6, Last7Completed: 1, Last7Cancelled: 1, Last7PlanRejected: 1,
				Last7Failed: 3, Last7NeedsLanding: 2, Last7FailOrigins: []byte(`{"unknown":1,"agent_failure":2}`)},
			{UserID: lifetimeOnly, Finished: 1, Completed: 1},
			{UserID: outcomesOnly, Email: "outcomes@example.com", Finished: 1, Failed: 1,
				FailOrigins: []byte(`{"unknown":1}`), Last7Finished: 1, Last7Failed: 1,
				Last7FailOrigins: []byte(`{"unknown":1}`)},
		},
	}
	h := newRunsHandler(t, st)
	rec := httptest.NewRecorder()
	h.AdminUsage(rec, httptest.NewRequest(http.MethodGet, "/api/admin/usage", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("%d: %s", rec.Code, rec.Body.String())
	}
	var body apitypes.AdminUsageDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Users) != 4 {
		t.Fatalf("union: %+v", body.Users)
	}
	for i, id := range []uuid.UUID{both, usageOnly, lifetimeOnly, outcomesOnly} {
		u := body.Users[i]
		if u.UserID != id.String() || u.Last7Days == nil {
			t.Fatalf("row order/bundle: %+v", u)
		}
		var raw map[string]any
		b, err := json.Marshal(u.Last7Outcomes)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(b, &raw); err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"last_failed_at", "last_failed_run_id", "last_failed_origin", "last_failed_user_id", "completed_since_last_failure"} {
			v, ok := raw[key]
			if !ok || v != nil {
				t.Fatalf("%s must be present null: %s", key, b)
			}
		}
		if u.Last7Outcomes.FailOrigins == nil {
			t.Fatal("nil origins")
		}
	}
	u := body.Users[0]
	wantUsage := apitypes.UsageDTO{InputTokens: 11, CacheReadTokens: 12, CacheCreationTokens: 13, OutputTokens: 14, CostUSD: 0.125}
	wantOutcomes := apitypes.RunOutcomesDTO{Finished: 6, Completed: 1, Cancelled: 1, PlanRejected: 1, Failed: 3, NeedsLanding: 2, FailOrigins: map[string]int64{"unknown": 1, "agent_failure": 2}}
	if !reflect.DeepEqual(*u.Last7Days, wantUsage) || !reflect.DeepEqual(u.Last7Outcomes, wantOutcomes) ||
		u.Last7RunCount != 3 || u.Last7SubscriptionRunCount != 1 || u.Last7UnreportedRunCount != 2 {
		t.Fatalf("seven-day mapping: %+v", u)
	}
	if u.Usage.InputTokens != 101 || u.RunCount != 9 || u.Outcomes.Failed != 7 ||
		u.Outcomes.LastFailedAt == nil || !u.Outcomes.LastFailedAt.Equal(at) ||
		u.Outcomes.LastFailedRunID == nil || *u.Outcomes.LastFailedRunID != runID.String() ||
		u.Outcomes.LastFailedOrigin == nil || *u.Outcomes.LastFailedOrigin != "unknown" ||
		u.Outcomes.CompletedSinceLastFailure == nil || *u.Outcomes.CompletedSinceLastFailure != 3 || u.Outcomes.LastFailedUserID != nil {
		t.Fatalf("lifetime changed: %+v", u)
	}
	if body.Users[1].Last7Days.InputTokens != 20 || body.Users[1].Last7Outcomes.Finished != 0 {
		t.Fatal("usage-only mapping")
	}
	if !reflect.DeepEqual(*body.Users[2].Last7Days, apitypes.UsageDTO{}) || body.Users[2].Last7RunCount != 0 || body.Users[2].Last7Outcomes.Finished != 0 {
		t.Fatal("lifetime-only zeros")
	}
	u = body.Users[3]
	if !reflect.DeepEqual(*u.Last7Days, apitypes.UsageDTO{}) || u.Last7RunCount != 0 ||
		u.Last7SubscriptionRunCount != 0 || u.Last7UnreportedRunCount != 0 ||
		u.Last7Outcomes.Failed != 1 || u.Last7Outcomes.FailOrigins["unknown"] != 1 {
		t.Fatalf("outcomes-only: %+v", u)
	}
}

func TestAdminUsageMalformedSevenDayOrigins(t *testing.T) {
	for _, withUsage := range []bool{false, true} {
		t.Run(map[bool]string{false: "outcomes-only", true: "with-usage"}[withUsage], func(t *testing.T) {
			id := uuid.New()
			st := &runsStore{adminRunOutcomesPerUser: []store.AdminRunOutcomesPerUserRow{{UserID: id, Last7FailOrigins: []byte(`{"unknown":"bad"}`)}}}
			if withUsage {
				st.adminPerUser = []store.AdminUsagePerUserRow{{UserID: id}}
			}
			rec := httptest.NewRecorder()
			newRunsHandler(t, st).AdminUsage(rec, httptest.NewRequest(http.MethodGet, "/api/admin/usage", nil))
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("malformed origins accepted: %d %s", rec.Code, rec.Body.String())
			}
		})
	}
}
