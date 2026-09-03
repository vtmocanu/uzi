package workersvc

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestRefoldRunUsageLiveDB proves M3's history refold against a REAL Postgres (PRD
// #1079). It seeds a pre-migration terminal run exactly as the 00187 migration would
// leave it — the eight 02854d5e fixture frames in run_messages, ONE collapsed
// epoch-0 run_usage row per model shaped like the shipped MAX-per-model fold (the
// 77.185539 rollup that IS the bug), and usage_refolded=false — then runs
// RefoldRunUsage and asserts the run's total becomes the fixture's per-leg SUM,
// keyed six ways by (model, epoch), with the marker set. It then checks convergence
// (a second call is a no-op; a re-delivered leg-2 frame is a GREATEST no-op) and
// selection (a chat run, a still-running run and a post-migration run are NOT picked
// by ListRunsPendingUsageRefold, while the still-running pre-migration run keeps the
// awaiting count above zero — the boot ticker's stop condition).
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (named OUTSIDE
// the uzi- namespace, per the store live-DB harness). A package that prints `ok` with
// PASS=0 is invalid, not green — hence the loud skip.
func TestRefoldRunUsageLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	defer pool.Close()
	q := store.New(pool)

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}

	userID, connID, repoID := uuid.New(), uuid.New(), uuid.New()
	exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("refold-%s@e2e", userID))
	exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, repoID, connID)

	const sess = "sess-02854d5e"
	// insertIssueRun makes an 'issue' run (repo_id + issue_iid, per runs_kind_shape).
	insertIssueRun := func(status string, usageRefolded bool) uuid.UUID {
		t.Helper()
		id := uuid.New()
		exec(`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind, session_id, usage_refolded)
		      VALUES ($1, $2, $3, 1, 't', 'd', $4, 'issue', $5, $6)`,
			id, userID, repoID, status, sess, usageRefolded)
		return id
	}

	// The run under test: a pre-migration terminal non-chat run, marked for refold.
	runID := insertIssueRun("completed", false)

	// Decoys the pending selector must EXCLUDE.
	// A chat run: repo_id/issue_iid NULL per runs_kind_shape; born refolded (00187 never
	// set chat rows false), so it is excluded by the usage_refolded marker.
	chatRunID := uuid.New()
	exec(`INSERT INTO runs (id, user_id, issue_title, issue_description, status, kind, session_id, usage_refolded)
	      VALUES ($1, $2, 't', 'd', 'completed', 'chat', $3, true)`, chatRunID, userID, sess)
	runningRunID := insertIssueRun("running", false)     // pre-migration but not yet terminal
	postMigrationID := insertIssueRun("completed", true) // born after the migration → already refolded

	// Seed the eight fixture frames (four init + four result) into run_messages.
	var frames recordedFrames
	readFixture(t, "result-frames-02854d5e.json", &frames)
	if len(frames.Frames) != 8 {
		t.Fatalf("fixture broken: expected 8 frames, got %d", len(frames.Frames))
	}
	for _, f := range frames.Frames {
		exec(`INSERT INTO run_messages (run_id, seq, kind, payload) VALUES ($1, $2, $3, $4)`,
			runID, f.Seq, f.Kind, []byte(f.Payload))
	}

	// Seed the collapsed epoch-0 rows the shipped MAX-per-model fold produced: one row
	// per model whose columns are the per-column MAX across that model's legs (opus's
	// leg-3 dominates every opus column, so this is the 75.03/0.01/2.15 = 77.185539
	// rollup). RefoldRunUsage must DELETE these before folding per leg, or their epoch-0
	// rows would sum on top of the correct per-leg rows.
	type col struct {
		in, cacheR, cacheC, out int64
		cost                    float64
	}
	collapsed := map[string]col{}
	var rollup recordedRollup
	readFixture(t, "run-usage-02854d5e.json", &rollup)
	for _, r := range rollup.Rows {
		c := collapsed[r.Model]
		c.in = max64(c.in, r.InputTokens)
		c.cacheR = max64(c.cacheR, r.CacheReadTokens)
		c.cacheC = max64(c.cacheC, r.CacheCreationTokens)
		c.out = max64(c.out, r.OutputTokens)
		if r.CostUSD > c.cost {
			c.cost = r.CostUSD
		}
		collapsed[r.Model] = c
	}
	for model, c := range collapsed {
		exec(`INSERT INTO run_usage (run_id, session_id, model, lineage_epoch, input_tokens, cache_read_tokens, cache_creation_tokens, output_tokens, cost_usd, updated_at)
		      VALUES ($1, $2, $3, 0, $4, $5, $6, $7, $8::numeric, now())`,
			runID, sess, model, c.in, c.cacheR, c.cacheC, c.out, fmt.Sprintf("%.6f", c.cost))
	}

	// --- Selection: only the terminal pre-migration run is pending; the decoys are not.
	pending, err := q.ListRunsPendingUsageRefold(ctx, store.ListRunsPendingUsageRefoldParams{Lim: 50})
	if err != nil {
		t.Fatalf("ListRunsPendingUsageRefold: %v", err)
	}
	pendingIDs := map[uuid.UUID]bool{}
	for _, r := range pending {
		pendingIDs[r.ID] = true
	}
	if !pendingIDs[runID] {
		t.Fatal("the pre-migration terminal run must be pending refold")
	}
	if pendingIDs[chatRunID] {
		t.Error("a chat run (usage_refolded=true) must NOT be pending")
	}
	if pendingIDs[runningRunID] {
		t.Error("a still-running run must NOT be pending (only terminal runs refold)")
	}
	if pendingIDs[postMigrationID] {
		t.Error("a post-migration run (usage_refolded=true) must NOT be pending")
	}

	// --- The refold itself.
	run, err := q.GetRunByID(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if err := RefoldRunUsage(ctx, pool, q, run); err != nil {
		t.Fatalf("RefoldRunUsage: %v", err)
	}

	// The run's total is now the fixture's per-leg SUM, not the collapsed 77.185539.
	assertFixtureTotal := func(when string) {
		t.Helper()
		var in, cacheR, cacheC, out int64
		var costOK bool
		if err := pool.QueryRow(ctx,
			`SELECT input_tokens, cache_read_tokens, cache_creation_tokens, output_tokens,
			        cost_usd = '153.582776'::numeric
			 FROM run_usage_totals WHERE run_id = $1`, runID).
			Scan(&in, &cacheR, &cacheC, &out, &costOK); err != nil {
			t.Fatalf("read run_usage_totals (%s): %v", when, err)
		}
		if in != 12785 || cacheR != 187880173 || cacheC != 5500712 || out != 1021240 {
			t.Fatalf("%s: totals wrong: in=%d cacheR=%d cacheC=%d out=%d (want 12785/187880173/5500712/1021240)",
				when, in, cacheR, cacheC, out)
		}
		if !costOK {
			t.Fatalf("%s: cost_usd must equal 153.582776 (the per-leg SUM), not the collapsed 77.185539", when)
		}
	}
	assertFixtureTotal("after refold")

	// Exactly six (model, epoch) rows: haiku@1, opus@1..4, sonnet@4.
	rowCount := func() int64 {
		var n int64
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM run_usage WHERE run_id = $1`, runID).Scan(&n); err != nil {
			t.Fatalf("count run_usage rows: %v", err)
		}
		return n
	}
	if n := rowCount(); n != 6 {
		t.Fatalf("expected 6 per-(model,epoch) rows after refold, got %d", n)
	}

	// The marker is set.
	refoldedNow := func(id uuid.UUID) bool {
		var b bool
		if err := pool.QueryRow(ctx, `SELECT usage_refolded FROM runs WHERE id = $1`, id).Scan(&b); err != nil {
			t.Fatalf("read usage_refolded: %v", err)
		}
		return b
	}
	if !refoldedNow(runID) {
		t.Fatal("usage_refolded must be true after RefoldRunUsage")
	}

	// --- Convergence: a second call is a no-op (delete + re-fold + mark, same result).
	run2, err := q.GetRunByID(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun (2): %v", err)
	}
	if err := RefoldRunUsage(ctx, pool, q, run2); err != nil {
		t.Fatalf("RefoldRunUsage (second call): %v", err)
	}
	assertFixtureTotal("after second refold")
	if n := rowCount(); n != 6 {
		t.Fatalf("second refold changed row count to %d, want 6 (must be a no-op)", n)
	}

	// A re-delivered leg-2 frame (the seq-1002 result, epoch 2, opus only) after the
	// refold recomputes the same position-absolute epoch and is a GREATEST no-op.
	var leg2 IncomingMessage
	for _, f := range frames.Frames {
		if f.Seq == 1002 {
			leg2 = IncomingMessage{Seq: f.Seq, Kind: f.Kind, Payload: f.Payload}
		}
	}
	if leg2.Seq == 0 {
		t.Fatal("fixture broken: no seq-1002 leg-2 result frame")
	}
	if err := foldUsageFrames(ctx, q, run2, []IncomingMessage{leg2}); err != nil {
		t.Fatalf("re-fold leg-2 frame: %v", err)
	}
	assertFixtureTotal("after leg-2 re-delivery")
	if n := rowCount(); n != 6 {
		t.Fatalf("leg-2 re-delivery changed row count to %d, want 6", n)
	}

	// --- The awaiting count outlives the refolded run: the still-running pre-migration
	// run keeps it above zero, which is exactly why the boot ticker stops on
	// CountRunsAwaitingUsageRefold==0 rather than on the terminal-only pending list.
	awaiting, err := q.CountRunsAwaitingUsageRefold(ctx)
	if err != nil {
		t.Fatalf("CountRunsAwaitingUsageRefold: %v", err)
	}
	if awaiting < 1 {
		t.Fatalf("awaiting = %d, want >= 1 (the still-running pre-migration run is not yet refolded)", awaiting)
	}
	stillPending, err := q.ListRunsPendingUsageRefold(ctx, store.ListRunsPendingUsageRefoldParams{Lim: 50})
	if err != nil {
		t.Fatalf("ListRunsPendingUsageRefold (after): %v", err)
	}
	for _, r := range stillPending {
		if r.ID == runID {
			t.Fatal("the refolded run must no longer be pending")
		}
	}
}

// TestListRunsPendingUsageRefoldExclusion pins the query-level exclusion the boot
// refold loop's head-of-line-starvation fix relies on (PRD #1079 M3 hardening): a
// per-run refold failure adds the run to a per-cycle skip-set passed as ExcludeIds so
// the drain advances PAST a poison row instead of re-listing the identical oldest
// batch forever, while an EMPTY exclusion list (id <> ALL('{}'::uuid[]) is TRUE for
// every row) excludes nothing. Seed three terminal pending runs; excluding two returns
// only the third; excluding none returns all three.
//
// MUTATION-VERIFY: drop the `AND id <> ALL(@exclude_ids::uuid[])` clause from
// ListRunsPendingUsageRefold and this test FAILS (the two "excluded" runs come back).
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres, same harness as
// TestRefoldRunUsageLiveDB.
func TestListRunsPendingUsageRefoldExclusion(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	defer pool.Close()
	q := store.New(pool)

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}

	userID, connID, repoID := uuid.New(), uuid.New(), uuid.New()
	exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("refold-excl-%s@e2e", userID))
	exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, repoID, connID)

	const sess = "sess-refold-excl"
	// Three terminal pre-migration runs, all pending refold (usage_refolded=false).
	insert := func() uuid.UUID {
		t.Helper()
		id := uuid.New()
		exec(`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind, session_id, usage_refolded)
		      VALUES ($1, $2, $3, 1, 't', 'd', 'completed', 'issue', $4, false)`,
			id, userID, repoID, sess)
		return id
	}
	a, b, c := insert(), insert(), insert()

	seen := func(exclude []uuid.UUID) map[uuid.UUID]bool {
		t.Helper()
		rows, err := q.ListRunsPendingUsageRefold(ctx, store.ListRunsPendingUsageRefoldParams{
			ExcludeIds: exclude,
			Lim:        50,
		})
		if err != nil {
			t.Fatalf("ListRunsPendingUsageRefold: %v", err)
		}
		m := map[uuid.UUID]bool{}
		for _, r := range rows {
			m[r.ID] = true
		}
		return m
	}

	// Excluding two of the three returns only the third.
	got := seen([]uuid.UUID{a, b})
	if got[a] || got[b] {
		t.Fatalf("excluded runs a/b must NOT be returned; got a=%v b=%v", got[a], got[b])
	}
	if !got[c] {
		t.Fatal("the non-excluded run c must be returned")
	}

	// Empty exclusion list excludes nothing: all three come back. Use a non-nil empty
	// slice, exactly as drainPending builds it (make([]uuid.UUID, 0, ...)).
	all := seen([]uuid.UUID{})
	if !all[a] || !all[b] || !all[c] {
		t.Fatalf("empty ExcludeIds must exclude nothing; got a=%v b=%v c=%v", all[a], all[b], all[c])
	}
}
