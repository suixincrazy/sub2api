package routes

import (
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/gin-gonic/gin"
)

func TestMyAccountModelCapabilityRoutesStayUnderUserScope(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	accounts := router.Group("/api/v1/my/accounts")
	registerMyAccountModelCapabilityRoutes(accounts, handler.NewMyResourceHandler(nil, nil))

	registered := make(map[string]bool)
	for _, route := range router.Routes() {
		registered[route.Method+" "+route.Path] = true
		if strings.Contains(route.Path, "/admin/") {
			t.Fatalf("user capability route registered under admin scope: %#v", route)
		}
	}

	for _, expected := range []string{
		"POST /api/v1/my/accounts/models/sync-upstream-preview",
		"POST /api/v1/my/accounts/:id/models/sync-upstream",
		"GET /api/v1/my/accounts/oauth/gemini/capabilities",
		"GET /api/v1/my/accounts/antigravity/default-model-mapping",
	} {
		if !registered[expected] {
			t.Fatalf("missing route %s; got %#v", expected, registered)
		}
	}
}
