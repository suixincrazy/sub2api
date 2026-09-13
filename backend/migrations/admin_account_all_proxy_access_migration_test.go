package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAdminAccountAllProxyAccessMigrationKeepsUserIsolation(t *testing.T) {
	content, err := FS.ReadFile("187_admin_account_all_proxy_access.sql")
	require.NoError(t, err)

	sql := strings.Join(strings.Fields(string(content)), " ")
	require.Contains(t, sql, "NEW.owner_user_id IS NOT NULL")
	require.Contains(t, sql, "proxy_owner IS DISTINCT FROM NEW.owner_user_id")
	require.Contains(t, sql, "NOT COALESCE(proxy_public, FALSE)")
	require.NotContains(t, sql, "system accounts cannot use user-owned proxies")
}
