-- +goose Up
ALTER TABLE server_settings ADD COLUMN google_client_id         TEXT NOT NULL DEFAULT '';
ALTER TABLE server_settings ADD COLUMN google_client_secret_enc TEXT NOT NULL DEFAULT '';

-- +goose Down
-- Deliberately a no-op, mirroring the SQLite migration: Postgres could drop the
-- column, but a down that lands on a different schema per engine is worse than a
-- down that lands on none.
