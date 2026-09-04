-- +goose Up
-- Track who archived a member so restore can be gated: the owner can restore
-- anyone; an admin can restore only members they archived themselves.
ALTER TABLE users ADD COLUMN archived_by TEXT;

-- +goose Down
-- Deliberately a no-op, mirroring the SQLite migration: Postgres could drop the
-- column, but a down that lands on a different schema per engine is worse than a
-- down that lands on none.
