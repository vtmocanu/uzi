-- Board preferences (PRD #196 M3) ------------------------------------------

-- name: GetBoardPrefs :one
-- The board view preferences for (user, repo). No ownership join: the owner read
-- path (repoForRequest) has already authorized the repo. Absent ⇒ no rows ⇒ the
-- handler returns the unset defaults ({extra_labels: null, show_all: false}).
SELECT * FROM board_prefs WHERE user_id = @user_id AND repo_id = @repo_id;

-- name: UpsertBoardPrefsForOwner :one
-- Owner-only write: inserts/updates the row ONLY when repo_id belongs to a
-- connection owned by user_id (the SELECT yields a row only then), so a non-owner
-- or unknown id writes nothing → no rows → 404 in the handler (defense-in-depth
-- mirroring UpsertRepoToolProfileForOwner). extra_labels is nullable jsonb: NULL is
-- the "not customized" sentinel, a JSON array is the user's absolute set.
INSERT INTO board_prefs (user_id, repo_id, extra_labels, show_all)
SELECT @user_id, r.id, @extra_labels, @show_all
FROM repos r
JOIN forge_connections c ON c.id = r.connection_id
WHERE r.id = @repo_id AND c.user_id = @user_id
ON CONFLICT (user_id, repo_id) DO UPDATE
    SET extra_labels = EXCLUDED.extra_labels,
        show_all     = EXCLUDED.show_all,
        updated_at   = now()
RETURNING *;
