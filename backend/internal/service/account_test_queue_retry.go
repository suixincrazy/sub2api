package service

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// 账号测试的「上游并发排队」重试。
//
// 真实转发链的每个出口都包在 retryUpstream429 里，管理端账号测试却是裸调上游后
// 直接 sendErrorAndEnd。中转上游（如 jianzhile）在并发槽满时回
// 429 {"error":{"code":"concurrency_queued","message":"All concurrency slots are busy.
// You are number N of M in the queue, please retry in about 5 seconds"}}，
// 这是请求级排队而不是账号配额：按上游给的间隔重发，排到了就会被放行。
// 不重发时测试恒报 429，与该账号在网关上的真实可用性相反。
//
// 只认明确的排队信号；usage_limit_reached 等配额型 429 仍走原有的一次失败 +
// reconcileOpenAI429State 路径，不在这里空转。
var (
	accountTestQueueRetryBudget   = 120 * time.Second
	accountTestQueueRetryMinDelay = time.Second
	accountTestQueueRetryMaxDelay = 10 * time.Second
)

var accountTestQueueRetryAfterPattern = regexp.MustCompile(`(?i)retry in about (\d+) seconds?`)

func isAccountTestUpstreamQueued429(statusCode int, body []byte) bool {
	if statusCode != http.StatusTooManyRequests {
		return false
	}
	if gjson.ValidBytes(body) && strings.EqualFold(gjson.GetBytes(body, "error.code").String(), "concurrency_queued") {
		return true
	}
	return strings.Contains(strings.ToLower(string(body)), "concurrency slots are busy")
}

func accountTestQueueRetryDelay(headers http.Header, body []byte) time.Duration {
	delay := openAIOAuth429SameAccountRetryDelay(headers, time.Time{})
	if headers.Get("Retry-After") == "" {
		if match := accountTestQueueRetryAfterPattern.FindSubmatch(body); len(match) == 2 {
			if seconds, err := strconv.Atoi(string(match[1])); err == nil {
				delay = time.Duration(seconds) * time.Second
			}
		}
	}
	if delay < accountTestQueueRetryMinDelay {
		delay = accountTestQueueRetryMinDelay
	}
	if delay > accountTestQueueRetryMaxDelay {
		delay = accountTestQueueRetryMaxDelay
	}
	return delay
}

// doAccountTestUpstreamWithQueueRetry 发送测试请求；上游回排队型 429 时在同一账号上
// 按上游建议间隔重发，直到放行、出现其它状态或耗尽墙钟预算。返回的响应体总是可读的
// （排队型 429 用尽预算时会把已读出的正文装回去），调用方的错误处理保持不变。
// req 须能经 GetBody 重放（bytes.Reader 构造的请求天然满足）。
func (s *AccountTestService) doAccountTestUpstreamWithQueueRetry(
	c *gin.Context,
	req *http.Request,
	account *Account,
	do func(*http.Request) (*http.Response, error),
) (*http.Response, error) {
	ctx := req.Context()
	deadline := time.Now().Add(accountTestQueueRetryBudget)
	attempt := req
	for retry := 0; ; retry++ {
		resp, err := do(attempt)
		if err != nil || resp == nil || resp.StatusCode != http.StatusTooManyRequests {
			return resp, err
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
		_ = resp.Body.Close()
		resp.Body = io.NopCloser(strings.NewReader(string(body)))
		if !isAccountTestUpstreamQueued429(resp.StatusCode, body) || req.GetBody == nil {
			return resp, nil
		}
		delay := accountTestQueueRetryDelay(resp.Header, body)
		if time.Now().Add(delay).After(deadline) {
			return resp, nil
		}
		accountID := int64(0)
		if account != nil {
			accountID = account.ID
		}
		slog.WarnContext(ctx, "account_test_upstream_queued_retry", "account_id", accountID, "retry", retry+1, "delay_ms", delay.Milliseconds())
		s.sendEvent(c, TestEvent{Type: "content", Text: fmt.Sprintf("Upstream queued (429 concurrency_queued), retry %d in %ds: %s\n", retry+1, int(delay/time.Second), strings.TrimSpace(extractUpstreamErrorMessage(body)))})
		if waitErr := sleepWithContext(ctx, delay); waitErr != nil {
			return resp, nil
		}
		next, err := replayAccountTestRequest(ctx, req)
		if err != nil {
			return resp, nil
		}
		attempt = next
	}
}

func replayAccountTestRequest(ctx context.Context, req *http.Request) (*http.Request, error) {
	body, err := req.GetBody()
	if err != nil {
		return nil, err
	}
	next := req.Clone(ctx)
	next.Body = body
	return next, nil
}
