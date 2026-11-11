package service

import "encoding/json"

const openAIWSRetainedOutputLimitBytes = 32 << 20

// Admit complete output rounds before retaining them across WebSocket turns.
// Once exhausted, the connection keeps its existing tool-only replay behavior.
type openAIWSReplayOutputBudget struct {
	used      int
	exhausted bool
}

func (b *openAIWSReplayOutputBudget) admit(items []json.RawMessage) bool {
	if b.exhausted {
		return false
	}
	remaining := openAIWSRetainedOutputLimitBytes - b.used
	for _, item := range items {
		if cap(item) > remaining-64 {
			b.exhausted = true
			return false
		}
		remaining -= cap(item) + 64
	}
	b.used = openAIWSRetainedOutputLimitBytes - remaining
	return true
}
