package store_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Issue #4684, migration 00542: the run_usage_model_totals and run_outcome_classes views
// the analytics Spend statements fold over. Both tests skip without
// UZI_TEST_DATABASE_URL (the store IT runner supplies a throwaway Postgres).

func openAnalyticsViewsDB(t *testing.T) (context.Context, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via the store integration runner for live-DB coverage")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return ctx, pool
}

func seedAnalyticsViewsUser(ctx context.Context, t *testing.T, pool *pgxpool.Pool, tag string) (userID, repoID uuid.UUID) {
	t.Helper()
	userID, connID, repoID := uuid.New(), uuid.New(), uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("aview-%s-%s@e2e", tag, userID))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/r-aview-'||$3, 'https://forge.e2e/g/r', 'main', true)`, repoID, connID, tag+"-"+userID.String())
	return userID, repoID
}

// reduceCostStatus is 00244's run-level cost_status CASE, restated in Go.
func reduceCostStatus(statuses []string) string {
	has := map[string]bool{}
	for _, s := range statuses {
		has[s] = true
	}
	switch {
	case has["unreported"]:
		return "unreported"
	case has["metered"] && has["subscription"]:
		return "unreported"
	case has["subscription"]:
		return "subscription"
	default:
		return "metered"
	}
}

func TestRunUsageModelTotalsParityLiveDB(t *testing.T) {
	ctx, pool := openAnalyticsViewsDB(t)
	userID, repoID := seedAnalyticsViewsUser(ctx, t, pool, "usage")

	iid := int64(2_000_000)
	seedRun := func() uuid.UUID {
		iid++
		id := uuid.New()
		mustExec(ctx, t, pool,
			`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status)
			 VALUES ($1, $2, $3, $4, 't', 'd', 'running')`, id, userID, repoID, iid)
		return id
	}
	// use inserts one run_usage row. The other token columns are derived from `in` so a
	// column-mixing bug shows up: output = in/10, cache_read = in/5, cache_creation = in/25
	// (callers pass multiples of 50 so each division is exact).
	use := func(run uuid.UUID, session, model string, epoch, lineage int, basis string, in int64, status, cost string) {
		t.Helper()
		mustExec(ctx, t, pool,
			`INSERT INTO run_usage (run_id, session_id, model, lineage_epoch, lineage_index, usage_basis,
			     input_tokens, cache_read_tokens, cache_creation_tokens, output_tokens, cost_usd, cost_status)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11::numeric, $12)`,
			run, session, model, epoch, lineage, basis, in, in/5, in/25, in/10, cost, status)
	}

	// multi: model A has per_leg legs at epochs 0 and 1, with epoch 1 split across two
	// sessions (MAX within the leg); model B is subscription. Run status: unreported.
	multi := seedRun()
	use(multi, "s1", "model-a", 0, 0, "per_leg", 1000, "metered", "1.5")
	use(multi, "s1", "model-a", 1, 0, "per_leg", 300, "metered", "0.5")
	use(multi, "s2", "model-a", 1, 0, "per_leg", 400, "metered", "0.7")
	use(multi, "s1", "model-b", 0, 0, "per_leg", 200, "subscription", "0")

	// cumulative: model A lineage 0 is per_leg 100, then cumulative legs reporting 250 and
	// 400 (running session totals); lineage 1 is a fresh session reporting a cumulative 50.
	// Model B is plain per_leg 50 + 100. Hand-computed, independent of either view:
	//   model A lineage 0: 100 + max(250-100, 400-100) = 400; lineage 1: 50 -> input 450
	//   (cache_read 90, cache_creation 18, output 45, cost 0.450000).
	//   model B: input 150, cache_read 30, cache_creation 6, output 15, cost 0.150000.
	cumulative := seedRun()
	use(cumulative, "s1", "model-a", 0, 0, "per_leg", 100, "metered", "0.1")
	use(cumulative, "s1", "model-a", 1, 0, "session_cumulative", 250, "metered", "0.25")
	use(cumulative, "s1", "model-a", 2, 0, "session_cumulative", 400, "metered", "0.4")
	use(cumulative, "s2", "model-a", 3, 1, "session_cumulative", 50, "metered", "0.05")
	use(cumulative, "s2", "model-b", 0, 0, "per_leg", 50, "metered", "0.05")
	use(cumulative, "s2", "model-b", 1, 0, "per_leg", 100, "metered", "0.1")

	// mixed: an unreported model plus a metered one (run status unreported).
	mixed := seedRun()
	use(mixed, "s1", "model-a", 0, 0, "per_leg", 50, "unreported", "0")
	use(mixed, "s1", "model-b", 0, 0, "per_leg", 100, "metered", "0.02")

	// subs: every model subscription (run status subscription).
	subs := seedRun()
	use(subs, "s1", "model-a", 0, 0, "per_leg", 50, "subscription", "0")
	use(subs, "s1", "model-b", 0, 0, "per_leg", 100, "subscription", "0")

	// legs: three models over three epochs, all metered.
	legs := seedRun()
	for e := 0; e < 3; e++ {
		for _, m := range []string{"model-a", "model-b", "model-c"} {
			use(legs, "s1", m, e, 0, "per_leg", int64(50*(e+1)), "metered", "0.01")
		}
	}

	// none: a run with no usage rows at all.
	none := seedRun()

	withUsage := []uuid.UUID{multi, cumulative, mixed, subs, legs}
	all := append(append([]uuid.UUID{}, withUsage...), none)

	// Per run: SUM over model rows equals run_usage_totals on every token column and cost.
	rows, err := pool.Query(ctx, `
		SELECT t.run_id
		FROM run_usage_totals t
		JOIN (SELECT run_id,
		             SUM(input_tokens)::bigint AS i, SUM(cache_read_tokens)::bigint AS cr,
		             SUM(cache_creation_tokens)::bigint AS cc, SUM(output_tokens)::bigint AS o,
		             SUM(cost_usd) AS c
		      FROM run_usage_model_totals WHERE run_id = ANY($1) GROUP BY run_id) m ON m.run_id = t.run_id
		WHERE t.run_id = ANY($1)
		  AND t.input_tokens = m.i AND t.cache_read_tokens = m.cr AND t.cache_creation_tokens = m.cc
		  AND t.output_tokens = m.o AND t.cost_usd = m.c`, all)
	if err != nil {
		t.Fatalf("parity query: %v", err)
	}
	matched := map[uuid.UUID]bool{}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		matched[id] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("parity rows: %v", err)
	}
	for _, id := range withUsage {
		if !matched[id] {
			t.Errorf("run %s: SUM over run_usage_model_totals rows differs from run_usage_totals", id)
		}
	}
	if len(matched) != len(withUsage) {
		t.Errorf("parity matched %d runs, want %d", len(matched), len(withUsage))
	}

	// Per-model statuses reduce (00244 CASE) to the run-level status; one row per
	// (run, model) present in run_usage; the no-usage run is absent from both views.
	type key struct {
		run   uuid.UUID
		model string
	}
	perModelStatus := map[uuid.UUID][]string{}
	seen := map[key]bool{}
	mrows, err := pool.Query(ctx,
		`SELECT run_id, model, cost_status FROM run_usage_model_totals WHERE run_id = ANY($1)`, all)
	if err != nil {
		t.Fatalf("model rows: %v", err)
	}
	for mrows.Next() {
		var k key
		var st string
		if err := mrows.Scan(&k.run, &k.model, &st); err != nil {
			t.Fatalf("scan model row: %v", err)
		}
		if seen[k] {
			t.Errorf("run_usage_model_totals has more than one row for run %s model %s", k.run, k.model)
		}
		seen[k] = true
		perModelStatus[k.run] = append(perModelStatus[k.run], st)
	}
	mrows.Close()
	if err := mrows.Err(); err != nil {
		t.Fatalf("model rows: %v", err)
	}
	for _, id := range withUsage {
		var want string
		if err := pool.QueryRow(ctx, `SELECT cost_status FROM run_usage_totals WHERE run_id = $1`, id).Scan(&want); err != nil {
			t.Fatalf("totals status (run %s): %v", id, err)
		}
		if got := reduceCostStatus(perModelStatus[id]); got != want {
			t.Errorf("run %s: reduced per-model cost_status %q (from %v) != run_usage_totals %q", id, got, perModelStatus[id], want)
		}
		var wantModels int
		if err := pool.QueryRow(ctx, `SELECT count(DISTINCT model) FROM run_usage WHERE run_id = $1`, id).Scan(&wantModels); err != nil {
			t.Fatalf("model count (run %s): %v", id, err)
		}
		if len(perModelStatus[id]) != wantModels {
			t.Errorf("run %s: %d view rows, want one per run_usage model (%d)", id, len(perModelStatus[id]), wantModels)
		}
	}
	for _, wantStatus := range []struct {
		run  uuid.UUID
		want string
	}{{multi, "unreported"}, {cumulative, "metered"}, {mixed, "unreported"}, {subs, "subscription"}, {legs, "metered"}} {
		if got := reduceCostStatus(perModelStatus[wantStatus.run]); got != wantStatus.want {
			t.Errorf("run %s: reduced status %q, want %q", wantStatus.run, got, wantStatus.want)
		}
	}
	if len(perModelStatus[none]) != 0 {
		t.Errorf("no-usage run has %d rows in run_usage_model_totals, want 0", len(perModelStatus[none]))
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM run_usage_totals WHERE run_id = $1`, none).Scan(&n); err != nil {
		t.Fatalf("totals count: %v", err)
	}
	if n != 0 {
		t.Errorf("no-usage run has %d rows in run_usage_totals, want 0", n)
	}

	// Hand-computed per-model values, independent of either view.
	checkModel := func(run uuid.UUID, model string, in, cr, cc, out int64, cost string) {
		t.Helper()
		var gIn, gCr, gCc, gOut int64
		var costOK bool
		err := pool.QueryRow(ctx,
			`SELECT input_tokens, cache_read_tokens, cache_creation_tokens, output_tokens, cost_usd = $3::numeric
			 FROM run_usage_model_totals WHERE run_id = $1 AND model = $2`, run, model, cost).
			Scan(&gIn, &gCr, &gCc, &gOut, &costOK)
		if err != nil {
			t.Fatalf("read model row (%s): %v", model, err)
		}
		if gIn != in || gCr != cr || gCc != cc || gOut != out || !costOK {
			t.Errorf("%s: got in %d/cr %d/cc %d/out %d (cost match %t), want %d/%d/%d/%d cost %s",
				model, gIn, gCr, gCc, gOut, costOK, in, cr, cc, out, cost)
		}
	}
	checkModel(cumulative, "model-a", 450, 90, 18, 45, "0.45")
	checkModel(cumulative, "model-b", 150, 30, 6, 15, "0.15")
	// multi model A: per_leg epoch 0 = 1000, epoch 1 = MAX(300, 400) = 400 -> 1400, cost 1.5 + 0.7.
	checkModel(multi, "model-a", 1400, 280, 56, 140, "2.2")
}

func TestRunOutcomeClassesParityLiveDB(t *testing.T) {
	ctx, pool := openAnalyticsViewsDB(t)
	userID, repoID := seedAnalyticsViewsUser(ctx, t, pool, "outcome")

	oiid := int64(3_000_000)
	seed := func(kind, status string, failOrigin *string) uuid.UUID {
		t.Helper()
		oiid++
		id := uuid.New()
		if kind == "chat" {
			mustExec(ctx, t, pool,
				`INSERT INTO runs (id, user_id, kind, issue_title, issue_description, status, fail_origin)
				 VALUES ($1, $2, $3, 't', 'd', $4, $5)`, id, userID, kind, status, failOrigin)
			return id
		}
		mustExec(ctx, t, pool,
			`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, fail_origin)
			 VALUES ($1, $2, $3, $4, $5, 't', 'd', $6, $7)`, id, userID, repoID, kind, oiid, status, failOrigin)
		return id
	}
	planRejected, baseDiverged, agentFailure := "plan_rejected", "base_diverged", "agent_failure"

	want := map[string]string{}
	add := func(outcome string, id uuid.UUID) { want[id.String()] = outcome }
	add("completed", seed("issue", "completed", nil))
	add("completed", seed("issue", "completed", nil))
	add("cancelled", seed("issue", "cancelled", nil))
	add("plan_rejected", seed("issue", "failed", &planRejected))
	add("base_diverged", seed("issue", "failed", &baseDiverged))
	add("failed", seed("issue", "failed", &agentFailure))
	add("failed", seed("issue", "failed", nil))
	add("", seed("issue", "running", nil))
	add("", seed("issue", "queued", nil))
	chat := seed("chat", "completed", nil)

	got := map[string]string{}
	rows, err := pool.Query(ctx,
		`SELECT c.run_id, c.outcome FROM run_outcome_classes c JOIN runs r ON r.id = c.run_id WHERE r.user_id = $1`, userID)
	if err != nil {
		t.Fatalf("outcome view: %v", err)
	}
	for rows.Next() {
		var id uuid.UUID
		var outcome *string
		if err := rows.Scan(&id, &outcome); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if outcome == nil {
			got[id.String()] = ""
		} else {
			got[id.String()] = *outcome
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("outcome rows: %v", err)
	}
	if _, present := got[chat.String()]; present {
		t.Errorf("chat run %s must be absent from run_outcome_classes", chat)
	}
	if len(got) != len(want) {
		t.Errorf("view has %d rows for the user, want %d (chat excluded)", len(got), len(want))
	}
	for id, w := range want {
		if g, ok := got[id]; !ok || g != w {
			t.Errorf("run %s: outcome %q (present=%t), want %q (empty = NULL, in flight)", id, g, ok, w)
		}
	}

	// Counts equal SelfRunOutcomes lifetime counts for the same user.
	counts := map[string]int64{}
	for _, o := range got {
		counts[o]++
	}
	self, err := store.New(pool).SelfRunOutcomes(ctx, store.SelfRunOutcomesParams{
		UserID: userID, LandableOrigins: workersvc.AllHumanLandableFailOrigins(),
	})
	if err != nil {
		t.Fatalf("SelfRunOutcomes: %v", err)
	}
	finished := counts["completed"] + counts["cancelled"] + counts["plan_rejected"] + counts["base_diverged"] + counts["failed"]
	for _, c := range []struct {
		name      string
		view, own int64
	}{
		{"finished", finished, self.LifetimeFinished},
		{"completed", counts["completed"], self.LifetimeCompleted},
		{"cancelled", counts["cancelled"], self.LifetimeCancelled},
		{"plan_rejected", counts["plan_rejected"], self.LifetimePlanRejected},
		{"base_diverged", counts["base_diverged"], self.LifetimeBaseDiverged},
		{"failed", counts["failed"], self.LifetimeFailed},
	} {
		if c.view != c.own {
			t.Errorf("%s: view count %d != SelfRunOutcomes %d", c.name, c.view, c.own)
		}
	}
	if counts["completed"] != 2 || finished != 7 {
		t.Errorf("seed sanity: completed %d finished %d, want 2 and 7", counts["completed"], finished)
	}
}
