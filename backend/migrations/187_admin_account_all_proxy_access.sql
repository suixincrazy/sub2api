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

    -- System accounts are managed by administrators and may use any proxy.
    -- User-owned accounts remain isolated to owned or explicitly public proxies.
    IF NEW.owner_user_id IS NOT NULL
       AND proxy_owner IS DISTINCT FROM NEW.owner_user_id
       AND NOT COALESCE(proxy_public, FALSE) THEN
        RAISE EXCEPTION 'user accounts can only use owned or public proxies' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
