-- User-owned resources follow the upstream 172 video billing migration.
ALTER TABLE groups ADD COLUMN IF NOT EXISTS owner_user_id BIGINT REFERENCES users(id);
ALTER TABLE accounts ADD COLUMN IF NOT EXISTS owner_user_id BIGINT REFERENCES users(id);
ALTER TABLE proxies ADD COLUMN IF NOT EXISTS owner_user_id BIGINT REFERENCES users(id);
ALTER TABLE proxies ADD COLUMN IF NOT EXISTS is_public BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE proxies ADD COLUMN IF NOT EXISTS kind VARCHAR(20) NOT NULL DEFAULT 'standard';
ALTER TABLE proxies ADD COLUMN IF NOT EXISTS extra JSONB NOT NULL DEFAULT '{}'::jsonb;
ALTER TABLE proxies DROP CONSTRAINT IF EXISTS proxies_public_system_only;
ALTER TABLE proxies ADD CONSTRAINT proxies_public_system_only CHECK (NOT is_public OR owner_user_id IS NULL);
ALTER TABLE redeem_codes ADD COLUMN IF NOT EXISTS owner_user_id BIGINT REFERENCES users(id);
ALTER TABLE user_subscriptions ADD COLUMN IF NOT EXISTS managed_by_user_id BIGINT REFERENCES users(id);
ALTER TABLE user_subscriptions ADD COLUMN IF NOT EXISTS source_type VARCHAR(32) NOT NULL DEFAULT '';
ALTER TABLE user_subscriptions ADD COLUMN IF NOT EXISTS source_redeem_code_id BIGINT REFERENCES redeem_codes(id) ON DELETE SET NULL;

ALTER TABLE groups DROP CONSTRAINT IF EXISTS groups_owner_user_id_fkey;
ALTER TABLE groups ADD CONSTRAINT groups_owner_user_id_fkey FOREIGN KEY (owner_user_id) REFERENCES users(id);
ALTER TABLE accounts DROP CONSTRAINT IF EXISTS accounts_owner_user_id_fkey;
ALTER TABLE accounts ADD CONSTRAINT accounts_owner_user_id_fkey FOREIGN KEY (owner_user_id) REFERENCES users(id);
ALTER TABLE proxies DROP CONSTRAINT IF EXISTS proxies_owner_user_id_fkey;
ALTER TABLE proxies ADD CONSTRAINT proxies_owner_user_id_fkey FOREIGN KEY (owner_user_id) REFERENCES users(id);
ALTER TABLE redeem_codes DROP CONSTRAINT IF EXISTS redeem_codes_owner_user_id_fkey;
ALTER TABLE redeem_codes ADD CONSTRAINT redeem_codes_owner_user_id_fkey FOREIGN KEY (owner_user_id) REFERENCES users(id);
ALTER TABLE user_subscriptions DROP CONSTRAINT IF EXISTS user_subscriptions_managed_by_user_id_fkey;
ALTER TABLE user_subscriptions ADD CONSTRAINT user_subscriptions_managed_by_user_id_fkey FOREIGN KEY (managed_by_user_id) REFERENCES users(id);

CREATE TABLE IF NOT EXISTS proxy_sources (
    id BIGSERIAL PRIMARY KEY,
    owner_user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name VARCHAR(100) NOT NULL,
    subscription_url TEXT NOT NULL,
    refresh_interval_minutes INT NOT NULL DEFAULT 1440,
    last_synced_at TIMESTAMPTZ,
    last_sync_status VARCHAR(20) NOT NULL DEFAULT 'never',
    last_sync_error TEXT,
    last_imported_count INT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_groups_owner_user_id ON groups(owner_user_id);
CREATE INDEX IF NOT EXISTS idx_groups_owner_deleted_at ON groups(owner_user_id, deleted_at);
DROP INDEX IF EXISTS groups_name_unique_active;
CREATE UNIQUE INDEX IF NOT EXISTS idx_groups_owner_name_active_unique ON groups(COALESCE(owner_user_id, 0), name) WHERE deleted_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_accounts_owner_user_id ON accounts(owner_user_id);
CREATE INDEX IF NOT EXISTS idx_accounts_owner_deleted_at ON accounts(owner_user_id, deleted_at);
-- Historical system accounts were allowed to share names. User-owned accounts are
-- new in this migration, so scope the new uniqueness rule to them to keep upgrades safe.
CREATE UNIQUE INDEX IF NOT EXISTS idx_accounts_owner_name_active_unique ON accounts(owner_user_id, name)
WHERE owner_user_id IS NOT NULL AND deleted_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_proxies_owner_user_id ON proxies(owner_user_id);
CREATE INDEX IF NOT EXISTS idx_proxies_owner_deleted_at ON proxies(owner_user_id, deleted_at);
CREATE INDEX IF NOT EXISTS idx_proxies_is_public ON proxies(is_public);
CREATE INDEX IF NOT EXISTS idx_proxies_kind ON proxies(kind);
-- Keep compatibility with pre-existing duplicate system proxy names while enforcing
-- per-owner uniqueness for every user-created proxy.
CREATE UNIQUE INDEX IF NOT EXISTS idx_proxies_owner_name_active_unique ON proxies(owner_user_id, name)
WHERE owner_user_id IS NOT NULL AND deleted_at IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS idx_proxies_owner_source_node_active_unique
ON proxies(owner_user_id, (extra->>'source_id'), (extra->>'source_node_key'))
WHERE owner_user_id IS NOT NULL AND deleted_at IS NULL
  AND extra ? 'source_id' AND extra ? 'source_node_key';

CREATE INDEX IF NOT EXISTS idx_redeem_codes_owner_user_id ON redeem_codes(owner_user_id);
CREATE INDEX IF NOT EXISTS idx_user_subscriptions_managed_by_user_id ON user_subscriptions(managed_by_user_id);
CREATE INDEX IF NOT EXISTS idx_user_subscriptions_source_type ON user_subscriptions(source_type);
CREATE INDEX IF NOT EXISTS idx_user_subscriptions_source_redeem_code_id ON user_subscriptions(source_redeem_code_id);

CREATE INDEX IF NOT EXISTS idx_proxy_sources_owner_user_id ON proxy_sources(owner_user_id);
CREATE INDEX IF NOT EXISTS idx_proxy_sources_owner_deleted_at ON proxy_sources(owner_user_id, deleted_at);
CREATE INDEX IF NOT EXISTS idx_proxy_sources_last_sync_status ON proxy_sources(last_sync_status);
CREATE UNIQUE INDEX IF NOT EXISTS idx_proxy_sources_owner_name_active_unique ON proxy_sources(owner_user_id, name) WHERE deleted_at IS NULL;

CREATE OR REPLACE FUNCTION enforce_account_group_owner_isolation()
RETURNS TRIGGER AS $$
DECLARE
    account_owner BIGINT;
    group_owner BIGINT;
BEGIN
    SELECT owner_user_id INTO account_owner FROM accounts WHERE id = NEW.account_id;
    SELECT owner_user_id INTO group_owner FROM groups WHERE id = NEW.group_id;
    IF account_owner IS DISTINCT FROM group_owner THEN
        RAISE EXCEPTION 'account and group owners must match' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_account_group_owner_isolation ON account_groups;
CREATE TRIGGER trg_account_group_owner_isolation
BEFORE INSERT OR UPDATE OF account_id, group_id ON account_groups
FOR EACH ROW EXECUTE FUNCTION enforce_account_group_owner_isolation();

CREATE OR REPLACE FUNCTION enforce_account_proxy_owner_isolation()
RETURNS TRIGGER AS $$
DECLARE
    proxy_owner BIGINT;
    proxy_public BOOLEAN;
BEGIN
    IF NEW.proxy_id IS NULL THEN
        RETURN NEW;
    END IF;
    SELECT owner_user_id, is_public INTO proxy_owner, proxy_public FROM proxies WHERE id = NEW.proxy_id;
    IF NEW.owner_user_id IS NULL AND proxy_owner IS NOT NULL THEN
        RAISE EXCEPTION 'system accounts cannot use user-owned proxies' USING ERRCODE = '23514';
    END IF;
    IF NEW.owner_user_id IS NOT NULL
       AND proxy_owner IS DISTINCT FROM NEW.owner_user_id
       AND NOT (proxy_owner IS NULL AND proxy_public) THEN
        RAISE EXCEPTION 'user accounts can only use owned or public system proxies' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_account_proxy_owner_isolation ON accounts;
CREATE TRIGGER trg_account_proxy_owner_isolation
BEFORE INSERT OR UPDATE OF owner_user_id, proxy_id ON accounts
FOR EACH ROW EXECUTE FUNCTION enforce_account_proxy_owner_isolation();

CREATE OR REPLACE FUNCTION prevent_public_proxy_revoke_while_in_use()
RETURNS TRIGGER AS $$
BEGIN
    IF OLD.owner_user_id IS NULL
       AND OLD.is_public = true
       AND (NEW.owner_user_id IS NOT NULL OR NEW.is_public = false)
       AND EXISTS (
           SELECT 1 FROM accounts
           WHERE proxy_id = OLD.id AND owner_user_id IS NOT NULL AND deleted_at IS NULL
       ) THEN
        RAISE EXCEPTION 'public proxy is still used by user-owned accounts' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_prevent_public_proxy_revoke_while_in_use ON proxies;
CREATE TRIGGER trg_prevent_public_proxy_revoke_while_in_use
BEFORE UPDATE OF owner_user_id, is_public ON proxies
FOR EACH ROW EXECUTE FUNCTION prevent_public_proxy_revoke_while_in_use();

CREATE OR REPLACE FUNCTION enforce_proxy_fallback_owner_isolation()
RETURNS TRIGGER AS $$
DECLARE
    backup_owner BIGINT;
    backup_public BOOLEAN;
BEGIN
    IF NEW.backup_proxy_id IS NULL THEN
        RETURN NEW;
    END IF;
    IF NEW.backup_proxy_id = NEW.id THEN
        RAISE EXCEPTION 'backup proxy cannot reference itself' USING ERRCODE = '23514';
    END IF;
    SELECT owner_user_id, is_public INTO backup_owner, backup_public
    FROM proxies WHERE id = NEW.backup_proxy_id;
    IF NEW.owner_user_id IS NULL AND backup_owner IS NOT NULL THEN
        RAISE EXCEPTION 'system proxies cannot fall back to user-owned proxies' USING ERRCODE = '23514';
    END IF;
    IF NEW.owner_user_id IS NOT NULL
       AND backup_owner IS DISTINCT FROM NEW.owner_user_id
       AND NOT (backup_owner IS NULL AND backup_public) THEN
        RAISE EXCEPTION 'user proxies can only fall back to owned or public system proxies' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_proxy_fallback_owner_isolation ON proxies;
CREATE TRIGGER trg_proxy_fallback_owner_isolation
BEFORE INSERT OR UPDATE OF owner_user_id, backup_proxy_id ON proxies
FOR EACH ROW EXECUTE FUNCTION enforce_proxy_fallback_owner_isolation();

CREATE OR REPLACE FUNCTION enforce_group_fallback_owner_isolation()
RETURNS TRIGGER AS $$
DECLARE
    fallback_owner BIGINT;
    fallback_id BIGINT;
BEGIN
    FOREACH fallback_id IN ARRAY ARRAY[NEW.fallback_group_id, NEW.fallback_group_id_on_invalid_request]
    LOOP
        IF fallback_id IS NULL THEN
            CONTINUE;
        END IF;
        SELECT owner_user_id INTO fallback_owner FROM groups WHERE id = fallback_id;
        IF fallback_owner IS DISTINCT FROM NEW.owner_user_id THEN
            RAISE EXCEPTION 'fallback groups must have the same owner' USING ERRCODE = '23514';
        END IF;
    END LOOP;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_group_fallback_owner_isolation ON groups;
CREATE TRIGGER trg_group_fallback_owner_isolation
BEFORE INSERT OR UPDATE OF owner_user_id, fallback_group_id, fallback_group_id_on_invalid_request ON groups
FOR EACH ROW EXECUTE FUNCTION enforce_group_fallback_owner_isolation();
