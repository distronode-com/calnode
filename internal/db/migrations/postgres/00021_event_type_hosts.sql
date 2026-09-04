-- +goose Up
-- Routing model: an event type owns a host list. Each host has a role —
-- required (always attends), rotation (one is picked per booking), or optional
-- (joins if free). The three UI modes (Normal/Round-robin/Group) are presets
-- over these roles. See docs/teams-and-routing.md.
CREATE TABLE event_type_hosts (
    id            TEXT    PRIMARY KEY,
    event_type_id TEXT    NOT NULL REFERENCES event_types(id) ON DELETE CASCADE,
    user_id       TEXT    NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role          TEXT    NOT NULL DEFAULT 'required'
                    CHECK (role IN ('required', 'rotation', 'optional')),
    priority      INTEGER NOT NULL DEFAULT 0,
    UNIQUE(event_type_id, user_id)
);

CREATE INDEX idx_event_type_hosts_event ON event_type_hosts(event_type_id);

-- Round-robin selection strategy for round_robin event types.
ALTER TABLE event_types ADD COLUMN rr_strategy TEXT NOT NULL DEFAULT 'even'
    CHECK (rr_strategy IN ('even', 'soonest', 'priority'));

-- Backfill: every existing event type gets its owner as the single required
-- host, so today's solo events become Normal with the owner as the one host and
-- host resolution is uniform from day one.
--
-- The id matches what SQLite's lower(hex(randomblob(16))) produces — 32 lowercase
-- hex characters — using gen_random_uuid, which is core since PostgreSQL 13 and so
-- needs no extension.
INSERT INTO event_type_hosts (id, event_type_id, user_id, role, priority)
SELECT replace(gen_random_uuid()::text, '-', ''), id, user_id, 'required', 0 FROM event_types;

-- +goose Down
DROP TABLE IF EXISTS event_type_hosts;
-- rr_strategy is deliberately left in place, mirroring the SQLite migration:
-- Postgres could drop it, but a down that lands on a different schema per engine
-- is worse than one that leaves a harmless column behind.
