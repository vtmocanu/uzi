-- +goose Up
SET LOCAL lock_timeout = '5s';
-- Both tables only reference users, so SHARE ROW EXCLUSIVE on it is the strongest lock this
-- migration takes; take it up front, before any CREATE TABLE ... REFERENCES.
LOCK TABLE users IN SHARE ROW EXCLUSIVE MODE;
-- Issue #4692 (provider usage-limit history, M1): raw five-minute samples of every accepted
-- provider gauge reading. The tables are history, not gauges: the poller (or the test-connection
-- handler) copies a reading in AFTER its gauge write succeeded, best effort, in one autocommit
-- INSERT ... SELECT ... ON CONFLICT DO NOTHING (queries/provider_usage_history.sql). A missing
-- bucket means "no accepted copy", never zero use. Goose numbers are draft until merge; renumber
-- above the live head on landing.
--
-- bucket_start is the five-minute bin of observed_at, computed by the database from a fixed
-- origin (date_bin on timestamptz with a timestamptz origin is IMMUTABLE, so a STORED generated
-- column can sit in the primary key and arbitrate ON CONFLICT). The first accepted copy in a
-- bucket wins.
--
-- user_id is the only foreign key: deleting the owner cascades the history away. The identity
-- column (user_secret_id / provider_account_id) deliberately has NO foreign key, so deleting a
-- credential leaves its history in place. No age-based expiry exists; no other index either,
-- because the primary key serves every access this issue makes (it only writes).
CREATE TABLE anthropic_usage_samples (
    user_id             uuid        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    user_secret_id      uuid        NOT NULL,
    observed_at         timestamptz NOT NULL,
    bucket_start        timestamptz GENERATED ALWAYS AS
        (date_bin('5 minutes', observed_at, TIMESTAMPTZ '2000-01-01 00:00:00+00')) STORED,
    enablement_rev      bigint      NOT NULL,
    source              text        NOT NULL CHECK (source IN ('usage_endpoint', 'header_probe')),
    five_hour_pct       smallint    CHECK (five_hour_pct BETWEEN 0 AND 100),
    five_hour_resets_at timestamptz,
    seven_day_pct       smallint    NOT NULL CHECK (seven_day_pct BETWEEN 0 AND 100),
    seven_day_resets_at timestamptz,
    created_at          timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, user_secret_id, bucket_start)
);

-- buckets is the projection the copy query builds from the gauge's reading (ids, flags and
-- window percent / length / reset only), bounded to 4 KiB by the query and by this CHECK.
CREATE TABLE codex_usage_samples (
    user_id                      uuid        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    provider_account_id          uuid        NOT NULL,
    observed_at                  timestamptz NOT NULL,
    bucket_start                 timestamptz GENERATED ALWAYS AS
        (date_bin('5 minutes', observed_at, TIMESTAMPTZ '2000-01-01 00:00:00+00')) STORED,
    observed_generation          bigint      NOT NULL,
    observed_credential_revision bigint      NOT NULL,
    buckets                      jsonb       NOT NULL
        CHECK (jsonb_typeof(buckets) = 'array' AND octet_length(buckets::text) <= 4096),
    created_at                   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, provider_account_id, bucket_start)
);

-- A sample's owner and identity never change. Nothing updates these rows (the copy is
-- ON CONFLICT DO NOTHING); the trigger makes a future mistake loud instead of silent.
-- +goose StatementBegin
CREATE FUNCTION provider_usage_samples_immutable_owner() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.user_id IS DISTINCT FROM OLD.user_id THEN
        RAISE EXCEPTION 'provider usage sample owner is immutable' USING ERRCODE = 'check_violation';
    END IF;
    IF TG_TABLE_NAME = 'anthropic_usage_samples' THEN
        IF NEW.user_secret_id IS DISTINCT FROM OLD.user_secret_id THEN
            RAISE EXCEPTION 'provider usage sample identity is immutable' USING ERRCODE = 'check_violation';
        END IF;
    ELSIF NEW.provider_account_id IS DISTINCT FROM OLD.provider_account_id THEN
        RAISE EXCEPTION 'provider usage sample identity is immutable' USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER anthropic_usage_samples_immutable_trg BEFORE UPDATE ON anthropic_usage_samples
FOR EACH ROW EXECUTE FUNCTION provider_usage_samples_immutable_owner();
CREATE TRIGGER codex_usage_samples_immutable_trg BEFORE UPDATE ON codex_usage_samples
FOR EACH ROW EXECUTE FUNCTION provider_usage_samples_immutable_owner();

-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP TABLE codex_usage_samples;
DROP TABLE anthropic_usage_samples;
DROP FUNCTION provider_usage_samples_immutable_owner();
