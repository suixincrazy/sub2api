ALTER TABLE proxy_sources
ADD COLUMN IF NOT EXISTS is_public BOOLEAN NOT NULL DEFAULT FALSE;

ALTER TABLE proxies DROP CONSTRAINT IF EXISTS proxies_public_system_only;

CREATE OR REPLACE FUNCTION enforce_account_proxy_owner_isolation()
RETURNS TRIGGER AS $$
DECLARE
    proxy_owner BIGINT;
    proxy_public BOOLEAN;
BEGIN
    IF NEW.proxy_id IS NULL THEN
        RETURN NEW;
    END IF;

    SELECT owner_user_id, is_public INTO proxy_owner, proxy_public
    FROM proxies
    WHERE id = NEW.proxy_id;

    IF NEW.owner_user_id IS NULL AND proxy_owner IS NOT NULL THEN
        RAISE EXCEPTION 'system accounts cannot use user-owned proxies' USING ERRCODE = '23514';
    END IF;

    IF NEW.owner_user_id IS NOT NULL
       AND proxy_owner IS DISTINCT FROM NEW.owner_user_id
       AND NOT COALESCE(proxy_public, FALSE) THEN
        RAISE EXCEPTION 'user accounts can only use owned or public proxies' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION prevent_public_proxy_revoke_while_in_use()
RETURNS TRIGGER AS $$
BEGIN
    IF OLD.is_public = TRUE
       AND NEW.is_public = FALSE
       AND EXISTS (
           SELECT 1
           FROM accounts
           WHERE proxy_id = OLD.id
             AND owner_user_id IS DISTINCT FROM NEW.owner_user_id
             AND deleted_at IS NULL
       ) THEN
        RAISE EXCEPTION 'public proxy is still used by another user account' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

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
    FROM proxies
    WHERE id = NEW.backup_proxy_id;

    IF NEW.owner_user_id IS NULL AND backup_owner IS NOT NULL THEN
        RAISE EXCEPTION 'system proxies cannot fall back to user-owned proxies' USING ERRCODE = '23514';
    END IF;

    IF NEW.owner_user_id IS NOT NULL
       AND backup_owner IS DISTINCT FROM NEW.owner_user_id
       AND NOT COALESCE(backup_public, FALSE) THEN
        RAISE EXCEPTION 'user proxies can only fall back to owned or public proxies' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
