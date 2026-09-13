-- Normalize proxy source columns created by the early Xray migration.
-- 239_add_xray_proxy_support.sql used `url`/`format`, while the service and
-- Ent schema use `subscription_url` plus lifecycle and quota fields. Keep the
-- legacy columns for compatibility, but make the service-facing shape total.

ALTER TABLE groups
    ADD COLUMN IF NOT EXISTS owner_user_id BIGINT REFERENCES users(id);
ALTER TABLE accounts
    ADD COLUMN IF NOT EXISTS owner_user_id BIGINT REFERENCES users(id);
ALTER TABLE redeem_codes
    ADD COLUMN IF NOT EXISTS owner_user_id BIGINT REFERENCES users(id);
ALTER TABLE user_subscriptions
    ADD COLUMN IF NOT EXISTS managed_by_user_id BIGINT REFERENCES users(id),
    ADD COLUMN IF NOT EXISTS source_type VARCHAR(32) NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS source_redeem_code_id BIGINT REFERENCES redeem_codes(id) ON DELETE SET NULL;

ALTER TABLE proxy_sources
    ADD COLUMN IF NOT EXISTS subscription_url TEXT,
    ADD COLUMN IF NOT EXISTS url TEXT,
    ADD COLUMN IF NOT EXISTS format VARCHAR(20) NOT NULL DEFAULT 'base64',
    ADD COLUMN IF NOT EXISTS is_public BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS sync_enabled BOOLEAN NOT NULL DEFAULT TRUE,
    ADD COLUMN IF NOT EXISTS sub_traffic_used BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS sub_traffic_total BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS sub_expires_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS sub_info_updated_at TIMESTAMPTZ;

UPDATE proxy_sources
SET subscription_url = COALESCE(NULLIF(subscription_url, ''), NULLIF(url, ''), '')
WHERE subscription_url IS NULL OR subscription_url = '';

UPDATE proxy_sources
SET url = subscription_url
WHERE url IS NULL OR url = '';

UPDATE proxy_sources
SET last_sync_status = 'never'
WHERE last_sync_status IS NULL OR last_sync_status = '';

ALTER TABLE proxy_sources
    ALTER COLUMN subscription_url SET NOT NULL,
    ALTER COLUMN last_sync_status SET DEFAULT 'never',
    ALTER COLUMN last_sync_status SET NOT NULL,
    ALTER COLUMN owner_user_id DROP NOT NULL;

CREATE INDEX IF NOT EXISTS idx_proxy_sources_sync_schedule
    ON proxy_sources(sync_enabled, last_synced_at, refresh_interval_minutes)
    WHERE deleted_at IS NULL;
