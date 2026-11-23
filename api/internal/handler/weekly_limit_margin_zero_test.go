package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #4123 module 6 / D7: margin 0 turns off the early-warning surfaces, including the
// Overview limit decision, but keeps the park explanation. Timed seven_day parks and weekly
// reserve pauses are "automatic" parks (web/src/lib/overview.ts), so the limit row is their
// only Overview item: with margin 15 the summary must carry tier warning, and with margin 0
// it must not, while run detail still carries the block.
func TestWeeklyLimitMarginZeroDropsParkedOverviewDecision(t *testing.T) {
	for _, park := range []string{"seven_day", "weekly_reserve"} {
		for _, margin := range []int16{15, 0} {
			t.Run(park+"/margin_"+map[bool]string{true: "on", false: "off"}[margin > 0], func(t *testing.T) {
				owner := store.User{ID: uuid.New()}
				run := wlRun(owner.ID, "seven_day")
				if park == "weekly_reserve" {
					run.Status = "paused"
					run.HoldReason = pgtype.Text{String: "weekly_reserve", Valid: true}
				}
				rs := &runsStore{ownerID: owner.ID, run: run, userRuns: []store.ListRunsForUserRow{{Run: run, PriorityClass: "normal"}}}
				h := wlRunsHandler(t, rs)
				h.SetWeeklyLimit(wlLoader(t, nearStoreFor(owner.ID, margin)))

				code, detail, _ := getRunWeekly(t, h, owner, run.ID)
				if code != http.StatusOK || detail == nil {
					t.Fatalf("detail: code %d, block %v: the park explanation must stay at every margin", code, detail)
				}
				if detail.Tier != apitypes.WeeklyLimitTierWarning || len(detail.Fix) == 0 {
					t.Fatalf("detail block lost its fix: %+v", detail)
				}

				req := httptest.NewRequest(http.MethodGet, "/api/runs?view=summary", nil)
				req = req.WithContext(mw.ContextWithUser(req.Context(), owner))
				rec := httptest.NewRecorder()
				h.ListRuns(rec, req)
				if rec.Code != http.StatusOK {
					t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
				}
				var body struct {
					Runs []struct {
						WeeklyLimit *apitypes.WeeklyLimitSummaryDTO `json:"weekly_limit"`
					} `json:"runs"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || len(body.Runs) != 1 {
					t.Fatalf("decode: %v %s", err, rec.Body.String())
				}
				summary := body.Runs[0].WeeklyLimit
				warning := summary != nil && summary.Tier == apitypes.WeeklyLimitTierWarning
				if margin > 0 && !warning {
					t.Fatalf("margin %d: the summary must carry tier warning for the Overview limit decision, got %+v", margin, summary)
				}
				if margin == 0 && warning {
					t.Fatalf("margin 0 still emits the warning summary that creates an Overview limit item: %+v", summary)
				}
			})
		}
	}
}
