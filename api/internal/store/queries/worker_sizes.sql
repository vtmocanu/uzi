-- Admin-managed hosted-worker sizes (PRD #4213). Integers only: millicores, MiB, GiB.

-- name: ListWorkerSizes :many
SELECT * FROM worker_sizes ORDER BY display_order, name;

-- name: GetWorkerSize :one
SELECT * FROM worker_sizes WHERE name = @name;

-- name: InsertBuiltinWorkerSize :execrows
-- Idempotent seed used by the startup reconciler: insert a MISSING builtin only. An
-- existing row of the same name (builtin, edited, or an admin size that shadows the
-- builtin name) is left untouched (DO NOTHING). Applying a shipped value change to an
-- already-seeded row is RefreshPristineBuiltinWorkerSize, a separate statement.
INSERT INTO worker_sizes (name, cpu_request_millicores, cpu_limit_millicores, memory_request_mib, memory_limit_mib,
                          data_gib, dind_data_gib, display_order, builtin, customized)
VALUES (@name, @cpu_request_millicores, @cpu_limit_millicores, @memory_request_mib, @memory_limit_mib,
        @data_gib, @dind_data_gib, @display_order, true, false)
ON CONFLICT (name) DO NOTHING;

-- name: RefreshPristineBuiltinWorkerSize :execrows
-- Boot-time delivery of shipped values to PRISTINE builtin rows only (builtin AND NOT
-- customized): an admin-edited row and an admin size that shadows a builtin name are never
-- touched. The IS DISTINCT FROM guard makes an unchanged row a no-op, so updated_at does
-- not move on every boot.
UPDATE worker_sizes
SET cpu_request_millicores = @cpu_request_millicores,
    cpu_limit_millicores = @cpu_limit_millicores,
    memory_request_mib = @memory_request_mib,
    memory_limit_mib = @memory_limit_mib,
    data_gib = @data_gib,
    dind_data_gib = @dind_data_gib,
    display_order = @display_order,
    updated_at = now()
WHERE name = @name AND builtin AND NOT customized
  AND (cpu_request_millicores, cpu_limit_millicores, memory_request_mib, memory_limit_mib, data_gib, dind_data_gib, display_order)
      IS DISTINCT FROM (@cpu_request_millicores::bigint, @cpu_limit_millicores::bigint, @memory_request_mib::bigint,
                        @memory_limit_mib::bigint, @data_gib::bigint, @dind_data_gib::bigint, @display_order::int);

-- name: ResetBuiltinWorkerSize :execrows
-- Restore a builtin row to the shipped values and clear customized. Builtin rows only.
UPDATE worker_sizes
SET cpu_request_millicores = @cpu_request_millicores,
    cpu_limit_millicores = @cpu_limit_millicores,
    memory_request_mib = @memory_request_mib,
    memory_limit_mib = @memory_limit_mib,
    data_gib = @data_gib,
    dind_data_gib = @dind_data_gib,
    display_order = @display_order,
    customized = false,
    updated_by = NULL,
    updated_at = now()
WHERE name = @name AND builtin;

-- name: GetControllerAdminSizesAdvertisement :one
SELECT admin_sizes_version, admin_sizes_advertised_at, poll_interval_seconds
FROM controller_report_status WHERE id = 1;

-- name: LockWorkerSizeForUpdate :one
-- Re-read a size row inside the admin mutation transaction, after the table's advisory
-- lock is held. FOR UPDATE so a concurrent reset/refresh of the same row waits.
SELECT * FROM worker_sizes WHERE name = @name FOR UPDATE;

-- name: UpdateWorkerSizeValues :one
-- Admin edit of an EXISTING size (PRD #4213 M2). customized is computed by the caller:
-- true for a builtin whose values now differ from the shipped ones, false otherwise
-- (CHECK (builtin OR NOT customized) keeps admin-added rows at false). display_order and
-- builtin are never edited here.
UPDATE worker_sizes
SET cpu_request_millicores = @cpu_request_millicores,
    cpu_limit_millicores = @cpu_limit_millicores,
    memory_request_mib = @memory_request_mib,
    memory_limit_mib = @memory_limit_mib,
    data_gib = @data_gib,
    dind_data_gib = @dind_data_gib,
    customized = @customized::boolean,
    updated_by = sqlc.narg('updated_by'),
    updated_at = now()
WHERE name = @name
RETURNING *;
