//go:build unit

package service

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/codexhistory"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestCodexHistoryAccountOverridesReachUpstream(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		for _, tc := range []struct {
			name     string
			gateway  bool
			override any
			filtered bool
		}{
			{"inherit_off", false, nil, false},
			{"inherit_on", true, nil, true},
			{"account_on", false, true, true},
			{"account_off", true, false, false},
		} {
			t.Run(tc.name+map[bool]string{false: "/normal", true: "/passthrough"}[passthrough], func(t *testing.T) {
				account := &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true,
					Credentials: map[string]any{"api_key": "test", "base_url": "https://api.example.invalid"},
					Extra:       map[string]any{"openai_passthrough": passthrough, CodexHistoryFilterAccountExtraKey: tc.override}}
				upstream := &retry429Upstream{t: t, repo: &anthropicWindowLimitRepo{}, accountID: 7}
				upstream.respond = func(_ int, request *http.Request) *http.Response {
					body, err := io.ReadAll(request.Body)
					require.NoError(t, err)
					if !strings.Contains(gjson.GetBytes(body, "include").String(), "reasoning.encrypted_content") {
						return &http.Response{StatusCode: 400, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"invalid codex request","code":"invalid_responses_request"}}`))}
					}
					require.Equal(t, !tc.filtered, strings.Contains(string(body), "foreign-ciphertext"))
					require.Equal(t, !tc.filtered, gjson.GetBytes(body, "input.0.id").Exists())
					require.Equal(t, "medium", gjson.GetBytes(body, "reasoning.effort").String())
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"resp_ok","status":"completed","model":"gpt-6-astra","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1}}`))}
				}
				svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}, httpUpstream: upstream}
				c, _ := newTransportFailoverTestContext(t)
				c.Set(CodexHistoryFilterRequestKey, &CodexHistoryFilterRequest{DefaultEnabled: tc.gateway})
				body := []byte(`{"model":"gpt-6-astra","instructions":"test","store":false,"stream":false,"reasoning":{"effort":"medium"},"include":["reasoning.encrypted_content"],"input":[{"type":"message","id":"msg_old","role":"user","content":"hello"},{"type":"reasoning","encrypted_content":"foreign-ciphertext"}]}`)
				_, err := svc.Forward(context.Background(), c, account, body)
				require.NoError(t, err)
				require.Equal(t, 1, upstream.calls)
			})
		}
	}
}

func TestCodexHistoryAccountRetryKeepsOriginalAndGatewayDefault(t *testing.T) {
	c, _ := newTransportFailoverTestContext(t)
	state := &CodexHistoryFilterRequest{DefaultEnabled: true}
	c.Set(CodexHistoryFilterRequestKey, state)
	body := []byte(`{"input":[{"type":"message","id":"msg_original","role":"user","content":"hello"},{"type":"reasoning","encrypted_content":"foreign"}]}`)
	before := string(body)
	enabled := &Account{Platform: PlatformOpenAI, Extra: map[string]any{CodexHistoryFilterAccountExtraKey: true}}
	disabled := &Account{Platform: PlatformOpenAI, Extra: map[string]any{CodexHistoryFilterAccountExtraKey: false}}
	ctx, filtered, err := ApplyCodexHistoryFilterForAccount(context.Background(), c, enabled, body)
	require.NoError(t, err)
	require.NotContains(t, string(filtered), "foreign")
	ctx, unchanged, err := ApplyCodexHistoryFilterForAccount(ctx, c, disabled, body)
	require.NoError(t, err)
	require.False(t, codexhistory.Enabled(ctx))
	require.Equal(t, before, string(unchanged))
	ctx, filtered, err = ApplyCodexHistoryFilterForAccount(ctx, c, &Account{Platform: PlatformOpenAI}, body)
	require.NoError(t, err)
	require.True(t, codexhistory.Enabled(ctx), "an earlier account override must not replace the gateway default")
	require.NotContains(t, string(filtered), "foreign")
	require.Equal(t, before, string(body))
	require.Equal(t, 1, state.Filtered.RemovedReasoningItems)
}

func TestCodexHistoryAccountDoesNotFilterOtherProtocols(t *testing.T) {
	c, _ := newTransportFailoverTestContext(t)
	account := &Account{Platform: PlatformOpenAI, Extra: map[string]any{CodexHistoryFilterAccountExtraKey: true}}
	body := []byte(`{"previous_response_id":"original"}`)
	_, actual, err := ApplyCodexHistoryFilterForAccount(context.Background(), c, account, body)
	require.NoError(t, err)
	require.Equal(t, body, actual)
}

func TestCodexHistoryAccountSettingValidation(t *testing.T) {
	for _, value := range []any{nil, true, false} {
		require.NoError(t, ValidateCodexHistoryFilterAccountExtra(PlatformOpenAI, map[string]any{CodexHistoryFilterAccountExtraKey: value}))
	}
	for _, value := range []any{"false", 0, []bool{true}} {
		extra := map[string]any{CodexHistoryFilterAccountExtraKey: value}
		require.Error(t, ValidateCodexHistoryFilterAccountExtra(PlatformOpenAI, extra))
		_, err := buildAccountForCreate(&CreateAccountInput{Platform: PlatformOpenAI}, extra)
		require.Error(t, err)
	}
}
