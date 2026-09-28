package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestOpenAIWSContextWindowRollover_ThroughIngress(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, metadata := range []string{"embedded", "direct", "embedded_then_omitted", "handshake_only"} {
		t.Run(metadata, func(t *testing.T) {
			cfg := newOpenAIWSExecutionScopeTestConfig()
			upstream := &openAIWSCaptureConn{}
			for turn := 1; turn <= 3; turn++ {
				upstream.events = append(upstream.events, []byte(fmt.Sprintf(
					`{"type":"response.completed","response":{"id":"resp_window_%d","model":"gpt-5.1","status":"completed","usage":{"input_tokens":1,"output_tokens":1}}}`, turn)))
			}
			pool := newOpenAIWSConnPool(cfg)
			defer pool.Close()
			pool.setClientDialerForTest(&openAIWSSingleConnDialer{conn: upstream})
			svc := &OpenAIGatewayService{
				cfg: cfg, httpUpstream: &httpUpstreamRecorder{}, cache: &stubGatewayCache{},
				openaiWSResolver: NewOpenAIWSProtocolResolver(cfg), toolCorrector: NewCodexToolCorrector(),
				openaiWSPool: pool,
			}
			account := &Account{
				ID: 467, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
				Status: StatusActive, Schedulable: true, Concurrency: 1,
				Credentials: map[string]any{"api_key": "sk-test"},
				Extra:       map[string]any{"responses_websockets_v2_enabled": true},
			}
			done := make(chan error, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := coderws.Accept(w, r, nil)
				if err != nil {
					done <- err
					return
				}
				defer func() { _ = conn.CloseNow() }()
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = r
				ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
				defer cancel()
				_, first, err := conn.Read(ctx)
				if err != nil {
					done <- err
					return
				}
				done <- svc.ProxyResponsesWebSocketFromClient(ctx, c, conn, account, "sk-test", first, nil)
			}))
			defer server.Close()
			headers := http.Header{}
			headers.Set("User-Agent", "unit-test-agent/1.0")
			headers.Set(openAIWSTurnMetadataHeader, `{"window_id":"window-a"}`)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			client, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), &coderws.DialOptions{HTTPHeader: headers})
			require.NoError(t, err)
			defer func() { _ = client.CloseNow() }()
			for turn := 1; turn <= 3; turn++ {
				payload := map[string]any{
					"type": "response.create", "model": "gpt-5.1", "store": true,
					"input": []map[string]any{{"role": "user", "content": fmt.Sprintf("turn-%d", turn)}},
				}
				if turn > 1 {
					payload["previous_response_id"] = fmt.Sprintf("resp_window_%d", turn-1)
					switch metadata {
					case "embedded":
						payload["client_metadata"] = map[string]any{openAIWSTurnMetadataHeader: `{"window_id":"window-b"}`}
					case "direct":
						payload["client_metadata"] = map[string]any{"x-codex-window-id": "window-b"}
					case "embedded_then_omitted":
						if turn == 2 {
							payload["client_metadata"] = map[string]any{openAIWSTurnMetadataHeader: `{"window_id":"window-b"}`}
						}
					}
				}
				body, err := json.Marshal(payload)
				require.NoError(t, err)
				require.NoError(t, client.Write(ctx, coderws.MessageText, body))
				_, response, err := client.Read(ctx)
				require.NoError(t, err)
				require.Equal(t, fmt.Sprintf("resp_window_%d", turn), gjson.GetBytes(response, "response.id").String())
			}
			require.NoError(t, client.Close(coderws.StatusNormalClosure, "done"))
			select {
			case err := <-done:
				require.NoError(t, err)
			case <-ctx.Done():
				t.Fatal("ingress did not finish")
			}
			upstream.mu.Lock()
			defer upstream.mu.Unlock()
			require.Len(t, upstream.writes, 3)
			if metadata == "handshake_only" {
				require.Equal(t, "resp_window_1", upstream.writes[1]["previous_response_id"])
			} else {
				require.NotContains(t, upstream.writes[1], "previous_response_id", "a new window must not retain the handshake window's response")
			}
			require.Equal(t, "resp_window_2", upstream.writes[2]["previous_response_id"], "the next turn in the same new window must keep its continuation")
		})
	}
}
