//go:build unit

package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 这两条用例锚定 2026-09-09 20:45:09 与 2026-09-11 11:51:16 两次线上断流。
// 两者是同一个设计缺陷的两个方向：丢弃代价预算量错了对象、且检查得太晚。

// 2026-09-09 20:45:09 的回归用例：HTTP 错误与 429 重试的墙钟不得计入丢弃代价预算。
//
// 线上链条（ops_error_logs id=23352）：
//
//	20:41:44.193  账号 14 短回合 prose=304 out=136 -> discarded，换号（正确）
//	20:42:12.383  账号 10 起 8 连 429 daily_free_credits_exhausted
//	20:44:58.851  账号 10 的 429 风暴结束，共 166.5 秒
//	20:45:09.591  账号 13 短回合 prose=236 out=121 -> **delivered**（用户看到断流）
//
// 账号 13 那一发与账号 14 形态几乎相同，却因为预算被 429 风暴烧穿而只能放行。预算量的应当是
// 「为丢弃重来花了多少墙钟」，429/502 这类 HTTP 失败根本没产生可丢弃的内容，不该记账。
//
// 用例刻意让那段「白烧的墙钟」由真实的 429 尝试产生，而不是手工前推锚点：退款是被测代码
// 自己的记账动作，如果测试直接把状态摆成「已退款」，那就是用结论证明结论——生产代码删掉
// defer 里的退款也照样绿。所以这里只压缩预算量级（45s -> 1.2s，线上 166.5s 的等比缩影），
// 让 429 那一发的真实耗时足以烧穿未退款的预算，红绿完全由生产记账决定。
func TestAnthropicDiscardBudgetExcludesHTTPFailureWallClock(t *testing.T) {
	svc, _ := newHoldbackTestGatewayService(t, nil, 15000)
	svc.cfg.Gateway.AnthropicHoldbackDiscardBudgetMs = 1200
	svc.rateLimitService = nil
	upstream := &anthropicSlowStatusUpstream{}
	svc.httpUpstream = upstream
	c, rec := newRefusalTestContext(t)
	requestBody := []byte(`{"model":"claude-opus-5","stream":true,"messages":[{"role":"user","content":"go on"}]}`)
	acct := newAnthropicAPIKeyAccountForTest()

	// 第一次尝试：账号 14 的短回合，被丢弃换号。这一次**应当**记账。
	upstream.next = func() (*http.Response, error) {
		return shortTurnSSE(strings.Repeat("a", 304), 136, false), nil
	}
	_, err := svc.forwardAnthropicAPIKeyPassthrough(context.Background(), c, acct,
		requestBody, "claude-opus-5", "claude-opus-5", true, time.Now())
	var failover *UpstreamFailoverError
	require.ErrorAs(t, err, &failover)
	require.Equal(t, 1, anthropicHoldbackDiscardsUsed(c))
	require.Empty(t, rec.Body.String())

	// 账号 10 的 429 风暴（线上 8 连、共 166.5 秒）。CustomErrorCodes 为空时
	// shouldRetryUpstreamError 恒 false，所以每次调用就是一次干净的 HTTP 失败尝试：
	// 没有任何内容被丢弃，它的墙钟必须被退回去。
	acct429 := newAnthropicAPIKeyAccountForTest()
	acct429.ID++
	for i := 0; i < 2; i++ {
		upstream.next = func() (*http.Response, error) {
			time.Sleep(700 * time.Millisecond)
			return &http.Response{
				StatusCode: http.StatusTooManyRequests,
				Header:     http.Header{},
				Body: io.NopCloser(strings.NewReader(
					`{"type":"error","error":{"type":"rate_limit_error","message":"daily_free_credits_exhausted"}}`)),
			}, nil
		}
		_, err = svc.forwardAnthropicAPIKeyPassthrough(context.Background(), c, acct429,
			requestBody, "claude-opus-5", "claude-opus-5", true, time.Now())
		require.ErrorAs(t, err, &failover)
		require.Equal(t, http.StatusTooManyRequests, failover.StatusCode)
	}
	// 两发 429 合计约 1.4 秒，已经超过 1.2 秒预算；退款没记上的话下一发必然被迫 delivered。
	require.Equal(t, 1, anthropicHoldbackDiscardsUsed(c), "429 不得增加丢弃计数")

	// 账号 13 的短回合。预算不该被 429 风暴烧穿，所以仍须 discard。
	upstream.next = func() (*http.Response, error) {
		return shortTurnSSE(strings.Repeat("b", 236), 121, false), nil
	}
	acct13 := newAnthropicAPIKeyAccountForTest()
	acct13.ID += 2
	result, err := svc.forwardAnthropicAPIKeyPassthrough(context.Background(), c, acct13,
		requestBody, "claude-opus-5", "claude-opus-5", true, time.Now())
	require.ErrorAs(t, err, &failover,
		"429 风暴的墙钟不得吃掉丢弃预算：账号 13 这一发必须继续换号，不能 delivered")
	require.Nil(t, result)
	require.Empty(t, rec.Body.String(), "客户端必须仍是零暴露")
	require.Equal(t, 2, anthropicHoldbackDiscardsUsed(c))
}

// anthropicSlowStatusUpstream 每次调用交出 next 现做的响应，用来在一次用例里串起
// 「短回合 -> 429 -> 短回合」这条跨账号链，并且能给失败那一发注入真实耗时。
type anthropicSlowStatusUpstream struct {
	next func() (*http.Response, error)
}

func (u *anthropicSlowStatusUpstream) Do(req *http.Request, proxyURL string, accountID int64, accountConcurrency int) (*http.Response, error) {
	if req != nil && req.Body != nil {
		_, _ = io.Copy(io.Discard, req.Body)
		_ = req.Body.Close()
	}
	if u.next == nil {
		return nil, errors.New("anthropicSlowStatusUpstream: next 未设置")
	}
	return u.next()
}

func (u *anthropicSlowStatusUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, accountConcurrency int, profile *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxyURL, accountID, accountConcurrency)
}

// 2026-09-11 11:51:16 的回归用例：预算耗尽必须在尝试**中途**放行缓冲区，不能等回合结束。
//
// 线上链条（client_request_id 54ffcb1c-...）：
//
//	11:50:41.734  选中账号 13
//	11:50:59.143  账号 13 prose=1077 out=402 -> discarded（此刻仅 17.4s，预算没到点）
//	11:50:59.147  换到账号 7 开始透传
//	11:51:16.020  客户端断开，累计 34.3 秒零字节；45 秒预算永远等不到
//
// 关键：持流期 !streamCommitted，keepalive 分支直接 continue 不发 ping，所以这 34.3 秒
// 客户端收到的是绝对零字节。预算必须变成尝试内的实时放行截止线。
func TestAnthropicDiscardBudgetReleasesMidAttempt(t *testing.T) {
	svc, _ := newHoldbackTestGatewayService(t, nil, 15000)
	svc.cfg.Gateway.AnthropicHoldbackMaxHoldMs = 600000
	svc.cfg.Gateway.AnthropicHoldbackDeadAirBudgetMs = 600000
	svc.cfg.Gateway.AnthropicHoldbackLongThinkingHoldMs = 600000
	svc.cfg.Gateway.AnthropicHoldbackDiscardBudgetMs = 1500
	c, rec := newRefusalTestContext(t)

	// 已经丢弃过一次内容，且预算只剩 1.5 秒 —— 复现「第二发尝试跑到中途」的状态。
	noteAnthropicHoldbackDiscard(c)
	noteAnthropicDiscardBudgetStart(c, time.Now())

	// 一条一直在吐思考、迟迟不给 stop_reason 的流：旧实现会一路攥到 EOF，
	// 客户端零字节直到自己断开。
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{},
		Body: &pacedBody{events: []string{
			`data: {"type":"message_start","message":{"usage":{"input_tokens":129000,"output_tokens":1}}}`,
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
			fmt.Sprintf(`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":%q}}`, strings.Repeat("t", 40)),
			fmt.Sprintf(`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":%q}}`, strings.Repeat("t", 40)),
			fmt.Sprintf(`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":%q}}`, strings.Repeat("t", 40)),
			fmt.Sprintf(`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":%q}}`, strings.Repeat("t", 40)),
			fmt.Sprintf(`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":%q}}`, strings.Repeat("t", 40)),
			fmt.Sprintf(`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":%q}}`, strings.Repeat("t", 40)),
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":402}}`,
			`data: {"type":"message_stop"}`,
		}, gap: 400 * time.Millisecond}}

	// 断言量的必须是「客户端多久拿到第一个字节」，不是整流耗时：这条流本身要 9 帧 × 400ms
	// 才放完，中途放行与攥到 stop_reason 的总耗时都在 4 秒上下，而两者的 body 和 nil error
	// 完全一样。只有首字节时刻能把「修好了」和「没修」区分开。
	firstByte := &firstByteAtWriter{ResponseWriter: c.Writer}
	c.Writer = firstByte

	start := time.Now()
	_, err := svc.handleStreamingResponseAnthropicAPIKeyPassthrough(context.Background(), resp, c,
		&Account{ID: 7, Name: "100xlabs-anthropic", Platform: PlatformAnthropic}, start, "claude-opus-5")

	require.NoError(t, err)
	var failover *UpstreamFailoverError
	require.False(t, errors.As(err, &failover), "预算已耗尽，不得再为这一发换号")
	require.NotEmpty(t, rec.Body.String(), "预算耗尽后必须把缓冲区放给客户端")
	require.False(t, firstByte.at.IsZero(), "客户端必须真的收到过字节")
	// 预算剩 1.5 秒，放行应当发生在第 4~5 帧（1.6~2.0s）而不是 stop_reason 所在的第 8 帧
	// （3.2s）。取 3 秒作阈：既容得下调度抖动，也容不下「攥到 stop_reason」。
	require.Less(t, firstByte.at.Sub(start), 3*time.Second,
		"预算剩 1.5 秒就必须在尝试中途放行，不能一路攥到 stop_reason（线上 34.3 秒零字节后客户端自己断开）")
}

// firstByteAtWriter 记下第一次真正写给客户端的时刻。持流的效果只能从这个量上看出来：
// 「攥着」与「放行」在 body 内容和返回错误上都不可分辨，差别只在客户端何时拿到第一个字节。
type firstByteAtWriter struct {
	gin.ResponseWriter
	at time.Time
}

func (w *firstByteAtWriter) Write(data []byte) (int, error) {
	if w.at.IsZero() && len(data) > 0 {
		w.at = time.Now()
	}
	return w.ResponseWriter.Write(data)
}

func (w *firstByteAtWriter) WriteString(data string) (int, error) {
	if w.at.IsZero() && data != "" {
		w.at = time.Now()
	}
	return w.ResponseWriter.WriteString(data)
}
