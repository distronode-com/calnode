-- +goose Up
--
-- The SQLite half of multi-tenant mode. SQLite CANNOT run multi-tenant —
-- config.Validate refuses MULTI_TENANT without a postgres:// DSN, because the
-- isolation guarantee is PostgreSQL row-level security and SQLite has no
-- equivalent. What this file does is keep the two schemas the same shape, so the
-- cross-engine comparison test stays meaningful and so no Go call site has to
-- ask which engine it is on before naming workspace_id.
--
-- Three deliberate differences from the Postgres file, all forced by SQLite:
--
--  1. No REFERENCES workspaces(id). SQLite rejects
--     `ADD COLUMN ... NOT NULL DEFAULT 'default' REFERENCES ws(id)` outright —
--     "Cannot add a REFERENCES column with non-NULL default value (1)", measured
--     on modernc.org/sqlite with foreign_keys=ON. Rebuilding all 32 tables to
--     get the constraint would be a large, risky migration whose only payoff is
--     ON DELETE CASCADE for a workspace delete, and workspace deletes only
--     happen in multi-tenant mode, i.e. never here. The two tables rebuilt below
--     omit it too, so the engine is consistent with itself rather than being
--     half-constrained.
--  2. No row-level security, and no policies. There is nothing to express them
--     with.
--  3. Only the four uniqueness changes that need the constraint MOVED are made.
--     users(email), event_types(slug), teams(slug) and server_settings' id = 1
--     singleton stay exactly as they are: with one workspace, a global unique
--     and a (workspace_id, x) unique admit precisely the same rows, and a
--     table rebuild of users or event_types to prove that would be four
--     hundred lines of risk for no behaviour change.

CREATE TABLE workspaces (
    id          TEXT PRIMARY KEY,
    slug        TEXT NOT NULL UNIQUE,
    public_host TEXT NOT NULL UNIQUE,
    region      TEXT NOT NULL DEFAULT '',
    status      TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'suspended')),
    created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    updated_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

-- public_host is empty on purpose: no HTTP request carries an empty Host, so the
-- default workspace is unreachable by host resolution.
INSERT INTO workspaces (id, slug, public_host, region, status)
VALUES ('default', 'default', '', '', 'active');

-- ── The column (D1) ───────────────────────────────────────────────────────────
-- A literal 'default' rather than a session setting: SQLite has no
-- current_setting, and with one workspace there is nothing to resolve.

ALTER TABLE api_keys ADD COLUMN workspace_id TEXT NOT NULL DEFAULT 'default';
ALTER TABLE availability_overrides ADD COLUMN workspace_id TEXT NOT NULL DEFAULT 'default';
ALTER TABLE availability_rules ADD COLUMN workspace_id TEXT NOT NULL DEFAULT 'default';
ALTER TABLE booking_answers ADD COLUMN workspace_id TEXT NOT NULL DEFAULT 'default';
ALTER TABLE booking_attendees ADD COLUMN workspace_id TEXT NOT NULL DEFAULT 'default';
ALTER TABLE booking_hosts ADD COLUMN workspace_id TEXT NOT NULL DEFAULT 'default';
ALTER TABLE booking_manage_tokens ADD COLUMN workspace_id TEXT NOT NULL DEFAULT 'default';
ALTER TABLE bookings ADD COLUMN workspace_id TEXT NOT NULL DEFAULT 'default';
ALTER TABLE calendar_connections ADD COLUMN workspace_id TEXT NOT NULL DEFAULT 'default';
ALTER TABLE connection_calendars ADD COLUMN workspace_id TEXT NOT NULL DEFAULT 'default';
ALTER TABLE event_type_hosts ADD COLUMN workspace_id TEXT NOT NULL DEFAULT 'default';
ALTER TABLE event_type_questions ADD COLUMN workspace_id TEXT NOT NULL DEFAULT 'default';
ALTER TABLE event_type_reminders ADD COLUMN workspace_id TEXT NOT NULL DEFAULT 'default';
ALTER TABLE event_types ADD COLUMN workspace_id TEXT NOT NULL DEFAULT 'default';
ALTER TABLE invite_tokens ADD COLUMN workspace_id TEXT NOT NULL DEFAULT 'default';
ALTER TABLE jobs ADD COLUMN workspace_id TEXT NOT NULL DEFAULT 'default';
ALTER TABLE magic_link_tokens ADD COLUMN workspace_id TEXT NOT NULL DEFAULT 'default';
ALTER TABLE notes ADD COLUMN workspace_id TEXT NOT NULL DEFAULT 'default';
ALTER TABLE oauth_access_tokens ADD COLUMN workspace_id TEXT NOT NULL DEFAULT 'default';
ALTER TABLE oauth_auth_codes ADD COLUMN workspace_id TEXT NOT NULL DEFAULT 'default';
ALTER TABLE recordings ADD COLUMN workspace_id TEXT NOT NULL DEFAULT 'default';
ALTER TABLE server_settings ADD COLUMN workspace_id TEXT NOT NULL DEFAULT 'default';
ALTER TABLE sessions ADD COLUMN workspace_id TEXT NOT NULL DEFAULT 'default';
ALTER TABLE team_members ADD COLUMN workspace_id TEXT NOT NULL DEFAULT 'default';
ALTER TABLE teams ADD COLUMN workspace_id TEXT NOT NULL DEFAULT 'default';
ALTER TABLE transcripts ADD COLUMN workspace_id TEXT NOT NULL DEFAULT 'default';
ALTER TABLE users ADD COLUMN workspace_id TEXT NOT NULL DEFAULT 'default';
ALTER TABLE webhook_deliveries ADD COLUMN workspace_id TEXT NOT NULL DEFAULT 'default';
ALTER TABLE webhooks ADD COLUMN workspace_id TEXT NOT NULL DEFAULT 'default';
ALTER TABLE zoom_connections ADD COLUMN workspace_id TEXT NOT NULL DEFAULT 'default';

-- idempotency_keys and meeting_consents get the column through their rebuild
-- below, because their PRIMARY KEY is what has to change and SQLite cannot ALTER
-- a table constraint.

-- ── idempotency_keys: PK (idempotency_key) → (workspace_id, idempotency_key) ──
CREATE TABLE idempotency_keys_new (
    workspace_id    TEXT NOT NULL DEFAULT 'default',
    idempotency_key TEXT NOT NULL,
    request_hash    TEXT NOT NULL,
    status_code     INTEGER,        -- NULL while the original request is in flight
    response_body   TEXT,
    booking_id      TEXT,
    created_at      TEXT NOT NULL,
    PRIMARY KEY (workspace_id, idempotency_key)
);
INSERT INTO idempotency_keys_new (idempotency_key, request_hash, status_code, response_body, booking_id, created_at)
    SELECT idempotency_key, request_hash, status_code, response_body, booking_id, created_at FROM idempotency_keys;
DROP TABLE idempotency_keys;
ALTER TABLE idempotency_keys_new RENAME TO idempotency_keys;

-- ── meeting_consents: PK (room, participant_identity) gains workspace_id ──────
CREATE TABLE meeting_consents_new (
    workspace_id         TEXT NOT NULL DEFAULT 'default',
    room                 TEXT NOT NULL,
    participant_identity TEXT NOT NULL,
    name                 TEXT NOT NULL DEFAULT '',
    decision             TEXT NOT NULL DEFAULT 'continue',  -- continue | leave
    decided_at           TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    PRIMARY KEY (workspace_id, room, participant_identity)
);
INSERT INTO meeting_consents_new (room, participant_identity, name, decision, decided_at)
    SELECT room, participant_identity, name, decision, decided_at FROM meeting_consents;
DROP TABLE meeting_consents;
ALTER TABLE meeting_consents_new RENAME TO meeting_consents;

-- ── The two unique indexes, which need no rebuild ─────────────────────────────
DROP INDEX ux_jobs_type_payload;
CREATE UNIQUE INDEX ux_jobs_type_payload ON jobs (workspace_id, type, payload);

DROP INDEX idx_notes_booking;
CREATE UNIQUE INDEX idx_notes_booking ON notes (workspace_id, booking_id);

-- ── The partial indexes on bookings and jobs ──────────────────────────────────
DROP INDEX idx_bookings_no_double;
CREATE UNIQUE INDEX idx_bookings_no_double ON bookings (workspace_id, host_id, start_at)
    WHERE status != 'cancelled';

DROP INDEX idx_bookings_host_time;
CREATE INDEX idx_bookings_host_time ON bookings (workspace_id, host_id, start_at, end_at)
    WHERE status = 'confirmed';

DROP INDEX idx_jobs_pending;
CREATE INDEX idx_jobs_pending ON jobs (workspace_id, run_at) WHERE status = 'pending';

DROP INDEX idx_jobs_running_expired;
CREATE INDEX idx_jobs_running_expired ON jobs (workspace_id, locked_until) WHERE status = 'running';

-- The worker claims and reaps across tenants on the platform handle, ordered by
-- run_at / locked_until globally, which a workspace-leading index cannot serve.
CREATE INDEX idx_jobs_pending_global ON jobs (run_at) WHERE status = 'pending';
CREATE INDEX idx_jobs_running_expired_global ON jobs (locked_until) WHERE status = 'running';

-- +goose Down
DROP INDEX IF EXISTS idx_jobs_running_expired_global;
DROP INDEX IF EXISTS idx_jobs_pending_global;

DROP INDEX idx_jobs_running_expired;
CREATE INDEX idx_jobs_running_expired ON jobs (locked_until) WHERE status = 'running';

DROP INDEX idx_jobs_pending;
CREATE INDEX idx_jobs_pending ON jobs (run_at) WHERE status = 'pending';

DROP INDEX idx_bookings_host_time;
CREATE INDEX idx_bookings_host_time ON bookings (host_id, start_at, end_at) WHERE status = 'confirmed';

DROP INDEX idx_bookings_no_double;
CREATE UNIQUE INDEX idx_bookings_no_double ON bookings (host_id, start_at) WHERE status != 'cancelled';

DROP INDEX idx_notes_booking;
CREATE UNIQUE INDEX idx_notes_booking ON notes (booking_id);

DROP INDEX ux_jobs_type_payload;
CREATE UNIQUE INDEX ux_jobs_type_payload ON jobs (type, payload);

CREATE TABLE meeting_consents_old (
    room                 TEXT NOT NULL,
    participant_identity TEXT NOT NULL,
    name                 TEXT NOT NULL DEFAULT '',
    decision             TEXT NOT NULL DEFAULT 'continue',
    decided_at           TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    PRIMARY KEY (room, participant_identity)
);
INSERT INTO meeting_consents_old (room, participant_identity, name, decision, decided_at)
    SELECT room, participant_identity, name, decision, decided_at FROM meeting_consents;
DROP TABLE meeting_consents;
ALTER TABLE meeting_consents_old RENAME TO meeting_consents;

CREATE TABLE idempotency_keys_old (
    idempotency_key TEXT PRIMARY KEY,
    request_hash    TEXT NOT NULL,
    status_code     INTEGER,
    response_body   TEXT,
    booking_id      TEXT,
    created_at      TEXT NOT NULL
);
INSERT INTO idempotency_keys_old (idempotency_key, request_hash, status_code, response_body, booking_id, created_at)
    SELECT idempotency_key, request_hash, status_code, response_body, booking_id, created_at FROM idempotency_keys;
DROP TABLE idempotency_keys;
ALTER TABLE idempotency_keys_old RENAME TO idempotency_keys;

ALTER TABLE zoom_connections DROP COLUMN workspace_id;
ALTER TABLE webhooks DROP COLUMN workspace_id;
ALTER TABLE webhook_deliveries DROP COLUMN workspace_id;
ALTER TABLE users DROP COLUMN workspace_id;
ALTER TABLE transcripts DROP COLUMN workspace_id;
ALTER TABLE teams DROP COLUMN workspace_id;
ALTER TABLE team_members DROP COLUMN workspace_id;
ALTER TABLE sessions DROP COLUMN workspace_id;
ALTER TABLE server_settings DROP COLUMN workspace_id;
ALTER TABLE recordings DROP COLUMN workspace_id;
ALTER TABLE oauth_auth_codes DROP COLUMN workspace_id;
ALTER TABLE oauth_access_tokens DROP COLUMN workspace_id;
ALTER TABLE notes DROP COLUMN workspace_id;
ALTER TABLE magic_link_tokens DROP COLUMN workspace_id;
ALTER TABLE jobs DROP COLUMN workspace_id;
ALTER TABLE invite_tokens DROP COLUMN workspace_id;
ALTER TABLE event_types DROP COLUMN workspace_id;
ALTER TABLE event_type_reminders DROP COLUMN workspace_id;
ALTER TABLE event_type_questions DROP COLUMN workspace_id;
ALTER TABLE event_type_hosts DROP COLUMN workspace_id;
ALTER TABLE connection_calendars DROP COLUMN workspace_id;
ALTER TABLE calendar_connections DROP COLUMN workspace_id;
ALTER TABLE bookings DROP COLUMN workspace_id;
ALTER TABLE booking_manage_tokens DROP COLUMN workspace_id;
ALTER TABLE booking_hosts DROP COLUMN workspace_id;
ALTER TABLE booking_attendees DROP COLUMN workspace_id;
ALTER TABLE booking_answers DROP COLUMN workspace_id;
ALTER TABLE availability_rules DROP COLUMN workspace_id;
ALTER TABLE availability_overrides DROP COLUMN workspace_id;
ALTER TABLE api_keys DROP COLUMN workspace_id;

DROP TABLE workspaces;
