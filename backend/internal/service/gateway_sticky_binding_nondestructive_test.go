package service

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// readFailureGatewayCache 模拟 Redis 读失败：返回非 sentinel 错误，
// 用于验证读不到既有绑定时选号阶段保守不写。
type readFailureGatewayCache struct {
	*schedulerTestGatewayCache
}

func (c *readFailureGatewayCache) GetSessionAccountID(context.Context, int64, string) (int64, error) {
	return 0, errors.New("redis read failed")
}

// 主号临时不可用（safeguards 冷却 / gate_check / RPM 红线 / 槽位满）时，选号阶段
// 会落到负载感知分支并选出副号。此时绝不能用副号覆盖仍然存在的原始粘性绑定，
// 否则主号冷却结束后 cache_lookup 永远读到副号，会话再也切不回主号。
// 账号被永久清理走 account_cleared 分支先删除绑定，届时无既有绑定可正常改绑。
func TestGatewayStickyBindingDuringSelectionIsNonDestructive(t *testing.T) {
	groupID := int64(8)
	primaryID := int64(5)
	fallbackID := int64(3)
	const sessionHash = "anthropic-failover"

	t.Run("temporary primary outage keeps the primary binding", func(t *testing.T) {
		cache := &schedulerTestGatewayCache{sessionBindings: map[string]int64{sessionHash: primaryID}}
		svc := &GatewayService{cache: cache}
		require.NoError(t, svc.bindGatewayStickySessionDuringSelection(context.Background(), &groupID, sessionHash, fallbackID))
		require.Equal(t, primaryID, cache.sessionBindings[sessionHash],
			"主号临时不可用时回退账号不得夺走粘性绑定，否则主号恢复后切不回")
	})

	t.Run("fresh session binds the selected account", func(t *testing.T) {
		cache := &schedulerTestGatewayCache{sessionBindings: map[string]int64{}}
		svc := &GatewayService{cache: cache}
		require.NoError(t, svc.bindGatewayStickySessionDuringSelection(context.Background(), &groupID, "fresh", fallbackID))
		require.Equal(t, fallbackID, cache.sessionBindings["fresh"], "无既有绑定的新会话应正常建立粘性")
	})

	t.Run("same account refreshes its own binding", func(t *testing.T) {
		cache := &schedulerTestGatewayCache{sessionBindings: map[string]int64{sessionHash: fallbackID}}
		svc := &GatewayService{cache: cache}
		require.NoError(t, svc.bindGatewayStickySessionDuringSelection(context.Background(), &groupID, sessionHash, fallbackID))
		require.Equal(t, fallbackID, cache.sessionBindings[sessionHash], "同一账号应正常续期自己的绑定")
	})

	t.Run("read failure still binds", func(t *testing.T) {
		// 读失败时无法判断是否存在既有绑定。此时仍然绑定：真正需要保护的
		// 「回退不覆盖主号」场景由 preserveStickyBindingForFailover 的 ctx 标记
		// 单独兜住（见下一个子用例），而放弃绑定会让缓存抖动期间整段会话彻底
		// 失去粘性，代价比偶发覆盖更大——覆盖还能被粘性抢占在下一次请求切回。
		cache := &readFailureGatewayCache{schedulerTestGatewayCache: &schedulerTestGatewayCache{sessionBindings: map[string]int64{}}}
		svc := &GatewayService{cache: cache}
		require.NoError(t, svc.bindGatewayStickySessionDuringSelection(context.Background(), &groupID, "absent", fallbackID))
		require.Equal(t, fallbackID, cache.sessionBindings["absent"], "读失败不应导致会话完全失去粘性")
	})

	t.Run("failover ctx marker protects the primary even when read fails", func(t *testing.T) {
		cache := &readFailureGatewayCache{schedulerTestGatewayCache: &schedulerTestGatewayCache{sessionBindings: map[string]int64{}}}
		svc := &GatewayService{cache: cache}
		ctx := preserveStickyBindingForFailover(context.Background(), primaryID,
			map[int64]struct{}{primaryID: {}})
		require.NoError(t, svc.bindGatewayStickySessionDuringSelection(ctx, &groupID, "absent", fallbackID))
		require.NotContains(t, cache.sessionBindings, "absent", "主号因报错被排除时，回退账号不得建立绑定")
	})
}

// OpenAI 侧同分组主号回退共享同一约束（粘性键带 openai: 前缀）。
func TestOpenAIStickyBindingDuringSelectionIsNonDestructive(t *testing.T) {
	groupID := int64(2)
	primaryID := int64(4)
	fallbackID := int64(1)
	const sessionHash = "openai-failover"
	const cacheKey = "openai:" + sessionHash

	t.Run("temporary primary outage keeps the primary binding", func(t *testing.T) {
		cache := &schedulerTestGatewayCache{sessionBindings: map[string]int64{cacheKey: primaryID}}
		svc := &OpenAIGatewayService{cache: cache}
		require.NoError(t, svc.bindOpenAIStickySessionDuringSelection(context.Background(), &groupID, sessionHash, fallbackID))
		require.Equal(t, primaryID, cache.sessionBindings[cacheKey],
			"主号临时不可用时回退账号不得夺走粘性绑定")
	})

	t.Run("fresh session binds the selected account", func(t *testing.T) {
		cache := &schedulerTestGatewayCache{sessionBindings: map[string]int64{}}
		svc := &OpenAIGatewayService{cache: cache}
		require.NoError(t, svc.bindOpenAIStickySessionDuringSelection(context.Background(), &groupID, sessionHash, fallbackID))
		require.Equal(t, fallbackID, cache.sessionBindings[cacheKey], "无既有绑定的新会话应正常建立粘性")
	})
}
