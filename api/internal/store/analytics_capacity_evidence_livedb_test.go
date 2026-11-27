//go:build analyticsevidence

package store_test

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/analytics"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4731 (U4): capacity workloads for the opt-in evidence harness in
// analytics_evidence_livedb_test.go (same build tag, same environment knobs; select them with
// UZI_EVIDENCE_ONLY=cap-). The capacity seed runs after the #4684 seed on the same connection:
// it gives every non-chat run a first_started_at and a lognormal-ish retained end, turns a few
// queued runs into ongoing ones, marks some runs as having waited on a usage limit, and adds the
// provider identities the four gauge statements read (more than 200 enabled Anthropic secrets
// and more than 200 linked Codex accounts with several aliases each, for the top owner).

const evTopEmail = "evid-0001@e2e"

var evCapacitySeed = []string{
	`SELECT setseed(0.4731)`,
	// Terminal runs: first_started_at = created_at + exponential queue delay (mean 2 min); duration
	// lognormal (median 15 min, sigma 1.3) with a 0.5% multi-day tail so some intervals cross
	// window starts. Chat runs never start (first_started_at stays NULL).
	`UPDATE runs r SET first_started_at = x.fs, finished_at = x.fe, status_since = x.fe
	 FROM (SELECT s.id, s.fs, LEAST(s.fs + s.dur * interval '1 second', TIMESTAMPTZ '` + evNowRFC + `' - interval '1 second') AS fe
	       FROM (SELECT id, LEAST(created_at + (-120 * ln(1 - u0)) * interval '1 second', TIMESTAMPTZ '` + evNowRFC + `' - interval '2 seconds') AS fs,
	                    CASE WHEN u3 < 0.005 THEN 86400 * (1 + 4 * u1)
	                         ELSE LEAST(exp(ln(900) + 1.3 * sqrt(-2 * ln(1 - u1)) * cos(2 * pi() * u2)), 1209600) END AS dur
	             FROM (SELECT id, created_at, random() AS u0, random() AS u1, random() AS u2, random() AS u3
	                   FROM runs WHERE status IN ('completed', 'failed', 'cancelled') AND kind <> 'chat') u) s) x
	 WHERE r.id = x.id`,
	// Ongoing: queued runs of the last 2 days start running, plus 1% of the older queued runs stay
	// "running" forever (stale-looking ongoing intervals that span whole windows).
	`UPDATE runs SET status = 'running', first_started_at = created_at + interval '30 seconds',
	        status_since = created_at + interval '30 seconds'
	 WHERE status = 'queued' AND (created_at > TIMESTAMPTZ '` + evNowRFC + `' - interval '2 days' OR random() < 0.01)`,
	`UPDATE runs SET limit_wait_count = 1 + floor(random() * 3)::int WHERE kind <> 'chat' AND random() < 0.08`,

	// Anthropic: the top owner holds 230 enabled-or-disabled tokens, everyone else 2; 5% disabled;
	// 90% of the enabled ones carry a current-revision reading.
	`INSERT INTO user_secrets (id, user_id, kind, label, is_default, ciphertext, sealed_with)
	 SELECT gen_random_uuid(), u.id, 'anthropic_token', 't' || lpad(g::text, 4, '0'), g = 1, '\x01'::bytea, 'master'
	 FROM users u
	 CROSS JOIN LATERAL generate_series(1, CASE WHEN u.email = '` + evTopEmail + `' THEN 230 ELSE 2 END) g
	 WHERE u.email LIKE 'evid-%'`,
	`UPDATE user_secrets SET disabled_at = TIMESTAMPTZ '` + evNowRFC + `' WHERE kind = 'anthropic_token' AND NOT is_default AND random() < 0.05`,
	`INSERT INTO anthropic_rate_limits (user_secret_id, user_id, five_hour_pct, five_hour_resets_at, seven_day_pct,
	                                    seven_day_resets_at, source, synced_at, enablement_rev)
	 SELECT id, user_id, floor(random() * 100)::int, TIMESTAMPTZ '` + evNowRFC + `' + interval '2 hours', floor(random() * 100)::int,
	        TIMESTAMPTZ '` + evNowRFC + `' + interval '3 days', 'usage_endpoint', TIMESTAMPTZ '` + evNowRFC + `', enablement_rev
	 FROM user_secrets WHERE kind = 'anthropic_token' AND disabled_at IS NULL AND random() < 0.9`,

	// Codex: the top owner holds 210 provider accounts, users 2-120 hold 1-3; each account has
	// 1-3 linked aliases (user_secrets kind codex_auth + a linked credential state); 3% of the
	// aliases are disabled; 85% of the accounts carry a reading stamped with their alias set.
	`CREATE TEMP TABLE ev_cxacc AS
	 SELECT u.id AS user_id, a, gen_random_uuid() AS acc_id
	 FROM users u
	 CROSS JOIN LATERAL generate_series(1, CASE WHEN u.email = '` + evTopEmail + `' THEN 210
	      WHEN substring(u.email from 6 for 4)::int <= 120 THEN 1 + substring(u.email from 6 for 4)::int % 3 ELSE 0 END) a
	 WHERE u.email LIKE 'evid-%'`,
	`INSERT INTO codex_provider_account (id, user_id, provider_user_id, workspace_account_id, sealed_login, sealed_with)
	 SELECT acc_id, user_id, 'p-' || acc_id, 'w-' || acc_id, '\x01'::bytea, 'master' FROM ev_cxacc`,
	`CREATE TEMP TABLE ev_cxal AS
	 SELECT c.user_id, c.acc_id, c.a, k, gen_random_uuid() AS sid
	 FROM ev_cxacc c CROSS JOIN LATERAL generate_series(1, 1 + (c.a % 3)) k`,
	`INSERT INTO user_secrets (id, user_id, kind, label, is_default, ciphertext, sealed_with)
	 SELECT sid, user_id, 'codex_auth', 'cx' || a || '-' || k, a = 1 AND k = 1, '\x01'::bytea, 'master' FROM ev_cxal`,
	`INSERT INTO codex_credential_state (user_secret_id, user_id, status, provider_account_id, material_revision)
	 SELECT sid, user_id, 'linked', acc_id, 0 FROM ev_cxal`,
	`UPDATE user_secrets SET disabled_at = TIMESTAMPTZ '` + evNowRFC + `' WHERE kind = 'codex_auth' AND NOT is_default AND random() < 0.03`,
	`INSERT INTO codex_account_rate_limits (user_id, provider_account_id, buckets, observed_generation,
	                                        observed_credential_revision, last_success_at, last_attempt_at, attempt_status, enablement_sig)
	 SELECT c.user_id, c.acc_id, '[{"id":"primary","used_percent":40}]'::jsonb, 0, 0,
	        TIMESTAMPTZ '` + evNowRFC + `', TIMESTAMPTZ '` + evNowRFC + `', 'ok',
	        (SELECT string_agg(us.id::text || ':' || us.enablement_rev::text, ',' ORDER BY us.id)
	         FROM ev_cxal l JOIN user_secrets us ON us.id = l.sid WHERE l.acc_id = c.acc_id AND us.disabled_at IS NULL)
	 FROM ev_cxacc c WHERE random() < 0.85`,
	`DROP TABLE ev_cxal`, `DROP TABLE ev_cxacc`,
	`ANALYZE runs`, `ANALYZE user_secrets`, `ANALYZE anthropic_rate_limits`, `ANALYZE codex_provider_account`,
	`ANALYZE codex_credential_state`, `ANALYZE codex_account_rate_limits`, `ANALYZE users`,
}

func (h *evHarness) seedCapacity() {
	h.t.Helper()
	start := time.Now()
	for _, s := range evCapacitySeed {
		h.simple(s)
	}
	h.logf("capacity seed took %s", time.Since(start).Round(time.Second))
}

// logCapacityShape records what the capacity workloads run against: terminal interval shape,
// nonterminal and usage-wait counts, and the provider identity counts.
func (h *evHarness) logCapacityShape() {
	h.t.Helper()
	top := "(SELECT id FROM users WHERE email = '" + evTopEmail + "')"
	for _, q := range []string{
		`SELECT 'terminal runs with first_started_at', count(*) FROM runs WHERE first_started_at IS NOT NULL AND status IN ('completed','failed','cancelled')`,
		`SELECT 'duration p50/p90/p99 seconds (terminal)', round(percentile_cont(0.5) WITHIN GROUP (ORDER BY extract(epoch FROM finished_at - first_started_at))::numeric) || '/' ||
		        round(percentile_cont(0.9) WITHIN GROUP (ORDER BY extract(epoch FROM finished_at - first_started_at))::numeric) || '/' ||
		        round(percentile_cont(0.99) WITHIN GROUP (ORDER BY extract(epoch FROM finished_at - first_started_at))::numeric)
		 FROM runs WHERE first_started_at IS NOT NULL AND status IN ('completed','failed','cancelled')`,
		`SELECT 'nonterminal by status', coalesce(string_agg(status || '=' || c, ' ' ORDER BY status), '') FROM (SELECT status, count(*) c FROM runs WHERE status NOT IN ('completed','failed','cancelled') GROUP BY status) x`,
		`SELECT 'ongoing with first_started_at', count(*) FROM runs WHERE first_started_at IS NOT NULL AND status NOT IN ('completed','failed','cancelled')`,
		`SELECT 'runs with limit_wait_count>0', count(*) FROM runs WHERE limit_wait_count > 0`,
		`SELECT 'top owner runs / total', (SELECT count(*) FROM runs WHERE user_id = ` + top + `) || ' / ' || (SELECT count(*) FROM runs)`,
		`SELECT 'anthropic secrets enabled (all / top owner)', (SELECT count(*) FROM user_secrets WHERE kind = 'anthropic_token' AND disabled_at IS NULL) || ' / ' ||
		        (SELECT count(*) FROM user_secrets WHERE kind = 'anthropic_token' AND disabled_at IS NULL AND user_id = ` + top + `)`,
		`SELECT 'anthropic readings', count(*) FROM anthropic_rate_limits`,
		`SELECT 'codex linked accounts (all / top owner)', count(*) || ' / ' || count(*) FILTER (WHERE a.user_id = ` + top + `)
		 FROM codex_provider_account a WHERE EXISTS (SELECT 1 FROM codex_credential_state s JOIN user_secrets us ON us.id = s.user_secret_id AND us.user_id = s.user_id
		       WHERE s.user_id = a.user_id AND s.provider_account_id = a.id AND s.status = 'linked' AND us.disabled_at IS NULL)`,
		`SELECT 'codex linked aliases enabled (all / per account avg)', count(*) || ' / ' || round(count(*)::numeric / nullif(count(DISTINCT s.provider_account_id), 0), 2)
		 FROM codex_credential_state s JOIN user_secrets us ON us.id = s.user_secret_id WHERE s.status = 'linked' AND us.disabled_at IS NULL`,
		`SELECT 'codex readings', count(*) FROM codex_account_rate_limits`,
		`SELECT 'users with any enabled identity', count(DISTINCT user_id) FROM user_secrets WHERE disabled_at IS NULL AND kind IN ('anthropic_token', 'codex_auth')`,
	} {
		h.logf("%s", h.shape(q))
	}
}

// capacityWorkload builds one Capacity statement workload. owner == uuid.Nil is the admin
// all-users statement; repo != uuid.Nil adds the repo filter.
func (h *evHarness) capacityWorkload(name string, owner uuid.UUID, win analytics.Window, repo uuid.UUID) evWorkload {
	h.t.Helper()
	kinds := runkind.All()
	var hist pgtype.Timestamptz
	var err error
	if owner != uuid.Nil {
		hist, err = h.q.AnalyticsOwnerHistoryStart(h.ctx, store.AnalyticsOwnerHistoryStartParams{UserID: owner, Kinds: kinds})
	} else {
		hist, err = h.q.AnalyticsAllHistoryStart(h.ctx, store.AnalyticsAllHistoryStartParams{Kinds: kinds})
	}
	if err != nil {
		h.t.Fatalf("history start: %v", err)
	}
	var earliest *time.Time
	if hist.Valid {
		e := hist.Time
		earliest = &e
	}
	b, err := analytics.ComputeBounds(h.now, time.UTC, win, earliest)
	if err != nil {
		h.t.Fatalf("ComputeBounds: %v", err)
	}
	if err := analytics.ValidateBuckets(b); err != nil {
		h.t.Fatalf("ValidateBuckets: %v", err)
	}
	var repoArg pgtype.UUID
	if repo != uuid.Nil {
		repoArg = pgtype.UUID{Bytes: repo, Valid: true}
	}
	capture := &capturingDBTX{}
	cq := store.New(capture)
	w := evWorkload{name: name}
	var scope string
	if owner != uuid.Nil {
		p := store.AnalyticsOwnerCapacityParams{
			Now: spendTS(b.Now), From: spendTS(b.From), UserID: owner, RepoID: repoArg, Kinds: kinds,
			BucketStarts: spendTSs(b.BucketStarts), BucketEnds: spendTSs(b.BucketEnds),
		}
		_, _ = cq.AnalyticsOwnerCapacity(h.ctx, p)
		w.stmts = []evStmt{{name: "AnalyticsOwnerCapacity", sql: store.AnalyticsOwnerCapacitySQL}}
		w.run = func(ctx context.Context, q *store.Queries) error {
			_, err := q.AnalyticsOwnerCapacity(ctx, p)
			return err
		}
		scope = "WHERE user_id = " + sqlLiteral(h.t, owner)
		h.logCapacitySelectivity(name, win, scope, b, func() ([]byte, error) { return h.q.AnalyticsOwnerCapacity(h.ctx, p) })
	} else {
		p := store.AnalyticsAllCapacityParams{
			Now: spendTS(b.Now), From: spendTS(b.From), RepoID: repoArg, Kinds: kinds,
			BucketStarts: spendTSs(b.BucketStarts), BucketEnds: spendTSs(b.BucketEnds),
		}
		_, _ = cq.AnalyticsAllCapacity(h.ctx, p)
		w.stmts = []evStmt{{name: "AnalyticsAllCapacity", sql: store.AnalyticsAllCapacitySQL}}
		w.run = func(ctx context.Context, q *store.Queries) error {
			_, err := q.AnalyticsAllCapacity(ctx, p)
			return err
		}
		h.logCapacitySelectivity(name, win, "", b, func() ([]byte, error) { return h.q.AnalyticsAllCapacity(h.ctx, p) })
	}
	lits := make([]string, len(capture.args))
	for i, a := range capture.args {
		lits[i] = sqlLiteral(h.t, a)
	}
	w.stmts[0].args = strings.Join(lits, ", ")
	return w
}

// logCapacitySelectivity runs the statement once and logs how much of the scope's runs the
// statement's candidate set selects, and an upper bound on the sweep events it sorts (two interval
// endpoints per interval plus two per bucket; endpoint rows outside the window may not be emitted,
// so the figure is not a measured row count).
func (h *evHarness) logCapacitySelectivity(name string, win analytics.Window, scope string, b analytics.Bounds, run func() ([]byte, error)) {
	h.t.Helper()
	raw, err := run()
	if err != nil {
		h.t.Fatalf("%s: %v", name, err)
	}
	var doc struct {
		Population map[string]int64 `json:"population"`
		UsageWait  struct {
			Window struct {
				Count int64 `json:"count"`
				N     int64 `json:"n"`
			} `json:"window"`
		} `json:"usage_wait"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		h.t.Fatalf("%s: decode: %v", name, err)
	}
	var total int64
	h.scalar("SELECT count(*) FROM runs "+scope, &total)
	cand, withIv := doc.Population["candidates"], doc.Population["with_interval"]
	events := 2*withIv + 2*int64(len(b.BucketStarts))
	h.logf("CAPACITY SELECTIVITY %s: window=%s from=%s buckets=%d scope_runs=%d candidates=%d (%.2f%% of scope) with_interval=%d without_start=%d invalid=%d ongoing=%d end_from_status_since=%d sweep_events_upper_bound=%d usage_wait_n=%d usage_wait_count=%d",
		name, win, b.From.Format(time.RFC3339), len(b.BucketStarts), total, cand, 100*float64(cand)/float64(max(total, 1)),
		withIv, doc.Population["without_start"], doc.Population["invalid_interval"], doc.Population["ongoing"],
		doc.Population["end_from_status_since"], events, doc.UsageWait.Window.N, doc.UsageWait.Window.Count)
}

func (h *evHarness) capacityWorkloads() []evWorkload {
	h.t.Helper()
	var topRepo uuid.UUID
	h.scalar(`SELECT repo_id FROM runs WHERE user_id = (SELECT id FROM users WHERE email = '`+evTopEmail+`') AND repo_id IS NOT NULL
	          GROUP BY repo_id ORDER BY count(*) DESC LIMIT 1`, &topRepo)
	owner := sqlLiteral(h.t, h.topOwner)
	gauge := func(name string, stmts []evStmt, run func(context.Context, *store.Queries) error) evWorkload {
		return evWorkload{name: name, stmts: stmts, run: run}
	}
	ws := []evWorkload{
		h.capacityWorkload("cap-owner-30d", h.topOwner, analytics.Window30d, uuid.Nil),
		h.capacityWorkload("cap-owner-window-all", h.topOwner, analytics.WindowAll, uuid.Nil),
		h.capacityWorkload("cap-admin-7d", uuid.Nil, analytics.Window7d, uuid.Nil),
		h.capacityWorkload("cap-admin-30d", uuid.Nil, analytics.Window30d, uuid.Nil),
		h.capacityWorkload("cap-admin-90d", uuid.Nil, analytics.Window90d, uuid.Nil),
		h.capacityWorkload("cap-admin-30d-repo", uuid.Nil, analytics.Window30d, topRepo),
		h.capacityWorkload("cap-admin-window-all", uuid.Nil, analytics.WindowAll, uuid.Nil),
		gauge("cap-gauge-owner-anthropic", []evStmt{{"AnalyticsOwnerAnthropicGauges", store.AnalyticsOwnerAnthropicGaugesSQL, owner}},
			func(ctx context.Context, q *store.Queries) error {
				_, err := q.AnalyticsOwnerAnthropicGauges(ctx, h.topOwner)
				return err
			}),
		gauge("cap-gauge-all-anthropic", []evStmt{{"AnalyticsAllAnthropicGauges", store.AnalyticsAllAnthropicGaugesSQL, ""}},
			func(ctx context.Context, q *store.Queries) error {
				_, err := q.AnalyticsAllAnthropicGauges(ctx)
				return err
			}),
		gauge("cap-gauge-owner-codex", []evStmt{{"AnalyticsOwnerCodexGauges", store.AnalyticsOwnerCodexGaugesSQL, owner}},
			func(ctx context.Context, q *store.Queries) error {
				_, err := q.AnalyticsOwnerCodexGauges(ctx, h.topOwner)
				return err
			}),
		gauge("cap-gauge-all-codex", []evStmt{{"AnalyticsAllCodexGauges", store.AnalyticsAllCodexGaugesSQL, ""}},
			func(ctx context.Context, q *store.Queries) error {
				_, err := q.AnalyticsAllCodexGauges(ctx)
				return err
			}),
	}
	return ws
}
