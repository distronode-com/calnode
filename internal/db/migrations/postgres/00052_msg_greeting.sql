-- +goose Up
-- Per-event-type override for the conversational assistant's opening greeting. Unlike
-- msg_confirmation/msg_cancellation/etc. (seeded with an English default at CreateEventType),
-- this is left NULL by default: the assistant falls back to the locale-keyed
-- "assistant_greeting" translation when unset, so translation keeps working automatically
-- for anyone who doesn't touch it. Only set (admin-authored, untranslated) once an operator
-- explicitly customizes it.
ALTER TABLE event_types ADD COLUMN msg_greeting TEXT;

-- +goose Down
-- Deliberately a no-op, mirroring the SQLite migration: Postgres could drop the
-- column, but a down that lands on a different schema per engine is worse than a
-- down that lands on none.
