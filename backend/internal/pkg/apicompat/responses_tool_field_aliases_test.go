package apicompat

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func decodeAliasTest(t *testing.T, raw string) map[string]any {
	t.Helper()
	var value map[string]any
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	require.NoError(t, decoder.Decode(&value))
	return value
}

func TestResponsesToolFieldAliasesSchemaAndHistory(t *testing.T) {
	req := decodeAliasTest(t, `{"tools":[{"type":"function","name":"configure","parameters":{"type":"object","$defs":{"audio":{"type":"string","enum":["0","1"]}},"properties":{"audio":{"$ref":"#/$defs/audio"},"nested":{"type":"array","items":{"type":"object","properties":{"video":{"type":"string"},"required":{"type":"string","enum":["audio"]}},"required":["video"]}}},"required":["audio"],"dependentRequired":{"audio":["nested"]},"default":{"audio":"0"},"additionalProperties":false}}],"input":[{"type":"function_call","name":"configure","arguments":"{\"audio\":\"1\",\"nested\":[{\"video\":\"x\",\"required\":\"audio\",\"integer\":9007199254740993}]}"},{"type":"function_call_output","output":"{\"audio\":\"keep\"}"}]}`)
	aliases, changed, err := AliasResponsesToolMediaFields(req)
	require.NoError(t, err)
	require.True(t, changed)
	tools, ok := req["tools"].([]any)
	require.True(t, ok)
	tool, ok := tools[0].(map[string]any)
	require.True(t, ok)
	encoded, err := json.Marshal(tool["parameters"])
	require.NoError(t, err)
	require.JSONEq(t, `{"type":"object","$defs":{"__s2a_audio":{"type":"string","enum":["0","1"]}},"properties":{"__s2a_audio":{"$ref":"#/$defs/__s2a_audio"},"nested":{"type":"array","items":{"type":"object","properties":{"__s2a_video":{"type":"string"},"required":{"type":"string","enum":["audio"]}},"required":["__s2a_video"]}}},"required":["__s2a_audio"],"dependentRequired":{"__s2a_audio":["nested"]},"default":{"__s2a_audio":"0"},"additionalProperties":false}`, string(encoded))
	items, ok := req["input"].([]any)
	require.True(t, ok)
	call, ok := items[0].(map[string]any)
	require.True(t, ok)
	args, ok := call["arguments"].(string)
	require.True(t, ok)
	restored, err := aliases.restore("configure", args)
	require.NoError(t, err)
	require.JSONEq(t, `{"audio":"1","nested":[{"video":"x","required":"audio","integer":9007199254740993}]}`, restored)
	require.Contains(t, restored, "9007199254740993")
	result, ok := items[1].(map[string]any)
	require.True(t, ok)
	require.Equal(t, `{"audio":"keep"}`, result["output"])
}

func TestResponsesToolFieldAliasesDeclinesUnsafeSchemas(t *testing.T) {
	for _, extra := range []string{
		`"patternProperties":{".*":{"type":"string"}}`,
		`"propertyNames":{"enum":["audio"]}`,
		`"$ref":"https://example.com/schema"`,
		`"$dynamicRef":"#node"`,
		`"$id":"https://example.com/schema"`,
	} {
		req := decodeAliasTest(t, `{"tools":[{"type":"function","name":"f","parameters":{"type":"object","properties":{"audio":{"type":"string"}},`+extra+`}}]}`)
		before, _ := json.Marshal(req)
		_, changed, err := AliasResponsesToolMediaFields(req)
		require.NoError(t, err)
		require.False(t, changed, extra)
		after, _ := json.Marshal(req)
		require.Equal(t, before, after)
	}
}

func TestResponsesToolFieldAliasesInterleavedStream(t *testing.T) {
	r := NewResponsesClientToolStreamRestorer(ResponsesClientToolMapping{FieldAliases: ResponsesToolFieldAliases{
		"a": {"audio": "__s2a_audio", "video": "__s2a_video"},
		"b": {"audio": "__s2a_audio", "video": "__s2a_video"},
	}})
	events := []string{
		`{"type":"response.output_item.added","sequence_number":0,"output_index":0,"item":{"type":"function_call","id":"a","call_id":"ca","name":"a","arguments":""}}`,
		`{"type":"response.output_item.added","sequence_number":1,"output_index":1,"item":{"type":"function_call","id":"b","call_id":"cb","name":"b","arguments":""}}`,
		`{"type":"response.function_call_arguments.delta","sequence_number":2,"item_id":"a","delta":"{\"__s2a_"}`,
		`{"type":"response.output_text.delta","sequence_number":3,"delta":"audio is a setting"}`,
		`{"type":"response.function_call_arguments.delta","sequence_number":4,"call_id":"cb","delta":"{\"__s2a_video\":\"2\"}"}`,
		`{"type":"response.function_call_arguments.delta","sequence_number":5,"item_id":"a","delta":"audio\":\"1\"}"}`,
		`{"type":"response.function_call_arguments.done","sequence_number":6,"item_id":"b","provider_extra":"keep"}`,
		`{"type":"response.output_item.done","sequence_number":7,"output_index":1,"item":{"type":"function_call","id":"b","call_id":"cb","name":"b"}}`,
		`{"type":"response.output_item.done","sequence_number":8,"output_index":0,"item":{"type":"function_call","id":"a","call_id":"ca","name":"a"}}`,
	}
	var output []map[string]any
	for index, event := range events {
		payloads, _, err := r.RestoreEvent([]byte(event))
		require.NoError(t, err)
		if index == 3 {
			require.Len(t, payloads, 1, "text must not wait for argument completion")
		}
		for _, payload := range payloads {
			require.NotContains(t, string(payload), "__s2a_")
			output = append(output, decodeAliasTest(t, string(payload)))
		}
	}
	var deltas []string
	for index, event := range output {
		require.Equal(t, json.Number(strconv.Itoa(index)), event["sequence_number"])
		if event["type"] == "response.function_call_arguments.delta" {
			delta, ok := event["delta"].(string)
			require.True(t, ok)
			deltas = append(deltas, delta)
		}
		if event["type"] == "response.function_call_arguments.done" {
			require.Equal(t, "keep", event["provider_extra"])
			arguments, ok := event["arguments"].(string)
			require.True(t, ok)
			require.JSONEq(t, `{"video":"2"}`, arguments)
		}
	}
	require.Equal(t, []string{`{"video":"2"}`, `{"audio":"1"}`}, deltas)
	require.Empty(t, r.aliasCalls)
}

func TestResponsesToolFieldAliasesRejectMalformedAndAmbiguousArguments(t *testing.T) {
	aliases := ResponsesToolFieldAliases{"f": {"audio": "__s2a_audio"}}
	for _, args := range []string{`{"__s2a_audio":`, `{"audio":"0","__s2a_audio":"1"}`} {
		_, err := aliases.restore("f", args)
		require.Error(t, err)
	}
	value, err := aliases.restore("other", `{"__s2a_audio":"1"}`)
	require.NoError(t, err)
	require.Equal(t, `{"__s2a_audio":"1"}`, value)
}

func TestResponsesToolFieldAliasesRestoreEchoedDeclarations(t *testing.T) {
	mapping := ResponsesClientToolMapping{FieldAliases: ResponsesToolFieldAliases{"configure": {"audio": "__s2a_audio"}}}
	response := `{"status":"completed","tools":[{"type":"function","name":"configure","parameters":{"type":"object","properties":{"__s2a_audio":{"type":"string"}},"required":["__s2a_audio"]}}],"output":[],"opaque":9007199254740993}`
	restored, _, err := RestoreResponsesClientToolPayload([]byte(response), mapping)
	require.NoError(t, err)
	require.NotContains(t, string(restored), "__s2a_audio")
	require.Contains(t, string(restored), "9007199254740993")
	restorer := NewResponsesClientToolStreamRestorer(mapping)
	for index, typ := range []string{"response.created", "response.in_progress", "response.completed"} {
		payload := `{"type":"` + typ + `","sequence_number":` + strconv.Itoa(index) + `,"response":` + response + `}`
		out, _, err := restorer.RestoreEvent([]byte(payload))
		require.NoError(t, err)
		require.Len(t, out, 1)
		require.NotContains(t, string(out[0]), "__s2a_audio")
		require.Contains(t, string(out[0]), "9007199254740993")
	}
}

func TestResponsesToolFieldAliasesAvoidExistingRequestContent(t *testing.T) {
	for _, input := range []string{
		`"Earlier diagnostics mentioned __s2a_audio and __s2a_video."`,
		`[{"type":"function_call","name":"configure","arguments":"{\"__s2a_audio\":\"keep\",\"audio\":\"__s2a_audio\"}"},{"type":"function_call_output","output":"__s2a_video"}]`,
	} {
		req := decodeAliasTest(t, `{"tools":[{"type":"function","name":"configure","parameters":{"type":"object","properties":{"audio":{"type":"string"},"__s2a_audio":{"type":"string"}},"required":["audio","__s2a_audio"],"description":"__s2a_audio is a literal field"}}],"input":`+input+`}`)
		before, err := json.Marshal(req)
		require.NoError(t, err)
		aliases, changed, err := AliasResponsesToolMediaFields(req)
		require.NoError(t, err)
		require.True(t, changed, "existing alias text must not disable compatibility")
		require.Equal(t, "__s2a_audio_2", aliases["configure"]["audio"])
		encoded, err := json.Marshal(req)
		require.NoError(t, err)
		require.Contains(t, string(encoded), `"__s2a_audio_2":{"type":"string"}`)
		require.Contains(t, string(encoded), `"__s2a_audio":{"type":"string"}`)
		require.NoError(t, restoreToolFieldValue(req, aliases))
		restored, err := json.Marshal(req)
		require.NoError(t, err)
		require.JSONEq(t, string(before), string(restored))
	}
}

func TestResponsesToolFieldAliasesAvoidSeveralReservedNames(t *testing.T) {
	req := decodeAliasTest(t, `{"input":"__s2a_audio __s2a_audio_2 __s2a_audio_3 __s2a_video","tools":[{"type":"function","name":"f","parameters":{"properties":{"audio":{"type":"string"},"video":{"type":"string"}}}}]}`)
	aliases, changed, err := AliasResponsesToolMediaFields(req)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, "__s2a_audio_4", aliases["f"]["audio"])
	require.Equal(t, "__s2a_video_2", aliases["f"]["video"])
}
