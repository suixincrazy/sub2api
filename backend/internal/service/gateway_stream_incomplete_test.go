package service

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// anthropicStreamLooksIncompleteDespiteTerminal 是纯判定，误报会把健康账号罚下线，
// 所以边界必须逐条钉住：白名单值、上游故障值、空值、以及「短但合法的回答」。
func TestAnthropicStreamLooksIncompleteDespiteTerminal(t *testing.T) {
	cases := []struct {
		name                 string
		stopReason           string
		visibleChars         int
		outputTokens         int
		sawContentBlockStart bool
		incomplete           bool
	}{
		// 白名单：四种正常收束都不得判残缺。
		{"end_turn 正常", "end_turn", 120, 30, true, false},
		{"max_tokens 正常", "max_tokens", 4000, 1024, true, false},
		{"tool_use 正常", "tool_use", 80, 20, true, false},
		{"stop_sequence 正常", "stop_sequence", 40, 10, true, false},
		{"pause_turn 正常", "pause_turn", 40, 10, true, false},
		{"大小写与空格不影响白名单", "  End_Turn  ", 40, 10, true, false},

		// 上游故障值：必须判残缺，这是信号 1 的主目标。
		{"未知故障值判残缺", "upstream_error", 120, 30, true, true},
		{"model_error 判残缺", "model_error", 120, 30, true, true},

		// refusal 例外：已由 reportSafetyRefusalWithoutFailover 专管，重复上报会打乱阈值窗口。
		{"refusal 交给拒答路径不重复判", "refusal", 0, 5, true, false},
		{"refusal 大小写同样放行", "REFUSAL", 0, 5, true, false},

		// 空 stop_reason：兼容层不一定填，只由信号 2 决定。
		{"空值且有正文不判", "", 120, 30, true, false},
		{"空值且开了块却零输出判残缺", "", 0, 0, true, true},
		{"空值且没开块不判", "", 0, 0, false, false},

		// 「短但合法」是误报的主要来源：只要 output_tokens 或可见字符任一为正就放行。
		{"极短回答不得误判", "", 2, 1, true, false},
		{"仅 output_tokens 为正也放行", "", 0, 5, true, false},
		{"仅可见字符为正也放行", "", 3, 0, true, false},

		// 没开正文块的空流由 sawTerminalEvent 那条既有路径处理，这里不插手。
		{"没开块即使零输出也不判", "", 0, 0, false, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason, incomplete := anthropicStreamLooksIncompleteDespiteTerminal(
				tc.stopReason, tc.visibleChars, tc.outputTokens, tc.sawContentBlockStart)
			require.Equal(t, tc.incomplete, incomplete)
			if incomplete {
				require.NotEmpty(t, reason, "判残缺必须给出可归因的原因，否则 ops 看板只有空壳")
			} else {
				require.Empty(t, reason)
			}
		})
	}
}

func TestAnthropicStopReasonIsHealthy(t *testing.T) {
	for _, s := range []string{"end_turn", "max_tokens", "tool_use", "stop_sequence", "pause_turn", " END_TURN "} {
		require.True(t, anthropicStopReasonIsHealthy(s), "%q 是正常收束", s)
	}
	// 白名单而非黑名单：未知值一律判异常，宁可多罚一次一分钟冷却。
	for _, s := range []string{"", "refusal", "upstream_error", "model_error", "unknown_future_value"} {
		require.False(t, anthropicStopReasonIsHealthy(s), "%q 不在白名单", s)
	}
}

func TestAnthropicVisibleDeltaChars(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    int
	}{
		{"text_delta 计数", `{"type":"content_block_delta","delta":{"type":"text_delta","text":"hello"}}`, 5},
		{"thinking_delta 计数", `{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"abc"}}`, 3},
		{"input_json_delta 计数", `{"type":"content_block_delta","delta":{"type":"input_json_delta","partial_json":"{\"a\":1}"}}`, 7},
		// signature 不是用户可见内容，算进去会让纯 signature 的拒答流被误判成有正文。
		{"signature_delta 不计数", `{"type":"content_block_delta","delta":{"type":"signature_delta","signature":"CAIS"}}`, 0},
		{"非 delta 帧不计数", `{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`, 0},
		{"content_block_start 不计数", `{"type":"content_block_start","content_block":{"type":"text","text":""}}`, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, anthropicVisibleDeltaChars(gjson.Parse(tc.payload)))
		})
	}
}

// 端到端信号 1：上游送了 message_stop，但 stop_reason 是故障值。旧判定 sawTerminalEvent=true
// 一路放行、账号不被罚，粘性把下一发重试原样送回同一个坏号——这正是「一直断流却从不主副切换」。
func TestAnthropicPassthrough_AbnormalStopReasonPenalizesAccount(t *testing.T) {
	c, _ := newRefusalTestContext(t)
	svc, repo := newRefusalTestGatewayService(t, refusalPenaltySettings)

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: sseBody(
			`data: {"type":"message_start","message":{"usage":{"input_tokens":11}}}`,
			"",
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			"",
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"半句话"}}`,
			"",
			`data: {"type":"message_delta","delta":{"stop_reason":"upstream_error"},"usage":{"output_tokens":3}}`,
			"",
			`data: {"type":"message_stop"}`,
			"",
		),
	}

	// 已提交的字节必须原样送达，不能因为判残缺就改写响应。
	_, err := svc.handleStreamingResponseAnthropicAPIKeyPassthrough(
		context.Background(), resp, c, &Account{ID: 5, Name: "primary", Platform: PlatformAnthropic}, time.Now(), "claude-opus-5")
	require.NoError(t, err)

	require.Equal(t, 1, repo.tempCalls, "异常 stop_reason 必须罚账号，否则重试被粘性送回坏号")
	var state TempUnschedState
	require.NoError(t, json.Unmarshal([]byte(repo.lastTempReason), &state))
	require.Equal(t, "stream_truncated", state.MatchedKeyword)

	events := opsEvents(t, c)
	require.Len(t, events, 1)
	require.Equal(t, "stream_incomplete_after_commit", events[0].Kind)
	require.Contains(t, events[0].Detail, "upstream_error")
}

// 端到端信号 2：开了正文块却零可见字符且 output_tokens<=0——截断的确定形态。
func TestAnthropicPassthrough_TerminalWithZeroOutputPenalizesAccount(t *testing.T) {
	c, _ := newRefusalTestContext(t)
	svc, repo := newRefusalTestGatewayService(t, refusalPenaltySettings)

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: sseBody(
			`data: {"type":"message_start","message":{"usage":{"input_tokens":11}}}`,
			"",
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			"",
			`data: {"type":"content_block_stop","index":0}`,
			"",
			`data: {"type":"message_stop"}`,
			"",
		),
	}

	_, err := svc.handleStreamingResponseAnthropicAPIKeyPassthrough(
		context.Background(), resp, c, &Account{ID: 6, Name: "primary", Platform: PlatformAnthropic}, time.Now(), "claude-opus-5")
	require.NoError(t, err)

	require.Equal(t, 1, repo.tempCalls, "开了块却零输出必须罚账号")
	events := opsEvents(t, c)
	require.Len(t, events, 1)
	require.Equal(t, "stream_incomplete_after_commit", events[0].Kind)
}

// 误报护栏：极短但合法的回答不得被罚。用户完全可能问一个只需几个 token 的问题，
// 罚下线健康账号的代价比漏罚一次高得多。
func TestAnthropicPassthrough_ShortLegitimateAnswerNotPenalized(t *testing.T) {
	c, rec := newRefusalTestContext(t)
	svc, repo := newRefusalTestGatewayService(t, refusalPenaltySettings)

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: sseBody(
			`data: {"type":"message_start","message":{"usage":{"input_tokens":11}}}`,
			"",
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			"",
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"4"}}`,
			"",
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`,
			"",
			`data: {"type":"message_stop"}`,
			"",
		),
	}

	_, err := svc.handleStreamingResponseAnthropicAPIKeyPassthrough(
		context.Background(), resp, c, &Account{ID: 7, Name: "primary", Platform: PlatformAnthropic}, time.Now(), "claude-opus-5")
	require.NoError(t, err)
	require.Contains(t, rec.Body.String(), `"4"`, "短回答必须原样送达")
	require.Equal(t, 0, repo.tempCalls, "一个字的正常回答不得罚账号")
	require.Empty(t, opsEvents(t, c))
}

// 误报护栏：拒答已由专门路径归因，不得被内容判定重复上报——一次故障记两次会打乱阈值窗口。
func TestAnthropicPassthrough_RefusalNotDoubleReportedAsIncomplete(t *testing.T) {
	c, _ := newRefusalTestContext(t)
	svc, repo := newRefusalTestGatewayService(t, refusalPenaltySettings)

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: sseBody(
			`data: {"type":"message_start","message":{"usage":{"input_tokens":11}}}`,
			"",
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"x"}}`,
			"",
			`data: {"type":"message_delta","delta":{"stop_reason":"refusal"},"usage":{"output_tokens":5}}`,
			"",
			`data: {"type":"message_stop"}`,
			"",
		),
	}

	_, err := svc.handleStreamingResponseAnthropicAPIKeyPassthrough(
		context.Background(), resp, c, &Account{ID: 8, Name: "primary", Platform: PlatformAnthropic}, time.Now(), "claude-opus-5")
	require.NoError(t, err)

	require.Equal(t, 1, repo.tempCalls, "拒答只罚一次")
	var state TempUnschedState
	require.NoError(t, json.Unmarshal([]byte(repo.lastTempReason), &state))
	require.Equal(t, "stream_safety_refusal", state.MatchedKeyword,
		"拒答不得被改写成 stream_truncated，否则运维分不清故障模式")
	require.Len(t, opsEvents(t, c), 1, "一条流只记一次归因")
}
