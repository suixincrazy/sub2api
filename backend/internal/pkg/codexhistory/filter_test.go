package codexhistory

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Generated from the user's restored proxy.cjs v2, not from the Go implementation.
func TestFilterMatchesOriginalV2(t *testing.T) {
	data, err := os.ReadFile("testdata/reference-v2.json")
	require.NoError(t, err)
	var cases []struct {
		Name                  string          `json:"name"`
		Input                 json.RawMessage `json:"input"`
		Output                json.RawMessage `json:"output"`
		ErrorCode             string          `json:"error_code"`
		Status                int             `json:"status"`
		RemovedReasoningItems int             `json:"removed_reasoning_items"`
		RemovedItemIDs        int             `json:"removed_item_ids"`
	}
	require.NoError(t, json.Unmarshal(data, &cases))
	for _, tt := range cases {
		t.Run(tt.Name, func(t *testing.T) {
			before := string(tt.Input)
			result, err := Filter(tt.Input)
			require.Equal(t, before, string(tt.Input))
			if tt.ErrorCode != "" {
				var filterErr *Error
				require.ErrorAs(t, err, &filterErr)
				require.Equal(t, tt.ErrorCode, filterErr.Code)
				require.Equal(t, tt.Status, filterErr.Status)
				require.Nil(t, result.Body)
				return
			}
			require.NoError(t, err)
			require.JSONEq(t, string(tt.Output), string(result.Body))
			require.Equal(t, tt.RemovedReasoningItems, result.RemovedReasoningItems)
			require.Equal(t, tt.RemovedItemIDs, result.RemovedItemIDs)
			again, err := Filter(result.Body)
			require.NoError(t, err)
			require.JSONEq(t, string(result.Body), string(again.Body))
			require.Zero(t, again.RemovedReasoningItems)
			require.Zero(t, again.RemovedItemIDs)
		})
	}
}

func TestFilterPreservesOpaqueNumbersAndToolData(t *testing.T) {
	body := []byte(`{"metadata":{"integer":9007199254740993},"input":[{"type":"function_call","id":"old","call_id":"pair","arguments":"{\"id\":9007199254740993}"},{"type":"function_call_output","call_id":"pair","output":{"id":9007199254740993}}],"reasoning":{"effort":"xhigh"}}`)
	result, err := Filter(body)
	require.NoError(t, err)
	require.Equal(t, 3, strings.Count(string(result.Body), "9007199254740993"))
	require.NotContains(t, string(result.Body), `"old"`)
	require.Contains(t, string(result.Body), `"effort":"xhigh"`)
}

func TestFilterMalformedJSONAndDisabledContext(t *testing.T) {
	for _, raw := range []string{"", "{", "{} {}"} {
		_, err := Filter([]byte(raw))
		var filterErr *Error
		require.ErrorAs(t, err, &filterErr)
		require.Equal(t, "invalid_request_json", filterErr.Code)
		body, err := Apply(context.Background(), []byte(raw))
		require.NoError(t, err)
		require.Equal(t, raw, string(body))
	}
	ctx, cancel := context.WithCancel(WithEnabled(context.Background()))
	cancel()
	require.True(t, Enabled(ctx))
	require.ErrorIs(t, ctx.Err(), context.Canceled)
	filtered, err := Apply(ctx, []byte(`{"store":true,"input":[{"type":"reasoning"}]}`))
	require.NoError(t, err)
	require.JSONEq(t, `{"store":false,"input":[]}`, string(filtered))
}
