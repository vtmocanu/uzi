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

// racingOutcomesStore decorates the real queries: each outcome-count read runs for real and
// then fires its hook once, which commits a terminal `failed` transition on a seeded run. That
// is a concurrent terminal transition landing deterministically right after the counts were
// read, with no sleeps and no goroutines. If the handler reads the per-origin breakdown in a
// separate statement, that read sees one more failure than `failed` did.
type racingOutcomesStore struct {
	*store.Queries
	afterSelf, afterAdmin, afterPerUser func()
}

func fireOnce(hook *func()) {
	if f := *hook; f != nil {
		*hook = nil
		f()
	}
}

func (s *racingOutcomesStore) SelfRunOutcomes(ctx context.Context, arg store.SelfRunOutcomesParams) (store.SelfRunOutcomesRow, error) {
	row, err := s.Queries.SelfRunOutcomes(ctx, arg)
	fireOnce(&s.afterSelf)
	return row, err
}

func (s *racingOutcomesStore) AdminRunOutcomes(ctx context.Context, landableOrigins []string) (store.AdminRunOutcomesRow, error) {
	row, err := s.Queries.AdminRunOutcomes(ctx, landableOrigins)
	fireOnce(&s.afterAdmin)
	return row, err
}

func (s *racingOutcomesStore) AdminRunOutcomesPerUser(ctx context.Context, landableOrigins []string) ([]store.AdminRunOutcomesPerUserRow, error) {
	rows, err := s.Queries.AdminRunOutcomesPerUser(ctx, landableOrigins)
	fireOnce(&s.afterPerUser)
	return rows, err
}

func sumOrigins(m map[string]int64) int64 {
	var s int64
	for _, v := range m {
		s += v
	}
	return s
}

func assertOriginsMatchFailed(t *testing.T, scope string, o apitypes.RunOutcomesDTO) {
	t.Helper()
	if s := sumOrigins(o.FailOrigins); s != o.Failed {
		t.Errorf("%s: sum(fail_origins)=%d != failed=%d (fail_origins=%v): the two were read from different snapshots",
			scope, s, o.Failed, o.FailOrigins)
	}
}

// TestUsageOutcomesSingleSnapshotIssue1451LiveDB is the issue #1451 regression: a run that turns
// terminal `failed` between the outcome-count read and the per-origin read must not make
// sum(fail_origins) != failed within one /usage or /admin/usage response. Each scope (self,
// factory, per-user) gets its own flippable run, flipped by a hook right after that scope's
// outcome counts were read.
func TestUsageOutcomesSingleSnapshotIssue1451LiveDB(t *testing.T) {
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

	userID, connID := uuid.New(), uuid.New()
	mustExecT(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("snap1451-%s@e2e", userID))
	t.Cleanup(func() { mustExecT(context.Background(), t, pool, `DELETE FROM users WHERE id = $1`, userID) })
	mustExecT(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	repoID := uuid.New()
	mustExecT(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1451, 'g/snap1451-'||$3, 'https://forge.e2e/g/snap1451', 'main', true)`,
		repoID, connID, userID.String())

	iid := int64(1_451_000)
	seedRun := func(status string, failOrigin *string) uuid.UUID {
		iid++
		id := uuid.New()
		mustExecT(ctx, t, pool,
			`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, fail_origin)
			 VALUES ($1, $2, $3, 'issue', $4, 't', 'd', $5, $6)`,
			id, userID, repoID, iid, status, failOrigin)
		return id
	}
	agentFailure := "agent_failure"
	seedRun("failed", &agentFailure) // an already-failed run, so fail_origins is non-empty
	seedRun("completed", nil)
	flipSelf := seedRun("queued", nil)
	flipFactory := seedRun("queued", nil)
	flipPerUser := seedRun("queued", nil)

	flip := func(runID uuid.UUID) func() {
		return func() {
			mustExecT(ctx, t, pool,
				`UPDATE runs SET status = 'failed', fail_origin = 'agent_failure' WHERE id = $1`, runID)
		}
	}
	racing := &racingOutcomesStore{
		Queries:      q,
		afterSelf:    flip(flipSelf),
		afterAdmin:   flip(flipFactory),
		afterPerUser: flip(flipPerUser),
	}
	h := &Handler{q: q, wsvc: workersvc.New(racing, nil, workersvc.Params{})}

	call := func(fn http.HandlerFunc, admin bool) []byte {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req = req.WithContext(mw.ContextWithUser(req.Context(), store.User{ID: userID, IsAdmin: admin, IsActive: true}))
		rec := httptest.NewRecorder()
		fn(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		return rec.Body.Bytes()
	}

	// --- SelfUsage: flipSelf turns failed right after SelfRunOutcomes returned.
	var self apitypes.SelfUsageDTO
	if err := json.Unmarshal(call(h.SelfUsage, false), &self); err != nil {
		t.Fatalf("decode self usage: %v", err)
	}
	if racing.afterSelf != nil {
		t.Fatal("self hook never fired: SelfUsage no longer reads SelfRunOutcomes, the regression is not exercised")
	}
	assertOriginsMatchFailed(t, "self lifetime", self.Outcomes.Lifetime)
	assertOriginsMatchFailed(t, "self last_7_days", self.Outcomes.Last7Days)

	// --- AdminUsage: flipFactory turns failed after AdminRunOutcomes, flipPerUser after
	// AdminRunOutcomesPerUser.
	var admin apitypes.AdminUsageDTO
	if err := json.Unmarshal(call(h.AdminUsage, true), &admin); err != nil {
		t.Fatalf("decode admin usage: %v", err)
	}
	if racing.afterAdmin != nil || racing.afterPerUser != nil {
		t.Fatal("admin hooks never fired: AdminUsage no longer reads the outcome queries, the regression is not exercised")
	}
	assertOriginsMatchFailed(t, "factory lifetime", admin.Factory.Outcomes.Lifetime)
	assertOriginsMatchFailed(t, "factory last_7_days", admin.Factory.Outcomes.Last7Days)
	var found bool
	for _, u := range admin.Users {
		if u.UserID == userID.String() {
			found = true
			assertOriginsMatchFailed(t, "per-user lifetime", u.Outcomes)
		}
	}
	if !found {
		t.Fatalf("admin usage has no row for the seeded user %s", userID)
	}
}

// Compile-time guard that the decorator still satisfies the workersvc store seam.
var _ workersvc.Store = (*racingOutcomesStore)(nil)
