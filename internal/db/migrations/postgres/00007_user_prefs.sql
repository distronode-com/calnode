-- +goose Up
ALTER TABLE users ADD COLUMN time_format TEXT NOT NULL DEFAULT '12h';
ALTER TABLE users ADD COLUMN week_start  INTEGER NOT NULL DEFAULT 1; -- 1=Monday, 0=Sunday
-- The two UPDATEs are kept in step with the SQLite migration; they are no-ops
-- here, because ADD COLUMN with a NOT NULL DEFAULT backfills every existing row.
UPDATE users SET time_format = '12h' WHERE time_format IS NULL;
UPDATE users SET week_start  = 1     WHERE week_start  IS NULL;

-- +goose Down
-- Deliberately a no-op, mirroring the SQLite migration: Postgres could drop the
-- column, but a down that lands on a different schema per engine is worse than a
-- down that lands on none.
