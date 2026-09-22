//go:build unit

package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	coderws "github.com/coder/websocket"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const openAIRejectedALBBody = "<html>\r\n<head><title>400 Bad Request</title></head>\r\n<body bgcolor=\"white\">\r\n<center><h1>400 Bad Request</h1></center>\r\n<hr><center>alb</center>\r\n</body>\r\n</html>"

func TestOpenAIRejectedRequestUses429RetryBudget(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, passthrough := range []bool{false, true} {
		for _, scenario := range []string{"alb_html", "generic_json", "empty_body", "stream_failed", "stream_error"} {
			t.Run(scenario+"/passthrough="+strconv.FormatBool(passthrough), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					repo := &anthropicWindowLimitRepo{}
					account := &Account{ID: 925, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
						Credentials: map[string]any{"api_key": "test", "base_url": "https://api.example.invalid"},
						Extra:       map[string]any{"openai_passthrough": passthrough}}
					cfg := &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}
					rl := NewRateLimitService(repo, nil, cfg, nil, nil)
					upstream := &retry429Upstream{t: t, repo: repo, accountID: account.ID}
					upstream.respond = func(_ int, _ *http.Request) *http.Response {
						status, body, contentType := 400, openAIRejectedALBBody, "text/html"
						switch scenario {
						case "generic_json":
							body = `{"error":{"type":"invalid_request_error","message":"Upstream rejected the request"}}`
							contentType = "application/json"
						case "empty_body":
							body = ""
							contentType = "application/json"
						case "stream_failed":
							status = 200
							contentType = "text/event-stream"
							body = "data: {\"type\":\"response.created\",\"response\":{\"id\":\"pending\"}}\n\ndata: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"type\":\"invalid_request_error\",\"message\":\"Upstream rejected the request\"}}}\n\n"
						case "stream_error":
							status = 200
							contentType = "text/event-stream"
							body = "data: {\"type\":\"error\",\"error\":{\"type\":\"invalid_request_error\",\"message\":\"Upstream rejected the request\"}}\n\n"
						}
						return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{contentType}, "Retry-After": []string{"1"}}, Body: io.NopCloser(strings.NewReader(body))}
					}
					svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream, rateLimitService: rl}
					rl.SetAccountRuntimeBlocker(svc)
					upstream.check = func() {
						require.False(t, svc.isOpenAIAccountRuntimeBlocked(account), "must not freeze before retry budget is exhausted")
					}
					c, rec := newTransportFailoverTestContext(t)
					c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
					_, err := svc.Forward(context.Background(), c, account, []byte(`{"model":"gpt-6-astra","input":"hello","stream":true}`))
					require.Equal(t, 7, upstream.calls, "initial attempt plus the existing six retries")
					var failover *UpstreamFailoverError
					require.ErrorAs(t, err, &failover)
					require.Equal(t, 429, failover.StatusCode)
					require.False(t, failover.RetryableOnSameAccount, "outer handlers must not multiply retry budget")
					require.Empty(t, rec.Body.String(), "failed attempts must not leak into client stream")
					require.Equal(t, 1, repo.rateLimitCalls, "apply the existing 429 cooldown only after exhaustion")
				})
			})
		}
	}
}

func TestOpenAIRejectedRequestHTTPProtocols(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, endpoint := range []string{"responses", "chat/completions", "messages"} {
		for _, mode := range []string{"responses", "raw_cc", "passthrough", "pool", "custom_400_rule"} {
			t.Run(endpoint+"/"+mode, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					repo := &anthropicWindowLimitRepo{}
					account := &Account{ID: 926, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
						Credentials: map[string]any{"api_key": "test", "base_url": "https://api.example.invalid"}, Extra: map[string]any{}}
					switch mode {
					case "raw_cc":
						account.Extra["openai_responses_supported"] = false
					case "passthrough":
						account.Extra["openai_passthrough"] = true
					case "pool":
						account.Credentials["pool_mode"] = true
						account.Credentials["pool_mode_retry_status_codes"] = []any{float64(400)}
					case "custom_400_rule":
						account.Credentials["temp_unschedulable_enabled"] = true
						account.Credentials["temp_unschedulable_rules"] = []any{map[string]any{"error_code": float64(400), "keywords": []any{"alb"}, "duration_minutes": float64(1)}}
					}
					cfg := &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize, LogUpstreamErrorBody: true, LogUpstreamErrorBodyMaxBytes: 2048}}
					rl := NewRateLimitService(repo, nil, cfg, nil, nil)
					up := &retry429Upstream{t: t, repo: repo, accountID: account.ID, respond: func(_ int, _ *http.Request) *http.Response {
						return &http.Response{StatusCode: 400, Header: http.Header{"Content-Type": []string{"text/html"}, "Retry-After": []string{"2"}}, Body: io.NopCloser(strings.NewReader(openAIRejectedALBBody))}
					}}
					svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: up, rateLimitService: rl}
					rl.SetAccountRuntimeBlocker(svc)
					c, rec := newTransportFailoverTestContext(t)
					c.Request = httptest.NewRequest(http.MethodPost, "/v1/"+endpoint, nil)
					body := []byte(`{"model":"gpt-6-astra","input":"hello","messages":[{"role":"user","content":"hello"}],"max_tokens":16,"stream":false}`)
					started := time.Now()
					var err error
					switch endpoint {
					case "responses":
						_, err = svc.Forward(context.Background(), c, account, body)
					case "messages":
						_, err = svc.ForwardAsAnthropic(context.Background(), c, account, body, "", "")
					default:
						_, err = svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "")
					}
					require.Equal(t, 7, up.calls)
					var failure *UpstreamFailoverError
					require.ErrorAs(t, err, &failure)
					require.Equal(t, 429, failure.StatusCode)
					require.False(t, failure.RetryableOnSameAccount)
					require.Equal(t, 12*time.Second, time.Since(started), "reuse Retry-After for all six retries")
					require.Equal(t, 1, repo.rateLimitCalls)
					require.Empty(t, rec.Body.String())
					raw, ok := c.Get(OpsUpstreamErrorsKey)
					require.True(t, ok, "retry attempts must retain the original 400 evidence")
					events := raw.([]*OpsUpstreamErrorEvent)
					require.Len(t, events, 7)
					for _, event := range events {
						require.Equal(t, 400, event.UpstreamStatusCode)
						require.Contains(t, event.Detail, "alb")
					}
				})
			})
		}
	}
}

func TestOpenAIRejectedRequestClassifierPreservesDiagnostics(t *testing.T) {
	for _, body := range []string{
		`{"error":{"type":"authentication_error","message":"Upstream rejected the request"}}`,
		`{"error":{"type":"permission_error","message":"Upstream rejected the request"}}`,
		`{"error":{"type":"invalid_request_error","param":"tools","message":"Upstream rejected the request"}}`,
		`{"error":{"code":"invalid_function_parameters","message":"Upstream rejected the request"}}`,
		`{"error":{"status_code":401,"message":"Upstream rejected the request"}}`,
		`{"message":"Invalid input","request":{"text":"Upstream rejected the request"}}`,
		`{"detail":"Invalid input"}`,
		`{"response":{"error":{"code":"invalid_function_parameters","message":"Upstream rejected the request"}}}`,
		`<html><title>400 Bad Request</title><body>Invalid header value</body></html>`,
		openAIInvalidFunctionParametersBody,
	} {
		t.Run(body, func(t *testing.T) {
			require.False(t, isOpenAIGenericUpstreamRejection(400, "", []byte(body)))
		})
	}
}

func TestOpenAIRejectedRequestSuccessCancellationAndCommittedOutput(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		for _, scenario := range []string{"success", "cancel", "committed"} {
			t.Run(scenario+"/"+strconv.FormatBool(passthrough), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					repo := &anthropicWindowLimitRepo{}
					account := &Account{ID: 927, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "test", "base_url": "https://api.example.invalid"}, Extra: map[string]any{"openai_passthrough": passthrough}}
					cfg := &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}
					rl := NewRateLimitService(repo, nil, cfg, nil, nil)
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					up := &retry429Upstream{t: t, repo: repo, accountID: account.ID}
					up.respond = func(attempt int, _ *http.Request) *http.Response {
						if scenario == "committed" {
							body := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\ndata: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"type\":\"invalid_request_error\",\"message\":\"Upstream rejected the request\"}}}\n\n"
							return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))}
						}
						if attempt <= 2 {
							if scenario == "cancel" {
								cancel()
							}
							return &http.Response{StatusCode: 400, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(openAIRejectedALBBody))}
						}
						body := `{"id":"resp_test","object":"response","status":"completed","model":"gpt-6-astra","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1}}`
						return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}
					}
					svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: up, rateLimitService: rl}
					rl.SetAccountRuntimeBlocker(svc)
					c, rec := newTransportFailoverTestContext(t)
					body := []byte(`{"model":"gpt-6-astra","input":"hello","stream":` + strconv.FormatBool(scenario == "committed") + `}`)
					_, err := svc.Forward(ctx, c, account, body)
					switch scenario {
					case "success":
						require.NoError(t, err)
						require.Equal(t, 3, up.calls)
						require.Contains(t, rec.Body.String(), `"text":"ok"`)
					case "cancel":
						require.ErrorIs(t, err, context.Canceled)
						require.Equal(t, 1, up.calls)
						require.Empty(t, rec.Body.String())
					case "committed":
						require.Error(t, err)
						require.Equal(t, 1, up.calls)
						require.Contains(t, rec.Body.String(), "partial")
					}
					require.Zero(t, repo.rateLimitCalls)
					require.False(t, svc.isOpenAIAccountRuntimeBlocked(account))
				})
			})
		}
	}
}

func TestOpenAIRejectedRequestWSUses429Budget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		repo := &anthropicWindowLimitRepo{}
		account := &Account{ID: 928, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
		svc := &OpenAIGatewayService{rateLimitService: NewRateLimitService(repo, nil, nil, nil, nil)}
		ctx, state, restore := beginUpstream429Retry(context.Background(), nil, account)
		defer restore()
		stub := &retry429FrameStub{onWrite: func(_ []byte) [][]byte {
			require.Zero(t, repo.rateLimitCalls)
			return [][]byte{[]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"Upstream rejected the request"}}`)}
		}}
		conn := &openAIWS429RetryFrameConn{FrameConn: stub, service: svc, account: account, state: state}
		require.NoError(t, conn.WriteFrame(ctx, coderws.MessageText, []byte(`{"type":"response.create","model":"gpt-6-astra","input":"hello"}`)))
		_, _, err := conn.ReadFrame(ctx)
		var failure *UpstreamFailoverError
		require.ErrorAs(t, err, &failure)
		require.Len(t, stub.writes, 7)
		require.Equal(t, 1, repo.rateLimitCalls)
	})
}

func TestOpenAIRejectedRequestDoesNotChangeOtherPlatforms(t *testing.T) {
	for _, platform := range []string{PlatformAnthropic, PlatformGrok, PlatformDeepseek} {
		t.Run(platform, func(t *testing.T) {
			account := &Account{ID: 930, Platform: platform, Type: AccountTypeAPIKey}
			svc := &OpenAIGatewayService{}
			ctx, _, restore := beginUpstream429Retry(context.Background(), nil, account)
			defer restore()
			require.Nil(t, svc.newOpenAIGenericRejectionRetryError(ctx, account, nil, []byte(openAIRejectedALBBody)))
			payload := []byte(`{"type":"response.failed","response":{"error":{"type":"invalid_request_error","message":"Upstream rejected the request"}}}`)
			require.False(t, openAIAccountStreamShouldFailover(account, payload, "Upstream rejected the request", "response.failed"))
			require.False(t, openAIAccountStreamRateLimit(account, payload, "Upstream rejected the request"))
		})
	}
}

func TestOpenAIRejectedRequestStreamingCompatProtocols(t *testing.T) {
	for _, endpoint := range []string{"responses", "chat/completions", "messages"} {
		for _, stream := range []bool{false, true} {
			for _, event := range []string{"error", "response.failed"} {
				t.Run(endpoint+"/stream="+strconv.FormatBool(stream)+"/"+event, func(t *testing.T) {
					synctest.Test(t, func(t *testing.T) {
						repo := &anthropicWindowLimitRepo{}
						account := &Account{ID: 931, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "test", "base_url": "https://api.example.invalid"}}
						cfg := &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}
						rl := NewRateLimitService(repo, nil, cfg, nil, nil)
						up := &retry429Upstream{t: t, repo: repo, accountID: account.ID, respond: func(_ int, _ *http.Request) *http.Response {
							payload := `{"type":"error","error":{"type":"invalid_request_error","message":"Upstream rejected the request"}}`
							if event == "response.failed" {
								payload = `{"type":"response.failed","response":{"id":"resp_fail","status":"failed","error":{"type":"invalid_request_error","message":"Upstream rejected the request"}}}`
							}
							return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: " + payload + "\n\n"))}
						}}
						svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: up, rateLimitService: rl}
						c, rec := newTransportFailoverTestContext(t)
						c.Request = httptest.NewRequest(http.MethodPost, "/v1/"+endpoint, nil)
						body := []byte(`{"model":"gpt-6-astra","input":"hello","messages":[{"role":"user","content":"hello"}],"max_tokens":16,"stream":` + strconv.FormatBool(stream) + `}`)
						var err error
						switch endpoint {
						case "responses":
							_, err = svc.Forward(context.Background(), c, account, body)
						case "messages":
							_, err = svc.ForwardAsAnthropic(context.Background(), c, account, body, "", "")
						default:
							_, err = svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "")
						}
						var failure *UpstreamFailoverError
						require.ErrorAs(t, err, &failure)
						require.Equal(t, 429, failure.StatusCode)
						require.Equal(t, 7, up.calls)
						require.Equal(t, 1, repo.rateLimitCalls)
						require.Empty(t, rec.Body.String())
					})
				})
			}
		}
	}
}
