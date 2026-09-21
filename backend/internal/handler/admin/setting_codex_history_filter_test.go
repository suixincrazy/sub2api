package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestCodexHistoryFilterAdminSettings(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := newTestSettingRepo()
	h := &SettingHandler{settingService: service.NewSettingService(repo, nil)}
	router := gin.New()
	router.GET("/filter", h.GetCodexHistoryFilter)
	router.PUT("/filter", h.UpdateCodexHistoryFilter)
	for _, body := range []string{`{}`, `{"enabled":null}`, `{"enabled":"true"}`, `{"enabled":1}`, `{`} {
		response := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPut, "/filter", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(response, req)
		require.Equal(t, http.StatusBadRequest, response.Code)
		require.Empty(t, repo.values)
	}
	for _, body := range []string{`{"enabled":true}`, `{"enabled":false}`} {
		response := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPut, "/filter", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(response, req)
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		read := httptest.NewRecorder()
		router.ServeHTTP(read, httptest.NewRequest(http.MethodGet, "/filter", nil))
		require.Equal(t, response.Body.String(), read.Body.String())
		require.Contains(t, read.Body.String(), `"filter_version":3`)
	}
	require.Equal(t, "false", repo.values[service.SettingKeyCodexHistoryFilterEnabled])
}
