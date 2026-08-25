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
//   - 客户端死气吃满时也一票否决 Discard，且这一条独立于 windowGone。换号会让新尝试的三条
//     截止线全部归零、客户端在同一个 HTTP 请求上从头再等一轮，这正是 14:33:58 那一发跨
//     3 次尝试攥到 60 秒的成因。
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
		// 死气闸门：下面四条钉住它的语义。
		//
		// 第一条是 14:33:58 那一发的回归——形态完全可疑、额度也还有，但客户端已经吃满预算，
		// 此时换号是纯亏，宁可把疑似截断交出去（截断还有 noteAnthropicShortTurnStreak 给下一发
		// 解绑兜着，断流是整发白丢）。
		{
			name:    "死气吃满：可疑形态也不再丢弃",
			deadAir: true,
			// 与「拿到 stop_reason 且形态可疑：丢弃重试」同一组量，只多了 deadAir。
			stopReason: "end_turn", proseRunes: 131, outputTokens: 70,
			want: anthropicHoldbackRelease,
		},
		{
			name:    "死气吃满：零正文也不再丢弃",
			deadAir: true,
			// 与「零正文的 end_turn：丢弃重试」同一组量。空回合那一档额度更宽，也照样否决。
			stopReason: "end_turn", proseRunes: 0, outputTokens: 1,
			want: anthropicHoldbackRelease,
		},
		// 这一条钉住「死气必须独立传参」：它得在 stop_reason **已到**的分支里生效。若只把它
		// 并进 windowGone，那个参数只在 stop_reason 还没到时才被读到，而 discard 恰恰发生在
		// stop_reason 已到的分支里，闸门会完全失效。
		{
			name:       "死气吃满而窗口没到：仍然否决丢弃",
			deadAir:    true,
			windowGone: false,
			stopReason: "end_turn", proseRunes: 131, outputTokens: 70,
			want: anthropicHoldbackRelease,
		},
		// 反面：死气没吃满时闸门不得改变任何既有判定，尤其不能提前放行还没定案的回合。
		{
			name:    "死气没吃满且判据没齐：照旧攥着",
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
		// 刻意不受死气否决：交付一条块序违规的响应等于把伪造的工具输出交给客户端，客户端会
		// 当真去用，代价高于多等一次尝试。这一条与上面「死气吃满：可疑形态也不再丢弃」构成对照。
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
// 死气那条不受这条约束——它锚在客户端请求起点上，从第一次尝试就已经在走，见
// TestAnthropicHoldbackObserverDeadAirSpansFailoverAttempts。这里传 0 把它关掉，单独考察
// 前两条。
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
	o := &anthropicHoldbackObserver{streamStartedAt: base.Add(-5 * time.Second)}
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

	// 死气那条从 streamStartedAt 起算，已经先走了 5 秒：25s 预算折算到 base 上只剩 20s，
	// 比 15s 窗口远、比不上上限；而 12s 预算折算后只剩 7s，比 10s 上限还近，该由它当先到的。
	require.Equal(t, base.Add(20*time.Second), o.holdbackDeadAirDeadline(25*time.Second),
		"死气截止时刻锚在客户端请求起点上，不是本次尝试起点")
	require.Equal(t, base.Add(7*time.Second),
		o.holdbackReleaseDeadline(15*time.Second, 10*time.Second, 12*time.Second),
		"死气预算折算后最近时由它当先到的")
	require.True(t, o.holdbackDeadAirDeadline(0).IsZero(), "死气配 0 时没有截止时刻")

	// 帧持续到达会把静默那条不断推后，上限和死气两条都不动——这正是三条并存的意义。
	later := base.Add(20 * time.Second)
	o.observe(gjson.Parse(`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"再想一层"}}`), true, later)
	require.Equal(t, later.Add(15*time.Second), o.holdbackSilenceDeadline(15*time.Second),
		"静默那条被新帧推后了")
	require.Equal(t, base.Add(10*time.Second),
		o.holdbackReleaseDeadline(15*time.Second, 10*time.Second, 0),
		"上限那条不受新帧影响，仍然是先到的那个")
	require.Equal(t, base.Add(20*time.Second), o.holdbackDeadAirDeadline(25*time.Second),
		"死气那条同样不受新帧影响")
}

// 2026-08-25 14:26:26 / 14:33:58 两发的回归用例：死气必须跨 failover 尝试累加。
//
// 前两条截止线都锚在**本次上游尝试**上（静默锚最后一帧、上限锚 firstCommitPointAt），一次
// discard 换号之后新尝试的 observer 是全新的，两条连同 10 秒上限一起归零，客户端却还挂在
// 同一个 HTTP 请求上继续干等。14:26:26 那一发跨 2 次尝试攥到 30.8 秒，14:33:58 跨 3 次约
// 60 秒——两者的 duration_ms 只记到 19.4s / 23.6s，因为它也是每次尝试重取的。
//
// 死气这条锚在 streamStartedAt 上，由 anthropicClientRequestStartedAt 从 gin.Context 取，
// 跨尝试是同一个值，所以它是三条里唯一不会被换号重置的。
func TestAnthropicHoldbackObserverDeadAirSpansFailoverAttempts(t *testing.T) {
	const budget = 25 * time.Second
	requestStart := time.Date(2026, 8, 25, 14, 25, 55, 0, time.UTC)

	// 第一次尝试：账号 9，11.6 秒后判定可疑并 discard 换号（实测 14:26:07.529）。
	first := &anthropicHoldbackObserver{streamStartedAt: requestStart}
	discardAt := requestStart.Add(11*time.Second + 600*time.Millisecond)
	first.observe(gjson.Parse(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"我看一下。"}}`), true, discardAt)
	require.False(t, first.deadAirElapsed(discardAt, budget), "11.6 秒还没吃满预算，此时换号是对的")

	// 第二次尝试：换到账号 5，observer 全新，但 streamStartedAt 仍是同一个客户端请求起点。
	second := &anthropicHoldbackObserver{streamStartedAt: requestStart}
	secondFrame := discardAt.Add(500 * time.Millisecond)
	second.observe(gjson.Parse(`{"type":"message_start","message":{"usage":{"input_tokens":129000}}}`), false, secondFrame)

	// 第二次尝试只送出不提交响应的 message_start 后静默，前两条线都没起算；唯独锚在
	// 客户端请求起点的死气预算会跨尝试继续走，并最终负责放行。
	release := requestStart.Add(30*time.Second + 760*time.Millisecond) // 实测客户端 30.76 秒零字节
	require.False(t, second.windowElapsed(release, 15*time.Second), "没有提交点时静默线不参与")
	require.False(t, second.maxHoldElapsed(release, 10*time.Second), "没有提交点时总时长线不参与")
	require.True(t, second.deadAirElapsed(release, budget), "死气跨尝试累加，此刻必须已经吃满")
	require.True(t, second.releaseDeadlineElapsed(release, 15*time.Second, 10*time.Second, budget),
		"三条任意一条到点就该放行——这一刻只有死气那条在说话")

	// 预算耗尽的那一刻正好是边界，前一毫秒不算。
	require.False(t, second.deadAirElapsed(requestStart.Add(budget-time.Millisecond), budget))
	require.True(t, second.deadAirElapsed(requestStart.Add(budget), budget))

	// 配 0 关掉；没有锚点（单测直接构造 observer 不播种）时同样不参与，避免零值时间被当成
	// 「1970 年就起算了」而恒定判为吃满。
	require.False(t, second.deadAirElapsed(requestStart.Add(time.Hour), 0), "配 0 时永不吃满")
	bare := &anthropicHoldbackObserver{}
	require.False(t, bare.deadAirElapsed(requestStart.Add(time.Hour), budget), "没有锚点时不参与判定")
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
	svc, _ := newHoldbackTestGatewayService(t, cache, 3000)

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
