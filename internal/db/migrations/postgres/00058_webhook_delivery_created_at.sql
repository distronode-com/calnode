-- +goose Up
-- webhook_deliveries had no timestamp of its own, so "the 50 most recent deliveries"
-- was expressed as ORDER BY rowid DESC. That is unportable — PostgreSQL has no rowid —
-- and it was never quite correct here either: SQLite's rowid tracks insertion order
-- only until something renumbers it, and VACUUM is allowed to.
--
-- The default is a constant empty string rather than a timestamp expression, matching
-- the SQLite half (whose ALTER TABLE ADD COLUMN forbids a parenthesised DEFAULT) so the
-- two engines backfill existing rows identically. New rows get their value bound by the
-- writer (internal/webhook). Rows that predate this migration keep '', which sorts last
-- under ORDER BY created_at DESC — correct, since they are the oldest deliveries on the
-- instance.
ALTER TABLE webhook_deliveries ADD COLUMN created_at TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE webhook_deliveries DROP COLUMN created_at;
