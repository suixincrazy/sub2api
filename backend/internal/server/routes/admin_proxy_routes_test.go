package routes

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAdminProxyResourceRoutesPrecedeDynamicProxyRoute(t *testing.T) {
	source, err := os.ReadFile("admin.go")
	require.NoError(t, err)

	text := string(source)
	dynamicRouteIndex := strings.Index(text, `proxies.GET("/:id", h.Admin.Proxy.GetByID)`)
	require.NotEqual(t, -1, dynamicRouteIndex)

	staticRoutes := []string{
		`proxies.POST("/import", h.Admin.Proxy.ImportProxyNodes)`,
		`proxies.GET("/sources", h.Admin.Proxy.ListProxySources)`,
		`proxies.POST("/sources", h.Admin.Proxy.CreateProxySource)`,
		`proxies.POST("/sources/sync-all", h.Admin.Proxy.SyncAllProxySources)`,
		`proxies.PUT("/sources/:id", h.Admin.Proxy.UpdateProxySource)`,
		`proxies.DELETE("/sources/:id", h.Admin.Proxy.DeleteProxySource)`,
		`proxies.POST("/sources/:id/sync", h.Admin.Proxy.SyncProxySource)`,
	}
	for _, route := range staticRoutes {
		routeIndex := strings.Index(text, route)
		require.NotEqualf(t, -1, routeIndex, "route %s must be registered", route)
		require.Lessf(t, routeIndex, dynamicRouteIndex, "route %s must precede /:id", route)
	}
}
