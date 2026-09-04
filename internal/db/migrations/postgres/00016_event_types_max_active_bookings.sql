-- +goose Up
-- Cap how many active (upcoming, non-cancelled) bookings a single invitee may
-- hold for an event type, keyed by their email. 1 = one at a time (default);
-- 0 = unlimited. Existing rows adopt the default of 1.
ALTER TABLE event_types ADD COLUMN max_active_bookings INTEGER NOT NULL DEFAULT 1;

-- +goose Down
-- Deliberately a no-op, mirroring the SQLite migration: Postgres could drop the
-- column, but a down that lands on a different schema per engine is worse than a
-- down that lands on none.
