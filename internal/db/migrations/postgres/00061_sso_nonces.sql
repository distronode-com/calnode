-- +goose Up
-- Single-use identifiers (jti) from the signed SSO hand-off tokens. A token's jti is
-- claimed here before its session is created, so a replay of a still-valid token
-- collides on the primary key instead of being handed a second session.
--
-- expires_at mirrors the token's own exp. The background worker purges rows past it in
-- the same GC pass that sweeps expired sessions and magic links, which is what keeps
-- this table the size of one token lifetime rather than of every sign-in ever made.
--
-- Nothing here is engine-specific except the collation: the column types are the
-- portable TEXT the rest of the schema uses, and no DEFAULT is needed because the
-- writer binds both values.
--
-- ⛔ expires_at is COLLATE "C" for the reason migration 00059 gives for the other 54
-- TEXT timestamps: the worker's purge is `WHERE expires_at < ?`, a lexicographic
-- comparison, and under a non-C database collation the ordering of the two timestamp
-- shapes this schema stores is not guaranteed to be byte order. It is declared here
-- rather than added by a later ALTER because the table is new in this migration.
-- internal/db/collation_test.go audits it by column NAME, so it is enforced.
CREATE TABLE sso_nonces (
    jti        TEXT PRIMARY KEY,
    expires_at TEXT COLLATE "C" NOT NULL
);

CREATE INDEX idx_sso_nonces_expires_at ON sso_nonces(expires_at);

-- +goose Down
DROP INDEX IF EXISTS idx_sso_nonces_expires_at;
DROP TABLE sso_nonces;
