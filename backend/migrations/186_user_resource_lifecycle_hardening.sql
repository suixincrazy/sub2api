-- Keep user-owned proxy fallback references consistent when a public proxy is
-- made private. Account references were already checked by migration 185;
-- fallback references need the same owner boundary.
CREATE OR REPLACE FUNCTION prevent_public_proxy_revoke_while_in_use()
RETURNS TRIGGER AS $$
BEGIN
	IF OLD.deleted_at IS NULL
	   AND NEW.deleted_at IS NOT NULL
	   AND (
	       EXISTS (
	           SELECT 1 FROM accounts
	           WHERE proxy_id = OLD.id AND deleted_at IS NULL
	       )
	       OR EXISTS (
	           SELECT 1 FROM proxies
	           WHERE backup_proxy_id = OLD.id AND deleted_at IS NULL
	       )
	   ) THEN
	    RAISE EXCEPTION 'proxy is still referenced by an active resource' USING ERRCODE = '23514';
	END IF;

    IF OLD.is_public = TRUE
       AND NEW.is_public = FALSE
       AND (
           EXISTS (
               SELECT 1
               FROM accounts
               WHERE proxy_id = OLD.id
                 AND owner_user_id IS DISTINCT FROM NEW.owner_user_id
                 AND deleted_at IS NULL
           )
           OR EXISTS (
               SELECT 1
               FROM proxies
               WHERE backup_proxy_id = OLD.id
                 AND owner_user_id IS DISTINCT FROM NEW.owner_user_id
                 AND deleted_at IS NULL
           )
       ) THEN
        RAISE EXCEPTION 'public proxy is still used by another user resource' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_prevent_public_proxy_revoke_while_in_use ON proxies;
CREATE TRIGGER trg_prevent_public_proxy_revoke_while_in_use
BEFORE UPDATE OF owner_user_id, is_public, deleted_at ON proxies
FOR EACH ROW EXECUTE FUNCTION prevent_public_proxy_revoke_while_in_use();

ALTER TABLE user_subscriptions
    ADD COLUMN IF NOT EXISTS revoked_by_user_id BIGINT;

ALTER TABLE user_subscriptions
    DROP CONSTRAINT IF EXISTS user_subscriptions_revoked_by_user_id_fkey;
ALTER TABLE user_subscriptions
    ADD CONSTRAINT user_subscriptions_revoked_by_user_id_fkey
    FOREIGN KEY (revoked_by_user_id) REFERENCES users(id) ON DELETE SET NULL;

-- Existing soft-deleted subscriptions predate revocation attribution. Treat
-- them conservatively as subscriber-initiated so a resource owner cannot
-- silently reactivate historical opt-outs. Administrators retain the explicit
-- force-restore path, which clears this marker.
UPDATE user_subscriptions
SET revoked_by_user_id = user_id
WHERE deleted_at IS NOT NULL AND revoked_by_user_id IS NULL;

CREATE INDEX IF NOT EXISTS idx_user_subscriptions_revoked_by_user_id
    ON user_subscriptions(revoked_by_user_id);

-- Subscription mutations change authorization without changing an API key
-- row. Enqueue hashed key invalidations in the same database transaction so
-- Redis or pub/sub outages cannot leave cross-instance auth state stale.
CREATE OR REPLACE FUNCTION enqueue_subscription_auth_cache_invalidation()
RETURNS TRIGGER AS $$
DECLARE
    old_user_id BIGINT;
    new_user_id BIGINT;
BEGIN
    IF TG_OP = 'UPDATE'
       AND OLD.user_id IS NOT DISTINCT FROM NEW.user_id
       AND OLD.group_id IS NOT DISTINCT FROM NEW.group_id
       AND OLD.status IS NOT DISTINCT FROM NEW.status
       AND OLD.expires_at IS NOT DISTINCT FROM NEW.expires_at
       AND OLD.deleted_at IS NOT DISTINCT FROM NEW.deleted_at THEN
        RETURN NEW;
    END IF;

    IF TG_OP <> 'INSERT' THEN
        old_user_id := OLD.user_id;
    END IF;
    IF TG_OP <> 'DELETE' THEN
        new_user_id := NEW.user_id;
    END IF;

    INSERT INTO auth_cache_invalidation_outbox (cache_key)
    SELECT encode(sha256(convert_to(k.key, 'UTF8')), 'hex')
    FROM api_keys AS k
    WHERE k.deleted_at IS NULL
      AND k.key <> ''
      AND (k.user_id = old_user_id OR k.user_id = new_user_id);

    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_user_subscriptions_auth_cache_invalidation ON user_subscriptions;
CREATE TRIGGER trg_user_subscriptions_auth_cache_invalidation
AFTER INSERT OR UPDATE OR DELETE ON user_subscriptions
FOR EACH ROW EXECUTE FUNCTION enqueue_subscription_auth_cache_invalidation();
