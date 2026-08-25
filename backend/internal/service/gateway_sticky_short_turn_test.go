package service

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
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
			// sseBody 用单个换行符连接各元素，而 SSE 的事件终止符是一个空行（连续两个换行）。
			// 所以最后一个事件必须跟两个空元素：常规链路 handleStreamingResponse
			// 是按空行切事件的，少一个 message_stop 就永远不会被处理，
			// 判定拿不到 stop_reason。透传链路是逐行读的，所以不受影响。
			"",
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
		proseRunes   int
		outputTokens int
		sawToolUse   bool
		suspicious   bool
		thinking     int
	}{
		// 线上实测的故障：end_turn + 几十个 output_token + 一两句话就收尾。
		{"实测故障形态 output_tokens=30", "end_turn", 60, 30, false, true, 0},
		{"实测故障形态 output_tokens=19", "end_turn", 41, 19, false, true, 0},
		// 2026-08-23 22:31:21 账号 9 那一发：131 个汉字（≈289 字节）+ out=70。
		// 旧实现按字节数，289>200 既漏判、又被当成正面证据清零了连击，是这次改动的锚点。
		{"22:31:21 中文截断 131 字/out=70", "end_turn", 131, 70, false, true, 0},
		{"同批漏判样本 183 字/out=90", "end_turn", 183, 90, false, true, 0},
		{"同批漏判样本 150 字/out=73", "end_turn", 150, 73, false, true, 0},
		{"大小写与空格不影响判定", "  End_Turn  ", 60, 30, false, true, 0},
		{"刚好卡在正文上限仍算可疑", "end_turn", anthropicShortTurnProseRuneLimit, 100, false, true, 0},
		{"刚好卡在 token 上限仍算可疑", "end_turn", 100, anthropicShortTurnOutputTokenLimit, false, true, 0},

		// 两条上限各自都要能独立否掉。
		{"超过正文上限不算可疑", "end_turn", anthropicShortTurnProseRuneLimit + 1, 100, false, false, 0},
		{"超过 token 上限不算可疑", "end_turn", 100, anthropicShortTurnOutputTokenLimit + 1, false, false, 0},
		// out=133 曾是「正常短回答」的下界锚点（旧标定：截断全 ≤112、正常短回答全 ≥133）。
		// 那条缝是假的：重新打标的 150 发样本里 out=131/142/143/148/156 配正文 252~394 rune
		// 的 5 发确确实实是截断，与 133 这一带完全重叠，token 数分不开两类。闸门抬到 320
		// 之后这一档归可疑，是拿误报换漏判的刻意选择——误报代价被丢弃额度封顶（用满即放行），
		// 漏判代价是用户再吃一发断流。
		{"out=133 现在归可疑带", "end_turn", 120, 133, false, true, 0},

		// 其余 stop_reason 都自带「模型确实干了活 / 被限制截断」的语义。
		{"tool_use 不算", "tool_use", 20, 10, false, false, 0},
		{"max_tokens 不算", "max_tokens", 20, 10, false, false, 0},
		{"stop_sequence 不算", "stop_sequence", 20, 10, false, false, 0},
		{"pause_turn 不算", "pause_turn", 20, 10, false, false, 0},
		{"异常 stop_reason 归确定性判定管", "upstream_error", 20, 10, false, false, 0},
		{"空 stop_reason 不算", "", 20, 10, false, false, 0},

		// 开了工具块的短回合是标准 agent 行为，绝不能解绑——否则每次工具调用都在打断粘性。
		{"开了 tool_use 块不算", "end_turn", 20, 10, true, false, 0},

		// output_tokens<=0 **且零正文**才归 anthropicStreamLooksIncompleteDespiteTerminal，
		// 在这里放行，避免同一次故障被两套逻辑各记一次。
		{"output_tokens 为零且零正文不算", "end_turn", 0, 0, false, false, 0},
		{"output_tokens 为负且零正文不算", "end_turn", 0, -1, false, false, 0},

		// 但 output_tokens<=0 **配着有正文**是另一回事：上游把 usage 报成 0 而字节确实出去了，
		// 残缺判定要求 visibleChars==0 所以不认，旧代码这里又直接放行，于是零信号零记账。
		// 2026-08-24 17:02:33 实证（usage_logs id=9544，账号 10）：output_tokens=0、
		// first_token_ms=10808 之后还跑了 2680ms，字节和内容块都出去了。
		{"usage 报 0 但有正文仍算可疑", "end_turn", 20, 0, false, true, 0},
		{"usage 报负数但有正文仍算可疑", "end_turn", 20, -1, false, true, 0},

		// 零正文 + output_tokens>0：账号 12（sotamodel）的实测形态，也是两套判定之间那条缝。
		// anthropicStreamLooksIncompleteDespiteTerminal 要求 output_tokens<=0（实际拿到 1），
		// 这里原先要求 proseRunes>0（实际拿到 0），于是三个检测器全部放行、持流判成 Release，
		// 一个没有答案的 200 被原样交付，客户端表现为断流后自己重发，而账号既没被排除也没被
		// 冷却，粘性把下一发原样送回去——正是「断流且不会自动切号」。
		//
		// 48h 统计（output_tokens=1 占该账号流式请求的比例）：账号 12 = 35.9%（51/142），
		// 账号 5/9 = 0.1%，账号 10/11 = 0%。健康账号几乎不产生这个形态，所以它是确定性的
		// 上游故障，判可疑不会误伤。
		{"零正文 out=1 是空回合", "end_turn", 0, 1, false, true, 0},
		{"零正文 out=30 是空回合", "end_turn", 0, 30, false, true, 0},
		// 空回合刻意**不**受 output_tokens 上限约束：思考 token 也计入 output_tokens，
		// 「想了很久一句没说」正是最典型的截断形态，卡上限反而会把它漏掉。
		{"零正文即使 token 上量仍算空回合", "end_turn", 0, anthropicShortTurnOutputTokenLimit + 1, false, true, 0},
		{"零正文即使 token 很多仍算空回合", "end_turn", 0, 4096, false, true, 0},
		// 其余闸门对空回合一视同仁：工具回合与非 end_turn 收尾都不算。
		{"零正文 + tool_use 块不算", "end_turn", 0, 1, true, false, 0},
		{"零正文但 max_tokens 收尾不算", "max_tokens", 0, 1, false, false, 0},

		// 想很久却只吐几个字：思考 token 计入 output_tokens，把它撑到 128 那道闸门之上，
		// 于是这一形态在加思考判据之前全程免检——不是窗口不够，是压根没被判为可疑。
		// 2026-08-24 实证（usage_logs id=9514 out=577 / id=9523 out=445，账号 10）：
		// 首字之后还跑了 6 秒多，客户端只收到 "d." 两个字符。
		{"想很久只吐 2 字算可疑", "end_turn", 2, 577, false, true, 900},
		{"同批样本 out=445 同形态", "end_turn", 2, 445, false, true, 900},
		{"正文刚好卡在思考后上限仍算可疑", "end_turn", anthropicPostThinkingProseRuneCeiling, 577, false, true, 900},
		{"思考刚好够长仍算可疑", "end_turn", 2, 577, false, true, anthropicShortTurnThinkingRuneFloor},
		// 两侧边界各自都要能独立否掉，否则这条判据会开始吃正常回合。
		{"思考不够长时仍由 token 闸门说话", "end_turn", 2, 577, false, false, anthropicShortTurnThinkingRuneFloor - 1},
		// 正文过了思考后上限，旁路关闭，但常规判据会用折算过的 token 数接手：
		// 577 里 900/941 归思考，正文只剩 ~25 token（13 个汉字），仍然不可能是真答案。
		// 这一行以前期望「放行」，那是折算之前只有 40 rune 旁路一个判据时的盲区，
		// 不是想守住的性质——19:46:49 那一发（正文 114 rune）就是从这个盲区漏过去的。
		{"正文过了思考后上限但折算后仍可疑", "end_turn", anthropicPostThinkingProseRuneCeiling + 1, 577, false, true, 900},
		// 19:46:49 实证形态：思考 454 / 正文 114 / out 286，宣布下一步就收尾。
		// 286 > 128 让原值闸门免检，折算后 57 才判得住。
		{"想很久+正文上百 rune 仍算可疑", "end_turn", 114, 286, false, true, 454},
		// 折算不能吃掉真正的成段回答。闸门 430 时，thinking 900/out 1000 要正文过
		// ~682 rune 折算值才够 430，所以放行的门槛落在 700 rune 这一带（折算 437）。
		// 这才是「思考很长但正文成段是真答案」想守的性质；正文退到 300 rune（折算 250）
		// 归可疑带，与 anthropicTurnProvesUpstreamHealthy 那侧同口径，两者互否。
		//
		// 闸门 320 时这两行写的是 500（折算 357）/300，抬到 430 之后 500 落进可疑带，
		// 是刻意的：这一带就是 22:20:13 那一类要收进来的形态。
		{"思考很长且正文成段是真答案", "end_turn", 700, 1000, false, false, 900},
		{"思考很长但正文只到 500 rune 仍可疑", "end_turn", 500, 1000, false, true, 900},
		{"思考很长但正文只到 300 rune 仍可疑", "end_turn", 300, 1000, false, true, 900},
		{"思考很长正文刚过健康下限仍可疑", "end_turn", 260, 800, false, true, 900},
		// 思考判据不该松掉别的闸门：tool_use 和非 end_turn 收尾一律照旧放行。
		{"想很久+tool_use 块仍不算", "end_turn", 2, 577, true, false, 900},
		{"想很久但 max_tokens 收尾不算", "max_tokens", 2, 577, false, false, 900},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.suspicious, anthropicTurnLooksSuspiciouslyShort(
				tc.stopReason, tc.proseRunes, tc.outputTokens, tc.sawToolUse, tc.thinking))
		})
	}
}

// anthropicTurnIsEmptyAnswer 把「一个字都没说」从「说得少」里切出来。这条分界决定要不要
// 罚号，所以边界必须单独钉住：
//   - 说得少可能是真的只有一句话的答案，罚号会把一个好账号从所有会话的调度池里摘掉；
//   - 一个字都没说不存在合法解释——end_turn 声明「我说完了」，却没有任何 text_delta。
//
// 它必须是 anthropicTurnLooksSuspiciouslyShort 的**子集**：否则调用点会出现「罚了号但没
// 解绑」的组合，粘性还钉在坏号上。
func TestAnthropicTurnIsEmptyAnswer(t *testing.T) {
	for _, tc := range []struct {
		name         string
		stopReason   string
		proseRunes   int
		outputTokens int
		toolUse      bool
		want         bool
	}{
		// 账号 12 的实测形态。
		{"零正文 out=1", "end_turn", 0, 1, false, true},
		{"零正文 out=30", "end_turn", 0, 30, false, true},
		// 不受 token 上限约束：长思考 + 零正文照样是空回合。
		{"零正文 token 上量", "end_turn", 0, anthropicShortTurnOutputTokenLimit + 1, false, true},

		// 有正文就不是空回合，哪怕短到会被解绑。这条守的就是「短≠空」，
		// 少了它罚号会扩散到所有短回答上。
		{"一个字也算有正文", "end_turn", 1, 30, false, false},
		{"实测短截断有正文，不算空", "end_turn", 131, 70, false, false},

		// 闸门与短回合判定共用，逐条确认没有旁路。
		{"tool_use 块不算", "end_turn", 0, 1, true, false},
		{"max_tokens 不算", "max_tokens", 0, 1, false, false},
		{"tool_use 收尾不算", "tool_use", 0, 1, false, false},
		{"空 stop_reason 不算", "", 0, 1, false, false},
		// output_tokens<=0 归确定性残缺判定，不在这里重复记一次。
		{"output_tokens 为零不算", "end_turn", 0, 0, false, false},
		{"output_tokens 为负不算", "end_turn", 0, -1, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := anthropicTurnIsEmptyAnswer(tc.stopReason, tc.proseRunes, tc.outputTokens, tc.toolUse)
			require.Equal(t, tc.want, got)
			if got {
				require.True(t, anthropicTurnLooksSuspiciouslyShort(
					tc.stopReason, tc.proseRunes, tc.outputTokens, tc.toolUse, 0),
					"空回合必须同时是可疑短回合，否则会出现罚了号却不解绑的组合")
			}
		})
	}
}

// 穷举确认子集关系：空回合永远蕴含可疑短回合。调用点靠这个不变式把「解绑」和「罚号」
// 叠在同一个分支里，一旦被破坏就会静默出现罚号但不解绑。
func TestEmptyAnswerImpliesSuspiciouslyShort(t *testing.T) {
	for _, sr := range []string{"end_turn", "max_tokens", "stop_sequence", "tool_use", "pause_turn", "refusal", ""} {
		for _, runes := range []int{0, 1, 50, anthropicShortTurnProseRuneLimit, anthropicShortTurnProseRuneLimit + 1} {
			for _, tokens := range []int{-1, 0, 1, 70, anthropicShortTurnOutputTokenLimit, anthropicShortTurnOutputTokenLimit + 1, 4096} {
				for _, toolUse := range []bool{false, true} {
					if anthropicTurnIsEmptyAnswer(sr, runes, tokens, toolUse) {
						require.True(t, anthropicTurnLooksSuspiciouslyShort(sr, runes, tokens, toolUse, 0),
							"stop_reason=%q prose=%d tokens=%d toolUse=%v 是空回合却不可疑", sr, runes, tokens, toolUse)
					}
				}
			}
		}
	}
}

// emptyAnswerSSE 复刻账号 12 的实测响应：协议层完整（message_delta + message_stop、
// stop_reason=end_turn），开了正文块，却一个 text_delta 都没有，output_tokens 只有 1。
//
// 与 shortTurnSSE 的唯一区别就是没有 text_delta —— 这一个差别决定了三个检测器全部放行。
func emptyAnswerSSE(outputTokens int) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: sseBody(
			`data: {"type":"message_start","message":{"usage":{"input_tokens":129000}}}`,
			"",
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			"",
			`data: {"type":"content_block_stop","index":0}`,
			"",
			fmt.Sprintf(`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":%d}}`, outputTokens),
			"",
			`data: {"type":"message_stop"}`,
			"",
			"",
		),
	}
}

// 正文只按 rune 数、且只数 text_delta。这两条是 2026-08-23 22:31:21 那次漏判的直接根因，
// 各自单独钉住：
//   - 单位错（len() 数字节）会让「200 上限」对中文只剩 66 字；
//   - 口径错（把思考链算成正文）会让「想很久只吐一句」这种最典型的截断被当成健康回合。
func TestAnthropicVisibleProseRunes(t *testing.T) {
	runes := func(payload string) int {
		return anthropicVisibleProseRunes(gjson.Parse(payload))
	}

	// 20 个汉字 = 60 字节。线上日志里 visible_chars=54 对应的是 18 个汉字，54=18×3，
	// 就是这个单位差。
	const cn = "临时文件都已清理删除，现在只剩部署目录。"
	require.Equal(t, 20, runes(fmt.Sprintf(
		`{"type":"content_block_delta","delta":{"type":"text_delta","text":%q}}`, cn)))
	require.Equal(t, 60, len(cn), "前提：中文一字三字节，字节数是 rune 数的三倍")

	// 思考链和工具入参都不算正文：模型可以想很久然后只吐一句就收尾，那正是要抓的形态。
	require.Zero(t, runes(
		`{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"想很久很久很久"}}`))
	require.Zero(t, runes(
		`{"type":"content_block_delta","delta":{"type":"input_json_delta","partial_json":"{\"a\":1}"}}`))
	require.Zero(t, runes(
		`{"type":"content_block_delta","delta":{"type":"signature_delta","signature":"abc"}}`))
	require.Zero(t, runes(`{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`))

	// anthropicVisibleDeltaChars 必须保持原样：它唯一的用途是会罚号的残缺判定里的判零，
	// 这次改动不得改变那条路径的行为。
	require.Equal(t, len(cn), anthropicVisibleDeltaChars(gjson.Parse(fmt.Sprintf(
		`{"type":"content_block_delta","delta":{"type":"text_delta","text":%q}}`, cn))),
		"字节口径的旧函数不得被改动")
	require.Positive(t, anthropicVisibleDeltaChars(gjson.Parse(
		`{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"想"}}`)),
		"旧函数仍应把思考链计入，判零口径不变")
}

// 端到端：两条信号都放行的「协议合法但疑似没把话说完」，连续两发后必须解除粘性绑定，
// 让客户端的下一发重新选号落到同优先级的另一个账号上。
//
// requireUnboundOnce 断言阈值 1 下「一发可疑短回合就解绑」这一整套后果。
//
// 为什么不再像旧版那样先断言 streaks==1 再跑第二发：阈值 1 下第一发就达标，解绑路径会
// 顺手把连击清零，所以 streaks 里根本不会留下痕迹。各条用例的诊断价值不受影响——它们靠
// 的是「这一发到底判没判成可疑」，而 deletedSessions 就是那个信号：如果这一发被误当成
// 正面证据（旧实现按字节数数中文正是如此），清零之后不会有任何解绑。
func requireUnboundOnce(t *testing.T, cache *shortTurnStreakCache, groupID int64, sessionKey string) {
	t.Helper()
	key := shortTurnStreakKey(groupID, sessionKey)
	require.Equal(t, 1, cache.deletedSessions[sessionKey], "阈值 1：一发可疑短回合就必须解除粘性绑定")
	require.NotContains(t, cache.sessionBindings, sessionKey)
	require.NotContains(t, cache.streaks, key, "解绑后必须清零连击数，否则会来回解绑")
	require.Equal(t, 1, cache.resets[key], "解绑必须伴随一次清零")
}

// 这就是修复的核心：stop_reason=end_turn 在白名单里（信号 1 放行）、output_tokens=30>0
// （信号 2 要求 <=0，放行），旧代码判为健康 → 不罚号 → 粘性把下一发原样送回同一个账号，
// 表现为「一直断流，自动切回可用账号没起作用」。
func TestAnthropicPassthrough_ShortTurnStreakUnbindsStickySession(t *testing.T) {
	const sessionKey = "sticky-short-turn"
	const groupID = int64(1)
	cache := newShortTurnStreakCache(sessionKey, 9)
	svc, repo := newShortTurnTestGatewayService(t, cache)

	runShortTurnPassthrough(t, svc, groupID, sessionKey, 9, shortTurnSSE("好的，我来看一下这个问题。", 30, false))
	requireUnboundOnce(t, cache, groupID, sessionKey)

	// 解绑那一刻必须补一次冷却：调度按 priority 升序取号，坏号若正好是本组最小的那个，
	// 光删绑定下一发照旧选回它——线上实测到过解绑后紧接着又回同一个号。见
	// reportAnthropicShortTurnUnbind。未达阈值的观测仍然零惩罚，由下面那条用例守。
	require.Equal(t, 1, repo.tempCalls, "解绑那一刻必须冷却账号，否则解绑对首选号是空操作")

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
	key := shortTurnStreakKey(groupID, sessionKey)

	// 正常成段回答：主动清零，且绝不解绑。
	long := strings.Repeat("正常长度的回答内容。", 40)
	runShortTurnPassthrough(t, svc, groupID, sessionKey, 9, shortTurnSSE(long, 800, false))
	require.Equal(t, 1, cache.resets[key], "正常回合必须主动清零连击数")
	require.Zero(t, cache.deletedSessions[sessionKey], "正常回合不得解绑")
	require.Equal(t, int64(9), cache.sessionBindings[sessionKey])

	// 再来几发正常回合：一直清零，一直不解绑。
	for i := 0; i < 3; i++ {
		runShortTurnPassthrough(t, svc, groupID, sessionKey, 9, shortTurnSSE(long, 800, false))
	}
	require.Zero(t, cache.deletedSessions[sessionKey], "连续正常回合不得解绑")
	require.Empty(t, cache.streaks, "正常回合之后不得留下连击数")
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

// runShortTurnRegularPath 跑一次常规（非透传）链路的流式转发，带粘性会话坐标。
//
// 这条链路是 handleStreamingResponse，第三方中转账号（不开 anthropic_passthrough 的
// apikey 号，如 upstream.example.invalid）走的就是它。与透传分支用同一组判定函数，但原先
// 完全没有采集判定所需的输入，所以短回合解绑对这条路是死代码。
func runShortTurnRegularPath(
	t *testing.T, svc *GatewayService, groupID int64, sessionKey string,
	accountID int64, resp *http.Response,
) {
	t.Helper()
	c, _ := newRefusalTestContext(t)
	ctx := WithStickySessionScope(context.Background(), groupID, sessionKey, false)
	_, err := svc.handleStreamingResponse(
		ctx, resp, c, &Account{ID: accountID, Name: "relay", Platform: PlatformAnthropic},
		time.Now(), "claude-opus-5", "claude-opus-5", false)
	require.NoError(t, err)
}

// 常规链路的回归锚点：这正是 2026-08-23 19:55:48 账号 9 那次断流的复现。
//
// 那次故障的形状与透传分支已修的完全一致（end_turn + output_tokens=30 + 一句开场白），
// 但账号 9 是第三方中转号、不走透传，于是请求进的是 handleStreamingResponse。该函数
// 收尾只判 sawTerminalEvent（有没有 message_stop），根本不看 stop_reason 和可见字符量，
// 所以 0.1.189 上线的短回合解绑对它一次都没生效：不解绑 → 粘性把下一发原样送回账号 9
// → 用户看到「断流依旧出在账号 9 上」。
func TestRegularPath_ShortTurnStreakUnbindsStickySession(t *testing.T) {
	const sessionKey = "regular-short-turn"
	const groupID = int64(1)
	cache := newShortTurnStreakCache(sessionKey, 9)
	svc, repo := newShortTurnTestGatewayService(t, cache)

	runShortTurnRegularPath(t, svc, groupID, sessionKey, 9, shortTurnSSE("好的，我来看一下这个问题。", 30, false))
	requireUnboundOnce(t, cache, groupID, sessionKey)

	// 与透传分支同口径：解绑那一刻补一次冷却，见 reportAnthropicShortTurnUnbind。
	require.Equal(t, 1, repo.tempCalls, "解绑那一刻必须冷却账号")
}

// 常规链路上的正常回合同样要清零连击数。
func TestRegularPath_NormalTurnResetsShortTurnStreak(t *testing.T) {
	const sessionKey = "regular-reset"
	const groupID = int64(1)
	cache := newShortTurnStreakCache(sessionKey, 9)
	svc, _ := newShortTurnTestGatewayService(t, cache)
	key := shortTurnStreakKey(groupID, sessionKey)

	long := strings.Repeat("正常长度的回答内容。", 40)
	runShortTurnRegularPath(t, svc, groupID, sessionKey, 9, shortTurnSSE(long, 800, false))
	require.Equal(t, 1, cache.resets[key], "正常回合必须主动清零")
	require.Empty(t, cache.streaks, "正常回合之后不得留下连击数")
	require.Zero(t, cache.deletedSessions[sessionKey], "正常回合不得解绑")
	require.Equal(t, int64(9), cache.sessionBindings[sessionKey])
}

// 常规链路上开了 tool_use 块的短回合不得累计——agent 的每次工具调用都是这个形状。
func TestRegularPath_ToolUseShortTurnNotCounted(t *testing.T) {
	const sessionKey = "regular-tool-use"
	const groupID = int64(1)
	cache := newShortTurnStreakCache(sessionKey, 9)
	svc, _ := newShortTurnTestGatewayService(t, cache)

	for i := 0; i < 3; i++ {
		runShortTurnRegularPath(t, svc, groupID, sessionKey, 9, shortTurnSSE("我来查一下", 30, true))
	}
	require.Empty(t, cache.streaks, "工具调用回合不得累计连击")
	require.Zero(t, cache.deletedSessions[sessionKey])
	require.Equal(t, int64(9), cache.sessionBindings[sessionKey])
}

// 常规链路的确定性残缺判定也必须接上：开了正文块却零可见字符且 output_tokens<=0
// 是无歧义的截断，这条要罚号（与只解绑的可疑短回合分开）。
func TestRegularPath_TerminalButEmptyContentIsPenalized(t *testing.T) {
	const sessionKey = "regular-empty"
	const groupID = int64(1)
	cache := newShortTurnStreakCache(sessionKey, 9)
	svc, repo := newShortTurnTestGatewayService(t, cache)

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: sseBody(
			`data: {"type":"message_start","message":{"usage":{"input_tokens":129000}}}`,
			"",
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			"",
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":0}}`,
			"",
			`data: {"type":"message_stop"}`,
			"",
			"",
		),
	}
	runShortTurnRegularPath(t, svc, groupID, sessionKey, 9, resp)

	require.Positive(t, repo.tempCalls, "开了块却零输出是确定性残缺，必须罚号")
	require.Empty(t, cache.streaks, "确定性残缺不走连击路径，避免同一次故障记两次")
}

// turnSSEWithStopReason 与 shortTurnSSE 同形，但可以指定 stop_reason，用来复刻线上
// 真实的 tool_use 中间回合（那种回合的 stop_reason 是 tool_use，不是 end_turn）。
func turnSSEWithStopReason(text string, outputTokens int, stopReason string, toolUse bool) *http.Response {
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
			fmt.Sprintf(`data: {"type":"message_delta","delta":{"stop_reason":%q},"usage":{"output_tokens":%d}}`, stopReason, outputTokens),
			"",
			`data: {"type":"message_stop"}`,
			"",
			"",
		),
	}
}

// anthropicTurnProvesUpstreamHealthy 的边界。这是唯一决定「哪种回合能清零连击」的地方。
// 放宽（比如让 tool_use 回合也算正面证据）会让解绑重新变成死代码——那正是账号 9 在
// 0.1.190 上依然断流的原因。
//
// 清零要两个条件同时成立（正文成段 + token 上量），是因为 22:31:21 那一发就是只靠
// 「字节数够多」（131 个汉字 ≈289 字节 > 200）就把连击清零的。少任何一条都会重演。
//
// token 那一侧比的是 anthropicVisibleOutputTokens 折算值、且与可疑判据共用
// anthropicShortTurnOutputTokenLimit，这是互斥的构造条件，见下面那个穷举用例。
func TestAnthropicTurnProvesUpstreamHealthy(t *testing.T) {
	const longProse = anthropicHealthyTurnMinProseRunes + 1
	const manyTokens = anthropicShortTurnOutputTokenLimit + 1
	for _, tc := range []struct {
		name       string
		stopReason string
		proseRunes int
		tokens     int
		toolUse    bool
		thinking   int
		want       bool
	}{
		{"成段正文的 end_turn 是正面证据", "end_turn", longProse, manyTokens, false, 0, true},
		{"被 max_tokens 截断但产出成段正文，也是正面证据", "max_tokens", longProse, manyTokens, false, 0, true},
		{"stop_sequence 收尾且正文成段", "stop_sequence", longProse, manyTokens, false, 0, true},

		{"短回合不是正面证据", "end_turn", 10, 20, false, 0, false},
		{"刚好等于正文下限仍不清零", "end_turn", anthropicHealthyTurnMinProseRunes, manyTokens, false, 0, false},
		{"刚好等于 token 上限仍不清零", "end_turn", longProse, anthropicShortTurnOutputTokenLimit, false, 0, false},
		// 22:31:21 的回归锚点：131 字放到旧的字节口径里是 289，会被判成正面证据。
		{"22:31:21 中文截断绝不能清零连击", "end_turn", 131, 70, false, 0, false},
		// 中间带（正文超过清零下限、但 token 数还在短回合上限内）一律不表态。
		{"正文够长但 token 数偏低不表态", "end_turn", longProse, 100, false, 0, false},
		// out=133 在 token 闸门抬到 320 之前是「正常短回答」的锚点，现在落进可疑带，
		// 于是这一侧必须同步变成不清零——否则两个判定同时为真。是刻意的：见
		// anthropicShortTurnOutputTokenLimit 上那段网格结论（漏判 27→12，误报恒为 4）。
		{"out=133/正文 210 字现在归可疑带，不再清零", "end_turn", 210, 133, false, 0, false},

		// 折算对这一侧同样生效。thinking 900 时正文要过 ~682 rune 才够 430，
		// 这是「思考很长但确实写了成段正文」的正面证据形态。
		{"思考很长但正文成段仍是正面证据", "end_turn", 700, 1000, false, 900, true},
		// 同一组 out/thinking，正文退到 500/300 都落进可疑带（折算 357/250），不能清零。
		{"思考很长而正文只到 500 rune 不清零", "end_turn", 500, 1000, false, 900, false},
		{"思考很长而正文只到 300 rune 不清零", "end_turn", 300, 1000, false, 900, false},

		{"tool_use 回合一律不表态，正文再长也不清零", "end_turn", longProse, manyTokens, true, 0, false},
		{"stop_reason 为 tool_use 的中间回合不表态", "tool_use", longProse, manyTokens, false, 0, false},
		{"pause_turn 是协议级续传，不表态", "pause_turn", longProse, manyTokens, false, 0, false},
		{"没有 stop_reason 不表态", "", longProse, manyTokens, false, 0, false},
		{"异常 stop_reason 不表态", "refusal", longProse, manyTokens, false, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, anthropicTurnProvesUpstreamHealthy(
				tc.stopReason, tc.proseRunes, tc.tokens, tc.toolUse, tc.thinking))
		})
	}
}

// 三态判定不得有重叠：同一个回合不可能既可疑又是正面证据，否则调用点的 if/else-if
// 顺序就成了隐式的优先级，改动顺序会静默改变行为。
//
// 必须连 thinkingRunes 一起枚举。2026-08-24 可疑侧改成比 anthropicVisibleOutputTokens
// 折算值时，清零侧还在比 output_tokens 原值，互斥当场就破了（prose 300/thinking 900/
// out 1000 折算 250 判可疑、原值 1000 判健康，两边同真），而这个用例当时把 thinking 恒定
// 传 0，折算在无思考回合上恒等于原值，于是全程绿灯。
func TestShortTurnPredicatesAreMutuallyExclusive(t *testing.T) {
	stopReasons := []string{"end_turn", "max_tokens", "stop_sequence", "tool_use", "pause_turn", "refusal", ""}
	runeCounts := []int{0, 1, 50,
		anthropicPostThinkingProseRuneCeiling, anthropicPostThinkingProseRuneCeiling + 1,
		anthropicHealthyTurnMinProseRunes, anthropicHealthyTurnMinProseRunes + 1,
		300, 500,
		anthropicShortTurnProseRuneLimit, anthropicShortTurnProseRuneLimit + 1, 5000}
	tokenCounts := []int{-1, 0, 1, 70, 133,
		anthropicShortTurnOutputTokenLimit, anthropicShortTurnOutputTokenLimit + 1, 1000, 4096}
	thinkingCounts := []int{0, 1,
		anthropicShortTurnThinkingRuneFloor - 1, anthropicShortTurnThinkingRuneFloor,
		900, 5739}

	for _, sr := range stopReasons {
		for _, runes := range runeCounts {
			for _, tokens := range tokenCounts {
				for _, thinking := range thinkingCounts {
					for _, toolUse := range []bool{false, true} {
						short := anthropicTurnLooksSuspiciouslyShort(sr, runes, tokens, toolUse, thinking)
						healthy := anthropicTurnProvesUpstreamHealthy(sr, runes, tokens, toolUse, thinking)
						require.False(t, short && healthy,
							"stop_reason=%q prose=%d tokens=%d thinking=%d toolUse=%v 同时命中两个判定",
							sr, runes, tokens, thinking, toolUse)
					}
				}
			}
		}
	}
}

// 账号 9 的线上复现（21:23:19 out=63/chars=132 截断 → 几发 tool_use → 21:26:34
// out=34/chars=81 又截断）。改动前 tool_use 回合会把连击清零，streak 永远停在 1，
// 解绑是死代码；改动后中间回合不表态。
//
// 阈值降到 1 之后第一发就解绑，所以这条用例守的不再是「连击跨过 tool_use」，而是三态
// 判定本身：tool_use 回合既不累计也**不清零**。清零次数卡在 1（解绑自带的那一次）就是
// 证据——旧的二元判断会让它涨到 4。
func TestAnthropicPassthrough_ToolUseTurnBetweenShortTurnsStillUnbinds(t *testing.T) {
	const sessionKey = "sticky-interleaved"
	const groupID = int64(1)
	cache := newShortTurnStreakCache(sessionKey, 9)
	svc, repo := newShortTurnTestGatewayService(t, cache)
	key := shortTurnStreakKey(groupID, sessionKey)

	// 第一发截断：解绑。
	runShortTurnPassthrough(t, svc, groupID, sessionKey, 9, shortTurnSSE("临时文件都已清理删除，现在只剩部署目录。更新记", 63, false))
	requireUnboundOnce(t, cache, groupID, sessionKey)

	// 中间几发正常的 agent 工具调用回合：不得清零，也不得累计。
	for i := 0; i < 3; i++ {
		runShortTurnPassthrough(t, svc, groupID, sessionKey, 9,
			turnSSEWithStopReason(strings.Repeat("读日志抓数据。", 30), 761, "tool_use", true))
	}
	require.Equal(t, 1, cache.resets[key], "tool_use 中间回合不得触发清零")
	require.Empty(t, cache.streaks, "tool_use 中间回合不得累计连击")

	// 第二发截断：再次解绑（解绑后连击已清零，所以又是从 1 开始达标）。
	runShortTurnPassthrough(t, svc, groupID, sessionKey, 9, shortTurnSSE("日志键都在那个函数里。抓前后日志", 34, false))
	require.Equal(t, 2, cache.deletedSessions[sessionKey], "第二发截断必须再解绑一次")
	require.Equal(t, 2, repo.tempCalls, "两次解绑各配一次冷却，中间的 tool_use 回合不得罚号")
}

// 常规链路上的同一条复现。
func TestRegularPath_ToolUseTurnBetweenShortTurnsStillUnbinds(t *testing.T) {
	const sessionKey = "regular-interleaved"
	const groupID = int64(1)
	cache := newShortTurnStreakCache(sessionKey, 9)
	svc, repo := newShortTurnTestGatewayService(t, cache)
	key := shortTurnStreakKey(groupID, sessionKey)

	runShortTurnRegularPath(t, svc, groupID, sessionKey, 9, shortTurnSSE("短回答", 63, false))
	requireUnboundOnce(t, cache, groupID, sessionKey)

	for i := 0; i < 3; i++ {
		runShortTurnRegularPath(t, svc, groupID, sessionKey, 9,
			turnSSEWithStopReason(strings.Repeat("读日志抓数据。", 30), 761, "tool_use", true))
	}
	require.Equal(t, 1, cache.resets[key], "tool_use 中间回合不得触发清零")
	require.Empty(t, cache.streaks, "tool_use 中间回合不得累计连击")

	runShortTurnRegularPath(t, svc, groupID, sessionKey, 9, shortTurnSSE("又一句短回答", 34, false))
	require.Equal(t, 2, cache.deletedSessions[sessionKey], "第二发截断必须再解绑一次")
	require.Equal(t, 2, repo.tempCalls, "两次解绑各配一次冷却")
}

// cjkTruncationText 复刻实测中文截断的正文形状，长度按 anthropicShortTurnProseRuneLimit
// 反推，保证「字节数越界、rune 数没越界」这条前提跟着常量一起动。
//
// 实测样本是 247 个汉字 / out=106（UTF-8 下 741 字节）。上限从 400 抬到 800 之后 741 字节
// 不再越界，写死 247 就验不出单位有没有改对了。所以长度改成从常量算：中文一字 3 字节，
// 取「上限/3 + 40」个汉字，字节数必然过界而 rune 数仍落在窗口内。out 保持实测的 106，
// 密度 0.35 仍在实测截断那一带（0.35~0.62）。
func cjkTruncationText(t *testing.T) string {
	t.Helper()
	const sentence = "临时文件都已清理删除，现在只剩部署目录。" // 20 字
	target := anthropicShortTurnProseRuneLimit/3 + 40
	repeats := target/utf8.RuneCountInString(sentence) + 1
	text := string([]rune(strings.Repeat(sentence, repeats))[:target])
	require.Equal(t, target, utf8.RuneCountInString(text), "回归锚点：rune 数必须正好是算出来的目标值")
	require.LessOrEqual(t, utf8.RuneCountInString(text), anthropicShortTurnProseRuneLimit,
		"前提：rune 数必须落在短回合窗口内")
	require.Greater(t, len(text), anthropicShortTurnProseRuneLimit,
		"前提：字节数必须越过同一条上限，否则这条用例验不出单位有没有改对")
	return text
}

// 实测中文截断的复现（透传链路），锚定的是「按字节数还是按 rune 数」这个单位错。
//
// 改动前 anthropicVisibleDeltaChars 用 len() 数 UTF-8 字节，中文一字三字节，于是
// 「200 字符上限」对中文实际只有 66 字。这一发的汉字数落在窗口内、字节数早已越界，
// 旧口径下既躲过短回合判定，又（在旧的 visibleChars>200 口径下）满足
// anthropicTurnProvesUpstreamHealthy，**反过来把已积累的连击清零**——比单纯漏判更糟：
// 解绑永远触发不了，粘性把下一发原样送回同一个坏号。改动后按 rune 数判定，必须解绑。
func TestAnthropicPassthrough_CJKTruncationUnbinds(t *testing.T) {
	const sessionKey = "sticky-cjk"
	const groupID = int64(1)
	cache := newShortTurnStreakCache(sessionKey, 9)
	svc, repo := newShortTurnTestGatewayService(t, cache)
	text := cjkTruncationText(t)

	// 解绑发生了，就证明这一发被判成了可疑：旧实现按字节数会反过来把它当成「上游把话
	// 说完了」的正面证据去清零，那样一次解绑都不会有。
	runShortTurnPassthrough(t, svc, groupID, sessionKey, 9, shortTurnSSE(text, 106, false))
	requireUnboundOnce(t, cache, groupID, sessionKey)
	require.Equal(t, 1, repo.tempCalls, "达到解绑阈值后必须冷却账号")
}

// 常规链路上的同一条中文截断复现。
func TestRegularPath_CJKTruncationUnbinds(t *testing.T) {
	const sessionKey = "regular-cjk"
	const groupID = int64(1)
	cache := newShortTurnStreakCache(sessionKey, 9)
	svc, repo := newShortTurnTestGatewayService(t, cache)
	text := cjkTruncationText(t)

	runShortTurnRegularPath(t, svc, groupID, sessionKey, 9, shortTurnSSE(text, 106, false))
	requireUnboundOnce(t, cache, groupID, sessionKey)
	require.Equal(t, 1, repo.tempCalls, "达到解绑阈值后必须冷却账号")
}

// 22:31:21 那一发（131 字 / out=70）的端到端复现。它比上面那条短，抓住它的是
// output_tokens 天花板与抬高后的正文上限，与单位无关——两条锚点都要留，否则日后
// 有人把 anthropicShortTurnOutputTokenLimit 调回去，只有这一类会静默漏掉。
func TestAnthropicPassthrough_ShortCJKTruncationUnbinds(t *testing.T) {
	const sessionKey = "sticky-cjk-short"
	const groupID = int64(1)
	cache := newShortTurnStreakCache(sessionKey, 9)
	svc, repo := newShortTurnTestGatewayService(t, cache)

	// 20 字 × 6 + 11 字 = 131 字 ≈393 字节：字节数越过旧的 200 上限，是旧实现漏判的原因。
	text := strings.Repeat("临时文件都已清理删除，现在只剩部署目录。", 6) + "接下来我去看一下日志。"
	require.Equal(t, 131, utf8.RuneCountInString(text))
	require.Greater(t, len(text), 200, "前提：字节数必须超过旧实现那条 200 的线")

	runShortTurnPassthrough(t, svc, groupID, sessionKey, 9, shortTurnSSE(text, 70, false))
	requireUnboundOnce(t, cache, groupID, sessionKey)
	require.Equal(t, 1, repo.tempCalls, "达到解绑阈值后必须冷却账号")
}

// 「想很久却只吐一句」是最典型的截断形态，不得因为思考链很长就被放过。
// 旧实现把 thinking_delta 计入可见字符，长思考 + 一句正文会被当成成段回答。
func TestAnthropicPassthrough_LongThinkingShortProseStillUnbinds(t *testing.T) {
	const sessionKey = "sticky-thinking"
	const groupID = int64(1)
	cache := newShortTurnStreakCache(sessionKey, 9)
	svc, repo := newShortTurnTestGatewayService(t, cache)

	resp := func() *http.Response {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body: sseBody(
				`data: {"type":"message_start","message":{"usage":{"input_tokens":129000}}}`,
				"",
				`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
				"",
				fmt.Sprintf(`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":%q}}`,
					strings.Repeat("我得先想清楚这里的因果链条。", 60)),
				"",
				`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
				"",
				`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"我看一下。"}}`,
				"",
				`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":70}}`,
				"",
				`data: {"type":"message_stop"}`,
				"",
				"",
			),
		}
	}

	runShortTurnPassthrough(t, svc, groupID, sessionKey, 9, resp())
	requireUnboundOnce(t, cache, groupID, sessionKey)
	require.Equal(t, 1, repo.tempCalls, "达到解绑阈值后必须冷却账号")
}

// 账号 12 的端到端复现（透传链路，持流关闭 → 空响应已经交付给客户端）。
//
// 这一发同时要三件事：解绑（下一发换号）、罚号（把账号从调度池里暂时摘掉）、并且**不**
// 因为解绑而漏掉罚号。只解绑治不了这个账号：解绑之后重新选号仍可能落回它（它还在池子里、
// 健康度满分），而它 35.9% 的请求都是空响应；只罚号也不够：粘性绑定还钉在它上面，罚号
// 期间那条会话会被判成「绑定的账号临时不可用」而不是换号。
func TestAnthropicPassthrough_EmptyAnswerTurnUnbindsAndPenalizes(t *testing.T) {
	const sessionKey = "sticky-empty-answer"
	const groupID = int64(1)
	cache := newShortTurnStreakCache(sessionKey, 12)
	svc, repo := newShortTurnTestGatewayService(t, cache)

	runShortTurnPassthrough(t, svc, groupID, sessionKey, 12, emptyAnswerSSE(1))

	requireUnboundOnce(t, cache, groupID, sessionKey)
	require.Equal(t, 1, repo.tempCalls,
		"end_turn 却一个字都没说必须冷却账号，且不得与短回合报告器重复计数")
}

// 同一条链路上「说得少」与「一个字没说」的对照。短回合达到解绑阈值后也要冷却账号，
// 但必须与空回合报告器互斥，避免同一发重复罚号。
func TestAnthropicPassthrough_ShortButNonEmptyTurnPenalizesOnce(t *testing.T) {
	const sessionKey = "sticky-short-not-penalized"
	const groupID = int64(1)
	cache := newShortTurnStreakCache(sessionKey, 12)
	svc, repo := newShortTurnTestGatewayService(t, cache)

	// 只有一个字的正文：短到必然解绑，并且只触发一次解绑冷却。
	runShortTurnPassthrough(t, svc, groupID, sessionKey, 12, shortTurnSSE("好", 30, false))
	requireUnboundOnce(t, cache, groupID, sessionKey)
	require.Equal(t, 1, repo.tempCalls, "短回合报告器必须只执行一次")
}

// 常规链路上的账号 12 复现。第三方中转号不走透传，两条链路必须同口径。
func TestRegularPath_EmptyAnswerTurnUnbindsAndPenalizes(t *testing.T) {
	const sessionKey = "regular-empty-answer"
	const groupID = int64(1)
	cache := newShortTurnStreakCache(sessionKey, 12)
	svc, repo := newShortTurnTestGatewayService(t, cache)

	runShortTurnRegularPath(t, svc, groupID, sessionKey, 12, emptyAnswerSSE(1))

	requireUnboundOnce(t, cache, groupID, sessionKey)
	require.Equal(t, 1, repo.tempCalls, "常规链路必须与透传同口径且只冷却一次")
}

// 空回合不受 output_tokens 上限约束的端到端形态：长思考 + 零正文 + token 上量。
// 按上限卡就会漏掉它，而这正是「想很久却什么都没说」的实际样子。
func TestAnthropicPassthrough_LongThinkingZeroProseIsPenalized(t *testing.T) {
	const sessionKey = "sticky-thinking-empty"
	const groupID = int64(1)
	cache := newShortTurnStreakCache(sessionKey, 12)
	svc, repo := newShortTurnTestGatewayService(t, cache)

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: sseBody(
			`data: {"type":"message_start","message":{"usage":{"input_tokens":129000}}}`,
			"",
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
			"",
			fmt.Sprintf(`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":%q}}`,
				strings.Repeat("我得先想清楚这里的因果链条。", 60)),
			"",
			`data: {"type":"content_block_stop","index":0}`,
			"",
			// 思考 token 全部计入 output_tokens，远超短回合上限，但正文是零。
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":900}}`,
			"",
			`data: {"type":"message_stop"}`,
			"",
			"",
		),
	}
	runShortTurnPassthrough(t, svc, groupID, sessionKey, 12, resp)

	requireUnboundOnce(t, cache, groupID, sessionKey)
	require.Equal(t, 1, repo.tempCalls, "思考 token 上量不得让零正文逃过判定或重复计数")
}
