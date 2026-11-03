package service

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// anyrouter-style relays answer reasoning.summary="none" with 400 invalid_value.
func TestFilterOpenAIResponsesNoneReasoningSummaryForCompatAccount(t *testing.T) {
	compat := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"base_url": "https://compat.example"}}
	body := []byte(`{"model":"gpt-6.1-sol","reasoning":{"effort":"max","summary":"none"},"input":[]}`)
	out, err := filterOpenAIResponsesNoneReasoningEffortForAccount(compat, body)
	require.NoError(t, err)
	require.False(t, gjson.GetBytes(out, "reasoning.summary").Exists())
	require.Equal(t, "max", gjson.GetBytes(out, "reasoning.effort").String())

	again, err := filterOpenAIResponsesNoneReasoningEffortForAccount(compat, out)
	require.NoError(t, err)
	require.Equal(t, string(out), string(again))

	auto := []byte(`{"reasoning":{"effort":"max","summary":"auto"}}`)
	out, err = filterOpenAIResponsesNoneReasoningEffortForAccount(compat, auto)
	require.NoError(t, err)
	require.Equal(t, string(auto), string(out))

	official := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{}}
	out, err = filterOpenAIResponsesNoneReasoningEffortForAccount(official, body)
	require.NoError(t, err)
	require.Equal(t, string(body), string(out))
}

func TestDeleteOpenAIResponsesNoneReasoningSummaryFromObject(t *testing.T) {
	compat := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"base_url": "https://compat.example"}}
	body := map[string]any{"reasoning": map[string]any{"summary": "none"}}
	deleteOpenAIResponsesNoneReasoningEffortFromObject(compat, body)
	_, ok := body["reasoning"]
	require.False(t, ok)
}
