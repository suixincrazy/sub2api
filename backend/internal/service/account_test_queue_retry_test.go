//go:build unit

package service

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const accountTestQueued429Body = `{"error":{"code":"concurrency_queued","message":"All concurrency slots are busy. You are number 27 of 54 in the queue, please retry in about 5 seconds"}}`

// withFastAccountTestQueueRetry 把排队重试的等待压到毫秒级，语义不变。
func withFastAccountTestQueueRetry(t *testing.T, budget time.Duration) {
	t.Helper()
	budgetBefore := accountTestQueueRetryBudget
	minBefore := accountTestQueueRetryMinDelay
	maxBefore := accountTestQueueRetryMaxDelay
	accountTestQueueRetryBudget = budget
	accountTestQueueRetryMinDelay = time.Millisecond
	accountTestQueueRetryMaxDelay = time.Millisecond
	t.Cleanup(func() {
		accountTestQueueRetryBudget = budgetBefore
		accountTestQueueRetryMinDelay = minBefore
		accountTestQueueRetryMaxDelay = maxBefore
	})
}

func newAccountTestQueuedAccount(id int64) *Account {
	return &Account{
		ID:          id,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Status:      StatusActive,
		Concurrency: 1,
		Credentials: map[string]any{"access_token": "test-token"},
	}
}

func newAccountTestStreamSuccess() *http.Response {
	resp := newJSONResponse(http.StatusOK, "")
	resp.Body = io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\"}\n\n"))
	return resp
}

// 排队型 429 必须在同号上重发，排到槽位后测试应判绿。
// 中转上游并发槽满时恒回该 429，不重发时账号测试与它在网关上的真实可用性相反。
func TestAccountTestService_Queued429RetriesUntilAdmitted(t *testing.T) {
	gin.SetMode(gin.TestMode)
	withFastAccountTestQueueRetry(t, time.Minute)
	ctx, recorder := newTestContext()

	upstream := &queuedHTTPUpstream{responses: []*http.Response{
		newJSONResponse(http.StatusTooManyRequests, accountTestQueued429Body),
		newJSONResponse(http.StatusTooManyRequests, accountTestQueued429Body),
		newAccountTestStreamSuccess(),
	}}
	svc := &AccountTestService{accountRepo: &openAIAccountTestRepo{}, httpUpstream: upstream}

	err := svc.testOpenAIAccountConnection(ctx, newAccountTestQueuedAccount(310), "gpt-5.4", "", "")
	require.NoError(t, err)
	require.Len(t, upstream.requests, 3, "queued 429 must be re-sent on the same account")
	require.Contains(t, recorder.Body.String(), "\"success\":true")

	// 重发必须带上完整请求体，否则上游看到的是空体请求。
	for i, req := range upstream.requests {
		require.NotNil(t, req.Body, "attempt %d lost its body", i+1)
		body, readErr := io.ReadAll(req.Body)
		require.NoError(t, readErr)
		require.Contains(t, string(body), "\"input\"", "attempt %d body was not replayed", i+1)
	}
}

// 配额型 429（usage_limit_reached）不是排队，重发只会空转：必须一次失败即返回。
func TestAccountTestService_Quota429DoesNotRetry(t *testing.T) {
	gin.SetMode(gin.TestMode)
	withFastAccountTestQueueRetry(t, time.Minute)
	ctx, _ := newTestContext()

	upstream := &queuedHTTPUpstream{responses: []*http.Response{
		newJSONResponse(http.StatusTooManyRequests, `{"error":{"type":"usage_limit_reached","message":"limit reached","resets_at":1777283883}}`),
	}}
	repo := &openAIAccountTestRepo{}
	svc := &AccountTestService{accountRepo: repo, httpUpstream: upstream}
	account := newAccountTestQueuedAccount(311)

	err := svc.testOpenAIAccountConnection(ctx, account, "gpt-5.4", "", "")
	require.Error(t, err)
	require.Len(t, upstream.requests, 1, "quota 429 must not be retried on the same account")
	// 原有的限流状态同步不能被重试层吞掉。
	require.Equal(t, account.ID, repo.rateLimitedID)
}

// 预算耗尽时返回的 429 响应体仍须可读，否则错误信息会退化成空串。
func TestAccountTestService_Queued429BudgetExhaustedKeepsReadableBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	withFastAccountTestQueueRetry(t, 0)
	ctx, recorder := newTestContext()

	upstream := &queuedHTTPUpstream{responses: []*http.Response{
		newJSONResponse(http.StatusTooManyRequests, accountTestQueued429Body),
	}}
	svc := &AccountTestService{accountRepo: &openAIAccountTestRepo{}, httpUpstream: upstream}

	err := svc.testOpenAIAccountConnection(ctx, newAccountTestQueuedAccount(312), "gpt-5.4", "", "")
	require.Error(t, err)
	require.Len(t, upstream.requests, 1)
	require.Contains(t, recorder.Body.String(), "concurrency_queued")
}

func TestIsAccountTestUpstreamQueued429(t *testing.T) {
	require.True(t, isAccountTestUpstreamQueued429(http.StatusTooManyRequests, []byte(accountTestQueued429Body)))
	// 非 JSON 正文也要认得出来（部分中转直接回纯文本）。
	require.True(t, isAccountTestUpstreamQueued429(http.StatusTooManyRequests, []byte("All concurrency slots are busy, retry later")))
	require.False(t, isAccountTestUpstreamQueued429(http.StatusTooManyRequests, []byte(`{"error":{"type":"usage_limit_reached"}}`)))
	// 状态码不是 429 时，正文再像排队也不算。
	require.False(t, isAccountTestUpstreamQueued429(http.StatusOK, []byte(accountTestQueued429Body)))
}

func TestAccountTestQueueRetryDelay(t *testing.T) {
	// Retry-After 优先于正文里的文字提示。
	headers := http.Header{"Retry-After": []string{"3"}}
	require.Equal(t, 3*time.Second, accountTestQueueRetryDelay(headers, []byte(accountTestQueued429Body)))

	// 没有 Retry-After 时用上游正文给的 "retry in about N seconds"。
	require.Equal(t, 5*time.Second, accountTestQueueRetryDelay(http.Header{}, []byte(accountTestQueued429Body)))

	// 两者都没有时回落到基础延迟，且被下限抬起来。
	require.Equal(t, accountTestQueueRetryMinDelay, accountTestQueueRetryDelay(http.Header{}, []byte(`{"error":{"code":"concurrency_queued"}}`)))

	// 上游给的超长间隔要被上限夹住，免得单次测试挂死。
	require.Equal(t, accountTestQueueRetryMaxDelay, accountTestQueueRetryDelay(http.Header{}, []byte(`{"error":{"code":"concurrency_queued","message":"retry in about 600 seconds"}}`)))
}
