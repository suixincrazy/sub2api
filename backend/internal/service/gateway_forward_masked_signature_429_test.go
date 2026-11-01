//go:build unit

package service

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// 复刻 anyrouter 现场：签名验不过时回的不是 400，而是这个 429。
const maskedSignature429Body = `{"error":{"message":"Service Unavailable","type":"error"},"type":"error"}`

const maskedSignatureThinkingHistoryBody = `{"model":"claude-sonnet-4-5-20250929","max_tokens":64,"thinking":{"type":"adaptive"},"messages":[` +
	`{"role":"user","content":"read data.txt"},` +
	`{"role":"assistant","content":[{"type":"thinking","thinking":"need to read","signature":"c2lnbmVkLWJ5LWFub3RoZXItdXBzdHJlYW0="},{"type":"tool_use","id":"toolu_1","name":"Read","input":{"file_path":"data.txt"}}]},` +
	`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"apple=42"}]}]}`

func maskedSignatureResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader([]byte(body))),
	}
}

func newMaskedSignature429Service(upstream *httpUpstreamSequenceRecorder) *GatewayService {
	return &GatewayService{
		cfg:            transportFailoverTestConfig(),
		httpUpstream:   upstream,
		settingService: NewSettingService(&settingRepoStub{values: map[string]string{}}, nil),
	}
}

func maskedSignatureParsed(body string) *ParsedRequest {
	return &ParsedRequest{Body: NewRequestBodyRef([]byte(body)), Model: "claude-sonnet-4-5-20250929"}
}

func TestForward_MaskedSignature429_RetriesWithThinkingDowngraded(t *testing.T) {
	c, _ := newTransportFailoverTestContext(t)
	upstream := &httpUpstreamSequenceRecorder{responses: []*http.Response{
		maskedSignatureResponse(http.StatusTooManyRequests, maskedSignature429Body),
		maskedSignatureResponse(http.StatusOK, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-4-5-20250929","content":[{"type":"text","text":"42"}],"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":1}}`),
	}}
	svc := newMaskedSignature429Service(upstream)

	result, err := svc.Forward(context.Background(), c, newAnthropicRegularAPIKeyAccount(), maskedSignatureParsed(maskedSignatureThinkingHistoryBody))

	require.NoError(t, err)
	require.NotNil(t, result)
	require.Len(t, upstream.bodies, 2, "必须且只多发一次降级重发")
	require.Contains(t, string(upstream.bodies[0]), `"type":"thinking"`)
	require.NotContains(t, string(upstream.bodies[1]), `"type":"thinking"`, "重发必须去掉 thinking 块")
	require.Contains(t, string(upstream.bodies[1]), `"tool_use"`)
}

func maskedSignature429Responses(n int) []*http.Response {
	out := make([]*http.Response, n)
	for i := range out {
		out[i] = maskedSignatureResponse(http.StatusTooManyRequests, maskedSignature429Body)
	}
	return out
}

func TestForward_MaskedSignature429_RetryStillFailingKeepsOriginal429(t *testing.T) {
	c, _ := newTransportFailoverTestContext(t)
	upstream := &httpUpstreamSequenceRecorder{responses: maskedSignature429Responses(upstream429MaxRetries + 2)}
	svc := newMaskedSignature429Service(upstream)

	_, err := svc.Forward(context.Background(), c, newAnthropicRegularAPIKeyAccount(), maskedSignatureParsed(maskedSignatureThinkingHistoryBody))

	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Equal(t, http.StatusTooManyRequests, failoverErr.StatusCode)
	// 原样 1 发 + 降级 1 发，之后 6 次同号重发都直接发降级 body，不再重发坏签名。
	require.Len(t, upstream.bodies, upstream429MaxRetries+2)
	require.Contains(t, string(upstream.bodies[0]), `"type":"thinking"`)
	for i, b := range upstream.bodies[1:] {
		require.NotContains(t, string(b), `"type":"thinking"`, "第 %d 次重发仍带 thinking", i+1)
	}
}

func TestForward_MaskedSignature429_NoThinkingHistoryNoExtraRequest(t *testing.T) {
	c, _ := newTransportFailoverTestContext(t)
	upstream := &httpUpstreamSequenceRecorder{responses: maskedSignature429Responses(upstream429MaxRetries + 1)}
	svc := newMaskedSignature429Service(upstream)

	_, err := svc.Forward(context.Background(), c, newAnthropicRegularAPIKeyAccount(),
		maskedSignatureParsed(`{"model":"claude-sonnet-4-5-20250929","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`))

	require.Error(t, err)
	// 只有原有的 1+6 次同号重发，没有降级引起的额外请求。
	require.Len(t, upstream.bodies, upstream429MaxRetries+1, "没有 thinking 历史就不该多发")
}

func TestShouldRectifyMaskedSignature429(t *testing.T) {
	svc := newMaskedSignature429Service(&httpUpstreamSequenceRecorder{})
	ctx := context.Background()
	apiKey := newAnthropicRegularAPIKeyAccount()
	model := "claude-sonnet-4-5-20250929"

	require.True(t, svc.shouldRectifyMaskedSignature429(ctx, apiKey, []byte(maskedSignature429Body), model))
	require.False(t, svc.shouldRectifyMaskedSignature429(ctx, apiKey, []byte(`{"type":"error","error":{"type":"rate_limit_error","message":"Number of request tokens has exceeded your per-minute rate limit"}}`), model),
		"真实限流 429 不能被当成签名错误")
	oauth := newAnthropicRegularAPIKeyAccount()
	oauth.Type = AccountTypeOAuth
	require.False(t, svc.shouldRectifyMaskedSignature429(ctx, oauth, []byte(maskedSignature429Body), model))

	disabled := NewSettingService(&settingRepoStub{values: map[string]string{SettingKeyRectifierSettings: `{"enabled":true,"thinking_signature_enabled":false}`}}, nil)
	svc.settingService = disabled
	require.False(t, svc.shouldRectifyMaskedSignature429(ctx, apiKey, []byte(maskedSignature429Body), model))
}
