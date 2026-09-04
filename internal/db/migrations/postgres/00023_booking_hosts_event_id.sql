-- +goose Up
-- Per-host external calendar event ID. Multi-host bookings (Group) create a
-- calendar event on each assigned host's calendar; we store each one here so it
-- can be moved/cancelled later. The primary host's id also lives in
-- bookings.external_event_id (kept for back-compat); this backfills its row.
ALTER TABLE booking_hosts ADD COLUMN external_event_id TEXT;

UPDATE booking_hosts
SET external_event_id = (
    SELECT b.external_event_id FROM bookings b WHERE b.id = booking_hosts.booking_id
)
WHERE is_primary = 1;

-- +goose Down
-- Deliberately a no-op, mirroring the SQLite migration: Postgres could drop the
-- column, but a down that lands on a different schema per engine is worse than a
-- down that lands on none.
