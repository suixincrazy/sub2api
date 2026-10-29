//go:build unit

package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 鉴权通过后 Key 被停用：在途请求的后续写入失败、请求上下文被取消。
func TestAPIKeyAuthRevokesInflightRequestWhenKeyDisabled(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var status atomic.Value
	status.Store(service.StatusActive)
	user := &service.User{ID: 7, Role: service.RoleUser, Status: service.StatusActive, Balance: 10, Concurrency: 3}
	apiKeyRepo := &stubApiKeyRepo{
		getByKey: func(ctx context.Context, key string) (*service.APIKey, error) {
			if key != "test-key" {
				return nil, service.ErrAPIKeyNotFound
			}
			return &service.APIKey{ID: 100, UserID: user.ID, Key: key, Status: status.Load().(string), User: user}, nil
		},
	}
	cfg := &config.Config{RunMode: config.RunModeSimple}
	apiKeyService := service.NewAPIKeyService(apiKeyRepo, nil, nil, nil, nil, nil, cfg)

	var firstErr, secondErr error
	var ctxCanceled bool
	router := gin.New()
	router.Use(gin.HandlerFunc(NewAPIKeyAuthMiddleware(apiKeyService, nil, cfg)))
	router.GET("/stream", func(c *gin.Context) {
		_, firstErr = c.Writer.WriteString("data: 1\n\n")
		c.Writer.Flush()

		status.Store("inactive")
		apiKeyService.InvalidateAuthCacheByKey(context.Background(), "test-key")
		select {
		case <-c.Request.Context().Done():
			ctxCanceled = true
		case <-time.After(2 * time.Second):
		}
		_, secondErr = c.Writer.WriteString("data: 2\n\n")
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/stream", nil)
	req.Header.Set("x-api-key", "test-key")
	router.ServeHTTP(w, req)

	require.NoError(t, firstErr)
	require.True(t, ctxCanceled, "request context must be canceled after the key is disabled")
	require.ErrorIs(t, secondErr, errAPIKeyRevokedInflight)
	require.Equal(t, "data: 1\n\n", w.Body.String())
}

// Key 未被停用时，失效（例如额度刷新）不影响在途请求。
func TestAPIKeyAuthKeepsInflightRequestOnUnrelatedInvalidation(t *testing.T) {
	gin.SetMode(gin.TestMode)

	user := &service.User{ID: 7, Role: service.RoleUser, Status: service.StatusActive, Balance: 10, Concurrency: 3}
	apiKeyRepo := &stubApiKeyRepo{
		getByKey: func(ctx context.Context, key string) (*service.APIKey, error) {
			return &service.APIKey{ID: 100, UserID: user.ID, Key: key, Status: service.StatusActive, User: user}, nil
		},
	}
	cfg := &config.Config{RunMode: config.RunModeSimple}
	apiKeyService := service.NewAPIKeyService(apiKeyRepo, nil, nil, nil, nil, nil, cfg)

	var writeErr error
	router := gin.New()
	router.Use(gin.HandlerFunc(NewAPIKeyAuthMiddleware(apiKeyService, nil, cfg)))
	router.GET("/stream", func(c *gin.Context) {
		apiKeyService.InvalidateAuthCacheByKey(context.Background(), "test-key")
		select {
		case <-c.Request.Context().Done():
		case <-time.After(200 * time.Millisecond):
		}
		_, writeErr = c.Writer.WriteString("ok")
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/stream", nil)
	req.Header.Set("x-api-key", "test-key")
	router.ServeHTTP(w, req)

	require.NoError(t, writeErr)
	require.Equal(t, "ok", w.Body.String())
}
