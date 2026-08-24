//go:build unit

package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// 零暴露持流的回归锚点。
//
// 要守住的核心事实：解绑只能让**下一发**换号，治不了当前这一发——检测量（stop_reason /
// output_tokens）落在 message_delta 上，而正文早在第一个 content_block_delta 就 flush
// 出去、200 已经钉死。持流把提交点推到「能判定」之后，于是疑似截断的响应可以在客户端
// 看到任何字节之前被丢掉并换号重试。
//
// 所以每条集成用例都必须同时断言两件事：返回的是 *UpstreamFailoverError，**并且**
// rec.Body 一个字节都没有。少了后者就退化成「事后归因」，改动等于没做。

func newHoldbackTestGatewayService(t *testing.T, cache GatewayCache, windowMs int) (*GatewayService, *streamTruncationRepoStub) {
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

// runHoldbackPassthrough 跑一次透传并把错误与已写出的字节都交回调用方。
// prepare 可用来在开跑前动 gin 上下文（例如预置「本次请求已重试过」的标记）。
func runHoldbackPassthrough(
	t *testing.T, svc *GatewayService, groupID int64, sessionKey string,
	accountID int64, resp *http.Response, prepare func(*gin.Context),
) (*httptest.ResponseRecorder, *gin.Context, error) {
	t.Helper()
	c, rec := newRefusalTestContext(t)
	if prepare != nil {
		prepare(c)
	}
	ctx := WithStickySessionScope(context.Background(), groupID, sessionKey, false)
	_, err := svc.handleStreamingResponseAnthropicAPIKeyPassthrough(
		ctx, resp, c, &Account{ID: accountID, Name: "primary", Platform: PlatformAnthropic},
		time.Now(), "claude-opus-5")
	return rec, c, err
}

// 三态判定的边界。这是唯一决定「攥着 / 放行 / 丢弃重试」的地方。
//
// 两条容易写错的顺序在这里被钉住：
//   - stop_reason 已知时优先于窗口。窗口只是「等不起了」的兜底，拿到全部判据就该直接定案，
//     否则一条 3 秒才吐完的截断会因为窗口先到而被放行。
//   - retryUsed 必须能一票否决 Discard。用户问的问题本来就只有一句话答案时，每个账号都会
//     给同样的短回合，不设上限会一路重试到调度耗尽，把一个完好的短回答变成 502。
func TestAnthropicHoldbackVerdict(t *testing.T) {
	cases := []struct {
		name         string
		windowGone   bool
		stopReason   string
		proseRunes   int
		outputTokens int
		toolUse      bool
		retryUsed    bool
		want         anthropicHoldbackDecision
	}{
		{
			name: "证据不足时继续攥着",
			want: anthropicHoldbackKeep,
		},
		{
			name: "正文刚起头也要继续攥着", proseRunes: 12, outputTokens: 4,
			want: anthropicHoldbackKeep,
		},
		{
			name: "tool_use 块立刻放行", toolUse: true, proseRunes: 3,
			want: anthropicHoldbackRelease,
		},
		{
			name: "正文越过短回合上限立刻放行", proseRunes: anthropicShortTurnProseRuneLimit + 1,
			want: anthropicHoldbackRelease,
		},
		{
			name: "正文正好卡在上限上还不能放行", proseRunes: anthropicShortTurnProseRuneLimit,
			want: anthropicHoldbackKeep,
		},
		{
			name:       "窗口耗尽而判据仍未到：放行，不能让客户端为启发式白等",
			windowGone: true, proseRunes: 50, outputTokens: 20,
			want: anthropicHoldbackRelease,
		},
		{
			name:       "拿到 stop_reason 且形态可疑：丢弃重试",
			stopReason: "end_turn", proseRunes: 131, outputTokens: 70,
			want: anthropicHoldbackDiscard,
		},
		// 账号 12 的形态：零正文 + output_tokens=1。原先这里判 Release，空响应直接交付。
		{
			name:       "零正文的 end_turn：丢弃重试",
			stopReason: "end_turn", proseRunes: 0, outputTokens: 1,
			want: anthropicHoldbackDiscard,
		},
		// 零正文不受 token 上限约束：思考 token 也计入 output_tokens。
		{
			name:       "零正文即使 token 上量也丢弃",
			stopReason: "end_turn", proseRunes: 0, outputTokens: 900,
			want: anthropicHoldbackDiscard,
		},
		// 判据没齐之前不能因为「暂时零正文」就丢：思考期、或正文还没开始吐的时候
		// proseRunes 本来就是 0，此刻丢弃会把每一条带思考的正常流都误杀。
		{
			name:       "零正文但 stop_reason 还没到：继续攥着",
			proseRunes: 0, outputTokens: 40,
			want: anthropicHoldbackKeep,
		},
		{
			name:       "零正文 + 窗口耗尽仍要放行，不能让客户端白等",
			windowGone: true, proseRunes: 0, outputTokens: 40,
			want: anthropicHoldbackRelease,
		},
		{
			name:       "零正文但已重试过一次：放行，避免一路重试到调度耗尽",
			stopReason: "end_turn", proseRunes: 0, outputTokens: 1, retryUsed: true,
			want: anthropicHoldbackRelease,
		},
		{
			name:       "stop_reason 优先于窗口：窗口已过但判据齐了仍然丢弃",
			windowGone: true, stopReason: "end_turn", proseRunes: 131, outputTokens: 70,
			want: anthropicHoldbackDiscard,
		},
		{
			name:       "已经重试过一次：同样的可疑形态必须放行，不能把短回答变成 502",
			stopReason: "end_turn", proseRunes: 131, outputTokens: 70, retryUsed: true,
			want: anthropicHoldbackRelease,
		},
		{
			name:       "token 上量的短正文是正常回答，放行",
			stopReason: "end_turn", proseRunes: 131, outputTokens: 200,
			want: anthropicHoldbackRelease,
		},
		{
			name:       "max_tokens 不是截断形态，放行",
			stopReason: "max_tokens", proseRunes: 131, outputTokens: 70,
			want: anthropicHoldbackRelease,
		},
		{
			name:       "tool_use 回合即使 token 很少也放行",
			stopReason: "tool_use", proseRunes: 20, outputTokens: 30, toolUse: true,
			want: anthropicHoldbackRelease,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := anthropicHoldbackVerdict(
				tc.windowGone, tc.stopReason, tc.proseRunes, tc.outputTokens, tc.toolUse, tc.retryUsed)
			require.Equal(t, tc.want, got)
		})
	}
}

// 观测器把「思考不算正文」和「窗口从原提交点起算」这两件事分开管。
//
// 长思考 + 一句正文恰恰是要抓的形态，所以 thinking_delta 不得计入 proseRunes；但它**确实**
// 是旧行为下的提交点，所以必须由它起算窗口——否则整段思考期都被攥着，客户端会长时间全黑
// （keepalive 在 !streamCommitted 下不写字节）。
func TestAnthropicHoldbackObserverSeparatesThinkingFromProse(t *testing.T) {
	base := time.Date(2026, 8, 24, 1, 0, 0, 0, time.UTC)
	o := &anthropicHoldbackObserver{}

	o.observe(gjson.Parse(`{"type":"message_start","message":{"usage":{"input_tokens":129000,"output_tokens":2}}}`), false, base)
	require.Equal(t, 2, o.outputTokens, "message_start 的初始 output_tokens 要收下")
	require.True(t, o.firstCommitPointAt.IsZero(), "message_start 不是提交点")

	o.observe(gjson.Parse(`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`), false, base)
	require.True(t, o.firstCommitPointAt.IsZero(), "空思考块的起始帧不提交，仍不起算窗口")

	thinking := base.Add(10 * time.Millisecond)
	o.observe(gjson.Parse(`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"我先想清楚因果链条"}}`), true, thinking)
	require.Zero(t, o.proseRunes, "思考链不是正文")
	require.Equal(t, thinking, o.firstCommitPointAt, "thinking_delta 是旧行为下的提交点，窗口从它起算")

	o.observe(gjson.Parse(`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"我看一下。"}}`), true, thinking.Add(time.Millisecond))
	require.Equal(t, 5, o.proseRunes, "正文按 rune 数")
	require.Equal(t, thinking, o.firstCommitPointAt, "提交点只记第一次")

	o.observe(gjson.Parse(`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":70}}`), false, thinking.Add(2*time.Millisecond))
	require.Equal(t, "end_turn", o.stopReason)
	require.Equal(t, 70, o.outputTokens, "message_delta 的终值必须盖掉 message_start 的初始值")

	require.False(t, o.windowElapsed(thinking.Add(2999*time.Millisecond), 3*time.Second))
	require.True(t, o.windowElapsed(thinking.Add(3*time.Second), 3*time.Second))
	require.False(t, o.windowElapsed(thinking.Add(time.Hour), 0), "窗口配 0 时永不耗尽")
}

// 还没到提交点就不该起算窗口，否则一条上游迟迟不吐内容的流会被判成「窗口已过」而丧失
// 持流保护。
func TestAnthropicHoldbackObserverWindowNeedsCommitPoint(t *testing.T) {
	o := &anthropicHoldbackObserver{}
	now := time.Date(2026, 8, 24, 1, 0, 0, 0, time.UTC)
	o.observe(gjson.Parse(`{"type":"ping"}`), false, now)
	require.False(t, o.windowElapsed(now.Add(time.Hour), time.Second))
}

// 端到端：疑似截断在客户端看到任何字节之前被丢掉并换号。这是整个改动的目的。
func TestAnthropicPassthrough_HoldbackDiscardsShortTurnWithoutExposure(t *testing.T) {
	const sessionKey = "holdback-discard"
	const groupID = int64(1)
	cache := newShortTurnStreakCache(sessionKey, 9)
	svc, repo := newHoldbackTestGatewayService(t, cache, 3000)

	rec, c, err := runHoldbackPassthrough(t, svc, groupID, sessionKey, 9,
		shortTurnSSE("临时文件都已清理删除，现在只剩部署目录。接下来我看日志", 70, false), nil)

	var failoverErr *UpstreamFailoverError
	require.True(t, errors.As(err, &failoverErr), "必须是可 failover 的上游错误，否则换不了号")
	require.Equal(t, GatewayFailureReason("anthropic_short_turn_holdback"), failoverErr.Reason)
	require.Equal(t, http.StatusBadGateway, failoverErr.StatusCode)
	require.True(t, failoverErr.ShouldRetryNextAccount(), "必须继续换号")
	require.False(t, failoverErr.RetryableOnSameAccount, "同一个坏中转上重试只会再截断一次")

	// 不得罚号：判据是启发式的。Scope=Request + RequestScopedTransient 是这个仓库里
	// 「故障与账号健康无关」的既有标记，TempUnscheduleRetryableError 会直接 return。
	require.Equal(t, GatewayFailureScopeRequest, failoverErr.Scope)
	require.True(t, failoverErr.RequestScopedTransient)
	require.Zero(t, repo.tempCalls, "持流丢弃不得罚号")

	// 零暴露：这是整条用例的重点，少了这两行就退化成事后归因。
	require.Empty(t, rec.Body.String(), "截断内容一个字节都不能写给客户端")
	require.False(t, c.Writer.Written())

	// 本次请求的重试额度已经用掉，第二次不能再丢。
	require.True(t, anthropicHoldbackRetryUsed(c))

	// 丢弃的同时必须解绑。这一条是「不会自动切号」的根因：非破坏性绑定
	// （gateway_service.go 里 existingAccountID != accountID 时直接 return nil）加上
	// failover 期间的 stickyFailoverBindingCtxKey 保护，意味着重试那一发落到别的账号上
	// **也不会**改写绑定。不解绑的话下一发客户端请求照旧从这个坏号起手，每发白烧一次
	// 上游调用，靠持流一遍遍丢掉——客户端看不到脏数据，但账号永远换不掉。
	// 代价只是丢一次 prompt cache，见 noteAnthropicShortTurnStreak。
	require.Equal(t, 1, cache.deletedSessions[sessionKey], "丢弃必须同时解绑，否则下一发还是这个号")
	require.NotContains(t, cache.sessionBindings, sessionKey)
	require.Empty(t, cache.streaks, "解绑后必须清零连击")
}

// 账号 12（sotamodel）的实测形态：协议完整、stop_reason=end_turn、开了正文块，却一个
// text_delta 都没有，output_tokens=1。这是三个检测器之间那条缝，也是这次改动的锚点。
//
// 原先的走法：anthropicStreamLooksIncompleteDespiteTerminal 要 output_tokens<=0（拿到 1）
// → 放行；anthropicTurnLooksSuspiciouslyShort 要 proseRunes>0（拿到 0）→ 放行；
// 于是持流判 Release，一个没有答案的 200 交付给客户端，且账号既没被排除也没被冷却。
// 客户端 4 秒后自己重发，那是一个全新请求（excluded_count=0），粘性把它原样送回账号 12。
func TestAnthropicPassthrough_HoldbackDiscardsEmptyAnswerWithoutExposure(t *testing.T) {
	const sessionKey = "holdback-empty-answer"
	const groupID = int64(1)
	cache := newShortTurnStreakCache(sessionKey, 12)
	svc, repo := newHoldbackTestGatewayService(t, cache, 3000)

	rec, c, err := runHoldbackPassthrough(t, svc, groupID, sessionKey, 12, emptyAnswerSSE(1), nil)

	var failoverErr *UpstreamFailoverError
	require.True(t, errors.As(err, &failoverErr), "空回合必须可 failover，否则换不了号")
	require.Equal(t, GatewayFailureReason("anthropic_short_turn_holdback"), failoverErr.Reason)
	require.True(t, failoverErr.ShouldRetryNextAccount())

	// 零暴露：空响应一个字节都不能到客户端。
	require.Empty(t, rec.Body.String(), "空回合一个字节都不能写给客户端")
	require.False(t, c.Writer.Written())
	require.True(t, anthropicHoldbackRetryUsed(c))

	// 解绑：让重试那一发之后的请求也别再回到这个号。
	require.Equal(t, 1, cache.deletedSessions[sessionKey])
	require.NotContains(t, cache.sessionBindings, sessionKey)

	// 罚号：与「说得少」分开。end_turn 声明说完了却一个字没说，不存在合法解释，
	// 所以这一档要把账号从调度池里暂时摘掉——否则解绑后重新选号仍可能落回它。
	require.Positive(t, repo.tempCalls, "空回合是确定性上游故障，必须冷却账号")
}

// 对照组：同样走丢弃路径，但正文非空（只是短）。这一发不得罚号——用户的问题本来就可能
// 只有一句话的答案，罚号会把一个好账号从所有会话的调度池里摘掉。
//
// 这一对（上面罚、这里不罚）是罚号边界的全部内容，必须成对存在。
func TestAnthropicPassthrough_HoldbackDiscardShortButNonEmptyDoesNotPenalize(t *testing.T) {
	const sessionKey = "holdback-discard-nonempty"
	const groupID = int64(1)
	cache := newShortTurnStreakCache(sessionKey, 12)
	svc, repo := newHoldbackTestGatewayService(t, cache, 3000)

	rec, _, err := runHoldbackPassthrough(t, svc, groupID, sessionKey, 12,
		shortTurnSSE("好", 30, false), nil)

	var failoverErr *UpstreamFailoverError
	require.True(t, errors.As(err, &failoverErr))
	require.Empty(t, rec.Body.String())
	require.Equal(t, 1, cache.deletedSessions[sessionKey], "短回合同样解绑")
	require.Zero(t, repo.tempCalls, "有正文就可能是合法短回答，只解绑不罚号")
}

// 同一发响应，在「本次请求已经丢过一次」之后必须原样交付，并回落到解绑。
//
// 没有这条兜底，问题本来就只有一句话答案时会一路重试到调度耗尽，最后把一个完好的短回答
// 变成 502——那比断流更糟。
func TestAnthropicPassthrough_HoldbackRetryUsedDeliversAndUnbinds(t *testing.T) {
	const sessionKey = "holdback-retry-used"
	const groupID = int64(1)
	cache := newShortTurnStreakCache(sessionKey, 9)
	svc, repo := newHoldbackTestGatewayService(t, cache, 3000)

	rec, _, err := runHoldbackPassthrough(t, svc, groupID, sessionKey, 9,
		shortTurnSSE("临时文件都已清理删除，现在只剩部署目录。接下来我看日志", 70, false),
		markAnthropicHoldbackRetryUsed)

	require.NoError(t, err)
	require.Contains(t, rec.Body.String(), "接下来我看日志", "第二次必须原样交付")
	require.Contains(t, rec.Body.String(), "message_stop")

	// 交付了就回落到解绑，让下一发换号。
	requireUnboundOnce(t, cache, groupID, sessionKey)
	require.Zero(t, repo.tempCalls)
}

// 成段回答必须照常交付，且不能因为持流丢帧或乱序。
func TestAnthropicPassthrough_HoldbackReleasesHealthyTurn(t *testing.T) {
	const sessionKey = "holdback-healthy"
	const groupID = int64(1)
	cache := newShortTurnStreakCache(sessionKey, 9)
	svc, _ := newHoldbackTestGatewayService(t, cache, 3000)

	long := strings.Repeat("正常长度的回答内容。", 45) // 450 rune > 400 上限，靠提前放行出口走
	rec, _, err := runHoldbackPassthrough(t, svc, groupID, sessionKey, 9, shortTurnSSE(long, 800, false), nil)

	require.NoError(t, err)
	body := rec.Body.String()
	require.Contains(t, body, long)
	require.Less(t, strings.Index(body, "message_start"), strings.Index(body, "content_block_start"),
		"攒下的前奏必须按原序重放")
	require.Less(t, strings.Index(body, "content_block_start"), strings.Index(body, "message_stop"))
	require.Zero(t, cache.deletedSessions[sessionKey])
}

// tool_use 回合走的是另一条提前放行出口：agent 每次工具调用都是这个形状，一旦被攥住
// 就会给每一次工具调用都加上一个窗口的延迟。
func TestAnthropicPassthrough_HoldbackReleasesToolUseTurn(t *testing.T) {
	const sessionKey = "holdback-tool-use"
	const groupID = int64(1)
	cache := newShortTurnStreakCache(sessionKey, 9)
	svc, _ := newHoldbackTestGatewayService(t, cache, 3000)

	rec, c, err := runHoldbackPassthrough(t, svc, groupID, sessionKey, 9,
		turnSSEWithStopReason("我来查一下", 30, "tool_use", true), nil)

	require.NoError(t, err)
	require.Contains(t, rec.Body.String(), "tool_use")
	require.False(t, anthropicHoldbackRetryUsed(c), "工具回合不得消耗重试额度")
}

// 窗口配 0 就必须完全退化成旧行为：短回合照常交付，只走解绑。
// 这是不重新构建就能关掉整套持流的开关，必须有用例守着。
func TestAnthropicPassthrough_HoldbackDisabledPreservesLegacyDelivery(t *testing.T) {
	const sessionKey = "holdback-off"
	const groupID = int64(1)
	cache := newShortTurnStreakCache(sessionKey, 9)
	svc, _ := newHoldbackTestGatewayService(t, cache, 0)

	rec, c, err := runHoldbackPassthrough(t, svc, groupID, sessionKey, 9,
		shortTurnSSE("临时文件都已清理删除，现在只剩部署目录。接下来我看日志", 70, false), nil)

	require.NoError(t, err, "关掉持流后不得再产生 failover")
	require.Contains(t, rec.Body.String(), "接下来我看日志")
	require.False(t, anthropicHoldbackRetryUsed(c))
	requireUnboundOnce(t, cache, groupID, sessionKey)
}

// stallThenEOF 在返回完 head 之后先卡住一段时间再给 EOF，用来逼出持流定时器那条分支。
type stallThenEOF struct {
	head   io.Reader
	stall  time.Duration
	waited bool
}

func (s *stallThenEOF) Read(p []byte) (int, error) {
	if s.head != nil {
		n, err := s.head.Read(p)
		if err == io.EOF {
			s.head = nil
			if n > 0 {
				return n, nil
			}
		} else {
			return n, err
		}
	}
	if !s.waited {
		s.waited = true
		time.Sleep(s.stall)
	}
	return 0, io.EOF
}

func (s *stallThenEOF) Close() error { return nil }

// 上游吐了几句就长时间静默、判据始终不来：窗口到点必须放行，不能让客户端无限等一个启发式。
//
// 窗口 1ms 对 100ms 的静默是 100 倍余量，所以先到的一定是定时器。
func TestAnthropicPassthrough_HoldbackWindowTimeoutReleasesUndecidedTurn(t *testing.T) {
	const sessionKey = "holdback-window"
	const groupID = int64(1)
	cache := newShortTurnStreakCache(sessionKey, 9)
	svc, _ := newHoldbackTestGatewayService(t, cache, 1)

	head := strings.Join([]string{
		`data: {"type":"message_start","message":{"usage":{"input_tokens":129000}}}`,
		"",
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		"",
		fmt.Sprintf(`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":%q}}`, "先看一眼日志。"),
		"",
		"",
	}, "\n")
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       &stallThenEOF{head: strings.NewReader(head), stall: 100 * time.Millisecond},
	}

	rec, _, err := runHoldbackPassthrough(t, svc, groupID, sessionKey, 9, resp, nil)

	// 没有终止事件，所以这一发照旧算残缺；关键是它必须已经被写给客户端，而不是卡在缓冲里。
	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.False(t, errors.As(err, &failoverErr), "窗口到点是放行，不是丢弃重试")
	require.Contains(t, rec.Body.String(), "先看一眼日志。", "窗口到点必须把攒下的帧放出去")
}
