-- +goose Up
-- Native GA4 / GTM: store just the ID; the booking page renders the official loader snippet
-- (no need to paste the whole script). Empty = that tag is off. Validated to the ID format on
-- write so the value is safe to interpolate into a script.
ALTER TABLE server_settings ADD COLUMN gtm_container_id  TEXT NOT NULL DEFAULT '';
ALTER TABLE server_settings ADD COLUMN ga4_measurement_id TEXT NOT NULL DEFAULT '';

-- +goose Down
-- Deliberately a no-op, mirroring the SQLite migration: Postgres could drop the
-- column, but a down that lands on a different schema per engine is worse than a
-- down that lands on none.
