//go:build unit

package handler

import (
	"bytes"
	"io"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type codexContextRouteUpstream struct {
	service.HTTPUpstream
	body []byte
}

func (u *codexContextRouteUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	u.body = body
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"text/event-stream"}},
		Body:       io.NopCloser(bytes.NewBufferString("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_context\",\"model\":\"gpt-5.1\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"OK\"}]}],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")),
	}, nil
}

func TestOpenAIResponsesCodexPagesRemainToolOutputs(t *testing.T) {
	upstream := &codexContextRouteUpstream{}
	h := newOpenAIResponsesFailoverTestHandler(t, upstream)
	c, rec := newOpenAIResponsesFailoverTestContext(t, nil)
	body := codexContextBody(codexOpenPageItem, codexSelectedPageItem, codexUserItem)
	c.Request.Body = io.NopCloser(bytes.NewReader(body))
	c.Request.ContentLength = int64(len(body))
	h.Responses(c)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NotEmpty(t, upstream.body)
	var outputCount int
	for _, item := range gjson.GetBytes(upstream.body, "input").Array() {
		if item.Get("type").String() == "function_call_output" {
			outputCount++
			require.False(t, item.Get("role").Exists())
			require.NotEmpty(t, item.Get("call_id").String())
		}
		require.NotContains(t, item.Raw, "external_codex_apps_")
	}
	require.Equal(t, 2, outputCount)
	require.True(t, service.AnalyzeToolCallOutputContextCoverageBytes(upstream.body).ContextCoversAllCallIDs)
}
