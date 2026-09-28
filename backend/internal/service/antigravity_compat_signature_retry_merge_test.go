//go:build unit

package service

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestAntigravityCompatSignatureOnlyRetryBoundary(t *testing.T) {
	gin.SetMode(gin.TestMode)
	signature := `data: {"response":{"responseId":"sig-only","candidates":[{"content":{"parts":[{"thought":true,"thoughtSignature":"test-signature"}]}}]}}` + "\n\n"
	malformed := `data: {"response":{"candidates":[{"finishReason":"MALFORMED_FUNCTION_CALL"}]}}` + "\n\n"
	for _, protocol := range []string{"responses", "chat"} {
		for _, tc := range []struct {
			name     string
			body     string
			contains string
		}{
			{name: "signature_eof", body: signature},
			{name: "malformed_function_call", body: signature + malformed},
			{name: "text_after_signature", body: signature + `data: {"response":{"candidates":[{"content":{"parts":[{"text":"visible-result"}]},"finishReason":"STOP"}]}}` + "\n\n", contains: "visible-result"},
			{name: "tool_after_signature", body: signature + `data: {"response":{"candidates":[{"content":{"parts":[{"functionCall":{"id":"call_test","name":"lookup","args":{"city":"Paris"}}}]},"finishReason":"STOP"}]}}` + "\n\n", contains: "lookup"},
		} {
			t.Run(protocol+"/"+tc.name, func(t *testing.T) {
				svc := newAntigravityCompatService(config.GatewayConfig{MaxLineSize: defaultMaxLineSize}, nil)
				c, recorder := newAntigravityCompatContext(http.MethodPost, "/", nil)
				resp := &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
					Body:       io.NopCloser(strings.NewReader(tc.body)),
				}
				defer func() { _ = resp.Body.Close() }()
				var result *antigravityStreamResult
				var err error
				if protocol == "responses" {
					result, err = svc.handleResponsesStreamingFromAntigravity(c, resp, time.Now(), "gemini-3.1-pro-high")
				} else {
					result, err = svc.handleChatCompletionsStreamingFromAntigravity(c, resp, time.Now(), "gemini-3.1-pro-high", true)
				}
				if tc.contains == "" {
					require.Nil(t, result)
					var failover *UpstreamFailoverError
					require.ErrorAs(t, err, &failover)
					require.True(t, failover.RetryableOnSameAccount)
					require.Empty(t, recorder.Body.String())
					require.Empty(t, recorder.Header().Get("Content-Type"))
					return
				}
				require.NoError(t, err)
				require.NotNil(t, result)
				require.Contains(t, recorder.Body.String(), tc.contains)
				require.Equal(t, "text/event-stream", recorder.Header().Get("Content-Type"))
			})
		}
	}
}
