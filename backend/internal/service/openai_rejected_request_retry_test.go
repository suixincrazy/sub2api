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
