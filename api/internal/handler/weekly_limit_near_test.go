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

// PRD #4123 M2 at the handler seam: a running or queued run on a credential near its weekly limit
// carries the block on detail and the compact form on summary lists, not only a parked run.

func wlRunning(owner uuid.UUID, status string) store.Run {
	r := wlRun(owner, "")
	r.Status, r.RateLimitType, r.LimitResetsAt, r.RetryNotBefore, r.LimitDeadSecretID = status, pgtype.Text{}, pgtype.Timestamptz{}, pgtype.Timestamptz{}, pgtype.UUID{}
	r.CredentialOverrideMode, r.CredentialOverrideSecretID = pgtype.Text{}, pgtype.UUID{}
	return r
}

func nearStoreFor(owner uuid.UUID, margin int16) *wlStore {
	s := newWLStore("spare", owner) // token A at 96%, B at 20%
	s.reserve = store.GetUserUsageReserveRow{WeeklyWarningMarginPts: margin}
	return s
}

func TestGetRunWeeklyLimitNearRunningRun(t *testing.T) {
	owner := store.User{ID: uuid.New()}
	for _, status := range []string{"running", "queued", "awaiting_input"} {
		run := wlRunning(owner.ID, status)
		h := wlRunsHandler(t, &runsStore{ownerID: owner.ID, run: run})
		h.SetWeeklyLimit(wlLoader(t, nearStoreFor(owner.ID, 15)))
		code, got, raw := getRunWeekly(t, h, owner, run.ID)
		if code != http.StatusOK || got == nil {
			t.Fatalf("%s: GET run = %d weekly_limit=%v: %s", status, code, got, raw)
		}
		if got.CredentialKey != "anthropic:"+wlTokA.String() || got.Pct == nil || *got.Pct != 96 {
			t.Errorf("%s: block = %+v", status, got)
		}
	}
	// Margin 0: no early block.
	run := wlRunning(owner.ID, "running")
	h := wlRunsHandler(t, &runsStore{ownerID: owner.ID, run: run})
	h.SetWeeklyLimit(wlLoader(t, nearStoreFor(owner.ID, 0)))
	if _, got, raw := getRunWeekly(t, h, owner, run.ID); got != nil {
		t.Fatalf("margin 0 produced a block: %s", raw)
	}
}

func TestListRunsWeeklyLimitNearRunsOwnerOnly(t *testing.T) {
	owner := store.User{ID: uuid.New()}
	running := wlRunning(owner.ID, "running")
	done := wlRunning(owner.ID, "completed")
	rs := &runsStore{ownerID: owner.ID, run: running, userRuns: []store.ListRunsForUserRow{
		{Run: running, PriorityClass: "normal"}, {Run: done, PriorityClass: "normal"},
	}}
	h := wlRunsHandler(t, rs)
	h.SetWeeklyLimit(wlLoader(t, nearStoreFor(owner.ID, 15)))

	for _, view := range []string{"?view=summary", ""} {
		req := httptest.NewRequest(http.MethodGet, "/api/runs"+view, nil)
		req = req.WithContext(mw.ContextWithUser(req.Context(), owner))
		rec := httptest.NewRecorder()
		h.ListRuns(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("list%s = %d: %s", view, rec.Code, rec.Body.String())
		}
		var body struct {
			Runs []struct {
				ID          string          `json:"id"`
				WeeklyLimit json.RawMessage `json:"weekly_limit"`
			} `json:"runs"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		have := map[string]string{}
		for _, r := range body.Runs {
			have[r.ID] = string(r.WeeklyLimit)
		}
		if have[running.ID.String()] == "" || have[running.ID.String()] == "null" {
			t.Errorf("list%s: the running near-limit run carries no weekly_limit: %v", view, have)
		}
		if v := have[done.ID.String()]; v != "" && v != "null" {
			t.Errorf("list%s: a completed run carries weekly_limit %s", view, v)
		}
		if view == "?view=summary" {
			var sum apitypes.WeeklyLimitSummaryDTO
			if err := json.Unmarshal([]byte(have[running.ID.String()]), &sum); err != nil {
				t.Fatal(err)
			}
			if sum.CredentialKey != "anthropic:"+wlTokA.String() || sum.CredentialLabel != "work" || sum.Tier == "" {
				t.Errorf("summary = %+v", sum)
			}
		}
	}
}
