-- +goose Up
-- Timestamps in Calnode are TEXT and are compared lexicographically on purpose:
-- the job queue claims with `run_at <= ?`, the recordings consent window is
-- `decided_at BETWEEN`, booking overlap is `start_at`/`end_at` against bound
-- strings, sessions and tokens expire on `expires_at > ?`, and several lists are
-- `ORDER BY created_at`. On SQLite that comparison is a byte comparison, always.
-- On PostgreSQL it is a comparison under the column's collation, which by default
-- is the database's — en_US.utf8 on a typical installation, i.e. a linguistic
-- collation that ignores punctuation and spaces at the primary level and orders
-- case at the tertiary one.
--
-- The schema stores two timestamp layouts on purpose (internal/dbtime: a
-- space-separated `2026-01-01 10:00:00` and a `2026-01-01T10:00:00.000Z`), and a
-- linguistic collation makes no promise about how those two shapes interleave,
-- nor about a shape some future writer or import adds. Pinning the columns to
-- COLLATE "C" makes the ordering byte ordering on both engines, so a predicate
-- proved correct on SQLite means the same thing on PostgreSQL, and it does so in
-- the schema rather than in 40-odd queries that would each have to remember a
-- COLLATE clause.
--
-- Scope: every TEXT column the tree compares or orders as a time. That is every
-- column named *_at plus jobs.locked_until, and also the four HH:MM
-- availability columns and availability_overrides.date, which are ordered as
-- times too (`ORDER BY day_of_week, start_time`, `ORDER BY date`). 54 columns
-- across 27 tables. internal/db/collation_test.go enumerates the migrated schema
-- and fails if a matching column is not C, so a timestamp column added later
-- cannot quietly miss this.
--
-- Cost: ALTER COLUMN TYPE takes ACCESS EXCLUSIVE on the table and rebuilds its
-- indexes. Calnode instances are small and this runs once, at the startup that
-- picks the migration up, but it is not a zero-downtime change on a large
-- database. Grouped one statement per table so each is rewritten once.
--
-- COLLATE "C" is a deterministic collation, so equality keeps comparing bytes and
-- every existing unique index (booking_manage_tokens' hashed PK, the partial
-- idx_bookings_no_double) keeps its exact current meaning.

ALTER TABLE api_keys
    ALTER COLUMN created_at TYPE TEXT COLLATE "C",
    ALTER COLUMN last_used_at TYPE TEXT COLLATE "C";
ALTER TABLE availability_overrides
    ALTER COLUMN date TYPE TEXT COLLATE "C",
    ALTER COLUMN end_time TYPE TEXT COLLATE "C",
    ALTER COLUMN start_time TYPE TEXT COLLATE "C";
ALTER TABLE availability_rules
    ALTER COLUMN end_time TYPE TEXT COLLATE "C",
    ALTER COLUMN start_time TYPE TEXT COLLATE "C";
ALTER TABLE booking_manage_tokens
    ALTER COLUMN created_at TYPE TEXT COLLATE "C",
    ALTER COLUMN expires_at TYPE TEXT COLLATE "C";
ALTER TABLE bookings
    ALTER COLUMN created_at TYPE TEXT COLLATE "C",
    ALTER COLUMN end_at TYPE TEXT COLLATE "C",
    ALTER COLUMN start_at TYPE TEXT COLLATE "C",
    ALTER COLUMN updated_at TYPE TEXT COLLATE "C";
ALTER TABLE calendar_connections
    ALTER COLUMN channel_expires_at TYPE TEXT COLLATE "C",
    ALTER COLUMN created_at TYPE TEXT COLLATE "C",
    ALTER COLUMN expiry_at TYPE TEXT COLLATE "C";
ALTER TABLE connection_calendars
    ALTER COLUMN created_at TYPE TEXT COLLATE "C";
ALTER TABLE crypto_keystore
    ALTER COLUMN created_at TYPE TEXT COLLATE "C",
    ALTER COLUMN updated_at TYPE TEXT COLLATE "C";
ALTER TABLE event_types
    ALTER COLUMN archived_at TYPE TEXT COLLATE "C",
    ALTER COLUMN created_at TYPE TEXT COLLATE "C";
ALTER TABLE idempotency_keys
    ALTER COLUMN created_at TYPE TEXT COLLATE "C";
ALTER TABLE invite_tokens
    ALTER COLUMN expires_at TYPE TEXT COLLATE "C",
    ALTER COLUMN used_at TYPE TEXT COLLATE "C";
ALTER TABLE jobs
    ALTER COLUMN created_at TYPE TEXT COLLATE "C",
    ALTER COLUMN locked_until TYPE TEXT COLLATE "C",
    ALTER COLUMN run_at TYPE TEXT COLLATE "C";
ALTER TABLE magic_link_tokens
    ALTER COLUMN created_at TYPE TEXT COLLATE "C",
    ALTER COLUMN expires_at TYPE TEXT COLLATE "C",
    ALTER COLUMN used_at TYPE TEXT COLLATE "C";
ALTER TABLE meeting_consents
    ALTER COLUMN decided_at TYPE TEXT COLLATE "C";
ALTER TABLE notes
    ALTER COLUMN created_at TYPE TEXT COLLATE "C",
    ALTER COLUMN updated_at TYPE TEXT COLLATE "C";
ALTER TABLE oauth_access_tokens
    ALTER COLUMN created_at TYPE TEXT COLLATE "C",
    ALTER COLUMN expires_at TYPE TEXT COLLATE "C",
    ALTER COLUMN last_used_at TYPE TEXT COLLATE "C";
ALTER TABLE oauth_auth_codes
    ALTER COLUMN created_at TYPE TEXT COLLATE "C",
    ALTER COLUMN expires_at TYPE TEXT COLLATE "C";
ALTER TABLE oauth_clients
    ALTER COLUMN created_at TYPE TEXT COLLATE "C";
ALTER TABLE recordings
    ALTER COLUMN created_at TYPE TEXT COLLATE "C",
    ALTER COLUMN updated_at TYPE TEXT COLLATE "C";
ALTER TABLE server_settings
    ALTER COLUMN updated_at TYPE TEXT COLLATE "C";
ALTER TABLE sessions
    ALTER COLUMN created_at TYPE TEXT COLLATE "C",
    ALTER COLUMN expires_at TYPE TEXT COLLATE "C";
ALTER TABLE teams
    ALTER COLUMN created_at TYPE TEXT COLLATE "C";
ALTER TABLE transcripts
    ALTER COLUMN created_at TYPE TEXT COLLATE "C",
    ALTER COLUMN updated_at TYPE TEXT COLLATE "C";
ALTER TABLE users
    ALTER COLUMN archived_at TYPE TEXT COLLATE "C",
    ALTER COLUMN created_at TYPE TEXT COLLATE "C";
ALTER TABLE webhook_deliveries
    ALTER COLUMN created_at TYPE TEXT COLLATE "C",
    ALTER COLUMN last_attempted_at TYPE TEXT COLLATE "C";
ALTER TABLE webhooks
    ALTER COLUMN created_at TYPE TEXT COLLATE "C";
ALTER TABLE zoom_connections
    ALTER COLUMN created_at TYPE TEXT COLLATE "C",
    ALTER COLUMN expiry_at TYPE TEXT COLLATE "C";

-- +goose Down
-- Back to the database default collation. pg_catalog."default" is the spelling for
-- "whatever the database was created with"; there is no way to say "unset".
ALTER TABLE api_keys
    ALTER COLUMN created_at TYPE TEXT COLLATE pg_catalog."default",
    ALTER COLUMN last_used_at TYPE TEXT COLLATE pg_catalog."default";
ALTER TABLE availability_overrides
    ALTER COLUMN date TYPE TEXT COLLATE pg_catalog."default",
    ALTER COLUMN end_time TYPE TEXT COLLATE pg_catalog."default",
    ALTER COLUMN start_time TYPE TEXT COLLATE pg_catalog."default";
ALTER TABLE availability_rules
    ALTER COLUMN end_time TYPE TEXT COLLATE pg_catalog."default",
    ALTER COLUMN start_time TYPE TEXT COLLATE pg_catalog."default";
ALTER TABLE booking_manage_tokens
    ALTER COLUMN created_at TYPE TEXT COLLATE pg_catalog."default",
    ALTER COLUMN expires_at TYPE TEXT COLLATE pg_catalog."default";
ALTER TABLE bookings
    ALTER COLUMN created_at TYPE TEXT COLLATE pg_catalog."default",
    ALTER COLUMN end_at TYPE TEXT COLLATE pg_catalog."default",
    ALTER COLUMN start_at TYPE TEXT COLLATE pg_catalog."default",
    ALTER COLUMN updated_at TYPE TEXT COLLATE pg_catalog."default";
ALTER TABLE calendar_connections
    ALTER COLUMN channel_expires_at TYPE TEXT COLLATE pg_catalog."default",
    ALTER COLUMN created_at TYPE TEXT COLLATE pg_catalog."default",
    ALTER COLUMN expiry_at TYPE TEXT COLLATE pg_catalog."default";
ALTER TABLE connection_calendars
    ALTER COLUMN created_at TYPE TEXT COLLATE pg_catalog."default";
ALTER TABLE crypto_keystore
    ALTER COLUMN created_at TYPE TEXT COLLATE pg_catalog."default",
    ALTER COLUMN updated_at TYPE TEXT COLLATE pg_catalog."default";
ALTER TABLE event_types
    ALTER COLUMN archived_at TYPE TEXT COLLATE pg_catalog."default",
    ALTER COLUMN created_at TYPE TEXT COLLATE pg_catalog."default";
ALTER TABLE idempotency_keys
    ALTER COLUMN created_at TYPE TEXT COLLATE pg_catalog."default";
ALTER TABLE invite_tokens
    ALTER COLUMN expires_at TYPE TEXT COLLATE pg_catalog."default",
    ALTER COLUMN used_at TYPE TEXT COLLATE pg_catalog."default";
ALTER TABLE jobs
    ALTER COLUMN created_at TYPE TEXT COLLATE pg_catalog."default",
    ALTER COLUMN locked_until TYPE TEXT COLLATE pg_catalog."default",
    ALTER COLUMN run_at TYPE TEXT COLLATE pg_catalog."default";
ALTER TABLE magic_link_tokens
    ALTER COLUMN created_at TYPE TEXT COLLATE pg_catalog."default",
    ALTER COLUMN expires_at TYPE TEXT COLLATE pg_catalog."default",
    ALTER COLUMN used_at TYPE TEXT COLLATE pg_catalog."default";
ALTER TABLE meeting_consents
    ALTER COLUMN decided_at TYPE TEXT COLLATE pg_catalog."default";
ALTER TABLE notes
    ALTER COLUMN created_at TYPE TEXT COLLATE pg_catalog."default",
    ALTER COLUMN updated_at TYPE TEXT COLLATE pg_catalog."default";
ALTER TABLE oauth_access_tokens
    ALTER COLUMN created_at TYPE TEXT COLLATE pg_catalog."default",
    ALTER COLUMN expires_at TYPE TEXT COLLATE pg_catalog."default",
    ALTER COLUMN last_used_at TYPE TEXT COLLATE pg_catalog."default";
ALTER TABLE oauth_auth_codes
    ALTER COLUMN created_at TYPE TEXT COLLATE pg_catalog."default",
    ALTER COLUMN expires_at TYPE TEXT COLLATE pg_catalog."default";
ALTER TABLE oauth_clients
    ALTER COLUMN created_at TYPE TEXT COLLATE pg_catalog."default";
ALTER TABLE recordings
    ALTER COLUMN created_at TYPE TEXT COLLATE pg_catalog."default",
    ALTER COLUMN updated_at TYPE TEXT COLLATE pg_catalog."default";
ALTER TABLE server_settings
    ALTER COLUMN updated_at TYPE TEXT COLLATE pg_catalog."default";
ALTER TABLE sessions
    ALTER COLUMN created_at TYPE TEXT COLLATE pg_catalog."default",
    ALTER COLUMN expires_at TYPE TEXT COLLATE pg_catalog."default";
ALTER TABLE teams
    ALTER COLUMN created_at TYPE TEXT COLLATE pg_catalog."default";
ALTER TABLE transcripts
    ALTER COLUMN created_at TYPE TEXT COLLATE pg_catalog."default",
    ALTER COLUMN updated_at TYPE TEXT COLLATE pg_catalog."default";
ALTER TABLE users
    ALTER COLUMN archived_at TYPE TEXT COLLATE pg_catalog."default",
    ALTER COLUMN created_at TYPE TEXT COLLATE pg_catalog."default";
ALTER TABLE webhook_deliveries
    ALTER COLUMN created_at TYPE TEXT COLLATE pg_catalog."default",
    ALTER COLUMN last_attempted_at TYPE TEXT COLLATE pg_catalog."default";
ALTER TABLE webhooks
    ALTER COLUMN created_at TYPE TEXT COLLATE pg_catalog."default";
ALTER TABLE zoom_connections
    ALTER COLUMN created_at TYPE TEXT COLLATE pg_catalog."default",
    ALTER COLUMN expiry_at TYPE TEXT COLLATE pg_catalog."default";
