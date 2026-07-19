-- +goose Up

-- PRD #90: cross-run agent memory — a durable, per-(user, repo) store the lead
-- writes a bounded learning into and a FUTURE run on the same repo reads back as
-- inert, nonce-fenced, untrusted context. The store is deliberately keyed on
-- (user_id, repo_id): only your runs on a repo write that (you, repo) memory, and
-- only your future runs on that same repo read it — no cross-user, no cross-repo
-- bleed (Decision: scope = per-user + per-repo, smallest blast radius).
--
-- Identity is NEVER taken from the write request body. The API derives (user_id,
-- repo_id) from the run claim (runs.user_id / runs.repo_id, reached through the
-- runOwnedByWorker gate), because the worker's join token is not user-scoped — a
-- compromised worker must not be able to write another user's memory (Decision:
-- server-side identity derivation is mandatory).
--
-- FK delete rules: user_id / repo_id CASCADE (the memory dies with the account or
-- the repo — a disconnected repo's poisoned entries must not outlive it); run_id
-- SET NULL, so run pruning keeps the entry (provenance, not ownership — the entry
-- belongs to the (user, repo), not the one run that wrote it). run_id is therefore
-- nullable both for that SET NULL and because it is provenance-only.
--
-- Caps (per-entry size, per-(user,repo) count, per-run write count) are enforced
-- server-side in the API/store service, NOT as DDL constraints here: the count cap
-- is an oldest-eviction (a CHECK cannot evict) and the sizes are validated at the
-- handler alongside the same limits the SDK tool schema advertises, so a single Go
-- source of truth owns them rather than being split across a constraint and code.
CREATE TABLE agent_memory (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    repo_id    uuid NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    run_id     uuid REFERENCES runs(id) ON DELETE SET NULL,
    title      text NOT NULL,
    body       text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- The read/list/evict access path: every query filters (user_id, repo_id) and
-- orders newest-first, so this composite index serves the claim-time read, the
-- user list, and the oldest-eviction scan in one shape.
CREATE INDEX idx_agent_memory_user_repo_created ON agent_memory (user_id, repo_id, created_at DESC);

-- +goose Down
DROP TABLE agent_memory;
