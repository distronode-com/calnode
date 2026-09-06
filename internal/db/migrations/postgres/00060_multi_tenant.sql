-- +goose Up
--
-- Multi-tenant mode: `workspaces` is the tenant root, every application table
-- gains a `workspace_id`, and a row-level-security policy per table makes the
-- isolation the database's job rather than the query author's.
--
-- ┌── The table list ────────────────────────────────────────────────────────┐
-- │ 32 TENANT tables (column + FK + policy), in the order they appear below: │
-- │   api_keys, availability_overrides, availability_rules, booking_answers, │
-- │   booking_attendees, booking_hosts, booking_manage_tokens, bookings,     │
-- │   calendar_connections, connection_calendars, event_type_hosts,          │
-- │   event_type_questions, event_type_reminders, event_types,               │
-- │   idempotency_keys, invite_tokens, jobs, magic_link_tokens,              │
-- │   meeting_consents, notes, oauth_access_tokens, oauth_auth_codes,        │
-- │   recordings, server_settings, sessions, team_members, teams,            │
-- │   transcripts, users, webhook_deliveries, webhooks, zoom_connections     │
-- │                                                                          │
-- │ 4 EXEMPT tables (no workspace_id, no policy):                            │
-- │   workspaces          — the tenant root itself                          │
-- │   crypto_keystore     — one DEK per process (ARCHITECTURE §5)           │
-- │   goose_db_version    — migration bookkeeping                           │
-- │   oauth_clients       — dynamic client registration is per client APP,   │
-- │                         not per tenant: one Claude/ChatGPT connector     │
-- │                         registration serves every workspace it is        │
-- │                         authorised against.                              │
-- └──────────────────────────────────────────────────────────────────────────┘
--
-- The same two lists live in Go as db.TenantTables / db.ExemptTables, and
-- TestPostgres_tenantTablesMatchSchema fails if either drifts from what this
-- file produced — so a table added by a later migration has to be classified,
-- and cannot be silently left unprotected.
--
-- ⚠️ ENABLE / FORCE ROW LEVEL SECURITY is deliberately NOT here. It is applied
-- by db.EnableRLS at boot, and only when MULTI_TENANT is set. The reason is
-- measured, not stylistic: FORCE makes the policy apply to the table OWNER too,
-- and in single-tenant mode DATABASE_URL *is* the owner. A schema migrated with
-- FORCE and no `app.workspace_id` binding returns 0 rows to its owner for every
-- SELECT — verified against PostgreSQL 17.11 with a NOBYPASSRLS owner role. A
-- superuser DSN hides it completely (superusers bypass RLS), which is exactly
-- the kind of green that proves nothing. A policy on a table whose RLS is not
-- enabled is inert, also verified, so single-tenant behaviour is unchanged.

CREATE TABLE workspaces (
    id          TEXT PRIMARY KEY,
    slug        TEXT NOT NULL UNIQUE,
    public_host TEXT NOT NULL UNIQUE,
    region      TEXT NOT NULL DEFAULT '',
    status      TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'suspended')),
    created_at  TEXT NOT NULL DEFAULT to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"') COLLATE "C",
    updated_at  TEXT NOT NULL DEFAULT to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"') COLLATE "C"
);

-- The single-tenant workspace. It has to exist before the ALTERs below, because
-- each one backfills existing rows with 'default' and then checks the new
-- foreign key. public_host is empty on purpose: no HTTP request can carry an
-- empty Host, so the default workspace is unreachable by host resolution and a
-- multi-tenant instance cannot accidentally route to it.
INSERT INTO workspaces (id, slug, public_host, region, status)
VALUES ('default', 'default', '', '', 'active');

-- ── The column (D1) ───────────────────────────────────────────────────────────
--
-- COALESCE around current_setting, rather than the bare current_setting the
-- design note spells: the bare two-argument-less form RAISES on an unset
-- parameter, so it would fail every INSERT in single-tenant mode. With the
-- missing_ok form plus COALESCE the column defaults to 'default' when nothing is
-- bound, which is what single-tenant mode wants, and is never reached in
-- multi-tenant mode because the handle binds the parameter before every
-- statement. It also fails CLOSED if a multi-tenant statement ever escapes that
-- binding: the row would be written as 'default' and the policy's WITH CHECK
-- compares it against an unset parameter (NULL), which is not true, so the
-- INSERT is refused with SQLSTATE 42501 rather than landing in the wrong tenant.

ALTER TABLE api_keys              ADD COLUMN workspace_id TEXT NOT NULL DEFAULT COALESCE(current_setting('app.workspace_id', true), 'default') REFERENCES workspaces(id) ON DELETE CASCADE;
ALTER TABLE availability_overrides ADD COLUMN workspace_id TEXT NOT NULL DEFAULT COALESCE(current_setting('app.workspace_id', true), 'default') REFERENCES workspaces(id) ON DELETE CASCADE;
ALTER TABLE availability_rules    ADD COLUMN workspace_id TEXT NOT NULL DEFAULT COALESCE(current_setting('app.workspace_id', true), 'default') REFERENCES workspaces(id) ON DELETE CASCADE;
ALTER TABLE booking_answers       ADD COLUMN workspace_id TEXT NOT NULL DEFAULT COALESCE(current_setting('app.workspace_id', true), 'default') REFERENCES workspaces(id) ON DELETE CASCADE;
ALTER TABLE booking_attendees     ADD COLUMN workspace_id TEXT NOT NULL DEFAULT COALESCE(current_setting('app.workspace_id', true), 'default') REFERENCES workspaces(id) ON DELETE CASCADE;
ALTER TABLE booking_hosts         ADD COLUMN workspace_id TEXT NOT NULL DEFAULT COALESCE(current_setting('app.workspace_id', true), 'default') REFERENCES workspaces(id) ON DELETE CASCADE;
ALTER TABLE booking_manage_tokens ADD COLUMN workspace_id TEXT NOT NULL DEFAULT COALESCE(current_setting('app.workspace_id', true), 'default') REFERENCES workspaces(id) ON DELETE CASCADE;
ALTER TABLE bookings              ADD COLUMN workspace_id TEXT NOT NULL DEFAULT COALESCE(current_setting('app.workspace_id', true), 'default') REFERENCES workspaces(id) ON DELETE CASCADE;
ALTER TABLE calendar_connections  ADD COLUMN workspace_id TEXT NOT NULL DEFAULT COALESCE(current_setting('app.workspace_id', true), 'default') REFERENCES workspaces(id) ON DELETE CASCADE;
ALTER TABLE connection_calendars  ADD COLUMN workspace_id TEXT NOT NULL DEFAULT COALESCE(current_setting('app.workspace_id', true), 'default') REFERENCES workspaces(id) ON DELETE CASCADE;
ALTER TABLE event_type_hosts      ADD COLUMN workspace_id TEXT NOT NULL DEFAULT COALESCE(current_setting('app.workspace_id', true), 'default') REFERENCES workspaces(id) ON DELETE CASCADE;
ALTER TABLE event_type_questions  ADD COLUMN workspace_id TEXT NOT NULL DEFAULT COALESCE(current_setting('app.workspace_id', true), 'default') REFERENCES workspaces(id) ON DELETE CASCADE;
ALTER TABLE event_type_reminders  ADD COLUMN workspace_id TEXT NOT NULL DEFAULT COALESCE(current_setting('app.workspace_id', true), 'default') REFERENCES workspaces(id) ON DELETE CASCADE;
ALTER TABLE event_types           ADD COLUMN workspace_id TEXT NOT NULL DEFAULT COALESCE(current_setting('app.workspace_id', true), 'default') REFERENCES workspaces(id) ON DELETE CASCADE;
ALTER TABLE idempotency_keys      ADD COLUMN workspace_id TEXT NOT NULL DEFAULT COALESCE(current_setting('app.workspace_id', true), 'default') REFERENCES workspaces(id) ON DELETE CASCADE;
ALTER TABLE invite_tokens         ADD COLUMN workspace_id TEXT NOT NULL DEFAULT COALESCE(current_setting('app.workspace_id', true), 'default') REFERENCES workspaces(id) ON DELETE CASCADE;
ALTER TABLE jobs                  ADD COLUMN workspace_id TEXT NOT NULL DEFAULT COALESCE(current_setting('app.workspace_id', true), 'default') REFERENCES workspaces(id) ON DELETE CASCADE;
ALTER TABLE magic_link_tokens     ADD COLUMN workspace_id TEXT NOT NULL DEFAULT COALESCE(current_setting('app.workspace_id', true), 'default') REFERENCES workspaces(id) ON DELETE CASCADE;
ALTER TABLE meeting_consents      ADD COLUMN workspace_id TEXT NOT NULL DEFAULT COALESCE(current_setting('app.workspace_id', true), 'default') REFERENCES workspaces(id) ON DELETE CASCADE;
ALTER TABLE notes                 ADD COLUMN workspace_id TEXT NOT NULL DEFAULT COALESCE(current_setting('app.workspace_id', true), 'default') REFERENCES workspaces(id) ON DELETE CASCADE;
ALTER TABLE oauth_access_tokens   ADD COLUMN workspace_id TEXT NOT NULL DEFAULT COALESCE(current_setting('app.workspace_id', true), 'default') REFERENCES workspaces(id) ON DELETE CASCADE;
ALTER TABLE oauth_auth_codes      ADD COLUMN workspace_id TEXT NOT NULL DEFAULT COALESCE(current_setting('app.workspace_id', true), 'default') REFERENCES workspaces(id) ON DELETE CASCADE;
ALTER TABLE recordings            ADD COLUMN workspace_id TEXT NOT NULL DEFAULT COALESCE(current_setting('app.workspace_id', true), 'default') REFERENCES workspaces(id) ON DELETE CASCADE;
ALTER TABLE server_settings       ADD COLUMN workspace_id TEXT NOT NULL DEFAULT COALESCE(current_setting('app.workspace_id', true), 'default') REFERENCES workspaces(id) ON DELETE CASCADE;
ALTER TABLE sessions              ADD COLUMN workspace_id TEXT NOT NULL DEFAULT COALESCE(current_setting('app.workspace_id', true), 'default') REFERENCES workspaces(id) ON DELETE CASCADE;
ALTER TABLE team_members          ADD COLUMN workspace_id TEXT NOT NULL DEFAULT COALESCE(current_setting('app.workspace_id', true), 'default') REFERENCES workspaces(id) ON DELETE CASCADE;
ALTER TABLE teams                 ADD COLUMN workspace_id TEXT NOT NULL DEFAULT COALESCE(current_setting('app.workspace_id', true), 'default') REFERENCES workspaces(id) ON DELETE CASCADE;
ALTER TABLE transcripts           ADD COLUMN workspace_id TEXT NOT NULL DEFAULT COALESCE(current_setting('app.workspace_id', true), 'default') REFERENCES workspaces(id) ON DELETE CASCADE;
ALTER TABLE users                 ADD COLUMN workspace_id TEXT NOT NULL DEFAULT COALESCE(current_setting('app.workspace_id', true), 'default') REFERENCES workspaces(id) ON DELETE CASCADE;
ALTER TABLE webhook_deliveries    ADD COLUMN workspace_id TEXT NOT NULL DEFAULT COALESCE(current_setting('app.workspace_id', true), 'default') REFERENCES workspaces(id) ON DELETE CASCADE;
ALTER TABLE webhooks              ADD COLUMN workspace_id TEXT NOT NULL DEFAULT COALESCE(current_setting('app.workspace_id', true), 'default') REFERENCES workspaces(id) ON DELETE CASCADE;
ALTER TABLE zoom_connections      ADD COLUMN workspace_id TEXT NOT NULL DEFAULT COALESCE(current_setting('app.workspace_id', true), 'default') REFERENCES workspaces(id) ON DELETE CASCADE;

-- ── Uniqueness that was global becomes per workspace (D8, D9) ─────────────────
--
-- What stays global, and why: api_keys.key_hash, sessions.id,
-- oauth_access_tokens.token_hash / refresh_hash, oauth_auth_codes.code_hash,
-- booking_manage_tokens.token_hash, magic_link_tokens.token_hash and
-- invite_tokens.token_hash are all CREDENTIALS. The tenant of a request that
-- carries one is resolved FROM it, so the lookup has to succeed before any
-- workspace is known — a composite key would make that lookup impossible.

-- server_settings keeps its id = 1 singleton per workspace, so the ~40
-- `WHERE id = 1` call sites need no edit: RLS narrows them to the tenant's row.
ALTER TABLE server_settings DROP CONSTRAINT server_settings_pkey;
ALTER TABLE server_settings ADD PRIMARY KEY (workspace_id, id);

ALTER TABLE idempotency_keys DROP CONSTRAINT idempotency_keys_pkey;
ALTER TABLE idempotency_keys ADD PRIMARY KEY (workspace_id, idempotency_key);

ALTER TABLE meeting_consents DROP CONSTRAINT meeting_consents_pkey;
ALTER TABLE meeting_consents ADD PRIMARY KEY (workspace_id, room, participant_identity);

ALTER TABLE users DROP CONSTRAINT users_email_key;
ALTER TABLE users ADD CONSTRAINT users_workspace_id_email_key UNIQUE (workspace_id, email);

ALTER TABLE event_types DROP CONSTRAINT event_types_slug_key;
ALTER TABLE event_types ADD CONSTRAINT event_types_workspace_id_slug_key UNIQUE (workspace_id, slug);

ALTER TABLE teams DROP CONSTRAINT teams_slug_key;
ALTER TABLE teams ADD CONSTRAINT teams_workspace_id_slug_key UNIQUE (workspace_id, slug);

DROP INDEX ux_jobs_type_payload;
CREATE UNIQUE INDEX ux_jobs_type_payload ON jobs (workspace_id, type, payload);

DROP INDEX idx_notes_booking;
CREATE UNIQUE INDEX idx_notes_booking ON notes (workspace_id, booking_id);

-- The partial indexes on bookings and jobs lead on workspace_id, because every
-- query that uses them now carries a workspace predicate from the policy.
DROP INDEX idx_bookings_no_double;
CREATE UNIQUE INDEX idx_bookings_no_double ON bookings (workspace_id, host_id, start_at)
    WHERE status <> 'cancelled';

DROP INDEX idx_bookings_host_time;
CREATE INDEX idx_bookings_host_time ON bookings (workspace_id, host_id, start_at, end_at)
    WHERE status = 'confirmed';

DROP INDEX idx_jobs_pending;
CREATE INDEX idx_jobs_pending ON jobs (workspace_id, run_at) WHERE status = 'pending';

DROP INDEX idx_jobs_running_expired;
CREATE INDEX idx_jobs_running_expired ON jobs (workspace_id, locked_until) WHERE status = 'running';

-- ⚠️ jobs is the one table worked ACROSS tenants: the worker's claim and its
-- crash-recovery reaper run on the platform handle with no workspace predicate
-- and order by run_at / locked_until globally. A workspace-leading index cannot
-- serve an ordered global scan, so the two originals are kept alongside under
-- _global names. Both pairs are tiny — jobs holds pending work, not history.
CREATE INDEX idx_jobs_pending_global ON jobs (run_at) WHERE status = 'pending';
CREATE INDEX idx_jobs_running_expired_global ON jobs (locked_until) WHERE status = 'running';

-- ── The policies (D2) ─────────────────────────────────────────────────────────
--
-- One permissive policy per tenant table, covering ALL commands. The
-- two-argument current_setting returns NULL for an unset parameter, so an
-- unbound session matches no row — reads return nothing and writes are refused
-- rather than defaulting to some tenant. The platform handle binds '' for the
-- same reason: no workspace id is the empty string, so '' matches nothing
-- either, and a stale value left on a pooled connection cannot leak into a
-- later statement because every statement sets the parameter itself.

CREATE POLICY api_keys_tenant ON api_keys USING (workspace_id = current_setting('app.workspace_id', true)) WITH CHECK (workspace_id = current_setting('app.workspace_id', true));
CREATE POLICY availability_overrides_tenant ON availability_overrides USING (workspace_id = current_setting('app.workspace_id', true)) WITH CHECK (workspace_id = current_setting('app.workspace_id', true));
CREATE POLICY availability_rules_tenant ON availability_rules USING (workspace_id = current_setting('app.workspace_id', true)) WITH CHECK (workspace_id = current_setting('app.workspace_id', true));
CREATE POLICY booking_answers_tenant ON booking_answers USING (workspace_id = current_setting('app.workspace_id', true)) WITH CHECK (workspace_id = current_setting('app.workspace_id', true));
CREATE POLICY booking_attendees_tenant ON booking_attendees USING (workspace_id = current_setting('app.workspace_id', true)) WITH CHECK (workspace_id = current_setting('app.workspace_id', true));
CREATE POLICY booking_hosts_tenant ON booking_hosts USING (workspace_id = current_setting('app.workspace_id', true)) WITH CHECK (workspace_id = current_setting('app.workspace_id', true));
CREATE POLICY booking_manage_tokens_tenant ON booking_manage_tokens USING (workspace_id = current_setting('app.workspace_id', true)) WITH CHECK (workspace_id = current_setting('app.workspace_id', true));
CREATE POLICY bookings_tenant ON bookings USING (workspace_id = current_setting('app.workspace_id', true)) WITH CHECK (workspace_id = current_setting('app.workspace_id', true));
CREATE POLICY calendar_connections_tenant ON calendar_connections USING (workspace_id = current_setting('app.workspace_id', true)) WITH CHECK (workspace_id = current_setting('app.workspace_id', true));
CREATE POLICY connection_calendars_tenant ON connection_calendars USING (workspace_id = current_setting('app.workspace_id', true)) WITH CHECK (workspace_id = current_setting('app.workspace_id', true));
CREATE POLICY event_type_hosts_tenant ON event_type_hosts USING (workspace_id = current_setting('app.workspace_id', true)) WITH CHECK (workspace_id = current_setting('app.workspace_id', true));
CREATE POLICY event_type_questions_tenant ON event_type_questions USING (workspace_id = current_setting('app.workspace_id', true)) WITH CHECK (workspace_id = current_setting('app.workspace_id', true));
CREATE POLICY event_type_reminders_tenant ON event_type_reminders USING (workspace_id = current_setting('app.workspace_id', true)) WITH CHECK (workspace_id = current_setting('app.workspace_id', true));
CREATE POLICY event_types_tenant ON event_types USING (workspace_id = current_setting('app.workspace_id', true)) WITH CHECK (workspace_id = current_setting('app.workspace_id', true));
CREATE POLICY idempotency_keys_tenant ON idempotency_keys USING (workspace_id = current_setting('app.workspace_id', true)) WITH CHECK (workspace_id = current_setting('app.workspace_id', true));
CREATE POLICY invite_tokens_tenant ON invite_tokens USING (workspace_id = current_setting('app.workspace_id', true)) WITH CHECK (workspace_id = current_setting('app.workspace_id', true));
CREATE POLICY jobs_tenant ON jobs USING (workspace_id = current_setting('app.workspace_id', true)) WITH CHECK (workspace_id = current_setting('app.workspace_id', true));
CREATE POLICY magic_link_tokens_tenant ON magic_link_tokens USING (workspace_id = current_setting('app.workspace_id', true)) WITH CHECK (workspace_id = current_setting('app.workspace_id', true));
CREATE POLICY meeting_consents_tenant ON meeting_consents USING (workspace_id = current_setting('app.workspace_id', true)) WITH CHECK (workspace_id = current_setting('app.workspace_id', true));
CREATE POLICY notes_tenant ON notes USING (workspace_id = current_setting('app.workspace_id', true)) WITH CHECK (workspace_id = current_setting('app.workspace_id', true));
CREATE POLICY oauth_access_tokens_tenant ON oauth_access_tokens USING (workspace_id = current_setting('app.workspace_id', true)) WITH CHECK (workspace_id = current_setting('app.workspace_id', true));
CREATE POLICY oauth_auth_codes_tenant ON oauth_auth_codes USING (workspace_id = current_setting('app.workspace_id', true)) WITH CHECK (workspace_id = current_setting('app.workspace_id', true));
CREATE POLICY recordings_tenant ON recordings USING (workspace_id = current_setting('app.workspace_id', true)) WITH CHECK (workspace_id = current_setting('app.workspace_id', true));
CREATE POLICY server_settings_tenant ON server_settings USING (workspace_id = current_setting('app.workspace_id', true)) WITH CHECK (workspace_id = current_setting('app.workspace_id', true));
CREATE POLICY sessions_tenant ON sessions USING (workspace_id = current_setting('app.workspace_id', true)) WITH CHECK (workspace_id = current_setting('app.workspace_id', true));
CREATE POLICY team_members_tenant ON team_members USING (workspace_id = current_setting('app.workspace_id', true)) WITH CHECK (workspace_id = current_setting('app.workspace_id', true));
CREATE POLICY teams_tenant ON teams USING (workspace_id = current_setting('app.workspace_id', true)) WITH CHECK (workspace_id = current_setting('app.workspace_id', true));
CREATE POLICY transcripts_tenant ON transcripts USING (workspace_id = current_setting('app.workspace_id', true)) WITH CHECK (workspace_id = current_setting('app.workspace_id', true));
CREATE POLICY users_tenant ON users USING (workspace_id = current_setting('app.workspace_id', true)) WITH CHECK (workspace_id = current_setting('app.workspace_id', true));
CREATE POLICY webhook_deliveries_tenant ON webhook_deliveries USING (workspace_id = current_setting('app.workspace_id', true)) WITH CHECK (workspace_id = current_setting('app.workspace_id', true));
CREATE POLICY webhooks_tenant ON webhooks USING (workspace_id = current_setting('app.workspace_id', true)) WITH CHECK (workspace_id = current_setting('app.workspace_id', true));
CREATE POLICY zoom_connections_tenant ON zoom_connections USING (workspace_id = current_setting('app.workspace_id', true)) WITH CHECK (workspace_id = current_setting('app.workspace_id', true));

-- workspaces itself is readable by the application role — h.publicURL(), the
-- suspended check and the host resolver all need it — and writable only by the
-- platform role, which owns the table and is therefore exempt from this policy.
CREATE POLICY workspaces_read ON workspaces FOR SELECT USING (true);

-- +goose Down
DROP POLICY IF EXISTS workspaces_read ON workspaces;

DROP POLICY IF EXISTS zoom_connections_tenant ON zoom_connections;
DROP POLICY IF EXISTS webhooks_tenant ON webhooks;
DROP POLICY IF EXISTS webhook_deliveries_tenant ON webhook_deliveries;
DROP POLICY IF EXISTS users_tenant ON users;
DROP POLICY IF EXISTS transcripts_tenant ON transcripts;
DROP POLICY IF EXISTS teams_tenant ON teams;
DROP POLICY IF EXISTS team_members_tenant ON team_members;
DROP POLICY IF EXISTS sessions_tenant ON sessions;
DROP POLICY IF EXISTS server_settings_tenant ON server_settings;
DROP POLICY IF EXISTS recordings_tenant ON recordings;
DROP POLICY IF EXISTS oauth_auth_codes_tenant ON oauth_auth_codes;
DROP POLICY IF EXISTS oauth_access_tokens_tenant ON oauth_access_tokens;
DROP POLICY IF EXISTS notes_tenant ON notes;
DROP POLICY IF EXISTS meeting_consents_tenant ON meeting_consents;
DROP POLICY IF EXISTS magic_link_tokens_tenant ON magic_link_tokens;
DROP POLICY IF EXISTS jobs_tenant ON jobs;
DROP POLICY IF EXISTS invite_tokens_tenant ON invite_tokens;
DROP POLICY IF EXISTS idempotency_keys_tenant ON idempotency_keys;
DROP POLICY IF EXISTS event_types_tenant ON event_types;
DROP POLICY IF EXISTS event_type_reminders_tenant ON event_type_reminders;
DROP POLICY IF EXISTS event_type_questions_tenant ON event_type_questions;
DROP POLICY IF EXISTS event_type_hosts_tenant ON event_type_hosts;
DROP POLICY IF EXISTS connection_calendars_tenant ON connection_calendars;
DROP POLICY IF EXISTS calendar_connections_tenant ON calendar_connections;
DROP POLICY IF EXISTS bookings_tenant ON bookings;
DROP POLICY IF EXISTS booking_manage_tokens_tenant ON booking_manage_tokens;
DROP POLICY IF EXISTS booking_hosts_tenant ON booking_hosts;
DROP POLICY IF EXISTS booking_attendees_tenant ON booking_attendees;
DROP POLICY IF EXISTS booking_answers_tenant ON booking_answers;
DROP POLICY IF EXISTS availability_rules_tenant ON availability_rules;
DROP POLICY IF EXISTS availability_overrides_tenant ON availability_overrides;
DROP POLICY IF EXISTS api_keys_tenant ON api_keys;

DROP INDEX IF EXISTS idx_jobs_running_expired_global;
DROP INDEX IF EXISTS idx_jobs_pending_global;

DROP INDEX idx_jobs_running_expired;
CREATE INDEX idx_jobs_running_expired ON jobs (locked_until) WHERE status = 'running';

DROP INDEX idx_jobs_pending;
CREATE INDEX idx_jobs_pending ON jobs (run_at) WHERE status = 'pending';

DROP INDEX idx_bookings_host_time;
CREATE INDEX idx_bookings_host_time ON bookings (host_id, start_at, end_at) WHERE status = 'confirmed';

DROP INDEX idx_bookings_no_double;
CREATE UNIQUE INDEX idx_bookings_no_double ON bookings (host_id, start_at) WHERE status <> 'cancelled';

DROP INDEX idx_notes_booking;
CREATE UNIQUE INDEX idx_notes_booking ON notes (booking_id);

DROP INDEX ux_jobs_type_payload;
CREATE UNIQUE INDEX ux_jobs_type_payload ON jobs (type, payload);

ALTER TABLE teams DROP CONSTRAINT teams_workspace_id_slug_key;
ALTER TABLE teams ADD CONSTRAINT teams_slug_key UNIQUE (slug);

ALTER TABLE event_types DROP CONSTRAINT event_types_workspace_id_slug_key;
ALTER TABLE event_types ADD CONSTRAINT event_types_slug_key UNIQUE (slug);

ALTER TABLE users DROP CONSTRAINT users_workspace_id_email_key;
ALTER TABLE users ADD CONSTRAINT users_email_key UNIQUE (email);

ALTER TABLE meeting_consents DROP CONSTRAINT meeting_consents_pkey;
ALTER TABLE meeting_consents ADD PRIMARY KEY (room, participant_identity);

ALTER TABLE idempotency_keys DROP CONSTRAINT idempotency_keys_pkey;
ALTER TABLE idempotency_keys ADD PRIMARY KEY (idempotency_key);

ALTER TABLE server_settings DROP CONSTRAINT server_settings_pkey;
ALTER TABLE server_settings ADD PRIMARY KEY (id);

ALTER TABLE zoom_connections      DROP COLUMN workspace_id;
ALTER TABLE webhooks              DROP COLUMN workspace_id;
ALTER TABLE webhook_deliveries    DROP COLUMN workspace_id;
ALTER TABLE users                 DROP COLUMN workspace_id;
ALTER TABLE transcripts           DROP COLUMN workspace_id;
ALTER TABLE teams                 DROP COLUMN workspace_id;
ALTER TABLE team_members          DROP COLUMN workspace_id;
ALTER TABLE sessions              DROP COLUMN workspace_id;
ALTER TABLE server_settings       DROP COLUMN workspace_id;
ALTER TABLE recordings            DROP COLUMN workspace_id;
ALTER TABLE oauth_auth_codes      DROP COLUMN workspace_id;
ALTER TABLE oauth_access_tokens   DROP COLUMN workspace_id;
ALTER TABLE notes                 DROP COLUMN workspace_id;
ALTER TABLE meeting_consents      DROP COLUMN workspace_id;
ALTER TABLE magic_link_tokens     DROP COLUMN workspace_id;
ALTER TABLE jobs                  DROP COLUMN workspace_id;
ALTER TABLE invite_tokens         DROP COLUMN workspace_id;
ALTER TABLE idempotency_keys      DROP COLUMN workspace_id;
ALTER TABLE event_types           DROP COLUMN workspace_id;
ALTER TABLE event_type_reminders  DROP COLUMN workspace_id;
ALTER TABLE event_type_questions  DROP COLUMN workspace_id;
ALTER TABLE event_type_hosts      DROP COLUMN workspace_id;
ALTER TABLE connection_calendars  DROP COLUMN workspace_id;
ALTER TABLE calendar_connections  DROP COLUMN workspace_id;
ALTER TABLE bookings              DROP COLUMN workspace_id;
ALTER TABLE booking_manage_tokens DROP COLUMN workspace_id;
ALTER TABLE booking_hosts         DROP COLUMN workspace_id;
ALTER TABLE booking_attendees     DROP COLUMN workspace_id;
ALTER TABLE booking_answers       DROP COLUMN workspace_id;
ALTER TABLE availability_rules    DROP COLUMN workspace_id;
ALTER TABLE availability_overrides DROP COLUMN workspace_id;
ALTER TABLE api_keys              DROP COLUMN workspace_id;

DROP TABLE workspaces;
