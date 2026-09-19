package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func newRefusalTestGatewayService(t *testing.T, settingsJSON string) (*GatewayService, *streamDeliveryRepoStub) {
	t.Helper()
	rl, repo, _ := newStreamDeliveryRateLimitService(t, settingsJSON)
	return &GatewayService{
		cfg:              &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}},
		rateLimitService: rl,
	}, repo
}

const refusalPenaltySettings = `{"enabled":true,"action":"temp_unsched","temp_unsched_minutes":1,"threshold_count":1,"threshold_window_minutes":10}`

func newRefusalTestContext(t *testing.T) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	return c, rec
}

func sseBody(lines ...string) io.ReadCloser {
	return io.NopCloser(strings.NewReader(strings.Join(lines, "\n")))
}

func opsEvents(t *testing.T, c *gin.Context) []*OpsUpstreamErrorEvent {
	t.Helper()
	raw, ok := c.Get(OpsUpstreamErrorsKey)
	if !ok {
		return nil
	}
	events, ok := raw.([]*OpsUpstreamErrorEvent)
	require.True(t, ok)
	return events
}

// 拒答在可见内容之后才到（真的没法切号了）：本次请求原样透传，但必须留 ops 归因并罚账号，
// 否则客户端紧接着的重试会被粘性送回同一个坏账号，表现为「一直报 safeguards 却从不切副号」。
func TestAnthropicPassthrough_RefusalAfterVisibleContentPenalizesAccount(t *testing.T) {
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
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"部分答案"}}`,
			"",
			`data: {"type":"message_delta","delta":{"stop_reason":"refusal"},"usage":{"output_tokens":9}}`,
			"",
			`data: {"type":"message_stop"}`,
			"",
		),
	}

	result, err := svc.handleStreamingResponseAnthropicAPIKeyPassthrough(
		context.Background(), resp, c, &Account{ID: 5, Name: "primary", Platform: PlatformAnthropic}, time.Now(), "claude-opus-5")
	require.NoError(t, err, "已提交的流仍须正常收尾，不能因为拒答就改写响应")
	require.NotNil(t, result)
	require.Contains(t, rec.Body.String(), "部分答案", "已提交的内容必须原样送达")

	require.Equal(t, 1, repo.tempCalls, "切不了号时必须罚账号，让下一发落到副号")
	var state TempUnschedState
	require.NoError(t, json.Unmarshal([]byte(repo.lastTempReason), &state))
	require.Equal(t, "stream_safety_refusal", state.MatchedKeyword, "拒答不得被误报成截断或空闲超时")

	events := opsEvents(t, c)
	require.Len(t, events, 1)
	require.Equal(t, "safety_refusal_no_failover", events[0].Kind)
	require.Equal(t, http.StatusForbidden, events[0].UpstreamStatusCode)
}

// 同一条流里出现多帧 refusal（message_delta 与 message_stop 都带）时只归因一次，
// 否则一次故障会被记成多次、把阈值窗口的计数打乱。
func TestAnthropicPassthrough_RepeatedRefusalFramesReportOnce(t *testing.T) {
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
			`data: {"type":"message_delta","delta":{"stop_reason":"refusal"}}`,
			"",
			`data: {"type":"message_delta","delta":{"stop_reason":"refusal"}}`,
			"",
			`data: {"type":"message_stop","message":{"stop_reason":"refusal"}}`,
			"",
		),
	}

	_, err := svc.handleStreamingResponseAnthropicAPIKeyPassthrough(
		context.Background(), resp, c, &Account{ID: 5, Name: "primary", Platform: PlatformAnthropic}, time.Now(), "claude-opus-5")
	require.NoError(t, err)
	require.Equal(t, 1, repo.tempCalls)
	require.Len(t, opsEvents(t, c), 1, "一条流只记一次拒答归因")
}

// 防止收紧提交判定收得过头：带真实思考内容的正常流不得被罚，且推迟提交的前奏必须
// 按原顺序完整补发给客户端。
func TestAnthropicPassthrough_ThinkingStreamStillDeliveredInOrder(t *testing.T) {
	c, rec := newRefusalTestContext(t)
	svc, repo := newRefusalTestGatewayService(t, refusalPenaltySettings)

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: sseBody(
			`data: {"type":"message_start","message":{"usage":{"input_tokens":11}}}`,
			"",
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
			"",
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"先想一想"}}`,
			"",
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"CAIS"}}`,
			"",
			`data: {"type":"content_block_stop","index":0}`,
			"",
			`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
			"",
			`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"答案"}}`,
			"",
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}`,
			"",
			`data: {"type":"message_stop"}`,
			"",
		),
	}

	result, err := svc.handleStreamingResponseAnthropicAPIKeyPassthrough(
		context.Background(), resp, c, &Account{ID: 5, Name: "primary", Platform: PlatformAnthropic}, time.Now(), "claude-opus-5")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 0, repo.tempCalls, "正常流不得罚账号")
	require.Empty(t, opsEvents(t, c))

	got := rec.Body.String()
	order := []string{"message_start", `"thinking":""`, "先想一想", "signature_delta", "答案", "message_stop"}
	last := -1
	for _, needle := range order {
		idx := strings.Index(got, needle)
		require.Greater(t, idx, last, "推迟提交的前奏必须按原顺序补发：%s 位置错乱", needle)
		last = idx
	}
	require.Equal(t, 7, result.usage.OutputTokens, "推迟提交不得影响计量")
}

// 非透传的非流式路径此前压根没有拒答处理。此刻响应体全在手、一个字节都还没写给客户端，
// failover 窗口完好，必须切号而不是把拒答透传出去。
func TestNonStreamingResponse_SafetyRefusalFailsOver(t *testing.T) {
	c, rec := newRefusalTestContext(t)
	svc, repo := newRefusalTestGatewayService(t, refusalPenaltySettings)

	body := `{"id":"msg_1","type":"message","stop_reason":"refusal","content":[{"type":"thinking","thinking":"","signature":"CAIS"}],"usage":{"input_tokens":103000,"output_tokens":5}}`
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}

	usage, err := svc.handleNonStreamingResponse(
		context.Background(), resp, c, &Account{ID: 5, Name: "primary", Platform: PlatformAnthropic}, "claude-opus-5", "claude-opus-5")
	require.Error(t, err)
	require.Nil(t, usage)

	var failover *UpstreamFailoverError
	require.True(t, errors.As(err, &failover))
	require.Equal(t, GatewayFailureReason("anthropic_safeguards"), failover.Reason)
	require.Zero(t, rec.Body.Len(), "切号前不得向客户端写任何字节")
	require.Equal(t, 0, repo.tempCalls, "真切号不额外罚账号")

	events := opsEvents(t, c)
	require.Len(t, events, 1)
	require.Equal(t, "safety_refusal_failover", events[0].Kind)
}

// 惩罚记账：拒答与截断/空闲超时共用 stream_timeout_settings，但归因文案必须可区分。
func TestHandleStreamRefused_MarksAccountTempUnschedulable(t *testing.T) {
	svc, repo, counter := newStreamDeliveryRateLimitService(t, refusalPenaltySettings)

	blocked := svc.HandleStreamRefused(context.Background(), &Account{ID: 7, Name: "acct"}, "claude-opus-5")
	require.True(t, blocked, "达阈后必须停止调度该账号")
	require.Equal(t, 1, repo.tempCalls)
	require.Equal(t, 1, counter.resets)

	var state TempUnschedState
	require.NoError(t, json.Unmarshal([]byte(repo.lastTempReason), &state))
	require.Equal(t, "stream_safety_refusal", state.MatchedKeyword)
	require.Contains(t, state.ErrorMessage, "claude-opus-5")
	require.Contains(t, state.ErrorMessage, "refusal")
	require.Equal(t, -1, state.RuleIndex)
}

// 运维关掉开关后拒答也不得罚账号。
func TestHandleStreamRefused_DisabledIsNoop(t *testing.T) {
	svc, repo, counter := newStreamDeliveryRateLimitService(t,
		`{"enabled":false,"action":"temp_unsched","temp_unsched_minutes":1,"threshold_count":1,"threshold_window_minutes":10}`)

	require.False(t, svc.HandleStreamRefused(context.Background(), &Account{ID: 10}, "m"))
	require.Equal(t, 0, repo.tempCalls)
	require.Equal(t, int64(0), counter.count)
}
