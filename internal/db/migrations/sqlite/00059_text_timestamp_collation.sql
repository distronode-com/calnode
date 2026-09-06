-- +goose Up
-- No-op on SQLite, deliberately, and the file exists so the two migration
-- directories keep one file per version (TestMigrationDirs_parity enforces that,
-- and TargetVersion is dialect-independent because of it).
--
-- The Postgres half pins every TEXT timestamp column to COLLATE "C" so that
-- `run_at <= ?`, the consent window, the booking overlap predicates, the token
-- expiries and `ORDER BY created_at` compare bytes there. SQLite has nothing to
-- pin: its only collations are BINARY (the default), NOCASE and RTRIM, and
-- BINARY *is* memcmp. The behaviour this migration buys on Postgres is what
-- SQLite already does, which is why the port could get this far without noticing.
--
-- A statement is included rather than leaving the section empty because goose
-- treats a migration with no statements as a parse problem, and a SELECT is the
-- cheapest way to say "nothing to do" in a file that still has to be applied and
-- recorded.
SELECT 1;

-- +goose Down
SELECT 1;
