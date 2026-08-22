package service

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// shortTurnStreakCache 是实现了可选扩展 stickyShortTurnStreakStore 的网关缓存替身，
// 对应线上真实的 *gatewayCache。
type shortTurnStreakCache struct {
	*schedulerTestGatewayCache
	streaks map[string]int64
	resets  map[string]int
	ttls    []time.Duration
}

func newShortTurnStreakCache(sessionKey string, boundAccountID int64) *shortTurnStreakCache {
	return &shortTurnStreakCache{
		schedulerTestGatewayCache: &schedulerTestGatewayCache{
			sessionBindings: map[string]int64{sessionKey: boundAccountID},
		},
		streaks: map[string]int64{},
		resets:  map[string]int{},
	}
}

func shortTurnStreakKey(groupID int64, sessionHash string) string {
	return fmt.Sprintf("%d:%s", groupID, sessionHash)
}

func (c *shortTurnStreakCache) IncrStickyShortTurnStreak(_ context.Context, groupID int64, sessionHash string, ttl time.Duration) (int64, error) {
	c.ttls = append(c.ttls, ttl)
	key := shortTurnStreakKey(groupID, sessionHash)
	c.streaks[key]++
	return c.streaks[key], nil
}

func (c *shortTurnStreakCache) ResetStickyShortTurnStreak(_ context.Context, groupID int64, sessionHash string) error {
	key := shortTurnStreakKey(groupID, sessionHash)
	delete(c.streaks, key)
	c.resets[key]++
	return nil
}

// newShortTurnTestGatewayService 与 newRefusalTestGatewayService 同源，只是额外挂上缓存：
// 解绑走的是 s.cache，不挂缓存这条路径整体是 no-op。
func newShortTurnTestGatewayService(t *testing.T, cache GatewayCache) (*GatewayService, *streamTruncationRepoStub) {
	t.Helper()
	rl, repo, _ := newStreamTruncationRateLimitService(t, refusalPenaltySettings)
	return &GatewayService{
		cfg:              &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}},
		rateLimitService: rl,
		cache:            cache,
	}, repo
}

// shortTurnSSE 拼一条协议完全合法的流：message_start → 正文块 → text_delta → message_delta
// {stop_reason:end_turn} → message_stop。这正是线上那次「不报错的断流」的形状。
func shortTurnSSE(text string, outputTokens int, toolUse bool) *http.Response {
	blockStart := `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`
	if toolUse {
		blockStart = `data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t1","name":"Bash","input":{}}}`
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: sseBody(
			`data: {"type":"message_start","message":{"usage":{"input_tokens":129000}}}`,
			"",
			blockStart,
			"",
			fmt.Sprintf(`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":%q}}`, text),
			"",
			fmt.Sprintf(`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":%d}}`, outputTokens),
			"",
			`data: {"type":"message_stop"}`,
			"",
		),
	}
}

// runShortTurnPassthrough 跑一次带粘性会话坐标的透传。每发都用新的 gin 上下文，
// 与线上「客户端下一发是一个全新请求」一致。
func runShortTurnPassthrough(
	t *testing.T, svc *GatewayService, groupID int64, sessionKey string,
	accountID int64, resp *http.Response,
) {
	t.Helper()
	c, _ := newRefusalTestContext(t)
	ctx := WithStickySessionScope(context.Background(), groupID, sessionKey, false)
	_, err := svc.handleStreamingResponseAnthropicAPIKeyPassthrough(
		ctx, resp, c, &Account{ID: accountID, Name: "primary", Platform: PlatformAnthropic},
		time.Now(), "claude-opus-5")
	require.NoError(t, err)
}

// 纯判定的边界。这是唯一决定「哪种回合算可疑」的地方，放宽任何一条都会开始误伤
// 合法回答的 prompt-cache 亲和性，收紧则漏掉线上实际发生的那种断流。
func TestAnthropicTurnLooksSuspiciouslyShort(t *testing.T) {
	cases := []struct {
		name         string
		stopReason   string
		visibleChars int
		outputTokens int
		sawToolUse   bool
		suspicious   bool
	}{
		// 线上实测的两次故障：end_turn + 十几到三十个 output_token + 一句开场白就收尾。
		{"实测故障形态 output_tokens=30", "end_turn", 60, 30, false, true},
		{"实测故障形态 output_tokens=19", "end_turn", 41, 19, false, true},
		{"大小写与空格不影响判定", "  End_Turn  ", 60, 30, false, true},
		{"刚好卡在上限仍算可疑", "end_turn", anthropicShortTurnVisibleCharLimit, 50, false, true},

		// 超过上限就是正常成段回答。
		{"超过上限不算可疑", "end_turn", anthropicShortTurnVisibleCharLimit + 1, 50, false, false},

		// 其余 stop_reason 都自带「模型确实干了活 / 被限制截断」的语义。
		{"tool_use 不算", "tool_use", 20, 10, false, false},
		{"max_tokens 不算", "max_tokens", 20, 10, false, false},
		{"stop_sequence 不算", "stop_sequence", 20, 10, false, false},
		{"pause_turn 不算", "pause_turn", 20, 10, false, false},
		{"异常 stop_reason 归确定性判定管", "upstream_error", 20, 10, false, false},
		{"空 stop_reason 不算", "", 20, 10, false, false},

		// 开了工具块的短回合是标准 agent 行为，绝不能解绑——否则每次工具调用都在打断粘性。
		{"开了 tool_use 块不算", "end_turn", 20, 10, true, false},

		// output_tokens<=0 与零可见字符归 anthropicStreamLooksIncompleteDespiteTerminal，
		// 在这里放行，避免同一次故障被两套逻辑各记一次。
		{"output_tokens 为零不算", "end_turn", 20, 0, false, false},
		{"output_tokens 为负不算", "end_turn", 20, -1, false, false},
		{"零可见字符不算", "end_turn", 0, 30, false, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.suspicious, anthropicTurnLooksSuspiciouslyShort(
				tc.stopReason, tc.visibleChars, tc.outputTokens, tc.sawToolUse))
		})
	}
}

// 端到端：两条信号都放行的「协议合法但疑似没把话说完」，连续两发后必须解除粘性绑定，
// 让客户端的下一发重新选号落到同优先级的另一个账号上。
//
// 这就是修复的核心：stop_reason=end_turn 在白名单里（信号 1 放行）、output_tokens=30>0
// （信号 2 要求 <=0，放行），旧代码判为健康 → 不罚号 → 粘性把下一发原样送回同一个账号，
// 表现为「一直断流，自动切回可用账号没起作用」。
func TestAnthropicPassthrough_ShortTurnStreakUnbindsStickySession(t *testing.T) {
	const sessionKey = "sticky-short-turn"
	const groupID = int64(1)
	cache := newShortTurnStreakCache(sessionKey, 9)
	svc, repo := newShortTurnTestGatewayService(t, cache)

	// 第一发：只累计，不动绑定。合法的短回答确实存在，一发就解绑会频繁丢 prompt cache。
	runShortTurnPassthrough(t, svc, groupID, sessionKey, 9, shortTurnSSE("好的，我来看一下这个问题。", 30, false))
	require.Equal(t, int64(1), cache.streaks[shortTurnStreakKey(groupID, sessionKey)])
	require.Equal(t, int64(9), cache.sessionBindings[sessionKey], "第一发不得解绑")
	require.Zero(t, cache.deletedSessions[sessionKey])

	// 第二发：达到阈值，解绑。
	runShortTurnPassthrough(t, svc, groupID, sessionKey, 9, shortTurnSSE("好的，我来看一下这个问题。", 30, false))
	require.Equal(t, 1, cache.deletedSessions[sessionKey], "连续两发可疑短回合必须解除粘性绑定")
	require.NotContains(t, cache.sessionBindings, sessionKey)

	// 解绑后连击数必须清零，否则下一发换到好账号上答一句短话就又达标，会来回解绑。
	require.NotContains(t, cache.streaks, shortTurnStreakKey(groupID, sessionKey))
	require.Equal(t, 1, cache.resets[shortTurnStreakKey(groupID, sessionKey)])

	// 全程不得罚号：判据是启发式的，罚号会把账号从所有会话的调度池里摘掉，误判代价太大。
	require.Zero(t, repo.tempCalls, "可疑短回合只解绑不罚号")

	// TTL 必须跟粘性绑定同寿，绑定都过期了连击数没有意义。
	for _, ttl := range cache.ttls {
		require.Equal(t, stickySessionTTL, ttl)
	}
}

// 一次正常回合必须清零连击数：否则跨越几小时的两次偶发短回答会被累加成一次误判。
func TestAnthropicPassthrough_NormalTurnResetsShortTurnStreak(t *testing.T) {
	const sessionKey = "sticky-reset"
	const groupID = int64(1)
	cache := newShortTurnStreakCache(sessionKey, 9)
	svc, _ := newShortTurnTestGatewayService(t, cache)

	runShortTurnPassthrough(t, svc, groupID, sessionKey, 9, shortTurnSSE("短回答", 30, false))
	require.Equal(t, int64(1), cache.streaks[shortTurnStreakKey(groupID, sessionKey)])

	// 正常成段回答：清零。
	long := strings.Repeat("正常长度的回答内容。", 40)
	runShortTurnPassthrough(t, svc, groupID, sessionKey, 9, shortTurnSSE(long, 800, false))
	require.NotContains(t, cache.streaks, shortTurnStreakKey(groupID, sessionKey), "正常回合必须清零连击数")

	// 再来一发短的：连击数重新从 1 开始，不得解绑。
	runShortTurnPassthrough(t, svc, groupID, sessionKey, 9, shortTurnSSE("短回答", 30, false))
	require.Equal(t, int64(1), cache.streaks[shortTurnStreakKey(groupID, sessionKey)])
	require.Zero(t, cache.deletedSessions[sessionKey], "被正常回合隔开的两次短回答不构成连击")
	require.Equal(t, int64(9), cache.sessionBindings[sessionKey])
}

// 开了 tool_use 块的短回合是标准 agent 行为，不得累计——否则每一次工具调用都在打断粘性。
func TestAnthropicPassthrough_ToolUseShortTurnNotCounted(t *testing.T) {
	const sessionKey = "sticky-tool-use"
	const groupID = int64(1)
	cache := newShortTurnStreakCache(sessionKey, 9)
	svc, _ := newShortTurnTestGatewayService(t, cache)

	for i := 0; i < 3; i++ {
		runShortTurnPassthrough(t, svc, groupID, sessionKey, 9, shortTurnSSE("我来查一下", 30, true))
	}
	require.Empty(t, cache.streaks, "工具调用回合不得累计连击")
	require.Zero(t, cache.deletedSessions[sessionKey])
	require.Equal(t, int64(9), cache.sessionBindings[sessionKey])
}

// 没有粘性会话坐标（无 session hash 的请求）时整条路径必须是 no-op：没有绑定可解。
func TestAnthropicPassthrough_ShortTurnWithoutStickyScopeIsNoop(t *testing.T) {
	const sessionKey = "sticky-absent"
	cache := newShortTurnStreakCache(sessionKey, 9)
	svc, repo := newShortTurnTestGatewayService(t, cache)

	for i := 0; i < 3; i++ {
		c, _ := newRefusalTestContext(t)
		_, err := svc.handleStreamingResponseAnthropicAPIKeyPassthrough(
			context.Background(), shortTurnSSE("短回答", 30, false), c,
			&Account{ID: 9, Name: "primary", Platform: PlatformAnthropic}, time.Now(), "claude-opus-5")
		require.NoError(t, err)
	}
	require.Empty(t, cache.streaks)
	require.Zero(t, cache.deletedSessions[sessionKey])
	require.Zero(t, repo.tempCalls)
}

// 不实现可选扩展的缓存必须静默退化成「不解绑」，即改动前的行为，而不是 panic。
// 可选接口的全部意义就在这里：GatewayCache 被大量替身实现，往里加方法会一次性弄坏所有 mock。
func TestAnthropicPassthrough_CacheWithoutStreakStoreDegradesSilently(t *testing.T) {
	const sessionKey = "sticky-no-store"
	const groupID = int64(1)
	plain := &schedulerTestGatewayCache{sessionBindings: map[string]int64{sessionKey: 9}}
	require.NotImplements(t, (*stickyShortTurnStreakStore)(nil), plain,
		"这个替身刻意不实现可选扩展，用于验证退化路径")

	svc, repo := newShortTurnTestGatewayService(t, plain)
	for i := 0; i < 3; i++ {
		runShortTurnPassthrough(t, svc, groupID, sessionKey, 9, shortTurnSSE("短回答", 30, false))
	}
	require.Equal(t, int64(9), plain.sessionBindings[sessionKey], "缺少计数能力时保持改动前行为")
	require.Zero(t, plain.deletedSessions[sessionKey])
	require.Zero(t, repo.tempCalls)
}

// 粘性会话坐标必须能穿过 ctx 送到转发层。少了这一步整套机制是死代码：
// StickySessionScopeFromContext 永远返回 ok=false，谁都不会解绑。
func TestStickySessionScopeRoundTrip(t *testing.T) {
	_, _, ok := StickySessionScopeFromContext(context.Background())
	require.False(t, ok, "没写入过就必须报缺失")

	ctx := WithStickySessionScope(context.Background(), 7, "sess-abc", false)
	groupID, sessionKey, ok := StickySessionScopeFromContext(ctx)
	require.True(t, ok)
	require.Equal(t, int64(7), groupID)
	require.Equal(t, "sess-abc", sessionKey)

	// 空 session key 不写入：没有粘性的请求本来就没有绑定可解。
	_, _, ok = StickySessionScopeFromContext(WithStickySessionScope(context.Background(), 7, "   ", false))
	require.False(t, ok)

	// 不得踩掉同一份 metadata 里的其他字段。
	ctx = WithSingleAccountRetry(WithStickySessionScope(context.Background(), 7, "sess-abc", false), true, false)
	_, _, ok = StickySessionScopeFromContext(ctx)
	require.True(t, ok)
	retry, ok := SingleAccountRetryFromContext(ctx)
	require.True(t, ok)
	require.True(t, retry)
}
