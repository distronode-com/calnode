-- +goose Up
-- Two per-workspace settings the platform API's `defaults` carries and server_settings
-- had nowhere to put. Both are per-TENANT facts on a multi-tenant instance, so the
-- environment cannot express them:
--
--   embed_allowed_origins  the origins allowed to embed this workspace's booking page.
--                          One tenant's allowlist must not apply to another's embed, and
--                          EMBED_ALLOWED_ORIGINS is one process-wide value.
--   stt_base_url           the speech-to-text endpoint host for this workspace's
--                          notetaker. It is a residency knob (transcribe inside one
--                          jurisdiction), and residency is a property of the tenant.
--
-- Both default to '' meaning "fall back to the process-wide value", so a single-tenant
-- instance and every existing row behave exactly as before.
--
-- ⚠️ These columns are WRITTEN by the platform API and not yet READ: the embed CORS
-- check still uses config.EmbedAllowedOrigins and the notetaker still uses
-- config.STTBaseURL. Storing them first means a provisioned workspace does not silently
-- lose the values the caller sent; wiring the readers is a separate change, because each
-- has to go through the per-workspace settings cache (D7) rather than a process-wide
-- read. Until then a multi-tenant deployment shares one embed allowlist and one STT host.
ALTER TABLE server_settings ADD COLUMN embed_allowed_origins TEXT NOT NULL DEFAULT '';
ALTER TABLE server_settings ADD COLUMN stt_base_url TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE server_settings DROP COLUMN stt_base_url;
ALTER TABLE server_settings DROP COLUMN embed_allowed_origins;
