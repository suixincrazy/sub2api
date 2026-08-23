//go:build unit

package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// socksResetError 复刻线上现场：SOCKS 代理隧道被对端 reset，HTTP 往返从未完成，
// 因此没有任何上游状态码可依据。
//
//	Post "https://upstream.example.invalid/v1/messages?beta=true": socks connect tcp
//	192.0.2.10:3660->upstream.example.invalid:443: read tcp ...: read: connection reset by peer
func socksResetError() error {
	return errors.New(`Post "https://upstream.example.invalid/v1/messages?beta=true": socks connect tcp 192.0.2.10:3660->upstream.example.invalid:443: read tcp 192.0.2.2:60016->192.0.2.10:3660: read: connection reset by peer`)
}

func transportFailoverTestConfig() *config.Config {
	return &config.Config{
		Security: config.SecurityConfig{
			URLAllowlist: config.URLAllowlistConfig{Enabled: false},
		},
	}
}

// newAnthropicRegularAPIKeyAccount 是走**常规**转发链路的 Anthropic API Key 账号
// （extra.anthropic_passthrough 未开启），即 upstream.example.invalid 这类第三方中转的形态。
func newAnthropicRegularAPIKeyAccount() *Account {
	return &Account{
		ID:          9301,
		Name:        "anthropic-regular-third-party",
		Platform:    PlatformAnthropic,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":  "third-party-key",
			"base_url": "https://upstream.example.invalid",
		},
		Status:      StatusActive,
		Schedulable: true,
	}
}

func newTransportFailoverTestContext(t *testing.T) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	return c, rec
}

// TestForward_SocksResetTransportError_ReturnsFailoverError 钉住线上事故的核心行为：
// 常规 Anthropic 转发链路遇到传输层 reset 时，必须返回 *UpstreamFailoverError，
// 否则 handler 的 errors.As 匹配不到，会把这一发直接当终态返回给客户端——
// 也就是「代理被 reset 就 502，一个号都不换」。
func TestForward_SocksResetTransportError_ReturnsFailoverError(t *testing.T) {
	c, _ := newTransportFailoverTestContext(t)

	svc := &GatewayService{
		cfg:          transportFailoverTestConfig(),
		httpUpstream: &anthropicHTTPUpstreamRecorder{err: socksResetError()},
	}
	account := newAnthropicRegularAPIKeyAccount()
	parsed := &ParsedRequest{
		Body:  NewRequestBodyRef([]byte(`{"model":"claude-sonnet-4-5-20250929","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)),
		Model: "claude-sonnet-4-5-20250929",
	}

	result, err := svc.Forward(context.Background(), c, account, parsed)

	require.Nil(t, result)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr,
		"传输层 reset 必须包成 UpstreamFailoverError，否则 handler 不会换号；实际类型 %T", err)
	require.Equal(t, http.StatusBadGateway, failoverErr.StatusCode)
	require.True(t, failoverErr.ShouldRetryNextAccount(), "必须允许切换到下一个账号")
}

// TestForward_SocksResetTransportError_DoesNotWriteResponse 钉住第二个必要条件：
// service 层不得写响应。handler 依赖 c.Writer.Size() 未变化来判断可以安全 failover；
// 一旦这里写了 502，handler 会认为响应已提交而放弃换号。
func TestForward_SocksResetTransportError_DoesNotWriteResponse(t *testing.T) {
	c, rec := newTransportFailoverTestContext(t)

	svc := &GatewayService{
		cfg:          transportFailoverTestConfig(),
		httpUpstream: &anthropicHTTPUpstreamRecorder{err: socksResetError()},
	}
	account := newAnthropicRegularAPIKeyAccount()
	parsed := &ParsedRequest{
		Body:  NewRequestBodyRef([]byte(`{"model":"claude-sonnet-4-5-20250929","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)),
		Model: "claude-sonnet-4-5-20250929",
	}

	_, _ = svc.Forward(context.Background(), c, account, parsed)

	// gin responseWriter 未写入时 Size() 是 -1（noWritten），不是 0。
	require.Equal(t, -1, c.Writer.Size(),
		"service 层写响应会让 handler 判定响应已提交而放弃 failover")
	require.Empty(t, rec.Body.String())
}

// TestForward_TransportError_ClientCanceled_NoFailover 客户端断开时不得换号：
// 上游从未表现出故障，换号只会用已取消的 context 再失败一次。
func TestForward_TransportError_ClientCanceled_NoFailover(t *testing.T) {
	c, _ := newTransportFailoverTestContext(t)

	svc := &GatewayService{
		cfg:          transportFailoverTestConfig(),
		httpUpstream: &anthropicHTTPUpstreamRecorder{err: context.Canceled},
	}
	account := newAnthropicRegularAPIKeyAccount()
	parsed := &ParsedRequest{
		Body:  NewRequestBodyRef([]byte(`{"model":"claude-sonnet-4-5-20250929","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)),
		Model: "claude-sonnet-4-5-20250929",
	}

	_, err := svc.Forward(context.Background(), c, account, parsed)

	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.False(t, errors.As(err, &failoverErr), "客户端断开不应触发 failover")
	require.ErrorIs(t, err, context.Canceled)
}

// TestForwardBedrock_TransportError_ReturnsFailoverError Bedrock 链路同类分支。
func TestForwardBedrock_TransportError_ReturnsFailoverError(t *testing.T) {
	c, rec := newTransportFailoverTestContext(t)

	svc := &GatewayService{
		cfg:          transportFailoverTestConfig(),
		httpUpstream: &anthropicHTTPUpstreamRecorder{err: socksResetError()},
	}
	account := &Account{
		ID:          9302,
		Name:        "bedrock-apikey",
		Platform:    PlatformAnthropic,
		Type:        AccountTypeBedrock,
		Concurrency: 1,
		Credentials: map[string]any{
			"auth_mode": "apikey",
			"api_key":   "bedrock-key",
			"region":    "us-east-1",
		},
		Status:      StatusActive,
		Schedulable: true,
	}
	parsed := &ParsedRequest{
		Body:  NewRequestBodyRef([]byte(`{"model":"claude-sonnet-4-5-20250929","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)),
		Model: "claude-sonnet-4-5-20250929",
	}

	result, err := svc.forwardBedrock(context.Background(), c, account, parsed, time.Now())

	require.Nil(t, result)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr, "Bedrock 传输层错误也必须换号；实际类型 %T", err)
	require.Equal(t, http.StatusBadGateway, failoverErr.StatusCode)
	require.Equal(t, -1, c.Writer.Size())
	require.Empty(t, rec.Body.String())
}
