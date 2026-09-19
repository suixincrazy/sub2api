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

// streamDeliveryRepoStub 在无 build tag 的 sessionWindowMockRepo 之上记录
// 惩罚落库的参数，用于断言归因文案而非只断言调用次数。
// （errorPolicyRepoStub 在 //go:build unit 里，无标签测试不能引用。）
type streamDeliveryRepoStub struct {
	sessionWindowMockRepo
	tempCalls      int
	setErrCalls    int
	lastErrorMsg   string
	lastTempReason string
	lastTempUntil  time.Time
}

func (r *streamDeliveryRepoStub) SetTempUnschedulable(_ context.Context, _ int64, until time.Time, reason string) error {
	r.tempCalls++
	r.lastTempReason = reason
	r.lastTempUntil = until
	return nil
}

func (r *streamDeliveryRepoStub) SetError(_ context.Context, _ int64, errorMsg string) error {
	r.setErrCalls++
	r.lastErrorMsg = errorMsg
	return nil
}

type streamDeliveryCounterStub struct {
	count  int64
	resets int
}

func (c *streamDeliveryCounterStub) IncrementTimeoutCount(_ context.Context, _ int64, _ int) (int64, error) {
	c.count++
	return c.count, nil
}
func (c *streamDeliveryCounterStub) GetTimeoutCount(_ context.Context, _ int64) (int64, error) {
	return c.count, nil
}
func (c *streamDeliveryCounterStub) ResetTimeoutCount(_ context.Context, _ int64) error {
	c.resets++
	c.count = 0
	return nil
}
func (c *streamDeliveryCounterStub) GetTimeoutCountTTL(_ context.Context, _ int64) (time.Duration, error) {
	return time.Minute, nil
}

var _ TimeoutCounterCache = (*streamDeliveryCounterStub)(nil)

func newStreamDeliveryRateLimitService(t *testing.T, settingsJSON string) (*RateLimitService, *streamDeliveryRepoStub, *streamDeliveryCounterStub) {
	t.Helper()
	repo := &streamDeliveryRepoStub{}
	counter := &streamDeliveryCounterStub{}
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

// 空闲超时的归因文案不得被截断改动波及。
func TestHandleStreamTimeout_KeepsTimeoutAttribution(t *testing.T) {
	svc, repo, _ := newStreamDeliveryRateLimitService(t,
		`{"enabled":true,"action":"temp_unsched","temp_unsched_minutes":1,"threshold_count":1,"threshold_window_minutes":10}`)

	require.True(t, svc.HandleStreamTimeout(context.Background(), &Account{ID: 8}, "claude-opus-5"))

	var state TempUnschedState
	require.NoError(t, json.Unmarshal([]byte(repo.lastTempReason), &state))
	require.Equal(t, "stream_timeout", state.MatchedKeyword)
	require.Contains(t, state.ErrorMessage, "Stream data interval timeout")
}

// EOF retains the upstream error and delivered bytes without custom account penalties.
func TestAnthropicPassthrough_TruncatedStreamKeepsUpstreamBehavior(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	rl, repo, _ := newStreamDeliveryRateLimitService(t,
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

	require.Zero(t, repo.tempCalls)
	require.Zero(t, repo.setErrCalls)
	require.Contains(t, rec.Body.String(), "message_start")
	_, hasEvents := c.Get(OpsUpstreamErrorsKey)
	require.False(t, hasEvents)

}

// 客户端自己取消时不得罚账号：换号无用，罚了只会误伤好账号。
func TestAnthropicPassthrough_ClientCanceledDoesNotPenalizeAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	ctx, cancel := context.WithCancel(context.Background())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil).WithContext(ctx)
	cancel()

	rl, repo, _ := newStreamDeliveryRateLimitService(t,
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

	rl, repo, counter := newStreamDeliveryRateLimitService(t,
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
