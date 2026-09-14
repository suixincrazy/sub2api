package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestModernProxyBackupRoundTrip(t *testing.T) {
	router, source := setupProxyDataRouter()
	source.proxies = nil
	for i, sni := range []string{"a.example.com", "b.example.com"} {
		source.proxies = append(source.proxies, service.Proxy{
			ID: int64(i + 1), Name: sni, Kind: "xray", Protocol: "vless", Host: "proxy.example.com", Port: 443, Status: service.StatusActive,
			Extra: map[string]any{"raw": "vless://00000000-0000-4000-8000-000000000001@proxy.example.com:443?security=tls&sni=" + sni},
		})
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/admin/proxies/data", nil))
	require.Equal(t, http.StatusOK, response.Code)
	var exported proxyDataResponse
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &exported))
	require.Len(t, exported.Data.Proxies, 2)
	require.NotEqual(t, exported.Data.Proxies[0].ProxyKey, exported.Data.Proxies[1].ProxyKey, "different TLS node settings must not collide")

	targetRouter, target := setupProxyDataRouter()
	target.proxies = nil
	body, err := json.Marshal(map[string]any{"data": exported.Data})
	require.NoError(t, err)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/admin/proxies/data", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	targetRouter.ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code)
	require.Len(t, target.createdProxies, 2)
	for i, created := range target.createdProxies {
		require.Equal(t, "xray", created.Kind)
		require.Equal(t, source.proxies[i].Extra, created.Extra)
	}
}
