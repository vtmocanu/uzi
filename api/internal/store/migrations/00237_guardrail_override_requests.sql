-- +goose Up

-- PRD #1432 M1: the guardrail-override-REQUEST seam. A member who cannot enable a
-- repo (its default-branch guardrail refuses, PRD #66) records a request here for an
-- admin to approve or reject, instead of self-allowing (the R6 route-around D8
-- forbids). This table is the DATA MODEL only — no gate reads it in M1.
--
-- Three notes on the columns, each a deliberate choice:
--
--   * findings jsonb is AUDIT/DISPLAY-ONLY. It snapshots the privcheck.Finding set
--     that blocked the enable at request time, so the admin sees WHY the member was
--     refused without a fresh forge read. It is never re-evaluated against and never
--     drives a gate; the authoritative block decision is always recomputed live.
--
--   * decided_by is a HISTORICAL AUDIT note, ON DELETE SET NULL — deleting the
--     deciding admin's user row nulls the attribution but keeps the decided request.
--     This is DISTINCT from the authoritative LIVE override actor, which stays
--     repos.guardrail_override_by with ON DELETE RESTRICT (see 00121): the live
--     override must not lose its actor, whereas a settled request is history that can
--     outlive the account. requested_by is ON DELETE CASCADE (a deleted requester's
--     pending/decided requests go with them, like the run/connection rows they own).
--
--   * the partial unique index caps ONE open (status = 'pending') request per repo,
--     so a member cannot pile up duplicate open requests and the admin queue holds at
--     most one row per repo. A decided (approved/rejected) request drops out of the
--     index, so the next refusal can open a fresh one — the arbiter that
--     UpsertGuardrailOverrideRequest's ON CONFLICT targets.
CREATE TABLE guardrail_override_requests (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    repo_id       uuid NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    requested_by  uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    reason        text NOT NULL,
    findings      jsonb NOT NULL DEFAULT '[]'::jsonb,
    status        text NOT NULL DEFAULT 'pending',
    created_at    timestamptz NOT NULL DEFAULT now(),
    decided_by    uuid REFERENCES users(id) ON DELETE SET NULL,
    decided_at    timestamptz,
    decision_note text,
    CONSTRAINT guardrail_override_requests_status_check CHECK (status IN ('pending','approved','rejected'))
);
CREATE UNIQUE INDEX guardrail_override_requests_one_pending
    ON guardrail_override_requests (repo_id) WHERE status = 'pending';

-- +goose Down
DROP TABLE guardrail_override_requests;
