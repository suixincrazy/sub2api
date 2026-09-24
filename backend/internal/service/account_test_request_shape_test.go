//go:build unit

package service

import (
	"io"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestAccountTestRequestShape(t *testing.T) {
	for _, tc := range []struct {
		name, platform, kind, model string
		chat                        bool
	}{
		{"claude_apikey", PlatformAnthropic, AccountTypeAPIKey, "claude-opus-5-5", false},
		{"claude_oauth", PlatformAnthropic, AccountTypeOAuth, "claude-opus-5-5", false},
		{"claude_setup", PlatformAnthropic, AccountTypeSetupToken, "claude-opus-5-5", false},
		{"gpt_responses_apikey", PlatformOpenAI, AccountTypeAPIKey, "gpt-6-astra", false},
		{"gpt_responses_oauth", PlatformOpenAI, AccountTypeOAuth, "gpt-6-astra", false},
		{"gpt_responses_setup", PlatformOpenAI, AccountTypeSetupToken, "gpt-6-astra", false},
		{"gpt_chat_apikey", PlatformOpenAI, AccountTypeAPIKey, "gpt-6-astra", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := &Account{ID: 970, Platform: tc.platform, Type: tc.kind, Concurrency: 1,
				Credentials: map[string]any{"api_key": "upstream-key", "access_token": "upstream-token", "base_url": "https://upstream.example"},
				Extra:       map[string]any{openai_compat.ExtraKeyResponsesSupported: !tc.chat}}
			if tc.kind == AccountTypeAPIKey {
				account.Credentials[credKeyHeaderOverrideEnabled] = true
				account.Credentials[credKeyHeaderOverrides] = map[string]any{
					"x-test-route":   "configured-route",
					"anthropic-beta": claude.APIKeyBetaHeader + "," + claude.BetaContext1M,
				}
			}
			response := adaptiveCNResponsesTestResponse()
			if tc.platform == PlatformAnthropic {
				response = adaptiveCNAnthropicTestResponse()
			} else if tc.chat {
				response = adaptiveCNChatTestResponse()
			}
			svc, upstream := adaptiveCNAccountTestService(account, response)
			c, recorder := newTestContext()
			c.Request.Header.Set("Authorization", "Bearer admin-login-token")
			c.Request.Header.Set("Cookie", "panel-session=secret")
			c.Request.Header.Set("User-Agent", "browser-user-agent")
			c.Request.Header.Set("x-api-key", "admin-panel-key")
			const prompt = "Reply with exactly: wire-check"
			require.NoError(t, svc.TestAccountConnection(c, account.ID, tc.model, prompt, AccountTestModeDefault))
			require.Len(t, upstream.requests, 1)
			req, body := upstream.requests[0], upstream.lastBody
			require.Equal(t, "application/json", getHeaderRaw(req.Header, "Content-Type"))
			require.Equal(t, "text/event-stream", getHeaderRaw(req.Header, "Accept"))
			require.Empty(t, getHeaderRaw(req.Header, "Cookie"))
			require.NotContains(t, getHeaderRaw(req.Header, "Authorization"), "admin-login-token")
			require.NotContains(t, getHeaderRaw(req.Header, "x-api-key"), "admin-panel-key")
			require.NotEqual(t, "browser-user-agent", getHeaderRaw(req.Header, "User-Agent"))
			require.True(t, gjson.GetBytes(body, "stream").Bool())
			require.Contains(t, recorder.Body.String(), `"success":true`)
			if tc.kind == AccountTypeAPIKey {
				require.Equal(t, "configured-route", getHeaderRaw(req.Header, "x-test-route"))
			}
			if tc.platform == PlatformAnthropic {
				require.Equal(t, prompt, gjson.GetBytes(body, "messages.0.content.0.text").String())
				require.Equal(t, "2023-06-01", getHeaderRaw(req.Header, "Anthropic-Version"))
				require.Contains(t, getHeaderRaw(req.Header, "User-Agent"), "claude-cli/")
				parsed := ParseMetadataUserID(gjson.GetBytes(body, "metadata.user_id").String())
				require.NotNil(t, parsed)
				require.Equal(t, parsed.SessionID, getHeaderRaw(req.Header, "X-Claude-Code-Session-Id"))
				require.NotEmpty(t, getHeaderRaw(req.Header, "X-Claude-Code-Session-Id"))
				require.NotEmpty(t, gjson.GetBytes(body, "tools").Array(), "Claude tests need the client tool declarations")
				require.Equal(t, "none", gjson.GetBytes(body, "tool_choice.type").String())
				if tc.kind == AccountTypeAPIKey {
					require.Contains(t, getHeaderRaw(req.Header, "Anthropic-Beta"), claude.BetaContext1M)
					require.Equal(t, "upstream-key", getHeaderRaw(req.Header, "X-Api-Key"))
				} else {
					require.Equal(t, "Bearer upstream-token", getHeaderRaw(req.Header, "Authorization"))
				}
			} else {
				require.NotEmpty(t, getHeaderRaw(req.Header, "User-Agent"))
				require.NotEmpty(t, getHeaderRaw(req.Header, "Originator"))
				require.NotEmpty(t, getHeaderRaw(req.Header, "Version"))
				require.NotEmpty(t, getHeaderRaw(req.Header, "X-Codex-Window-ID"))
				require.NotEmpty(t, gjson.GetBytes(body, "tools").Array(), "GPT tests need protocol-appropriate tool declarations")
				require.Equal(t, "none", gjson.GetBytes(body, "tool_choice").String())
				if tc.chat {
					require.Equal(t, prompt, gjson.GetBytes(body, "messages.0.content").String())
					require.Equal(t, "function", gjson.GetBytes(body, "tools.0.type").String())
					require.NotEmpty(t, gjson.GetBytes(body, "tools.0.function.name").String())
				} else {
					require.Equal(t, prompt, gjson.GetBytes(body, "input.0.content.0.text").String())
					require.Equal(t, "function", gjson.GetBytes(body, "tools.0.type").String())
					require.NotEmpty(t, gjson.GetBytes(body, "tools.0.name").String())
				}
			}
		})
	}
}

func TestAccountTestClaudeDoesNotReportSuccessForEmptyOrTruncatedStream(t *testing.T) {
	for _, body := range []string{"", "<html>gateway error</html>", "data: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"partial\"}}\n\n"} {
		c, recorder := newTestContext()
		svc := &AccountTestService{}
		err := svc.processClaudeStream(c, strings.NewReader(body))
		require.Error(t, err)
		require.NotContains(t, recorder.Body.String(), `"success":true`)
	}
}

func TestAccountTestClaudeHandlesFinalUnterminatedEvent(t *testing.T) {
	c, recorder := newTestContext()
	svc := &AccountTestService{}
	require.NoError(t, svc.processClaudeStream(c, io.NopCloser(strings.NewReader("data: {\"type\":\"message_stop\"}"))))
	require.Contains(t, recorder.Body.String(), `"success":true`)
}
