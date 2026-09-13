-- Xray: subscription source management.
-- Adds a per-source auto-sync switch and the airport quota/expiry snapshot that
-- the `subscription-userinfo` response header exposes on refresh.
--
-- No new index: proxy_sources stays small (per-owner cap) and the scheduler's due
-- lookup orders by COALESCE(last_synced_at, created_at), which no plain column
-- index would serve anyway.

ALTER TABLE proxy_sources
    ADD COLUMN IF NOT EXISTS sync_enabled BOOLEAN NOT NULL DEFAULT TRUE,
    ADD COLUMN IF NOT EXISTS sub_traffic_used BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS sub_traffic_total BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS sub_expires_at TIMESTAMPTZ NULL,
    ADD COLUMN IF NOT EXISTS sub_info_updated_at TIMESTAMPTZ NULL;
