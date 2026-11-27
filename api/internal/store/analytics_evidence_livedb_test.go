//go:build analyticsevidence

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/analytics"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Issue #4684 (m5): plan and runtime evidence for the Spend statements and the options reads.
// This is a measurement harness, not a gate: it is compiled only with -tags analyticsevidence
// and runs only with UZI_ANALYTICS_PLAN_EVIDENCE=1 against a throwaway database (it
// TRUNCATEs users CASCADE) that carries the uzi_test_harness.database_marker row seeded by
// e2e/test-database-marker.sql; without it the run fails before touching anything. Run, one named package at a time:
//
//	cd api && UZI_ANALYTICS_PLAN_EVIDENCE=1 UZI_TEST_DATABASE_URL=... \
//	  go test -tags analyticsevidence -count=1 -v -timeout 50m \
//	  -run TestAnalyticsPlanEvidenceLiveDB ./internal/store
//
// Optional environment:
//
//	UZI_EVIDENCE_LOG        raw log path (appended); default stdout only
//	UZI_EVIDENCE_KEEP=1     leave the seed in place (default: TRUNCATE at the end)
//	UZI_EVIDENCE_RESEED=0   reuse an existing seed (skips the TRUNCATE and the seed). The
//	                        Reliability phase rewrites rows (landable-origin step), so a seed it
//	                        already ran on fails the run while the Spend phase is enabled: reseed,
//	                        or set UZI_EVIDENCE_SPEND=0
//	UZI_EVIDENCE_BASELINE=0 skip the baseline phase
//	UZI_EVIDENCE_INDEXES=   comma list of candidate numbers 1-7 (default 1,2,3,4; "" or "0" none; 5 is the #4731 Capacity candidate, 6-7 the #4730 Quality candidates)
//	UZI_EVIDENCE_ONLY=      comma list of workload-name substrings (cap- selects the #4731 capacity workloads)
//	UZI_EVIDENCE_RUNS=20    measured executions per workload and mode
//	UZI_EVIDENCE_MODES=     custom,generic (timing modes for the baseline)
//	UZI_EVIDENCE_SPEND=0    skip the Spend/options phase (and its candidate indexes)
//	UZI_EVIDENCE_RELIABILITY=0  skip the Reliability phase (issue #4685 m3)
//	UZI_EVIDENCE_REL_INDEX=0    skip the runs_terminal_end_idx before/after step

const (
	evNowRFC = "2026-11-20T12:00:00Z"
	evRuns   = 120000
)

// evSeed is executed statement by statement on ONE connection (temp tables).
var evSeed = []string{
	`SELECT setseed(0.4684)`,
	`TRUNCATE users CASCADE`,
	`INSERT INTO users (id, email, password_hash)
	 SELECT gen_random_uuid(), 'evid-' || lpad(g::text, 4, '0') || '@e2e', 'x' FROM generate_series(1, 200) g`,
	`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	 SELECT gen_random_uuid(), id, 'gitlab', 'https://forge.e2e', 'bot', 1, '\x01'::bytea FROM users WHERE email LIKE 'evid-%'`,
	`CREATE TEMP TABLE ev_owner AS
	 SELECT row_number() OVER (ORDER BY u.email) AS idx, u.id AS user_id, c.id AS conn_id
	 FROM users u JOIN forge_connections c ON c.user_id = u.id WHERE u.email LIKE 'evid-%'`,
	// 1250 repos: owner 1 holds 100, owners 2-10 hold 200 between them, the rest spread over 11-200.
	`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	 SELECT gen_random_uuid(), o.conn_id, j, 'g/evid-' || lpad(j::text, 5, '0'), 'https://forge.e2e/g/evid-' || j, 'main', true
	 FROM generate_series(1, 1250) j
	 JOIN ev_owner o ON o.idx = CASE WHEN j <= 100 THEN 1 WHEN j <= 300 THEN 2 + (j % 9) ELSE 11 + (j % 190) END`,
	`CREATE TEMP TABLE ev_repo AS
	 SELECT r.id, o.idx, row_number() OVER (PARTITION BY o.idx ORDER BY r.path_with_namespace) AS rn,
	        count(*) OVER (PARTITION BY o.idx) AS cnt
	 FROM repos r JOIN ev_owner o ON o.conn_id = r.connection_id WHERE r.path_with_namespace LIKE 'g/evid-%'`,
	// Owner skew: one owner 20%, owners 2-10 another 40% (top 10 = 60%), the rest 40% over 190.
	// Kinds: delivery 81% (issue 50, task 10, self_improve 6, ci_fix 7, prompt 5, job 3),
	// supporting 19% (judge 17, chat 2).
	`CREATE TEMP TABLE ev_gen AS
	 SELECT g, oidx,
	        CASE WHEN rk < 0.50 THEN 'issue' WHEN rk < 0.60 THEN 'task' WHEN rk < 0.66 THEN 'self_improve'
	             WHEN rk < 0.73 THEN 'ci_fix' WHEN rk < 0.78 THEN 'prompt' WHEN rk < 0.81 THEN 'job'
	             WHEN rk < 0.98 THEN 'judge' ELSE 'chat' END AS kind,
	        rs, rm, rq, rr, rh,
	        TIMESTAMPTZ '` + evNowRFC + `' - (CASE WHEN rt < 0.10 THEN (rt / 0.10) * 30
	                                             ELSE 30 + ((rt - 0.10) / 0.90) * 1065 END) * interval '1 day' AS ts
	 FROM (SELECT g,
	              CASE WHEN a < 0.20 THEN 1 WHEN a < 0.60 THEN 2 + floor(random() * 9)::int
	                   ELSE 11 + floor(random() * 190)::int END AS oidx,
	              random() AS rk, random() AS rs, random() AS rt, random() AS rm, random() AS rq,
	              random() AS rr, random() AS rh
	       FROM (SELECT g, random() AS a FROM generate_series(1, ` + strconv.Itoa(evRuns) + `) g) s) s2`,
	`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind, created_at,
	                   finished_at, status_since, harness, fail_origin, mr_iid, mr_state, branch, pipeline_id,
	                   pipeline_ref, job_type)
	 SELECT s.id, s.user_id, s.repo_id,
	        CASE WHEN s.kind IN ('issue', 'self_improve') THEN s.g END,
	        't', 'd', s.status, s.kind, s.ts, s.fin, COALESCE(s.fin, s.ts),
	        CASE WHEN s.rh < 0.7 THEN 'claude' ELSE 'codex' END,
	        CASE WHEN s.status = 'failed' THEN
	             CASE WHEN s.rq < 0.15 THEN 'plan_rejected' WHEN s.rq < 0.25 THEN 'base_diverged'
	                  WHEN s.rq < 0.70 THEN 'agent_failure' END END,
	        s.mr_iid,
	        CASE WHEN s.mr_iid IS NOT NULL THEN CASE WHEN s.mr_iid % 5 < 2 THEN 'merged' ELSE 'opened' END END,
	        CASE WHEN s.kind = 'task' THEN 'uzi/task/' || s.id END,
	        CASE WHEN s.kind = 'ci_fix' THEN s.g END,
	        CASE WHEN s.kind = 'ci_fix' THEN 'refs/pipe/' || s.g END,
	        CASE WHEN s.kind = 'job' THEN 'research' END
	 FROM (SELECT gen_random_uuid() AS id, o.user_id, g.g, g.kind, g.ts, g.rh, g.rq,
	              CASE WHEN g.kind IN ('chat', 'job') THEN NULL ELSE r.id END AS repo_id,
	              CASE WHEN g.rs < 0.62 THEN 'completed' WHEN g.rs < 0.80 THEN 'failed' WHEN g.rs < 0.90 THEN 'cancelled'
	                   WHEN g.rs >= 0.97 AND g.kind = 'issue' THEN 'queued' ELSE 'completed' END AS status,
	              CASE WHEN NOT (g.rs >= 0.97 AND g.kind = 'issue') THEN g.ts + (60 + g.rh * 7200) * interval '1 second' END AS fin,
	              CASE WHEN g.kind IN ('issue', 'task', 'self_improve', 'ci_fix', 'prompt') AND g.rm < 0.30
	                   THEN 1 + ((hashtext(g.g::text)::bigint & 2147483647) % 150)::int END AS mr_iid
	       FROM ev_gen g
	       JOIN ev_owner o ON o.idx = g.oidx
	       LEFT JOIN ev_repo r ON r.idx = g.oidx AND r.rn = 1 + floor(g.rr * r.cnt)::int
	       WHERE g.kind <> 'judge') s`,
	`CREATE TEMP TABLE ev_iss AS
	 SELECT id, user_id, row_number() OVER (PARTITION BY user_id ORDER BY created_at, id) AS rn,
	        count(*) OVER (PARTITION BY user_id) AS cnt
	 FROM runs WHERE kind = 'issue'`,
	`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind, created_at,
	                   finished_at, status_since, harness, target_run_id)
	 SELECT gen_random_uuid(), o.user_id, NULL, NULL, 't', 'd', CASE WHEN g.rs < 0.8 THEN 'completed' ELSE 'failed' END,
	        'judge', g.ts, g.ts + (60 + g.rh * 900) * interval '1 second', g.ts + (60 + g.rh * 900) * interval '1 second',
	        'claude', i.id
	 FROM ev_gen g
	 JOIN ev_owner o ON o.idx = g.oidx
	 JOIN ev_iss i ON i.user_id = o.user_id AND i.rn = 1 + floor(g.rr * i.cnt)::int
	 WHERE g.kind = 'judge'`,
	// run_usage: ~2 sessions x 2 models x 1-3 epochs per non-chat run; 10% of runs have no usage
	// at all, 15% record session_cumulative legs, 10% subscription and 10% unreported legs.
	`INSERT INTO run_usage (run_id, session_id, model, lineage_epoch, lineage_index, input_tokens, cache_read_tokens,
	                        cache_creation_tokens, output_tokens, cost_usd, harness, cost_status, usage_basis)
	 SELECT r.id, 'sess-' || s, 'ev-model-' || ((r.h % 8 + m * 3) % 8), e, s,
	        CASE WHEN r.h % 100 < 25 THEN (e + 1) * (100 + r.h % 50) ELSE 100 + e * 10 + r.h % 50 END,
	        20 + r.h % 30, 5 + r.h % 7, 50 + e,
	        CASE (r.h / 100) % 10 WHEN 0 THEN 0 WHEN 1 THEN 0
	             ELSE 0.001 * (e + 1) * (1 + r.h % 7) * CASE WHEN r.h % 100 < 25 THEN 2 ELSE 1 END END,
	        r.harness,
	        CASE (r.h / 100) % 10 WHEN 0 THEN 'subscription' WHEN 1 THEN 'unreported' ELSE 'metered' END,
	        CASE WHEN r.h % 100 < 25 THEN 'session_cumulative' ELSE 'per_leg' END
	 FROM (SELECT id, harness, (hashtext(id::text)::bigint & 2147483647) AS h
	       FROM runs WHERE kind <> 'chat' AND user_id IN (SELECT user_id FROM ev_owner)) r
	 CROSS JOIN generate_series(0, 1) s
	 CROSS JOIN generate_series(0, 1) m
	 CROSS JOIN generate_series(0, 2) e
	 WHERE r.h % 100 >= 10 AND e <= r.h % 3`,
	`DROP TABLE ev_gen`, `DROP TABLE ev_iss`, `DROP TABLE ev_repo`, `DROP TABLE ev_owner`,
	`ANALYZE users`, `ANALYZE forge_connections`, `ANALYZE repos`, `ANALYZE runs`, `ANALYZE run_usage`,
}

// evSeedQuality extends the #4684 seed for the #4730 Quality statements (milestone 3). It runs
// after evSeed on the same connection and never touches what evSeed generated except for one
// UPDATE that adds merge witnesses. Stated distributions (the seed logs the measured values):
//   - mr_rework runs: per PR (one row per (repo, mr_iid), owned by the PR's first run's owner)
//     0 reworks 55%, 1 reworks 25%, 2 reworks 12%, 3-5 reworks 8%; 12% of reworks are created
//     before the PR's first run (out of window for window-seeded PRs), 25% have trigger_source
//     manual, 7% point at a run of another PR (an unverified link);
//   - merge witnesses: 45% of the unmerged PR-carrying runs become merged (a majority of PRs
//     end up witnessed; the evSeed 40% of mr runs were already merged);
//   - plan cross-checks: 40% of delivery leads (issue, task, self_improve, prompt, ci_fix) carry
//     1 round (55%), 2 (30%) or 3 (15%) with the automatic round budget on; non-final rounds
//     are revise, the final round approve 60% / block 10% / failed 10% / revise 20%; 12% of the
//     final rounds of leads younger than 10 days are pending; 4% are interrupted; 10% of the
//     leads created 30-60 days ago are decided inside the last 30 days; plan_md, planning_diff
//     and findings are TOASTed text (8 KB, 6 KB on 30% and up to 12 findings of 288 bytes);
//   - code cross-checks: 35% of the leads carry exactly one code attempt: completed 72%, failed
//     20%, pending 8% (leads younger than 10 days only); 60% of the completed ones carry findings
//     (1-15 of ~400 bytes, 1.5% over 64 KB); guidance, repo-instruction and code_context texts are
//     TOASTed; 10% of the leads created 30-60 days ago are decided inside the last 30 days;
//   - run_reviews: 60% of the judge-eligible (issue, ci_fix) completed/failed runs, 15% of them
//     failed; judge_skips: another 5% of the eligible runs.
var evSeedQuality = []string{
	`SELECT setseed(0.4730)`,
	`CREATE TEMP TABLE ev_pr AS
	 SELECT x.*, lead(x.seed_id) OVER (ORDER BY x.repo_id, x.mr_iid) AS other_id,
	        (hashtext(x.seed_id::text || 'n')::bigint & 2147483647) % 1000 AS rk
	 FROM (SELECT DISTINCT ON (repo_id, mr_iid) repo_id, mr_iid, user_id, id AS seed_id, created_at AS first_at
	       FROM runs WHERE mr_iid IS NOT NULL AND repo_id IS NOT NULL AND kind <> 'mr_rework'
	       ORDER BY repo_id, mr_iid, created_at, id) x`,
	`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind, created_at,
	                   finished_at, status_since, harness, mr_iid, pipeline_ref, target_run_id, trigger_source)
	 SELECT gen_random_uuid(), p.user_id, p.repo_id, NULL, 't', 'd', 'completed', 'mr_rework', s.ts,
	        s.ts + interval '1 hour', s.ts + interval '1 hour', CASE WHEN s.h % 3 = 0 THEN 'codex' ELSE 'claude' END,
	        p.mr_iid, 'refs/mr/' || left(p.seed_id::text, 8) || '-' || k,
	        CASE WHEN s.h % 100 < 7 THEN COALESCE(p.other_id, p.seed_id) ELSE p.seed_id END,
	        CASE WHEN s.h % 100 >= 40 AND s.h % 100 < 65 THEN 'manual' ELSE 'mr_rework' END
	 FROM (SELECT q.*, CASE WHEN rk < 550 THEN 0 WHEN rk < 800 THEN 1 WHEN rk < 920 THEN 2 ELSE 3 + rk % 3 END AS n FROM ev_pr q) p
	 CROSS JOIN LATERAL generate_series(1, p.n) k
	 CROSS JOIN LATERAL (SELECT (hashtext(p.seed_id::text || '/' || k::text)::bigint & 2147483647) AS h) hh
	 CROSS JOIN LATERAL (SELECT hh.h,
	        LEAST(CASE WHEN hh.h % 100 < 12 THEN p.first_at - (1 + hh.h % 25) * interval '1 day'
	                   ELSE p.first_at + k * interval '6 hours' + (hh.h % 600) * interval '1 minute' END,
	              TIMESTAMPTZ '` + evNowRFC + `' - interval '2 hours') AS ts) s`,
	`DROP TABLE ev_pr`,
	`UPDATE runs SET mr_state = 'merged'
	 WHERE mr_iid IS NOT NULL AND kind <> 'mr_rework' AND mr_state IS DISTINCT FROM 'merged'
	   AND (hashtext(id::text || 'w')::bigint & 2147483647) % 100 < 45`,
	`VACUUM (ANALYZE) runs`,
	`CREATE TEMP TABLE ev_lead AS
	 SELECT r.id, r.created_at, r.harness,
	        (hashtext(r.id::text || 'p')::bigint & 2147483647) % 1000 AS hp,
	        (hashtext(r.id::text || 'c')::bigint & 2147483647) % 1000 AS hc,
	        (hashtext(r.id::text || 'r')::bigint & 2147483647) % 100 AS hr
	 FROM runs r WHERE r.kind IN ('issue', 'task', 'self_improve', 'prompt', 'ci_fix')`,
	`INSERT INTO cross_checks (lead_run_id, stage, round, lead_claim_generation, plan_md, milestones, size_class, base_commit,
	                          planning_diff, candidate_digest, checker_harness, verdict, reason_class, findings, decided_at,
	                          deadline_at, created_at, interrupted_at, automatic_rounds_enabled, automatic_revision_limit, wait_credited)
	 SELECT f.id, 'plan', f.k, 0, f.blob, '[{"id":"m1"}]'::jsonb, 'M', repeat('a', 40),
	        CASE WHEN f.h % 10 < 3 THEN left(f.blob, 6000) END, sha256(convert_to(f.id::text, 'UTF8')),
	        CASE WHEN f.h % 2 = 0 THEN 'claude' ELSE 'codex' END, f.v,
	        CASE f.v WHEN 'pending' THEN NULL WHEN 'failed' THEN (ARRAY['model_error', 'timed_out', 'superseded', 'checker_unavailable'])[1 + f.h % 4] ELSE f.v END,
	        CASE WHEN f.v IN ('pending', 'failed') THEN NULL
	             ELSE jsonb_build_object('items', COALESCE((SELECT jsonb_agg(jsonb_build_object('id', 'f' || g, 'severity', 'medium', 'text', repeat(md5(g::text || f.h::text), 9)))
	                                                         FROM generate_series(1, CASE WHEN f.v = 'approve' THEN f.h % 3 ELSE 1 + f.h % 12 END) g), '[]'::jsonb)) END,
	        CASE WHEN f.v = 'pending' THEN NULL ELSE f.decided END,
	        f.created + interval '1 hour', f.created,
	        CASE WHEN f.v <> 'pending' AND f.h % 100 >= 96 THEN f.created + interval '1 minute' END,
	        true, 2, true
	 FROM (SELECT g.*, GREATEST(g.created0, LEAST(g.decided0, TIMESTAMPTZ '` + evNowRFC + `' - interval '1 second')) AS decided,
	              LEAST(g.created0, TIMESTAMPTZ '` + evNowRFC + `' - interval '1 minute') AS created
	       FROM (SELECT l.id, k, n, l.hr, h, b.blob,
	                    l.created_at + k * interval '40 minutes' AS created0,
	                    CASE WHEN l.created_at < TIMESTAMPTZ '` + evNowRFC + `' - interval '30 days'
	                          AND l.created_at >= TIMESTAMPTZ '` + evNowRFC + `' - interval '60 days' AND h % 100 < 10
	                         THEN TIMESTAMPTZ '` + evNowRFC + `' - (h % 43200) * interval '1 minute'
	                         ELSE l.created_at + k * interval '40 minutes' + (3 + h % 40) * interval '1 minute' END AS decided0,
	                    CASE WHEN k = n AND l.created_at > TIMESTAMPTZ '` + evNowRFC + `' - interval '10 days' AND h % 100 < 12 THEN 'pending'
	                         WHEN k < n THEN 'revise'
	                         WHEN h % 100 < 60 THEN 'approve' WHEN h % 100 < 70 THEN 'block' WHEN h % 100 < 80 THEN 'failed' ELSE 'revise' END AS v
	             FROM (SELECT x.*, CASE WHEN hr < 55 THEN 1 WHEN hr < 85 THEN 2 ELSE 3 END AS n FROM ev_lead x WHERE hp < 400) l
	             CROSS JOIN LATERAL generate_series(1, l.n) k
	             CROSS JOIN LATERAL (SELECT (hashtext(l.id::text || '/' || k::text)::bigint & 2147483647) AS h) hh
	             CROSS JOIN LATERAL (SELECT string_agg(md5(l.id::text || g::text), '') AS blob FROM generate_series(1, 250) g) b) g) f`,
	`INSERT INTO cross_checks (lead_run_id, stage, round, lead_claim_generation, base_commit, head_commit, candidate_digest, verdict,
	                          outcome, reason_class, findings, decided_at, deadline_at, created_at, interrupted_at, checker_harness,
	                          code_context, guidance_snapshot, guidance_text, guidance_digest, repo_instructions_enabled,
	                          repo_instructions_text, repo_instructions_digest, wait_credited)
	 SELECT f.id, 'code', 1, 0, substr(md5(f.id::text) || md5(f.id::text || 'x'), 1, 40), substr(md5(f.id::text || 'h') || md5(f.id::text || 'y'), 1, 40),
	        sha256(convert_to(f.id::text, 'UTF8')), 'failed', f.o,
	        CASE WHEN f.o = 'failed' THEN (ARRAY['model_error', 'timed_out', 'superseded', 'malformed'])[1 + f.h % 4] END,
	        CASE WHEN f.o <> 'completed' OR f.h % 100 < 40 THEN CASE WHEN f.o = 'completed' THEN '[]'::jsonb END
	             ELSE (SELECT jsonb_agg(jsonb_build_object('id', 'f' || g, 'severity', 'medium', 'text', repeat(md5(g::text || f.h::text), 12)))
	                   FROM generate_series(1, CASE WHEN f.h % 1000 < 15 THEN 200 ELSE 1 + f.h % 15 END) g) END,
	        CASE WHEN f.o = 'pending' THEN NULL ELSE f.decided END, f.created + interval '1 hour', f.created,
	        CASE WHEN f.o <> 'pending' AND f.h % 100 >= 97 THEN f.created + interval '1 minute' END,
	        CASE WHEN f.h % 100 < 3 THEN NULL WHEN f.harness = 'claude' THEN 'codex' ELSE 'claude' END,
	        jsonb_build_object('issue_title', 't', 'issue_description', left(f.blob, 3000)),
	        CASE WHEN f.h % 2 = 0 THEN left(f.blob, 3000) ELSE '' END, CASE WHEN f.h % 2 = 0 THEN left(f.blob, 3000) ELSE '' END,
	        sha256(convert_to(CASE WHEN f.h % 2 = 0 THEN left(f.blob, 3000) ELSE '' END, 'UTF8')),
	        f.h % 3 = 0, CASE WHEN f.h % 3 = 0 THEN left(f.blob, 2500) ELSE '' END,
	        sha256(convert_to(CASE WHEN f.h % 3 = 0 THEN left(f.blob, 2500) ELSE '' END, 'UTF8')), true
	 FROM (SELECT g.*, GREATEST(g.created, LEAST(g.decided0, TIMESTAMPTZ '` + evNowRFC + `' - interval '1 second')) AS decided
	       FROM (SELECT l.id, l.harness, h, b.blob,
	                    LEAST(l.created_at + (20 + h % 90) * interval '1 minute', TIMESTAMPTZ '` + evNowRFC + `' - interval '1 minute') AS created,
	                    CASE WHEN l.created_at < TIMESTAMPTZ '` + evNowRFC + `' - interval '30 days'
	                          AND l.created_at >= TIMESTAMPTZ '` + evNowRFC + `' - interval '60 days' AND h % 100 < 10
	                         THEN TIMESTAMPTZ '` + evNowRFC + `' - (h % 43200) * interval '1 minute'
	                         ELSE l.created_at + (30 + h % 90) * interval '1 minute' END AS decided0,
	                    CASE WHEN l.created_at > TIMESTAMPTZ '` + evNowRFC + `' - interval '10 days' AND h % 100 < 8 THEN 'pending'
	                         WHEN h % 100 < 80 THEN 'completed' ELSE 'failed' END AS o
	             FROM ev_lead l
	             CROSS JOIN LATERAL (SELECT (hashtext(l.id::text || '/code')::bigint & 2147483647) AS h) hh
	             CROSS JOIN LATERAL (SELECT string_agg(md5(l.id::text || g::text), '') AS blob FROM generate_series(1, 100) g) b
	             WHERE l.hc < 350) g) f`,
	`DROP TABLE ev_lead`,
	`INSERT INTO run_reviews (target_run_id, user_id, verdict, status, created_at, updated_at)
	 SELECT r.id, r.user_id,
	        CASE WHEN h % 100 < 30 THEN 'ideal' WHEN h % 100 < 75 THEN 'ok' ELSE 'issues' END,
	        CASE WHEN (h / 100) % 100 < 15 THEN 'failed' ELSE 'complete' END,
	        COALESCE(r.finished_at, r.created_at) + interval '5 minutes', COALESCE(r.finished_at, r.created_at) + interval '5 minutes'
	 FROM (SELECT r.*, (hashtext(r.id::text || 'v')::bigint & 2147483647) AS h FROM runs r
	       WHERE r.kind IN ('issue', 'ci_fix') AND r.status IN ('completed', 'failed')) r
	 WHERE r.h % 100 < 60`,
	`INSERT INTO judge_skips (target_run_id, harness, reason)
	 SELECT r.id, 'claude', 'no_usable_credential'
	 FROM (SELECT r.id, (hashtext(r.id::text || 'v')::bigint & 2147483647) AS h FROM runs r
	       WHERE r.kind IN ('issue', 'ci_fix') AND r.status IN ('completed', 'failed')) r
	 WHERE r.h % 100 >= 95`,
	`ANALYZE cross_checks`, `ANALYZE run_reviews`, `ANALYZE judge_skips`, `ANALYZE runs`,
}

type evStmt struct{ name, sql, args string }

type evWorkload struct {
	name  string
	stmts []evStmt
	run   func(ctx context.Context, q *store.Queries) error
	spend bool
	// rawRun, when set, replaces run: it executes a standalone SQL text on the timed connection
	// (the latest-failure row lookup and count parts, which have no sqlc method of their own).
	rawRun func(ctx context.Context, c *pgxpool.Conn) error
	// idx marks the workloads re-measured with the runs_terminal_end_idx candidate.
	idx bool
}

type evHarness struct {
	t        *testing.T
	ctx      context.Context
	pool     *pgxpool.Pool
	q        *store.Queries
	conn     *pgxpool.Conn
	log      *os.File
	now      time.Time
	topOwner uuid.UUID
	runs     int
	modes    []string
	// jitOff mirrors the production snapshot (store.RunAnalyticsSnapshot sets jit = off) for the
	// Reliability phase; the Spend phase keeps the server default so its figures stay comparable.
	jitOff bool
	// Optional observers (nil for the Spend harness): onPlan sees every EXPLAIN measure takes,
	// onTiming every timing it records.
	onPlan   func(w evWorkload, mode string, st evStmt, p *evPlan)
	onTiming func(phase string, w evWorkload, mode string, tm evTiming)
}

func (h *evHarness) logf(format string, a ...any) {
	line := fmt.Sprintf(format, a...)
	if h.log != nil {
		_, _ = h.log.WriteString(line + "\n")
	}
	h.t.Log(line)
}

func (h *evHarness) simple(sql string) {
	h.t.Helper()
	if _, err := h.conn.Exec(h.ctx, sql, pgx.QueryExecModeSimpleProtocol); err != nil {
		h.t.Fatalf("exec %.200q: %v", sql, err)
	}
}

func (h *evHarness) scalar(sql string, dst ...any) {
	h.t.Helper()
	if err := h.conn.QueryRow(h.ctx, sql, pgx.QueryExecModeSimpleProtocol).Scan(dst...); err != nil {
		h.t.Fatalf("query %.200q: %v", sql, err)
	}
}

func evEnvInt(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil {
		return v
	}
	return def
}

// newEvidenceHarness gates on UZI_ANALYTICS_PLAN_EVIDENCE, proves the throwaway marker, migrates,
// and builds the harness (pool, pinned connection, now, run counts, modes, optional log). The
// Spend and Flow evidence tests share it.
func newEvidenceHarness(t *testing.T) *evHarness {
	t.Helper()
	if os.Getenv("UZI_ANALYTICS_PLAN_EVIDENCE") != "1" {
		t.Skip("UZI_ANALYTICS_PLAN_EVIDENCE=1 not set; this is a measurement harness, not a gate")
	}
	// Verify the throwaway marker BEFORE anything migrates, runs DDL or TRUNCATEs. A missing
	// marker fails (never skips): the operator asked for a destructive harness.
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Fatal("UZI_TEST_DATABASE_URL not set; the evidence harness needs a throwaway database")
	}
	pmRequireThrowawayDB(context.Background(), t, dsn)
	ctx, pool := openAnalyticsViewsDB(t)
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	t.Cleanup(conn.Release)
	now, err := time.Parse(time.RFC3339, evNowRFC)
	if err != nil {
		t.Fatalf("now: %v", err)
	}
	h := &evHarness{t: t, ctx: ctx, pool: pool, q: store.New(pool), conn: conn, now: now, runs: evEnvInt("UZI_EVIDENCE_RUNS", 20), modes: []string{"custom", "generic"}}
	if m := os.Getenv("UZI_EVIDENCE_MODES"); m != "" {
		h.modes = strings.Split(m, ",")
	}
	if p := os.Getenv("UZI_EVIDENCE_LOG"); p != "" {
		f, err := os.OpenFile(filepath.Clean(p), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // G304/G703: the operator-chosen log path of a measurement harness that never runs in CI
		if err != nil {
			t.Fatalf("open log: %v", err)
		}
		t.Cleanup(func() { _ = f.Close() })
		h.log = f
	}
	return h
}

func TestAnalyticsPlanEvidenceLiveDB(t *testing.T) {
	h := newEvidenceHarness(t)
	pool := h.pool
	h.simple(`SET statement_timeout = 0`)
	for _, ix := range evIndexes {
		h.simple("DROP INDEX IF EXISTS " + ix.name)
	}
	if os.Getenv("UZI_EVIDENCE_KEEP") != "1" {
		t.Cleanup(func() {
			for _, ix := range evIndexes {
				_, _ = pool.Exec(context.Background(), "DROP INDEX IF EXISTS "+ix.name)
			}
			_, _ = pool.Exec(context.Background(), "DROP INDEX IF EXISTS "+evTerminalIdxName)
			_, _ = pool.Exec(context.Background(), `TRUNCATE users CASCADE`)
		})
	}

	h.seed()
	spendOn := os.Getenv("UZI_EVIDENCE_SPEND") != "0"
	if spendOn && os.Getenv("UZI_EVIDENCE_RESEED") == "0" {
		// The seed carries no human-landable origin, so one means an earlier run's Reliability
		// landable-origin step already rewrote this retained seed.
		var rewritten int64
		h.scalar(`SELECT count(*) FROM runs WHERE fail_origin = ANY(`+evSQLTextArray(workersvc.AllHumanLandableFailOrigins())+`)`, &rewritten)
		if rewritten > 0 {
			t.Fatalf("the retained seed already ran the Reliability landable-origin step (%d runs carry a human-landable fail_origin), "+
				"so the Spend phase would measure rewritten rows; reseed (unset UZI_EVIDENCE_RESEED=0) or set UZI_EVIDENCE_SPEND=0", rewritten)
		}
	}
	var workloads []evWorkload
	if spendOn {
		workloads = h.workloads()
	}

	if spendOn && os.Getenv("UZI_EVIDENCE_BASELINE") != "0" {
		h.logf("\n######## BASELINE (no candidate index) ########")
		h.logDisk("baseline")
		for _, w := range workloads {
			h.measure("baseline", w, h.modes, true)
		}
	}
	list := "1,2,3,4"
	if v, ok := os.LookupEnv("UZI_EVIDENCE_INDEXES"); ok {
		list = v
	}
	if !spendOn {
		list = ""
	}
	for _, s := range strings.Split(list, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil || n < 1 || n > len(evIndexes) {
			continue
		}
		ix := evIndexes[n-1]
		h.logf("\n######## CANDIDATE %d: %s ########", n, ix.ddl)
		start := time.Now()
		h.simple(ix.ddl)
		h.simple(`ANALYZE runs`)
		h.simple(`ANALYZE cross_checks`)
		var size int64
		h.scalar(fmt.Sprintf("SELECT pg_relation_size('%s')", ix.name), &size)
		h.logf("CREATE INDEX + ANALYZE took %s; index size %d bytes", time.Since(start).Round(time.Millisecond), size)
		h.logDisk("with " + ix.name)
		for _, w := range workloads {
			h.measure("idx"+strconv.Itoa(n), w, []string{"custom"}, false, ix.name)
		}
		h.simple("DROP INDEX " + ix.name)
		h.simple(`ANALYZE runs`)
		h.simple(`ANALYZE cross_checks`)
	}

	// Issue #4685 (m3): the Reliability evidence runs AFTER every Spend workload, because the
	// landable-origin step rewrites rows the Spend measurements must see untouched.
	if os.Getenv("UZI_EVIDENCE_RELIABILITY") != "0" {
		h.reliabilityEvidence()
	}
}

var evIndexes = []struct{ name, ddl string }{
	{"evid_runs_created_idx", `CREATE INDEX evid_runs_created_idx ON runs (created_at)`},
	{"evid_runs_repo_mr_idx", `CREATE INDEX evid_runs_repo_mr_idx ON runs (repo_id, mr_iid) WHERE mr_iid IS NOT NULL`},
	{"evid_runs_repo_mr_merged_idx", `CREATE INDEX evid_runs_repo_mr_merged_idx ON runs (repo_id, mr_iid) WHERE mr_state = 'merged'`},
	{"evid_runs_user_repo_mr_merged_idx", `CREATE INDEX evid_runs_user_repo_mr_merged_idx ON runs (user_id, repo_id, mr_iid) WHERE mr_state = 'merged'`},
	// #4731 candidate: the terminal-end arm of the admin all-users Capacity statement (no user prefix).
	{"evid_runs_terminal_end_idx", `CREATE INDEX evid_runs_terminal_end_idx ON runs ((COALESCE(finished_at, status_since))) WHERE status IN ('completed', 'failed', 'cancelled')`},
	// Issue #4730 candidates (6 and 7): the rework join and the cross-check decision window.
	{"evid_runs_rework_repo_mr_idx", `CREATE INDEX evid_runs_rework_repo_mr_idx ON runs (repo_id, mr_iid) WHERE kind = 'mr_rework'`},
	{"evid_cc_decided_idx", `CREATE INDEX evid_cc_decided_idx ON cross_checks (decided_at)`},
}

func (h *evHarness) seed() {
	h.t.Helper()
	if os.Getenv("UZI_EVIDENCE_RESEED") != "0" {
		start := time.Now()
		for _, s := range evSeed {
			h.simple(s)
		}
		h.seedCapacity()
		h.logf("seed (Spend base and Capacity) took %s", time.Since(start).Round(time.Second))
		start = time.Now()
		for _, s := range evSeedQuality {
			h.simple(s)
		}
		h.logf("seed (Quality extension) took %s", time.Since(start).Round(time.Second))
	}
	var nRuns, nUsage, nRepos, nUsers, nRunRepos int64
	h.scalar(`SELECT (SELECT count(*) FROM runs), (SELECT count(*) FROM run_usage), (SELECT count(*) FROM repos),
	                 (SELECT count(*) FROM users WHERE email LIKE 'evid-%'), (SELECT count(DISTINCT repo_id) FROM runs)`,
		&nRuns, &nUsage, &nRepos, &nUsers, &nRunRepos)
	if nRuns < 100000 {
		h.t.Fatalf("seed has %d runs, need >= 100000", nRuns)
	}
	h.scalar(`SELECT id FROM users WHERE email = 'evid-0001@e2e'`, &h.topOwner)
	h.logf("SEED SHAPE: runs=%d run_usage=%d repos=%d users=%d distinct run repos=%d", nRuns, nUsage, nRepos, nUsers, nRunRepos)
	h.logf("%s", h.shape(`SELECT 'owner1 share', round(100.0 * count(*) FILTER (WHERE user_id = (SELECT id FROM users WHERE email = 'evid-0001@e2e')) / count(*), 1) FROM runs`))
	h.logf("%s", h.shape(`SELECT 'top10 owners share', round(100.0 * sum(c) FILTER (WHERE rk <= 10) / sum(c), 1)
	                       FROM (SELECT count(*) AS c, row_number() OVER (ORDER BY count(*) DESC) AS rk FROM runs GROUP BY user_id) x`))
	h.logf("%s", h.shape(`SELECT 'last 30d share', round(100.0 * count(*) FILTER (WHERE created_at >= TIMESTAMPTZ '`+evNowRFC+`' - interval '30 days') / count(*), 1) FROM runs`))
	h.logf("%s", h.shape(`SELECT 'last 90d share', round(100.0 * count(*) FILTER (WHERE created_at >= TIMESTAMPTZ '`+evNowRFC+`' - interval '90 days') / count(*), 1) FROM runs`))
	h.logf("%s", h.shape(`SELECT 'mr_iid share', round(100.0 * count(mr_iid) / count(*), 1) FROM runs`))
	h.logf("%s", h.shape(`SELECT 'merged share of mr', round(100.0 * count(*) FILTER (WHERE mr_state = 'merged') / nullif(count(mr_iid), 0), 1) FROM runs`))
	h.logf("%s", h.shape(`SELECT 'chat share', round(100.0 * count(*) FILTER (WHERE kind = 'chat') / count(*), 1) FROM runs`))
	h.logf("%s", h.shape(`SELECT 'delivery share', round(100.0 * count(*) FILTER (WHERE kind IN ('issue','mr_rework','ci_fix','prompt','self_improve','task','job')) / count(*), 1) FROM runs`))
	h.logf("%s", h.shape(`SELECT 'status mix', string_agg(status || '=' || c, ' ' ORDER BY status) FROM (SELECT status, count(*) c FROM runs GROUP BY status) x`))
	h.logf("%s", h.shape(`SELECT 'kind mix', string_agg(kind || '=' || c, ' ' ORDER BY kind) FROM (SELECT kind, count(*) c FROM runs GROUP BY kind) x`))
	h.logf("%s", h.shape(`SELECT 'merged witness distinct (repo,mr)', count(*) FROM (SELECT DISTINCT repo_id, mr_iid FROM runs WHERE mr_state = 'merged' AND mr_iid IS NOT NULL AND repo_id IS NOT NULL) x`))
	h.logf("%s", h.shape(`SELECT 'usage legs/run (with usage)', round(count(*)::numeric / count(DISTINCT run_id), 2) FROM run_usage`))
	h.logf("%s", h.shape(`SELECT 'runs without usage (non-chat)', count(*) FROM runs r WHERE kind <> 'chat' AND NOT EXISTS (SELECT 1 FROM run_usage u WHERE u.run_id = r.id)`))
	h.logf("%s", h.shape(`SELECT 'session_cumulative legs', count(*) FROM run_usage WHERE usage_basis = 'session_cumulative'`))
	h.logCapacityShape()
	h.qualityShape()
}

// qualityShape logs the measured #4730 seed: row counts, the 30d range selectivity, the fan-out
// of reworks per PR, plan attempts per lead and reviews per eligible target, and the detoast
// weight of cross_checks.
func (h *evHarness) qualityShape() {
	h.t.Helper()
	// The window the 30d workloads run on: ComputeBounds' UTC midnight 29 calendar days before
	// today through now, not now minus 30 days.
	b30, err := analytics.ComputeBounds(h.now, time.UTC, analytics.Window30d, nil)
	if err != nil {
		h.t.Fatalf("ComputeBounds 30d: %v", err)
	}
	win := `TIMESTAMPTZ '` + b30.From.UTC().Format(time.RFC3339Nano) + `'`
	upper := `TIMESTAMPTZ '` + b30.Now.UTC().Format(time.RFC3339Nano) + `'`
	h.logf("QUALITY SEED SHAPE (30d window %s to %s)", b30.From.UTC().Format(time.RFC3339), b30.Now.UTC().Format(time.RFC3339))
	h.logf("%s", h.shape(`SELECT 'row counts', 'runs=' || (SELECT count(*) FROM runs) || ' mr_rework=' || (SELECT count(*) FROM runs WHERE kind = 'mr_rework')
	    || ' cross_checks=' || (SELECT count(*) FROM cross_checks) || ' (plan ' || (SELECT count(*) FROM cross_checks WHERE stage = 'plan')
	    || ', code ' || (SELECT count(*) FROM cross_checks WHERE stage = 'code') || ') run_reviews=' || (SELECT count(*) FROM run_reviews)
	    || ' judge_skips=' || (SELECT count(*) FROM judge_skips)`))
	h.logf("%s", h.shape(`SELECT 'size', 'runs=' || pg_size_pretty(pg_total_relation_size('runs')) || ' cross_checks=' || pg_size_pretty(pg_total_relation_size('cross_checks'))
	    || ' (toast ' || pg_size_pretty(pg_total_relation_size(reltoastrelid)) || ') run_reviews=' || pg_size_pretty(pg_total_relation_size('run_reviews'))
	    FROM pg_class WHERE oid = 'cross_checks'::regclass`))
	for _, scope := range []struct{ label, pred string }{
		{"overall", "true"},
		{"largest owner", "user_id = (SELECT id FROM users WHERE email = 'evid-0001@e2e')"},
	} {
		h.logf("%s", h.shape(`SELECT '30d range selectivity of runs (`+scope.label+`)', round(100.0 * count(*) FILTER (WHERE created_at >= `+win+` AND created_at < `+upper+`) / count(*), 2) || '% of ' || count(*) || ' runs ('
	        || count(*) FILTER (WHERE created_at >= `+win+`) || ' in window)' FROM runs WHERE `+scope.pred))
		h.logf("%s", h.shape(`SELECT 'rw_seed cohort 30d (`+scope.label+`)', count(*) || ' PRs from ' || sum(c) || ' delivery runs' FROM
	        (SELECT repo_id, mr_iid, count(*) AS c FROM runs WHERE created_at >= `+win+` AND created_at < `+upper+`
	         AND kind = ANY(ARRAY['issue','mr_rework','ci_fix','prompt','self_improve','task','job']) AND mr_iid IS NOT NULL AND repo_id IS NOT NULL AND `+scope.pred+`
	         GROUP BY repo_id, mr_iid) x`))
	}
	h.logf("%s", h.shape(`SELECT 'PRs (distinct repo,mr) / witnessed share', count(*) || ' / ' || round(100.0 * count(*) FILTER (WHERE w) / count(*), 1) || '%' FROM
	    (SELECT repo_id, mr_iid, bool_or(mr_state = 'merged') AS w FROM runs WHERE mr_iid IS NOT NULL AND repo_id IS NOT NULL GROUP BY repo_id, mr_iid) x`))
	h.logf("%s", h.shape(`SELECT 'reworks per PR (0 / 1 / 2 / 3+)', string_agg(g || '=' || c, ' ' ORDER BY g) FROM
	    (SELECT CASE WHEN n >= 3 THEN '3+' ELSE n::text END AS g, count(*) AS c FROM
	       (SELECT p.repo_id, p.mr_iid, count(w.id) AS n FROM (SELECT DISTINCT repo_id, mr_iid FROM runs WHERE mr_iid IS NOT NULL AND repo_id IS NOT NULL AND kind <> 'mr_rework') p
	        LEFT JOIN runs w ON w.kind = 'mr_rework' AND w.repo_id = p.repo_id AND w.mr_iid = p.mr_iid GROUP BY p.repo_id, p.mr_iid) y GROUP BY 1) x`))
	h.logf("%s", h.shape(`SELECT 'reworks: manual / created before the 30d window / unverified link', round(100.0 * count(*) FILTER (WHERE r.trigger_source = 'manual') / count(*), 1) || '% / '
	    || round(100.0 * count(*) FILTER (WHERE r.created_at < `+win+`) / count(*), 1) || '% / '
	    || round(100.0 * count(*) FILTER (WHERE t.repo_id IS DISTINCT FROM r.repo_id OR t.mr_iid IS DISTINCT FROM r.mr_iid OR t.user_id IS DISTINCT FROM r.user_id) / count(*), 1) || '%'
	    FROM runs r LEFT JOIN runs t ON t.id = r.target_run_id WHERE r.kind = 'mr_rework'`))
	h.logf("%s", h.shape(`SELECT 'plan attempts per lead (1 / 2 / 3 rounds), leads with plan checks', string_agg(n || '=' || c, ' ' ORDER BY n) || ' of ' || (SELECT count(*) FROM runs WHERE kind IN ('issue','task','self_improve','prompt','ci_fix')) || ' delivery leads' FROM
	    (SELECT n, count(*) AS c FROM (SELECT lead_run_id, count(*) AS n FROM cross_checks WHERE stage = 'plan' GROUP BY lead_run_id) y GROUP BY n) x`))
	h.logf("%s", h.shape(`SELECT 'plan final verdict mix', string_agg(verdict || '=' || c, ' ' ORDER BY verdict) FROM (SELECT verdict, count(*) c FROM cross_checks WHERE stage = 'plan' GROUP BY 1) x`))
	h.logf("%s", h.shape(`SELECT 'code attempts per lead / outcome mix', max(n) || ' max; ' || string_agg(outcome || '=' || c, ' ' ORDER BY outcome) FROM
	    (SELECT outcome, count(*) AS c, 1 AS n FROM cross_checks WHERE stage = 'code' GROUP BY 1) x`))
	h.logf("%s", h.shape(`SELECT 'cross_checks decided in 30d window (plan / code), of which lead created before window', count(*) FILTER (WHERE cc.stage = 'plan') || ' / ' || count(*) FILTER (WHERE cc.stage = 'code')
	    || ', older leads ' || count(*) FILTER (WHERE l.created_at < `+win+`)
	    FROM cross_checks cc JOIN runs l ON l.id = cc.lead_run_id WHERE cc.decided_at >= `+win+` AND cc.decided_at < `+upper+``))
	h.logf("%s", h.shape(`SELECT 'pending attempts (plan / code)', count(*) FILTER (WHERE stage = 'plan' AND verdict = 'pending') || ' / ' || count(*) FILTER (WHERE stage = 'code' AND outcome = 'pending') FROM cross_checks`))
	h.logf("%s", h.shape(`SELECT 'judge-eligible targets / reviewed / failed reviews / skips', t.n || ' / ' || t.rev || ' (' || round(100.0 * t.rev / t.n, 1) || '%) / ' || t.fl || ' / ' || t.sk FROM
	    (SELECT count(*) AS n, count(rv.id) AS rev, count(rv.id) FILTER (WHERE rv.status = 'failed') AS fl, count(js.target_run_id) AS sk
	     FROM runs r LEFT JOIN run_reviews rv ON rv.target_run_id = r.id LEFT JOIN judge_skips js ON js.target_run_id = r.id
	     WHERE r.kind IN ('issue', 'ci_fix') AND r.status IN ('completed', 'failed')) t`))
	h.logf("%s", h.shape(`SELECT 'reviews per eligible target (max)', max(c) FROM (SELECT count(*) AS c FROM run_reviews GROUP BY target_run_id) x`))
}

func (h *evHarness) shape(sql string) string {
	var label, val string
	h.scalar("SELECT a::text, b::text FROM ("+sql+") x(a, b)", &label, &val)
	return fmt.Sprintf("  %s: %s", label, val)
}

func (h *evHarness) logDisk(label string) {
	for _, rel := range []string{"runs", "run_usage"} {
		var rs, is, ts int64
		h.scalar(fmt.Sprintf("SELECT pg_relation_size('%[1]s'), pg_indexes_size('%[1]s'), pg_total_relation_size('%[1]s')", rel), &rs, &is, &ts)
		h.logf("DISK [%s] %-9s relation=%d indexes=%d total=%d bytes", label, rel, rs, is, ts)
	}
}

// ---- workloads ----

// evSpendOpts are the optional filters of a spend workload. The zero value reproduces the
// unfiltered call: every kind, no harness filter, repo names hidden (the owner route).
type evSpendOpts struct {
	harness     string   // "" means no harness filter
	kinds       []string // nil means runkind.All()
	revealRepos bool     // the admin route sets this on the owner-scoped statement
}

func (h *evHarness) spendWorkload(name string, owner uuid.UUID, win analytics.Window, split, breakdown string) evWorkload {
	h.t.Helper()
	return h.spendWorkloadWith(name, owner, win, split, breakdown, evSpendOpts{})
}

func (h *evHarness) spendWorkloadWith(name string, owner uuid.UUID, win analytics.Window, split, breakdown string, opts evSpendOpts) evWorkload {
	h.t.Helper()
	kinds := opts.kinds
	if kinds == nil {
		kinds = runkind.All()
	}
	var harness pgtype.Text
	if opts.harness != "" {
		harness = pgtype.Text{String: opts.harness, Valid: true}
	}
	var hist pgtype.Timestamptz
	var err error
	if owner != uuid.Nil {
		hist, err = h.q.AnalyticsOwnerHistoryStart(h.ctx, store.AnalyticsOwnerHistoryStartParams{UserID: owner, Harness: harness, Kinds: kinds})
	} else {
		hist, err = h.q.AnalyticsAllHistoryStart(h.ctx, store.AnalyticsAllHistoryStartParams{Harness: harness, Kinds: kinds})
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
	var prev pgtype.Timestamptz
	if b.PreviousFrom != nil {
		prev = spendTS(*b.PreviousFrom)
	}
	ownerP := store.AnalyticsOwnerSpendParams{
		UserID: owner, RevealRepos: opts.revealRepos, Harness: harness,
		Now: spendTS(b.Now), From: spendTS(b.From), PreviousFrom: prev, FoldFrom: spendTS(b.FoldFrom),
		BucketStarts: spendTSs(b.BucketStarts), BucketEnds: spendTSs(b.BucketEnds),
		RollStarts: spendTSs(b.RollStarts), RollEnds: spendTSs(b.RollEnds),
		Kinds: kinds, DeliveryKinds: runkind.Delivery(), Split: split, Breakdown: breakdown,
	}
	allP := store.AnalyticsAllSpendParams{
		Harness: harness,
		Now:     spendTS(b.Now), From: spendTS(b.From), PreviousFrom: prev, FoldFrom: spendTS(b.FoldFrom),
		BucketStarts: spendTSs(b.BucketStarts), BucketEnds: spendTSs(b.BucketEnds),
		RollStarts: spendTSs(b.RollStarts), RollEnds: spendTSs(b.RollEnds),
		Kinds: kinds, DeliveryKinds: runkind.Delivery(), Split: split, Breakdown: breakdown,
	}
	capture := &capturingDBTX{}
	cq := store.New(capture)
	w := evWorkload{name: name, spend: true}
	if owner != uuid.Nil {
		_, _ = cq.AnalyticsOwnerSpend(h.ctx, ownerP)
		w.stmts = []evStmt{{name: "AnalyticsOwnerSpend", sql: store.AnalyticsOwnerSpendSQL}}
		w.run = func(ctx context.Context, q *store.Queries) error {
			_, err := q.AnalyticsOwnerSpend(ctx, ownerP)
			return err
		}
	} else {
		_, _ = cq.AnalyticsAllSpend(h.ctx, allP)
		w.stmts = []evStmt{{name: "AnalyticsAllSpend", sql: store.AnalyticsAllSpendSQL}}
		w.run = func(ctx context.Context, q *store.Queries) error {
			_, err := q.AnalyticsAllSpend(ctx, allP)
			return err
		}
	}
	lits := make([]string, len(capture.args))
	for i, a := range capture.args {
		lits[i] = sqlLiteral(h.t, a)
	}
	w.stmts[0].args = strings.Join(lits, ", ")
	h.logf("WORKLOAD %s: window=%s buckets=%d rolling=%d from=%s fold_from=%s split=%s breakdown=%s",
		name, win, len(b.BucketStarts), len(b.RollStarts), b.From.Format(time.RFC3339), b.FoldFrom.Format(time.RFC3339), split, breakdown)
	return w
}

// qualityWorkload is the #4730 Quality statement for owner (owner-shaped, which is also what the
// admin route runs for a selected user) or, for owner == uuid.Nil, the all-scope statement, with
// every kind and no repo or harness filter, exactly as the handler builds it.
func (h *evHarness) qualityWorkload(name string, owner uuid.UUID, win analytics.Window) evWorkload {
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
	var judge []string
	for _, k := range runkind.All() {
		if runkind.JudgeEligible(k) {
			judge = append(judge, k)
		}
	}
	planLead, codeLead := qualityCall{}.leadKinds()
	always, prestart := workersvc.JudgeSkipFailOrigins()
	ownerP := store.AnalyticsOwnerQualityParams{
		UserID: owner, Now: spendTS(b.Now), From: spendTS(b.From), BucketStarts: spendTSs(b.BucketStarts), BucketEnds: spendTSs(b.BucketEnds),
		Kinds: kinds, DeliveryKinds: runkind.Delivery(), AlwaysSkipOrigins: always, PrestartOrigins: prestart, JudgeKinds: judge,
		PlanLeadKinds: planLead, CodeLeadKinds: codeLead,
	}
	allP := store.AnalyticsAllQualityParams{
		Now: spendTS(b.Now), From: spendTS(b.From), BucketStarts: spendTSs(b.BucketStarts), BucketEnds: spendTSs(b.BucketEnds),
		Kinds: kinds, DeliveryKinds: runkind.Delivery(), AlwaysSkipOrigins: always, PrestartOrigins: prestart, JudgeKinds: judge,
		PlanLeadKinds: planLead, CodeLeadKinds: codeLead,
	}
	w := evWorkload{name: name}
	if owner != uuid.Nil {
		w.stmts = []evStmt{{name: "AnalyticsOwnerQuality", sql: store.AnalyticsOwnerQualitySQL}}
		w.run = func(ctx context.Context, q *store.Queries) error {
			_, err := q.AnalyticsOwnerQuality(ctx, ownerP)
			return err
		}
	} else {
		w.stmts = []evStmt{{name: "AnalyticsAllQuality", sql: store.AnalyticsAllQualitySQL}}
		w.run = func(ctx context.Context, q *store.Queries) error {
			_, err := q.AnalyticsAllQuality(ctx, allP)
			return err
		}
	}
	w.stmts[0].args = qualityExecuteArgs(h.t, h.ctx, owner, b)
	h.logf("WORKLOAD %s: window=%s buckets=%d from=%s now=%s", name, win, len(b.BucketStarts), b.From.Format(time.RFC3339), b.Now.Format(time.RFC3339))
	return w
}

const (
	evSQLOwnerRepoOptions = `SELECT r.id, r.path_with_namespace FROM repos r JOIN forge_connections c ON c.id = r.connection_id
		WHERE c.user_id = $1::uuid ORDER BY r.path_with_namespace COLLATE "C", r.id LIMIT 201`
	evSQLAllRepoOptions = `SELECT r.id, r.path_with_namespace FROM repos r ORDER BY r.path_with_namespace COLLATE "C", r.id LIMIT 201`
	evSQLUserOptions    = `SELECT u.id, u.email FROM users u ORDER BY u.email COLLATE "C", u.id LIMIT 201`
	evSQLOwnerHistory   = `SELECT MIN(r.created_at)::timestamptz FROM runs r WHERE r.user_id = $1::uuid
		AND ($2::uuid IS NULL OR r.repo_id = $2::uuid) AND ($3::text IS NULL OR r.harness = $3::text) AND r.kind = ANY($4::text[])`
	evSQLAllHistory = `SELECT MIN(r.created_at)::timestamptz FROM runs r
		WHERE ($1::uuid IS NULL OR r.repo_id = $1::uuid) AND ($2::text IS NULL OR r.harness = $2::text) AND r.kind = ANY($3::text[])`
)

func (h *evHarness) optionWorkloads() []evWorkload {
	kinds := sqlLiteral(h.t, runkind.All())
	owner := sqlLiteral(h.t, h.topOwner)
	return []evWorkload{
		{
			name: "owner options",
			stmts: []evStmt{
				{"AnalyticsOwnerRepoOptions", evSQLOwnerRepoOptions, owner},
				{"AnalyticsOwnerHistoryStart", evSQLOwnerHistory, owner + ", NULL::uuid, NULL::text, " + kinds},
			},
			run: func(ctx context.Context, q *store.Queries) error {
				if _, err := q.AnalyticsOwnerRepoOptions(ctx, h.topOwner); err != nil {
					return err
				}
				_, err := q.AnalyticsOwnerHistoryStart(ctx, store.AnalyticsOwnerHistoryStartParams{UserID: h.topOwner, Kinds: runkind.All()})
				return err
			},
		},
		{
			name: "admin options",
			stmts: []evStmt{
				{"AnalyticsAllRepoOptions", evSQLAllRepoOptions, ""},
				{"AnalyticsUserOptions", evSQLUserOptions, ""},
				{"AnalyticsAllHistoryStart", evSQLAllHistory, "NULL::uuid, NULL::text, " + kinds},
			},
			run: func(ctx context.Context, q *store.Queries) error {
				if _, err := q.AnalyticsAllRepoOptions(ctx); err != nil {
					return err
				}
				if _, err := q.AnalyticsUserOptions(ctx); err != nil {
					return err
				}
				_, err := q.AnalyticsAllHistoryStart(ctx, store.AnalyticsAllHistoryStartParams{Kinds: runkind.All()})
				return err
			},
		},
	}
}

func (h *evHarness) workloads() []evWorkload {
	all := []evWorkload{
		h.spendWorkload("owner-30d", h.topOwner, analytics.Window30d, "harness", "kind"),
		h.spendWorkload("owner-90d", h.topOwner, analytics.Window90d, "harness", "kind"),
		h.spendWorkload("admin-all-30d", uuid.Nil, analytics.Window30d, "harness", "kind"),
		h.spendWorkload("admin-all-window-all", uuid.Nil, analytics.WindowAll, "harness", "kind"),
		h.spendWorkload("admin-30d-user-user", uuid.Nil, analytics.Window30d, "user", "user"),
		h.spendWorkload("admin-all-window-user-user", uuid.Nil, analytics.WindowAll, "user", "user"),
		h.spendWorkload("admin-30d-repo-repo", uuid.Nil, analytics.Window30d, "repo", "repo"),
		h.spendWorkload("admin-all-window-repo-repo", uuid.Nil, analytics.WindowAll, "repo", "repo"),
		h.spendWorkload("owner-window-all", h.topOwner, analytics.WindowAll, "harness", "kind"),
		h.spendWorkloadWith("owner-30d-harness-kind", h.topOwner, analytics.Window30d, "harness", "kind",
			evSpendOpts{harness: "claude", kinds: []string{runkind.Issue}}),
		h.spendWorkloadWith("admin-user-30d-harness-kind", h.topOwner, analytics.Window30d, "harness", "kind",
			evSpendOpts{harness: "claude", kinds: []string{runkind.Issue}, revealRepos: true}),
	}
	// Issue #4730: the Quality statements. The admin route's user=<uuid> request runs the same
	// owner-shaped statement as the owner route; it is listed under its own name so the table
	// carries the four scopes the HTTP evidence covers.
	all = append(all,
		h.qualityWorkload("quality-owner-30d", h.topOwner, analytics.Window30d),
		h.qualityWorkload("quality-owner-window-all", h.topOwner, analytics.WindowAll),
		h.qualityWorkload("quality-admin-all-users-30d", uuid.Nil, analytics.Window30d),
		h.qualityWorkload("quality-admin-user-30d", h.topOwner, analytics.Window30d),
		h.qualityWorkload("quality-admin-all-users-window-all", uuid.Nil, analytics.WindowAll),
	)
	all = append(all, h.optionWorkloads()...)
	all = append(all, h.capacityWorkloads()...)
	return evOnly(all)
}

// evOnly applies UZI_EVIDENCE_ONLY (comma list of workload-name substrings).
func evOnly(all []evWorkload) []evWorkload {
	only := os.Getenv("UZI_EVIDENCE_ONLY")
	if only == "" {
		return all
	}
	var out []evWorkload
	for _, w := range all {
		if evNameSelected(w.name) {
			out = append(out, w)
		}
	}
	return out
}

func evNameSelected(name string) bool {
	only := os.Getenv("UZI_EVIDENCE_ONLY")
	if only == "" {
		return true
	}
	for _, f := range strings.Split(only, ",") {
		if f != "" && strings.Contains(name, f) {
			return true
		}
	}
	return false
}

// ---- measurement ----

type evTiming struct {
	median, p95, max time.Duration
	n                int
	reduced          bool
}

func (h *evHarness) measure(phase string, w evWorkload, modes []string, fullPlans bool, idxNames ...string) {
	h.t.Helper()
	phase = os.Getenv("UZI_EVIDENCE_LABEL") + phase
	h.logf("\n==== [%s] %s ====", phase, w.name)
	for _, mode := range []string{"custom", "generic"} {
		for _, st := range w.stmts {
			p := h.explain(mode, st)
			if h.onPlan != nil {
				h.onPlan(w, mode, st, p)
			}
			h.logf("-- EXPLAIN %s plan_cache_mode=%s (generic_plans=%d custom_plans=%d): exec=%.1fms planning=%.1fms temp_written_blocks=%d temp_read_blocks=%d disk_sorts=%d jit_ms=%.1f jit_funcs=%.0f shared_hit=%d shared_read=%d",
				st.name, mode, p.generic, p.custom, p.execMs, p.planMs, p.tempW, p.tempR, p.sortDisk, p.jitMs, p.jitFuncs, p.sharedHit, p.sharedRead)
			for _, n := range idxNames {
				h.logf("   uses %s: %v", n, p.usesIndex(n))
			}
			h.logf("   runs_pkey probes (loops): %.0f", p.loopsOf("Index Scan using runs_pkey on runs")+p.loopsOf("Index Only Scan using runs_pkey on runs"))
			h.logf("   scans:")
			for _, l := range p.scanLines() {
				h.logf("     %s", l)
			}
			for _, l := range p.nlLines {
				h.logf("   NL %s", l)
			}
			if fullPlans {
				h.logf("   plan (interesting nodes):")
				for _, l := range p.lines {
					h.logf("     %s", l)
				}
			}
		}
	}
	for _, mode := range modes {
		tm := h.timeWorkload(w, mode)
		if h.onTiming != nil {
			h.onTiming(phase, w, mode, tm)
		}
		h.logf("TIMING %s %s mode=%s n=%d reduced=%v median=%.1fms p95=%.1fms max=%.1fms",
			phase, w.name, mode, tm.n, tm.reduced, ms(tm.median), ms(tm.p95), ms(tm.max))
	}
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

func (h *evHarness) timeWorkload(w evWorkload, mode string) evTiming {
	h.t.Helper()
	c, err := h.pool.Acquire(h.ctx)
	if err != nil {
		h.t.Fatalf("acquire: %v", err)
	}
	defer c.Release()
	stmts := []string{`SET statement_timeout = 0`, fmt.Sprintf("SET plan_cache_mode = force_%s_plan", mode)}
	if h.jitOff {
		stmts = append(stmts, `SET jit = off`)
	}
	for _, s := range stmts {
		if _, err := c.Exec(h.ctx, s, pgx.QueryExecModeSimpleProtocol); err != nil {
			h.t.Fatalf("%s: %v", s, err)
		}
	}
	// The connection returns to the pool; RESET ALL clears plan_cache_mode and statement_timeout
	// (it leaves pgx's client-side statement cache valid, unlike DISCARD ALL).
	defer func() { _, _ = c.Exec(context.Background(), `RESET ALL`, pgx.QueryExecModeSimpleProtocol) }()
	q := store.New(c)
	run := w.run
	if w.rawRun != nil {
		run = func(ctx context.Context, _ *store.Queries) error { return w.rawRun(ctx, c) }
	}
	warm, n := 3, h.runs
	tm := evTiming{}
	for i := 0; i < warm; i++ {
		start := time.Now()
		if err := run(h.ctx, q); err != nil {
			h.t.Fatalf("%s warm-up: %v", w.name, err)
		}
		// A statement that already takes more than 8 s (the request budget) is measured with one
		// warm-up and at most 8 runs; the table flags it as reduced.
		if i == 0 && time.Since(start) > 8*time.Second {
			warm, n, tm.reduced = 1, min(n, 8), true
		}
	}
	durs := make([]time.Duration, 0, n)
	for i := 0; i < n; i++ {
		start := time.Now()
		if err := run(h.ctx, q); err != nil {
			h.t.Fatalf("%s: %v", w.name, err)
		}
		durs = append(durs, time.Since(start))
	}
	sort.Slice(durs, func(i, j int) bool { return durs[i] < durs[j] })
	tm.n = len(durs)
	if tm.n%2 == 1 {
		tm.median = durs[tm.n/2]
	} else {
		tm.median = (durs[tm.n/2-1] + durs[tm.n/2]) / 2
	}
	tm.p95 = durs[int(math.Ceil(0.95*float64(tm.n)))-1]
	tm.max = durs[tm.n-1]
	return tm
}

// ---- EXPLAIN ----

type evScan struct{ loops, rows, ms float64 }

type evPlan struct {
	execMs, planMs        float64
	jitMs, jitFuncs       float64
	tempW, tempR          int64
	sharedHit, sharedRead int64
	generic, custom       int64
	scans                 map[string]*evScan
	lines, nlLines        []string
	indexNames            map[string]bool
	sortDisk              int
	totalMs               float64
	root                  map[string]any // the EXPLAIN plan root, for checks that need the tree
}

func (p *evPlan) loopsOf(label string) float64 {
	if s := p.scans[label]; s != nil {
		return s.loops
	}
	return 0
}

func (p *evPlan) usesIndex(name string) bool { return p.indexNames[name] }

func (p *evPlan) scanLines() []string {
	keys := make([]string, 0, len(p.scans))
	for k := range p.scans {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		s := p.scans[k]
		out = append(out, fmt.Sprintf("%-70s loops=%.0f rows=%.0f time=%.1fms", k, s.loops, s.rows, s.ms))
	}
	return out
}

// explain PREPAREs st and runs EXPLAIN (ANALYZE, BUFFERS) EXECUTE under the given plan_cache_mode.
// A generic-mode plan is only accepted when pg_prepared_statements shows generic_plans >= 1.
func (h *evHarness) explain(mode string, st evStmt) *evPlan {
	h.t.Helper()
	h.simple(fmt.Sprintf("SET plan_cache_mode = force_%s_plan", mode))
	h.simple("DEALLOCATE ALL")
	h.simple("PREPARE evstmt AS " + st.sql)
	exec := "EXECUTE evstmt"
	if st.args != "" {
		exec += "(" + st.args + ")"
	}
	var raw []byte
	h.scalar("EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+exec, &raw)
	p := &evPlan{scans: map[string]*evScan{}, indexNames: map[string]bool{}}
	h.scalar(`SELECT generic_plans, custom_plans FROM pg_prepared_statements WHERE name = 'evstmt'`, &p.generic, &p.custom)
	h.simple("DEALLOCATE ALL")
	h.simple("RESET plan_cache_mode")
	if mode == "generic" && p.generic < 1 {
		h.t.Fatalf("%s: generic_plans = %d; EXPLAIN did not run the generic plan", st.name, p.generic)
	}
	var doc []map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil || len(doc) != 1 {
		h.t.Fatalf("%s: decode EXPLAIN JSON: %v", st.name, err)
	}
	p.execMs, _ = doc[0]["Execution Time"].(float64)
	p.planMs, _ = doc[0]["Planning Time"].(float64)
	if jit, ok := doc[0]["JIT"].(map[string]any); ok {
		p.jitFuncs = evNum(jit, "Functions")
		if tm, ok := jit["Timing"].(map[string]any); ok {
			p.jitMs = evNum(tm, "Total")
		}
	}
	root, _ := doc[0]["Plan"].(map[string]any)
	p.totalMs = p.execMs
	p.root = root
	p.sharedHit, p.sharedRead = int64(evNum(root, "Shared Hit Blocks")), int64(evNum(root, "Shared Read Blocks"))
	p.walk(root, 0)
	return p
}

func evNum(n map[string]any, k string) float64 { f, _ := n[k].(float64); return f }

func evStr(n map[string]any, k string) string { s, _ := n[k].(string); return s }

func evKids(n map[string]any) []map[string]any {
	raw, _ := n["Plans"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, k := range raw {
		if m, ok := k.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func evLabel(n map[string]any) string {
	l := evStr(n, "Node Type")
	if jt := evStr(n, "Join Type"); jt != "" && strings.Contains(l, "Join") {
		l += " (" + jt + ")"
	}
	if ix := evStr(n, "Index Name"); ix != "" {
		l += " using " + ix
	}
	if rel := evStr(n, "Relation Name"); rel != "" {
		l += " on " + rel
	}
	if cte := evStr(n, "CTE Name"); cte != "" {
		l += " " + cte
	}
	if sn := evStr(n, "Subplan Name"); sn != "" {
		l += " [" + sn + "]"
	}
	return l
}

func (p *evPlan) walk(n map[string]any, depth int) {
	loops := evNum(n, "Actual Loops")
	rows := evNum(n, "Actual Rows") * loops
	timeMs := evNum(n, "Actual Total Time") * loops
	label := evLabel(n)
	tw := int64(evNum(n, "Temp Written Blocks"))
	if tw > p.tempW {
		p.tempW = tw
	}
	if tr := int64(evNum(n, "Temp Read Blocks")); tr > p.tempR {
		p.tempR = tr
	}
	if evStr(n, "Sort Space Type") == "Disk" {
		p.sortDisk++
	}
	nodeType := evStr(n, "Node Type")
	isScan := evStr(n, "Relation Name") != "" || evStr(n, "Index Name") != ""
	if ix := evStr(n, "Index Name"); ix != "" {
		p.indexNames[ix] = true
	}
	if isScan {
		s := p.scans[label]
		if s == nil {
			s = &evScan{}
			p.scans[label] = s
		}
		s.loops += loops
		s.rows += rows
		s.ms += timeMs
	}
	isJoin := strings.Contains(nodeType, "Join") || nodeType == "Nested Loop"
	interesting := isScan || isJoin || nodeType == "CTE Scan" || nodeType == "WindowAgg" ||
		tw > 0 || (p.totalMs > 0 && timeMs >= 0.05*p.totalMs)
	if interesting {
		spill := ""
		if tw > 0 {
			spill = fmt.Sprintf(" temp_written=%d", tw)
		}
		if sm := evStr(n, "Sort Method"); sm != "" {
			spill += " sort=" + sm + "/" + evStr(n, "Sort Space Type")
		}
		p.lines = append(p.lines, fmt.Sprintf("%s%s rows=%.0f loops=%.0f time=%.1fms%s", strings.Repeat("  ", min(depth, 24)), label, rows, loops, timeMs, spill))
	}
	kids := evKids(n)
	if nodeType == "Nested Loop" && len(kids) == 2 {
		inner := kids[1]
		il := evNum(inner, "Actual Loops")
		if il >= 1000 || rows >= 50000 {
			p.nlLines = append(p.nlLines, fmt.Sprintf("%s: outer=%s (rows %.0f) inner=%s (loops %.0f, rows/loop %.1f) total_rows=%.0f time=%.1fms",
				evLabel(n), evLabel(kids[0]), evNum(kids[0], "Actual Rows")*evNum(kids[0], "Actual Loops"), evLabel(inner), il, evNum(inner, "Actual Rows"), rows, timeMs))
		}
	}
	for _, k := range kids {
		p.walk(k, depth+1)
	}
}

// ---- Issue #4685 (m3): Reliability and latest-failure evidence ----

const (
	evTerminalIdxName = "runs_terminal_end_idx"
	evTerminalIdxDDL  = `CREATE INDEX runs_terminal_end_idx ON runs ((COALESCE(finished_at, status_since)), id) WHERE status IN ('completed', 'failed', 'cancelled')`
	// evLandablePct is the share (percent) of Delivery agent_failure runs rewritten to a
	// human-landable fail_origin. The seed's origins are not landable, so without the rewrite
	// the needs_landing branch of the cohort statement never fires.
	evLandablePct = 35
)

func evSQLTextArray(vals []string) string {
	return "ARRAY['" + strings.Join(vals, "', '") + "']::text[]"
}

func evHash(col, salt string) string {
	return "(hashtext(" + col + "::text || '" + salt + "')::bigint & 2147483647)"
}

// evLandableStmts is the landable-origin step. Every selection is a function of the run id, so
// the step is idempotent under UZI_EVIDENCE_RESEED=0. Shares: evLandablePct of Delivery
// agent_failure runs get a landable origin; of those, 30% an available recovery capture, 10% an
// expired capture (a decoy the statement must not count), 25% a preserved patch (independent
// hash), the rest neither. 5% of Delivery runs carry a requeue count.
func evLandableStmts() []string {
	origins := evSQLTextArray(workersvc.AllHumanLandableFailOrigins())
	deliv := evSQLTextArray(runkind.Delivery())
	n := strconv.Itoa(len(workersvc.AllHumanLandableFailOrigins()))
	return []string{
		`UPDATE runs SET fail_origin = (` + origins + `)[1 + ((` + evHash("id", "o") + ` / 100) % ` + n + `)::int]
		 WHERE status = 'failed' AND fail_origin = 'agent_failure' AND kind = ANY(` + deliv + `)
		   AND ` + evHash("id", "o") + ` % 100 < ` + strconv.Itoa(evLandablePct),
		// ~3.2 KB of incompressible text: stored out of line, so it widens nothing in the heap.
		`UPDATE runs SET preserved_patch = (SELECT string_agg(md5(runs.id::text || g::text), '') FROM generate_series(1, 100) g)
		 WHERE status = 'failed' AND fail_origin = ANY(` + origins + `) AND ` + evHash("id", "p") + ` % 100 < 25`,
		`UPDATE runs SET requeue_count = 1 + ((` + evHash("id", "q") + ` / 100) % 3)::int
		 WHERE kind = ANY(` + deliv + `) AND requeue_count = 0 AND ` + evHash("id", "q") + ` % 100 < 5`,
		`INSERT INTO recovery_custody_holds (id, user_id, repo_id, run_id, generation, state, original_worker_id, original_worker_identity, released_at)
		 SELECT gen_random_uuid(), r.user_id, r.repo_id, r.id, 1, 'released', gen_random_uuid(), 'ev-worker', now()
		 FROM runs r
		 WHERE r.status = 'failed' AND r.fail_origin = ANY(` + origins + `) AND ` + evHash("r.id", "c") + ` % 100 < 40
		   AND NOT EXISTS (SELECT 1 FROM recovery_custody_holds h WHERE h.run_id = r.id)`,
		`INSERT INTO recovery_captures (id, hold_id, run_id, user_id, original_worker_identity, source_sha, idempotency_key, state, manifest_bound, expires_at)
		 SELECT gen_random_uuid(), h.id, h.run_id, h.user_id, 'ev-worker', md5(h.run_id::text), 'ev-' || h.run_id::text,
		        CASE WHEN ` + evHash("h.run_id", "c") + ` % 100 < 30 THEN 'available' ELSE 'expired' END, true, now() + interval '7 days'
		 FROM recovery_custody_holds h
		 WHERE h.original_worker_identity = 'ev-worker'
		   AND NOT EXISTS (SELECT 1 FROM recovery_captures c WHERE c.hold_id = h.id)`,
		`ANALYZE runs`, `ANALYZE recovery_custody_holds`, `ANALYZE recovery_captures`,
	}
}

func (h *evHarness) landableStep() {
	h.t.Helper()
	h.logf("\n######## RELIABILITY: landable-origin step (%d%% of Delivery agent_failure runs) ########", evLandablePct)
	start := time.Now()
	for _, s := range evLandableStmts() {
		h.simple(s)
	}
	h.logf("landable step took %s", time.Since(start).Round(time.Millisecond))
	land := "status = 'failed' AND fail_origin = ANY(" + evSQLTextArray(workersvc.AllHumanLandableFailOrigins()) + ")"
	capOK := "EXISTS (SELECT 1 FROM recovery_captures c WHERE c.run_id = runs.id AND c.user_id = runs.user_id AND c.state = 'available')"
	for _, q := range []string{
		`SELECT 'landable-origin failed runs', count(*) FROM runs WHERE ` + land,
		`SELECT '  with available capture only', count(*) FROM runs WHERE ` + land + ` AND ` + capOK + ` AND preserved_patch IS NULL`,
		`SELECT '  with preserved_patch only', count(*) FROM runs WHERE ` + land + ` AND NOT ` + capOK + ` AND preserved_patch IS NOT NULL`,
		`SELECT '  with both', count(*) FROM runs WHERE ` + land + ` AND ` + capOK + ` AND preserved_patch IS NOT NULL`,
		`SELECT '  with neither (not needs_landing)', count(*) FROM runs WHERE ` + land + ` AND NOT ` + capOK + ` AND preserved_patch IS NULL`,
		`SELECT '  expired-capture decoys without patch', count(*) FROM runs WHERE ` + land + ` AND NOT ` + capOK + ` AND preserved_patch IS NULL AND EXISTS (SELECT 1 FROM recovery_captures c WHERE c.run_id = runs.id AND c.state = 'expired')`,
		`SELECT 'recovery_captures available/expired', (SELECT count(*) FILTER (WHERE state = 'available') FROM recovery_captures) || '/' || (SELECT count(*) FILTER (WHERE state = 'expired') FROM recovery_captures)`,
		`SELECT 'failed origins', string_agg(coalesce(fail_origin, 'NULL') || '=' || c, ' ' ORDER BY fail_origin) FROM (SELECT fail_origin, count(*) c FROM runs WHERE status = 'failed' GROUP BY 1) x`,
		`SELECT 'requeued runs', count(*) FROM runs WHERE requeue_count > 0`,
	} {
		h.logf("%s", h.shape(q))
	}
}

// evRelSpec is one request shape: the scope (owner-shaped or all-users statement), the window
// and the cohort filters. breakdowns lists the Breakdown values the shape is measured with.
type evRelSpec struct {
	name       string
	owner      uuid.UUID // uuid.Nil: the all-users statements
	window     analytics.Window
	repo       uuid.UUID
	reveal     bool // the admin route with user=<uuid>: owner-shaped statement, repo names revealed
	breakdowns []string
	idx        bool // re-measured with the runs_terminal_end_idx candidate
}

func evRepoArg(id uuid.UUID) pgtype.UUID {
	if id == uuid.Nil {
		return pgtype.UUID{}
	}
	return pgtype.UUID{Bytes: id, Valid: true}
}

func evRelHistory(ctx context.Context, q *store.Queries, spec evRelSpec) (*time.Time, error) {
	var hist pgtype.Timestamptz
	var err error
	if spec.owner != uuid.Nil {
		hist, err = q.AnalyticsOwnerHistoryStart(ctx, store.AnalyticsOwnerHistoryStartParams{UserID: spec.owner, RepoID: evRepoArg(spec.repo), Kinds: runkind.Delivery()})
	} else {
		hist, err = q.AnalyticsAllHistoryStart(ctx, store.AnalyticsAllHistoryStartParams{RepoID: evRepoArg(spec.repo), Kinds: runkind.Delivery()})
	}
	if err != nil || !hist.Valid {
		return nil, err
	}
	e := hist.Time
	return &e, nil
}

func evRelOwnerParams(spec evRelSpec, breakdown string, b analytics.Bounds) store.AnalyticsOwnerReliabilityParams {
	var prev pgtype.Timestamptz
	if b.PreviousFrom != nil {
		prev = spendTS(*b.PreviousFrom)
	}
	return store.AnalyticsOwnerReliabilityParams{
		From: spendTS(b.From), PreviousFrom: prev, Breakdown: breakdown, Now: spendTS(b.Now), FoldFrom: spendTS(b.FoldFrom),
		BucketStarts: spendTSs(b.BucketStarts), BucketEnds: spendTSs(b.BucketEnds),
		RollStarts: spendTSs(b.RollStarts), RollEnds: spendTSs(b.RollEnds),
		RollHarnesses: []string{"claude", "codex"}, LandableOrigins: workersvc.AllHumanLandableFailOrigins(),
		RevealRepos: spec.reveal, UserID: spec.owner, RepoID: evRepoArg(spec.repo), Kinds: runkind.Delivery(),
	}
}

func evRelAllParams(spec evRelSpec, breakdown string, b analytics.Bounds) store.AnalyticsAllReliabilityParams {
	var prev pgtype.Timestamptz
	if b.PreviousFrom != nil {
		prev = spendTS(*b.PreviousFrom)
	}
	return store.AnalyticsAllReliabilityParams{
		From: spendTS(b.From), PreviousFrom: prev, Breakdown: breakdown, Now: spendTS(b.Now), FoldFrom: spendTS(b.FoldFrom),
		BucketStarts: spendTSs(b.BucketStarts), BucketEnds: spendTSs(b.BucketEnds),
		RollStarts: spendTSs(b.RollStarts), RollEnds: spendTSs(b.RollEnds),
		RollHarnesses: []string{"claude", "codex"}, LandableOrigins: workersvc.AllHumanLandableFailOrigins(),
		RepoID: evRepoArg(spec.repo), Kinds: runkind.Delivery(),
	}
}

func (h *evHarness) relCohortWorkload(spec evRelSpec, breakdown string) evWorkload {
	h.t.Helper()
	earliest, err := evRelHistory(h.ctx, h.q, spec)
	if err != nil {
		h.t.Fatalf("history start: %v", err)
	}
	b, err := analytics.ComputeBounds(h.now, time.UTC, spec.window, earliest)
	if err != nil {
		h.t.Fatalf("ComputeBounds: %v", err)
	}
	capture := &capturingDBTX{}
	cq := store.New(capture)
	w := evWorkload{name: "rel-" + spec.name + "-" + breakdown, idx: spec.idx}
	if spec.owner != uuid.Nil {
		p := evRelOwnerParams(spec, breakdown, b)
		_, _ = cq.AnalyticsOwnerReliability(h.ctx, p)
		w.stmts = []evStmt{{name: "AnalyticsOwnerReliability", sql: store.AnalyticsOwnerReliabilitySQL}}
		w.run = func(ctx context.Context, q *store.Queries) error {
			_, err := q.AnalyticsOwnerReliability(ctx, p)
			return err
		}
	} else {
		p := evRelAllParams(spec, breakdown, b)
		_, _ = cq.AnalyticsAllReliability(h.ctx, p)
		w.stmts = []evStmt{{name: "AnalyticsAllReliability", sql: store.AnalyticsAllReliabilitySQL}}
		w.run = func(ctx context.Context, q *store.Queries) error {
			_, err := q.AnalyticsAllReliability(ctx, p)
			return err
		}
	}
	lits := make([]string, len(capture.args))
	for i, a := range capture.args {
		lits[i] = sqlLiteral(h.t, a)
	}
	w.stmts[0].args = strings.Join(lits, ", ")
	h.logf("WORKLOAD %s: window=%s buckets=%d rolling=%d from=%s fold_from=%s reveal=%v repo=%s",
		w.name, spec.window, len(b.BucketStarts), len(b.RollStarts), b.From.Format(time.RFC3339), b.FoldFrom.Format(time.RFC3339), spec.reveal, spec.repo)
	return w
}

// evLatestSpec is one latest-failure scope: owner uuid.Nil is the all-users statement.
type evLatestSpec struct {
	name        string
	owner, repo uuid.UUID
	idx         bool
}

// evLatestPartsSQL returns the two parts of the latest-failure statement as standalone texts:
// the row lookup (the last_failure CTE) and the strict-after completed count with the failure's
// terminal timestamp as a parameter. The parameter order is $1 repo, $2 harness, $3 kinds, then
// the owner id (owner statement only), then the timestamp (count only).
func evLatestPartsSQL(owner bool) (row, count string) {
	userRow, userCnt, ended := "", "", "$4"
	if owner {
		userRow, userCnt, ended = " AND user_id = $4::uuid", " AND r3.user_id = $4::uuid", "$5"
	}
	row = `SELECT id, user_id, COALESCE(finished_at, status_since) AS ended_at, COALESCE(fail_origin, 'unknown') AS origin
		FROM runs
		WHERE status = 'failed' AND fail_origin IS DISTINCT FROM 'plan_rejected' AND fail_origin IS DISTINCT FROM 'base_diverged'
		  AND fn_outcome_kind(kind)` + userRow + `
		  AND ($1::uuid IS NULL OR repo_id = $1::uuid) AND ($2::text IS NULL OR harness = $2::text) AND kind = ANY($3::text[])
		ORDER BY COALESCE(finished_at, status_since) DESC, id DESC LIMIT 1`
	count = `SELECT count(*)::bigint FROM runs r3
		WHERE COALESCE(r3.finished_at, r3.status_since) > ` + ended + `::timestamptz
		  AND r3.status = 'completed' AND fn_outcome_kind(r3.kind)` + userCnt + `
		  AND ($1::uuid IS NULL OR r3.repo_id = $1::uuid) AND ($2::text IS NULL OR r3.harness = $2::text) AND r3.kind = ANY($3::text[])`
	return row, count
}

func evDrain(ctx context.Context, c *pgxpool.Conn, sql string, args []any) error {
	rows, err := c.Query(ctx, sql, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
	}
	return rows.Err()
}

// latestWorkloads returns three workloads for one scope: the full sqlc statement, and its row
// lookup and strict-after count as standalone equivalent queries (the count's timestamp is the
// failure's real terminal timestamp), so the two parts are reported separately.
func (h *evHarness) latestWorkloads(sp evLatestSpec) []evWorkload {
	h.t.Helper()
	kinds := runkind.Delivery()
	repo := evRepoArg(sp.repo)
	ownerScope := sp.owner != uuid.Nil
	capture := &capturingDBTX{}
	cq := store.New(capture)
	full := evWorkload{name: "latest-" + sp.name + "-full", idx: sp.idx}
	var res store.AnalyticsAllLatestFailureRow
	var err error
	if ownerScope {
		p := store.AnalyticsOwnerLatestFailureParams{UserID: sp.owner, RepoID: repo, Kinds: kinds}
		var r store.AnalyticsOwnerLatestFailureRow
		r, err = h.q.AnalyticsOwnerLatestFailure(h.ctx, p)
		res = store.AnalyticsAllLatestFailureRow(r)
		_, _ = cq.AnalyticsOwnerLatestFailure(h.ctx, p)
		full.stmts = []evStmt{{name: "AnalyticsOwnerLatestFailure", sql: store.AnalyticsOwnerLatestFailureSQL}}
		full.run = func(ctx context.Context, q *store.Queries) error {
			_, err := q.AnalyticsOwnerLatestFailure(ctx, p)
			return err
		}
	} else {
		p := store.AnalyticsAllLatestFailureParams{RepoID: repo, Kinds: kinds}
		res, err = h.q.AnalyticsAllLatestFailure(h.ctx, p)
		_, _ = cq.AnalyticsAllLatestFailure(h.ctx, p)
		full.stmts = []evStmt{{name: "AnalyticsAllLatestFailure", sql: store.AnalyticsAllLatestFailureSQL}}
		full.run = func(ctx context.Context, q *store.Queries) error {
			_, err := q.AnalyticsAllLatestFailure(ctx, p)
			return err
		}
	}
	if err != nil {
		h.t.Fatalf("latest failure: %v", err)
	}
	lits := make([]string, len(capture.args))
	for i, a := range capture.args {
		lits[i] = sqlLiteral(h.t, a)
	}
	full.stmts[0].args = strings.Join(lits, ", ")
	h.logf("LATEST %s: found=%v failed_at=%s origin=%s completed_since=%d repo=%s",
		sp.name, res.Found, res.FailedAt.Time.Format(time.RFC3339), res.Origin, res.CompletedSinceLastFailure, sp.repo)

	rowSQL, cntSQL := evLatestPartsSQL(ownerScope)
	rowArgs := []any{repo, pgtype.Text{}, kinds}
	if ownerScope {
		rowArgs = append(rowArgs, sp.owner)
	}
	cntArgs := append(slices.Clone(rowArgs), res.FailedAt)
	lit := func(args []any) string {
		out := make([]string, len(args))
		for i, a := range args {
			out[i] = sqlLiteral(h.t, a)
		}
		return strings.Join(out, ", ")
	}
	rowW := evWorkload{name: "latest-" + sp.name + "-row", idx: sp.idx,
		stmts:  []evStmt{{name: "latest-row-lookup", sql: rowSQL, args: lit(rowArgs)}},
		rawRun: func(ctx context.Context, c *pgxpool.Conn) error { return evDrain(ctx, c, rowSQL, rowArgs) }}
	cntW := evWorkload{name: "latest-" + sp.name + "-count", idx: sp.idx,
		stmts:  []evStmt{{name: "latest-strict-after-count", sql: cntSQL, args: lit(cntArgs)}},
		rawRun: func(ctx context.Context, c *pgxpool.Conn) error { return evDrain(ctx, c, cntSQL, cntArgs) }}
	return []evWorkload{full, rowW, cntW}
}

func (h *evHarness) busiestRepo(owner uuid.UUID) uuid.UUID {
	h.t.Helper()
	where := ""
	if owner != uuid.Nil {
		where = " AND user_id = '" + owner.String() + "'"
	}
	var id uuid.UUID
	var n int64
	h.scalar(`SELECT repo_id, count(*) FROM runs WHERE repo_id IS NOT NULL AND kind = ANY(`+evSQLTextArray(runkind.Delivery())+`)`+where+
		` GROUP BY repo_id ORDER BY count(*) DESC, repo_id LIMIT 1`, &id, &n)
	h.logf("busiest repo (owner=%s): %s with %d Delivery runs", owner, id, n)
	return id
}

func (h *evHarness) relSpecs() ([]evRelSpec, []evLatestSpec) {
	h.t.Helper()
	ownRepo, allRepo := h.busiestRepo(h.topOwner), h.busiestRepo(uuid.Nil)
	pmH := []string{"primary_model", "harness"}
	specs := []evRelSpec{
		{name: "owner-30d", owner: h.topOwner, window: analytics.Window30d, breakdowns: pmH},
		{name: "owner-all", owner: h.topOwner, window: analytics.WindowAll, breakdowns: pmH},
		{name: "admin-all-30d", window: analytics.Window30d, breakdowns: []string{"primary_model", "harness", "user"}, idx: true},
		{name: "admin-all-90d", window: analytics.Window90d, breakdowns: pmH, idx: true},
		{name: "admin-user-all", owner: h.topOwner, window: analytics.WindowAll, reveal: true, breakdowns: pmH},
		{name: "owner-30d-repo", owner: h.topOwner, window: analytics.Window30d, repo: ownRepo, breakdowns: pmH},
		{name: "admin-all-30d-repo", window: analytics.Window30d, repo: allRepo, breakdowns: pmH, idx: true},
	}
	latest := []evLatestSpec{
		{name: "owner", owner: h.topOwner},
		{name: "owner-repo", owner: h.topOwner, repo: ownRepo},
		{name: "all", idx: true},
		{name: "all-repo", repo: allRepo, idx: true},
	}
	return specs, latest
}

type evReqStep struct{ auth, hist, cohort, latest time.Duration }

// relRequestOnce runs the production request sequence through store.RunAnalyticsSnapshot (8 s
// deadline, statement_timeout '8s', jit off): the authorize-equivalent reads, the history start,
// the cohort statement plus its document decode, and the latest-failure statement.
func (h *evHarness) relRequestOnce(spec evRelSpec, breakdown string) (time.Duration, evReqStep, error) {
	var st evReqStep
	kinds := runkind.Delivery()
	start := time.Now()
	err := store.RunAnalyticsSnapshot(h.ctx, h.pool, func(ctx context.Context, s *store.AnalyticsSnapshot) error {
		t0 := time.Now()
		if _, err := s.Q.AnalyticsTimezoneNames(ctx); err != nil {
			return err
		}
		if spec.reveal {
			if _, err := s.Q.AnalyticsUserExists(ctx, spec.owner); err != nil {
				return err
			}
		}
		if spec.repo != uuid.Nil {
			var err error
			if spec.owner == uuid.Nil {
				_, err = s.Q.AnalyticsRepoExists(ctx, spec.repo)
			} else {
				_, err = s.Q.AnalyticsOwnerRepoAccessible(ctx, store.AnalyticsOwnerRepoAccessibleParams{RepoID: spec.repo, UserID: spec.owner})
			}
			if err != nil {
				return err
			}
		}
		st.auth = time.Since(t0)
		t0 = time.Now()
		earliest, err := evRelHistory(ctx, s.Q, spec)
		if err != nil {
			return err
		}
		st.hist = time.Since(t0)
		b, err := analytics.ComputeBounds(h.now, time.UTC, spec.window, earliest)
		if err != nil {
			return err
		}
		t0 = time.Now()
		var raw []byte
		if spec.owner != uuid.Nil {
			raw, err = s.Q.AnalyticsOwnerReliability(ctx, evRelOwnerParams(spec, breakdown, b))
		} else {
			raw, err = s.Q.AnalyticsAllReliability(ctx, evRelAllParams(spec, breakdown, b))
		}
		if err != nil {
			return err
		}
		if _, err := store.DecodeAnalyticsReliabilityDoc(raw); err != nil {
			return err
		}
		st.cohort = time.Since(t0)
		t0 = time.Now()
		if spec.owner != uuid.Nil {
			_, err = s.Q.AnalyticsOwnerLatestFailure(ctx, store.AnalyticsOwnerLatestFailureParams{UserID: spec.owner, RepoID: evRepoArg(spec.repo), Kinds: kinds})
		} else {
			_, err = s.Q.AnalyticsAllLatestFailure(ctx, store.AnalyticsAllLatestFailureParams{RepoID: evRepoArg(spec.repo), Kinds: kinds})
		}
		st.latest = time.Since(t0)
		return err
	})
	return time.Since(start), st, err
}

func evStats(d []time.Duration) (median, p95, maxD time.Duration) {
	if len(d) == 0 {
		return 0, 0, 0
	}
	s := slices.Clone(d)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	n := len(s)
	if n%2 == 1 {
		median = s[n/2]
	} else {
		median = (s[n/2-1] + s[n/2]) / 2
	}
	return median, s[int(math.Ceil(0.95*float64(n)))-1], s[n-1]
}

// timeRequests measures the request sequence on the production pool (default plan_cache_mode).
// A request that hits the 8 s deadline or statement_timeout is counted as a timeout and its
// elapsed time kept; any other error fails the harness.
func (h *evHarness) timeRequests(phase string, spec evRelSpec, breakdown string) {
	h.t.Helper()
	name := "rel-" + spec.name + "-" + breakdown
	if !evNameSelected(name) {
		return
	}
	warm, n := 3, h.runs
	var totals, auth, hist, cohort, latest []time.Duration
	timeouts, warmTimeouts := 0, 0
	for i := 0; i < warm+n; i++ {
		d, st, err := h.relRequestOnce(spec, breakdown)
		timedOut := false
		if err != nil {
			if !errors.Is(store.ClassifyAnalyticsErr(err), store.ErrAnalyticsTimeout) {
				h.t.Fatalf("%s request: %v", name, err)
			}
			timedOut = true
		}
		if i < warm {
			// A warm-up is never a sample, timed out or not. A timed-out warm-up ends the warm-up
			// phase at itself (so the next request is measured) and shortens the measured run.
			if timedOut {
				warmTimeouts++
				warm, n = i+1, min(n, 5)
			}
			continue
		}
		if timedOut {
			timeouts++
		}
		totals, auth, hist, cohort, latest = append(totals, d), append(auth, st.auth), append(hist, st.hist), append(cohort, st.cohort), append(latest, st.latest)
	}
	med, p95, mx := evStats(totals)
	ma, _, _ := evStats(auth)
	mh, _, _ := evStats(hist)
	mc, _, _ := evStats(cohort)
	ml, _, _ := evStats(latest)
	h.logf("REQUEST %s%s %s n=%d timeouts=%d warm_timeouts=%d total median=%.1fms p95=%.1fms max=%.1fms | step medians: auth=%.1fms history=%.1fms cohort+decode=%.1fms latest=%.1fms",
		os.Getenv("UZI_EVIDENCE_LABEL"), phase, name, len(totals), timeouts, warmTimeouts, ms(med), ms(p95), ms(mx), ms(ma), ms(mh), ms(mc), ms(ml))
}

func (h *evHarness) reliabilityEvidence() {
	h.t.Helper()
	h.landableStep()
	h.jitOff = true
	h.simple(`SET jit = off`)
	h.logf("Reliability phase runs with jit = off, as store.RunAnalyticsSnapshot does")
	h.simple("DROP INDEX IF EXISTS " + evTerminalIdxName)
	h.simple(`ANALYZE runs`)
	var idxList string
	h.scalar(`SELECT string_agg(indexname || '=' || pg_relation_size(indexname::regclass), ' ' ORDER BY indexname) FROM pg_indexes WHERE tablename = 'runs'`, &idxList)
	h.logf("runs indexes (bytes): %s", idxList)
	h.logDisk("reliability baseline")

	specs, latest := h.relSpecs()
	var ws []evWorkload
	for _, sp := range specs {
		for _, bd := range sp.breakdowns {
			ws = append(ws, h.relCohortWorkload(sp, bd))
		}
	}
	for _, ls := range latest {
		ws = append(ws, h.latestWorkloads(ls)...)
	}
	ws = evOnly(ws)

	h.logf("\n######## RELIABILITY BASELINE (no %s) ########", evTerminalIdxName)
	for _, w := range ws {
		h.measure("rel-base", w, h.modes, true)
	}
	h.logf("\n######## RELIABILITY REQUEST-LEVEL (baseline) ########")
	for _, sp := range specs {
		for _, bd := range sp.breakdowns {
			h.timeRequests("rel-base", sp, bd)
		}
	}
	if os.Getenv("UZI_EVIDENCE_REL_INDEX") == "0" {
		return
	}

	h.logf("\n######## CANDIDATE: %s ########", evTerminalIdxDDL)
	start := time.Now()
	h.simple(evTerminalIdxDDL)
	h.simple(`ANALYZE runs`)
	var size int64
	h.scalar(fmt.Sprintf("SELECT pg_relation_size('%s')", evTerminalIdxName), &size)
	h.logf("CREATE INDEX + ANALYZE took %s; index size %d bytes", time.Since(start).Round(time.Millisecond), size)
	h.logDisk("with " + evTerminalIdxName)
	for _, w := range ws {
		if w.idx {
			h.measure("rel-idx", w, h.modes, true, evTerminalIdxName)
		}
	}
	h.logf("\n######## RELIABILITY REQUEST-LEVEL (with %s) ########", evTerminalIdxName)
	for _, sp := range specs {
		if !sp.idx {
			continue
		}
		for _, bd := range sp.breakdowns {
			h.timeRequests("rel-idx", sp, bd)
		}
	}
	h.simple("DROP INDEX " + evTerminalIdxName)
	h.simple(`ANALYZE runs`)
}
