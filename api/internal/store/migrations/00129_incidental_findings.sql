-- +goose Up

-- PRD #333: Incidental Findings — a worker mid-run flags an off-task bug without
-- blocking its turn; the user is told asynchronously and files (or dismisses) it on
-- their own schedule, with the same guardrail every forge write in uzi honours (the
-- human gates every filing; the worker never writes to the forge).
--
-- Two tables, mirroring the judge backlog family (Decision 1) rather than overloading
-- issue_proposals or collapsing into a single table:
--
--   findings              — PER-RUN EVIDENCE, one row per report. Two runs that find the
--                           same bug write two rows. Analogue of review_recommendations
--                           (00059). Deleting a run cascades its evidence away (D12).
--   finding_dispositions  — the COORDINATE-KEYED lifecycle, one row per
--                           (user_id, repo_id, location). Collapses the judge's two
--                           side-tables (recommendation_dispositions + recommendation_-
--                           filed_issues) into ONE linear status machine (D1).
--
-- The dedup lives in the STORAGE COORDINATE (D7), a deliberate divergence from the
-- judge's read-time rollup: the disposition survives even if its evidence rows are later
-- cascaded away with a deleted run, so a filed/dismissed coordinate stays resolved.

-- findings — per-run evidence. run_id CASCADEs (D12: evidence is per-run, mirrors
-- review_recommendations). user_id/repo_id also CASCADE — a finding belongs to the run's
-- owner and their repo, exactly like a proposal or a judge recommendation. location is
-- ALREADY CANONICALISED by the service before insert (D3: repo-root-relative, forward
-- slashes, leading ./ dropped, lowercased, whitespace-stripped, at most one symbol
-- token) — this table stores the canonical form and does not re-normalise. title /
-- description_md / labels / confidence are the agent-supplied, already-sanitised payload
-- (D4: the sanitisers run service-side before insert; the store holds inert text).
CREATE TABLE findings (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id         uuid NOT NULL REFERENCES runs ON DELETE CASCADE,
    user_id        uuid NOT NULL REFERENCES users ON DELETE CASCADE,
    repo_id        uuid NOT NULL REFERENCES repos ON DELETE CASCADE,
    location       text NOT NULL,
    title          text NOT NULL,
    description_md text NOT NULL,
    labels         jsonb NOT NULL DEFAULT '[]'::jsonb,
    confidence     text NOT NULL DEFAULT '',
    created_at     timestamptz NOT NULL DEFAULT now()
);

-- CountFindingsForRun (the per-run cap enforcement, M2) scans by run_id.
CREATE INDEX idx_findings_run ON findings (run_id);

-- The backlog LEFT JOIN (ListFindingsBacklog, D7) matches evidence to its disposition on
-- the full (user_id, repo_id, location) coordinate; this index serves that join.
CREATE INDEX idx_findings_coord ON findings (user_id, repo_id, location);

-- finding_dispositions — the cross-run coordinate lifecycle (D1). status walks a single
-- linear machine open → filing → filed, or open → dismissed; filing → open on a forge
-- failure (retryable), and filed/dismissed → open only when a materially-different
-- content_hash re-surfaces the coordinate (D3). filing_since is the transient claim
-- marker (mirrors recommendation_filed_issues.filing_since / issue_proposals.
-- confirming_since): the filing flow claims open → filing with a guarded UPDATE before
-- the forge CreateIssue, so of two concurrent files exactly one wins.
--
-- content_hash is the sha256 of the normalised title+description (D3, the judge's
-- rationale_hash idea reinstated): because the coordinate has no content discriminator,
-- two DIFFERENT bugs at the same file#symbol would otherwise collide and D6 suppression
-- would permanently hide the second — so a report whose coordinate is already
-- filed/dismissed but whose hash DIFFERS re-opens it, while an identical hash is
-- suppressed.
--
-- last_title is a snapshot updated on each report (D12): a filed coordinate must stay
-- legible in the backlog after its evidence rows are cascaded away with a deleted run,
-- and the backlog's LEFT JOIN then has no findings row to read a title from.
--
-- There is deliberately NO run FK here (D12): the disposition is a cross-run coordinate,
-- so it intentionally orphans if every producing run is ever deleted — desirable for
-- filed/dismissed (they must stay resolved). user_id/repo_id CASCADE like everywhere else.
--
-- The status/dismiss_reason invariant: a dismissal MUST carry a reason and every other
-- status MUST NOT — the table CHECK ties them so the enum cannot drift out of sync
-- (mirrors recommendation_dispositions).
CREATE TABLE finding_dispositions (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id         uuid NOT NULL REFERENCES users ON DELETE CASCADE,
    repo_id         uuid NOT NULL REFERENCES repos ON DELETE CASCADE,
    location        text NOT NULL,
    status          text NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'filing', 'filed', 'dismissed')),
    filed_issue_iid bigint,
    filing_since    timestamptz,
    dismiss_reason  text CHECK (dismiss_reason IN ('wont_do', 'not_an_issue')),
    -- A reason is present IFF the disposition is a dismissal.
    CHECK ((status = 'dismissed') = (dismiss_reason IS NOT NULL)),
    content_hash    text NOT NULL DEFAULT '',
    last_title      text NOT NULL DEFAULT '',
    created_at      timestamptz NOT NULL DEFAULT now(),
    resolved_at     timestamptz,
    -- The coordinate: one disposition per (user_id, repo_id, location). Serves the
    -- UpsertOpenDisposition ON CONFLICT, the coordinate-keyed guarded UPDATEs (claim /
    -- settle / revert / dismiss / re-open), and the backlog dedup. repo_id in the
    -- coordinate is the whole answer to "how do we differentiate repos" (D3): the same
    -- location under two repos is two distinct coordinates by construction.
    UNIQUE (user_id, repo_id, location)
);

-- +goose Down
DROP TABLE finding_dispositions;
DROP TABLE findings;
