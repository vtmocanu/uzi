package handler

import (
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// numericFor builds a numeric(12,6)-shaped cost for a fake row (microdollar
// quantized, matching the fold's numericUSD), so the handler's float conversion is
// exercised against a realistic value.
func numericFor(usd float64) pgtype.Numeric {
	return pgtype.Numeric{Int: big.NewInt(int64(usd*1e6 + 0.5)), Exp: -6, Valid: true}
}

// usageBody is the decoded shape of a run's optional usage bundle.
type usageBody struct {
	InputTokens         int64   `json:"input_tokens"`
	CacheReadTokens     int64   `json:"cache_read_tokens"`
	CacheCreationTokens int64   `json:"cache_creation_tokens"`
	OutputTokens        int64   `json:"output_tokens"`
	CostUSD             float64 `json:"cost_usd"`
}

// outcomesBody is the decoded shape of a RunOutcomesDTO (PRD #1293 failed-run rate).
type outcomesBody struct {
	Finished     int64            `json:"finished"`
	Completed    int64            `json:"completed"`
	Cancelled    int64            `json:"cancelled"`
	PlanRejected int64            `json:"plan_rejected"`
	Failed       int64            `json:"failed"`
	FailOrigins  map[string]int64 `json:"fail_origins"`
}

// assertOutcomeInvariants pins the two PRD #1293 contract invariants on a decoded window:
// finished == completed+cancelled+plan_rejected+failed, and sum(fail_origins) == failed.
func assertOutcomeInvariants(t *testing.T, name string, o outcomesBody) {
	t.Helper()
	if got := o.Completed + o.Cancelled + o.PlanRejected + o.Failed; got != o.Finished {
		t.Fatalf("%s: finished (%d) != completed+cancelled+plan_rejected+failed (%d)", name, o.Finished, got)
	}
	var sum int64
	for _, c := range o.FailOrigins {
		sum += c
	}
	if sum != o.Failed {
		t.Fatalf("%s: sum(fail_origins)=%d != failed=%d", name, sum, o.Failed)
	}
	if o.FailOrigins == nil {
		t.Fatalf("%s: fail_origins must be a non-nil map (marshals {} not null)", name)
	}
}

func TestSelfUsageReturnsScopedTotals(t *testing.T) {
	user := store.User{ID: uuid.New()}
	st := &runsStore{
		selfUsage: store.SelfUsageRow{
			LifetimeInputTokens: 5000, LifetimeCacheReadTokens: 1200, LifetimeCacheCreationTokens: 300,
			LifetimeOutputTokens: 2500, LifetimeCostUsd: numericFor(0.42),
			Last7InputTokens: 800, Last7OutputTokens: 400, Last7CostUsd: numericFor(0.07),
			RunCount: 3,
		},
		// PRD #1293 outcomes: lifetime 80+5+3+12=100 finished; last7 15+1+1+3=20 finished.
		selfRunOutcomes: store.SelfRunOutcomesRow{
			LifetimeFinished: 100, LifetimeCompleted: 80, LifetimeCancelled: 5, LifetimePlanRejected: 3, LifetimeFailed: 12,
			Last7Finished: 20, Last7Completed: 15, Last7Cancelled: 1, Last7PlanRejected: 1, Last7Failed: 3,
		},
		// Per-origin causes: lifetime sums to 12 (=failed), last7 sums to 3 (=failed); a
		// NULL origin arrives from the query already bucketed as "unknown".
		selfRunOutcomeOrigins: []store.SelfRunOutcomeOriginsRow{
			{WindowTag: "lifetime", Origin: "agent_failure", Cnt: 8},
			{WindowTag: "lifetime", Origin: "run_timeout", Cnt: 3},
			{WindowTag: "lifetime", Origin: "unknown", Cnt: 1},
			{WindowTag: "last7", Origin: "agent_failure", Cnt: 2},
			{WindowTag: "last7", Origin: "run_timeout", Cnt: 1},
		},
	}
	h := newRunsHandler(t, st)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/usage", nil)
	h.SelfUsage(rec, req.WithContext(mw.ContextWithUser(req.Context(), user)))
	if rec.Code != http.StatusOK {
		t.Fatalf("SelfUsage = %d, want 200", rec.Code)
	}
	var body struct {
		Lifetime  usageBody `json:"lifetime"`
		Last7Days usageBody `json:"last_7_days"`
		RunCount  int64     `json:"run_count"`
		Outcomes  struct {
			Lifetime  outcomesBody `json:"lifetime"`
			Last7Days outcomesBody `json:"last_7_days"`
		} `json:"outcomes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Lifetime.InputTokens != 5000 || body.Lifetime.OutputTokens != 2500 || body.Lifetime.CacheReadTokens != 1200 {
		t.Fatalf("lifetime tokens wrong: %+v", body.Lifetime)
	}
	if body.Lifetime.CostUSD != 0.42 {
		t.Fatalf("lifetime cost = %v, want 0.42", body.Lifetime.CostUSD)
	}
	if body.Last7Days.InputTokens != 800 || body.Last7Days.CostUSD != 0.07 {
		t.Fatalf("last_7_days wrong: %+v", body.Last7Days)
	}
	if body.RunCount != 3 {
		t.Fatalf("run_count = %d, want 3", body.RunCount)
	}
	// PRD #1293: both windows carry the outcome counts, causes, and the two invariants.
	lt := body.Outcomes.Lifetime
	if lt.Finished != 100 || lt.Completed != 80 || lt.Cancelled != 5 || lt.PlanRejected != 3 || lt.Failed != 12 {
		t.Fatalf("lifetime outcomes wrong: %+v", lt)
	}
	if lt.FailOrigins["agent_failure"] != 8 || lt.FailOrigins["run_timeout"] != 3 || lt.FailOrigins["unknown"] != 1 {
		t.Fatalf("lifetime fail_origins wrong: %+v", lt.FailOrigins)
	}
	assertOutcomeInvariants(t, "self lifetime", lt)
	l7 := body.Outcomes.Last7Days
	if l7.Finished != 20 || l7.Failed != 3 {
		t.Fatalf("last7 outcomes wrong: %+v", l7)
	}
	if l7.FailOrigins["agent_failure"] != 2 || l7.FailOrigins["run_timeout"] != 1 {
		t.Fatalf("last7 fail_origins wrong: %+v", l7.FailOrigins)
	}
	assertOutcomeInvariants(t, "self last7", l7)
}

func TestSelfUsageRequiresAuth(t *testing.T) {
	h := newRunsHandler(t, &runsStore{})
	rec := httptest.NewRecorder()
	// No user in context → 401.
	h.SelfUsage(rec, httptest.NewRequest(http.MethodGet, "/api/usage", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("SelfUsage without auth = %d, want 401", rec.Code)
	}
}

func TestAdminUsageShapesFactoryAndUsers(t *testing.T) {
	// Factory total is 900 in / 300 out, and the two per-user rows sum to it — the
	// handler must pass both through unchanged (the SQL guarantees the sum equality;
	// this asserts the shaping and that no row is dropped).
	heavyID, lightID, brokenID := uuid.New(), uuid.New(), uuid.New()
	st := &runsStore{
		adminTotals: store.AdminUsageTotalsRow{
			LifetimeInputTokens: 900, LifetimeOutputTokens: 300, LifetimeCostUsd: numericFor(0.30), RunCount: 4,
			EarliestRun: pgtype.Timestamptz{Time: time.Date(2026, time.May, 12, 9, 0, 0, 0, time.UTC), Valid: true},
		},
		adminPerUser: []store.AdminUsagePerUserRow{
			{UserID: heavyID, Email: "heavy@x", InputTokens: 600, OutputTokens: 200, CostUsd: numericFor(0.20), RunCount: 3},
			{UserID: lightID, Email: "light@x", InputTokens: 300, OutputTokens: 100, CostUsd: numericFor(0.10), RunCount: 1},
		},
		// PRD #1293 factory outcomes: lifetime 150+10+5+35=200; last7 30+2+1+7=40.
		adminRunOutcomes: store.AdminRunOutcomesRow{
			LifetimeFinished: 200, LifetimeCompleted: 150, LifetimeCancelled: 10, LifetimePlanRejected: 5, LifetimeFailed: 35,
			Last7Finished: 40, Last7Completed: 30, Last7Cancelled: 2, Last7PlanRejected: 1, Last7Failed: 7,
		},
		adminRunOutcomeOrigins: []store.AdminRunOutcomeOriginsRow{
			{WindowTag: "lifetime", Origin: "agent_failure", Cnt: 20},
			{WindowTag: "lifetime", Origin: "run_timeout", Cnt: 10},
			{WindowTag: "lifetime", Origin: "unknown", Cnt: 5},
			{WindowTag: "last7", Origin: "agent_failure", Cnt: 5},
			{WindowTag: "last7", Origin: "run_timeout", Cnt: 2},
		},
		// Per-user outcomes: heavy has a usage row AND outcomes; light has a usage row but
		// NO outcomes row (so it gets a zero RunOutcomesDTO with an empty fail_origins {});
		// broken is OUTCOME-ONLY (no usage row) so it must be APPENDED after the cost-sorted
		// usage rows with zero usage (D5). Order in the slice is heavy, broken (light omitted).
		adminRunOutcomesPerUser: []store.AdminRunOutcomesPerUserRow{
			{UserID: heavyID, Email: "heavy@x", Finished: 120, Completed: 100, Cancelled: 5, PlanRejected: 3, Failed: 12},
			{UserID: brokenID, Email: "broken@x", Finished: 30, Completed: 10, Cancelled: 2, PlanRejected: 1, Failed: 17},
		},
		adminRunOutcomeOriginsPerUser: []store.AdminRunOutcomeOriginsPerUserRow{
			{UserID: heavyID, Origin: "agent_failure", Cnt: 10},
			{UserID: heavyID, Origin: "run_timeout", Cnt: 2},
			{UserID: brokenID, Origin: "unknown", Cnt: 17}, // every broken-token run died with NULL origin
		},
	}
	h := newRunsHandler(t, st)
	rec := httptest.NewRecorder()
	h.AdminUsage(rec, httptest.NewRequest(http.MethodGet, "/api/admin/usage", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("AdminUsage = %d, want 200", rec.Code)
	}
	var body struct {
		Factory struct {
			Lifetime usageBody `json:"lifetime"`
			RunCount int64     `json:"run_count"`
			Outcomes struct {
				Lifetime  outcomesBody `json:"lifetime"`
				Last7Days outcomesBody `json:"last_7_days"`
			} `json:"outcomes"`
		} `json:"factory"`
		Users []struct {
			Email    string       `json:"email"`
			Usage    usageBody    `json:"usage"`
			RunCount int64        `json:"run_count"`
			Outcomes outcomesBody `json:"outcomes"`
		} `json:"users"`
		EarliestRun *string `json:"earliest_run"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Factory.Lifetime.InputTokens != 900 || body.Factory.Lifetime.OutputTokens != 300 {
		t.Fatalf("factory totals wrong: %+v", body.Factory.Lifetime)
	}
	// PRD #1293: factory outcomes carry both windows, causes, and the invariants.
	fl := body.Factory.Outcomes.Lifetime
	if fl.Finished != 200 || fl.Failed != 35 || fl.PlanRejected != 5 {
		t.Fatalf("factory lifetime outcomes wrong: %+v", fl)
	}
	if fl.FailOrigins["agent_failure"] != 20 || fl.FailOrigins["unknown"] != 5 {
		t.Fatalf("factory lifetime fail_origins wrong: %+v", fl.FailOrigins)
	}
	assertOutcomeInvariants(t, "factory lifetime", fl)
	assertOutcomeInvariants(t, "factory last7", body.Factory.Outcomes.Last7Days)
	// The factory's earliest-run timestamp flows through for the "since <date>" line.
	if body.EarliestRun == nil || !strings.Contains(*body.EarliestRun, "2026-05-12") {
		t.Fatalf("earliest_run did not pass through, got %v", body.EarliestRun)
	}
	// D5: the outcome-only broken user is appended, so there are now 3 rows.
	if len(body.Users) != 3 {
		t.Fatalf("want 3 per-user rows (2 usage + 1 outcome-only), got %d", len(body.Users))
	}
	// The usage rows still sum to the factory total (the zero-usage broken row adds nothing).
	var sumIn int64
	for _, u := range body.Users {
		sumIn += u.Usage.InputTokens
	}
	if sumIn != body.Factory.Lifetime.InputTokens {
		t.Fatalf("per-user rows (%d) must sum to factory total (%d)", sumIn, body.Factory.Lifetime.InputTokens)
	}
	// Heaviest-cost usage row is still first; the outcome-only row is appended AFTER them.
	if body.Users[0].Email != "heavy@x" {
		t.Fatalf("users[0].email = %q, want heavy@x (heaviest first)", body.Users[0].Email)
	}
	if body.Users[1].Email != "light@x" {
		t.Fatalf("users[1].email = %q, want light@x", body.Users[1].Email)
	}
	// (b) per-user outcomes attach by id: heavy's outcomes came through with its causes.
	heavy := body.Users[0]
	if heavy.Outcomes.Finished != 120 || heavy.Outcomes.Failed != 12 {
		t.Fatalf("heavy outcomes wrong: %+v", heavy.Outcomes)
	}
	if heavy.Outcomes.FailOrigins["agent_failure"] != 10 || heavy.Outcomes.FailOrigins["run_timeout"] != 2 {
		t.Fatalf("heavy fail_origins wrong: %+v", heavy.Outcomes.FailOrigins)
	}
	assertOutcomeInvariants(t, "heavy", heavy.Outcomes)
	// A usage row with NO outcomes row gets a zero RunOutcomesDTO with a non-nil empty map.
	light := body.Users[1]
	if light.Outcomes.Finished != 0 || light.Outcomes.Failed != 0 {
		t.Fatalf("light should have zero outcomes, got %+v", light.Outcomes)
	}
	if light.Outcomes.FailOrigins == nil || len(light.Outcomes.FailOrigins) != 0 {
		t.Fatalf("light fail_origins must be a non-nil empty map, got %v", light.Outcomes.FailOrigins)
	}
	// (c) the outcome-only user is last, with zero usage and its outcomes+causes populated.
	broken := body.Users[2]
	if broken.Email != "broken@x" {
		t.Fatalf("users[2].email = %q, want broken@x (outcome-only, appended last)", broken.Email)
	}
	if broken.Usage.InputTokens != 0 || broken.RunCount != 0 {
		t.Fatalf("broken user must have zero usage, got usage=%+v runCount=%d", broken.Usage, broken.RunCount)
	}
	if broken.Outcomes.Finished != 30 || broken.Outcomes.Failed != 17 {
		t.Fatalf("broken outcomes wrong: %+v", broken.Outcomes)
	}
	if broken.Outcomes.FailOrigins["unknown"] != 17 {
		t.Fatalf("broken fail_origins should bucket NULL as unknown=17, got %+v", broken.Outcomes.FailOrigins)
	}
	assertOutcomeInvariants(t, "broken", broken.Outcomes)
}

// The admin usage endpoint is gated by RequireAdmin (the route lives under that
// middleware group): a non-admin gets 403, an admin passes through to 200.
func TestAdminUsageRequiresAdmin(t *testing.T) {
	h := newRunsHandler(t, &runsStore{})
	gated := mw.RequireAdmin(http.HandlerFunc(h.AdminUsage))

	nonAdmin := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/admin/usage", nil)
	gated.ServeHTTP(nonAdmin, req.WithContext(mw.ContextWithUser(req.Context(), store.User{ID: uuid.New(), IsAdmin: false})))
	if nonAdmin.Code != http.StatusForbidden {
		t.Fatalf("non-admin on /api/admin/usage = %d, want 403", nonAdmin.Code)
	}

	admin := httptest.NewRecorder()
	gated.ServeHTTP(admin, req.WithContext(mw.ContextWithUser(req.Context(), store.User{ID: uuid.New(), IsAdmin: true})))
	if admin.Code != http.StatusOK {
		t.Fatalf("admin on /api/admin/usage = %d, want 200", admin.Code)
	}
}

func TestListRunsAttachesUsageOnlyWhenPresent(t *testing.T) {
	user := store.User{ID: uuid.New()}
	st := &runsStore{userRuns: []store.ListRunsForUserRow{
		{
			Run: store.Run{ID: uuid.New(), Status: "completed"}, RepoPath: "g/r",
			UsageInputTokens:  pgtype.Int8{Int64: 1200, Valid: true},
			UsageOutputTokens: pgtype.Int8{Int64: 800, Valid: true},
			UsageCostUsd:      numericFor(0.05),
		},
		{Run: store.Run{ID: uuid.New(), Status: "queued"}, RepoPath: "g/r"}, // no usage rows → NULL columns
	}}
	h := newRunsHandler(t, st)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/runs", nil)
	h.ListRuns(rec, req.WithContext(mw.ContextWithUser(req.Context(), user)))
	if rec.Code != http.StatusOK {
		t.Fatalf("ListRuns = %d, want 200", rec.Code)
	}
	var body struct {
		Runs []struct {
			Usage *usageBody `json:"usage"`
		} `json:"runs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Runs) != 2 {
		t.Fatalf("want 2 runs, got %d", len(body.Runs))
	}
	if body.Runs[0].Usage == nil || body.Runs[0].Usage.InputTokens != 1200 || body.Runs[0].Usage.CostUSD != 0.05 {
		t.Fatalf("run with usage should carry it: %+v", body.Runs[0].Usage)
	}
	if body.Runs[1].Usage != nil {
		t.Fatalf("run without usage rows must omit usage (never a fake 0), got %+v", body.Runs[1].Usage)
	}
}

func TestGetRunAttachesUsageWhenPresent(t *testing.T) {
	owner := store.User{ID: uuid.New()}
	runID := uuid.New()
	base := store.Run{ID: runID, UserID: owner.ID, Status: "completed"}

	// With usage rows.
	withUsage := &runsStore{
		ownerID: owner.ID, run: base,
		hasRunUsage:   true,
		runUsageTotal: store.GetRunUsageTotalRow{InputTokens: 999, OutputTokens: 111, CostUsd: numericFor(0.02)},
	}
	rec := httptest.NewRecorder()
	newRunsHandler(t, withUsage).GetRun(rec, runReq(owner, runID))
	if rec.Code != http.StatusOK {
		t.Fatalf("GetRun = %d, want 200", rec.Code)
	}
	var body struct {
		Run struct {
			Usage *usageBody `json:"usage"`
		} `json:"run"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Run.Usage == nil || body.Run.Usage.InputTokens != 999 || body.Run.Usage.CostUSD != 0.02 {
		t.Fatalf("GetRun should attach usage: %+v", body.Run.Usage)
	}

	// Without usage rows (default: GetRunUsageTotal returns ErrNoRows) → absent.
	noUsage := &runsStore{ownerID: owner.ID, run: base}
	rec2 := httptest.NewRecorder()
	newRunsHandler(t, noUsage).GetRun(rec2, runReq(owner, runID))
	var body2 struct {
		Run struct {
			Usage *usageBody `json:"usage"`
		} `json:"run"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &body2); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body2.Run.Usage != nil {
		t.Fatalf("a run with no usage rows must omit usage, got %+v", body2.Run.Usage)
	}
}
