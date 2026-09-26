//go:build unit

package service

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestKeepaliveShortRequestUsesAccountClientAndProxyRoute(t *testing.T) {
	for _, tc := range []struct {
		name, platform, kind string
		chat                 bool
	}{
		{"responses_apikey", PlatformOpenAI, AccountTypeAPIKey, false},
		{"responses_oauth", PlatformOpenAI, AccountTypeOAuth, false},
		{"chat", PlatformOpenAI, AccountTypeAPIKey, true},
		{"claude", PlatformAnthropic, AccountTypeAPIKey, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := &Account{ID: 8, Platform: tc.platform, Type: tc.kind, Concurrency: 1,
				Credentials: map[string]any{"api_key": "test-key", "access_token": "test-token", "base_url": "https://upstream.example"},
				Extra:       map[string]any{openai_compat.ExtraKeyResponsesSupported: !tc.chat}}
			response := adaptiveCNResponsesTestResponse()
			model := "gpt-6-astra"
			if tc.chat {
				response = adaptiveCNChatTestResponse()
			}
			if tc.platform == PlatformAnthropic {
				response = adaptiveCNAnthropicTestResponse()
				model = "claude-sonnet-4-6"
			}
			proxyID := int64(12)
			account.ProxyID = &proxyID
			account.Proxy = &Proxy{ID: proxyID, Protocol: "http", Host: "proxy.example", Port: 3128, Status: StatusActive}
			account.Credentials["model_mapping"] = map[string]any{"keepalive-alias": model}
			svc, upstream := adaptiveCNAccountTestService(account, response)
			result, err := svc.RunKeepaliveBackground(context.Background(), 8, "keepalive-alias")
			require.NoError(t, err)
			require.Equal(t, "success", result.Status)
			require.Len(t, upstream.requests, 1)
			body := upstream.lastBody
			require.Equal(t, "http://proxy.example:3128", upstream.lastProxyURL)
			require.Equal(t, model, gjson.GetBytes(body, "model").String())
			if tc.platform == PlatformAnthropic {
				require.Equal(t, "test-key", upstream.lastReq.Header.Get("X-Api-Key"))
			} else if tc.kind == AccountTypeOAuth {
				require.Equal(t, "Bearer test-token", upstream.lastReq.Header.Get("Authorization"))
			} else {
				require.Equal(t, "Bearer test-key", upstream.lastReq.Header.Get("Authorization"))
			}
			require.Contains(t, string(body), keepalivePrompt)
			require.Equal(t, 200, result.HTTPStatus)
			if tc.platform == PlatformAnthropic {
				require.Equal(t, int64(128), gjson.GetBytes(body, "max_tokens").Int())
			} else if tc.chat {
				require.Equal(t, int64(128), gjson.GetBytes(body, "max_completion_tokens").Int())
			} else {
				require.Equal(t, "low", gjson.GetBytes(body, "reasoning.effort").String())
				require.NotEmpty(t, gjson.GetBytes(body, "tools").Array())
				require.Equal(t, tc.kind != AccountTypeOAuth, gjson.GetBytes(body, "max_output_tokens").Exists())
				require.Equal(t, "reasoning.encrypted_content", gjson.GetBytes(body, "include.0").String())
			}
		})
	}
}

func TestKeepaliveQueued429ReturnsToSchedulerWithUpstreamDelay(t *testing.T) {
	account := newAccountTestQueuedAccount(8)
	response := newJSONResponse(429, accountTestQueued429Body)
	response.Header.Set("Retry-After", "120")
	svc, upstream := adaptiveCNAccountTestService(account, response)
	result, err := svc.RunKeepaliveBackground(context.Background(), 8, "gpt-6-astra")
	require.NoError(t, err)
	require.Equal(t, "failed", result.Status)
	require.Len(t, upstream.requests, 1)
	require.Equal(t, 429, result.HTTPStatus)
	require.Equal(t, 120*time.Second, result.RetryAfter)
	require.Contains(t, result.ErrorMessage, "concurrency_queued")
}

func TestKeepaliveRejectsEmptyAndTruncatedResponses(t *testing.T) {
	for _, body := range []string{"", "data: {\"type\":\"response.completed\"}\n\n", "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n"} {
		response := newJSONResponse(200, "")
		response.Body = io.NopCloser(strings.NewReader(body))
		svc, _ := adaptiveCNAccountTestService(newAccountTestQueuedAccount(8), response)
		result, err := svc.RunKeepaliveBackground(context.Background(), 8, "gpt-6-astra")
		require.NoError(t, err)
		require.Equal(t, "failed", result.Status)
		require.NotEmpty(t, result.ErrorMessage)
	}
}

func TestKeepaliveRejectsMediaAliasBeforeSending(t *testing.T) {
	account := newAccountTestQueuedAccount(8)
	account.Credentials["model_mapping"] = map[string]any{"alias": "gpt-image-2"}
	svc, upstream := adaptiveCNAccountTestService(account, &http.Response{})
	result, err := svc.RunKeepaliveBackground(context.Background(), 8, "alias")
	require.NoError(t, err)
	require.Equal(t, "failed", result.Status)
	require.Empty(t, upstream.requests)
}

func TestKeepaliveHonorsCodexQuotaResetHeaders(t *testing.T) {
	response := newJSONResponse(429, `{"error":{"message":"quota exceeded"}}`)
	response.Header.Set("X-Codex-Primary-Used-Percent", "100")
	response.Header.Set("X-Codex-Primary-Window-Minutes", "300")
	response.Header.Set("X-Codex-Primary-Reset-After-Seconds", "7200")
	svc, upstream := adaptiveCNAccountTestService(newAccountTestQueuedAccount(8), response)
	result, err := svc.RunKeepaliveBackground(context.Background(), 8, "gpt-6-astra")
	require.NoError(t, err)
	require.Len(t, upstream.requests, 1)
	require.InDelta(t, float64(2*time.Hour), float64(result.RetryAfter), float64(time.Second))
}

func TestKeepaliveGeminiRequiresProtocolCompletion(t *testing.T) {
	for _, tc := range []struct{ body, status string }{
		{`data: {"candidates":[{"content":{"parts":[{"text":"partial"}]}}]}` + "\n\n", "failed"},
		{`data: {"candidates":[{"content":{"parts":[{"text":"OK"}]},"finishReason":"STOP"}]}` + "\n\n", "success"},
	} {
		account := &Account{ID: 8, Platform: PlatformGemini, Type: AccountTypeAPIKey, Concurrency: 1, Credentials: map[string]any{"api_key": "test-key", "base_url": "https://upstream.example"}}
		response := newJSONResponse(200, tc.body)
		svc, _ := adaptiveCNAccountTestService(account, response)
		result, err := svc.RunKeepaliveBackground(context.Background(), 8, "gemini-2.5-flash")
		require.NoError(t, err)
		require.Equal(t, tc.status, result.Status)
	}
}

func TestKeepaliveHonorsOtherQuotaResetHintsAndPreservesBody(t *testing.T) {
	for _, tc := range []struct{ name, body, header, value string }{
		{"anthropic", `{"error":{"message":"rate limited"}}`, "anthropic-ratelimit-unified-reset", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)},
		{"gemini", `{"error":{"details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"300s"}]}}`, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newTestContext()
			state := &accountKeepaliveState{}
			c.Set(accountKeepaliveContextKey, state)
			resp := newJSONResponse(429, tc.body)
			if tc.header != "" {
				resp.Header.Set(tc.header, tc.value)
			}
			observeKeepaliveResponse(c, resp)
			expected := time.Hour
			if tc.name == "gemini" {
				expected = 5 * time.Minute
			}
			require.InDelta(t, float64(expected), float64(state.retryAfter), float64(2*time.Second))
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.Equal(t, tc.body, string(body))
		})
	}
}
