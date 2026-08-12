-- +goose Up
-- Persisted last-fire summary (PRD #308 M2, Decision 1). A single nullable jsonb column
-- holding the serialized outcome of the last SCHEDULED fire — matched/started/skipped
-- with typed skip reasons — so the schedules UI and CLI can show "what the last tick did"
-- without a history table (Decision 1: one last-fire column, not an append-only log). It
-- is written ONLY by AdvanceSchedule on the success/benign advance path; the park and
-- transient paths never touch it, so a parked/transient fire shows the PRIOR last_fire or
-- none (Decision 5). NULL = the schedule has never fired. RunNow does NOT persist here
-- (Decision 3), since it never reaches the advance path.
ALTER TABLE run_schedules ADD COLUMN last_fire jsonb;   -- NULL = never fired (PRD #308 M2)

-- +goose Down
ALTER TABLE run_schedules DROP COLUMN last_fire;
