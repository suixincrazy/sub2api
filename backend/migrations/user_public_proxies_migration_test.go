package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUserPublicProxiesMigrationWidensOnlyPublicAccess(t *testing.T) {
	content, err := FS.ReadFile("185_user_public_proxies.sql")
	require.NoError(t, err)

	sql := strings.Join(strings.Fields(string(content)), " ")
	require.Contains(t, sql, "ADD COLUMN IF NOT EXISTS is_public BOOLEAN NOT NULL DEFAULT FALSE")
	require.Contains(t, sql, "proxy_owner IS DISTINCT FROM NEW.owner_user_id AND NOT COALESCE(proxy_public, FALSE)")
	require.Contains(t, sql, "owner_user_id IS DISTINCT FROM NEW.owner_user_id")
	require.Contains(t, sql, "backup_owner IS DISTINCT FROM NEW.owner_user_id AND NOT COALESCE(backup_public, FALSE)")
	require.Contains(t, sql, "system accounts cannot use user-owned proxies")
	require.Contains(t, sql, "system proxies cannot fall back to user-owned proxies")
}

func TestUserResourceLifecycleMigrationProtectsFallbackReferencesAndTracksRevocations(t *testing.T) {
	content, err := FS.ReadFile("186_user_resource_lifecycle_hardening.sql")
	require.NoError(t, err)
	sql := strings.Join(strings.Fields(string(content)), " ")
	require.Contains(t, sql, "backup_proxy_id = OLD.id")
	require.Contains(t, sql, "owner_user_id IS DISTINCT FROM NEW.owner_user_id")
	require.Contains(t, sql, "BEFORE UPDATE OF owner_user_id, is_public, deleted_at ON proxies")
	require.Contains(t, sql, "ADD COLUMN IF NOT EXISTS revoked_by_user_id BIGINT")
	require.Contains(t, sql, "FOREIGN KEY (revoked_by_user_id) REFERENCES users(id) ON DELETE SET NULL")
	require.Contains(t, sql, "WHERE deleted_at IS NOT NULL AND revoked_by_user_id IS NULL")
	require.Contains(t, sql, "CREATE TRIGGER trg_user_subscriptions_auth_cache_invalidation")
	require.Contains(t, sql, "INSERT INTO auth_cache_invalidation_outbox (cache_key)")
	require.Contains(t, sql, "encode(sha256(convert_to(k.key, 'UTF8')), 'hex')")
}
