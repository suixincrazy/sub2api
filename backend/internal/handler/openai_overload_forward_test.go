//go:build unit

package handler

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

type overloadForwardUpstream struct {
	service.HTTPUpstream
	accounts []int64
}

func (u *overloadForwardUpstream) Do(_ *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	u.accounts = append(u.accounts, accountID)
	if len(u.accounts) <= 6 {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(
			"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_test\"}}\n\n" +
				"data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial output\"}\n\n" +
				"data: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp_test\",\"error\":{\"code\":\"server_is_overloaded\",\"message\":\"Our servers are currently overloaded. Please try again later.\"}}}\n\n"))}, nil
	}
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_ok\",\"status\":\"completed\",\"model\":\"gpt-5.1\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]}]}}\n\n"))}, nil
}

func TestOpenAIOverloadForward_SemanticSSEFailureRecoversPriority(t *testing.T) {
	upstream := &overloadForwardUpstream{}
	handler := newOpenAIResponsesFailoverTestHandler(t, upstream)
	for i := 0; i < 8; i++ {
		c, rec := newOpenAIResponsesFailoverTestContext(t, nil)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.1","stream":true,"input":"hello"}`))
		c.Request.Header.Set("Content-Type", "application/json")
		handler.Responses(c)
		require.Len(t, upstream.accounts, i+1, "one upstream attempt per logical request")
		if i < 6 {
			require.Contains(t, rec.Body.String(), "overloaded")
		} else {
			require.Contains(t, rec.Body.String(), "response.completed")
		}
	}
	require.Equal(t, []int64{1, 1, 1, 1, 1, 1, 2, 1}, upstream.accounts)
}

func TestOpenAIOverloadForward_WSTerminalResultCountsOnce(t *testing.T) {
	h := &OpenAIGatewayHandler{gatewayService: &service.OpenAIGatewayService{}}
	group := int64(2)
	high := &service.Account{ID: 16, Platform: service.PlatformOpenAI, Priority: 0}
	low := &service.Account{ID: 1, Platform: service.PlatformOpenAI, Priority: 10}
	result := &service.OpenAIForwardResult{OpenAIWSMode: true, UpstreamTerminalEvent: "response.failed", UpstreamCapacityShed: true}
	for i := 0; i < 5; i++ {
		h.observeOpenAIOverloadResult(&group, high, "alias", result, nil)
	}
	require.False(t, h.gatewayService.ShouldReselectOpenAIAccountAfterOverload(&group, high, "alias"))
	h.observeOpenAIOverloadResult(&group, high, "alias", result, nil)
	require.True(t, h.gatewayService.ShouldReselectOpenAIAccountAfterOverload(&group, high, "alias"))
	h.observeOpenAIOverloadResult(&group, low, "alias", &service.OpenAIForwardResult{OpenAIWSMode: true, UpstreamTerminalEvent: "response.completed"}, nil)
	require.False(t, h.gatewayService.ShouldReselectOpenAIAccountAfterOverload(&group, high, "alias"))
	require.True(t, h.gatewayService.ShouldReselectOpenAIAccountAfterOverload(&group, low, "alias"))
}
