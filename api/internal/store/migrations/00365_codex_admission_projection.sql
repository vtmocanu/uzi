-- +goose Up
-- Read-only queued admission, including the per-run recovery spread from M1.
CREATE VIEW codex_queued_run_admission AS
SELECT r.id AS run_id, r.user_id,
       (CASE WHEN a.rate_expiry IS NOT NULL
                   AND (a.overload_expiry IS NULL OR a.rate_expiry >= a.overload_expiry)
             THEN 'rate_limit' ELSE 'overload' END)::text AS codex_admission_class,
       codex_admission_run_eligible_at(r.id)::timestamptz AS codex_admission_eligible_at
FROM runs r
JOIN codex_credential_state c ON c.user_secret_id = r.codex_secret_id AND c.user_id = r.user_id
JOIN codex_account_admission_throttle a ON a.user_id = r.user_id AND a.provider_account_id = c.provider_account_id
WHERE r.status = 'queued'
  AND codex_admission_run_eligible_at(r.id) > now();

-- +goose Down
DROP VIEW codex_queued_run_admission;
