package service

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
)

// 405 是端点级故障（中转站没挂 POST /v1/messages，nginx 直接回 405 Not Allowed），
// 402 是中转站欠费。两者都与请求内容无关，必须换号；否则请求在
// handleErrorResponse 里被写成终态 502，handler 的 errors.As 匹配不到
// UpstreamFailoverError，低优先级档永远轮不到。
func TestGatewayServiceShouldFailoverUpstreamError_405And402AreFailoverEligible(t *testing.T) {
	svc := &GatewayService{}

	assert.True(t, svc.shouldFailoverUpstreamError(http.StatusMethodNotAllowed),
		"405 must trigger failover so a broken relay endpoint falls through to a lower-priority account")
	assert.True(t, svc.shouldFailoverUpstreamError(http.StatusPaymentRequired),
		"402 must trigger failover: an out-of-credit relay is an account-scoped condition")
}

func TestGatewayServiceShouldFailoverUpstreamError_Whitelist(t *testing.T) {
	svc := &GatewayService{}

	for _, code := range []int{401, 402, 403, 405, 429, 529, 500, 502, 503, 504} {
		assert.True(t, svc.shouldFailoverUpstreamError(code), "status %d should trigger failover", code)
	}

	// 400/404 由请求本身决定，换号只会把同一个错误重放 N 遍。
	for _, code := range []int{200, 201, 400, 404, 408, 422} {
		assert.False(t, svc.shouldFailoverUpstreamError(code), "status %d should NOT trigger failover", code)
	}
}

// 两条链路各有一份独立白名单，必须逐码一致，否则同一个上游状态码在
// Anthropic 侧和 OpenAI 侧会得到相反的处理。
func TestShouldFailoverUpstreamError_AnthropicAndOpenAIAgree(t *testing.T) {
	gw := &GatewayService{}
	oa := &OpenAIGatewayService{}

	for code := 200; code <= 599; code++ {
		assert.Equal(t, oa.shouldFailoverUpstreamError(code), gw.shouldFailoverUpstreamError(code),
			"status %d classified differently by the two gateways", code)
	}
}
