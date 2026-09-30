-- +goose Up

-- PRD #1906 M3: the per-run fetch credential, the api-side fetch admission and the source
-- log of a profile-bound research run (Decisions 3, 7 and 8).
--
-- runs.egress_profile_id binds a run to a named site list (egress_profiles, M1). It is set
-- at insert only (M8 wires the job create; until then nothing sets it in production) and
-- is IMMUTABLE afterwards (the trigger below): a run can never be re-pointed at another
-- list, or unbound, after it exists. ON DELETE RESTRICT, not SET NULL: a SET NULL would
-- silently turn a bound run into an unbound one, which claims with a forge credential and
-- the full tool set. An admin deleting a list that runs still reference gets a 409.
--
-- runs.egress_snapshot is the list's EFFECTIVE entries (egressprofile.EffectiveEntries)
-- plus its name, written once at the run's first claim: {"profile": name, "entries": [...]}.
-- The fetcher's Begin returns this snapshot, never the live list, so an admin edit does not
-- change a claimed run. The same trigger refuses to change it once set.
--
-- Two CHECKs keep a bound run off the lanes that cannot honour it: the Codex harness
-- (its web search is server-side, Decision 10) and chat runs (claimed on their own lane).
ALTER TABLE runs ADD COLUMN egress_profile_id uuid REFERENCES egress_profiles(id) ON DELETE RESTRICT;
ALTER TABLE runs ADD COLUMN egress_snapshot jsonb;
ALTER TABLE runs ADD CONSTRAINT runs_egress_profile_not_codex
    CHECK (egress_profile_id IS NULL OR harness IS DISTINCT FROM 'codex');
ALTER TABLE runs ADD CONSTRAINT runs_egress_profile_not_chat
    CHECK (egress_profile_id IS NULL OR kind <> 'chat');
CREATE INDEX idx_runs_egress_profile_id ON runs (egress_profile_id) WHERE egress_profile_id IS NOT NULL;

-- +goose StatementBegin
CREATE FUNCTION runs_egress_binding_immutable() RETURNS trigger AS $$
BEGIN
    IF OLD.egress_profile_id IS DISTINCT FROM NEW.egress_profile_id THEN
        RAISE EXCEPTION 'runs.egress_profile_id is immutable after insert (run %)', OLD.id
            USING ERRCODE = 'check_violation';
    END IF;
    IF OLD.egress_snapshot IS NOT NULL AND OLD.egress_snapshot IS DISTINCT FROM NEW.egress_snapshot THEN
        RAISE EXCEPTION 'runs.egress_snapshot is immutable once written (run %)', OLD.id
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER runs_egress_binding_immutable_trg
    BEFORE UPDATE OF egress_profile_id, egress_snapshot ON runs
    FOR EACH ROW
    EXECUTE FUNCTION runs_egress_binding_immutable();

-- One row per profile-bound run: the current fetch credential (sha256 of the uzf_ token
-- the claim delivered; the plaintext is never stored) and the run's fetch counters. The
-- counters are the single row every admission serializes on (Decision 3: per-replica
-- counters do not add up across fetcher replicas).
--
-- claim_generation is the runs.claim_generation the credential was minted under. Every
-- re-claim rotates the token, clears revoked_at and re-stamps the generation (UPSERT), so a
-- token from an earlier claim no longer matches any row, and a row whose generation is not
-- the run's current one (a re-claim whose assembly failed before minting) is refused.
--
-- The counters are RUN totals: a re-claim does not reset them. reserved_bytes is the sum of
-- the open reservations; used_bytes the bytes actually returned; files the admitted
-- attempts not reported refused; inflight the open reservations; attempts every admission
-- request made with a valid credential, including those refused at admission, so a loop of
-- refused requests is bounded too.
CREATE TABLE run_fetch_credentials (
    run_id uuid PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE,
    token_hash bytea NOT NULL UNIQUE CHECK (octet_length(token_hash) = 32),
    claim_generation bigint NOT NULL,
    revoked_at timestamptz,
    reserved_bytes bigint NOT NULL DEFAULT 0 CHECK (reserved_bytes >= 0),
    used_bytes bigint NOT NULL DEFAULT 0 CHECK (used_bytes >= 0),
    files bigint NOT NULL DEFAULT 0 CHECK (files >= 0),
    inflight bigint NOT NULL DEFAULT 0 CHECK (inflight >= 0),
    attempts bigint NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- One admitted fetch: the per-file maximum bytes, one file slot and one concurrency slot,
-- held until the fetcher's Complete settles it (or the stale sweep, or a re-claim, releases
-- it). settled means the bytes and the concurrency slot went back to the counters.
CREATE TABLE run_fetch_reservations (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id uuid NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    claim_generation bigint NOT NULL,
    bytes bigint NOT NULL CHECK (bytes > 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    settled boolean NOT NULL DEFAULT false,
    settled_at timestamptz
);
CREATE INDEX idx_run_fetch_reservations_run ON run_fetch_reservations (run_id);
CREATE INDEX idx_run_fetch_reservations_open ON run_fetch_reservations (created_at) WHERE NOT settled;

-- The source log (Decision 8): one row per attempt the fetcher reported, allowed or
-- refused. The run comes from the forwarded credential, never from the fetcher's body.
-- url, final_url, content_type and reason are site- or agent-controlled: the api escapes
-- control, format (bidi) and invalid bytes at write time (termsafe.EscapeBounded) and caps
-- their length, so the CHECKs below are only a floor. reservation_id is UNIQUE: a second
-- Complete for the same reservation writes nothing.
CREATE TABLE run_fetches (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id uuid NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    reservation_id uuid NOT NULL UNIQUE,
    url text NOT NULL CHECK (octet_length(url) <= 8192),
    final_url text NOT NULL DEFAULT '' CHECK (octet_length(final_url) <= 8192),
    verdict text NOT NULL CHECK (verdict IN ('allowed', 'refused')),
    reason text NOT NULL DEFAULT '' CHECK (octet_length(reason) <= 64),
    http_status integer NOT NULL DEFAULT 0 CHECK (http_status BETWEEN 0 AND 999),
    content_type text NOT NULL DEFAULT '' CHECK (octet_length(content_type) <= 512),
    bytes bigint NOT NULL DEFAULT 0 CHECK (bytes >= 0),
    sha256 text NOT NULL DEFAULT '' CHECK (sha256 = '' OR sha256 ~ '^[0-9a-f]{64}$'),
    started_at timestamptz NOT NULL,
    finished_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_run_fetches_run ON run_fetches (run_id, created_at, id);

-- Revocation. Every status write is an UPDATE of runs.status (the 00266 premise), so an
-- AFTER UPDATE OF status trigger sees every writer. The rule is "the run left the claimed
-- or running state": a terminal status (completed, failed, cancelled), a requeue (queued,
-- including a claim-assembly failure that requeues and SweepClaimedNeverStarted), any park
-- (limit_wait, pool_wait, recovery_wait, paused, awaiting_*). Once revoked, only the next
-- claim's mint un-revokes, and that mint also rotates the token. Begin requires 'running'
-- anyway, and this one rule covers every current and future park, requeue and terminal
-- writer without naming them.
--
-- Revoking early loses nothing ONLY because a bound run never comes back to running
-- without a new claim. Most parks resume through 'queued' and a re-claim, whose mint
-- issues a fresh credential. Three statuses resume IN PLACE instead (SetRunRunning takes
-- awaiting_approval, awaiting_input and awaiting_followup straight back to running, with
-- no claim and so no mint), which would leave the resumed run holding a revoked
-- credential and every fetch refused. The research runner never reports them today (it
-- reports running, completed and failed only), and runs_egress_profile_no_in_place_park
-- below makes that a schema rule rather than a property of one runner: a bound run
-- cannot enter those statuses at all, whoever writes them. A future in-place resume for a
-- bound run has to re-mint (or not revoke) first, and then relax that CHECK.
-- +goose StatementBegin
CREATE FUNCTION run_fetch_credential_revoke() RETURNS trigger AS $$
BEGIN
    UPDATE run_fetch_credentials
    SET revoked_at = COALESCE(revoked_at, now()), updated_at = now()
    WHERE run_id = NEW.id;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER run_fetch_credential_revoke_trg
    AFTER UPDATE OF status ON runs
    FOR EACH ROW
    WHEN (NEW.egress_profile_id IS NOT NULL
          AND OLD.status IS DISTINCT FROM NEW.status
          AND NEW.status NOT IN ('claimed', 'running'))
    EXECUTE FUNCTION run_fetch_credential_revoke();

ALTER TABLE runs ADD CONSTRAINT runs_egress_profile_no_in_place_park
    CHECK (egress_profile_id IS NULL
           OR status NOT IN ('awaiting_approval', 'awaiting_input', 'awaiting_followup'));

-- +goose Down
ALTER TABLE runs DROP CONSTRAINT runs_egress_profile_no_in_place_park;
DROP TRIGGER run_fetch_credential_revoke_trg ON runs;
DROP FUNCTION run_fetch_credential_revoke();
DROP TABLE run_fetches;
DROP TABLE run_fetch_reservations;
DROP TABLE run_fetch_credentials;
DROP TRIGGER runs_egress_binding_immutable_trg ON runs;
DROP FUNCTION runs_egress_binding_immutable();
DROP INDEX idx_runs_egress_profile_id;
ALTER TABLE runs DROP CONSTRAINT runs_egress_profile_not_chat;
ALTER TABLE runs DROP CONSTRAINT runs_egress_profile_not_codex;
ALTER TABLE runs DROP COLUMN egress_snapshot;
ALTER TABLE runs DROP COLUMN egress_profile_id;
