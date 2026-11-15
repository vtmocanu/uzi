package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/clitoken"
)

// PRD #4208 M1 (V24): the fleet.hibernation_retirement dry run through the REAL admin-health
// endpoint and the real evaluator. One evaluation covers at most 50 workers, oldest first; a
// 51st worker makes the check warn and is left out of the evidence, and 50 is the ok control.
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (./e2e/run-store-it.sh).

// Retirement health evaluates the entire fleet, not just this fixture's owner. Give each
// test a fresh database rather than skipping or modifying unrelated shared-package workers.
func retirementHealthLiveDB(t *testing.T) (*Handler, http.Handler, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close(context.Background()) })
	name := "retirement_health_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	ident := pgx.Identifier{name}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+ident); err != nil {
		t.Fatalf("create dedicated retirement health database: %v", err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := admin.Exec(cleanup, "DROP DATABASE "+ident+" WITH (FORCE)"); err != nil {
			t.Errorf("drop dedicated retirement health database: %v", err)
		}
	})
	// Reuse the existing URL/keyword-DSN override, including query-string dbname handling.
	dedicatedDSN := ephemeralPriorityDSN(t, dsn, name)
	probe, err := pgx.Connect(ctx, dedicatedDSN)
	if err != nil {
		t.Fatal(err)
	}
	var actual string
	err = probe.QueryRow(ctx, "SELECT current_database()").Scan(&actual)
	_ = probe.Close(ctx)
	if err != nil || actual != name {
		t.Fatalf("retirement health database = %q, want %q: %v", actual, name, err)
	}
	t.Setenv("UZI_TEST_DATABASE_URL", dedicatedDSN)
	// cliLiveDB migrates the new database and wires the real services and router. Its pool
	// cleanup runs before DROP DATABASE because test cleanups run in reverse order.
	h, router, pool := cliLiveDB(t)
	if err := pool.QueryRow(ctx, "SELECT current_database()").Scan(&actual); err != nil || actual != name {
		t.Fatalf("retirement health fixture connected to %q, want %q: %v", actual, name, err)
	}
	return h, router, pool
}

const retirementConditionalText = "would retire only after M2 verifies: actual PV reclaim policy; no intervening pod between observations (sampled only in M1)"

// seedEligibleHibernated inserts n persistent hosted workers hibernated for more than 22 days, each
// with a complete accepted inventory that nothing blocks. Worker "ret-1" is the newest, "ret-<n>" the oldest.
func seedEligibleHibernated(t *testing.T, ctx context.Context, h *Handler, user uuid.UUID, n int) {
	t.Helper()
	mustExecT(ctx, t, h.pool, `
WITH w AS (
  INSERT INTO workers (id, user_id, name, token_hash, status, kind, hosted_size, template_declared, hibernation_state, hibernation_op_id, hibernated_at)
  SELECT gen_random_uuid(), $1, 'ret-' || g, decode(md5(g::text || clock_timestamp()::text), 'hex'), 'online', 'hosted', 'm', 'base',
         'hibernated', gen_random_uuid(), now() - interval '22 days' - g * interval '1 minute'
  FROM generate_series(1, $2::int) g
  RETURNING id, hibernation_op_id
), p AS (
  SELECT id, hibernation_op_id AS op, gen_random_uuid() AS pod FROM w
)
INSERT INTO worker_hibernation_inventories (worker_id, op_id, observed_at, deployment_uid, deployment_resource_version, pod_uid, pvcs,
  register_nonce, received_at, status, complete, entries, receipt_keys, digest, fingerprint, lane, mechanism, uncovered,
  scaling_down_pod_uid, fence_generation, scaled_down_at, last_held_at, exclusion_final_at, exclusion_final_digest, exclusion_final_fingerprint)
SELECT id, op, now(), 'dep', 'rv', pod,
  '[{"role":"data","name":"d","uid":"u1","class_reclaim_policy":"Delete"},{"role":"dind_data","name":"i","uid":"u3","class_reclaim_policy":"Delete"}]'::jsonb,
  'nonce', now(), 'accepted', true, '[]'::jsonb, '[]'::jsonb, repeat('b', 64), repeat('a', 64), 'dind', 'epoch_ex+latch+quiescence', '[]'::jsonb,
  pod, 1, now(), now(), now(), repeat('b', 64), repeat('a', 64)
FROM p`, user, n)
}

type retirementCheck struct {
	Severity string `json:"severity"`
	Summary  string `json:"summary"`
	Evidence []struct {
		Label string `json:"label"`
		Value string `json:"value"`
	} `json:"evidence"`
}

func fetchRetirementCheck(t *testing.T, router http.Handler, token string) retirementCheck {
	t.Helper()
	rec := bearerReq(router, http.MethodGet, "/api/admin/health", token)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/admin/health = %d: %s", rec.Code, rec.Body.String())
	}
	var doc struct {
		Checks []struct {
			ID string `json:"id"`
			retirementCheck
		} `json:"checks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	for _, c := range doc.Checks {
		if c.ID == "fleet.hibernation_retirement" {
			return c.retirementCheck
		}
	}
	t.Fatalf("no fleet.hibernation_retirement check in %s", rec.Body.String())
	return retirementCheck{}
}

func TestAdminHealthHibernationRetirementCapLiveDB(t *testing.T) {
	h, router, pool := retirementHealthLiveDB(t)
	ctx := context.Background()
	h.wsvc.SetTxBeginner(pool)
	// The handler serves a cached evaluation for a few seconds; each fetch advances this clock past it.
	clock := time.Now()
	h.now = func() time.Time { return clock }
	admin := cliSeedUser(t, pool, true)
	token := cliMintToken(t, pool, admin, clitoken.ScopeAdminRO)
	fetch := func() retirementCheck {
		clock = clock.Add(time.Minute)
		return fetchRetirementCheck(t, router, token)
	}
	var existing int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workers WHERE hibernation_state IN ('hibernated','waking')`).Scan(&existing); err != nil {
		t.Fatal(err)
	}
	if existing != 0 {
		t.Fatalf("fresh retirement health database contains %d unexpected hibernated/waking workers", existing)
	}

	// Nothing hibernated: not applicable.
	if c := fetch(); c.Severity != "na" || !strings.Contains(c.Summary, "Dry run: no volumes are deleted") {
		t.Fatalf("empty fleet = %+v, want na", c)
	}

	seedEligibleHibernated(t, ctx, h, admin, 51)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM workers WHERE name LIKE 'ret-%' AND user_id = $1`, admin)
	})
	var newest string
	if err := pool.QueryRow(ctx, `SELECT left(id::text, 8) FROM workers WHERE name = 'ret-1'`).Scan(&newest); err != nil {
		t.Fatal(err)
	}
	c := fetch()
	if c.Severity != "warn" || !strings.Contains(c.Summary, "Dry run: no volumes are deleted") || !strings.HasSuffix(c.Summary, " This check evaluated 50 of 51 (oldest first).") ||
		strings.Contains(c.Summary, "shown") {
		t.Fatalf("51 eligible workers = %q %q, want warn naming the cap", c.Severity, c.Summary)
	}
	if len(c.Evidence) == 0 {
		t.Fatal("no evidence rows")
	}
	for _, e := range c.Evidence {
		if e.Label == newest {
			t.Fatalf("the newest worker %s is in the evidence", newest)
		}
		if want := retirementConditionalText + "; class policy Delete"; e.Value != want {
			t.Fatalf("evidence value = %q, want %q", e.Value, want)
		}
	}

	// Control: exactly 50 is fully evaluated, nothing is blocked, and the check is ok.
	if _, err := pool.Exec(ctx, `DELETE FROM workers WHERE name = 'ret-1'`); err != nil {
		t.Fatal(err)
	}
	c = fetch()
	if c.Severity != "ok" || strings.Contains(c.Summary, "(oldest first)") || !strings.Contains(c.Summary, "Dry run: no volumes are deleted") {
		t.Fatalf("50 eligible workers = %q %q, want ok", c.Severity, c.Summary)
	}
}

// PRD #4208 M1 (V24): three real evaluator outcomes rendered by the mounted admin-health endpoint,
// not hand-built verdicts. Example 2 is a blocked worker past the period whose retained source has no
// receipt; example 3 is a waking worker whose older op's inventory was accepted; the third row is a
// worker nothing blocks.
func TestAdminHealthHibernationRetirementExamplesLiveDB(t *testing.T) {
	h, router, pool := retirementHealthLiveDB(t)
	ctx := context.Background()
	h.wsvc.SetTxBeginner(pool)
	clock := time.Now()
	h.now = func() time.Time { return clock }
	admin := cliSeedUser(t, pool, true)
	token := cliMintToken(t, pool, admin, clitoken.ScopeAdminRO)
	var existing int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workers WHERE hibernation_state IN ('hibernated','waking')`).Scan(&existing); err != nil {
		t.Fatal(err)
	}
	if existing != 0 {
		t.Fatalf("fresh retirement health database contains %d unexpected hibernated/waking workers", existing)
	}

	// ret-1 (newest) stays conditional, ret-2 is blocked, ret-3 (oldest) is waking.
	seedEligibleHibernated(t, ctx, h, admin, 3)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM workers WHERE name LIKE 'ret-%' AND user_id = $1`, admin)
	})
	run := uuid.New()
	entries := `[{"kind":"runner_ref","path":"refs/uzi-runner/agent/issue-3","classification":"needs_receipt","run_id":"` + run.String() +
		`","tip":"` + strings.Repeat("a", 40) + `","content_state":"ref"}]`
	mustExecT(ctx, t, h.pool, `UPDATE worker_hibernation_inventories SET entries = $2::jsonb
		WHERE worker_id = (SELECT id FROM workers WHERE name = 'ret-2' AND user_id = $1)`, admin, entries)
	mustExecT(ctx, t, h.pool, `UPDATE workers SET hibernation_state = 'waking', hibernation_op_id = gen_random_uuid(), wake_requested_at = now()
		WHERE name = 'ret-3' AND user_id = $1`, admin)
	short := func(name string) string {
		var id string
		if err := pool.QueryRow(ctx, `SELECT left(id::text, 8) FROM workers WHERE name = $1 AND user_id = $2`, name, admin).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}

	clock = clock.Add(time.Minute)
	c := fetchRetirementCheck(t, router, token)
	if c.Severity != "warn" {
		t.Fatalf("severity = %q (%s), want warn for the blocked worker", c.Severity, c.Summary)
	}
	want := map[string]string{
		short("ret-1"): retirementConditionalText + "; class policy Delete",
		short("ret-2"): "blocked by no_receipt (runner_ref:refs/uzi-runner/agent/issue-3 run " + run.String() + "); class policy Delete, actual PV unverified",
		short("ret-3"): "cancelled: wake requested",
	}
	got := map[string]string{}
	for _, e := range c.Evidence {
		got[e.Label] = e.Value
	}
	if len(got) != len(want) {
		t.Fatalf("evidence = %+v, want exactly %d rows", c.Evidence, len(want))
	}
	for label, value := range want {
		if got[label] != value {
			t.Fatalf("evidence %s = %q, want %q", label, got[label], value)
		}
	}
}
