-- +goose Up
-- See the Postgres half for why these two exist. Identical here: plain ADD COLUMN with a
-- '' default, so no table is rebuilt and every existing row keeps behaving as it did.
--
-- SQLite cannot run multi-tenant (config.Validate refuses MULTI_TENANT without a
-- postgres:// DSN), so on this engine the columns are only ever the empty fallback. They
-- exist here because the two migration directories keep one file per version, and because
-- the schema-comparison test asserts the engines agree on column sets.
ALTER TABLE server_settings ADD COLUMN embed_allowed_origins TEXT NOT NULL DEFAULT '';
ALTER TABLE server_settings ADD COLUMN stt_base_url TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE server_settings DROP COLUMN stt_base_url;
ALTER TABLE server_settings DROP COLUMN embed_allowed_origins;
