-- +goose Up
CREATE TABLE server_settings (
    -- Not an identity column: the row is seeded below with an explicit id and the
    -- CHECK makes 1 the only legal value, so a sequence would only be misleading.
    id              INTEGER PRIMARY KEY CHECK(id = 1),
    smtp_host       TEXT     NOT NULL DEFAULT '',
    smtp_port       TEXT     NOT NULL DEFAULT '587',
    smtp_user       TEXT     NOT NULL DEFAULT '',
    smtp_pass_enc   TEXT     NOT NULL DEFAULT '',
    smtp_tls        SMALLINT NOT NULL DEFAULT 0,
    smtp_starttls   SMALLINT NOT NULL DEFAULT 1,
    email_from      TEXT     NOT NULL DEFAULT '',
    email_from_name TEXT     NOT NULL DEFAULT 'Calnode',
    updated_at      TEXT     NOT NULL DEFAULT (to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD HH24:MI:SS'))
);
-- Seed the single row so UPDATE statements always find it.
INSERT INTO server_settings (id) VALUES (1) ON CONFLICT DO NOTHING;

-- +goose Down
DROP TABLE IF EXISTS server_settings;
