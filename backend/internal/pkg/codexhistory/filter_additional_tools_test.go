package codexhistory

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestFilterStripsAdditionalToolsItemID(t *testing.T) {
	body := []byte(`{"model":"m","input":[` +
		`{"type":"additional_tools","id":"at_1","role":"developer","tools":[{"type":"function","name":"f","id":"keep"}]},` +
		`{"type":"local_shell_call","id":"lsh_1","call_id":"c1","action":{}},` +
		`{"type":"mcp_call","id":"mcp_1","name":"x"},` +
		`{"type":"unknown_future","id":"u_1"}]}`)
	result, err := Filter(body)
	require.NoError(t, err)
	require.Equal(t, 1, result.RemovedItemIDs)
	require.False(t, gjson.GetBytes(result.Body, "input.0.id").Exists())
	require.Equal(t, "keep", gjson.GetBytes(result.Body, "input.0.tools.0.id").String())
	require.Equal(t, "c1", gjson.GetBytes(result.Body, "input.1.call_id").String())
	require.Equal(t, "lsh_1", gjson.GetBytes(result.Body, "input.1.id").String())
	require.Equal(t, "mcp_1", gjson.GetBytes(result.Body, "input.2.id").String())
	require.Equal(t, "u_1", gjson.GetBytes(result.Body, "input.3.id").String())

	again, err := Filter(result.Body)
	require.NoError(t, err)
	require.Equal(t, string(result.Body), string(again.Body))
}
