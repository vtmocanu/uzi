-- +goose Up

-- A user's outbound-only agent worker (the uzi-agent container). It never
-- accepts inbound connections; it authenticates every call with a join token
-- whose sha256 is stored here (the plaintext token is shown once at issuance —
-- a net-new mechanism, no reusable precedent in PRD #1). status is
-- heartbeat-derived (offline|online); "busy" is NOT stored — it is derived at
-- read time from an active claimed/running/awaiting_approval run.
CREATE TABLE workers (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id           uuid NOT NULL REFERENCES users ON DELETE CASCADE,
    name              text NOT NULL,
    token_hash        bytea NOT NULL UNIQUE,       -- sha256(join token)
    status            text NOT NULL DEFAULT 'offline' CHECK (status IN ('offline', 'online')),
    last_heartbeat_at timestamptz,
    version           text,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX idx_workers_user ON workers (user_id);

-- One runs row is the unit of work; an issue can accumulate several runs over
-- its life. The issue title/description are snapshotted at queue time so the run
-- is self-contained: PRD #2's reconcile can evict the issues cache row between
-- queue and claim, and the run must still be buildable. status walks the state
-- machine queued → claimed → running → awaiting_approval → running → completed |
-- failed | cancelled.
CREATE TABLE runs (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id           uuid NOT NULL REFERENCES users ON DELETE CASCADE,
    repo_id           uuid NOT NULL REFERENCES repos ON DELETE CASCADE,   -- repos.id is uuid (PRD #2)
    issue_iid         bigint NOT NULL,                                    -- forge iid, snapshotted below
    issue_title       text NOT NULL,
    issue_description text NOT NULL,
    status            text NOT NULL DEFAULT 'queued'
        CHECK (status IN ('queued', 'claimed', 'running', 'awaiting_approval', 'completed', 'failed', 'cancelled')),
    requeue_count     int NOT NULL DEFAULT 0,      -- worker-death re-queues; capped by RUN_MAX_REQUEUES
    worker_id         uuid REFERENCES workers ON DELETE SET NULL,   -- affinity: a re-queued run prefers its prior worker
    session_id        text,                        -- SDK session for resume
    last_seq          int NOT NULL DEFAULT 0,      -- high-water mark of run_messages.seq; returned in claim
    branch            text,
    mr_iid            bigint,
    failure_reason    text,
    plan_md           text,                        -- captured plan awaiting approval
    iteration_count   int NOT NULL DEFAULT 0,      -- implement⇄review loop counter (cap: RUN_MAX_ITERATIONS)
    claimed_at        timestamptz,
    started_at        timestamptz,
    finished_at       timestamptz,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX idx_runs_user ON runs (user_id);
CREATE INDEX idx_runs_repo ON runs (repo_id);
CREATE INDEX idx_runs_worker ON runs (worker_id);
-- Claim scan: oldest claimable run for a user.
CREATE INDEX idx_runs_claimable ON runs (user_id, status, created_at);

-- One non-terminal run per issue: fixes bottega's check-then-insert TOCTOU race
-- with a DB constraint. Terminal runs are excluded so an issue can be re-run
-- once its prior run finishes.
CREATE UNIQUE INDEX uq_runs_one_active_per_issue
    ON runs (repo_id, issue_iid)
    WHERE status NOT IN ('completed', 'failed', 'cancelled');

-- Seq-numbered, persisted message log. Reconnecting browsers replay from a
-- last-seen seq, so the live stream is never lossy (fixes multica's lossy
-- buffer). UNIQUE(run_id, seq) makes the worker's batched append idempotent.
CREATE TABLE run_messages (
    id         bigserial PRIMARY KEY,
    run_id     uuid NOT NULL REFERENCES runs ON DELETE CASCADE,
    seq        int NOT NULL,             -- per-run, gapless
    kind       text NOT NULL,            -- text|thinking|tool_use|tool_result|status|error|user_message|plan
    agent      text,                     -- which (sub)agent produced it (lead|coder|reviewer|…)
    payload    jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (run_id, seq)
);

-- Steering channel: follow-ups + plan verdicts + cancel, consumed FIFO by the
-- worker's GET /inputs poll (or transitioned server-side when no poller is live).
CREATE TABLE run_user_inputs (
    id          bigserial PRIMARY KEY,
    run_id      uuid NOT NULL REFERENCES runs ON DELETE CASCADE,
    kind        text NOT NULL CHECK (kind IN ('follow_up', 'approve_plan', 'reject_plan', 'cancel')),
    body        text,
    consumed_at timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now()
);

-- FIFO consume scan: unconsumed inputs for a run, oldest first.
CREATE INDEX idx_run_user_inputs_pending ON run_user_inputs (run_id, id) WHERE consumed_at IS NULL;

-- +goose Down
DROP TABLE run_user_inputs;
DROP TABLE run_messages;
DROP TABLE runs;
DROP TABLE workers;
