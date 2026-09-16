//go:build unit

package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 常规链路（handleStreamingResponse）零暴露持流的回归锚点。
//
// 为什么这条链路必须单独有一套用例：线上 4 个 anthropic apikey 账号的
// extra.anthropic_passthrough 全部缺席，IsAnthropicAPIKeyPassthroughEnabled() 恒为假，
// 于是 forwardOnce 从不进透传分支 —— 100% 的生产流量走的是这里。此前所有持流修复
// （零暴露、四条截止线、丢弃预算、长思考放宽）都只落在 gateway_anthropic_passthrough.go
// 里，生产从未执行过那份代码。征状：anthropic_short_turn_streak_unbind 的 103 行日志
// 全是 disposition=delivered，一条 discarded 都没有。
//
// 与透传那套用例同口径，每条都必须同时断言两件事：返回 *UpstreamFailoverError，
// **并且** rec.Body 一个字节都没有。少了后者就退回「事后归因」，改动等于没做。

// newRegularHoldbackTestGatewayService 造一个带持流窗口的常规链路 service。
// 与 newShortTurnTestGatewayService 的唯一区别就是 AnthropicHoldbackWindowMs：
// 那个 helper 不配窗口，所以它那批用例全程跑在持流关闭的路径上。
func newRegularHoldbackTestGatewayService(t *testing.T, cache GatewayCache, windowMs int) (*GatewayService, *streamTruncationRepoStub) {
	t.Helper()
	rl, repo, _ := newStreamTruncationRateLimitService(t, refusalPenaltySettings)
	return &GatewayService{
		cfg: &config.Config{Gateway: config.GatewayConfig{
			MaxLineSize:               defaultMaxLineSize,
			AnthropicHoldbackWindowMs: windowMs,
		}},
		rateLimitService: rl,
		cache:            cache,
	}, repo
}

// runRegularHoldback 跑一次常规链路转发，把错误与已写出的字节都交回调用方。
func runRegularHoldback(
	t *testing.T, svc *GatewayService, groupID int64, sessionKey string,
	accountID int64, resp *http.Response,
) (*httptest.ResponseRecorder, *gin.Context, error) {
	t.Helper()
	c, rec := newRefusalTestContext(t)
	ctx := WithStickySessionScope(context.Background(), groupID, sessionKey, false)
	_, err := svc.handleStreamingResponse(
		ctx, resp, c, &Account{ID: accountID, Name: "relay", Platform: PlatformAnthropic},
		time.Now(), "claude-opus-5", "claude-opus-5", false)
	return rec, c, err
}

// findOpsEvent 在事件列表里定位指定 Kind 的那一条。
// disposition 只写进 Detail（见 reportAnthropicShortTurnUnbind / reportAnthropicBlockOrderViolation），
// 而且报告器事件排在 failover 事件之前，所以不能只看 events 的最后一条。
func findOpsEvent(t *testing.T, events []*OpsUpstreamErrorEvent, kind string) *OpsUpstreamErrorEvent {
	t.Helper()
	for _, ev := range events {
		if ev != nil && ev.Kind == kind {
			return ev
		}
	}
	require.Failf(t, "事件列表缺少期望的 Kind", "kind=%s events=%+v", kind, events)
	return nil
}

// 核心用例：合法短回合在常规链路上必须在客户端看到任何字节之前被丢弃并换号。
//
// 这正是生产上一直没治好的那一类：end_turn + output_tokens=9~16 + 一句开场白，
// duration_ms ≈ first_token_ms（首字节就已经是终态）。透传链路 2026-08-25 起就能
// discard 它，常规链路直到本次改动前只能 delivered。
func TestRegularPathHoldback_ShortTurnDiscardedBeforeCommit(t *testing.T) {
	const sessionKey = "regular-holdback-short"
	const groupID = int64(1)
	cache := newShortTurnStreakCache(sessionKey, 4)
	svc, repo := newRegularHoldbackTestGatewayService(t, cache, 3000)

	rec, c, err := runRegularHoldback(t, svc, groupID, sessionKey, 4,
		shortTurnSSE("好的，我来看一下这个问题。", 9, false))

	require.Error(t, err)
	var failover *UpstreamFailoverError
	require.ErrorAs(t, err, &failover, "疑似截断必须换号，不能把响应交付给客户端")
	require.Empty(t, rec.Body.String(), "零暴露：丢弃的响应一个字节都不能漏给客户端")
	require.Zero(t, rec.Body.Len(), "无字节写出时不得钉死状态码")

	// 解绑 + 冷却，与透传链路同口径：解绑只治下一发，账号本身还得进冷却。
	require.Equal(t, 1, cache.deletedSessions[sessionKey], "丢弃那一刻必须解除粘性绑定")
	require.Equal(t, 1, repo.tempCalls, "解绑那一刻必须冷却账号")

	// 归因必须写成 discarded：这是与 delivered 的分水岭，也是线上验真的唯一凭据。
	events := opsEvents(t, c)
	require.NotEmpty(t, events)
	require.Contains(t, findOpsEvent(t, events, "short_turn_streak_unbind").Detail, "disposition=discarded",
		"持流拦下的必须记 disposition=discarded，否则线上分不清有没有真生效")
}

// 正常成段回答必须原样交付，且字节完整：持流只改变**什么时候**写，不改变写什么。
func TestRegularPathHoldback_NormalTurnDeliveredIntact(t *testing.T) {
	const sessionKey = "regular-holdback-normal"
	const groupID = int64(1)
	cache := newShortTurnStreakCache(sessionKey, 4)
	svc, repo := newRegularHoldbackTestGatewayService(t, cache, 3000)

	long := strings.Repeat("正常长度的回答内容。", 80)
	rec, _, err := runRegularHoldback(t, svc, groupID, sessionKey, 4, shortTurnSSE(long, 900, false))

	require.NoError(t, err)
	body := rec.Body.String()
	require.Contains(t, body, "message_start", "缓冲区必须原样放行，不能吞掉前置帧")
	require.Contains(t, body, "text_delta")
	require.Contains(t, body, "message_stop", "终止帧必须到达客户端")
	require.Zero(t, cache.deletedSessions[sessionKey], "正常回合不得解绑")
	require.Zero(t, repo.tempCalls, "正常回合不得罚号")
}

// tool_use 回合不得被丢弃：开了工具块的短回合是标准 agent 行为。
// 注意判据要等 stop_reason 才生效（67108eb6d 那一笔的修正），所以这里用完整流验。
func TestRegularPathHoldback_ToolUseTurnDelivered(t *testing.T) {
	const sessionKey = "regular-holdback-tooluse"
	const groupID = int64(1)
	cache := newShortTurnStreakCache(sessionKey, 4)
	svc, repo := newRegularHoldbackTestGatewayService(t, cache, 3000)

	rec, _, err := runRegularHoldback(t, svc, groupID, sessionKey, 4, shortTurnSSE("我来查一下", 30, true))

	require.NoError(t, err)
	require.Contains(t, rec.Body.String(), "tool_use", "工具回合必须原样交付")
	require.Zero(t, cache.deletedSessions[sessionKey], "工具回合不得解绑")
	require.Zero(t, repo.tempCalls)
}

// 块序违规走自己那条出口：判据是确定性的，归因不能混进短回合那一档。
func TestRegularPathHoldback_BlockOrderViolationDiscarded(t *testing.T) {
	const sessionKey = "regular-holdback-blockorder"
	const groupID = int64(1)
	cache := newShortTurnStreakCache(sessionKey, 4)
	svc, repo := newRegularHoldbackTestGatewayService(t, cache, 3000)

	rec, c, err := runRegularHoldback(t, svc, groupID, sessionKey, 4,
		blockOrderViolationSSE("upstream_error"))

	require.Error(t, err)
	var failover *UpstreamFailoverError
	require.ErrorAs(t, err, &failover)
	require.Empty(t, rec.Body.String(), "零暴露：伪造的工具结果一个字节都不能漏出去")
	require.Equal(t, 1, cache.deletedSessions[sessionKey])
	require.Equal(t, 1, repo.tempCalls)

	events := opsEvents(t, c)
	require.NotEmpty(t, events)
	violation := findOpsEvent(t, events, "block_order_violation")
	require.Contains(t, violation.Detail, "disposition=discarded")
}

// 窗口配 0 必须逐位退化成改动前的行为：交付 + 事后归因（delivered）。
// 这一条守的是「持流可以随时关掉」这个性质，也是上线的退路。
func TestRegularPathHoldback_WindowZeroKeepsLegacyBehavior(t *testing.T) {
	const sessionKey = "regular-holdback-off"
	const groupID = int64(1)
	cache := newShortTurnStreakCache(sessionKey, 4)
	svc, repo := newRegularHoldbackTestGatewayService(t, cache, 0)

	rec, c, err := runRegularHoldback(t, svc, groupID, sessionKey, 4,
		shortTurnSSE("好的，我来看一下这个问题。", 9, false))

	require.NoError(t, err, "关掉持流时不再 failover")
	require.Contains(t, rec.Body.String(), "message_stop", "关掉持流时照旧交付")
	require.Equal(t, 1, cache.deletedSessions[sessionKey], "事后归因仍要解绑下一发")
	require.Equal(t, 1, repo.tempCalls)

	events := opsEvents(t, c)
	require.NotEmpty(t, events)
	require.Contains(t, findOpsEvent(t, events, "short_turn_streak_unbind").Detail, "disposition=delivered",
		"关掉持流后只有 delivered 一种结局")
}
