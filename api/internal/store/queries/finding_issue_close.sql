-- Findings Filed→Done sync (PRD #1183 Child B, M3). Rides the existing poller tick: after a
-- repo's issue cache is refreshed, any finding coordinate whose filed issue has just been
-- observed closed moves to Done, exactly once, on the open→closed edge. Mirrors
-- judge_issue_close.sql (PRD #98 M6) — see that file for the edge-marker rationale.

-- name: ListFindingIssueCloseEdges :many
-- The pass's working set for one repo: SETTLED filed coordinates whose cached issue is closed
-- and whose open→closed EDGE has not yet been consumed.
--
-- THE JOIN KEY IS (repo_id, forge_issue_iid), never forge_issue_iid alone. A forge issue iid is
-- per-project, not global — `issues` is keyed ON CONFLICT (repo_id, forge_issue_iid) — so
-- joining on iid alone would let closing issue #7 in repo X auto-Done a finding filed as #7 into
-- repo Y (cross-repo, and since findings are owner-scoped, possibly cross-user). fd.repo_id is
-- NOT NULL, so the equality carries no NULL-repo hazard the judge query must dodge.
--
-- The other filters, each load-bearing:
--   * fd.status = 'filed' — only SETTLED coordinates (a mid-filing claim is not filed).
--   * fd.filed_issue_iid IS NOT NULL — a settled row always has one; the guard keeps a NULL from
--     matching some other row's iid and also keeps the partial index predicate exact.
--   * fd.close_synced_at IS NULL — the EDGE. Without it the pass is level-triggered and re-fires
--     every tick while the issue stays closed, re-applying after a human Undo.
--   * i.state = 'closed' — the cached snapshot. A reopen is NOT handled here on purpose:
--     close_synced_at stays stamped, so a flapping issue cannot ping-pong the backlog.
--
-- Projects the disposition id (what ApplyFindingIssueCloseEdge keys on) and filed_issue_iid
-- (logging only). Ordered by fd.id for a stable batch; the partial index idx_finding_dispositions_
-- close_pending is exactly this working set.
SELECT
    fd.id              AS id,
    fd.filed_issue_iid AS filed_issue_iid
FROM finding_dispositions fd
JOIN issues i
    ON i.repo_id = fd.repo_id
   AND i.forge_issue_iid = fd.filed_issue_iid
WHERE fd.repo_id = @repo_id
  AND fd.status = 'filed'
  AND fd.filed_issue_iid IS NOT NULL
  AND fd.close_synced_at IS NULL
  AND i.state = 'closed'
ORDER BY fd.id ASC;

-- name: ApplyFindingIssueCloseEdge :execrows
-- Apply ONE close edge: write the automatic Done and consume the edge in a single guarded
-- statement. The guard `status = 'filed' AND close_synced_at IS NULL` is the whole correctness
-- story — it never overwrites a coordinate a human already moved (a dismissed/open/done row is
-- not 'filed'), and two concurrent pollers cannot both consume one edge (the second sees
-- close_synced_at already stamped). Provenance is fixed in the query text — status 'done',
-- set_via 'issue_close' — so no call site can attribute a system action to a person. rows-affected
-- is 1 on a real apply, 0 when the guard already failed (raced or human-superseded).
UPDATE finding_dispositions
SET status = 'done',
    set_via = 'issue_close',
    resolved_at = now(),
    close_synced_at = now()
WHERE id = @id
  AND status = 'filed'
  AND close_synced_at IS NULL;
