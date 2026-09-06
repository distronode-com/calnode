-- +goose Up
-- Single-use identifiers (jti) from the signed SSO hand-off tokens. A token's jti is
-- claimed here before its session is created, so a replay of a still-valid token
-- collides on the primary key instead of being handed a second session.
--
-- expires_at mirrors the token's own exp. The background worker purges rows past it in
-- the same GC pass that sweeps expired sessions and magic links, which is what keeps
-- this table the size of one token lifetime rather than of every sign-in ever made.
CREATE TABLE sso_nonces (
    jti        TEXT PRIMARY KEY,
    expires_at TEXT NOT NULL
);

CREATE INDEX idx_sso_nonces_expires_at ON sso_nonces(expires_at);

-- +goose Down
DROP INDEX IF EXISTS idx_sso_nonces_expires_at;
DROP TABLE sso_nonces;
