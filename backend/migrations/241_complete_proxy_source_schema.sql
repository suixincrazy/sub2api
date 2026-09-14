-- Converge the legacy url/format layout and the canonical subscription schema.
-- Keep legacy columns so existing backups and deployments remain readable.
ALTER TABLE proxy_sources
    ADD COLUMN IF NOT EXISTS subscription_url TEXT,
    ADD COLUMN IF NOT EXISTS url TEXT,
    ADD COLUMN IF NOT EXISTS format VARCHAR(20) DEFAULT 'base64',
    ADD COLUMN IF NOT EXISTS is_public BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS sync_enabled BOOLEAN NOT NULL DEFAULT TRUE,
    ADD COLUMN IF NOT EXISTS sub_traffic_used BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS sub_traffic_total BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS sub_expires_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS sub_info_updated_at TIMESTAMPTZ;

UPDATE proxy_sources
SET subscription_url = COALESCE(NULLIF(subscription_url, ''), url, '')
WHERE subscription_url IS NULL OR subscription_url = '';

UPDATE proxy_sources
SET last_sync_status = 'never'
WHERE last_sync_status IS NULL OR last_sync_status = '';

ALTER TABLE proxy_sources
    ALTER COLUMN subscription_url SET NOT NULL,
    ALTER COLUMN url DROP NOT NULL,
    ALTER COLUMN format DROP NOT NULL,
    ALTER COLUMN last_sync_status SET DEFAULT 'never',
    ALTER COLUMN last_sync_status SET NOT NULL,
    ALTER COLUMN owner_user_id DROP NOT NULL;

CREATE UNIQUE INDEX IF NOT EXISTS idx_proxy_sources_owner_name_active_unique
    ON proxy_sources (COALESCE(owner_user_id, 0), name)
    WHERE deleted_at IS NULL;

CREATE UNIQUE INDEX IF NOT EXISTS idx_proxies_owner_source_node_active_unique
    ON proxies (COALESCE(owner_user_id, 0), (extra->>'source_id'), (extra->>'source_node_key'))
    WHERE deleted_at IS NULL AND extra ? 'source_id' AND extra ? 'source_node_key';

CREATE INDEX IF NOT EXISTS idx_proxy_sources_sync_schedule
    ON proxy_sources(sync_enabled, last_synced_at, refresh_interval_minutes)
    WHERE deleted_at IS NULL;
