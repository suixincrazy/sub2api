//go:build unit

package service

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/codexhistory"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestCodexHistoryFilterSurvivesAccountTransformsAndRetries(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, kind := range []string{AccountTypeAPIKey, AccountTypeOAuth, AccountTypeSetupToken} {
		for _, passthrough := range []bool{false, true} {
			name := kind + "/normal"
			if passthrough {
				name = kind + "/passthrough"
			}
			t.Run(name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					repo := &anthropicWindowLimitRepo{}
					account := &Account{ID: 900, Platform: PlatformOpenAI, Type: kind, Status: StatusActive, Schedulable: true, Concurrency: 1,
						Credentials: map[string]any{"api_key": "test", "access_token": "test", "chatgpt_account_id": "test", "base_url": "https://api.example.invalid"},
						Extra:       map[string]any{"openai_passthrough": passthrough},
					}
					cfg := &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}
					rl := NewRateLimitService(repo, nil, cfg, nil, nil)
					upstream := &retry429Upstream{t: t, repo: repo, accountID: account.ID}
					upstream.respond = func(attempt int, request *http.Request) *http.Response {
						body, err := io.ReadAll(request.Body)
						require.NoError(t, err)
						require.Equal(t, gjson.False, gjson.GetBytes(body, "store").Type)
						require.Equal(t, "xhigh", gjson.GetBytes(body, "reasoning.effort").String())
						require.NotContains(t, string(body), "reasoning.encrypted_content")
						require.NotContains(t, string(body), "foreign-ciphertext")
						for _, item := range gjson.GetBytes(body, "input").Array() {
							require.NotEqual(t, "reasoning", item.Get("type").String())
							require.False(t, item.Get("id").Exists())
						}
						require.EqualValues(t, len(body), request.ContentLength)
						if attempt == 1 {
							return &http.Response{StatusCode: 429, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"error":{"type":"rate_limit_error","message":"slow down"}}`))}
						}
						response := `{"id":"resp_ok","object":"response","status":"completed","model":"gpt-5.4","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1}}`
						contentType := "application/json"
						if gjson.GetBytes(body, "stream").Bool() {
							contentType = "text/event-stream"
							response = "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":" + response + "}\n\n"
						}
						return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{contentType}}, Body: io.NopCloser(strings.NewReader(response))}
					}
					svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream, rateLimitService: rl}
					rl.SetAccountRuntimeBlocker(svc)
					c, _ := newTransportFailoverTestContext(t)
					ctx := codexhistory.WithEnabled(context.Background())
					c.Request = c.Request.WithContext(ctx)
					body := []byte(`{"model":"gpt-5.4","instructions":"test","store":true,"stream":false,"reasoning":{"effort":"xhigh"},"include":["reasoning.encrypted_content"],"input":[{"type":"message","id":"old-message","role":"user","content":[{"type":"input_text","text":"hello"}]},{"type":"reasoning","encrypted_content":"foreign-ciphertext"}]}`)
					_, err := svc.Forward(ctx, c, account, body)
					require.NoError(t, err)
					require.Equal(t, 2, upstream.calls, "retain the gateway's existing 429 retry policy")
					require.Zero(t, repo.rateLimitCalls)
				})
			})
		}
	}
}
