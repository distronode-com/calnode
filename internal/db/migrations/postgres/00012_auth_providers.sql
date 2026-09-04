-- +goose Up

-- Email/password login and OAuth provider columns on users.
-- email_login=1 means the user can authenticate with email+password.
-- provider/provider_id store the OAuth identity (only one provider per user).
ALTER TABLE users ADD COLUMN email_login  SMALLINT NOT NULL DEFAULT 0;
ALTER TABLE users ADD COLUMN password_hash TEXT;
ALTER TABLE users ADD COLUMN provider     TEXT;   -- 'google', 'microsoft', etc.
ALTER TABLE users ADD COLUMN provider_id  TEXT;   -- provider's opaque user ID

-- Invite tokens: single-use, locked to a specific email, 7-day expiry.
CREATE TABLE invite_tokens (
    id          TEXT PRIMARY KEY,
    email       TEXT NOT NULL,
    token_hash  TEXT NOT NULL UNIQUE,
    created_by  TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    expires_at  TEXT NOT NULL,
    used_at     TEXT
);

CREATE INDEX idx_invite_tokens_email ON invite_tokens(email);

-- +goose Down

DROP INDEX IF EXISTS idx_invite_tokens_email;
DROP TABLE IF EXISTS invite_tokens;

-- The SQLite migration rebuilds users here because it cannot drop a column;
-- Postgres drops the four directly, which reaches the same schema.
ALTER TABLE users DROP COLUMN provider_id;
ALTER TABLE users DROP COLUMN provider;
ALTER TABLE users DROP COLUMN password_hash;
ALTER TABLE users DROP COLUMN email_login;
