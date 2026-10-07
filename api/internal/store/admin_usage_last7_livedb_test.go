package store_test

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// The transaction pins PostgreSQL now() for an inclusive cutoff and rolls back
// the isolated factory dataset, including captures, after every assertion.
func TestAdminUsageSevenDayWindowsLiveDB(t *testing.T) {
	e := setupHarnessEnv(t)
	ctx := e.ctx
	tx, err := e.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := tx.Rollback(context.Background()); err != nil && err != pgx.ErrTxClosed {
			t.Error(err)
		}
	}()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := tx.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	// Prior live-DB tests leave runs in the shared database. This reset is visible
	// only in this transaction and is undone by rollback; the harness runs serially.
	exec("TRUNCATE runs CASCADE")
	q := store.New(tx)
	users := []uuid.UUID{e.userID, uuid.New(), uuid.New(), uuid.New()}
	for _, id := range users[1:] {
		exec("INSERT INTO users (id,email,password_hash) VALUES ($1,$2,'x')", id, fmt.Sprintf("last7-%s@example.com", id))
	}
	type seed struct {
		user                            int
		age, status, kind, origin, cost string
		factor                          int64
		patch                           bool
	}
	rows := []seed{
		{0, "7 days", "completed", "issue", "", "metered", 1, false},
		{0, "7 days 0.000001 seconds", "failed", "issue", "agent_failure", "metered", 2, false},
		{0, "1 day", "cancelled", "issue", "", "subscription", 3, false},
		{0, "1 day", "failed", "issue", "plan_rejected", "unreported", 4, false},
		{0, "1 day", "failed", "issue", "", "", 0, false},
		{0, "1 day", "failed", "issue", "workflow_scope_missing", "", 0, true},
		{0, "1 day", "failed", "issue", "push_secret_blocked", "", 0, false},
		{0, "1 day", "failed", "issue", "history_rewritten", "", 0, false},
		{0, "1 day", "failed", "issue", "finalize_base_align_conflict", "", 0, false},
		{0, "1 day", "failed", "issue", "agent_failure", "", 0, true},
		{0, "1 day", "queued", "issue", "", "", 0, false},
		{0, "1 day", "failed", "chat", "agent_failure", "metered", 100, false},
		{0, "1 day", "failed", "judge", "agent_failure", "metered", 5, false},
		{0, "1 day", "failed", "cross_check", "agent_failure", "metered", 6, false},
		{1, "10 days", "completed", "issue", "", "metered", 7, false},
		{1, "10 days", "failed", "issue", "workflow_scope_missing", "metered", 8, true},
		{2, "1 day", "failed", "issue", "", "", 0, false},
		{3, "1 day", "running", "issue", "", "subscription", 9, false},
	}
	ids := make([]uuid.UUID, len(rows))
	for i, r := range rows {
		ids[i] = uuid.New()
		var origin, patch any
		if r.origin != "" {
			origin = r.origin
		}
		if r.patch {
			patch = "saved patch"
		}
		// Respect each kind's shape, so excluded kinds exercise the real queries.
		var repo, iid, target, budget any = e.repoID, int64(243900 + i), nil, nil
		harness, reportOnly := "claude", false
		if r.kind == "chat" || r.kind == "judge" {
			repo, iid = nil, nil
		}
		if r.kind == "judge" || r.kind == "cross_check" {
			target = ids[0]
		}
		if r.kind == "cross_check" {
			iid, harness, reportOnly, budget = nil, "codex", true, 60
		}
		exec(`INSERT INTO runs (id,user_id,repo_id,issue_iid,issue_title,issue_description,kind,status,fail_origin,preserved_patch,created_at,finished_at,target_run_id,harness,report_only,budget_wall_seconds)
   VALUES ($1,$2,$3,$4,'t','d',$5,$6,$7,$8,now()-$9::interval,now(),$10,$11,$12,$13)`,
			ids[i], users[r.user], repo, iid, r.kind, r.status, origin, patch, r.age, target, harness, reportOnly, budget)
		if r.factor != 0 {
			cost := int64(0)
			if r.cost == "metered" {
				cost = r.factor * 5
			}
			exec(`INSERT INTO run_usage (run_id,session_id,model,input_tokens,cache_read_tokens,cache_creation_tokens,output_tokens,cost_usd,cost_status)
    VALUES ($1,'s','m',$2,$3,$4,$5,$6::numeric/1000000,$7)`, ids[i], r.factor, 2*r.factor, 3*r.factor, 4*r.factor, cost, r.cost)
		}
	}
	// Recent creation with an old finish must still be included. Conversely the run
	// just outside the cutoff finishes now and must stay out of the seven-day window.
	exec("UPDATE runs SET finished_at=now()-interval '20 days' WHERE id=$1", ids[0])
	capture := func(run uuid.UUID, owner uuid.UUID, state string) {
		hold := uuid.New()
		exec(`INSERT INTO recovery_custody_holds (id,user_id,run_id,generation,state,original_worker_id,original_worker_identity)
   VALUES ($1,$2,$3,1,'open',$4,'fixture')`, hold, owner, run, uuid.New())
		exec(`INSERT INTO recovery_captures (hold_id,run_id,user_id,original_worker_identity,source_sha,idempotency_key,state)
   VALUES ($1,$2,$3,'fixture','abc',$4,$5)`, hold, run, owner, uuid.New().String(), state)
	}
	capture(ids[6], users[0], "available")
	capture(ids[6], users[0], "available") // EXISTS must not fan out.
	capture(ids[7], users[0], "expired")
	capture(ids[8], users[1], "available") // Wrong owner is not recoverable.

	usage, err := q.AdminUsagePerUser(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byUser := map[uuid.UUID]store.AdminUsagePerUserRow{}
	for _, r := range usage {
		byUser[r.UserID] = r
	}
	if len(byUser) != 3 {
		t.Fatalf("usage population: %+v", usage)
	}
	if _, ok := byUser[users[2]]; ok {
		t.Fatal("outcomes-only user has usage")
	}
	// Independent expected token totals, microdollars, counts and cost exclusions.
	wantUsage := map[uuid.UUID][16]int64{
		users[0]: {21, 42, 63, 84, 70, 6, 1, 1, 19, 38, 57, 76, 60, 5, 1, 1},
		users[1]: {15, 30, 45, 60, 75, 2, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
		users[3]: {9, 18, 27, 36, 0, 1, 1, 0, 9, 18, 27, 36, 0, 1, 1, 0},
	}
	var sums [16]int64
	for id, want := range wantUsage {
		r, ok := byUser[id]
		if !ok {
			t.Fatalf("missing lifetime user %s", id)
		}
		got := [16]int64{r.InputTokens, r.CacheReadTokens, r.CacheCreationTokens, r.OutputTokens, microsOf(t, r.CostUsd), r.RunCount, r.SubscriptionRunCount, r.UnreportedRunCount,
			r.Last7InputTokens, r.Last7CacheReadTokens, r.Last7CacheCreationTokens, r.Last7OutputTokens, microsOf(t, r.Last7CostUsd), r.Last7RunCount, r.Last7SubscriptionRunCount, r.Last7UnreportedRunCount}
		if got != want {
			t.Fatalf("usage %s: got %v want %v", id, got, want)
		}
		for i, v := range got {
			sums[i] += v
		}
	}
	totals, err := q.AdminUsageTotals(ctx)
	if err != nil {
		t.Fatal(err)
	}
	factory := [16]int64{totals.LifetimeInputTokens, totals.LifetimeCacheReadTokens, totals.LifetimeCacheCreationTokens, totals.LifetimeOutputTokens, microsOf(t, totals.LifetimeCostUsd), totals.RunCount, totals.LifetimeSubscriptionRunCount, totals.LifetimeUnreportedRunCount,
		totals.Last7InputTokens, totals.Last7CacheReadTokens, totals.Last7CacheCreationTokens, totals.Last7OutputTokens, microsOf(t, totals.Last7CostUsd), 6, totals.Last7SubscriptionRunCount, totals.Last7UnreportedRunCount}
	// AdminUsageTotals has no seven-day run count; assert that slot independently.
	if sums != factory || sums != [16]int64{45, 90, 135, 180, 145, 9, 2, 1, 28, 56, 84, 112, 60, 6, 2, 1} {
		t.Fatalf("factory usage: sums %v factory %v", sums, factory)
	}

	origins := workersvc.AllHumanLandableFailOrigins()
	outcomes, err := q.AdminRunOutcomesPerUser(ctx, origins)
	if err != nil {
		t.Fatal(err)
	}
	wantCounts := map[uuid.UUID][12]int64{
		users[0]: {10, 1, 1, 1, 7, 2, 9, 1, 1, 1, 6, 2},
		users[1]: {2, 1, 0, 0, 1, 1, 0, 0, 0, 0, 0, 0},
		users[2]: {1, 0, 0, 0, 1, 0, 1, 0, 0, 0, 1, 0},
	}
	wantOrigins := map[uuid.UUID][2]map[string]int64{
		users[0]: {{"agent_failure": 2, "unknown": 1, "workflow_scope_missing": 1, "push_secret_blocked": 1, "history_rewritten": 1, "finalize_base_align_conflict": 1},
			{"agent_failure": 1, "unknown": 1, "workflow_scope_missing": 1, "push_secret_blocked": 1, "history_rewritten": 1, "finalize_base_align_conflict": 1}},
		users[1]: {{"workflow_scope_missing": 1}, {}},
		users[2]: {{"unknown": 1}, {"unknown": 1}},
	}
	decode := func(raw []byte) map[string]int64 {
		t.Helper()
		var m map[string]int64
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	var countSums [12]int64
	mapSums := [2]map[string]int64{{}, {}}
	if len(outcomes) != 3 {
		t.Fatalf("outcomes population: %+v", outcomes)
	}
	for _, r := range outcomes {
		got := [12]int64{r.Finished, r.Completed, r.Cancelled, r.PlanRejected, r.Failed, r.NeedsLanding, r.Last7Finished, r.Last7Completed, r.Last7Cancelled, r.Last7PlanRejected, r.Last7Failed, r.Last7NeedsLanding}
		want, ok := wantCounts[r.UserID]
		if !ok || got != want {
			t.Fatalf("outcomes %s: got %v want %v", r.UserID, got, want)
		}
		for i, v := range got {
			countSums[i] += v
		}
		maps := [2]map[string]int64{decode(r.FailOrigins), decode(r.Last7FailOrigins)}
		if !reflect.DeepEqual(maps, wantOrigins[r.UserID]) {
			t.Fatalf("origins %s: %+v", r.UserID, maps)
		}
		for i, m := range maps {
			for k, v := range m {
				mapSums[i][k] += v
			}
		}
		// Lifetime recency still observes finished_at, independently of created_at.
		if !r.LastFailedAt.Valid || r.LastFailedOrigin == "" || r.CompletedSinceLastFailure != 0 {
			t.Fatalf("lifetime recency: %+v", r)
		}
		if r.UserID == users[1] && r.LastFailedRunID != ids[15] {
			t.Fatal("old failure lost from lifetime recency")
		}
		self, err := q.SelfRunOutcomes(ctx, store.SelfRunOutcomesParams{UserID: r.UserID, LandableOrigins: origins})
		if err != nil {
			t.Fatal(err)
		}
		if self.LastFailedRunID != r.LastFailedRunID || self.LastFailedAt != r.LastFailedAt || self.LastFailedOrigin != r.LastFailedOrigin || self.CompletedSinceLastFailure != r.CompletedSinceLastFailure {
			t.Fatal("lifetime recency differs from self")
		}
	}
	f, err := q.AdminRunOutcomes(ctx, origins)
	if err != nil {
		t.Fatal(err)
	}
	counts := [12]int64{f.LifetimeFinished, f.LifetimeCompleted, f.LifetimeCancelled, f.LifetimePlanRejected, f.LifetimeFailed, f.LifetimeNeedsLanding, f.Last7Finished, f.Last7Completed, f.Last7Cancelled, f.Last7PlanRejected, f.Last7Failed, f.Last7NeedsLanding}
	if counts != countSums || counts != [12]int64{13, 2, 1, 1, 9, 3, 10, 1, 1, 1, 7, 2} {
		t.Fatalf("factory counts %v sums %v", counts, countSums)
	}
	if !reflect.DeepEqual(mapSums, [2]map[string]int64{decode(f.LifetimeFailOrigins), decode(f.Last7FailOrigins)}) {
		t.Fatal("factory origins differ from per-user sum")
	}
	if !f.LastFailedAt.Valid || f.CompletedSinceLastFailure != 0 || f.LastFailedUserID == uuid.Nil {
		t.Fatal("factory lifetime recency lost")
	}
}
