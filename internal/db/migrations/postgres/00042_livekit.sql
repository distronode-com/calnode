-- LiveKit (self-hostable WebRTC video) as a built-in meeting location.
--
-- Two parts:
--   1. Instance-level config columns (server URL + API key/secret, secret encrypted) on
--      server_settings, plus a livekit_room column on bookings — like Zoom/Stripe.
--   2. Widen the event_types.location_type CHECK to allow 'livekit'.
--
-- The SQLite migration rebuilds event_types for part 2, and needs NO TRANSACTION plus
-- PRAGMA foreign_keys=OFF to do it without cascade-deleting the child rows. Postgres
-- replaces the constraint in place, so none of that applies: this file runs in goose's
-- transaction like every other one. event_types_location_type_check is the name Postgres
-- gave the inline CHECK in 00001 (<table>_<column>_check).

-- +goose Up
ALTER TABLE server_settings ADD COLUMN livekit_url            TEXT NOT NULL DEFAULT '';
ALTER TABLE server_settings ADD COLUMN livekit_api_key        TEXT NOT NULL DEFAULT '';
ALTER TABLE server_settings ADD COLUMN livekit_api_secret_enc TEXT NOT NULL DEFAULT '';
ALTER TABLE bookings ADD COLUMN livekit_room TEXT NOT NULL DEFAULT '';

ALTER TABLE event_types
    DROP CONSTRAINT event_types_location_type_check,
    ADD CONSTRAINT event_types_location_type_check
        CHECK (location_type IN ('zoom','google_meet','teams','custom_video','phone','in_person','link','livekit'));

-- +goose Down
-- Irreversible widening of a CHECK; the added columns are harmless. No-op down.
