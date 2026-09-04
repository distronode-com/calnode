-- +goose Up
-- Member offboarding is archiving (soft-delete), not hard delete: the row and
-- all its links (past bookings, event types, team memberships) are preserved.
-- archived_at NULL = active; a timestamp = archived (login blocked, hidden from
-- default lists, skipped in routing).
ALTER TABLE users ADD COLUMN archived_at TEXT;

-- +goose Down
-- Deliberately a no-op, mirroring the SQLite migration: Postgres could drop the
-- column, but a down that lands on a different schema per engine is worse than a
-- down that lands on none.
