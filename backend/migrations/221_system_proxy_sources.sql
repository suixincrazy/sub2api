ALTER TABLE proxy_sources
    ALTER COLUMN owner_user_id DROP NOT NULL;

DROP INDEX IF EXISTS idx_proxy_sources_owner_name_active_unique;
CREATE UNIQUE INDEX IF NOT EXISTS idx_proxy_sources_owner_name_active_unique
    ON proxy_sources (COALESCE(owner_user_id, 0), name)
    WHERE deleted_at IS NULL;

DROP INDEX IF EXISTS idx_proxies_owner_source_node_active_unique;
CREATE UNIQUE INDEX IF NOT EXISTS idx_proxies_owner_source_node_active_unique
    ON proxies (
        COALESCE(owner_user_id, 0),
        (extra->>'source_id'),
        (extra->>'source_node_key')
    )
    WHERE deleted_at IS NULL
      AND extra ? 'source_id'
      AND extra ? 'source_node_key';
