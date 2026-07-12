package handler

import (
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	mw "gitlab.example.com/vtmocanu/uzi/api/internal/middleware"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
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

func TestSelfUsageReturnsScopedTotals(t *testing.T) {
	user := store.User{ID: uuid.New()}
	st := &runsStore{selfUsage: store.SelfUsageRow{
		LifetimeInputTokens: 5000, LifetimeCacheReadTokens: 1200, LifetimeCacheCreationTokens: 300,
		LifetimeOutputTokens: 2500, LifetimeCostUsd: numericFor(0.42),
		Last7InputTokens: 800, Last7OutputTokens: 400, Last7CostUsd: numericFor(0.07),
		RunCount: 3,
	}}
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
	st := &runsStore{
		adminTotals: store.AdminUsageTotalsRow{
			LifetimeInputTokens: 900, LifetimeOutputTokens: 300, LifetimeCostUsd: numericFor(0.30), RunCount: 4,
		},
		adminPerUser: []store.AdminUsagePerUserRow{
			{UserID: uuid.New(), Email: "heavy@x", InputTokens: 600, OutputTokens: 200, CostUsd: numericFor(0.20), RunCount: 3},
			{UserID: uuid.New(), Email: "light@x", InputTokens: 300, OutputTokens: 100, CostUsd: numericFor(0.10), RunCount: 1},
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
		} `json:"factory"`
		Users []struct {
			Email    string    `json:"email"`
			Usage    usageBody `json:"usage"`
			RunCount int64     `json:"run_count"`
		} `json:"users"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Factory.Lifetime.InputTokens != 900 || body.Factory.Lifetime.OutputTokens != 300 {
		t.Fatalf("factory totals wrong: %+v", body.Factory.Lifetime)
	}
	if len(body.Users) != 2 {
		t.Fatalf("want 2 per-user rows, got %d", len(body.Users))
	}
	sumIn := body.Users[0].Usage.InputTokens + body.Users[1].Usage.InputTokens
	if sumIn != body.Factory.Lifetime.InputTokens {
		t.Fatalf("per-user rows (%d) must sum to factory total (%d)", sumIn, body.Factory.Lifetime.InputTokens)
	}
	if body.Users[0].Email != "heavy@x" {
		t.Fatalf("users[0].email = %q, want heavy@x (heaviest first)", body.Users[0].Email)
	}
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
