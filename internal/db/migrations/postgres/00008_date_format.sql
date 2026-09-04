-- +goose Up
ALTER TABLE users ADD COLUMN date_format TEXT NOT NULL DEFAULT 'dmy';
-- The UPDATE is kept in step with the SQLite migration; it is a no-op here,
-- because ADD COLUMN with a NOT NULL DEFAULT backfills every existing row.
UPDATE users SET date_format = 'dmy' WHERE date_format IS NULL;

-- +goose Down
-- Deliberately a no-op, mirroring the SQLite migration: Postgres could drop the
-- column, but a down that lands on a different schema per engine is worse than a
-- down that lands on none.
