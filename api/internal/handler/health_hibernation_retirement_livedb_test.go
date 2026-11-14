package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/clitoken"
)

// PRD #4208 M1 (V24): the fleet.hibernation_retirement dry run through the REAL admin-health
// endpoint and the real evaluator. One evaluation covers at most 50 workers, oldest first; a
// 51st worker makes the check warn and is left out of the evidence, and 50 is the ok control.
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (./e2e/run-store-it.sh).

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
	h, router, pool := cliLiveDB(t)
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
		t.Skipf("%d unrelated hibernated workers share this database", existing)
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
