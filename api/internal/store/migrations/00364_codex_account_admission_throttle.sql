-- +goose Up
CREATE TABLE codex_account_admission_throttle (
    user_id uuid NOT NULL,
    provider_account_id uuid NOT NULL,
    rate_step integer NOT NULL DEFAULT 0 CHECK (rate_step BETWEEN 0 AND 30),
    rate_expiry timestamptz,
    rate_last_qualifying timestamptz,
    overload_step integer NOT NULL DEFAULT 0 CHECK (overload_step BETWEEN 0 AND 30),
    overload_expiry timestamptz,
    overload_last_qualifying timestamptz,
    observation_clock timestamptz,
    receipts jsonb NOT NULL DEFAULT '[]' CHECK (jsonb_typeof(receipts) = 'array' AND jsonb_array_length(receipts) <= 256),
    overload_votes jsonb NOT NULL DEFAULT '[]' CHECK (jsonb_typeof(overload_votes) = 'array' AND jsonb_array_length(overload_votes) <= 256),
    episode_id uuid,
    spread_width_ms integer NOT NULL DEFAULT 0 CHECK (spread_width_ms BETWEEN 0 AND 60000),
    effective_expiry timestamptz,
    recovery_floor timestamptz,
    PRIMARY KEY (user_id, provider_account_id),
    FOREIGN KEY (user_id, provider_account_id) REFERENCES codex_provider_account(user_id, id) ON DELETE CASCADE,
    CHECK (rate_expiry IS NULL OR isfinite(rate_expiry)),
    CHECK (rate_last_qualifying IS NULL OR isfinite(rate_last_qualifying)),
    CHECK (overload_last_qualifying IS NULL OR isfinite(overload_last_qualifying)),
    CHECK (overload_expiry IS NULL OR isfinite(overload_expiry)),
    CHECK (observation_clock IS NULL OR isfinite(observation_clock)),
    CHECK (effective_expiry IS NULL OR isfinite(effective_expiry)),
    CHECK (recovery_floor IS NULL OR isfinite(recovery_floor))
);

-- Pure policy: callers supply deterministic clock/jitter/seed only at the SQL seam.
-- Production uses a single captured server clock and server-generated random inputs.
-- Arrays bound every loop to 256 entries; overflow aborts the whole statement.
-- +goose StatementBegin
CREATE FUNCTION codex_admission_transition(
    p codex_account_admission_throttle, owner_id uuid, account_id uuid,
    run_id uuid, generation bigint, attempt_id uuid, signal_class text,
    received_at timestamptz, jitter double precision, seed uuid
) RETURNS codex_account_admission_throttle LANGUAGE plpgsql IMMUTABLE AS $$
DECLARE
    t timestamptz;
    receipt_key text := run_id::text || '/' || generation::text || '/' || attempt_id::text || '/' || signal_class;
    v jsonb;
    votes jsonb := '[]';
    pair_count integer := 0;
    qualifying boolean := false;
    active boolean;
    old_effective timestamptz;
    old_window_end timestamptz;
    duration_ms double precision;
    vote_tag text;
BEGIN
    IF received_at IS NULL OR jitter IS NULL OR seed IS NULL OR NOT isfinite(received_at) OR jitter < 0 OR jitter > 0.2 OR jitter = 'NaN'::float8
       OR signal_class NOT IN ('rate_limit', 'overload') OR generation <= 0 THEN
        RAISE EXCEPTION 'invalid admission observation' USING ERRCODE = '22023';
    END IF;
    IF p.user_id IS NULL THEN
        p.user_id := owner_id; p.provider_account_id := account_id;
        p.rate_step := 0; p.overload_step := 0;
        p.receipts := '[]'; p.overload_votes := '[]'; p.spread_width_ms := 0;
    END IF;
    -- Duplicate detection precedes pruning, clock movement and every policy transition.
    IF EXISTS (SELECT 1 FROM jsonb_array_elements(p.receipts) e WHERE e->>'k' = receipt_key) THEN
        RETURN p;
    END IF;
    t := greatest(received_at, p.observation_clock);
    SELECT coalesce(jsonb_agg(e), '[]') INTO p.receipts
      FROM jsonb_array_elements(p.receipts) e WHERE (e->>'t')::timestamptz > t - interval '60 seconds';
    IF jsonb_array_length(p.receipts) >= 256 THEN
        RAISE EXCEPTION 'admission capacity exceeded' USING ERRCODE = '54000';
    END IF;
    p.receipts := p.receipts || jsonb_build_array(jsonb_build_object('k', receipt_key, 't', t));
    old_effective := p.effective_expiry;
    old_window_end := old_effective + p.spread_width_ms * interval '1 millisecond';
    active := coalesce(t < p.overload_expiry, false);
    vote_tag := CASE WHEN active THEN p.episode_id::text ELSE NULL END;
    -- Expired episode votes become suppression tombstones, retaining their ORIGINAL time.
    FOR v IN SELECT value FROM jsonb_array_elements(p.overload_votes) LOOP
        IF (v->>'t')::timestamptz > t - interval '5 minutes' THEN
            IF NOT active AND v->>'episode' IS NOT NULL THEN
                v := v || '{"consumed":true}'::jsonb;
            END IF;
            votes := votes || jsonb_build_array(v);
        END IF;
    END LOOP;
    IF signal_class = 'overload' THEN
        IF NOT EXISTS (SELECT 1 FROM jsonb_array_elements(votes) e WHERE e->>'run' = run_id::text) THEN
            IF jsonb_array_length(votes) >= 256 THEN
                RAISE EXCEPTION 'admission capacity exceeded' USING ERRCODE = '54000';
            END IF;
            votes := votes || jsonb_build_array(jsonb_build_object(
                'run', run_id, 't', t, 'consumed', false, 'episode', vote_tag));
        END IF;
        SELECT count(*) INTO pair_count FROM jsonb_array_elements(votes) e
          WHERE NOT (e->>'consumed')::boolean AND (e->>'episode') IS NOT DISTINCT FROM vote_tag;
        qualifying := pair_count >= 2;
        IF qualifying THEN
            SELECT jsonb_agg(CASE WHEN NOT (e->>'consumed')::boolean
                AND (e->>'episode') IS NOT DISTINCT FROM vote_tag
                THEN e || '{"consumed":true}'::jsonb ELSE e END) INTO votes
              FROM jsonb_array_elements(votes) e;
            IF NOT active THEN
                p.overload_step := CASE WHEN t - p.overload_last_qualifying <= interval '15 minutes'
                    THEN least(p.overload_step + 1, 30) ELSE 0 END;
                duration_ms := least(15000.0 * power(2.0, p.overload_step), 300000.0) * (1 + jitter);
                p.overload_expiry := t + duration_ms * interval '1 millisecond';
            END IF;
            p.overload_last_qualifying := t;
        END IF;
    ELSE
        qualifying := true;
        IF p.rate_expiry IS NULL OR t >= p.rate_expiry THEN
            p.rate_step := CASE WHEN t - p.rate_last_qualifying <= interval '30 minutes'
                THEN least(p.rate_step + 1, 30) ELSE 0 END;
            duration_ms := least(30000.0 * power(2.0, p.rate_step), 900000.0) * (1 + jitter);
            p.rate_expiry := t + duration_ms * interval '1 millisecond';
        END IF;
        p.rate_last_qualifying := t;
    END IF;
    p.overload_votes := votes;
    p.observation_clock := t;
    p.effective_expiry := greatest(p.rate_expiry, p.overload_expiry, p.recovery_floor);
    IF qualifying AND (old_effective IS NULL OR t >= old_effective) AND p.effective_expiry > t THEN
        -- A new seed cannot pull any run ahead of the prior episode's spread.
        IF old_window_end > t THEN
            p.recovery_floor := greatest(p.recovery_floor, old_window_end);
            p.effective_expiry := greatest(p.effective_expiry, p.recovery_floor);
        END IF;
        p.episode_id := seed;
        p.spread_width_ms := floor(least(duration_ms, 60000))::integer;
    END IF;
    RETURN p;
END;
$$;
-- +goose StatementEnd

-- Stable digest over UUID bytes (not the server's version-dependent hashtext).
-- NULL means unrestricted; the returned finite deadline also includes recovery spread.
-- +goose StatementBegin
CREATE FUNCTION codex_admission_run_eligible_at(target_run uuid)
RETURNS timestamptz LANGUAGE sql STABLE AS $$
    SELECT a.effective_expiry + CASE WHEN a.spread_width_ms > 0 THEN
        (('x' || substr(md5(uuid_send(r.id) || uuid_send(a.episode_id)), 1, 15))::bit(60)::bigint
            % a.spread_width_ms) * interval '1 millisecond'
        ELSE interval '0' END
    FROM runs r
    JOIN codex_credential_state c ON c.user_secret_id = r.codex_secret_id AND c.user_id = r.user_id
    JOIN codex_account_admission_throttle a ON a.user_id = r.user_id AND a.provider_account_id = c.provider_account_id
    WHERE r.id = target_run AND r.harness = 'codex' AND r.codex_auth_mode = 'subscription';
$$;
-- +goose StatementEnd

-- +goose Down
DROP FUNCTION codex_admission_run_eligible_at(uuid);
DROP FUNCTION codex_admission_transition(codex_account_admission_throttle, uuid, uuid, uuid, bigint, uuid, text, timestamptz, double precision, uuid);
DROP TABLE codex_account_admission_throttle;
