package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #3876: every per-user row carries both harness splits. A usage row copies the
// store counts (four distinct values, so a cross-window or cross-field swap fails); an outcomes-only row and a PR-only row (no usage, so no runs) carry {0,0}.
func TestAdminUsageHarnessRuns(t *testing.T) {
	usageID, outcomeID, prOnlyID := uuid.New(), uuid.New(), uuid.New()
	st := &runsStore{
		adminPerUser: []store.AdminUsagePerUserRow{{
			UserID: usageID, Email: "usage@x", RunCount: 10, Last7RunCount: 3,
			ClaudeRunCount: 7, CodexRunCount: 3, Last7ClaudeRunCount: 2, Last7CodexRunCount: 1,
		}},
		adminRunOutcomesPerUser: []store.AdminRunOutcomesPerUserRow{
			{UserID: outcomeID, Email: "outcome@x", Finished: 1, Failed: 1},
		},
		prCountsPerUser: []store.PRCountsPerUserRow{
			{UserID: prOnlyID, Email: "pronly@x", LifetimePrCount: 2, Last7PrCount: 1},
		},
	}
	h := newRunsHandler(t, st)
	rec := httptest.NewRecorder()
	h.AdminUsage(rec, httptest.NewRequest(http.MethodGet, "/api/admin/usage", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("AdminUsage = %d: %s", rec.Code, rec.Body.String())
	}
	// Decode into raw maps so a missing key is distinguishable from a zero count.
	var raw struct {
		Users []map[string]json.RawMessage `json:"users"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if len(raw.Users) != 3 {
		t.Fatalf("want 3 rows (usage, outcomes-only, pr-only), got %d: %s", len(raw.Users), rec.Body.String())
	}
	want := [][2]map[string]int64{
		{{"claude": 7, "codex": 3}, {"claude": 2, "codex": 1}},
		{{"claude": 0, "codex": 0}, {"claude": 0, "codex": 0}},
		{{"claude": 0, "codex": 0}, {"claude": 0, "codex": 0}},
	}
	for i, row := range raw.Users {
		for j, key := range []string{"harness_runs", "last7_harness_runs"} {
			b, ok := row[key]
			if !ok {
				t.Fatalf("row %d is missing %q: %s", i, key, rec.Body.String())
			}
			var got map[string]int64
			if err := json.Unmarshal(b, &got); err != nil {
				t.Fatalf("row %d %s: %v", i, key, err)
			}
			if len(got) != 2 || got["claude"] != want[i][j]["claude"] || got["codex"] != want[i][j]["codex"] {
				t.Fatalf("row %d %s = %v, want %v", i, key, got, want[i][j])
			}
		}
	}
}
