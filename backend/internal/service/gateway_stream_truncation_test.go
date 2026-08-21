package service

import (
	"context"
	"encoding/json"
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

// streamTruncationRepoStub 在无 build tag 的 sessionWindowMockRepo 之上记录
// 惩罚落库的参数，用于断言归因文案而非只断言调用次数。
// （errorPolicyRepoStub 在 //go:build unit 里，无标签测试不能引用。）
type streamTruncationRepoStub struct {
	sessionWindowMockRepo
	tempCalls      int
	setErrCalls    int
	lastErrorMsg   string
	lastTempReason string
	lastTempUntil  time.Time
}

func (r *streamTruncationRepoStub) SetTempUnschedulable(_ context.Context, _ int64, until time.Time, reason string) error {
	r.tempCalls++
	r.lastTempReason = reason
	r.lastTempUntil = until
	return nil
}

func (r *streamTruncationRepoStub) SetError(_ context.Context, _ int64, errorMsg string) error {
	r.setErrCalls++
	r.lastErrorMsg = errorMsg
	return nil
}

type streamTruncationCounterStub struct {
	count  int64
	resets int
}

func (c *streamTruncationCounterStub) IncrementTimeoutCount(_ context.Context, _ int64, _ int) (int64, error) {
	c.count++
	return c.count, nil
}
func (c *streamTruncationCounterStub) GetTimeoutCount(_ context.Context, _ int64) (int64, error) {
	return c.count, nil
}
func (c *streamTruncationCounterStub) ResetTimeoutCount(_ context.Context, _ int64) error {
	c.resets++
	c.count = 0
	return nil
}
func (c *streamTruncationCounterStub) GetTimeoutCountTTL(_ context.Context, _ int64) (time.Duration, error) {
	return time.Minute, nil
}

var _ TimeoutCounterCache = (*streamTruncationCounterStub)(nil)

func newStreamTruncationRateLimitService(t *testing.T, settingsJSON string) (*RateLimitService, *streamTruncationRepoStub, *streamTruncationCounterStub) {
	t.Helper()
	repo := &streamTruncationRepoStub{}
	counter := &streamTruncationCounterStub{}
	svc := &RateLimitService{
		accountRepo:         repo,
		timeoutCounterCache: counter,
	}
	svc.settingService = NewSettingService(&fakeSettingRepo{
		vals: map[string]string{SettingKeyStreamTimeoutSettings: settingsJSON},
	}, &config.Config{})
	svc.timeoutCounterCache = counter
	return svc, repo, counter
}

// 上游把已提交的流截断且没送终止事件时，本次请求已无法 failover（字节已 flush），
// 但必须罚账号——否则客户端紧接着的重试会被粘性送回同一个坏账号，表现为
// 「报错但从不主副切换」。
func TestHandleStreamTruncated_MarksAccountTempUnschedulable(t *testing.T) {
	svc, repo, counter := newStreamTruncationRateLimitService(t,
		`{"enabled":true,"action":"temp_unsched","temp_unsched_minutes":1,"threshold_count":1,"threshold_window_minutes":10}`)

	blocked := svc.HandleStreamTruncated(context.Background(), &Account{ID: 7, Name: "acct"}, "claude-opus-5")
	require.True(t, blocked, "达阈后必须停止调度该账号")
	require.Equal(t, 1, repo.tempCalls)
	require.Equal(t, 1, counter.resets, "达阈后应重置计数窗口")

	var state TempUnschedState
	require.NoError(t, json.Unmarshal([]byte(repo.lastTempReason), &state))
	require.Equal(t, "stream_truncated", state.MatchedKeyword,
		"截断不得被误报成空闲超时，否则运维无法区分故障模式")
	require.Contains(t, state.ErrorMessage, "claude-opus-5")
	require.Contains(t, state.ErrorMessage, "truncated")
	require.Equal(t, -1, state.RuleIndex)
}

// 空闲超时的归因文案不得被截断改动波及。
func TestHandleStreamTimeout_KeepsTimeoutAttribution(t *testing.T) {
	svc, repo, _ := newStreamTruncationRateLimitService(t,
		`{"enabled":true,"action":"temp_unsched","temp_unsched_minutes":1,"threshold_count":1,"threshold_window_minutes":10}`)

	require.True(t, svc.HandleStreamTimeout(context.Background(), &Account{ID: 8}, "claude-opus-5"))

	var state TempUnschedState
	require.NoError(t, json.Unmarshal([]byte(repo.lastTempReason), &state))
	require.Equal(t, "stream_timeout", state.MatchedKeyword)
	require.Contains(t, state.ErrorMessage, "Stream data interval timeout")
}

// 未达阈值时只累计计数，不罚账号。
func TestHandleStreamTruncated_BelowThresholdDoesNotPenalize(t *testing.T) {
	svc, repo, counter := newStreamTruncationRateLimitService(t,
		`{"enabled":true,"action":"temp_unsched","temp_unsched_minutes":1,"threshold_count":3,"threshold_window_minutes":10}`)

	require.False(t, svc.HandleStreamTruncated(context.Background(), &Account{ID: 9}, "m"))
	require.False(t, svc.HandleStreamTruncated(context.Background(), &Account{ID: 9}, "m"))
	require.Equal(t, 0, repo.tempCalls)
	require.Equal(t, int64(2), counter.count)

	require.True(t, svc.HandleStreamTruncated(context.Background(), &Account{ID: 9}, "m"))
	require.Equal(t, 1, repo.tempCalls)
}

// 运维关掉开关后不得罚账号。
func TestHandleStreamTruncated_DisabledIsNoop(t *testing.T) {
	svc, repo, counter := newStreamTruncationRateLimitService(t,
		`{"enabled":false,"action":"temp_unsched","temp_unsched_minutes":1,"threshold_count":1,"threshold_window_minutes":10}`)

	require.False(t, svc.HandleStreamTruncated(context.Background(), &Account{ID: 10}, "m"))
	require.Equal(t, 0, repo.tempCalls)
	require.Equal(t, int64(0), counter.count)
}

// action=error 时用截断文案落 SetError，不得再写成空闲超时。
func TestHandleStreamTruncated_ActionErrorUsesTruncationMessage(t *testing.T) {
	svc, repo, _ := newStreamTruncationRateLimitService(t,
		`{"enabled":true,"action":"error","temp_unsched_minutes":1,"threshold_count":1,"threshold_window_minutes":10}`)

	require.True(t, svc.HandleStreamTruncated(context.Background(), &Account{ID: 11}, "claude-opus-5"))
	require.Equal(t, 1, repo.setErrCalls)
	require.Contains(t, repo.lastErrorMsg, "truncated")
	require.Contains(t, repo.lastErrorMsg, "repeated failures")
}

// 端到端：透传流被上游截断（已提交 content_block_start）后，必须既记 ops 归因
// 又罚账号。这正是线上那次 "HTTP 200 + 空/畸形响应，且从不主副切换" 的成因。
func TestAnthropicPassthrough_TruncatedStreamAfterCommitPenalizesAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	rl, repo, _ := newStreamTruncationRateLimitService(t,
		`{"enabled":true,"action":"temp_unsched","temp_unsched_minutes":1,"threshold_count":1,"threshold_window_minutes":10}`)
	svc := &GatewayService{
		cfg:              &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}},
		rateLimitService: rl,
	}

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`data: {"type":"message_start","message":{"usage":{"input_tokens":11}}}`,
			"",
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			"",
		}, "\n"))),
	}

	result, err := svc.handleStreamingResponseAnthropicAPIKeyPassthrough(
		context.Background(), resp, c, &Account{ID: 5, Name: "acct", Platform: PlatformAnthropic}, time.Now(), "claude-opus-5")
	require.Error(t, err)
	require.Contains(t, err.Error(), "missing terminal event")
	require.NotNil(t, result, "已计量的 usage 不得漏记")

	require.Equal(t, 1, repo.tempCalls, "截断必须罚账号，否则客户端重试会被粘性送回同一个坏账号")
	var state TempUnschedState
	require.NoError(t, json.Unmarshal([]byte(repo.lastTempReason), &state))
	require.Equal(t, "stream_truncated", state.MatchedKeyword)

	raw, ok := c.Get(OpsUpstreamErrorsKey)
	require.True(t, ok, "必须留下 ops 归因，否则 ops_error_logs 只有一行 upstream 字段全 null 的空壳")
	events, ok := raw.([]*OpsUpstreamErrorEvent)
	require.True(t, ok)
	require.Len(t, events, 1)
	require.Equal(t, "stream_truncated_after_commit", events[0].Kind)
	require.Equal(t, int64(5), events[0].AccountID)
	require.Equal(t, http.StatusBadGateway, events[0].UpstreamStatusCode)
}

// 客户端自己取消时不得罚账号：换号无用，罚了只会误伤好账号。
func TestAnthropicPassthrough_ClientCanceledDoesNotPenalizeAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	ctx, cancel := context.WithCancel(context.Background())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil).WithContext(ctx)
	cancel()

	rl, repo, _ := newStreamTruncationRateLimitService(t,
		`{"enabled":true,"action":"temp_unsched","temp_unsched_minutes":1,"threshold_count":1,"threshold_window_minutes":10}`)
	svc := &GatewayService{
		cfg:              &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}},
		rateLimitService: rl,
	}

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`data: {"type":"message_start","message":{"usage":{"input_tokens":11}}}`,
			"",
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			"",
		}, "\n"))),
	}

	_, err := svc.handleStreamingResponseAnthropicAPIKeyPassthrough(
		context.Background(), resp, c, &Account{ID: 6, Name: "acct", Platform: PlatformAnthropic}, time.Now(), "claude-opus-5")
	require.Error(t, err)
	require.Equal(t, 0, repo.tempCalls, "客户端取消不是账号故障")
	_, hasEvents := c.Get(OpsUpstreamErrorsKey)
	require.False(t, hasEvents, "客户端取消不应记为上游故障")
}

// 正常收到 message_stop 的流不得触发任何惩罚。
func TestAnthropicPassthrough_CompleteStreamDoesNotPenalizeAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	rl, repo, counter := newStreamTruncationRateLimitService(t,
		`{"enabled":true,"action":"temp_unsched","temp_unsched_minutes":1,"threshold_count":1,"threshold_window_minutes":10}`)
	svc := &GatewayService{
		cfg:              &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}},
		rateLimitService: rl,
	}

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`data: {"type":"message_start","message":{"usage":{"input_tokens":11}}}`,
			"",
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			"",
			`data: {"type":"message_delta","usage":{"output_tokens":5}}`,
			"",
			`data: {"type":"message_stop"}`,
			"",
		}, "\n"))),
	}

	result, err := svc.handleStreamingResponseAnthropicAPIKeyPassthrough(
		context.Background(), resp, c, &Account{ID: 12, Name: "acct", Platform: PlatformAnthropic}, time.Now(), "claude-opus-5")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 0, repo.tempCalls)
	require.Equal(t, int64(0), counter.count, "正常流不得累计失败计数")
}
