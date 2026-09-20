package routes

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type codexHistoryRouteRepo struct{ service.SettingRepository }

func (codexHistoryRouteRepo) GetValue(_ context.Context, key string) (string, error) {
	if key == service.SettingKeyCodexHistoryFilterEnabled {
		return "true", nil
	}
	return "", service.ErrSettingNotFound
}

func TestNativeCodexHistoryFilterMountedOnAllResponsesAliases(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	settings := service.NewSettingService(codexHistoryRouteRepo{}, nil)
	RegisterGatewayRoutes(router, &handler.Handlers{
		Gateway: &handler.GatewayHandler{}, OpenAIGateway: &handler.OpenAIGatewayHandler{},
		AsyncImage: handler.NewAsyncImageHandler(nil, nil),
	}, middleware.APIKeyAuthMiddleware(func(c *gin.Context) {
		groupID := int64(1)
		c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{GroupID: &groupID, Group: &service.Group{Platform: service.PlatformOpenAI}})
		c.Next()
	}), nil, nil, nil, settings, nil, &config.Config{Gateway: config.GatewayConfig{MaxBodySize: 1 << 20}})
	for _, prefix := range []string{"", "/v1", "/backend-api/codex"} {
		for _, tt := range []struct {
			method, suffix, body, code string
			status                     int
		}{
			{http.MethodPost, "/responses", `{"previous_response_id":"old"}`, "response_reference_not_portable", 400},
			{http.MethodPost, "/responses/", `{"input":[{"type":"item_reference","id":"old"}]}`, "encrypted_context_not_portable", 400},
			{http.MethodPost, "/responses/compact", `{}`, "encrypted_compaction_disabled", 409},
			{http.MethodGet, "/responses", ``, "websocket_filtering_unsupported", 426},
			{http.MethodPost, "/responses/%20", `{"previous_response_id":"old"}`, "response_reference_not_portable", 400},
			{http.MethodPost, "/responses/compact%20", `{}`, "encrypted_compaction_disabled", 409},
			{http.MethodPost, "/responses/compact/detail", `{}`, "encrypted_compaction_disabled", 409},
			{http.MethodPost, "/responses/responsesX", `{"previous_response_id":"old"}`, "response_reference_not_portable", 400},
		} {
			response := httptest.NewRecorder()
			req := httptest.NewRequest(tt.method, prefix+tt.suffix, strings.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(response, req)
			require.Equal(t, tt.status, response.Code, "%s %s: %s", tt.method, prefix+tt.suffix, response.Body.String())
			require.Contains(t, response.Body.String(), tt.code)
		}
	}
	status, err := settings.GetCodexHistoryFilterStatus(context.Background())
	require.NoError(t, err)
	require.EqualValues(t, 24, status.Stats.BlockedRequests)
}
