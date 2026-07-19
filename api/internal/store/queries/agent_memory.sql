-- name: InsertAgentMemory :one
-- Write one memory entry. The caller supplies user_id/repo_id DERIVED FROM THE RUN
-- CLAIM (never the request body) plus the writing run_id as provenance. The
-- per-run write cap and the oldest-eviction that keep the (user,repo) set bounded
-- run around this insert in the store service.
INSERT INTO agent_memory (user_id, repo_id, run_id, title, body)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: ListAgentMemoryForUserRepo :many
-- The claim-time read + the worker's per-run list, scoped to one (user, repo),
-- newest first. id DESC is the deterministic tiebreak when two entries share a
-- created_at (sub-tick inserts within one run).
SELECT * FROM agent_memory
WHERE user_id = $1 AND repo_id = $2
ORDER BY created_at DESC, id DESC;

-- name: ListAgentMemoryForUser :many
-- The user-facing list across ALL of the caller's repos (web UI + `uzi memory
-- list`). Joins repos for the human-readable name; scoped to user_id only, newest
-- first. The JOIN (not LEFT JOIN) is safe: repo_id is NOT NULL and CASCADEs, so a
-- memory row never outlives its repo.
SELECT m.id, m.repo_id, r.path_with_namespace AS repo_name,
       m.title, m.body, m.run_id, m.created_at
FROM agent_memory m
JOIN repos r ON r.id = m.repo_id
WHERE m.user_id = $1
ORDER BY m.created_at DESC, m.id DESC;

-- name: CountAgentMemoryForRun :one
-- The per-run write-cap check: how many entries this run has already written. The
-- store service rejects the write when this is at the cap, bounding spam within a
-- single run.
SELECT count(*) FROM agent_memory WHERE run_id = $1;

-- name: EvictAgentMemoryOverCap :exec
-- The oldest-eviction that keeps at most the newest N (the count cap) for one
-- (user, repo). Run right after an insert: it deletes every entry beyond the
-- newest sqlc.arg(keep) for that (user, repo), so the set can never grow past the
-- cap. Ordering matches the read index (created_at DESC, id DESC) so "newest" is
-- the same set the reader would surface.
DELETE FROM agent_memory AS m
WHERE m.user_id = $1 AND m.repo_id = $2
  AND m.id NOT IN (
    SELECT keep.id FROM agent_memory AS keep
    WHERE keep.user_id = $1 AND keep.repo_id = $2
    ORDER BY keep.created_at DESC, keep.id DESC
    LIMIT sqlc.arg(keep_count)
  );

-- name: DeleteAgentMemory :execrows
-- Delete one of the CALLER'S entries. Owner-scoped by user_id, so a foreign or
-- unknown id touches zero rows (the handler maps that to 404) — never a cross-user
-- delete. Returns the affected-row count so the handler can tell not-found/not-
-- yours from a real delete.
DELETE FROM agent_memory WHERE id = $1 AND user_id = $2;
