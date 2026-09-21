package codexhistory

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFilterPortableHistoryWebSearch(t *testing.T) {
	body := []byte(`{"model":"gpt-6-astra","input":[{"type":"web_search_call","id":"ws_foreign_resource","status":"completed","action":{"type":"search","query":"release notes","sources":[{"type":"url","url":"https://example.invalid/notes","title":"Release notes"}]}},{"type":"message","id":"msg_foreign","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"The release is ready.","annotations":[{"type":"url_citation","url":"https://example.invalid/notes","title":"Release notes","start_index":0,"end_index":21}]}]}]}`)
	result, err := Filter(body)
	require.NoError(t, err)
	require.NotContains(t, string(result.Body), "ws_foreign_resource", "native tool IDs must not look up an item in the previous Azure resource")
	require.Contains(t, string(result.Body), "release notes")
	require.Contains(t, string(result.Body), "https://example.invalid/notes")
	require.Contains(t, string(result.Body), "url_citation")
	require.Contains(t, string(result.Body), "final_answer")
	require.Equal(t, 2, result.RemovedItemIDs)
	var decoded map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(result.Body, &decoded))
	var items []map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(decoded["input"], &items))
	require.Equal(t, "web_search_call", stringValue(items[0]["type"]))
	require.Equal(t, "completed", stringValue(items[0]["status"]))
	require.JSONEq(t, `{"type":"search","query":"release notes","sources":[{"type":"url","url":"https://example.invalid/notes","title":"Release notes"}]}`, string(items[0]["action"]))
	again, err := Filter(result.Body)
	require.NoError(t, err)
	require.JSONEq(t, string(result.Body), string(again.Body))
	require.Zero(t, again.RemovedItemIDs)
	require.Zero(t, again.NormalizedAgentTextParts)
}

func TestFilterPortableHistoryAgentMessage(t *testing.T) {
	body := []byte(`{"reasoning":{"effort":"max"},"input":[{"type":"agent_message","id":"amsg_foreign_resource","author":"/root/review","recipient":"/root","content":[{"type":"input_text","text":"Review result: "},{"type":"encrypted_content","encrypted_content":"No issues found in the routing checks.","metadata":{"id":"keep_metadata"}}]},{"type":"function_call_output","call_id":"keep_pair","output":{"encrypted_content":"opaque user tool value","id":9007199254740993}}]}`)
	result, err := Filter(body)
	require.NoError(t, err)
	require.NotContains(t, string(result.Body), "amsg_foreign_resource")
	require.NotContains(t, string(result.Body), `"type":"encrypted_content"`, "the custom-provider agent text must not be sent to a decryption path")
	require.Contains(t, string(result.Body), "Review result: ")
	require.Contains(t, string(result.Body), "No issues found in the routing checks.")
	require.Contains(t, string(result.Body), "/root/review")
	require.Contains(t, string(result.Body), "/root")
	require.Contains(t, string(result.Body), "keep_pair")
	require.Contains(t, string(result.Body), `"metadata":{"id":"keep_metadata"}`)
	require.Contains(t, string(result.Body), "9007199254740993")
	require.Contains(t, string(result.Body), `"encrypted_content":"opaque user tool value"`)
	require.Equal(t, 1, result.RemovedItemIDs)
	require.Equal(t, 1, result.NormalizedAgentTextParts)
	require.Equal(t, 1, strings.Count(string(result.Body), "No issues found in the routing checks."))
	again, err := Filter(result.Body)
	require.NoError(t, err)
	require.JSONEq(t, string(result.Body), string(again.Body))
	require.Zero(t, again.RemovedItemIDs)
	require.Zero(t, again.NormalizedAgentTextParts)
}

func TestFilterPortableHistoryAgentEncryptedPayload(t *testing.T) {
	body := []byte(`{"input":[{"type":"agent_message","id":"amsg_foreign","author":"/root/child","recipient":"/root","content":[{"type":"input_text","text":"Agent reply:"},{"type":"encrypted_content","encrypted_content":"gAAAAAB-test-encrypted-agent-payload"}]}]}`)
	result, err := Filter(body)
	require.NoError(t, err)
	require.NotContains(t, string(result.Body), "amsg_foreign")
	require.Contains(t, string(result.Body), `"type":"encrypted_content"`)
	require.Contains(t, string(result.Body), "gAAAAAB-test-encrypted-agent-payload", "encrypted agent task/result content is not discardable reasoning")
	require.Contains(t, string(result.Body), "Agent reply:")
	require.Zero(t, result.RemovedReasoningItems)
	require.Equal(t, 1, result.RemovedItemIDs)
}

func TestFilterPortableHistoryToolSearchIDs(t *testing.T) {
	body := []byte(`{"input":[{"type":"tool_search_call","id":"tsc_foreign","call_id":"tsc_pair","execution":"client","arguments":{"query":"tools","id":"nested_keep"}},{"type":"tool_search_output","id":"tsc_result_foreign","call_id":"tsc_pair","execution":"client","tools":[{"type":"function","name":"run","parameters":{"type":"object","properties":{"id":{"type":"string"}}}}]}]}`)
	result, err := Filter(body)
	require.NoError(t, err)
	require.NotContains(t, string(result.Body), "tsc_foreign")
	require.NotContains(t, string(result.Body), "tsc_result_foreign")
	require.Equal(t, 2, strings.Count(string(result.Body), "tsc_pair"))
	require.Contains(t, string(result.Body), "nested_keep")
	require.Contains(t, string(result.Body), `"id":{"type":"string"}`)
	require.Equal(t, 2, result.RemovedItemIDs)
}

func TestFilterPortableHistoryOpaqueAgentTokensAndPlaintext(t *testing.T) {
	for _, value := range []string{"gAAAAAB-test-token", strings.Repeat("A", 90), strings.Repeat("eyJj", 20) + ".signature"} {
		t.Run("opaque_"+value[:6], func(t *testing.T) {
			body, err := json.Marshal(map[string]any{"input": []any{map[string]any{"type": "agent_message", "content": []any{map[string]any{"type": "encrypted_content", "encrypted_content": value}}}}})
			require.NoError(t, err)
			result, err := Filter(body)
			require.NoError(t, err)
			require.Zero(t, result.NormalizedAgentTextParts)
			require.Contains(t, string(result.Body), value)
		})
	}
	for _, value := range []string{"", "ok", "Plaintext task result.", "Routing checked.\nNo issues."} {
		t.Run("text_"+value, func(t *testing.T) {
			body, err := json.Marshal(map[string]any{"input": []any{map[string]any{"type": "agent_message", "content": []any{map[string]any{"type": "encrypted_content", "encrypted_content": value}}}}})
			require.NoError(t, err)
			result, err := Filter(body)
			require.NoError(t, err)
			require.Equal(t, 1, result.NormalizedAgentTextParts)
			require.NotContains(t, string(result.Body), "encrypted_content")
			var decoded map[string]any
			require.NoError(t, json.Unmarshal(result.Body, &decoded))
			part := decoded["input"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)
			require.Equal(t, "input_text", part["type"])
			require.Equal(t, value, part["text"])
		})
	}
}

func TestFilterPortableHistoryMalformedAgentPartsRemainUnchanged(t *testing.T) {
	body := []byte(`{"input":[{"type":"agent_message","content":[null,7,"opaque",{"type":"encrypted_content","encrypted_content":null},{"type":"encrypted_content","encrypted_content":42},{"type":"input_text","text":"text","encrypted_content":"user data"}]}]}`)
	result, err := Filter(body)
	require.NoError(t, err)
	require.Zero(t, result.NormalizedAgentTextParts)
	require.JSONEq(t, `{"store":false,"input":[{"type":"agent_message","content":[null,7,"opaque",{"type":"encrypted_content","encrypted_content":null},{"type":"encrypted_content","encrypted_content":42},{"type":"input_text","text":"text","encrypted_content":"user data"}]}]}`, string(result.Body))
}
