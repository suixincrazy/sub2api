package service

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const multimodalSchemaError = `{"error":{"code":"multimodal_forbidden","message":"audio/video content is not allowed","type":"invalid_request_error"}}`

func TestOpenAIMultimodalSchemaCompatForward(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, passthrough := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("passthrough=%v/stream=%v", passthrough, stream), func(t *testing.T) {
				body := []byte(fmt.Sprintf(`{"model":"test-model","stream":%v,"input":[{"role":"user","content":"Configure the fingerprint."},{"type":"function_call","call_id":"old","name":"configure","arguments":"{\"fingerprint_config\":{\"audio\":\"0\"}}"},{"type":"function_call_output","call_id":"old","output":"OK"}],"tools":[{"type":"function","name":"configure","parameters":{"type":"object","properties":{"fingerprint_config":{"type":"object","properties":{"audio":{"type":"string","enum":["0","1"]}},"required":["audio"],"additionalProperties":false}},"required":["fingerprint_config"],"additionalProperties":false}}]}`, stream))
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
				response := `{"id":"resp_compat","status":"completed","output":[{"type":"function_call","id":"fc_new","call_id":"new","name":"configure","arguments":"{\"fingerprint_config\":{\"__s2a_audio\":\"1\"}}"}],"usage":{"input_tokens":1,"output_tokens":1}}`
				contentType := "application/json"
				if stream {
					contentType = "text/event-stream"
					response = strings.Join([]string{
						`data: {"type":"response.output_item.added","sequence_number":0,"output_index":0,"item":{"type":"function_call","id":"fc_new","call_id":"new","name":"configure","arguments":""}}`,
						`data: {"type":"response.function_call_arguments.delta","sequence_number":1,"item_id":"fc_new","output_index":0,"delta":"{\"fingerprint_config\":{\"__s2"}`,
						`data: {"type":"response.function_call_arguments.delta","sequence_number":2,"item_id":"fc_new","output_index":0,"delta":"a_audio\":\"1\"}}"}`,
						`data: {"type":"response.function_call_arguments.done","sequence_number":3,"item_id":"fc_new","output_index":0,"arguments":"{\"fingerprint_config\":{\"__s2a_audio\":\"1\"}}"}`,
						`data: {"type":"response.output_item.done","sequence_number":4,"output_index":0,"item":{"type":"function_call","id":"fc_new","call_id":"new","name":"configure","arguments":"{\"fingerprint_config\":{\"__s2a_audio\":\"1\"}}"}}`,
						`data: {"type":"response.completed","sequence_number":5,"response":` + response + `}`,
					}, "\n\n") + "\n\n"
				}
				upstream := &httpUpstreamRecorder{responses: []*http.Response{
					{StatusCode: 400, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(multimodalSchemaError))},
					{StatusCode: 200, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(response))},
				}}
				svc := openAIClientToolsTestService(upstream)
				account := &Account{ID: 81234, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "test-key"}}
				if !passthrough {
					account.Type = AccountTypeOAuth
					account.Credentials = map[string]any{"access_token": "test-token"}
					account.Extra = map[string]any{"openai_passthrough": false, openai_compat.ExtraKeyResponsesMode: string(openai_compat.ResponsesSupportModeAuto)}
				}
				var err error
				if passthrough {
					_, err = svc.forwardOpenAIPassthrough(context.Background(), c, account, body, body, "test-model", false, nil, stream, time.Now())
				} else {
					_, err = svc.Forward(context.Background(), c, account, body)
				}
				require.NoError(t, err)
				require.Len(t, upstream.bodies, 2)
				require.True(t, gjson.GetBytes(upstream.bodies[0], "tools.0.parameters.properties.fingerprint_config.properties.audio").Exists())
				require.Equal(t, "string", gjson.GetBytes(upstream.bodies[1], "tools.0.parameters.properties.fingerprint_config.properties.__s2a_audio.type").String())
				require.JSONEq(t, `{"fingerprint_config":{"__s2a_audio":"0"}}`, gjson.GetBytes(upstream.bodies[1], "input.1.arguments").String())
				require.NotContains(t, recorder.Body.String(), "__s2a_audio")
				if stream {
					require.Contains(t, recorder.Body.String(), `\"audio\":\"1\"`)
					require.Contains(t, recorder.Body.String(), "response.completed")
				} else {
					require.JSONEq(t, `{"fingerprint_config":{"audio":"1"}}`, gjson.Get(recorder.Body.String(), "output.0.arguments").String())
				}
			})
		}
	}
}

func TestOpenAIMultimodalSchemaCompatGuardAndRetryLimit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	base := []byte(`{"model":"test-model","input":"test","tools":[{"type":"function","name":"configure","parameters":{"type":"object","properties":{"audio":{"type":"string"}}}}]}`)
	for _, test := range []struct {
		name    string
		body    []byte
		failure string
		status  int
		want    int
	}{
		{"retry exactly once", base, multimodalSchemaError, 400, 2},
		{"different error", base, `{"error":{"code":"invalid_request_error","message":"audio/video content is not allowed"}}`, 400, 1},
		{"different message", base, `{"error":{"code":"multimodal_forbidden","message":"other cause"}}`, 400, 1},
		{"no schema", []byte(`{"model":"test-model","input":"audio"}`), multimodalSchemaError, 400, 1},
		{"real media", []byte(`{"model":"test-model","input":[{"role":"user","content":[{"type":"input_audio","input_audio":{"data":"AAAA","format":"wav"}}]}],"tools":[{"type":"function","name":"f","parameters":{"properties":{"audio":{"type":"string"}}}}]}`), multimodalSchemaError, 400, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			upstream := &httpUpstreamRecorder{responses: repeatHTTPResponseForTest(newOpenAIRejectedFieldTestResponse(test.status, test.failure), 3)}
			svc := openAIClientToolsTestService(upstream)
			c := newOpenAIRejectedFieldTestContext(test.body)
			_, err := svc.Forward(context.Background(), c, newOpenAIRejectedFieldTestAccount(), test.body)
			require.Error(t, err)
			require.Len(t, upstream.bodies, test.want)
		})
	}
	for _, status := range []int{401, 403, 429, 500} {
		_, ok := prepareOpenAIMultimodalSchemaRetry(newOpenAIRejectedFieldTestContext(base), status, base, []byte(multimodalSchemaError))
		require.False(t, ok)
	}
	aliased, ok := prepareOpenAIMultimodalSchemaRetry(newOpenAIRejectedFieldTestContext(base), 400, base, []byte(multimodalSchemaError))
	require.True(t, ok)
	_, ok = prepareOpenAIMultimodalSchemaRetry(newOpenAIRejectedFieldTestContext(aliased), 400, aliased, []byte(multimodalSchemaError))
	require.False(t, ok)
}

func TestOpenAIMultimodalSchemaCompatNamespacesAndMapping(t *testing.T) {
	body := []byte(`{"model":"test-model","input":[{"type":"function_call","namespace":"browser","name":"configure","arguments":"{\"audio\":\"0\"}"}],"tools":[{"type":"custom","name":"exec"},{"type":"namespace","name":"browser","tools":[{"type":"function","name":"configure","parameters":{"type":"object","properties":{"audio":{"type":"string"}}}}]}]}`)
	c := newOpenAIRejectedFieldTestContext(body)
	first, mapping, err := adaptOpenAIResponsesClientTools(body)
	require.NoError(t, err)
	setOpenAIResponsesClientToolMapping(c, mapping)
	adapted, ok := prepareOpenAIMultimodalSchemaRetry(c, 400, first, []byte(multimodalSchemaError))
	require.True(t, ok)
	name := gjson.GetBytes(adapted, "tools.1.name").String()
	response, err := sjson.Set(`{"output":[{"type":"function_call","name":"placeholder","arguments":"{\"__s2a_audio\":\"1\"}"},{"type":"function_call","name":"exec","arguments":"{\"input\":\"pwd\"}"}]}`, "output.0.name", name)
	require.NoError(t, err)
	restored, err := restoreOpenAIResponsesClientToolPayload(c, []byte(response))
	require.NoError(t, err)
	require.Equal(t, "configure", gjson.GetBytes(restored, "output.0.name").String())
	require.Equal(t, "browser", gjson.GetBytes(restored, "output.0.namespace").String())
	require.JSONEq(t, `{"audio":"1"}`, gjson.GetBytes(restored, "output.0.arguments").String())
	require.Equal(t, "custom_tool_call", gjson.GetBytes(restored, "output.1.type").String())
	clearOpenAIResponsesClientToolMapping(c)
	_, ok = openAIResponsesClientToolMapping(c)
	require.False(t, ok)
}
