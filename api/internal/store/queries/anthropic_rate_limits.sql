-- name: ListAnthropicTokensToPoll :many
-- Every anthropic_token secret to poll each tick (PRD #104 M5): the token's id and
-- owner plus the sealed ciphertext and its sealed_with, so the poller opens them in
-- one pass instead of re-fetching each ciphertext (N+1). The ciphertext is opened
-- in-process via the vault path and is never logged nor placed in any error string.
-- Whether a given token can actually be opened (vault unlocked, master-sealed
-- exception) is decided at open time, so a locked user's tokens still appear here
-- and are skipped (PRD #53 D3).
--
-- One row per TOKEN, not per user (it was ListUsersWithAnthropicToken until M5).
-- The windows Anthropic reports are per-credential, so a user with three tokens is
-- three readings; polling only the default would leave the other two rendering
-- someone else's numbers. Poll cost therefore scales with token count, not user
-- count — see R3.
--
-- ORDER BY keeps a tick's work deterministic, which makes a slow tick's log
-- readable and a partial tick's coverage predictable rather than arbitrary.
SELECT id, user_id, ciphertext, sealed_with FROM user_secrets
WHERE kind = 'anthropic_token'
ORDER BY user_id, id;

-- name: UpsertRateLimits :exec
-- Overwrite ONE token's gauge row each poll tick (PRD #53 D4, repointed by #104
-- M5). A malformed reading never reaches here (the poller fails closed and keeps
-- the last good row, D5), so every write carries a complete reading.
--
-- user_id rides along rather than being looked up: the caller already has it from
-- the poll listing, and it is half of the composite FK that ties this row to a
-- (user, token) pair that exists.
--
-- The FK is checked on the INSERT path only: ON CONFLICT .. DO UPDATE deliberately
-- does not touch user_id, so an upsert over an EXISTING row rewrites the reading
-- without re-validating ownership. That is safe BY CONSTRUCTION, not by the
-- caller's discipline — user_secret_id is the global PRIMARY KEY of user_secrets,
-- so an id belongs to exactly one owner for its whole life and no call site can
-- construct a mismatched (user_id, user_secret_id) pair to smuggle through the
-- conflict path. Stated this way on purpose: "the poller always passes a matching
-- pair" would be the weaker true reason, and the weaker one is the one that rots
-- the moment someone adds a third caller.
INSERT INTO anthropic_rate_limits (
    user_secret_id, user_id, five_hour_pct, five_hour_resets_at,
    seven_day_pct, seven_day_resets_at, source, synced_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (user_secret_id) DO UPDATE SET
    five_hour_pct       = EXCLUDED.five_hour_pct,
    five_hour_resets_at = EXCLUDED.five_hour_resets_at,
    seven_day_pct       = EXCLUDED.seven_day_pct,
    seven_day_resets_at = EXCLUDED.seven_day_resets_at,
    source              = EXCLUDED.source,
    synced_at           = EXCLUDED.synced_at;

-- name: ListRateLimitsForUser :many
-- One user's meters, one row per TOKEN, for GET /api/me/rate-limits (PRD #104 D4 —
-- a breaking response-shape change from the single reading PRD #53 returned).
--
-- Driven from user_secrets and LEFT JOINed to the gauge, so a token with no reading
-- yet still appears (as `unavailable`) instead of vanishing from the list: a user
-- who just added a token should see it listed with no numbers, not see nothing.
-- Carries label + is_default because the UI has to say WHICH credential each meter
-- describes — the whole point of the change — and those are names, never values.
--
-- Ordered default-first then by label, so the meter a user's unbound workers spend
-- against leads the list.
--
-- auto_eligible (PRD #111 M2) joins the projection because this query, and only
-- this query, carries BOTH halves of a token's auto-selection story: the owner's
-- opt-in and the gauge reading that decides whether the selector could actually
-- pick it. The handler feeds the pair to autoselect.Classify and ships the answer
-- as a string, so the web never re-derives eligibility from pcts and timestamps
-- (D21). The LEFT JOIN above is what makes "never polled" expressible at all — an
-- INNER JOIN would drop exactly the token whose silent ineligibility R7 is about.
SELECT s.id            AS user_secret_id,
       s.label         AS label,
       s.is_default    AS is_default,
       s.auto_eligible AS auto_eligible,
       rl.five_hour_pct,
       rl.five_hour_resets_at,
       rl.seven_day_pct,
       rl.seven_day_resets_at,
       rl.source,
       rl.synced_at
FROM user_secrets s
LEFT JOIN anthropic_rate_limits rl ON rl.user_secret_id = s.id
WHERE s.user_id = $1 AND s.kind = 'anthropic_token'
ORDER BY s.is_default DESC, lower(s.label) ASC;

-- name: ListRateLimits :many
-- Every user's tokens LEFT JOINed to their gauge rows, for
-- GET /api/admin/rate-limits (PRD #104 D4): one row per (user, token), plus one row
-- per TOKEN-LESS user so the admin view still lists everyone.
--
-- The LEFT JOIN chain is users → user_secrets → anthropic_rate_limits, so a
-- token-less user yields a single row with a NULL user_secret_id (rendered
-- `no_token`), a token with no reading yields a row with a NULL synced_at
-- (`unavailable`), and a token with a reading yields the reading. has_token is
-- therefore derivable here (user_secret_id IS NOT NULL) rather than needing the
-- separate EXISTS the per-user shape used.
--
-- vault_locked is computed in-memory from the live vault, not stored, so it is not
-- selected here.
--
-- auto_eligible (PRD #111 M2) is selected here for the same reason the per-user
-- query selects it, and it is NOT optional: both queries feed the one
-- TokenRateLimitDTO, so omitting it here would make the admin view report every
-- token as un-pooled — a confident, uniform, wrong answer, which is worse than the
-- field being absent. It is nullable through the LEFT JOIN (a token-less user's row
-- has no secret at all), and that row is skipped before the flag is read.
SELECT
    u.id            AS user_id,
    u.email         AS email,
    u.display_name  AS display_name,
    s.id            AS user_secret_id,
    s.label         AS label,
    s.is_default    AS is_default,
    s.auto_eligible AS auto_eligible,
    rl.five_hour_pct,
    rl.five_hour_resets_at,
    rl.seven_day_pct,
    rl.seven_day_resets_at,
    rl.source,
    rl.synced_at
FROM users u
LEFT JOIN user_secrets s ON s.user_id = u.id AND s.kind = 'anthropic_token'
LEFT JOIN anthropic_rate_limits rl ON rl.user_secret_id = s.id
ORDER BY u.email ASC, s.is_default DESC NULLS LAST, lower(s.label) ASC;

-- name: ListAutoSelectCandidates :many
-- Every anthropic_token one user holds, with its gauge reading and the number of
-- runs currently spending it, for PRD #111 M4's ranker.
--
-- 🔴 DELIBERATELY NOT FILTERED ON auto_eligible, and that is not an oversight to be
-- "optimised" away. The whole eligibility gate lives in ONE place
-- (autoselect.Classify, D21); filtering here would split it between SQL and Go, and
-- the ranker could then no longer tell "the user pooled nothing" from "the user
-- pooled tokens that are all stale" — different fallback reasons that send a user to
-- different places (settings vs. the poller). The WHERE clause is ownership and
-- kind, which are facts about which rows EXIST, never about which are pickable.
--
-- Both LEFT JOINs are load-bearing for the same reason. A token with no gauge row
-- must appear and classify `no_reading` rather than vanish — that row IS R7's silent
-- no-op made visible — and a token with no runs on it must yield 0 rather than
-- disappear from the ranking the moment it is idle, which is precisely when it
-- should win.
--
-- The in-flight rollup counts EVERY lane and EVERY reason (D18): it models
-- concurrent spend against a credential, and nothing about how a run acquired that
-- credential changes the quota it consumes. So chat and judge runs count (M1 made
-- them countable for the first time), and it does not filter on
-- anthropic_select_reason — excluding fallback-chosen runs would blind the bias to
-- exactly the pile-up a fallback creates. `awaiting_approval` counts because the
-- worker holds the session and resumes on the same token; that one is a deliberate
-- conservative choice, not an oversight.
--
-- @user_id appears twice on purpose, once in each scope. sqlc collapses repeated
-- named params into ONE argument; writing $1 in one place and @user_id in the other
-- yields two, which is a confusing Params struct rather than a bug — but only until
-- someone passes different values to them.
--
-- ORDER BY s.id makes the row order deterministic. autoselect.Select is
-- order-independent by construction, so this is for readable diffs and reproducible
-- tests, not for correctness.
SELECT s.id                     AS user_secret_id,
       s.label                  AS label,
       s.auto_eligible          AS auto_eligible,
       rl.five_hour_pct,
       rl.five_hour_resets_at,
       rl.seven_day_pct,
       rl.seven_day_resets_at,
       rl.synced_at,
       COALESCE(f.n, 0)::bigint AS in_flight_runs
FROM user_secrets s
LEFT JOIN anthropic_rate_limits rl ON rl.user_secret_id = s.id
LEFT JOIN (
    -- 🔴 'limit_wait' IS EXCLUDED DELIBERATELY, AND WIDENING THIS STATUS SET TO
    -- INCLUDE IT IS WRONG (PRD #35, ADR-35 D4). This is the line someone reaches for
    -- on noticing that a promoted wave can converge, so the reasoning lives here
    -- rather than in the ADR alone — an unexplained exclusion reads as an oversight
    -- and gets "fixed".
    --
    -- A PARKED RUN'S anthropic_secret_id NAMES THE CREDENTIAL IT SPENT, NOT THE ONE
    -- IT IS ABOUT TO SPEND. The park does not clear it, promotion does not clear it,
    -- and only the next claim's recordRunCredential overwrites it. So counting parked
    -- runs would pile phantom load onto the EXHAUSTED credential — the one the ranker
    -- is already avoiding for being empty — and would add exactly ZERO asymmetry
    -- between the candidates a promoted wave will actually converge on.
    --
    -- The general form, which is the part worth preserving: the in-flight bias works
    -- because it is PER-TOKEN ASYMMETRIC, and a run that has not yet chosen
    -- contributes no asymmetry. Parked load is structurally unrankable. No widening
    -- of this counter — by status, by retry_not_before, by anything — can spread a
    -- wave, because the information it would need does not exist until the run picks.
    -- The mechanism that DOES spread a wave is the jitter on retry_not_before
    -- (workersvc/limitwait.go); if convergence ever bites, that range is the knob.
    --
    -- 'awaiting_approval' counts for the opposite reason and the contrast is the
    -- point: that run's worker is holding its session and WILL resume on that same
    -- token, so its load is real and already directed.
    SELECT r.anthropic_secret_id AS sid, count(*) AS n
    FROM runs r
    WHERE r.user_id = @user_id
      AND r.anthropic_secret_id IS NOT NULL
      AND r.status IN ('claimed', 'running', 'awaiting_approval')
    GROUP BY r.anthropic_secret_id
) f ON f.sid = s.id
WHERE s.user_id = @user_id
  AND s.kind = 'anthropic_token'
ORDER BY s.id;

-- name: UserHasAnthropicToken :one
-- Whether the user holds an anthropic_token secret, for GET /api/me/rate-limits:
-- the handler derives `no_token` from this (secret-existence), not from the
-- rate_limits rows being absent. Deliberately NOT filtered on is_default — it
-- answers "does this user have any credential at all", which is the question
-- `no_token` asks. Never selects the ciphertext.
SELECT EXISTS (
    SELECT 1 FROM user_secrets WHERE user_id = $1 AND kind = 'anthropic_token'
);

-- name: DeleteRateLimits :execrows
-- Drop every gauge row a user holds. Since #104 M5 the composite FK CASCADES a
-- token's gauge row when the token itself is deleted, so this is no longer the
-- mechanism that prevents a ghost reading — the database is. It stays as the
-- belt-and-suspenders sweep the token-delete path still runs (PRD #53 D3b), and as
-- the thing that would still clear rows if a future schema change ever loosened
-- that cascade. Idempotent: 0 rows when there is nothing to drop.
DELETE FROM anthropic_rate_limits WHERE user_id = $1;
