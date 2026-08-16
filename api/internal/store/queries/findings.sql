-- Incidental-findings queries (PRD #333). The FULL query set lands in M1 so no later
-- milestone regenerates the sqlc foundation (Parallelization plan, R10): the capture path
-- (M2), the notification coalescing (M3), the backlog read (M4) and the filing/dismissal
-- forge-write (M5) all draw from here. Two tables: `findings` is per-run evidence,
-- `finding_dispositions` is the coordinate-keyed (user_id, repo_id, location) lifecycle.
-- See 00129_incidental_findings.sql for the table rationale and decision references.
--
-- The coordinate for every disposition-keyed UPDATE below is the full triple
-- (user_id, repo_id, location); all three are passed as params, never derived from a
-- request body — the service resolves (user_id, repo_id) from the claimed run (D3).

-- name: InsertFinding :one
-- Record one piece of per-run evidence (M2). location/title/description_md/labels are
-- ALREADY canonicalised + sanitised by the service (D3/D4); this INSERT stores the inert
-- text verbatim. RETURNING * so the capture path can echo the created id back to the
-- worker (the emitted card acts on it) and compute nothing twice.
INSERT INTO findings (run_id, user_id, repo_id, location, title, description_md, labels, confidence)
VALUES (@run_id, @user_id, @repo_id, @location, @title, @description_md, @labels, @confidence)
RETURNING *;

-- name: CountFindingsForRun :one
-- The per-run capture cap (D11, MaxFindingsPerRun): the service counts before insert and
-- returns a soft error past the cap so a noisy run can't flood the backlog. Served by
-- idx_findings_run.
SELECT count(*) FROM findings WHERE run_id = @run_id;

-- name: UpsertOpenDisposition :one
-- Claim the coordinate as `open` on the FIRST report (M2). ON CONFLICT DO NOTHING is
-- load-bearing: it NEVER resurrects a filed/dismissed row and never disturbs an existing
-- open one — so a dismissed bug stays gone across runs (D6 suppression) and the re-open
-- of a materially-different finding is a SEPARATE guarded UPDATE (ReopenDispositionOn-
-- HashMismatch), not this insert. RETURNING * means an actual insert returns the new row
-- while a conflict returns pgx.ErrNoRows — that IS the did-I-insert signal the caller
-- reads (same shape as ClaimRecommendationFiledIssue). The UNIQUE(user_id, repo_id,
-- location) makes the insert itself atomic, so two concurrent first-reports converge on
-- one open row (D6).
INSERT INTO finding_dispositions (user_id, repo_id, location, status, content_hash, last_title)
VALUES (@user_id, @repo_id, @location, 'open', @content_hash, @last_title)
ON CONFLICT (user_id, repo_id, location) DO NOTHING
RETURNING *;

-- name: UpdateDispositionLastTitle :execrows
-- Keep an already-OPEN coordinate current (M2). UpsertOpenDisposition does DO NOTHING on
-- conflict, so a re-report of an open coordinate would otherwise leave last_title /
-- content_hash frozen at the first report; this refreshes them so the backlog shows the
-- latest title. Guarded to status='open' so it can never mutate a filed/dismissed row
-- (that path is the re-open UPDATE, which also clears the resolved state). rows-affected
-- distinguishes "refreshed an open row" from "coordinate is resolved, left untouched".
UPDATE finding_dispositions
SET last_title = @last_title,
    content_hash = @content_hash
WHERE user_id = @user_id AND repo_id = @repo_id AND location = @location
  AND status = 'open';

-- name: ReopenDispositionOnHashMismatch :execrows
-- Re-surface a RESOLVED coordinate whose content materially changed (D3). A report whose
-- coordinate is already filed/dismissed but whose content_hash DIFFERS re-opens it (new
-- `open`, so M3 re-notifies); an identical hash matches zero rows and stays suppressed
-- (D6). Clears the whole resolved state — dismiss_reason, resolved_at, filed_issue_iid,
-- filing_since — so the CHECK ((status='dismissed')=(reason IS NOT NULL)) holds (open ⇒
-- NULL reason) and a re-opened coordinate is cleanly fileable again. rows-affected (1 vs
-- 0) tells the caller whether it re-opened. The `content_hash IS DISTINCT FROM
-- @content_hash` guard is what makes an identical-hash re-report a no-op.
UPDATE finding_dispositions
SET status = 'open',
    dismiss_reason = NULL,
    content_hash = @content_hash,
    last_title = @last_title,
    resolved_at = NULL,
    filed_issue_iid = NULL,
    filing_since = NULL
WHERE user_id = @user_id AND repo_id = @repo_id AND location = @location
  AND status IN ('filed', 'dismissed')
  AND content_hash IS DISTINCT FROM @content_hash;

-- name: ClaimFindingForFiling :execrows
-- The claim-first two-phase forge write (M5, D4), open → filing. A guarded UPDATE, NOT an
-- INSERT-on-conflict: of two concurrent file requests on the same coordinate exactly one
-- moves the single row out of `open`, so exactly one wins and the loser (0 rows affected)
-- is a 409. filing_since is the transient claim marker; the forge CreateIssue happens
-- AFTER this returns 1, and SettleFindingFiled / RevertFindingFiling close the claim.
UPDATE finding_dispositions
SET status = 'filing',
    filing_since = now()
WHERE user_id = @user_id AND repo_id = @repo_id AND location = @location
  AND status = 'open';

-- name: SettleFindingFiled :execrows
-- Close a won claim after the forge issue is created (M5), filing → filed. Guarded to
-- status='filing' so only the claim-winner settles; stamps the forge iid, clears the
-- transient marker and records resolved_at. rows-affected=1 confirms the settle landed.
UPDATE finding_dispositions
SET status = 'filed',
    filed_issue_iid = @filed_issue_iid,
    filing_since = NULL,
    resolved_at = now()
WHERE user_id = @user_id AND repo_id = @repo_id AND location = @location
  AND status = 'filing';

-- name: RevertFindingFiling :execrows
-- Undo a claim whose forge CreateIssue FAILED (M5), filing → open, so the coordinate is
-- retryable. Guarded to status='filing' so it only ever reverts the in-flight claim,
-- never a settled filed row. Clears filing_since; leaves content_hash / last_title as they
-- were (the finding did not change, only the forge write failed).
UPDATE finding_dispositions
SET status = 'open',
    filing_since = NULL
WHERE user_id = @user_id AND repo_id = @repo_id AND location = @location
  AND status = 'filing';

-- name: SweepStrandedFilingFindings :execrows
-- Boot/interval reaper for filing claims stranded by a crash (M5 review, mirror of
-- review_issues.sql SweepStrandedRecommendationClaims). A FileFinding killed AFTER
-- ClaimFindingForFiling (status='filing', filing_since set) but before SettleFindingFiled /
-- RevertFindingFiling leaves the coordinate `filing` forever — ClaimFindingForFiling and
-- DismissFinding both guard status='open', so nothing else can ever move it. This resets it
-- to `open` (clearing filing_since) so the user can re-file. @cutoff MUST be clamped
-- >= 2x ForgeHTTPTimeout by the caller (cfg.IssueFilingStuckTimeout, config.go clamp) so a
-- slow-but-alive CreateIssue is never reset mid-flight.
--
-- ACCEPTED TRADEOFF, stated explicitly (the same one the judge reaper accepts): a coordinate
-- whose CreateIssue actually SUCCEEDED but whose settle failed is also reset to `open`, so a
-- later re-file creates a SECOND forge issue. That rare duplicate is chosen deliberately over
-- a permanent dead-end coordinate — the 2x-timeout clamp makes it require a crash between a
-- succeeded forge write and its settle, and re-opening a re-fileable coordinate is strictly
-- better than stranding it. Returns rows affected (for the sweeper log).
UPDATE finding_dispositions
SET status = 'open',
    filing_since = NULL
WHERE status = 'filing' AND filing_since IS NOT NULL AND filing_since < @cutoff;

-- name: DismissFinding :execrows
-- The user's triage: dismiss a coordinate with a reason (M5), open → dismissed. Guarded
-- to status='open' by design (documented): a coordinate mid-filing or already filed is not
-- dismissable — the user reverts/handles the filing first. The status/reason CHECK rejects
-- a NULL @dismiss_reason, so a reasonless dismissal errors rather than silently landing.
UPDATE finding_dispositions
SET status = 'dismissed',
    dismiss_reason = @dismiss_reason,
    resolved_at = now()
WHERE user_id = @user_id AND repo_id = @repo_id AND location = @location
  AND status = 'open';

-- name: GetIncidentalFinding :one
-- Fetch one evidence row by id, OWNER-SCOPED (M4/M5). The (id, user_id) match IS the
-- ownership check: another user's finding id resolves to zero rows, surfaced as a 404
-- exactly like an unknown id. The filing endpoint resolves the title/description to file
-- from THIS stored, already-sanitised row (D4), never from the request body.
SELECT * FROM findings WHERE id = @id AND user_id = @user_id;

-- name: CountOpenFindingsForUser :one
-- The open-findings count (D8) — the nav badge / meta count (M4/M8), a NEW count source
-- separate from the shared bell `unread`. Counts coordinates (dispositions), not evidence
-- rows, so a bug seen in N runs counts once. Optional ?repo= narrows it; NULL is all repos.
SELECT count(*) FROM finding_dispositions
WHERE user_id = @user_id
  AND status = 'open'
  AND (sqlc.narg('repo_id')::uuid IS NULL OR repo_id = sqlc.narg('repo_id')::uuid);

-- name: ListFindingsBacklog :many
-- The per-repo Findings backlog (D7, M4), DISPOSITION-DRIVEN so a filed/dismissed
-- coordinate survives even after its evidence rows are cascaded away with a deleted run.
-- FROM finding_dispositions LEFT JOIN findings on the full coordinate, GROUP BY the
-- disposition (its id, the PK, functionally determines every projected d.* column), so
-- each coordinate is ONE row regardless of how many evidence rows it has.
--
-- seen_in_runs = count(DISTINCT f.run_id): the "seen in N runs" occurrence count the
-- backlog UX depends on (R2). It reads 0 for a disposition whose evidence is all gone
-- (the LEFT JOIN yields NULL run_ids, which count(DISTINCT ...) skips) — and last_title
-- (a disposition snapshot, D12) is what keeps such a coordinate legible.
--
-- Bucketing is driven by @status (the service maps the UX bucket → status: to_file→'open',
-- filed→'filed', dismissed→'dismissed'); a NULL @status is the `all` bucket. ?repo= filters
-- by coordinate repo. ?run= is a SEMI-JOIN (EXISTS), not a WHERE on the joined evidence, so
-- anchoring to a run does NOT shrink seen_in_runs — a coordinate arrived at from a run's
-- notification still shows every run it recurs in (the judge run-anchor pattern).
--
-- repo_path (r.path_with_namespace via an INNER JOIN on repos): the coordinate's repo is
-- rendered/grouped-by everywhere a finding is shown (D3/D8), so the backlog carries the
-- human path, not just the id. repos.repo_id is an FK with ON DELETE CASCADE, so a live
-- disposition always has its repo — the inner JOIN cannot drop a row. It is in the GROUP BY
-- because it is a non-aggregated projection.
--
-- latest_finding_id: the id of the NEWEST evidence row at the coordinate — the actionable id
-- the web/CLI drive POST /findings/{id}/issue|dismiss on (M5). It MUST type NULLABLE: it is
-- NULL for a filed/dismissed coordinate whose evidence was cascaded away with a deleted run
-- (a display-only, non-actionable row) — which is fine, last_title keeps it legible (D12).
-- sqlc v1.30.0 infers a scalar subquery / derived-table column on findings.id (a NOT NULL
-- PK) as NON-null and emits a bare uuid.UUID that panics scanning that NULL; only a column
-- of a LEFT-JOINED BASE TABLE is inferred nullable. So `latest` is the findings table itself,
-- LEFT JOINed and constrained by a NOT EXISTS to the single newest evidence row per
-- coordinate: no evidence row is strictly greater than the max on (created_at, id) — a total
-- order because id is a unique PK — so exactly one (or, with no evidence, none) matches.
-- latest.id is constant per d.id, so it joins the GROUP BY unchanged.
SELECT
    d.user_id                        AS user_id,
    d.repo_id                        AS repo_id,
    r.path_with_namespace            AS repo_path,
    d.location                       AS location,
    d.status                         AS status,
    d.last_title                     AS last_title,
    d.filed_issue_iid                AS filed_issue_iid,
    d.resolved_at                    AS resolved_at,
    count(DISTINCT f.run_id)         AS seen_in_runs,
    latest.id                        AS latest_finding_id
FROM finding_dispositions d
JOIN repos r ON r.id = d.repo_id
LEFT JOIN findings f
    ON f.user_id = d.user_id AND f.repo_id = d.repo_id AND f.location = d.location
LEFT JOIN findings latest
    ON latest.user_id = d.user_id AND latest.repo_id = d.repo_id AND latest.location = d.location
    AND NOT EXISTS (
        SELECT 1 FROM findings f3
        WHERE f3.user_id = d.user_id AND f3.repo_id = d.repo_id AND f3.location = d.location
          AND (f3.created_at, f3.id) > (latest.created_at, latest.id)
    )
WHERE d.user_id = @user_id
  AND (sqlc.narg('status')::text IS NULL OR d.status = sqlc.narg('status')::text)
  AND (sqlc.narg('repo_id')::uuid IS NULL OR d.repo_id = sqlc.narg('repo_id')::uuid)
  AND (
      sqlc.narg('run_id')::uuid IS NULL
      OR EXISTS (
          SELECT 1 FROM findings f2
          WHERE f2.user_id = d.user_id
            AND f2.repo_id = d.repo_id
            AND f2.location = d.location
            AND f2.run_id = sqlc.narg('run_id')::uuid
      )
  )
GROUP BY d.id, r.path_with_namespace, latest.id
ORDER BY d.created_at DESC, d.id DESC;
