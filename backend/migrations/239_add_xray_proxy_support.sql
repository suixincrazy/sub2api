-- Migration: Add xray proxy support
-- Extends the proxies table to support xray-based protocols (VMess, VLESS, Trojan, etc.)
-- and adds tables for proxy sources and quality monitoring

-- 1. Extend proxies table with xray fields
ALTER TABLE proxies
    ADD COLUMN IF NOT EXISTS owner_user_id BIGINT REFERENCES users(id) ON DELETE CASCADE,
    ADD COLUMN IF NOT EXISTS is_public BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS kind VARCHAR(20) NOT NULL DEFAULT 'standard',
    ADD COLUMN IF NOT EXISTS expires_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS fallback_mode VARCHAR(20) NOT NULL DEFAULT 'none',
    ADD COLUMN IF NOT EXISTS backup_proxy_id BIGINT REFERENCES proxies(id) ON DELETE SET NULL,
    ADD COLUMN IF NOT EXISTS expiry_warn_days INT NOT NULL DEFAULT 7,
    ADD COLUMN IF NOT EXISTS extra JSONB NOT NULL DEFAULT '{}'::jsonb;

-- Update protocol column to support xray protocols
ALTER TABLE proxies ALTER COLUMN protocol TYPE VARCHAR(50);
COMMENT ON COLUMN proxies.protocol IS 'http/https/socks5/vmess/vless/trojan/shadowsocks/hysteria/tuic/anytls/naive/wireguard';

-- Add indexes for xray fields
CREATE INDEX IF NOT EXISTS idx_proxies_owner_user_id ON proxies(owner_user_id) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_proxies_kind ON proxies(kind) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_proxies_expires_at ON proxies(expires_at) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_proxies_backup_proxy_id ON proxies(backup_proxy_id) WHERE deleted_at IS NULL;

-- 2. Create proxy_sources table for subscription management
CREATE TABLE IF NOT EXISTS proxy_sources (
    id                        BIGSERIAL PRIMARY KEY,
    owner_user_id             BIGINT REFERENCES users(id) ON DELETE CASCADE,
    name                      VARCHAR(200) NOT NULL,
    url                       TEXT NOT NULL,
    format                    VARCHAR(20) NOT NULL,
    sync_enabled              BOOLEAN NOT NULL DEFAULT true,
    refresh_interval_minutes  INT NOT NULL DEFAULT 60,
    last_synced_at            TIMESTAMPTZ,
    last_sync_status          VARCHAR(20),
    last_sync_error           TEXT,
    last_imported_count       INT NOT NULL DEFAULT 0,
    created_at                TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at                TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at                TIMESTAMPTZ
);

COMMENT ON COLUMN proxy_sources.format IS 'base64/clash/singbox';
COMMENT ON COLUMN proxy_sources.last_sync_status IS 'syncing/success/error';

CREATE INDEX IF NOT EXISTS idx_proxy_sources_owner_user_id ON proxy_sources(owner_user_id) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_proxy_sources_sync_schedule ON proxy_sources(sync_enabled, last_synced_at, refresh_interval_minutes) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_proxy_sources_last_sync_status ON proxy_sources(last_sync_status) WHERE deleted_at IS NULL;

-- 3. Create proxy_quality_records table for latency monitoring
CREATE TABLE IF NOT EXISTS proxy_quality_records (
    id               BIGSERIAL PRIMARY KEY,
    proxy_id         BIGINT NOT NULL REFERENCES proxies(id) ON DELETE CASCADE,
    latency_ms       INT,
    is_available     BOOLEAN NOT NULL,
    error_message    TEXT,
    checked_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_proxy_quality_records_proxy_id ON proxy_quality_records(proxy_id, checked_at DESC);
CREATE INDEX IF NOT EXISTS idx_proxy_quality_records_checked_at ON proxy_quality_records(checked_at);
