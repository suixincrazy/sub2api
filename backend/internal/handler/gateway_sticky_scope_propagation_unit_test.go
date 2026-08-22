//go:build unit

package handler

import (
	"bytes"
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// stickyScopeRecorderCache 记下 handler 查粘性绑定时用的键，用来和写进 ctx 的键比对。
type stickyScopeRecorderCache struct {
	lookupGroupIDs []int64
	lookupKeys     []string
}

func (c *stickyScopeRecorderCache) GetSessionAccountID(_ context.Context, groupID int64, sessionHash string) (int64, error) {
	c.lookupGroupIDs = append(c.lookupGroupIDs, groupID)
	c.lookupKeys = append(c.lookupKeys, sessionHash)
	return 0, service.ErrStickySessionNotFound
}

func (c *stickyScopeRecorderCache) SetSessionAccountID(context.Context, int64, string, int64, time.Duration) error {
	return nil
}
func (c *stickyScopeRecorderCache) RefreshSessionTTL(context.Context, int64, string, time.Duration) error {
	return nil
}
func (c *stickyScopeRecorderCache) DeleteSessionAccountID(context.Context, int64, string) error {
	return nil
}
func (c *stickyScopeRecorderCache) SetGrokVideoPendingBilling(context.Context, string, []byte, time.Duration) error {
	return nil
}
func (c *stickyScopeRecorderCache) GetGrokVideoPendingBilling(context.Context, string) ([]byte, error) {
	return nil, nil
}
func (c *stickyScopeRecorderCache) ClaimGrokVideoBilled(context.Context, string, time.Duration) (bool, error) {
	return true, nil
}
func (c *stickyScopeRecorderCache) ReleaseGrokVideoBilled(context.Context, string) error { return nil }
func (c *stickyScopeRecorderCache) SetReasoningContent(context.Context, string, string, time.Duration) error {
	return nil
}
func (c *stickyScopeRecorderCache) GetReasoningContent(context.Context, string) (string, error) {
	return "", service.ErrReasoningContentNotFound
}

// handler 必须在选号之前把粘性会话坐标写进 ctx，否则转发层的
// StickySessionScopeFromContext 永远返回 ok=false，「连续可疑短回合就解绑」整套机制
// 是死代码：会话会被永久钉在那个只吐一句就收尾的账号上，表现为「一直断流，自动切换
// 没起作用」。这条断言钉住的是这段接线本身——service 包的测试自己注入 ctx，抓不到它。
//
// 键必须与 handler 查绑定时用的完全一致：解绑和查绑定用的不是同一个键，
// 就等于解了个不存在的绑定，故障照旧。
func TestGatewayHandlerMessages_PropagatesStickySessionScopeToForwardLayer(t *testing.T) {
	gin.SetMode(gin.TestMode)

	groupID := int64(2003)
	accountID := int64(1003)

	group := &service.Group{
		ID:       groupID,
		Hydrated: true,
		Platform: service.PlatformAnthropic,
		Status:   service.StatusActive,
	}

	// 复用 warmup 拦截账号：它能在不接上游的前提下走完选号并成功返回，
	// 而 ctx 写入发生在选号之前，因此拦截路径足以观测到这段接线。
	account := &service.Account{
		ID:       accountID,
		Name:     "sticky-scope-1",
		Platform: service.PlatformAntigravity,
		Type:     service.AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token":              "tok_xxx",
			"intercept_warmup_requests": true,
		},
		Extra: map[string]any{
			"mixed_scheduling": true,
		},
		Concurrency:   1,
		Priority:      1,
		Status:        service.StatusActive,
		Schedulable:   true,
		AccountGroups: []service.AccountGroup{{AccountID: accountID, GroupID: groupID}},
	}

	cache := &stickyScopeRecorderCache{}
	h, cleanup := newTestGatewayHandler(t, group, []*service.Account{account}, cache)
	defer cleanup()

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	body := []byte(`{
		"model": "claude-sonnet-4-5",
		"max_tokens": 256,
		"messages": [{"role":"user","content":[{"type":"text","text":"Warmup"}]}]
	}`)
	req := httptest.NewRequest("POST", "/v1/messages", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "claude-cli/2.0.0")
	req = req.WithContext(context.WithValue(req.Context(), ctxkey.Group, group))
	c.Request = req

	apiKey := &service.APIKey{
		ID:      3003,
		UserID:  4003,
		GroupID: &groupID,
		Status:  service.StatusActive,
		User: &service.User{
			ID:          4003,
			Concurrency: 10,
			Balance:     100,
		},
		Group: group,
	}
	c.Set(string(middleware.ContextKeyAPIKey), apiKey)
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: apiKey.UserID, Concurrency: 10})

	h.Messages(c)
	require.Equal(t, 200, rec.Code)

	require.NotEmpty(t, cache.lookupKeys, "handler 应当查过一次粘性绑定")
	scopeGroupID, sessionKey, ok := service.StickySessionScopeFromContext(c.Request.Context())
	require.True(t, ok, "粘性会话坐标没进 ctx，转发层将无法解绑")
	require.Equal(t, cache.lookupKeys[0], sessionKey, "解绑用的键必须与查绑定用的键一致")
	require.Equal(t, cache.lookupGroupIDs[0], scopeGroupID, "分组必须一致，否则会打到别的分组的同名会话")
	require.Equal(t, groupID, scopeGroupID)
}
