package handler

import (
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// Shape captured from Codex Desktop 26.1103 (cli 0.167.0-alpha.3.2) rollouts.
const codexOpenPageItem = `{"type":"function_call_output","id":"fco_01a1c4d6-0af3-7590-8490-96988589f87d","name":"codex_apps_open_page","namespace":"agent","output":"{\"page_id\":null}","internal_chat_message_metadata_passthrough":{"turn_id":"01a1c4d6-061f-7e30-9ece-62a257672954","create_time":1794303724.275169}}`
const codexSelectedPageItem = `{"type":"function_call_output","id":"fco_01a1c4d6-0af5-7220-b4dd-63e4e90c6c72","name":"codex_apps_selected_page_part_1_of_1","namespace":"agent","output":"Companion Page snapshot part 1 of 1:\n{\"page_id\":null,\"document_view\":null}"}`
const codexUserItem = `{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}`

func codexContextBody(items ...string) []byte {
	return []byte(`{"model":"gpt-6-astra","stream":true,"input":[` + strings.Join(items, ",") + `]}`)
}

func TestPairCodexContextCallOutputsDesktopShape(t *testing.T) {
	body := codexContextBody(codexOpenPageItem, `{"type":"message","role":"developer","content":[{"type":"input_text","text":"x"}]}`, codexSelectedPageItem, codexUserItem)
	before := service.ValidateFunctionCallOutputContextBytes(body)
	require.True(t, before.HasFunctionCallOutputMissingCallID)
	require.False(t, before.HasToolCallContext)

	got, paired := pairCodexContextCallOutputs(body)
	require.Equal(t, 2, paired)

	input := gjson.GetBytes(got, "input").Array()
	require.Len(t, input, 6)
	types := make([]string, 0, len(input))
	for _, item := range input {
		types = append(types, item.Get("type").String())
	}
	require.Equal(t, []string{"function_call", "function_call_output", "message", "function_call", "function_call_output", "message"}, types)

	for _, pair := range [][2]int{{0, 1}, {3, 4}} {
		call, output := input[pair[0]], input[pair[1]]
		callID := call.Get("call_id").String()
		require.True(t, strings.HasPrefix(callID, codexContextCallIDPrefix))
		require.Equal(t, callID, output.Get("call_id").String())
		require.Equal(t, "agent", call.Get("namespace").String())
		require.Equal(t, "{}", call.Get("arguments").String())
		require.False(t, output.Get("id").Exists())
		require.False(t, output.Get("name").Exists())
		require.False(t, output.Get("namespace").Exists())
	}
	require.Equal(t, "codex_apps_open_page", input[0].Get("name").String())
	require.Equal(t, `{"page_id":null}`, input[1].Get("output").String())
	require.Equal(t, "codex_apps_selected_page_part_1_of_1", input[3].Get("name").String())
	require.Equal(t, "Companion Page snapshot part 1 of 1:\n{\"page_id\":null,\"document_view\":null}", input[4].Get("output").String())
	require.NotEqual(t, input[0].Get("call_id").String(), input[3].Get("call_id").String())
	require.Equal(t, "1794303724.275169", input[1].Get("internal_chat_message_metadata_passthrough.create_time").Raw)
	require.Equal(t, "hi", input[5].Get("content.0.text").String())
	require.True(t, gjson.GetBytes(got, "stream").Bool())

	after := service.ValidateFunctionCallOutputContextBytes(got)
	require.False(t, after.HasFunctionCallOutputMissingCallID)
	require.True(t, after.HasToolCallContext)
	require.True(t, service.AnalyzeToolCallOutputContextCoverageBytes(got).ContextCoversAllCallIDs)
}

func TestPairCodexContextCallOutputsDeterministicAndIdempotent(t *testing.T) {
	body := codexContextBody(codexOpenPageItem, codexSelectedPageItem, codexUserItem)
	first, paired := pairCodexContextCallOutputs(body)
	require.Equal(t, 2, paired)
	second, pairedAgain := pairCodexContextCallOutputs(body)
	require.Equal(t, 2, pairedAgain)
	require.Equal(t, first, second)

	again, none := pairCodexContextCallOutputs(first)
	require.Zero(t, none)
	require.Equal(t, first, again)
}

func TestPairCodexContextCallOutputsRepeatedSnapshotsGetDistinctIDs(t *testing.T) {
	noID := `{"type":"function_call_output","name":"codex_apps_open_page","namespace":"agent","output":"{\"page_id\":null}"}`
	body := codexContextBody(noID, codexUserItem, noID, codexUserItem)
	got, paired := pairCodexContextCallOutputs(body)
	require.Equal(t, 2, paired)
	first := gjson.GetBytes(got, "input.0.call_id").String()
	second := gjson.GetBytes(got, "input.3.call_id").String()
	require.NotEqual(t, first, second)
	require.Equal(t, first+"_2", second)
	require.Equal(t, second, gjson.GetBytes(got, "input.4.call_id").String())
}

func TestPairCodexContextCallOutputsAvoidsExistingCallIDs(t *testing.T) {
	_, paired := pairCodexContextCallOutputs(codexContextBody(codexOpenPageItem))
	require.Equal(t, 1, paired)
	probe, _ := pairCodexContextCallOutputs(codexContextBody(codexOpenPageItem))
	taken := gjson.GetBytes(probe, "input.0.call_id").String()

	body := codexContextBody(`{"type":"function_call","call_id":"`+taken+`","name":"exec","arguments":"{}"}`, `{"type":"function_call_output","call_id":"`+taken+`","output":"ok"}`, codexOpenPageItem)
	got, paired := pairCodexContextCallOutputs(body)
	require.Equal(t, 1, paired)
	require.Equal(t, taken, gjson.GetBytes(got, "input.1.call_id").String())
	require.Equal(t, taken+"_2", gjson.GetBytes(got, "input.2.call_id").String())
	require.Equal(t, taken+"_2", gjson.GetBytes(got, "input.3.call_id").String())
}

func TestPairCodexContextCallOutputsIgnoresOtherShapes(t *testing.T) {
	tests := []struct {
		name string
		body []byte
	}{
		{name: "no tool output", body: codexContextBody(codexUserItem)},
		{name: "other namespace", body: codexContextBody(`{"type":"function_call_output","name":"x","namespace":"codex_app","output":"y"}`)},
		{name: "no namespace", body: codexContextBody(`{"type":"function_call_output","name":"x","output":"y"}`)},
		{name: "missing name", body: codexContextBody(`{"type":"function_call_output","namespace":"agent","output":"y"}`)},
		{name: "padded name", body: codexContextBody(`{"type":"function_call_output","name":" x ","namespace":"agent","output":"y"}`)},
		{name: "has call id", body: codexContextBody(`{"type":"function_call","call_id":"c1","name":"x","namespace":"agent","arguments":"{}"}`, `{"type":"function_call_output","call_id":"c1","name":"x","namespace":"agent","output":"y"}`)},
		{name: "non string call id", body: codexContextBody(`{"type":"function_call_output","call_id":7,"name":"x","namespace":"agent","output":"y"}`)},
		{name: "custom tool output", body: codexContextBody(`{"type":"custom_tool_call_output","name":"x","namespace":"agent","output":"y"}`)},
		{name: "string input", body: []byte(`{"model":"m","input":"function_call_output \"agent\""}`)},
		{name: "duplicate members", body: []byte(`{"model":"m","input":[` + codexOpenPageItem + `],"input":[]}`)},
		{name: "invalid json", body: []byte(`{"input":[{"type":"function_call_output","namespace":"agent"`)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, paired := pairCodexContextCallOutputs(tt.body)
			require.Zero(t, paired)
			require.Equal(t, tt.body, got)
		})
	}
}

func TestPairCodexContextCallOutputsKeepsPreviousResponseID(t *testing.T) {
	body := []byte(`{"model":"m","previous_response_id":"resp_1","input":[` + codexOpenPageItem + `]}`)
	got, paired := pairCodexContextCallOutputs(body)
	require.Equal(t, 1, paired)
	require.Equal(t, "resp_1", gjson.GetBytes(got, "previous_response_id").String())
}

func TestPairCodexContextCallOutputsMultipartPreservesToolRole(t *testing.T) {
	first := `{"type":"function_call_output","name":"codex_apps_selected_page_part_1_of_2","namespace":"agent","output":"Companion Page snapshot part 1 of 2:\n{\"page_id\":\"page1\",\"document_view\":\"hello"}`
	second := `{"type":"function_call_output","name":"codex_apps_selected_page_part_2_of_2","namespace":"agent","output":"Companion Page snapshot part 2 of 2:\n world\"}"}`
	body := []byte(`{"model":"gpt-6-astra","metadata":{"large":9007199254740993},"input":[` + first + `,` + second + `]}`)
	got, paired := pairCodexContextCallOutputs(body)
	require.Equal(t, 2, paired)
	require.Equal(t, "9007199254740993", gjson.GetBytes(got, "metadata.large").Raw)
	items := gjson.GetBytes(got, "input").Array()
	require.Len(t, items, 4)
	for i, original := range []string{first, second} {
		output := items[i*2+1]
		require.Equal(t, "function_call_output", output.Get("type").String())
		require.False(t, output.Get("role").Exists())
		require.Equal(t, gjson.Get(original, "output").String(), output.Get("output").String())
	}
	again, paired := pairCodexContextCallOutputs(got)
	require.Zero(t, paired)
	require.Equal(t, got, again)
}

func TestCodexBootstrapNormalizersCoexistWithContextOutputs(t *testing.T) {
	automation := `{"type":"function_call_output","namespace":"codex_app","name":"automation_update","output":` +
		mustJSON(t, `<heartbeat><automation_id>wiki</automation_id></heartbeat>`) + `}`
	delegation := `{"type":"function_call_output","namespace":"codex_app","name":"create_thread","output":"` + delegationEnvelope + `"}`

	for name, candidate := range map[string]string{"automation": automation, "delegation": delegation} {
		t.Run(name, func(t *testing.T) {
			body := codexContextBody(codexOpenPageItem, codexSelectedPageItem, candidate)
			var changed bool
			if name == "automation" {
				body, changed = normalizeCodexAutomationBootstrap(body)
			} else {
				body, changed = normalizeCodexDelegationBootstrap(body)
			}
			require.True(t, changed)
			require.Equal(t, "message", gjson.GetBytes(body, "input.2.type").String())
			require.Equal(t, "user", gjson.GetBytes(body, "input.2.role").String())
			require.Equal(t, "agent", gjson.GetBytes(body, "input.0.namespace").String())

			got, paired := pairCodexContextCallOutputs(body)
			require.Equal(t, 2, paired)
			require.Len(t, gjson.GetBytes(got, "input").Array(), 5)
			require.Equal(t, "message", gjson.GetBytes(got, "input.4.type").String())
			validation := service.ValidateFunctionCallOutputContextBytes(got)
			require.False(t, validation.HasFunctionCallOutputMissingCallID)
			require.True(t, validation.HasToolCallContext)
		})
	}
}
