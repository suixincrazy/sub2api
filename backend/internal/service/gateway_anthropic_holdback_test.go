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
//   - 丢弃额度必须能一票否决 Discard。用户问的问题本来就只有一句话答案时，每个账号都会
//     给同样的短回合，不设上限会一路重试到调度耗尽，把一个完好的短回答变成 502。
//   - 额度按形态分档。空回合不存在「本来就该是空的答案」这种合法解释，所以它的上限比短回合
//     宽；两种启发式形态共用一个计数器，各自比自己的上限，块序判据另有独立额度。
//   - 实际持流预算只在 stop_reason 未到时触发 Release。终止证据已齐时仍按短回合判据定案，
//     否则慢 TTFB 会让预算在首个可提交帧到达前就耗尽，短回合仍然漏给客户端。
func TestAnthropicHoldbackVerdict(t *testing.T) {
	cases := []struct {
		name               string
		windowGone         bool
		deadAir            bool
		stopReason         string
		proseRunes         int
		outputTokens       int
		toolUse            bool
		heuristicDiscards  int
		blockOrderDiscards int
		thinking           int
		blockOrder         bool
		want               anthropicHoldbackDecision
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
		// 4 条 disposition=delivered（2026-08-24 15:04:12 / 15:12:13 / 18:09:58 / 18:14:40）
		// 的根因就在这里：启发式两档共用一次性额度时，第一个坏号把额度吃光，第二个坏号的空
		// 响应必然放行。下一条才是真正区分两档的那一行：启发式额度已到短回合上限，空回合仍然丢。
		{
			name:       "零正文且已丢弃过一次：仍然丢弃，这是 4 条 delivered 的根因",
			stopReason: "end_turn", proseRunes: 0, outputTokens: 1, heuristicDiscards: 1,
			want: anthropicHoldbackDiscard,
		},
		{
			name:       "零正文额度用满前的最后一次仍然丢弃",
			stopReason: "end_turn", proseRunes: 0, outputTokens: 1,
			heuristicDiscards: anthropicEmptyAnswerDiscardBudget - 1,
			want:              anthropicHoldbackDiscard,
		},
		// 仍然留上限：额度耗尽后退化成放行（等于改动前的行为），不会把池子重试到 502。
		{
			name:       "零正文丢满额度后放行，不能一路重试到调度耗尽",
			stopReason: "end_turn", proseRunes: 0, outputTokens: 1,
			heuristicDiscards: anthropicEmptyAnswerDiscardBudget,
			want:              anthropicHoldbackRelease,
		},
		{
			name:       "stop_reason 优先于窗口：窗口已过但判据齐了仍然丢弃",
			windowGone: true, stopReason: "end_turn", proseRunes: 131, outputTokens: 70,
			want: anthropicHoldbackDiscard,
		},
		// 19:22:03（prose 287 / out 69）与 19:23:58（prose 217 / out 54）这两发之所以交付，
		// 就是因为额度只有一次、已被前一个坏号吃光。同时坏掉两个号是常态，所以第二次仍要丢。
		{
			name:       "短回合已丢弃过一次：仍然丢弃，这是 19:2x 两发交付的根因",
			stopReason: "end_turn", proseRunes: 131, outputTokens: 70, heuristicDiscards: 1,
			want: anthropicHoldbackDiscard,
		},
		{
			name:       "短回合丢满额度后放行，不能把真短答案变成 502",
			stopReason: "end_turn", proseRunes: 131, outputTokens: 70,
			heuristicDiscards: anthropicShortTurnDiscardBudget,
			want:              anthropicHoldbackRelease,
		},
		// 两种启发式形态共用一个计数器，所以顺序无关：为空回合丢满之后，短回合那一档立刻判定
		// 启发式额度已尽，短回合上限的保护不会因为空回合放宽而失效。
		{
			name:       "空回合丢满之后短回合不再丢",
			stopReason: "end_turn", proseRunes: 131, outputTokens: 70,
			heuristicDiscards: anthropicEmptyAnswerDiscardBudget,
			want:              anthropicHoldbackRelease,
		},
		// 用常量表达而不是写死数字：这一条曾因为闸门从 128 抬到 320 而变成陈旧断言
		// （outputTokens: 200 在 320 之下，早就不再"上量"了），红了一次没人发现。
		{
			name:       "token 上量的短正文是正常回答，放行",
			stopReason: "end_turn", proseRunes: 131,
			outputTokens: anthropicShortTurnOutputTokenLimit + 1,
			want:         anthropicHoldbackRelease,
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
		// 账号 10 的形态（usage_logs id=9514 / id=9523）：想了很久，正文只有 "d." 两个字符。
		// 思考 token 把 output_tokens 撑到 577，128 那道闸门恒为假，这一形态在加思考判据之前
		// 全程免检——窗口调到 15s 也拦不住，因为压根没被判为可疑。
		{
			name:       "想很久却只吐几个字：丢弃重试",
			stopReason: "end_turn", proseRunes: 2, outputTokens: 577, thinking: 900,
			want: anthropicHoldbackDiscard,
		},
		// 下面两条守住这条新判据的两侧边界，防止它把正常回合也吃掉。
		{
			name:       "思考不够长时仍由 token 闸门说话，放行",
			stopReason: "end_turn", proseRunes: 2, outputTokens: 577,
			thinking: anthropicShortTurnThinkingRuneFloor - 1,
			want:     anthropicHoldbackRelease,
		},
		// 越过 anthropicPostThinkingProseRuneCeiling 只是**关掉思考旁路**，不等于放行：
		// 之后仍要过折算后的 token 闸门。思考 900 + 正文 600 时折算系数是 600/1500=0.4，
		// out 必须大于 430/0.4≈1075 才算「说出来的部分够多」。
		//
		// 旧用例在这里写的是 prose=41 / out=577，折算只有 25，判定必然是可疑，却期望放行，
		// 是一条从折算判据上线那天起就红着的陈旧断言。
		{
			name:       "思考很长而正文与产出成比例：是真答案，放行",
			stopReason: "end_turn", proseRunes: 600, outputTokens: 1200, thinking: 900,
			want: anthropicHoldbackRelease,
		},
		{
			name:       "思考很长而正文只是刚过旁路上限：折算后仍然可疑",
			stopReason: "end_turn", proseRunes: anthropicPostThinkingProseRuneCeiling + 1,
			outputTokens: 577, thinking: 900,
			want: anthropicHoldbackDiscard,
		},
		// 账号 10 的另一形态（usage_logs id=9544）：上游把 usage 报成 0，而字节**确实**出去了。
		// 旧代码在 output_tokens<=0 处无条件让位给残缺判定，可那边要求 visibleChars==0，于是
		// 两套判定都不认它，零信号零记账。现在这一档由正文长度说话。
		{
			name:       "usage 报 0 但有正文：仍按短回合丢弃",
			stopReason: "end_turn", proseRunes: 2, outputTokens: 0,
			want: anthropicHoldbackDiscard,
		},
		// 而 usage 报 0 **且**零正文是另一回事：那是「开了块却零输出」的确定形态，让给
		// anthropicStreamLooksIncompleteDespiteTerminal 去罚号，避免同一次故障记两次。
		{
			name:       "usage 报 0 且零正文：让给残缺判定，这里放行",
			stopReason: "end_turn", proseRunes: 0, outputTokens: 0,
			want: anthropicHoldbackRelease,
		},
		// 持流预算只限制等待判据，不能覆盖已经到齐的短回合证据。
		{
			name:       "持流预算耗尽但短回合证据已齐：仍然丢弃",
			deadAir:    true,
			stopReason: "end_turn", proseRunes: 131, outputTokens: 70,
			want: anthropicHoldbackDiscard,
		},
		{
			name:       "持流预算耗尽但空回合证据已齐：仍然丢弃",
			deadAir:    true,
			stopReason: "end_turn", proseRunes: 0, outputTokens: 1,
			want: anthropicHoldbackDiscard,
		},
		{
			name:       "持流预算耗尽而窗口没到：判据未齐时放行",
			deadAir:    true,
			windowGone: false,
			proseRunes: 131, outputTokens: 70,
			want: anthropicHoldbackRelease,
		},
		// 反面：预算没吃满时不得提前放行还没定案的回合。
		{
			name:    "持流预算没吃满且判据没齐：照旧攥着",
			deadAir: false, proseRunes: 12, outputTokens: 4,
			want: anthropicHoldbackKeep,
		},
		// 块序违规这一档：2026-08-25 21:25:40 那一发（账号 9，text / thinking / text，
		// stop_reason=end_turn、正文 872 rune、out=591）。它在协议层完全合法，既有判据全部
		// 放行，所以这一条必须排在**所有**提前放行出口之前，否则等于没加。
		{
			name:       "块序违规：正文超上限也要丢弃",
			blockOrder: true,
			stopReason: "end_turn", proseRunes: anthropicShortTurnProseRuneLimit + 1, outputTokens: 591,
			want: anthropicHoldbackDiscard,
		},
		{
			name:       "块序违规：带 tool_use 块也要丢弃",
			blockOrder: true, toolUse: true,
			stopReason: "tool_use", proseRunes: 900, outputTokens: 400,
			want: anthropicHoldbackDiscard,
		},
		// 判据还没齐（stop_reason 未到）时就能定案：违规在 content_block_start 那一刻就成立，
		// 不必等 message_delta，越早换号越省客户端的等待。
		{
			name:       "块序违规：stop_reason 还没到也立刻丢弃",
			blockOrder: true, proseRunes: 798,
			want: anthropicHoldbackDiscard,
		},
		// 块序违规同样优先于持流预算。
		{
			name:       "块序违规：死气吃满也照旧丢弃",
			blockOrder: true, deadAir: true, windowGone: true,
			stopReason: "end_turn", proseRunes: 872, outputTokens: 591,
			want: anthropicHoldbackDiscard,
		},
		// 额度封顶后退化成放行 + 事后归因，不会把池子掏空成 502。
		{
			name:       "块序违规丢满额度后退回既有判定：放行",
			blockOrder: true, blockOrderDiscards: anthropicBlockOrderDiscardBudget,
			stopReason: "end_turn", proseRunes: 872, outputTokens: 591,
			want: anthropicHoldbackRelease,
		},
		// 额度耗尽时不得把「本来该攥着」的回合提前放行：退回既有判定即原样，不是无条件 Release。
		{
			name:       "块序违规额度耗尽且判据没齐：照旧攥着",
			blockOrder: true, blockOrderDiscards: anthropicBlockOrderDiscardBudget,
			proseRunes: 12, outputTokens: 4,
			want: anthropicHoldbackKeep,
		},
		{
			name:       "启发式额度耗尽不影响块序违规",
			blockOrder: true, heuristicDiscards: anthropicEmptyAnswerDiscardBudget,
			stopReason: "end_turn", proseRunes: 872, outputTokens: 591,
			want: anthropicHoldbackDiscard,
		},
		{
			name:               "块序额度耗尽不影响短回合",
			blockOrderDiscards: anthropicBlockOrderDiscardBudget,
			stopReason:         "end_turn", proseRunes: 131, outputTokens: 70,
			want: anthropicHoldbackDiscard,
		},
		// 反面：没有违规时这一档不得改变任何既有判定。
		{
			name:       "无违规的长正文回合照旧放行",
			blockOrder: false,
			stopReason: "end_turn", proseRunes: anthropicShortTurnProseRuneLimit + 1, outputTokens: 591,
			want: anthropicHoldbackRelease,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := anthropicHoldbackVerdict(
				tc.windowGone, tc.deadAir, tc.stopReason, tc.proseRunes, tc.outputTokens,
				tc.toolUse, tc.heuristicDiscards, tc.blockOrderDiscards, tc.thinking, tc.blockOrder)
			require.Equal(t, tc.want, got)
		})
	}
}

// 观测器把「思考不算正文」和「窗口量的是上游静默」这两件事分开管。
//
// 长思考 + 一句正文恰恰是要抓的形态，所以 thinking_delta 不得计入 proseRunes；但它**确实**
// 是旧行为下的提交点，所以窗口从它之后才有资格起算——那之前的持流不是本机制新增的延迟。
// 起算之后，计时起点是最后一帧有内容的 data，不是提交点：见 windowElapsed 的注释。
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
	require.Equal(t, thinking, o.firstCommitPointAt, "thinking_delta 是旧行为下的提交点，窗口从它之后才有资格起算")

	o.observe(gjson.Parse(`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"我看一下。"}}`), true, thinking.Add(time.Millisecond))
	require.Equal(t, 5, o.proseRunes, "正文按 rune 数")
	require.Equal(t, thinking, o.firstCommitPointAt, "提交点只记第一次")

	o.observe(gjson.Parse(`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":70}}`), false, thinking.Add(2*time.Millisecond))
	require.Equal(t, "end_turn", o.stopReason)
	require.Equal(t, 70, o.outputTokens, "message_delta 的终值必须盖掉 message_start 的初始值")

	// 窗口从最后一帧有内容的 data 起算，不是从提交点起算。这里最后一帧是上面那条
	// message_delta（thinking+2ms），比提交点晚 2ms。
	last := thinking.Add(2 * time.Millisecond)
	require.Equal(t, last, o.silenceSince(), "静默起点是最后一帧内容")
	require.False(t, o.windowElapsed(last.Add(3*time.Second-time.Millisecond), 3*time.Second))
	require.True(t, o.windowElapsed(last.Add(3*time.Second), 3*time.Second))
	require.Equal(t, last.Add(3*time.Second), o.holdbackSilenceDeadline(3*time.Second))
	require.False(t, o.windowElapsed(last.Add(time.Hour), 0), "窗口配 0 时永不耗尽")
	require.True(t, o.holdbackSilenceDeadline(0).IsZero(), "窗口配 0 时没有截止时刻")

	// 两套口径的分界就在这 2ms 上：旧实现从提交点起算总时长，会在 thinking+3s 判耗尽。
	// 这条用例里差值只有 2ms，真实长思考回合里差的是整段思考时长，见
	// TestAnthropicHoldbackObserverWindowMeasuresSilenceNotTotalHold。
	require.False(t, o.windowElapsed(thinking.Add(3*time.Second), 3*time.Second),
		"旧口径在这一刻判窗口耗尽，静默口径必须还没到")
}

// 块序违规判据：thinking / redacted_thinking 块出现在 text 块之后。
//
// 判据的全部价值在于它是确定性的，所以两侧边界都要钉死：合法序（thinking 先于 text，含同一
// 回合里多个 thinking / 多个 text）永不置位；违规序一旦出现就不再回落。
func TestAnthropicContentBlockOrderTracker(t *testing.T) {
	t.Run("合法序：thinking 先于 text", func(t *testing.T) {
		var tr anthropicContentBlockOrderTracker
		tr.note("thinking")
		tr.note("text")
		require.False(t, tr.violation())
	})
	t.Run("合法序：多个 thinking 连着多个 text", func(t *testing.T) {
		var tr anthropicContentBlockOrderTracker
		for _, b := range []string{"thinking", "redacted_thinking", "text", "text"} {
			tr.note(b)
		}
		require.False(t, tr.violation())
	})
	t.Run("合法序：纯 text 与纯工具回合", func(t *testing.T) {
		var tr anthropicContentBlockOrderTracker
		for _, b := range []string{"text", "tool_use", "text", "server_tool_use", "text"} {
			tr.note(b)
		}
		require.False(t, tr.violation(), "工具块与 text 交替是常态，不是违规")
	})
	// 21:25:40 那一发的块序。
	t.Run("违规序：text / thinking / text", func(t *testing.T) {
		var tr anthropicContentBlockOrderTracker
		tr.note("text")
		require.False(t, tr.violation(), "只有 text 时还不构成违规")
		tr.note("thinking")
		require.True(t, tr.violation())
		tr.note("text")
		require.True(t, tr.violation(), "置位后不再回落")
	})
	t.Run("违规序：redacted_thinking 同样算", func(t *testing.T) {
		var tr anthropicContentBlockOrderTracker
		tr.note("text")
		tr.note("redacted_thinking")
		require.True(t, tr.violation())
	})
	t.Run("块类型大小写与空白不敏感", func(t *testing.T) {
		var tr anthropicContentBlockOrderTracker
		tr.note(" TEXT ")
		tr.note("Thinking")
		require.True(t, tr.violation())
	})
	t.Run("未知块类型与空串不表态", func(t *testing.T) {
		var tr anthropicContentBlockOrderTracker
		for _, b := range []string{"", "  ", "image", "unknown_future_block"} {
			tr.note(b)
		}
		require.False(t, tr.violation())
		require.False(t, tr.sawTextBlock, "未知块不得被当成 text，否则后续合法 thinking 会误判")
	})
	t.Run("nil 接收者不 panic", func(t *testing.T) {
		var tr *anthropicContentBlockOrderTracker
		require.False(t, tr.violation())
	})
}

// 观测器要从真实 SSE 帧里认出块序违规，而不是只在裸类型串上工作。
func TestAnthropicHoldbackObserverDetectsBlockOrderViolation(t *testing.T) {
	base := time.Date(2026, 8, 25, 21, 25, 40, 0, time.UTC)

	legal := &anthropicHoldbackObserver{}
	legal.observe(gjson.Parse(`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`), false, base)
	legal.observe(gjson.Parse(`{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`), true, base)
	require.False(t, legal.blockOrder.violation(), "thinking 先于 text 是合法序")

	// 21:25:40 那一发：text / thinking / text，正文 872 rune、stop_reason=end_turn。
	bad := &anthropicHoldbackObserver{}
	bad.observe(gjson.Parse(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`), true, base)
	bad.observe(gjson.Parse(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Tool results:"}}`), true, base)
	require.False(t, bad.blockOrder.violation())
	bad.observe(gjson.Parse(`{"type":"content_block_start","index":1,"content_block":{"type":"thinking","thinking":""}}`), false, base)
	require.True(t, bad.blockOrder.violation(), "text 之后的 thinking 块是协议违规")

	// 工具块仍要被正常识别，块序跟踪不能把 sawToolUseBlock 挤掉。
	tool := &anthropicHoldbackObserver{}
	tool.observe(gjson.Parse(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","name":"Bash"}}`), true, base)
	require.True(t, tool.sawToolUseBlock)
	require.False(t, tool.blockOrder.violation())
}

// 2026-08-25 02:06:53 那一发的回归用例：帧还在持续到达时，窗口不得耗尽。
//
// 旧实现从 firstCommitPointAt 起算总时长，而 anthropicSSEPayloadCommitsResponse 把
// thinking_delta 算作提交帧，于是长思考回合的窗口在思考期就走完、缓冲被无条件提交；等
// stop_reason 到达、判定拿齐量的时候，字节已经出去了，再判「可疑」也只能给下一发解绑。
func TestAnthropicHoldbackObserverWindowMeasuresSilenceNotTotalHold(t *testing.T) {
	const window = 3 * time.Second
	base := time.Date(2026, 8, 25, 2, 6, 0, 0, time.UTC)
	o := &anthropicHoldbackObserver{}

	// 每 500ms 一帧思考，连续 30 秒——十倍于窗口。
	const frames, gap = 60, 500 * time.Millisecond
	now := base
	for i := 0; i < frames; i++ {
		o.observe(gjson.Parse(`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"再想一层"}}`), true, now)
		require.False(t, o.windowElapsed(now, window),
			"第 %d 帧刚到就判窗口耗尽（已持流 %s）", i, now.Sub(base))
		now = now.Add(gap)
	}
	require.Zero(t, o.proseRunes, "整段都是思考，没有正文")

	// 上游真的静默满一个窗口，才认定等不起。
	last := now.Add(-gap)
	require.Greater(t, last.Sub(base), 9*window, "构造前提：持流总时长要远超一个窗口")
	require.False(t, o.windowElapsed(last.Add(window-time.Millisecond), window))
	require.True(t, o.windowElapsed(last.Add(window), window))
}

// ping 只证明连接活着，不证明上游还在产出，所以它不刷新静默起点——「上游吐了几句就长时间
// 静默」正是要抓的形态，而中转在这种形态下往往还在按秒发 ping。
func TestAnthropicHoldbackObserverPingDoesNotRefreshSilence(t *testing.T) {
	const window = 3 * time.Second
	base := time.Date(2026, 8, 25, 2, 6, 0, 0, time.UTC)
	o := &anthropicHoldbackObserver{}

	o.observe(gjson.Parse(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"我看一下。"}}`), true, base)
	for i := 1; i <= 10; i++ {
		o.observe(gjson.Parse(`{"type":"ping"}`), false, base.Add(time.Duration(i)*time.Second))
	}
	require.Equal(t, base, o.silenceSince(), "ping 不算内容，静默起点仍是那帧正文")
	require.True(t, o.windowElapsed(base.Add(window), window), "只发 ping 等同静默，窗口照常耗尽")
}

// message_start / ping 没有正文也必须播种累计持流预算：上游首帧已经到达后再长时间卡住，
// 定时器不能因为没有 firstCommitPointAt 而永久睡死。ping 只证明连接活着，不会把预算起点推后。
func TestAnthropicHoldbackObserverDeadAirCountsPreludeAndPingSilence(t *testing.T) {
	const budget = 25 * time.Second
	base := time.Date(2026, 8, 25, 14, 10, 30, 0, time.UTC)
	o := &anthropicHoldbackObserver{}

	o.observe(gjson.Parse(`{"type":"message_start","message":{"usage":{"input_tokens":129000}}}`), false, base)
	o.observe(gjson.Parse(`{"type":"ping"}`), false, base.Add(10*time.Second))

	require.True(t, o.firstCommitPointAt.IsZero(), "前奏和 ping 不应伪造提交点")
	require.Equal(t, base.Add(budget), o.holdbackDeadAirDeadline(budget),
		"预算从第一帧 message_start 起算，ping 不得刷新起点")
	require.False(t, o.deadAirElapsed(base.Add(budget-time.Millisecond), budget))
	require.True(t, o.deadAirElapsed(base.Add(budget), budget),
		"只有前奏后静默满预算才放行")
}

// 还没到提交点就不该起算窗口，否则一条上游迟迟不吐内容的流会被判成「窗口已过」而丧失
// 持流保护。
func TestAnthropicHoldbackObserverWindowNeedsCommitPoint(t *testing.T) {
	o := &anthropicHoldbackObserver{}
	now := time.Date(2026, 8, 24, 1, 0, 0, 0, time.UTC)
	o.observe(gjson.Parse(`{"type":"ping"}`), false, now)
	require.False(t, o.windowElapsed(now.Add(time.Hour), time.Second))
}

// 2026-08-25 08:28:10 那一发的回归用例：上游一直在吐，持流总时长也必须封顶。
//
// 静默口径修掉了 02:06:53 那一类，代价是持流不再有任何上界——只要上游持续出帧，静默起点
// 就不断刷新。那一发 out=3677 被攥了 83.4 秒（first_token_ms 83394 ≈ duration_ms 83423），
// 客户端全程零字节。总时长上限从 firstCommitPointAt 起算、任何新帧都不能续期，专治这一类。
func TestAnthropicHoldbackObserverMaxHoldCapsTotalHold(t *testing.T) {
	const window = 15 * time.Second
	const maxHold = 10 * time.Second
	base := time.Date(2026, 8, 25, 8, 28, 10, 0, time.UTC)
	o := &anthropicHoldbackObserver{}

	// 每 200ms 一帧思考，连续 30 秒：静默那条永远不会到点。
	const gap = 200 * time.Millisecond
	now := base
	for now.Sub(base) < 30*time.Second {
		o.observe(gjson.Parse(`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"再想一层"}}`), true, now)
		now = now.Add(gap)
	}
	require.False(t, o.windowElapsed(now, window),
		"帧一直在到，静默窗口按定义永不耗尽——这正是需要第二条上限的理由")

	require.Equal(t, base, o.firstCommitPointAt, "上限从第一个提交点起算")
	require.False(t, o.maxHoldElapsed(base.Add(maxHold-time.Millisecond), maxHold))
	require.True(t, o.maxHoldElapsed(base.Add(maxHold), maxHold))
	require.True(t, o.releaseDeadlineElapsed(base.Add(maxHold), window, maxHold, 0),
		"两条截止线任意一条到点就该放行")
	require.False(t, o.releaseDeadlineElapsed(base.Add(maxHold-time.Millisecond), window, maxHold, 0))

	// 上限配 0 表示不封顶，退化成只有静默一条。now 停在最后一帧之后 200ms，此刻静默那条
	// 也没到点：两条都不放行，这正是改动前的行为——帧一直到，就一直攥着。
	require.False(t, o.maxHoldElapsed(base.Add(time.Hour), 0), "上限配 0 时永不封顶")
	require.True(t, o.holdbackMaxHoldDeadline(0).IsZero())
	require.False(t, o.releaseDeadlineElapsed(now, window, 0, 0),
		"上限配 0 且帧刚到过，谁都不该放行——这就是改动前的行为")
	// 而上限配 0 时放行权仍归静默那条：等它耗尽照样放行，不会因为上限缺席就永久攥着。
	require.True(t, o.releaseDeadlineElapsed(now.Add(window), window, 0, 0))
}

// 上限没起算之前不得放行：还没到提交点就说明持流一秒都还没开始。
//
// 累计持流预算从第一帧 SSE 起算；这里传 0 把它关掉，单独考察前两条。
func TestAnthropicHoldbackObserverMaxHoldNeedsCommitPoint(t *testing.T) {
	o := &anthropicHoldbackObserver{}
	now := time.Date(2026, 8, 25, 8, 0, 0, 0, time.UTC)
	o.observe(gjson.Parse(`{"type":"ping"}`), false, now)
	require.False(t, o.maxHoldElapsed(now.Add(time.Hour), time.Second))
	require.True(t, o.holdbackMaxHoldDeadline(time.Second).IsZero())
	require.True(t, o.holdbackReleaseDeadline(time.Second, time.Second, 0).IsZero())
}

// 定时器分支只认 holdbackReleaseDeadline 一个口径，所以它必须始终等于三条截止线里先到的
// 那个。口径分歧的后果是定时器睡过上限、白攥一段——正是这一条要钉死的。
func TestAnthropicHoldbackReleaseDeadlineTakesEarlierOfThree(t *testing.T) {
	base := time.Date(2026, 8, 25, 8, 28, 10, 0, time.UTC)
	o := &anthropicHoldbackObserver{holdbackElapsedBefore: 5 * time.Second}
	o.observe(gjson.Parse(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"我看一下。"}}`), true, base)

	// 线上配置：窗口 15s、上限 10s。上游此刻静默，两条都已起算，上限先到。
	require.Equal(t, base.Add(10*time.Second),
		o.holdbackReleaseDeadline(15*time.Second, 10*time.Second, 0),
		"上限比窗口近时取上限")
	// 反过来窗口更近时取窗口。
	require.Equal(t, base.Add(3*time.Second),
		o.holdbackReleaseDeadline(3*time.Second, 10*time.Second, 0),
		"窗口比上限近时取窗口")
	// 各自配 0 时退化成另一条。
	require.Equal(t, base.Add(10*time.Second), o.holdbackReleaseDeadline(0, 10*time.Second, 0))
	require.Equal(t, base.Add(3*time.Second), o.holdbackReleaseDeadline(3*time.Second, 0, 0))
	require.True(t, o.holdbackReleaseDeadline(0, 0, 0).IsZero(), "三条都配 0 时没有截止时刻")

	// 前次尝试已经实际持流 5 秒：25s 预算折算到 base 上只剩 20s，比 15s 窗口远、比不上
	// 上限；而 12s 预算只剩 7s，比 10s 上限还近，该由它先到。
	require.Equal(t, base.Add(20*time.Second), o.holdbackDeadAirDeadline(25*time.Second),
		"累计预算必须扣掉此前尝试已经持流的时间")
	require.Equal(t, base.Add(7*time.Second),
		o.holdbackReleaseDeadline(15*time.Second, 10*time.Second, 12*time.Second),
		"死气预算折算后最近时由它当先到的")
	require.True(t, o.holdbackDeadAirDeadline(0).IsZero(), "死气配 0 时没有截止时刻")

	// 帧持续到达会把静默那条不断推后，上限和累计预算两条都不动。
	later := base.Add(20 * time.Second)
	o.observe(gjson.Parse(`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"再想一层"}}`), true, later)
	require.Equal(t, later.Add(15*time.Second), o.holdbackSilenceDeadline(15*time.Second),
		"静默那条被新帧推后了")
	require.Equal(t, base.Add(10*time.Second),
		o.holdbackReleaseDeadline(15*time.Second, 10*time.Second, 0),
		"上限那条不受新帧影响，仍然是先到的那个")
	require.Equal(t, base.Add(20*time.Second), o.holdbackDeadAirDeadline(25*time.Second),
		"累计预算同样不受新帧影响")
}

// 2026-08-25 14:26:26 / 14:33:58 两发的回归用例：实际持流时间必须跨 failover 尝试累加。
//
// 前两条截止线都锚在**本次上游尝试**上（静默锚最后一帧、上限锚 firstCommitPointAt），一次
// discard 换号之后新尝试的 observer 是全新的，两条连同 10 秒上限一起归零，客户端却还挂在
// 同一个 HTTP 请求上继续干等。累计预算把每次尝试从第一帧 SSE 到丢弃的时长写进 gin.Context，
// 新尝试再从该值继续计时；首帧之前的上游 TTFB 不计入。
func TestAnthropicHoldbackObserverDeadAirSpansFailoverAttempts(t *testing.T) {
	const budget = 25 * time.Second
	firstFrameAt := time.Date(2026, 8, 25, 14, 25, 55, 0, time.UTC)

	// 第一次尝试从 message_start 起实际持流 11.6 秒后判定可疑并 discard。
	first := &anthropicHoldbackObserver{}
	first.observe(gjson.Parse(`{"type":"message_start","message":{"usage":{"input_tokens":129000}}}`), false, firstFrameAt)
	discardAt := firstFrameAt.Add(11*time.Second + 600*time.Millisecond)
	first.observe(gjson.Parse(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"我看一下。"}}`), true, discardAt)
	require.False(t, first.deadAirElapsed(discardAt, budget), "11.6 秒还没吃满预算，此时换号是对的")
	require.Equal(t, 11*time.Second+600*time.Millisecond, first.holdbackElapsed(discardAt))

	// 丢弃前把累计值写入同一个 gin.Context；第二次尝试从该值继续。
	c, _ := newRefusalTestContext(t)
	noteAnthropicHoldbackElapsed(c, first.holdbackElapsed(discardAt))
	second := &anthropicHoldbackObserver{holdbackElapsedBefore: anthropicHoldbackElapsedBefore(c)}
	secondFrame := discardAt.Add(500 * time.Millisecond)
	second.observe(gjson.Parse(`{"type":"message_start","message":{"usage":{"input_tokens":129000}}}`), false, secondFrame)

	// 第二次尝试只送出不提交响应的 message_start 后静默，前两条线都没起算；累计预算还剩
	// 13.4 秒，并在该边界负责放行。
	release := secondFrame.Add(13*time.Second + 400*time.Millisecond)
	require.False(t, second.windowElapsed(release, 15*time.Second), "没有提交点时静默线不参与")
	require.False(t, second.maxHoldElapsed(release, 10*time.Second), "没有提交点时总时长线不参与")
	require.True(t, second.deadAirElapsed(release, budget), "实际持流时间跨尝试累加，此刻必须已经吃满")
	require.True(t, second.releaseDeadlineElapsed(release, 15*time.Second, 10*time.Second, budget),
		"三条任意一条到点就该放行——这一刻只有死气那条在说话")
	require.Equal(t, release, second.holdbackDeadAirDeadline(budget))

	// 预算耗尽的那一刻正好是边界，前一毫秒不算。
	require.False(t, second.deadAirElapsed(release.Add(-time.Millisecond), budget))
	require.True(t, second.deadAirElapsed(release, budget))

	// 配 0 关掉；没有收到任何 SSE 帧时同样不参与。
	require.False(t, second.deadAirElapsed(release.Add(time.Hour), 0), "配 0 时永不吃满")
	bare := &anthropicHoldbackObserver{}
	require.False(t, bare.deadAirElapsed(release.Add(time.Hour), budget), "没有 SSE 帧时不参与判定")
	require.True(t, bare.holdbackDeadAirDeadline(budget).IsZero())
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

	// 达到解绑阈值后必须冷却当前账号，避免删掉粘性绑定后调度仍选回同一个首选号。
	// 只有未达到阈值的观测才不罚号；这条用例的阈值为 1，已经触发解绑。
	require.Equal(t, GatewayFailureScopeRequest, failoverErr.Scope)
	require.True(t, failoverErr.RequestScopedTransient)
	require.Equal(t, 1, repo.tempCalls, "持流丢弃在解绑时必须冷却账号")

	// 零暴露：这是整条用例的重点，少了这两行就退化成事后归因。
	require.Empty(t, rec.Body.String(), "截断内容一个字节都不能写给客户端")
	require.False(t, c.Writer.Written())

	// 本次请求已经丢弃过一次。短回合额度是 1，所以第二次同形态就得放行。
	require.Equal(t, 1, anthropicHoldbackDiscardsUsed(c))

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

// 2026-08-26 19:29:13 的精确回归：上游首个可提交帧到达前，客户端已经等了约 36.5 秒，
// 随后只吐出 17 token 的过渡句就以 end_turn 收尾。死气预算若从客户端请求起点计算，会在
// content_block_start 到达时立刻判定预算耗尽并提交响应，message_delta 再到时已经无法切号。
func TestAnthropicPassthrough_HoldbackLateFirstTokenStillDiscardsShortTurn(t *testing.T) {
	const sessionKey = "holdback-late-first-token"
	const groupID = int64(1)
	cache := newShortTurnStreakCache(sessionKey, 9)
	svc, _ := newHoldbackTestGatewayService(t, cache, 3000)
	svc.cfg.Gateway.AnthropicHoldbackDeadAirBudgetMs = 25000

	c, rec := newRefusalTestContext(t)
	ctx := WithStickySessionScope(context.Background(), groupID, sessionKey, false)
	_, err := svc.handleStreamingResponseAnthropicAPIKeyPassthrough(
		ctx,
		shortTurnSSE("模拟器起来了。恢复端口转发，同步最新脚本上去复跑。", 17, false),
		c,
		&Account{ID: 9, Name: "primary", Platform: PlatformAnthropic},
		time.Now().Add(-36500*time.Millisecond),
		"claude-opus-5",
	)

	var failoverErr *UpstreamFailoverError
	require.True(t, errors.As(err, &failoverErr),
		"慢 TTFB 不能提前耗尽持流预算，否则 17-token 短回合会被当作正常响应交付")
	require.Equal(t, GatewayFailureReason("anthropic_short_turn_holdback"), failoverErr.Reason)
	require.True(t, failoverErr.ShouldRetryNextAccount())
	require.Empty(t, rec.Body.String(), "短回合一个字节都不能写给客户端")
	require.False(t, c.Writer.Written())
}

func TestAnthropicPassthrough_BlockOrderDiscardHasIndependentBudget(t *testing.T) {
	const sessionKey = "holdback-block-order-budget"
	const groupID = int64(1)
	cache := newShortTurnStreakCache(sessionKey, 9)
	svc, repo := newHoldbackTestGatewayService(t, cache, 3000)

	rec, c, err := runHoldbackPassthrough(t, svc, groupID, sessionKey, 9,
		blockOrderViolationSSE("end_turn"), func(c *gin.Context) {
			c.Set(anthropicHoldbackDiscardsKey, anthropicEmptyAnswerDiscardBudget)
		})

	var failoverErr *UpstreamFailoverError
	require.True(t, errors.As(err, &failoverErr))
	require.Equal(t, GatewayFailureReason("anthropic_block_order_violation"), failoverErr.Reason)
	require.Empty(t, rec.Body.String())
	require.Equal(t, anthropicEmptyAnswerDiscardBudget, anthropicHoldbackDiscardsUsed(c))
	require.Equal(t, 1, anthropicBlockOrderDiscardsUsed(c))
	require.Equal(t, 1, cache.deletedSessions[sessionKey])
	require.Equal(t, 1, repo.tempCalls)
}

func TestAnthropicPassthrough_ExhaustedBlockOrderBudgetFallsBackToHeuristic(t *testing.T) {
	const sessionKey = "holdback-block-order-exhausted"
	const groupID = int64(1)
	cache := newShortTurnStreakCache(sessionKey, 9)
	svc, repo := newHoldbackTestGatewayService(t, cache, 3000)

	rec, c, err := runHoldbackPassthrough(t, svc, groupID, sessionKey, 9,
		blockOrderViolationSSEWithOutput("end_turn", 70), func(c *gin.Context) {
			c.Set(anthropicBlockOrderDiscardsKey, anthropicBlockOrderDiscardBudget)
		})

	var failoverErr *UpstreamFailoverError
	require.True(t, errors.As(err, &failoverErr))
	require.Equal(t, GatewayFailureReason("anthropic_short_turn_holdback"), failoverErr.Reason)
	require.Empty(t, rec.Body.String())
	require.Equal(t, anthropicBlockOrderDiscardBudget, anthropicBlockOrderDiscardsUsed(c))
	require.Equal(t, 1, anthropicHoldbackDiscardsUsed(c))
	require.Equal(t, 1, cache.deletedSessions[sessionKey], "块序额度耗尽后的启发式回退仍必须解绑粘性会话")
	require.Equal(t, 1, repo.tempCalls, "启发式回退解绑时必须冷却账号一次")
	events := opsEvents(t, c)
	require.Len(t, events, 2)
	require.Equal(t, "short_turn_streak_unbind", events[0].Kind)
	require.Equal(t, "short_turn_holdback_failover", events[1].Kind)
}

func TestAnthropicPassthrough_BlockOrderWinsOverIncompleteAfterCommit(t *testing.T) {
	const sessionKey = "delivered-block-order"
	const groupID = int64(1)
	cache := newShortTurnStreakCache(sessionKey, 9)
	svc, repo := newShortTurnTestGatewayService(t, cache)

	c, rec := newRefusalTestContext(t)
	ctx := WithStickySessionScope(context.Background(), groupID, sessionKey, false)
	_, err := svc.handleStreamingResponseAnthropicAPIKeyPassthrough(
		ctx, blockOrderViolationSSE("upstream_error"), c,
		&Account{ID: 9, Name: "primary", Platform: PlatformAnthropic}, time.Now(), "claude-opus-5")
	require.NoError(t, err)
	require.NotEmpty(t, rec.Body.String())
	require.Equal(t, 1, cache.deletedSessions[sessionKey])
	require.Equal(t, 1, repo.tempCalls)
	events := opsEvents(t, c)
	require.Len(t, events, 1)
	require.Equal(t, "block_order_violation", events[0].Kind)
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
	require.Equal(t, 1, anthropicHoldbackDiscardsUsed(c))

	// 解绑：让重试那一发之后的请求也别再回到这个号。
	require.Equal(t, 1, cache.deletedSessions[sessionKey])
	require.NotContains(t, cache.sessionBindings, sessionKey)

	// 罚号：与「说得少」分开。end_turn 声明说完了却一个字没说，不存在合法解释，
	// 所以这一档要把账号从调度池里暂时摘掉——否则解绑后重新选号仍可能落回它。
	require.Equal(t, 1, repo.tempCalls, "空回合必须冷却账号且不得与短回合报告器重复计数")
}

// 短但非空的回合达到解绑阈值后也必须冷却账号；报告器只应执行一次。
// 空回合与这一档仍由调用点二选一，不能因为短回合非空而漏掉冷却。
func TestAnthropicPassthrough_HoldbackDiscardShortButNonEmptyPenalizesOnce(t *testing.T) {
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
	require.Equal(t, 1, repo.tempCalls, "达到解绑阈值后必须冷却账号")
}

// 短回合（有正文）丢过一次之后仍要继续丢，这是 19:2x 那两发交付的直接修复。
//
// 实证形态：账号 10 连续五发短回合都被成功丢弃，但其中两发的重试落到账号 9 上就交付了——
// 19:22:03（prose 287 / out 69）与 19:23:58（prose 217 / out 54）都只留下
// sticky_short_turn_unbound、没有配对的 holdback_failover。一次性额度的隐含假设是「第二个
// 号大概率是好的」，而同时坏掉两个号是常态。这里先记一次丢弃（模拟账号 10 那一发），再喂
// 一条短回合，必须仍然丢弃。
func TestAnthropicPassthrough_HoldbackShortTurnStillDiscardedAfterFirstDiscard(t *testing.T) {
	const sessionKey = "holdback-short-second-account"
	const groupID = int64(1)
	cache := newShortTurnStreakCache(sessionKey, 9)
	svc, repo := newHoldbackTestGatewayService(t, cache, 3000)

	rec, c, err := runHoldbackPassthrough(t, svc, groupID, sessionKey, 9,
		shortTurnSSE("临时文件都已清理删除，现在只剩部署目录。接下来我看日志", 70, false),
		noteAnthropicHoldbackDiscard)

	var failoverErr *UpstreamFailoverError
	require.True(t, errors.As(err, &failoverErr),
		"第二个坏号的短回合必须继续丢弃，这正是 19:2x 两发交付的成因")
	require.True(t, failoverErr.ShouldRetryNextAccount())
	require.Empty(t, rec.Body.String(), "一个字节都不能写给客户端")
	require.Equal(t, 2, anthropicHoldbackDiscardsUsed(c), "计数要累加，不是布尔")
	require.Equal(t, 1, repo.tempCalls, "该回合只报告一次解绑冷却")
}

// 短回合的额度不是无限：丢满 anthropicShortTurnDiscardBudget 次之后必须原样交付并回落到解绑。
//
// 没有这条兜底，问题本来就只有一句话答案时会一路重试到调度耗尽，最后把一个完好的短回答
// 变成 502——那比断流更糟。所以「短但有正文」这一档的上限刻意小于空回合那一档。
func TestAnthropicPassthrough_HoldbackShortTurnBudgetExhaustedDeliversAndUnbinds(t *testing.T) {
	const sessionKey = "holdback-short-budget-used"
	const groupID = int64(1)
	cache := newShortTurnStreakCache(sessionKey, 9)
	svc, repo := newHoldbackTestGatewayService(t, cache, 3000)

	rec, c, err := runHoldbackPassthrough(t, svc, groupID, sessionKey, 9,
		shortTurnSSE("临时文件都已清理删除，现在只剩部署目录。接下来我看日志", 70, false),
		func(c *gin.Context) {
			c.Set(anthropicHoldbackDiscardsKey, anthropicShortTurnDiscardBudget)
		})

	require.NoError(t, err, "额度耗尽后不得再产生 failover，否则会把短回答变成 502")
	require.Contains(t, rec.Body.String(), "接下来我看日志", "额度用尽必须原样交付")
	require.Contains(t, rec.Body.String(), "message_stop")
	require.Equal(t, anthropicShortTurnDiscardBudget, anthropicHoldbackDiscardsUsed(c),
		"放行不消耗额度")

	// 交付了就回落到解绑，让下一发换号。
	requireUnboundOnce(t, cache, groupID, sessionKey)
	require.Equal(t, 1, repo.tempCalls, "交付路径达到解绑阈值后也必须冷却账号")
}

// 空回合的额度比短回合宽，这是 4 条 disposition=delivered 的直接修复。
//
// 实证形态：账号 9 被丢弃后 5~6 秒，账号 12 的空回合落地成一个 200。启发式两档共用一次性
// 额度时，第一个坏号把额度吃光，第二个坏号必然放行。这里先记一次丢弃（模拟账号 9 那一发），
// 再喂一条空回合，必须仍然丢弃。
func TestAnthropicPassthrough_HoldbackEmptyAnswerStillDiscardedAfterFirstDiscard(t *testing.T) {
	const sessionKey = "holdback-empty-second-account"
	const groupID = int64(1)
	cache := newShortTurnStreakCache(sessionKey, 12)
	svc, repo := newHoldbackTestGatewayService(t, cache, 3000)

	rec, c, err := runHoldbackPassthrough(t, svc, groupID, sessionKey, 12, emptyAnswerSSE(1),
		noteAnthropicHoldbackDiscard)

	var failoverErr *UpstreamFailoverError
	require.True(t, errors.As(err, &failoverErr),
		"第二个坏号的空回合必须继续丢弃，这正是 4 条 delivered 的成因")
	require.Equal(t, GatewayFailureReason("anthropic_short_turn_holdback"), failoverErr.Reason)
	require.True(t, failoverErr.ShouldRetryNextAccount())

	require.Empty(t, rec.Body.String(), "空响应一个字节都不能写给客户端")
	require.False(t, c.Writer.Written())
	require.Equal(t, 2, anthropicHoldbackDiscardsUsed(c), "计数要累加，不是布尔")
	require.Equal(t, 1, repo.tempCalls, "空回合必须只冷却一次")
}

// 空回合的额度也不是无限：丢满 anthropicEmptyAnswerDiscardBudget 次之后必须退化成放行。
//
// 这条守的是「不新增 502」——额度耗尽后回到今天的行为（交付 + 解绑 + 冷却），而不是把
// 请求一路重试到调度池耗尽。
func TestAnthropicPassthrough_HoldbackEmptyAnswerBudgetExhaustedDelivers(t *testing.T) {
	const sessionKey = "holdback-empty-budget-used"
	const groupID = int64(1)
	cache := newShortTurnStreakCache(sessionKey, 12)
	svc, repo := newHoldbackTestGatewayService(t, cache, 3000)

	rec, c, err := runHoldbackPassthrough(t, svc, groupID, sessionKey, 12, emptyAnswerSSE(1),
		func(c *gin.Context) {
			c.Set(anthropicHoldbackDiscardsKey, anthropicEmptyAnswerDiscardBudget)
		})

	require.NoError(t, err, "额度耗尽后不得再产生 failover，否则会新增 502")
	require.Contains(t, rec.Body.String(), "message_stop")
	require.Equal(t, anthropicEmptyAnswerDiscardBudget, anthropicHoldbackDiscardsUsed(c),
		"放行不消耗额度")

	// 交付了就回落到解绑 + 冷却，与 delivered 那一档同口径。
	requireUnboundOnce(t, cache, groupID, sessionKey)
	require.Equal(t, 1, repo.tempCalls, "交付出去的空回合仍要冷却且不得重复计数")
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
	require.Zero(t, anthropicHoldbackDiscardsUsed(c), "工具回合不得消耗丢弃额度")
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
	require.Zero(t, anthropicHoldbackDiscardsUsed(c))
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

// 只有 message_start / ping、始终没有正文提交点时，静默窗口与单次总时长上限都不会起算。
// 累计持流预算必须独立唤醒流式循环并放行前奏，否则会一直等到更外层的 180 秒流超时。
func TestAnthropicPassthrough_HoldbackDeadAirReleasesPreludeOnlyStream(t *testing.T) {
	const sessionKey = "holdback-prelude-dead-air"
	const groupID = int64(1)
	cache := newShortTurnStreakCache(sessionKey, 9)
	svc, _ := newHoldbackTestGatewayService(t, cache, 3000)
	svc.cfg.Gateway.AnthropicHoldbackDeadAirBudgetMs = 1

	head := strings.Join([]string{
		`data: {"type":"message_start","message":{"usage":{"input_tokens":129000}}}`,
		"",
		`data: {"type":"ping"}`,
		"",
		"",
	}, "\n")
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       &stallThenEOF{head: strings.NewReader(head), stall: 100 * time.Millisecond},
	}

	rec, _, err := runHoldbackPassthrough(t, svc, groupID, sessionKey, 9, resp, nil)

	require.Error(t, err, "没有终止事件仍应按残缺流返回错误")
	var failoverErr *UpstreamFailoverError
	require.False(t, errors.As(err, &failoverErr), "预算到点是放行前奏，不是丢弃换号")
	require.Contains(t, rec.Body.String(), `"type":"message_start"`,
		"没有提交点时也必须由累计持流预算把前奏写出")
	require.Contains(t, rec.Body.String(), `"type":"ping"`)
}

// 前奏帧先播种较晚的累计预算，随后到达正文提交点时，静默窗口/单次上限可能变得更早。
// 定时器必须按新的最早截止线重排；否则上游在旧预算到点前断流，会被误判成可切号的未提交流，
// 丢掉本来已经可以交付的前奏。
func TestAnthropicPassthrough_HoldbackRearmsEarlierDeadlineAfterPrelude(t *testing.T) {
	const sessionKey = "holdback-rearm-earlier-deadline"
	const groupID = int64(1)
	cache := newShortTurnStreakCache(sessionKey, 9)
	svc, _ := newHoldbackTestGatewayService(t, cache, 20)
	svc.cfg.Gateway.AnthropicHoldbackMaxHoldMs = 50
	svc.cfg.Gateway.AnthropicHoldbackDeadAirBudgetMs = 200

	head := strings.Join([]string{
		`data: {"type":"message_start","message":{"usage":{"input_tokens":129000}}}`,
		"",
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		"",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"先看一下。"}}`,
		"",
		"",
	}, "\n")
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       &stallThenEOF{head: strings.NewReader(head), stall: 100 * time.Millisecond},
	}

	rec, _, err := runHoldbackPassthrough(t, svc, groupID, sessionKey, 9, resp, nil)

	var failoverErr *UpstreamFailoverError
	require.Error(t, err, "上游没有终止事件，放行后仍应报告残缺流")
	require.False(t, errors.As(err, &failoverErr),
		"正文提交点后的更早截止线必须先唤醒并放行，不能等到 EOF 才误切号")
	require.Contains(t, rec.Body.String(), "先看一下。")
}

// pacedBody 按固定间隔逐个交出 SSE 事件，用来构造「帧一直在来、但总持流时长远超窗口」
// 的流。stallThenEOF 构造的是相反的形态（先给完再长时间静默）。
type pacedBody struct {
	events []string
	gap    time.Duration
	buf    string
	idx    int
}

func (p *pacedBody) Read(dst []byte) (int, error) {
	if p.buf == "" {
		if p.idx >= len(p.events) {
			return 0, io.EOF
		}
		time.Sleep(p.gap)
		// SSE 的事件终止符是空行，所以每个事件后面跟两个换行。
		p.buf = p.events[p.idx] + "\n\n"
		p.idx++
	}
	n := copy(dst, p.buf)
	p.buf = p.buf[n:]
	return n, nil
}

func (p *pacedBody) Close() error { return nil }

// pacedThenReadError 与 pacedBody 的唯一差别是收尾：事件放完之后给一个读错误而不是 io.EOF，
// 复现 2026-08-27 23:24:24 那一发上游中途 unexpected EOF 的形态。EOF 走的是 scanner 正常结束
// 那条分支，读错误走 ev.err 那条——线上那一发是后者。
type pacedThenReadError struct {
	pacedBody
	err error
}

func (p *pacedThenReadError) Read(dst []byte) (int, error) {
	n, err := p.pacedBody.Read(dst)
	if err == io.EOF {
		return n, p.err
	}
	return n, err
}

func (p *pacedThenReadError) Close() error { return nil }

// zeroProseThinkingTrickleEvents 构造 23:24:24 那一发的形态：只有 message_start + 一个思考块 +
// 慢速涓流的 thinking_delta，全程零正文、没有 stop_reason、没有终止事件。
//
// 思考刻意压在 anthropicShortTurnThinkingRuneFloor(200) 之下（12 帧 × 14 = 168 rune），
// 这样放宽只可能来自 longThinkingJudgementPending 的零正文入口，不会被 200 那道闸门顺带解锁。
func zeroProseThinkingTrickleEvents() []string {
	events := []string{
		`data: {"type":"message_start","message":{"usage":{"input_tokens":226880,"output_tokens":1}}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
	}
	for i := 0; i < 12; i++ {
		events = append(events,
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"我得先想清楚这里的因果链条。"}}`)
	}
	return events
}

// 2026-08-25 02:06:53 那一发的端到端回归：长思考回合里帧一直在来，总持流时长是窗口的
// 一倍半，但任何相邻两帧之间都没有静默满一个窗口。
//
// 旧实现从首个提交帧起算**总时长**，而 anthropicSSEPayloadCommitsResponse 把
// thinking_delta 算作提交帧，于是定时器在思考期间就开火、无条件 flushPendingPrelude()：
// 字节出去、200 钉死，等 stop_reason 到达时判定虽然照旧判可疑，也只来得及给下一发解绑。
// 改成静默口径之后定时器只是唤醒器，开火后复核发现帧还在来就续期，判定得以等到 stop_reason。
func TestAnthropicPassthrough_HoldbackSurvivesLongThinkingWithoutExposure(t *testing.T) {
	const sessionKey = "holdback-long-thinking"
	const groupID = int64(1)
	const windowMs = 500
	const gap = 30 * time.Millisecond
	cache := newShortTurnStreakCache(sessionKey, 10)
	svc, repo := newHoldbackTestGatewayService(t, cache, windowMs)

	events := []string{
		`data: {"type":"message_start","message":{"usage":{"input_tokens":129000,"output_tokens":2}}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
	}
	// 20 帧思考，单帧间隔只有窗口的 6%，整段思考期却是窗口的 1.2 倍。
	for i := 0; i < 20; i++ {
		events = append(events, fmt.Sprintf(
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":%q}}`,
			"我得先想清楚这里的因果链条。"))
	}
	// 收尾是 16:42:55 那一发的形态：思考 280 rune，正文只有 "d." 两个字符。
	events = append(events,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"d."}}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":577}}`,
		`data: {"type":"message_stop"}`,
	)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       &pacedBody{events: events, gap: gap},
	}

	started := time.Now()
	rec, c, err := runHoldbackPassthrough(t, svc, groupID, sessionKey, 10, resp, nil)
	require.Greater(t, time.Since(started), windowMs*time.Millisecond,
		"构造前提：持流总时长必须超过一个窗口，否则这条用例退化成快流、验不出续期")

	var failoverErr *UpstreamFailoverError
	require.True(t, errors.As(err, &failoverErr), "帧还在来就不该放行，判定必须等到 stop_reason")
	require.Equal(t, GatewayFailureReason("anthropic_short_turn_holdback"), failoverErr.Reason)
	require.True(t, failoverErr.ShouldRetryNextAccount())

	// 零暴露：这是整条用例的重点。旧口径下这里会拿到那段思考的字节。
	require.Empty(t, rec.Body.String(), "判定成形之前一个字节都不能写给客户端")
	require.False(t, c.Writer.Written())
	require.Equal(t, 1, anthropicHoldbackDiscardsUsed(c))
	require.Equal(t, 1, repo.tempCalls, "持流丢弃在解绑时必须冷却账号")
	require.Equal(t, 1, cache.deletedSessions[sessionKey], "丢弃必须同时解绑")
}

// 2026-08-24 16:42:55 / 16:51:15 两发的端到端复现（usage_logs id=9514 out=577、
// id=9523 out=445，账号 10）：思考链很长，正文只有 "d." 两个字符就 end_turn 收尾。
//
// 这一形态是把窗口从 3000 调到 15000 之后**依然**漏掉的那一类，而原因不在窗口：思考
// token 计入 output_tokens，577 越过当时 128 的 anthropicShortTurnOutputTokenLimit 那道
// 闸门，anthropicTurnLooksSuspiciouslyShort 直接 return false，于是它压根没被判为可疑，
// 持流一律判 Release、三个检测器全部沉默、连解绑都不触发。两发的
// duration_ms - first_token_ms 都是 6.1 秒，窗口再宽也拦不住一个免检的形态。
//
// 所以这条用例守的是「思考判据排在 token 闸门之前」这个顺序，与窗口大小无关。闸门后来
// 抬到 320，577 仍在它之上，这条用例的前提不受影响。
func TestAnthropicPassthrough_HoldbackDiscardsThinkingInflatedTurnWithoutExposure(t *testing.T) {
	const sessionKey = "holdback-thinking-inflated"
	const groupID = int64(1)
	cache := newShortTurnStreakCache(sessionKey, 10)
	svc, repo := newHoldbackTestGatewayService(t, cache, 3000)

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: sseBody(
			`data: {"type":"message_start","message":{"usage":{"input_tokens":129000,"output_tokens":2}}}`,
			"",
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
			"",
			// 14 rune × 60 = 840 rune，远超 anthropicShortTurnThinkingRuneFloor。
			fmt.Sprintf(`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":%q}}`,
				strings.Repeat("我得先想清楚这里的因果链条。", 60)),
			"",
			`data: {"type":"content_block_stop","index":0}`,
			"",
			`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
			"",
			`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"d."}}`,
			"",
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":577}}`,
			"",
			`data: {"type":"message_stop"}`,
			"",
			"",
		),
	}

	rec, c, err := runHoldbackPassthrough(t, svc, groupID, sessionKey, 10, resp, nil)

	var failoverErr *UpstreamFailoverError
	require.True(t, errors.As(err, &failoverErr),
		"思考撑大 output_tokens 的截断必须可 failover，否则 token 闸门会让它全程免检")
	require.Equal(t, GatewayFailureReason("anthropic_short_turn_holdback"), failoverErr.Reason)
	require.True(t, failoverErr.ShouldRetryNextAccount())

	// 零暴露：这一形态原先是原样交付的，客户端亲眼看到过那两个字符。
	require.Empty(t, rec.Body.String(), `"d." 一个字节都不能写给客户端`)
	require.False(t, c.Writer.Written())
	require.Equal(t, 1, anthropicHoldbackDiscardsUsed(c))

	require.Equal(t, 1, cache.deletedSessions[sessionKey], "丢弃必须同时解绑，否则下一发还是这个号")
	// 有正文（哪怕只有两个字符）就不是空回合，但达到解绑阈值仍要冷却当前账号。
	require.Equal(t, 1, repo.tempCalls, "达到解绑阈值后必须冷却账号")
}

// 2026-08-24 17:02:33 那一发的端到端复现（usage_logs id=9544，账号 10）：上游把
// output_tokens 报成 0，而字节**确实**出去了——first_token_ms=10808 之后还跑了 2680ms。
//
// 改动前这一档落在两套判定之间的缝里：anthropicTurnLooksSuspiciouslyShort 见
// output_tokens<=0 就无条件 return false（让给残缺判定），而
// anthropicStreamLooksIncompleteDespiteTerminal 要求 visibleChars==0（这里有字节）也不认。
// 两边都不认，于是零信号、零记账、原样交付。收紧成 outputTokens<=0 && proseRunes==0 之后，
// 有正文的这一档回到短回合判据里，由正文长度说话。
func TestAnthropicPassthrough_HoldbackDiscardsZeroUsageWithProseWithoutExposure(t *testing.T) {
	const sessionKey = "holdback-zero-usage"
	const groupID = int64(1)
	cache := newShortTurnStreakCache(sessionKey, 10)
	svc, repo := newHoldbackTestGatewayService(t, cache, 3000)

	rec, c, err := runHoldbackPassthrough(t, svc, groupID, sessionKey, 10,
		shortTurnSSE("我看一下日志。", 0, false), nil)

	var failoverErr *UpstreamFailoverError
	require.True(t, errors.As(err, &failoverErr),
		"usage 报 0 却吐了字节，必须仍按短回合判，否则两套判定都不认它")
	require.Equal(t, GatewayFailureReason("anthropic_short_turn_holdback"), failoverErr.Reason)
	require.True(t, failoverErr.ShouldRetryNextAccount())

	require.Empty(t, rec.Body.String(), "截断内容一个字节都不能写给客户端")
	require.False(t, c.Writer.Written())
	require.Equal(t, 1, anthropicHoldbackDiscardsUsed(c))

	require.Equal(t, 1, cache.deletedSessions[sessionKey])
	// 这一档由正文长度归入短回合；达到解绑阈值后仍需冷却当前账号。
	require.Equal(t, 1, repo.tempCalls, "达到解绑阈值后必须冷却账号")
}

// ---------------------------------------------------------------------------
// 非流式链路
//
// 2026-08-27 13:46:46 的精确回归。整套判定原先只挂在 SSE 路径上，`stream: false` 的请求走
// handleNonStreamingResponseAnthropicAPIKeyPassthrough，一行判定都不过：那一发
// （usage_logs id=14515，账号 9，output_tokens=102、stop_reason=end_turn、无 tool_use、
// 正文 79 rune）在流式口径下必然判可疑，却既没有 holdback_failover 也没有
// sticky_short_turn_unbound，粘性照旧把下一发送回同一个号。24 小时窗口里非流式回合 189 发、
// output_tokens<=430 的占 99.5%，全部零覆盖。
//
// 非流式没有持流、没有截止线——响应体到手时判据就全在里面，丢弃是纯零暴露。所以这一组用例
// 的断言重点与流式侧一致：`errors.As(err, &failoverErr)` **并且** rec.Body 一个字节都没有。
// ---------------------------------------------------------------------------

// nonStreamMessageBody 拼一条协议完全合法的非流式 Anthropic 响应：给定的 content 块 +
// stop_reason + usage。contentBlocks 传原始 JSON 片段，方便逐个用例摆出不同块序。
func nonStreamMessageBody(stopReason string, outputTokens int, contentBlocks ...string) []byte {
	return []byte(fmt.Sprintf(
		`{"id":"msg_nonstream","type":"message","role":"assistant","model":"claude-opus-5",`+
			`"content":[%s],"stop_reason":%q,"stop_sequence":null,`+
			`"usage":{"input_tokens":32944,"cache_read_input_tokens":83103,"output_tokens":%d}}`,
		strings.Join(contentBlocks, ","), stopReason, outputTokens))
}

func nonStreamTextBlock(text string) string {
	return fmt.Sprintf(`{"type":"text","text":%q}`, text)
}

func nonStreamResponse(body []byte) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"application/json"},
			"x-request-id": []string{"rid-nonstream"},
		},
		Body: io.NopCloser(strings.NewReader(string(body))),
	}
}

// runNonStreamPassthrough 与 runHoldbackPassthrough 一一对应，只是走非流式那条函数。
func runNonStreamPassthrough(
	t *testing.T, svc *GatewayService, groupID int64, sessionKey string,
	accountID int64, body []byte, prepare func(*gin.Context),
) (*httptest.ResponseRecorder, *gin.Context, error) {
	t.Helper()
	c, rec := newRefusalTestContext(t)
	if prepare != nil {
		prepare(c)
	}
	ctx := WithStickySessionScope(context.Background(), groupID, sessionKey, false)
	_, err := svc.handleNonStreamingResponseAnthropicAPIKeyPassthrough(
		ctx, nonStreamResponse(body), c,
		&Account{ID: accountID, Name: "primary", Platform: PlatformAnthropic},
		"claude-opus-5")
	return rec, c, err
}

// 13:46:46 那一发的原形：out=102、end_turn、79 rune 正文。必须在写出任何字节之前换号。
func TestAnthropicPassthrough_NonStreamDiscardsShortTurnWithoutExposure(t *testing.T) {
	const sessionKey = "nonstream-short-turn"
	const groupID = int64(1)
	cache := newShortTurnStreakCache(sessionKey, 9)
	svc, repo := newHoldbackTestGatewayService(t, cache, 3000)

	rec, c, err := runNonStreamPassthrough(t, svc, groupID, sessionKey, 9,
		nonStreamMessageBody("end_turn", 102, nonStreamTextBlock(
			"我看一下这个文件。临时文件都已清理删除，现在只剩部署目录。接下来我去读一下日志，确认那几条归因事件有没有落盘。")),
		nil)

	var failoverErr *UpstreamFailoverError
	require.True(t, errors.As(err, &failoverErr),
		"非流式短回合必须也能换号，否则 99.5% 的非流式回合零覆盖")
	require.Equal(t, GatewayFailureReason("anthropic_short_turn_holdback"), failoverErr.Reason)
	require.Equal(t, http.StatusBadGateway, failoverErr.StatusCode)
	require.True(t, failoverErr.ShouldRetryNextAccount())
	require.False(t, failoverErr.RetryableOnSameAccount)
	require.Equal(t, GatewayFailureScopeRequest, failoverErr.Scope)
	require.True(t, failoverErr.RequestScopedTransient)

	// 零暴露：非流式在 c.Data() 之前就拿到了全部判据，这两行必须成立。
	require.Empty(t, rec.Body.String(), "截断内容一个字节都不能写给客户端")
	require.False(t, c.Writer.Written())

	require.Equal(t, 1, anthropicHoldbackDiscardsUsed(c))
	require.Equal(t, 1, cache.deletedSessions[sessionKey], "丢弃必须同时解绑，否则下一发还是这个号")
	require.NotContains(t, cache.sessionBindings, sessionKey)
	require.Equal(t, 1, repo.tempCalls, "达到解绑阈值后必须冷却账号")
}

// 长正文由 verdict 自己放行：判据只有下界闸门，越过上限就不该再攥。
func TestAnthropicPassthrough_NonStreamDeliversLongTurn(t *testing.T) {
	const sessionKey = "nonstream-long-turn"
	cache := newShortTurnStreakCache(sessionKey, 9)
	svc, repo := newHoldbackTestGatewayService(t, cache, 3000)

	body := nonStreamMessageBody("end_turn", 1200, nonStreamTextBlock(
		strings.Repeat("长", anthropicShortTurnProseRuneLimit+1)))
	rec, c, err := runNonStreamPassthrough(t, svc, 1, sessionKey, 9, body, nil)

	require.NoError(t, err)
	require.JSONEq(t, string(body), rec.Body.String(), "正常回合必须逐字节原样交付")
	require.Equal(t, 0, anthropicHoldbackDiscardsUsed(c))
	require.Equal(t, 0, repo.tempCalls)
	require.Equal(t, int64(9), cache.sessionBindings[sessionKey], "正常回合不得动粘性绑定")
}

// tool_use 中间回合天生短，绝不能判可疑——否则每一次工具调用都要白烧一次换号。
func TestAnthropicPassthrough_NonStreamDeliversToolUseTurn(t *testing.T) {
	const sessionKey = "nonstream-tool-use"
	cache := newShortTurnStreakCache(sessionKey, 9)
	svc, repo := newHoldbackTestGatewayService(t, cache, 3000)

	body := nonStreamMessageBody("tool_use", 61,
		nonStreamTextBlock("我读一下这个文件。"),
		`{"type":"tool_use","id":"toolu_1","name":"Read","input":{"file_path":"/tmp/a"}}`)
	rec, c, err := runNonStreamPassthrough(t, svc, 1, sessionKey, 9, body, nil)

	require.NoError(t, err)
	require.JSONEq(t, string(body), rec.Body.String())
	require.Equal(t, 0, anthropicHoldbackDiscardsUsed(c))
	require.Equal(t, 0, repo.tempCalls)
	require.Equal(t, int64(9), cache.sessionBindings[sessionKey])
}

// 丢弃额度必须一票否决：用户的问题本来就只有一句话答案时，不设上限会一路换号到调度耗尽，
// 把一个完好的短回答变成 502。
func TestAnthropicPassthrough_NonStreamShortTurnBudgetCapsRetries(t *testing.T) {
	const sessionKey = "nonstream-budget"
	cache := newShortTurnStreakCache(sessionKey, 9)
	svc, _ := newHoldbackTestGatewayService(t, cache, 3000)

	body := nonStreamMessageBody("end_turn", 70, nonStreamTextBlock("好的，已经改完了。"))
	rec, c, err := runNonStreamPassthrough(t, svc, 1, sessionKey, 9, body, func(c *gin.Context) {
		c.Set(anthropicHoldbackDiscardsKey, anthropicEmptyAnswerDiscardBudget)
	})

	require.NoError(t, err, "额度耗尽后必须退化成放行，而不是把真短答案重试成 502")
	require.JSONEq(t, string(body), rec.Body.String())
	require.Equal(t, anthropicEmptyAnswerDiscardBudget, anthropicHoldbackDiscardsUsed(c),
		"放行不得再吃额度")
}

// 块序违规在非流式侧同样是确定性判据，走独立额度、独立归因。
func TestAnthropicPassthrough_NonStreamDiscardsBlockOrderViolation(t *testing.T) {
	const sessionKey = "nonstream-block-order"
	cache := newShortTurnStreakCache(sessionKey, 9)
	svc, repo := newHoldbackTestGatewayService(t, cache, 3000)

	// text 之后又冒出 thinking：扩展思考协议里不存在这种形态，只能是上游拼错了块序。
	// 正文刻意超过短回合上限，证明这一档没有被「长正文提前放行」短路掉。
	rec, c, err := runNonStreamPassthrough(t, svc, 1, sessionKey, 9,
		nonStreamMessageBody("end_turn", 591,
			nonStreamTextBlock("Tool results:\n\n[Bash] SYNTAX OK"),
			`{"type":"thinking","thinking":"`+strings.Repeat("思", 240)+`"}`,
			nonStreamTextBlock(strings.Repeat("正", anthropicShortTurnProseRuneLimit+1))),
		nil)

	var failoverErr *UpstreamFailoverError
	require.True(t, errors.As(err, &failoverErr))
	require.Equal(t, GatewayFailureReason("anthropic_block_order_violation"), failoverErr.Reason)
	require.Empty(t, rec.Body.String(), "伪造的工具输出一个字节都不能交给客户端")
	require.False(t, c.Writer.Written())
	require.Equal(t, 1, anthropicBlockOrderDiscardsUsed(c))
	require.Equal(t, 0, anthropicHoldbackDiscardsUsed(c), "块序违规不得吃启发式额度")
	require.Equal(t, 1, cache.deletedSessions[sessionKey])
	require.Equal(t, 1, repo.tempCalls)
}

// 关掉持流窗口就整套机制关闭，两条链路含义必须一致。
func TestAnthropicPassthrough_NonStreamHoldbackDisabledDeliversShortTurn(t *testing.T) {
	const sessionKey = "nonstream-disabled"
	cache := newShortTurnStreakCache(sessionKey, 9)
	svc, repo := newHoldbackTestGatewayService(t, cache, 0)

	body := nonStreamMessageBody("end_turn", 102, nonStreamTextBlock("我看一下这个文件。"))
	rec, c, err := runNonStreamPassthrough(t, svc, 1, sessionKey, 9, body, nil)

	require.NoError(t, err)
	require.JSONEq(t, string(body), rec.Body.String())
	require.Equal(t, 0, anthropicHoldbackDiscardsUsed(c))
	require.Equal(t, 0, repo.tempCalls)
	require.Equal(t, int64(9), cache.sessionBindings[sessionKey])
}

// 客户端已经走了就别判：此刻残缺与上游无关，换号只会白烧一次调用。
func TestAnthropicPassthrough_NonStreamSkipsVerdictAfterClientCancel(t *testing.T) {
	const sessionKey = "nonstream-cancelled"
	cache := newShortTurnStreakCache(sessionKey, 9)
	svc, repo := newHoldbackTestGatewayService(t, cache, 3000)

	body := nonStreamMessageBody("end_turn", 102, nonStreamTextBlock("我看一下这个文件。"))
	_, c, err := runNonStreamPassthrough(t, svc, 1, sessionKey, 9, body, func(c *gin.Context) {
		cancelCtx, cancel := context.WithCancel(c.Request.Context())
		cancel()
		c.Request = c.Request.WithContext(cancelCtx)
	})

	require.NoError(t, err, "客户端取消时不得再为启发式换号")
	require.Equal(t, 0, anthropicHoldbackDiscardsUsed(c))
	require.Equal(t, 0, repo.tempCalls)
	require.Equal(t, int64(9), cache.sessionBindings[sessionKey])
}

// 提取口径必须与流式侧逐位对齐，否则同一条回合在两条链路上会得出不同结论。
func TestAnthropicNonStreamTurnShapeFromBody(t *testing.T) {
	t.Run("空体不报错也不产生判据", func(t *testing.T) {
		shape := anthropicNonStreamTurnShapeFromBody(nil)
		require.Equal(t, anthropicNonStreamTurnShape{}, shape)
	})

	t.Run("按 rune 数正文、只数 text 块", func(t *testing.T) {
		shape := anthropicNonStreamTurnShapeFromBody(nonStreamMessageBody("end_turn", 102,
			`{"type":"thinking","thinking":"思思思"}`,
			nonStreamTextBlock("中文四个字"),
			nonStreamTextBlock("再来三字")))
		require.Equal(t, "end_turn", shape.stopReason)
		require.Equal(t, 102, shape.outputTokens)
		require.Equal(t, 9, shape.proseRunes, "两个 text 块按 rune 累加，thinking 不计入正文")
		require.Equal(t, 3, shape.thinkingRunes)
		require.False(t, shape.sawToolUseBlock)
		require.False(t, shape.blockOrder.violation(), "thinking 在 text 之前是合法形态")
	})

	t.Run("redacted_thinking 的载荷在 data 字段", func(t *testing.T) {
		shape := anthropicNonStreamTurnShapeFromBody(nonStreamMessageBody("end_turn", 40,
			`{"type":"redacted_thinking","data":"abcdef"}`, nonStreamTextBlock("答")))
		require.Equal(t, 6, shape.thinkingRunes)
		require.Equal(t, 1, shape.proseRunes)
	})

	t.Run("tool_use 与 server_tool_use 都算工具块", func(t *testing.T) {
		for _, blockType := range []string{"tool_use", "server_tool_use"} {
			shape := anthropicNonStreamTurnShapeFromBody(nonStreamMessageBody("tool_use", 61,
				fmt.Sprintf(`{"type":%q,"id":"t1","name":"Bash","input":{}}`, blockType)))
			require.True(t, shape.sawToolUseBlock, blockType)
		}
	})

	t.Run("text 之后的 thinking 是块序违规", func(t *testing.T) {
		shape := anthropicNonStreamTurnShapeFromBody(nonStreamMessageBody("end_turn", 591,
			nonStreamTextBlock("Tool results:"),
			`{"type":"thinking","thinking":"思"}`,
			nonStreamTextBlock("乱码")))
		require.True(t, shape.blockOrder.violation())
	})
}

// ---------------------------------------------------------------------------
// 常规（非透传）非流式链路
//
// 与上面那一组是同一个盲区的另一半：handleNonStreamingResponse 走的是非透传分支，
// 判定同样一行都不过。两条链路共用 discardNonStreamTurnIfSuspicious，所以这里只需要
// 证明「接线接上了」与「model 口径取的是 originalModel」，判据本身的边界由
// TestAnthropicHoldbackVerdict 覆盖，不重复铺。
// ---------------------------------------------------------------------------

// runNonStreamRegular 跑常规链路的非流式段。cache 必须挂上，否则解绑走不到。
func runNonStreamRegular(
	t *testing.T, svc *GatewayService, groupID int64, sessionKey string,
	accountID int64, body []byte,
) (*httptest.ResponseRecorder, *gin.Context, error) {
	t.Helper()
	c, rec := newRefusalTestContext(t)
	ctx := WithStickySessionScope(context.Background(), groupID, sessionKey, false)
	_, err := svc.handleNonStreamingResponse(
		ctx, nonStreamResponse(body), c,
		&Account{ID: accountID, Name: "primary", Platform: PlatformAnthropic},
		"claude-opus-5", "claude-opus-5")
	return rec, c, err
}

func TestNonStreamingResponse_DiscardsShortTurnWithoutExposure(t *testing.T) {
	const sessionKey = "regular-nonstream-short-turn"
	cache := newShortTurnStreakCache(sessionKey, 9)
	svc, repo := newHoldbackTestGatewayService(t, cache, 3000)

	rec, c, err := runNonStreamRegular(t, svc, 1, sessionKey, 9,
		nonStreamMessageBody("end_turn", 102, nonStreamTextBlock(
			"我看一下这个文件。临时文件都已清理删除，现在只剩部署目录。接下来我去读一下日志。")))

	var failoverErr *UpstreamFailoverError
	require.True(t, errors.As(err, &failoverErr), "常规链路的非流式段同样不能零覆盖")
	require.Equal(t, GatewayFailureReason("anthropic_short_turn_holdback"), failoverErr.Reason)
	require.True(t, failoverErr.ShouldRetryNextAccount())

	require.Empty(t, rec.Body.String(), "截断内容一个字节都不能写给客户端")
	require.False(t, c.Writer.Written())
	require.Equal(t, 1, cache.deletedSessions[sessionKey])
	require.Equal(t, 1, repo.tempCalls)
}

func TestNonStreamingResponse_DeliversHealthyTurn(t *testing.T) {
	const sessionKey = "regular-nonstream-healthy"
	cache := newShortTurnStreakCache(sessionKey, 9)
	svc, repo := newHoldbackTestGatewayService(t, cache, 3000)

	body := nonStreamMessageBody("end_turn", 1200, nonStreamTextBlock(
		strings.Repeat("长", anthropicShortTurnProseRuneLimit+1)))
	rec, _, err := runNonStreamRegular(t, svc, 1, sessionKey, 9, body)

	require.NoError(t, err)
	require.JSONEq(t, string(body), rec.Body.String(), "正常回合必须原样交付")
	require.Equal(t, 0, repo.tempCalls)
	require.Equal(t, int64(9), cache.sessionBindings[sessionKey])
}

// 非 JSON 的 2xx 必须仍然落到既有的 invalidNonStreamingJSONFailoverError 上，
// 不能被新加的判定抢先（那条路径的归因是 non-JSON，不是短回合）。
func TestNonStreamingResponse_NonJSONStillFailsOverAsInvalidJSON(t *testing.T) {
	cache := newShortTurnStreakCache("regular-nonstream-nonjson", 9)
	svc, _ := newHoldbackTestGatewayService(t, cache, 3000)

	c, rec := newRefusalTestContext(t)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/plain"}},
		Body:       io.NopCloser(strings.NewReader("(upstream request failed)")),
	}
	_, err := svc.handleNonStreamingResponse(
		context.Background(), resp, c,
		&Account{ID: 9, Name: "primary", Platform: PlatformAnthropic},
		"claude-opus-5", "claude-opus-5")

	var failoverErr *UpstreamFailoverError
	require.True(t, errors.As(err, &failoverErr))
	require.Equal(t, http.StatusBadGateway, failoverErr.StatusCode)
	require.Empty(t, rec.Body.String())
	require.Equal(t, 0, anthropicHoldbackDiscardsUsed(c), "非 JSON 体不得吃启发式额度")
}

// 2026-08-27 14:12:03 那一发的回归用例（usage_logs id=14596，账号 9，stream=t）：
// out=851、prose=481 rune、first_token_ms=18174、duration_ms=54785。持流上线后
// first_token_ms 等于放行时刻，所以 firstCommitPointAt ≈ 8174ms、10 秒上限在 18174ms 到点
// 放行，而 stop_reason 直到 54785ms 才来——判定迟到 36.6 秒，日志里那条
// anthropic_short_turn_streak_unbind 只能记成 disposition=delivered。
//
// 这一类是 AnthropicHoldbackMaxHoldMs 立论的例外：那条上限说「out>430 结构性不可判，攥着纯
// 亏」，而 anthropicVisibleOutputTokens 会把 851 按 prose/(prose+thinking) 折算，
// thinking≥509 rune 时就落回 430 闸门之内——所以它**能**被判可疑，攥着有收益。
func TestAnthropicHoldbackObserverLongThinkingExtendsTotalHoldCaps(t *testing.T) {
	const window = 15 * time.Second
	const maxHold = 10 * time.Second
	const deadAir = 25 * time.Second
	const floor = 60 * time.Second

	// 真实时间轴：t0 是请求起点，首帧 SSE 在 8174ms，stop_reason 在 54785ms。
	t0 := time.Date(2026, 8, 27, 14, 12, 3, 0, time.UTC)
	firstFrameAt := t0.Add(8174 * time.Millisecond)
	commitPointAt := firstFrameAt.Add(26 * time.Millisecond)
	stopReasonAt := t0.Add(54785 * time.Millisecond)

	newObserver := func(longThinkingFloor time.Duration) *anthropicHoldbackObserver {
		o := &anthropicHoldbackObserver{longThinkingHoldFloor: longThinkingFloor}
		o.observe(gjson.Parse(`{"type":"message_start","message":{"usage":{"input_tokens":32944,"output_tokens":2}}}`), false, firstFrameAt)
		o.observe(gjson.Parse(`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`), false, commitPointAt)
		// 14 rune × 40 = 560 rune 思考，越过 anthropicShortTurnThinkingRuneFloor(200)，
		// 也满足折算把 851 压回 430 之内所需的 509。帧一路铺到 stop_reason 之前。
		at := commitPointAt
		for i := 0; i < 40; i++ {
			o.observe(gjson.Parse(fmt.Sprintf(
				`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":%q}}`,
				"我得先想清楚这里的因果链条。")), true, at)
			at = at.Add(time.Second)
		}
		return o
	}

	// 放宽关掉（floor=0）时复现线上行为：两条总时长线都在 stop_reason 之前到点。
	off := newObserver(0)
	require.Equal(t, commitPointAt, off.firstCommitPointAt)
	require.Equal(t, firstFrameAt, off.holdbackStartedAt)
	require.GreaterOrEqual(t, off.thinkingRunes, 509, "构造前提：折算必须能把 851 压回闸门之内")
	require.False(t, off.windowElapsed(stopReasonAt, window),
		"构造前提：思考帧一直在来，静默那条按定义不会到点")
	require.True(t, off.maxHoldElapsed(stopReasonAt, maxHold), "线上现状：10 秒上限先到点")
	require.True(t, off.deadAirElapsed(stopReasonAt, deadAir), "25 秒死气预算同样先到点")
	require.True(t, off.releaseDeadlineElapsed(stopReasonAt, window, maxHold, deadAir),
		"于是缓冲在判定成形之前就被放行——这就是 14:12:03 的根因")

	// 放宽打开后，三条线全部推到 60 秒，stop_reason 到达时判定窗口仍然完好。
	on := newObserver(floor)
	require.False(t, on.maxHoldElapsed(stopReasonAt, maxHold), "长思考回合的总时长上限抬到 60 秒")
	require.False(t, on.deadAirElapsed(stopReasonAt, deadAir), "死气预算必须同步抬高，只放宽一条等于没放宽")
	require.False(t, on.windowElapsed(stopReasonAt, window), "静默窗口同样抬到 60 秒")
	require.False(t, on.releaseDeadlineElapsed(stopReasonAt, window, maxHold, deadAir),
		"判定得以等到 stop_reason，这一发才可能被零暴露丢弃")
	// 定时器与判定共用一个口径：三条线都必须同样被推到 60 秒处。
	require.Equal(t, commitPointAt.Add(floor), on.holdbackMaxHoldDeadline(maxHold))
	require.Equal(t, firstFrameAt.Add(floor), on.holdbackDeadAirDeadline(deadAir))
	require.Equal(t, on.lastContentFrameAt.Add(floor), on.holdbackSilenceDeadline(window))
	// 三条一起被推远之后，先到的是死气那条（第一帧 SSE +60s，比 firstCommitPointAt 更早起算）。
	// 它仍在 stop_reason 之后，判定窗口完好；定时器只认这一个口径，不得与判定分叉。
	releaseAt := on.holdbackReleaseDeadline(window, maxHold, deadAir)
	require.Equal(t, on.holdbackDeadAirDeadline(deadAir), releaseAt,
		"三条同步放宽后先到的是起算最早的死气那条")
	require.True(t, releaseAt.After(stopReasonAt), "先到的截止线必须晚于 stop_reason")
	require.False(t, on.releaseDeadlineElapsed(releaseAt.Add(-time.Millisecond), window, maxHold, deadAir))
	require.True(t, on.releaseDeadlineElapsed(releaseAt, window, maxHold, deadAir),
		"判定必须与 holdbackReleaseDeadline 在同一毫秒上翻转")

	// 抬高仍有硬上界。上面那条流由起算最早的死气线兜住；这里换成帧一路铺过 60 秒的流，
	// 专门验总时长上限这条下限本身封得住——否则放宽就变成了无限攥着。
	paced := &anthropicHoldbackObserver{longThinkingHoldFloor: floor}
	paced.observe(gjson.Parse(`{"type":"message_start","message":{"usage":{"input_tokens":32944}}}`), false, firstFrameAt)
	for at := commitPointAt; !at.After(commitPointAt.Add(floor + 10*time.Second)); at = at.Add(time.Second) {
		paced.observe(gjson.Parse(fmt.Sprintf(
			`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":%q}}`,
			"我得先想清楚这里的因果链条。")), true, at)
	}
	require.True(t, paced.longThinkingJudgementPending())
	require.False(t, paced.windowElapsed(commitPointAt.Add(floor), window),
		"构造前提：帧一路在来，静默那条不参与，只剩下限兜底")
	require.False(t, paced.maxHoldElapsed(commitPointAt.Add(floor-time.Millisecond), maxHold))
	require.True(t, paced.maxHoldElapsed(commitPointAt.Add(floor), maxHold),
		"下限本身是硬上界，不能变成无限攥着")

	// 自限：stop_reason 一到，放宽立刻失效——判据齐了就该当帧定案，没有继续放宽的理由。
	on.observe(gjson.Parse(`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":851}}`), false, stopReasonAt)
	require.False(t, on.longThinkingJudgementPending())
	require.True(t, on.maxHoldElapsed(stopReasonAt, maxHold), "拿到 stop_reason 后上限回落到 10 秒")
	require.Equal(t, commitPointAt.Add(maxHold), on.holdbackMaxHoldDeadline(maxHold))
}

// 放宽的边界：正文已经越过 anthropicShortTurnProseRuneLimit 的回合一律不放宽。
//
// 2026-08-28 重钉。这一条原先守的是「思考不够长 + 已经在写正文就不放宽」，立论是
// out>430 的长回答结构性不可判、攥着纯亏首字延迟（改静默口径时实测 p90 首字节 42s→113s）。
// 那条立论在持流期站不住：outputTokens 的终值与 stop_reason 同帧到达，持流全程只能看到
// message_start 里的初值（实测 1~2），所以「这一发最终会不会越过 430」在放行时刻是不可知的。
// 分不开的两类里，攥住是唯一能保住换号窗口的做法；这一档的首字节延迟由用户 2026-08-28
// 明确让出（「愿意专门为 out>430 这一档让出首字节延迟」）。
//
// 换上来的边界是持流期唯一单调可用的判据：正文长度。正文一旦越过上限，
// anthropicTurnLooksSuspiciouslyShort 就再也不可能判可疑，同一刻 anthropicHoldbackVerdict
// 自己那条 proseRunes > 上限的提前放行出口也同帧生效，两处口径严格对齐。
func TestAnthropicHoldbackObserverLongProseStopsRelaxing(t *testing.T) {
	const maxHold = 10 * time.Second
	const deadAir = 25 * time.Second
	const floor = 60 * time.Second
	base := time.Date(2026, 8, 27, 14, 12, 3, 0, time.UTC)

	o := &anthropicHoldbackObserver{longThinkingHoldFloor: floor}
	o.observe(gjson.Parse(`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`), false, base)
	// 14 rune × 10 = 140 rune，低于 anthropicShortTurnThinkingRuneFloor(200)：
	// 第一个入口拿不到，这一条测的就是第三个入口自己的边界。
	o.observe(gjson.Parse(fmt.Sprintf(
		`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":%q}}`,
		strings.Repeat("我得先想清楚这里的因果链条。", 10))), true, base)
	// 正文越过上限：这一发再也不可能被判可疑，攥着拿不到任何收益。
	o.observe(gjson.Parse(fmt.Sprintf(
		`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":%q}}`,
		strings.Repeat("这", anthropicShortTurnProseRuneLimit+1))), true, base)
	require.Less(t, o.thinkingRunes, anthropicShortTurnThinkingRuneFloor, "构造前提：思考不够长，第一个入口不成立")
	require.Greater(t, o.proseRunes, anthropicShortTurnProseRuneLimit, "构造前提：正文已越过上限")
	require.False(t, o.longThinkingJudgementPending())
	require.True(t, o.maxHoldElapsed(base.Add(maxHold), maxHold), "正文越过上限时 10 秒上限照旧生效")
	require.True(t, o.deadAirElapsed(base.Add(deadAir), deadAir))
	require.Equal(t, base.Add(maxHold), o.holdbackMaxHoldDeadline(maxHold))
	require.Equal(t, base.Add(15*time.Second), o.holdbackSilenceDeadline(15*time.Second),
		"不放宽时静默窗口保持基准值")

	// 对照：同样是思考不够长，正文卡在上限上（还没越过）就必须仍在放宽状态——判定还可能
	// 落在 Discard 上，这一档正是 18:14:29（prose=849 out=399 disposition=delivered）要治的。
	atLimit := &anthropicHoldbackObserver{longThinkingHoldFloor: floor}
	atLimit.observe(gjson.Parse(`{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`), true, base)
	atLimit.observe(gjson.Parse(fmt.Sprintf(
		`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":%q}}`,
		strings.Repeat("这", anthropicShortTurnProseRuneLimit))), true, base)
	require.Equal(t, anthropicShortTurnProseRuneLimit, atLimit.proseRunes, "构造前提：正好卡在上限上")
	require.True(t, atLimit.longThinkingJudgementPending(), "卡在上限上仍可能判可疑，必须继续攥")
	require.False(t, atLimit.maxHoldElapsed(base.Add(maxHold), maxHold))
	require.Equal(t, base.Add(floor), atLimit.holdbackMaxHoldDeadline(maxHold))

	// 显式配 0 关掉的截止线不得被下限重新打开：那会让「关掉某条线」的意图被悄悄推翻。
	long := &anthropicHoldbackObserver{longThinkingHoldFloor: floor}
	long.observe(gjson.Parse(fmt.Sprintf(
		`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":%q}}`,
		strings.Repeat("我得先想清楚这里的因果链条。", 40))), true, base)
	require.True(t, long.longThinkingJudgementPending(), "构造前提：这一条确实处在放宽状态")
	require.False(t, long.maxHoldElapsed(base.Add(time.Hour), 0), "上限配 0 时下限也不得把它拉起来")
	require.True(t, long.holdbackMaxHoldDeadline(0).IsZero())
	require.False(t, long.deadAirElapsed(base.Add(time.Hour), 0))
	require.True(t, long.holdbackDeadAirDeadline(0).IsZero())

	// 下限比基准值还小时等于不放宽，取两者的大者。
	require.True(t, long.maxHoldElapsed(base.Add(2*time.Minute), 90*time.Second),
		"基准值本身比下限更宽时以基准值为准")
}

// 2026-08-27 23:24:24 那一发的观察器级回归（usage_logs id=15219，账号 11 justwoker-anthropic，
// in=226880 out=0 first_token_ms=15361 duration_ms=33445）。
//
// 形态：思考慢速涓流、全程零正文、stop_reason 始终没来。整发只积了 337 rune 思考、耗时
// 33.4 秒，约 12 rune/s；放行发生在 firstCommitPointAt(5361ms) + 10s 上限 = 15361ms，
// 那一刻思考只累到 120 rune 上下，200 那道闸门够不着，于是放宽拿不到、缓冲被放出去。上游
// 18.084 秒之后才 EOF，此时 streamCommitted 已为真，读错误只能走
// reportStreamTruncatedAfterCommit——「stream truncated after commit, no failover possible」。
//
// 判定按帧推进，所以每一处判定都必须拿**那一刻**的观察器状态去问，不能拿整发终值。下面按
// 时刻各建一个观察器，就是为了不把 337 这个终值误当成放行时刻的状态。
//
// 这一条要钉死两件事：
//  1. 零正文入口不看思考长短。放行时刻的 126 rune 远低于闸门，放宽照样必须成立。
//  2. 三条线必须一起被推远。只抬两条总时长线的话，静默窗口反过来成了最短的那条，放行权
//     原封不动地换了个持有者，放宽等于没做。
func TestAnthropicHoldbackObserverZeroProseThinkingExtendsAllThreeCaps(t *testing.T) {
	const window = 15 * time.Second
	const maxHold = 10 * time.Second
	const deadAir = 25 * time.Second
	const floor = 60 * time.Second

	// 真实时间轴：t0 是 gateway_check_start，首个 thinking_delta 在 5361ms（= 放行时刻
	// 15361ms - 10s 上限），上游 EOF 在 33445ms。
	t0 := time.Date(2026, 8, 27, 15, 23, 50, 992000000, time.UTC)
	firstFrameAt := t0.Add(5200 * time.Millisecond)
	commitPointAt := t0.Add(5361 * time.Millisecond)
	releasedAt := t0.Add(15361 * time.Millisecond) // 线上实际放行时刻
	eofAt := t0.Add(33445 * time.Millisecond)

	// 每帧 14 rune，按 gap 铺 frames 帧。12 rune/s 的实测速率下 10 秒约合 9 帧 126 rune，
	// 整发 33.4 秒约合 24 帧 336 rune ≈ 线上的 337。
	newObserver := func(longThinkingFloor, gap time.Duration, frames int) *anthropicHoldbackObserver {
		o := &anthropicHoldbackObserver{longThinkingHoldFloor: longThinkingFloor}
		o.observe(gjson.Parse(`{"type":"message_start","message":{"usage":{"input_tokens":226880,"output_tokens":1}}}`), false, firstFrameAt)
		o.observe(gjson.Parse(`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`), false, commitPointAt)
		at := commitPointAt
		for i := 0; i < frames; i++ {
			o.observe(gjson.Parse(fmt.Sprintf(
				`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":%q}}`,
				"我得先想清楚这里的因果链条。")), true, at)
			at = at.Add(gap)
		}
		return o
	}
	const trickleGap = 1150 * time.Millisecond
	const framesAtRelease = 9 // 放行时刻已到的帧数
	const framesAtEOF = 24    // 整发的帧数

	// 放行时刻的状态：10 秒上限到点，而 200 那道闸门还差得远。这就是线上的结局。
	off := newObserver(0, trickleGap, framesAtRelease)
	require.Equal(t, commitPointAt, off.firstCommitPointAt)
	require.Zero(t, off.proseRunes, "构造前提：全程零正文")
	require.Less(t, off.thinkingRunes, anthropicShortTurnThinkingRuneFloor,
		"构造前提：放行那一刻思考涓流还够不到 200 那道闸门")
	require.Empty(t, off.stopReason, "构造前提：stop_reason 始终没来")
	require.False(t, off.windowElapsed(releasedAt, window),
		"构造前提：帧一直在来，放行的不是静默那条")
	require.True(t, off.maxHoldElapsed(releasedAt, maxHold), "线上现状：10 秒上限在 15361ms 到点")
	require.True(t, off.releaseDeadlineElapsed(releasedAt, window, maxHold, deadAir),
		"于是缓冲在 EOF 之前 18.084 秒就被放行——放行之后 streamCommitted 为真，failover 出口关掉")

	// 同一时刻打开放宽：零正文入口必须在这里就成立，否则救不了这一发。
	onAtRelease := newObserver(floor, trickleGap, framesAtRelease)
	require.True(t, onAtRelease.longThinkingJudgementPending(),
		"零正文入口不看思考长短：放行时刻的 126 rune 也必须成立")
	require.False(t, onAtRelease.releaseDeadlineElapsed(releasedAt, window, maxHold, deadAir),
		"放宽必须在放行时刻之前生效，晚一帧都来不及")

	// EOF 时刻的状态：三条线全部抬到 60 秒，缓冲仍在手里。
	on := newObserver(floor, trickleGap, framesAtEOF)
	require.Equal(t, 336, on.thinkingRunes, "构造前提：整发思考总量对齐线上的 337")
	require.False(t, on.maxHoldElapsed(eofAt, maxHold), "总时长上限抬到 60 秒")
	require.False(t, on.deadAirElapsed(eofAt, deadAir), "死气预算同步抬高")
	require.False(t, on.releaseDeadlineElapsed(eofAt, window, maxHold, deadAir),
		"EOF 到达时三条都没到点，streamCommitted 仍为假，failover 出口完好")
	require.Equal(t, commitPointAt.Add(floor), on.holdbackMaxHoldDeadline(maxHold))
	require.Equal(t, firstFrameAt.Add(floor), on.holdbackDeadAirDeadline(deadAir))
	require.Equal(t, on.lastContentFrameAt.Add(floor), on.holdbackSilenceDeadline(window))
	require.True(t, on.holdbackReleaseDeadline(window, maxHold, deadAir).After(eofAt),
		"定时器口径必须与判定一致：先到的那条也要晚于 EOF")

	// 为什么第三条也必须交出去：涓流在 EOF 之前就停住时，静默那条成为最短的一条。上面那条
	// 铺到 EOF 前 1.6 秒的流验不出这一点（静默按定义到不了点），换成同样 336 rune、但在 EOF
	// 前约 19 秒就停住的疏涓流——两条流思考总量相同，唯一差别是帧的疏密。
	stalledOff := newObserver(0, 400*time.Millisecond, framesAtEOF)
	require.True(t, stalledOff.lastContentFrameAt.Add(window).Before(eofAt),
		"构造前提：这条流确实静默满一个基准窗口")
	require.True(t, stalledOff.windowElapsed(eofAt, window),
		"不放宽静默那条时它在 EOF 之前到点——只抬两条总时长线等于把放行权换个持有者")
	stalledOn := newObserver(floor, 400*time.Millisecond, framesAtEOF)
	require.False(t, stalledOn.windowElapsed(eofAt, window),
		"三条同步放宽后静默那条也撑过 EOF")
	require.False(t, stalledOn.releaseDeadlineElapsed(eofAt, window, maxHold, deadAir),
		"这才是放宽真正生效：疏涓流同样保住 failover 窗口")

	// 自限：stop_reason 一到，放宽立刻失效，三条线全部回落到基准值。
	// message_delta 不是 ping，所以它同时把静默起点刷到 eofAt——静默那条从这一刻重新起算 15 秒。
	on.observe(gjson.Parse(`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":337}}`), false, eofAt)
	require.False(t, on.longThinkingJudgementPending())
	require.Equal(t, commitPointAt.Add(maxHold), on.holdbackMaxHoldDeadline(maxHold))
	require.Equal(t, eofAt.Add(window), on.holdbackSilenceDeadline(window),
		"拿到 stop_reason 后静默窗口回落到基准值")
	require.Equal(t, firstFrameAt.Add(deadAir), on.holdbackDeadAirDeadline(deadAir),
		"死气预算同样回落")
}

// longThinkingTruncatedEvents 构造 2026-08-27 14:12:03 那一发的形态：思考期远长于两条总时长
// 类截止线，收尾是 480 rune 正文 + end_turn + output_tokens=851。
//
// 折算之后 851 × 480/(480+980) = 279 <= anthropicShortTurnOutputTokenLimit(430)，也就是说
// 这一发**能**被判可疑；而 851 原值远在闸门之外，正是 AnthropicHoldbackMaxHoldMs 那条上限
// 认定「结构性不可判、攥着纯亏」的那一批。两者的差别全在思考长度上。
func longThinkingTruncatedEvents() []string {
	const thinkingChunk = "我得先想清楚这里的因果链条。" // 14 rune
	events := []string{
		`data: {"type":"message_start","message":{"usage":{"input_tokens":32944,"cache_read_input_tokens":83103,"output_tokens":2}}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
		// 第一帧思考就把 thinkingRunes 顶到 560，越过 anthropicShortTurnThinkingRuneFloor(200)。
		// 刻意不靠后续帧慢慢累积：放宽要在总时长上限到点**之前**生效才有意义，让它从第一帧
		// 思考起就成立，用例才不会去赌「累到 200 rune」和「上限到点」谁先谁后。
		fmt.Sprintf(`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":%q}}`,
			strings.Repeat(thinkingChunk, 40)),
	}
	// 30 帧小思考把帧流铺过两条截止线，同时让静默那条一直被续期。
	for i := 0; i < 30; i++ {
		events = append(events, fmt.Sprintf(
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":%q}}`,
			thinkingChunk))
	}
	return append(events,
		`data: {"type":"content_block_stop","index":0}`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
		fmt.Sprintf(`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":%q}}`,
			strings.Repeat("先按这个方案往下做。", 48)), // 480 rune
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":851}}`,
		`data: {"type":"message_stop"}`,
	)
}

// 2026-08-27 14:12:03 的端到端回归：长思考回合必须挺过两条总时长类截止线，判定成形之前
// 一个字节都不写给客户端。
//
// 与 TestAnthropicPassthrough_HoldbackSurvivesLongThinkingWithoutExposure 的分工：那一条守
// 的是**静默窗口**不被持续到达的帧提前耗尽（02:06:53 的根因），两条总时长线在那条用例里都
// 配成 0。这一条守的是两条总时长线自己——它们不受新帧续期，10s/25s 一到就无条件放行，而
// 长思考回合的判据要到 46.6 秒才齐。
func TestAnthropicPassthrough_HoldbackLongThinkingOutlastsTotalHoldCaps(t *testing.T) {
	const sessionKey = "holdback-long-thinking-max-hold"
	const groupID = int64(1)
	cache := newShortTurnStreakCache(sessionKey, 9)
	svc, repo := newHoldbackTestGatewayService(t, cache, 5000)
	// 两条总时长线都远短于思考期（38 帧 × 20ms ≈ 760ms），放宽下限远长于整条流。
	svc.cfg.Gateway.AnthropicHoldbackMaxHoldMs = 300
	svc.cfg.Gateway.AnthropicHoldbackDeadAirBudgetMs = 400
	svc.cfg.Gateway.AnthropicHoldbackLongThinkingHoldMs = 20000

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       &pacedBody{events: longThinkingTruncatedEvents(), gap: 20 * time.Millisecond},
	}

	started := time.Now()
	rec, c, err := runHoldbackPassthrough(t, svc, groupID, sessionKey, 9, resp, nil)
	require.Greater(t, time.Since(started), 400*time.Millisecond,
		"构造前提：整条流必须长于两条总时长线，否则这条用例验不出放宽")

	var failoverErr *UpstreamFailoverError
	require.True(t, errors.As(err, &failoverErr), "判定必须等到 stop_reason，而不是在 300ms 上限处放行")
	require.Equal(t, GatewayFailureReason("anthropic_short_turn_holdback"), failoverErr.Reason)
	require.True(t, failoverErr.ShouldRetryNextAccount())

	require.Empty(t, rec.Body.String(), "判定成形之前一个字节都不能写给客户端")
	require.False(t, c.Writer.Written())
	require.Equal(t, 1, anthropicHoldbackDiscardsUsed(c))
	require.Equal(t, 1, repo.tempCalls, "持流丢弃在解绑时必须冷却账号")
	require.Equal(t, 1, cache.deletedSessions[sessionKey], "丢弃必须同时解绑")
}

// 同一条流、只把放宽关掉（AnthropicHoldbackLongThinkingHoldMs=0），复现线上 14:12:03 的结局：
// 总时长上限到点放行，字节出去、200 钉死，等 stop_reason 到达时判定虽然照旧判可疑，也只
// 来得及给下一发解绑（日志里那条 disposition=delivered）。
//
// 这一条是上一条的反证：两条用例的唯一差别就是这一个配置项，所以能证明零暴露确实来自放宽，
// 而不是这条流本身就不会被提前放行。
func TestAnthropicPassthrough_HoldbackLongThinkingExposedWhenExtensionDisabled(t *testing.T) {
	const sessionKey = "holdback-long-thinking-no-extension"
	const groupID = int64(1)
	cache := newShortTurnStreakCache(sessionKey, 9)
	svc, _ := newHoldbackTestGatewayService(t, cache, 5000)
	svc.cfg.Gateway.AnthropicHoldbackMaxHoldMs = 300
	svc.cfg.Gateway.AnthropicHoldbackDeadAirBudgetMs = 400
	svc.cfg.Gateway.AnthropicHoldbackLongThinkingHoldMs = 0

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       &pacedBody{events: longThinkingTruncatedEvents(), gap: 20 * time.Millisecond},
	}

	rec, c, err := runHoldbackPassthrough(t, svc, groupID, sessionKey, 9, resp, nil)

	require.NoError(t, err, "放行之后流是完整的，不该报错")
	var failoverErr *UpstreamFailoverError
	require.False(t, errors.As(err, &failoverErr), "字节已经出去，不存在换号窗口")
	require.Contains(t, rec.Body.String(), "我得先想清楚这里的因果链条。",
		"这就是要治的暴露：上限到点把思考段写给了客户端")
	require.Equal(t, 0, anthropicHoldbackDiscardsUsed(c), "已提交的流不得再走丢弃分支")
	require.Equal(t, 1, cache.deletedSessions[sessionKey],
		"判定照旧成立，但只赶得上给下一发解绑——线上那条 disposition=delivered")
}

// 长正文回合必须照旧放行、绝不能被持流拖成换号。
//
// 判据是单调性：正文越过 anthropicShortTurnProseRuneLimit 之后
// anthropicTurnLooksSuspiciouslyShort 再也不可能判它可疑（那是判可疑的必要条件），所以继续攥
// 不会改变结论，只会白亏首字节延迟。
//
// 2026-08-28 把 longThinkingJudgementPending 的第三个入口放宽到「正文尚未越过上限」之后，
// 这一档的放行出口换了人：正文还在 (0,1300] 区间内时放宽成立、两条总时长线都被抬到放宽下限，
// 于是 AnthropicHoldbackMaxHoldMs 那条定时器压根轮不到开火，真正生效的是 verdict 里
// `proseRunes > anthropicShortTurnProseRuneLimit` 那条提前放行出口。两处共用同一个常量，
// 所以放宽的边界与放行的边界在同一个数上翻转，不存在缝。
//
// 因此这一条钉的是**结果**——长正文回合零 failover、攒下的字节照旧写给客户端——而不是哪条
// 出口先生效。刻意不再断言「200ms 上限到点放行」：那个断言在新口径下描述的是一条不可达路径。
func TestAnthropicPassthrough_HoldbackLongProseReleasesWithoutFailover(t *testing.T) {
	const sessionKey = "holdback-long-prose-releases"
	const groupID = int64(1)
	cache := newShortTurnStreakCache(sessionKey, 9)
	svc, _ := newHoldbackTestGatewayService(t, cache, 5000)
	svc.cfg.Gateway.AnthropicHoldbackMaxHoldMs = 200
	svc.cfg.Gateway.AnthropicHoldbackDeadAirBudgetMs = 0
	svc.cfg.Gateway.AnthropicHoldbackLongThinkingHoldMs = 20000

	// 思考只有 7 rune（远低于 anthropicShortTurnThinkingRuneFloor），正文一路铺过上限。
	events := []string{
		`data: {"type":"message_start","message":{"usage":{"input_tokens":32944,"output_tokens":2}}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"先看一眼日志。"}}`,
		`data: {"type":"content_block_stop","index":0}`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
	}
	// 9 rune × 150 = 1350 rune，越过 anthropicShortTurnProseRuneLimit(1300)。
	for i := 0; i < 150; i++ {
		events = append(events,
			`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"这一段还在往下写。"}}`)
	}
	// 整条流约 155ms，短于 200ms 的总时长上限：这样一来放行只可能来自正文上限那条出口，
	// 不会被定时器抢先，否则这条用例证不出想证的东西。
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       &pacedBody{events: events, gap: time.Millisecond},
	}

	rec, _, err := runHoldbackPassthrough(t, svc, groupID, sessionKey, 9, resp, nil)

	require.Error(t, err, "上游没送终止事件，放行后仍应报告残缺流")
	var failoverErr *UpstreamFailoverError
	require.False(t, errors.As(err, &failoverErr),
		"正文越过上限的回合结构性不可判，必须放行而不是换号")
	require.Contains(t, rec.Body.String(), "这一段还在往下写。",
		"越过上限那一刻就该把攒下的帧写给客户端")
}

// 2026-08-28 18:14:29 那一发的端到端回归（usage_logs id=17451，账号 10，stream=t）：
// prose_runes=849 output_tokens=399 stop_reason=end_turn，first_token_ms=20587、
// duration_ms=21421 —— 判据只比放行晚 834ms 到齐，而 disposition=delivered：字节已经出去，
// 判定虽然照旧判可疑，也只赶得上给下一发解绑，这一发的换号窗口彻底关掉。
// 同形态还有 14:43:23（usage_logs id=16807）：+12091ms 放行、stop_reason 在 +13956ms 到，
// 差 1865ms，当时短回合丢弃额度还剩 1 次没用。
//
// 这一发的思考只有 14 rune，所以两个旧入口都不成立（思考没到 200、正文也不是零），
// AnthropicHoldbackMaxHoldMs 那条 10 秒上限按原值把缓冲放了出去。第三个入口改成
// 「正文尚未越过 anthropicShortTurnProseRuneLimit」之后，849 rune 仍在上限之内、
// 判定仍有可能落在 Discard 上，于是三条线一起抬到下限，判据得以等到 stop_reason。
//
// 终值 output_tokens=399 在闸门（430）之内，所以 stop_reason 一到判定就是可疑 —— 这一发
// 因此可以被零暴露丢弃。持流期看不到这个终值（它与 stop_reason 同帧到达），正是为什么
// 范围只能用正文长度限定、不能用 token 闸门限定。
func TestAnthropicPassthrough_HoldbackShortThinkingLongProseStillJudged(t *testing.T) {
	const sessionKey = "holdback-short-thinking-long-prose"
	const groupID = int64(1)
	cache := newShortTurnStreakCache(sessionKey, 10)
	svc, repo := newHoldbackTestGatewayService(t, cache, 5000)
	// 两条总时长线都远短于整条流，放宽下限远长于它 —— 复现线上「上限先到点」的相对关系。
	svc.cfg.Gateway.AnthropicHoldbackMaxHoldMs = 300
	svc.cfg.Gateway.AnthropicHoldbackDeadAirBudgetMs = 400
	svc.cfg.Gateway.AnthropicHoldbackLongThinkingHoldMs = 20000

	// 思考 14 rune（远低于 anthropicShortTurnThinkingRuneFloor），正文 849 rune
	// （= 283 帧 × 3 rune，落在 (0, anthropicShortTurnProseRuneLimit] 之内）。
	events := []string{
		`data: {"type":"message_start","message":{"usage":{"input_tokens":64512,"output_tokens":2}}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"先看一眼日志。"}}`,
		`data: {"type":"content_block_stop","index":0}`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
	}
	for i := 0; i < 283; i++ {
		events = append(events,
			`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"往下写"}}`)
	}
	events = append(events,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":399}}`,
		`data: {"type":"message_stop"}`)

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       &pacedBody{events: events, gap: 2 * time.Millisecond},
	}

	started := time.Now()
	rec, c, err := runHoldbackPassthrough(t, svc, groupID, sessionKey, 10, resp, nil)
	require.Greater(t, time.Since(started), 400*time.Millisecond,
		"构造前提：整条流必须长于两条总时长线，否则这条用例验不出放宽")

	var failoverErr *UpstreamFailoverError
	require.True(t, errors.As(err, &failoverErr),
		"正文没越过上限时判定必须等到 stop_reason，而不是在 300ms 上限处放行")
	require.Equal(t, GatewayFailureReason("anthropic_short_turn_holdback"), failoverErr.Reason)
	require.True(t, failoverErr.ShouldRetryNextAccount())

	require.Empty(t, rec.Body.String(), "零暴露：判定成形之前一个字节都不能写给客户端")
	require.False(t, c.Writer.Written())
	require.Equal(t, 1, anthropicHoldbackDiscardsUsed(c))
	require.Equal(t, 1, repo.tempCalls, "持流丢弃在解绑时必须冷却账号")
	require.Equal(t, 1, cache.deletedSessions[sessionKey], "丢弃必须同时解绑")
}

// 2026-08-27 23:24:24 的端到端回归（usage_logs id=15219，账号 11 justwoker-anthropic，
// in=226880 out=0 first_token_ms=15361 duration_ms=33445）。
//
// 线上时间轴：首个 thinking_delta 在 5361ms 落地 → 10 秒总时长上限在 15361ms 到点放行 →
// 上游在 33445ms 才 unexpected EOF。放行那一刻 streamCommitted 变真、HTTP 200 钉死，18 秒后
// 的读错误只能走 reportStreamTruncatedAfterCommit：
//
//	ERROR [Anthropic Passthrough] stream truncated after commit, no failover possible:
//	account=11(justwoker-anthropic) model=claude-opus-5
//	reason=stream read error after commit: unexpected EOF
//
// 与 TestAnthropicPassthrough_HoldbackLongThinkingOutlastsTotalHoldCaps 的分工：那一条的思考
// 第一帧就顶过 200 rune，靠的是 longThinkingJudgementPending 的折算入口；这一条的思考全程
// 168 rune，只可能走零正文那个入口。两条用例合起来盖住放宽的两个入口。
func TestAnthropicPassthrough_HoldbackZeroProseThinkingSurvivesReadError(t *testing.T) {
	const sessionKey = "holdback-zero-prose-thinking-read-error"
	const groupID = int64(1)
	cache := newShortTurnStreakCache(sessionKey, 11)
	svc, _ := newHoldbackTestGatewayService(t, cache, 200)
	// 三条线都远短于整条流（14 帧 × 30ms ≈ 420ms），放宽下限远长于整条流。
	svc.cfg.Gateway.AnthropicHoldbackMaxHoldMs = 150
	svc.cfg.Gateway.AnthropicHoldbackDeadAirBudgetMs = 250
	svc.cfg.Gateway.AnthropicHoldbackLongThinkingHoldMs = 20000

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: &pacedThenReadError{
			pacedBody: pacedBody{events: zeroProseThinkingTrickleEvents(), gap: 30 * time.Millisecond},
			err:       io.ErrUnexpectedEOF,
		},
	}

	rec, c, err := runHoldbackPassthrough(t, svc, groupID, sessionKey, 11, resp, nil)

	var failoverErr *UpstreamFailoverError
	require.True(t, errors.As(err, &failoverErr),
		"读错误到达时缓冲必须还在手里，才谈得上换号——线上这里是 no failover possible")
	require.True(t, failoverErr.ShouldRetryNextAccount())
	require.Empty(t, rec.Body.String(), "换号窗口的前提是客户端一个字节都没收到")
	require.False(t, c.Writer.Written())
}

// 同一条流、只把放宽关掉，复现线上 23:24:24 的结局：总时长上限在读错误之前到点放行，
// 字节出去、200 钉死，读错误只能报 truncated after commit。
//
// 这一条是上一条的反证：两条用例的唯一差别就是 AnthropicHoldbackLongThinkingHoldMs，
// 所以能证明零暴露确实来自零正文那个入口，而不是这条流本身就不会被提前放行。
func TestAnthropicPassthrough_HoldbackZeroProseThinkingExposedWhenExtensionDisabled(t *testing.T) {
	const sessionKey = "holdback-zero-prose-thinking-no-extension"
	const groupID = int64(1)
	cache := newShortTurnStreakCache(sessionKey, 11)
	svc, _ := newHoldbackTestGatewayService(t, cache, 200)
	svc.cfg.Gateway.AnthropicHoldbackMaxHoldMs = 150
	svc.cfg.Gateway.AnthropicHoldbackDeadAirBudgetMs = 250
	svc.cfg.Gateway.AnthropicHoldbackLongThinkingHoldMs = 0

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: &pacedThenReadError{
			pacedBody: pacedBody{events: zeroProseThinkingTrickleEvents(), gap: 30 * time.Millisecond},
			err:       io.ErrUnexpectedEOF,
		},
	}

	rec, _, err := runHoldbackPassthrough(t, svc, groupID, sessionKey, 11, resp, nil)

	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.False(t, errors.As(err, &failoverErr), "字节已经出去，不存在换号窗口")
	require.Contains(t, rec.Body.String(), "我得先想清楚这里的因果链条。",
		"这就是要治的暴露：150ms 上限到点把思考段写给了客户端")
}

// 2026-08-28 06:55:07 那一发暴露的洞（usage_logs id=15404，账号 9 100xlabs-anthropic，
// in=79733 out=0 first_token_ms=158286 duration_ms=240799）。
//
// 洞在「零正文」那个入口原本附加的 thinkingRunes > 0 上：能提交响应的帧本身**不一定贡献
// rune**。anthropicSSEPayloadCommitsResponse 对带非空 thinking 的 content_block_start 返回
// true，而 anthropicThinkingRunes 只数 content_block_delta 的 thinking_delta，于是首个提交帧
// 落地那一刻 thinkingRunes 还是 0、proseRunes 也是 0，两个入口双双不成立，三条线全按未放宽的
// 原值走，maxHold 在 firstCommitPointAt+10s 就把缓冲放了出去。text 块、tool_use 块的起始帧
// 同理，redacted_thinking 带 data 时也一样。
//
// 判据只有两件事：stop_reason 未到、正文为零。这一条钉的就是「提交了但一个 rune 都没有」
// 这个中间态必须已经在放宽状态里。
func TestAnthropicHoldbackObserverZeroRuneCommitFrameStillRelaxes(t *testing.T) {
	const window = 15 * time.Second
	const maxHold = 10 * time.Second
	const deadAir = 25 * time.Second
	const floor = 360 * time.Second

	base := time.Date(2026, 8, 27, 22, 55, 7, 0, time.UTC)

	// 三种「提交但零 rune」的起始帧，逐个确认放宽都成立。
	for _, tc := range []struct {
		name  string
		frame string
	}{
		{"thinking块起始帧带非空thinking", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"嗯"}}`},
		{"redacted_thinking块起始帧带data", `{"type":"content_block_start","index":0,"content_block":{"type":"redacted_thinking","data":"AAAA"}}`},
		{"text块起始帧", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := &anthropicHoldbackObserver{longThinkingHoldFloor: floor}
			o.observe(gjson.Parse(`{"type":"message_start","message":{"usage":{"input_tokens":79733,"output_tokens":1}}}`), false, base)

			parsed := gjson.Parse(tc.frame)
			require.True(t, anthropicSSEPayloadCommitsResponse([]byte(tc.frame)),
				"构造前提：这一帧在旧行为下会提交响应")
			o.observe(parsed, true, base)

			require.False(t, o.firstCommitPointAt.IsZero(), "构造前提：提交点已经就位")
			require.Zero(t, o.thinkingRunes, "构造前提：提交帧本身不贡献 thinking rune")
			require.Zero(t, o.proseRunes, "构造前提：正文仍是零")
			require.True(t, o.longThinkingJudgementPending(),
				"提交了但零 rune 时放宽必须成立——这正是 06:55:07 漏掉的中间态")

			// 三条线都被推到下限，原值到点时一条都不该放行。
			require.False(t, o.maxHoldElapsed(base.Add(maxHold), maxHold))
			require.False(t, o.windowElapsed(base.Add(window), window))
			require.False(t, o.deadAirElapsed(base.Add(deadAir), deadAir))
			require.Equal(t, base.Add(floor), o.holdbackMaxHoldDeadline(maxHold))
			require.Equal(t, base.Add(floor), o.holdbackSilenceDeadline(window))

			// 到了下限才放行。
			require.True(t, o.maxHoldElapsed(base.Add(floor), maxHold))
		})
	}

	// 自限性没变：stop_reason 一落地，放宽当帧蒸发。
	done := &anthropicHoldbackObserver{longThinkingHoldFloor: floor}
	done.observe(gjson.Parse(`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"嗯"}}`), true, base)
	require.True(t, done.longThinkingJudgementPending(), "构造前提：先处在放宽状态")
	done.observe(gjson.Parse(`{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`), false, base.Add(time.Second))
	require.False(t, done.longThinkingJudgementPending(), "stop_reason 到了就不该再放宽")
	require.True(t, done.maxHoldElapsed(base.Add(maxHold), maxHold), "蒸发后原值立刻生效")

	// 短正文不蒸发：判定仍有可能落在 Discard 上（正文没越过
	// anthropicShortTurnProseRuneLimit），所以必须继续攥着，否则换号窗口就是在这里丢掉的。
	// 2026-08-28 之前这里断言的是「正文一出现就蒸发」，那正是 18:14:29 那一发
	// （prose_runes=849 output_tokens=399 disposition=delivered）漏掉换号窗口的根因。
	prose := &anthropicHoldbackObserver{longThinkingHoldFloor: floor}
	prose.observe(gjson.Parse(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`), true, base)
	require.True(t, prose.longThinkingJudgementPending(), "构造前提：先处在放宽状态")
	prose.observe(gjson.Parse(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"好的，按这个来。"}}`), true, base.Add(time.Second))
	require.Positive(t, prose.proseRunes)
	require.LessOrEqual(t, prose.proseRunes, anthropicShortTurnProseRuneLimit, "构造前提：正文还在上限之内")
	require.True(t, prose.longThinkingJudgementPending(),
		"正文尚未越过上限时判定仍可能是 Discard，放宽必须继续成立")
	require.False(t, prose.maxHoldElapsed(base.Add(maxHold), maxHold), "原值不得在这一档放行")
	require.True(t, prose.maxHoldElapsed(base.Add(floor), maxHold), "上界仍由下限封住")

	// 正文越过上限才蒸发：那一刻 anthropicTurnLooksSuspiciouslyShort 再也不可能判可疑，
	// verdict 自己那条 `proseRunes > anthropicShortTurnProseRuneLimit` 提前放行出口同帧生效，
	// 两处口径严格对齐，继续攥着纯亏首字延迟。
	long := &anthropicHoldbackObserver{longThinkingHoldFloor: floor}
	long.observe(gjson.Parse(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`), true, base)
	require.True(t, long.longThinkingJudgementPending(), "构造前提：先处在放宽状态")
	long.observe(gjson.Parse(fmt.Sprintf(
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":%q}}`,
		strings.Repeat("长", anthropicShortTurnProseRuneLimit+1))), true, base.Add(time.Second))
	require.Greater(t, long.proseRunes, anthropicShortTurnProseRuneLimit, "构造前提：正文已越过上限")
	require.False(t, long.longThinkingJudgementPending(), "越过上限后不再可判，放宽必须蒸发")
	require.True(t, long.maxHoldElapsed(base.Add(maxHold), maxHold), "蒸发后原值立刻生效")
}
