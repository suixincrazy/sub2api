//go:build unit

package service

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestChatStructuredFallbackShares429RetryBudget(t *testing.T) {
	for _, scenario := range []string{"responses_rate_limit", "raw_exhausted", "raw_recovers", "request_canceled", "backoff_canceled"} {
		t.Run(scenario, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				gin.SetMode(gin.TestMode)
				body := []byte(`{"model":"gpt-5.4","messages":[{"role":"user","content":"hello"}],"stream":false}`)
				requestCtx, cancelRequest := context.WithCancel(context.Background())
				defer cancelRequest()
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body)).WithContext(requestCtx)
				original := c.Request
				account := structuredInputRetryTestAccount()
				repo := &anthropicWindowLimitRepo{}
				upstream := &retry429Upstream{t: t, repo: repo, accountID: account.ID}
				responses, chats := 0, 0
				upstream.respond = func(_ int, request *http.Request) *http.Response {
					status := http.StatusTooManyRequests
					payload := `{"error":{"type":"rate_limit_error","message":"rate limit reached"}}`
					switch request.URL.Path {
					case "/v1/responses":
						responses++
						if scenario != "responses_rate_limit" {
							status = http.StatusBadRequest
							payload = `{"error":{"code":"invalid_type","param":"input","message":"Expected a string, but got an array instead."}}`
						}
					case "/v1/chat/completions":
						chats++
						if scenario == "raw_recovers" && chats > 1 {
							status = http.StatusOK
							payload = `{"id":"chatcmpl_retry","object":"chat.completion","model":"gpt-5.4","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
						}
						if scenario == "request_canceled" {
							cancelRequest()
						}
						if scenario == "backoff_canceled" && chats == 1 {
							go func() {
								time.Sleep(time.Millisecond)
								cancelRequest()
							}()
						}
					default:
						t.Fatalf("unexpected upstream path: %s", request.URL.Path)
					}
					return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(payload))}
				}
				cfg := structuredInputRetryTestConfig()
				rl := NewRateLimitService(repo, nil, cfg, nil, nil)
				svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream, rateLimitService: rl}
				result, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "")
				require.Same(t, original, c.Request, "restore the original request after forwarding")
				switch scenario {
				case "responses_rate_limit":
					require.Equal(t, 7, responses)
					require.Zero(t, chats, "429 must not trigger a format fallback")
				case "raw_exhausted":
					require.Equal(t, 1, responses, "format rejection may only be retried once as raw Chat")
					require.Equal(t, 7, chats, "retain the initial raw request plus six 429 retries")
				case "raw_recovers":
					require.NoError(t, err)
					require.NotNil(t, result)
					require.Equal(t, "/v1/chat/completions", result.UpstreamEndpoint)
					require.Equal(t, 1, responses)
					require.Equal(t, 2, chats)
					next, _ := gin.CreateTestContext(httptest.NewRecorder())
					next.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
					_, err = svc.ForwardAsChatCompletions(context.Background(), next, account, body, "", "")
					require.NoError(t, err)
					require.Equal(t, 2, responses, "a new call must make its own format decision")
					require.Equal(t, 3, chats)
				default:
					require.ErrorIs(t, err, context.Canceled)
					require.Equal(t, 1, responses)
					require.Equal(t, 1, chats)
				}
				if scenario == "responses_rate_limit" || scenario == "raw_exhausted" {
					var failure *UpstreamFailoverError
					require.ErrorAs(t, err, &failure)
					require.Equal(t, http.StatusTooManyRequests, failure.StatusCode)
					require.False(t, failure.RetryableOnSameAccount)
					require.Equal(t, 1, repo.rateLimitCalls, "freeze once after the shared retry budget is exhausted")
				} else {
					require.Zero(t, repo.rateLimitCalls)
				}
				require.Zero(t, repo.tempUnschedCalls)
				require.Zero(t, repo.modelRateLimitCalls)
			})
		})
	}
}
