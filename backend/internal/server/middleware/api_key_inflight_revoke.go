package middleware

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// errAPIKeyRevokedInflight 让网关的流式循环走既有的「客户端断开」分支：
// 停止向客户端写、继续把上游读完以完成计费。
var errAPIKeyRevokedInflight = errors.New("api key revoked during request")

type apiKeyRevocableWriter struct {
	gin.ResponseWriter
	revoked atomic.Bool
}

func (w *apiKeyRevocableWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (w *apiKeyRevocableWriter) Write(data []byte) (int, error) {
	if w.revoked.Load() {
		return 0, errAPIKeyRevokedInflight
	}
	return w.ResponseWriter.Write(data)
}

func (w *apiKeyRevocableWriter) WriteString(data string) (int, error) {
	if w.revoked.Load() {
		return 0, errAPIKeyRevokedInflight
	}
	return w.ResponseWriter.WriteString(data)
}

// trackAPIKeyInflight 在鉴权通过后登记请求：Key 在请求进行中被停用/删除时，
// 后续写入失败并取消请求上下文（非流式上游随之中止）。返回值须在 c.Next() 之后调用。
func trackAPIKeyInflight(c *gin.Context, apiKeyService *service.APIKeyService, key string) func() {
	if c == nil || c.Request == nil || apiKeyService == nil || key == "" {
		return func() {}
	}
	ctx, cancel := context.WithCancel(c.Request.Context())
	c.Request = c.Request.WithContext(ctx)
	writer := &apiKeyRevocableWriter{ResponseWriter: c.Writer}
	c.Writer = writer
	release := apiKeyService.TrackInflightRequest(key, func() {
		writer.revoked.Store(true)
		cancel()
	})
	return func() {
		release()
		cancel()
	}
}
