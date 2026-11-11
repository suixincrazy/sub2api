package service

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOpenAIWSReplayOutputBudgetBoundsRetainedRounds(t *testing.T) {
	var budget openAIWSReplayOutputBudget
	item := json.RawMessage(make([]byte, 1024))
	for budget.used+cap(item)+64 <= openAIWSRetainedOutputLimitBytes {
		require.True(t, budget.admit([]json.RawMessage{item}))
	}
	used := budget.used
	require.False(t, budget.admit([]json.RawMessage{item}))
	require.True(t, budget.exhausted)
	for range 100 {
		require.False(t, budget.admit([]json.RawMessage{json.RawMessage(`{}`)}))
	}
	require.Equal(t, used, budget.used)
	require.LessOrEqual(t, budget.used, openAIWSRetainedOutputLimitBytes)
}

func TestOpenAIWSReplayOutputBudgetRejectsWholeRound(t *testing.T) {
	budget := openAIWSReplayOutputBudget{used: openAIWSRetainedOutputLimitBytes - 100}
	used := budget.used
	require.False(t, budget.admit([]json.RawMessage{json.RawMessage(`{}`), json.RawMessage(`{}`)}))
	require.Equal(t, used, budget.used)
	retained := make([]byte, 1, 1024)
	budget = openAIWSReplayOutputBudget{used: openAIWSRetainedOutputLimitBytes - 100}
	require.False(t, budget.admit([]json.RawMessage{retained}), "capacity accounts for the retained backing buffer")
}
