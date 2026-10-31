package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// TestAdminUsagePRCountsNoUsageUserLiveDB (issue #3686): a user whose only run is still
// `running` with an mr_iid has no usage row and no terminal outcome, yet owns a PR, so
// AdminUsage must surface a zero-usage row carrying pr_count 1 / last7_pr_count 1. The
// database is shared with other tests, so only this test's own user is asserted.
func TestAdminUsagePRCountsNoUsageUserLiveDB(t *testing.T) {
	ctx := context.Background()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh for live-DB coverage")
	}
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	q := store.New(pool)

	userID, connID, repoID := uuid.New(), uuid.New(), uuid.New()
	mustExecT(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("prcount3686-%s@e2e", userID))
	t.Cleanup(func() { mustExecT(context.Background(), t, pool, `DELETE FROM users WHERE id = $1`, userID) })
	mustExecT(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	mustExecT(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 3686, 'g/prcount3686-'||$3, 'https://forge.e2e/g/prcount3686', 'main', true)`,
		repoID, connID, userID.String())
	mustExecT(ctx, t, pool,
		`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, mr_iid)
		 VALUES ($1, $2, $3, 'issue', 368600, 't', 'd', 'running', 36860)`,
		uuid.New(), userID, repoID)

	h := &Handler{q: q, wsvc: workersvc.New(q, nil, workersvc.Params{CustodyHoldLimit: defaultCustodyLimit()})}
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req = req.WithContext(mw.ContextWithUser(req.Context(), store.User{ID: userID, IsAdmin: true, IsActive: true}))
	rec := httptest.NewRecorder()
	h.AdminUsage(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var admin apitypes.AdminUsageDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &admin); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if admin.Factory.LifetimePRCount < 1 || admin.Factory.Last7PRCount < 1 {
		t.Errorf("factory pr counts %d/%d must include the seeded PR", admin.Factory.LifetimePRCount, admin.Factory.Last7PRCount)
	}
	// One response: every PR owner gets exactly one row, and the rows' counts sum to the
	// factory's in both windows (the LiveDB packages run serially, so nothing writes runs
	// between the handler's reads).
	var sumLifetime, sumLast7 int64
	rowsByUser := map[string]int{}
	for _, u := range admin.Users {
		sumLifetime += u.PRCount
		sumLast7 += u.Last7PRCount
		rowsByUser[u.UserID]++
		if rowsByUser[u.UserID] > 1 {
			t.Errorf("user %s has more than one row", u.UserID)
		}
	}
	if sumLifetime != admin.Factory.LifetimePRCount || sumLast7 != admin.Factory.Last7PRCount {
		t.Errorf("per-user pr counts sum to %d/%d, factory reports %d/%d",
			sumLifetime, sumLast7, admin.Factory.LifetimePRCount, admin.Factory.Last7PRCount)
	}
	var found bool
	for _, u := range admin.Users {
		if u.UserID != userID.String() {
			continue
		}
		found = true
		if u.PRCount != 1 || u.Last7PRCount != 1 {
			t.Errorf("pr counts = %d/%d, want 1/1", u.PRCount, u.Last7PRCount)
		}
		if u.RunCount != 0 || u.Usage.CostUSD != 0 || u.Last7Days == nil {
			t.Errorf("row must be zero-usage with non-null last_7_days: %+v", u)
		}
		if u.Outcomes.FailOrigins == nil || u.Last7Outcomes.FailOrigins == nil {
			t.Errorf("fail_origins must be {} not null: %+v", u)
		}
	}
	if !found {
		t.Fatalf("admin usage has no row for the PR-only user %s", userID)
	}
}
