-- name: RevokeVaultBrowserSlots :one
-- The users trigger deletes slots in the same statement; overflow aborts it.
UPDATE users SET vault_slot_generation = vault_slot_generation + 1
WHERE id = $1 RETURNING vault_slot_generation;

-- name: SetVaultAutoReunlock :exec
UPDATE users SET vault_auto_reunlock = @enabled WHERE id = @id;

-- name: GetVaultReunlockDays :one
SELECT COALESCE((SELECT value FROM app_settings WHERE key = 'vault_reunlock_days'), '7')::text AS days;

-- name: GetVaultBrowserSlot :one
SELECT * FROM vault_browser_slots WHERE id = @id AND user_id = @user_id;

-- name: PrepareVaultBrowserSlot :exec
-- Called only after the owner fence and users row lock. Remove expired and
-- same-browser rows before applying the cap; at most 31 others may remain.
DELETE FROM vault_browser_slots
WHERE user_id = @user_id AND (expires_at <= @observed_at OR browser_id = @browser_id);

-- name: TrimVaultBrowserSlots :exec
DELETE FROM vault_browser_slots WHERE id IN (
    SELECT s.id FROM vault_browser_slots s WHERE s.user_id = $1
    ORDER BY s.created_at DESC, s.id DESC OFFSET 31
);

-- name: InsertVaultBrowserSlot :exec
INSERT INTO vault_browser_slots
(id, user_id, browser_id, wrapped_dek, created_at, expires_at, token_version, vault_epoch, generation)
VALUES (@id, @user_id, @browser_id, @wrapped_dek, @created_at, @expires_at, @token_version, @vault_epoch, @generation);

-- name: DeleteExpiredVaultBrowserSlots :execrows
-- Bounded child-only sweep. Never acquire owner locks after these child locks.
DELETE FROM vault_browser_slots WHERE id IN (
    SELECT s.id FROM vault_browser_slots s WHERE s.expires_at <= @observed_at
    ORDER BY s.expires_at, s.id LIMIT LEAST(GREATEST(@batch_size::integer, 0), 1000)
    FOR UPDATE SKIP LOCKED
);
