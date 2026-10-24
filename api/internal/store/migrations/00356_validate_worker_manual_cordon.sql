-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE workers VALIDATE CONSTRAINT workers_manual_cordon_coherent;

-- +goose Down
SELECT 1;
