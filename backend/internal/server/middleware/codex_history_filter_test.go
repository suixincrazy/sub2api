package middleware

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/codexhistory"
	"github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/andybalholm/brotli"
	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/require"
)

const historyFilterSample = `{"model":"mock-model","store":true,"reasoning":{"effort":"xhigh"},"include":["reasoning.encrypted_content"],"input":[{"type":"reasoning","encrypted_content":"HISTORY_CANARY"},{"type":"function_call","id":"old","call_id":"pair","name":"tool","arguments":"{}"},{"type":"function_call_output","id":"old-result","call_id":"pair","output":"ok"}]}`

func historyFilterRouter(t *testing.T, enabled bool, handler gin.HandlerFunc) (*gin.Engine, *service.SettingService) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	svc := service.NewSettingService(&panelRateLimitStubRepo{}, nil)
	require.NoError(t, svc.SetCodexHistoryFilterEnabled(context.Background(), enabled))
	filter := NewCodexHistoryFilter(svc)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		if c.GetHeader("Authorization") != "Bearer AUTH_CANARY" {
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}
		c.Set(string(ContextKeyAPIKey), allowlistAPIKey(true, "mock-model"))
		c.Next()
	})
	router.Use(filter.Prepare, GroupModelAllowlist(), filter.Apply)
	for _, prefix := range []string{"", "/v1", "/backend-api/codex"} {
		for _, suffix := range []string{"/responses", "/responses/", "/responses/compact", "/responses/input_tokens", "/models"} {
			router.Any(prefix+suffix, handler)
		}
	}
	return router, svc
}

func historyFilterRequest(router *gin.Engine, method, path string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer AUTH_CANARY")
	req.Header.Set("Content-Type", "application/json")
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, req)
	return response
}

func TestCodexHistoryFilterCompressedRequestsAndAliases(t *testing.T) {
	calls := 0
	want, err := codexhistory.Filter([]byte(historyFilterSample))
	require.NoError(t, err)
	router, svc := historyFilterRouter(t, true, func(c *gin.Context) {
		calls++
		body, err := httputil.ReadRequestBodyWithPrealloc(c.Request)
		require.NoError(t, err)
		require.JSONEq(t, string(want.Body), string(body))
		require.EqualValues(t, len(body), c.Request.ContentLength)
		require.Equal(t, strconv.Itoa(len(body)), c.GetHeader("Content-Length"))
		require.Empty(t, c.GetHeader("Content-Encoding"))
		require.Empty(t, c.GetHeader("Expect"))
		require.Equal(t, "Bearer AUTH_CANARY", c.GetHeader("Authorization"))
		require.Equal(t, "session-1", c.GetHeader("X-Session-Id"))
		require.Equal(t, "keep", c.Query("mode"))
		c.Header("X-Test", "unchanged")
		c.Data(http.StatusTooManyRequests, "application/json", []byte(`{"from":"upstream"}`))
	})
	for _, encoding := range []string{"identity", "gzip", "deflate", "br", "zstd"} {
		t.Run(encoding, func(t *testing.T) {
			var data bytes.Buffer
			var encoder io.WriteCloser
			switch encoding {
			case "gzip":
				encoder = gzip.NewWriter(&data)
			case "deflate":
				encoder = zlib.NewWriter(&data)
			case "br":
				encoder = brotli.NewWriter(&data)
			case "zstd":
				var err error
				encoder, err = zstd.NewWriter(&data)
				require.NoError(t, err)
			default:
				_, _ = data.WriteString(historyFilterSample)
			}
			if encoder != nil {
				_, err := encoder.Write([]byte(historyFilterSample))
				require.NoError(t, err)
				require.NoError(t, encoder.Close())
			}
			response := historyFilterRequest(router, http.MethodPost, "/v1/responses?mode=keep", data.Bytes(), map[string]string{"Content-Encoding": encoding, "Expect": "100-continue", "X-Session-Id": "session-1"})
			require.Equal(t, 429, response.Code)
			require.Equal(t, `{"from":"upstream"}`, response.Body.String())
			require.Equal(t, "unchanged", response.Header().Get("X-Test"))
		})
	}
	for _, path := range []string{"/responses/", "/backend-api/codex/responses"} {
		response := historyFilterRequest(router, http.MethodPost, path+"?mode=keep", []byte(historyFilterSample), map[string]string{"X-Session-Id": "session-1"})
		require.Equal(t, 429, response.Code)
	}
	status, err := svc.GetCodexHistoryFilterStatus(context.Background())
	require.NoError(t, err)
	require.Equal(t, 7, calls, "the filter must not add retries")
	require.EqualValues(t, 7, status.Stats.FilteredRequests)
	require.EqualValues(t, 7, status.Stats.RemovedReasoningItems)
	require.EqualValues(t, 14, status.Stats.RemovedItemIDs)
	require.EqualValues(t, 7, status.Stats.UpstreamHTTPErrors)
	require.Equal(t, 429, *status.Stats.LastUpstreamStatus)
}

func TestCodexHistoryFilterRejectsUnsafeRequestsBeforeForwarding(t *testing.T) {
	calls := 0
	router, svc := historyFilterRouter(t, true, func(c *gin.Context) { calls++; c.Status(200) })
	for _, tt := range []struct {
		method, path, body, encoding, code string
		status                             int
	}{
		{http.MethodPost, "/v1/responses", `{`, "", "invalid_request_json", 400},
		{http.MethodPost, "/responses", `{"previous_response_id":"old"}`, "", "response_reference_not_portable", 400},
		{http.MethodPost, "/responses", `{"input":[{"type":"item_reference","id":"old"}]}`, "", "encrypted_context_not_portable", 400},
		{http.MethodPost, "/responses", `{"context_management":[{"type":"compaction"}]}`, "", "encrypted_compaction_disabled", 400},
		{http.MethodPost, "/backend-api/codex/responses/compact", `{}`, "", "encrypted_compaction_disabled", 409},
		{http.MethodGet, "/v1/responses", ``, "", "websocket_filtering_unsupported", 426},
		{http.MethodPost, "/responses", `{}`, "compress", "unsupported_content_encoding", 415},
		{http.MethodPost, "/responses", `broken gzip`, "gzip", "invalid_compressed_request", 400},
	} {
		response := historyFilterRequest(router, tt.method, tt.path, []byte(tt.body), map[string]string{"Content-Encoding": tt.encoding})
		require.Equal(t, tt.status, response.Code, response.Body.String())
		require.Contains(t, response.Body.String(), `"code":"`+tt.code+`"`)
	}
	response := historyFilterRequest(router, http.MethodPost, "/responses", []byte(historyFilterSample), map[string]string{"Origin": "https://example.test"})
	require.Equal(t, 403, response.Code)
	body := &readTrackingBody{Reader: strings.NewReader(historyFilterSample)}
	req := httptest.NewRequest(http.MethodPost, "/responses", body)
	req.Header.Set("Authorization", "Bearer AUTH_CANARY")
	req.ContentLength = codexhistory.MaxBodySize + 1
	response = httptest.NewRecorder()
	router.ServeHTTP(response, req)
	require.Equal(t, 413, response.Code)
	require.False(t, body.read)
	require.Zero(t, calls)
	status, err := svc.GetCodexHistoryFilterStatus(context.Background())
	require.NoError(t, err)
	require.EqualValues(t, 10, status.Stats.BlockedRequests)
	require.Zero(t, status.Stats.FilteredRequests)
}

func TestCodexHistoryFilterPreservesAuthAndModelAllowlist(t *testing.T) {
	calls := 0
	router, svc := historyFilterRouter(t, true, func(c *gin.Context) { calls++; c.Status(200) })
	response := historyFilterRequest(router, http.MethodPost, "/responses", []byte(historyFilterSample), map[string]string{"Authorization": "wrong"})
	require.Equal(t, 401, response.Code)
	for _, body := range []string{`{"model":"denied","model":"mock-model","input":[]}`, `{"model":"mock-model","Model":"denied","input":[]}`} {
		response = historyFilterRequest(router, http.MethodPost, "/responses", []byte(body), nil)
		require.Equal(t, 404, response.Code, response.Body.String())
	}
	require.Zero(t, calls)
	status, err := svc.GetCodexHistoryFilterStatus(context.Background())
	require.NoError(t, err)
	require.Zero(t, status.Stats.FilteredRequests)
}

func TestCodexHistoryFilterOffAndUnrelatedRoutesAreTransparent(t *testing.T) {
	var actual []byte
	router, svc := historyFilterRouter(t, false, func(c *gin.Context) {
		actual, _ = io.ReadAll(c.Request.Body)
		c.Status(200)
	})
	response := historyFilterRequest(router, http.MethodPost, "/responses", []byte(historyFilterSample), nil)
	require.Equal(t, 200, response.Code)
	require.Equal(t, historyFilterSample, string(actual))
	require.NoError(t, svc.SetCodexHistoryFilterEnabled(context.Background(), true))
	response = historyFilterRequest(router, http.MethodPost, "/responses/input_tokens", []byte(historyFilterSample), nil)
	require.Equal(t, 200, response.Code)
	require.Equal(t, historyFilterSample, string(actual))
	status, err := svc.GetCodexHistoryFilterStatus(context.Background())
	require.NoError(t, err)
	require.Zero(t, status.Stats.FilteredRequests)
}

func TestCodexHistoryFilterStreamsUnmodifiedAndPropagatesCancellation(t *testing.T) {
	canceled := make(chan struct{})
	first := "event: response.output_text.delta\ndata: {\"delta\":\"first\",\"encrypted_content\":\"response-unchanged\"}\n\n"
	router, _ := historyFilterRouter(t, true, func(c *gin.Context) {
		c.Header("Content-Type", "text/event-stream")
		_, _ = c.Writer.WriteString(first)
		c.Writer.Flush()
		<-c.Request.Context().Done()
		close(canceled)
	})
	server := httptest.NewServer(router)
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/responses", strings.NewReader(historyFilterSample))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer AUTH_CANARY")
	client := &http.Client{Timeout: 3 * time.Second}
	response, err := client.Do(req)
	require.NoError(t, err)
	reader := bufio.NewReader(response.Body)
	var streamed strings.Builder
	for i := 0; i < 3; i++ {
		line, err := reader.ReadString('\n')
		require.NoError(t, err)
		streamed.WriteString(line)
	}
	require.Equal(t, first, streamed.String(), "response data must pass before the request finishes")
	cancel()
	_ = response.Body.Close()
	select {
	case <-canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("request cancellation did not reach the forwarding context")
	}
}

func TestCodexHistoryFilterLogsOnlyCountsAndCodes(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	router, svc := historyFilterRouter(t, true, func(c *gin.Context) {
		c.Set(service.OpsUpstreamErrorsKey, []*service.OpsUpstreamErrorEvent{{Kind: "request_error", UpstreamStatusCode: 0}})
		c.Status(502)
	})
	require.Equal(t, 502, historyFilterRequest(router, http.MethodPost, "/responses", []byte(historyFilterSample), nil).Code)
	require.Equal(t, 400, historyFilterRequest(router, http.MethodPost, "/responses", []byte(`{"conversation":"HISTORY_CANARY"}`), nil).Code)
	require.NotContains(t, logs.String(), "AUTH_CANARY")
	require.NotContains(t, logs.String(), "HISTORY_CANARY")
	require.Contains(t, logs.String(), "removed_reasoning_items")
	require.Contains(t, logs.String(), "response_reference_not_portable")
	status, err := svc.GetCodexHistoryFilterStatus(context.Background())
	require.NoError(t, err)
	require.EqualValues(t, 1, status.Stats.UpstreamErrors)
}
