-- +goose Up
ALTER TABLE availability_overrides ADD COLUMN reason TEXT NOT NULL DEFAULT 'day_off';
-- Back-fill existing rows: custom hours rows get 'custom_hours', unavailable rows keep 'day_off'.
UPDATE availability_overrides SET reason = 'custom_hours' WHERE is_available = 1;
UPDATE availability_overrides SET reason = 'day_off'      WHERE is_available = 0;

-- +goose Down
-- Deliberately a no-op, mirroring the SQLite migration: Postgres could drop the
-- column, but a down that lands on a different schema per engine is worse than a
-- down that lands on none.
