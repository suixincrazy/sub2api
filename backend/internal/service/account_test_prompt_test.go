package service

import (
	"encoding/json"
	"testing"
)

func TestCreateOpenAITestPayloadUsesCustomPrompt(t *testing.T) {
	payload := createOpenAITestPayload("gpt-5.6", true, "  Reply with pong  ")
	input, ok := payload["input"].([]map[string]any)
	if !ok || len(input) == 0 {
		t.Fatalf("unexpected OpenAI test input: %#v", payload["input"])
	}
	content, ok := input[0]["content"].([]map[string]any)
	if !ok || len(content) == 0 {
		t.Fatalf("unexpected OpenAI test content: %#v", input[0]["content"])
	}
	if got := content[0]["text"]; got != "Reply with pong" {
		t.Fatalf("custom OpenAI test prompt was not preserved: %#v", got)
	}
}

func TestCreateTestPayloadUsesDefaultAndCustomPrompt(t *testing.T) {
	defaultPayload, err := createTestPayload("claude-sonnet-4-6", "")
	if err != nil {
		t.Fatalf("create default payload: %v", err)
	}
	if got := claudeTestPayloadPrompt(t, defaultPayload); got != "hi" {
		t.Fatalf("expected default prompt, got %q", got)
	}

	customPayload, err := createTestPayload("claude-sonnet-4-6", "Summarize this")
	if err != nil {
		t.Fatalf("create custom payload: %v", err)
	}
	if got := claudeTestPayloadPrompt(t, customPayload); got != "Summarize this" {
		t.Fatalf("expected custom prompt, got %q", got)
	}
}

func TestBuildGrokManualTestBodyUsesCustomPrompt(t *testing.T) {
	body, err := buildGrokManualTestBody("grok-4", "Reply with pong")
	if err != nil {
		t.Fatalf("build Grok test body: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode Grok test body: %v", err)
	}
	if got := payload["input"]; got != "Reply with pong" {
		t.Fatalf("custom Grok test prompt was not preserved: %#v", got)
	}
}

func claudeTestPayloadPrompt(t *testing.T, payload map[string]any) string {
	t.Helper()
	messages, ok := payload["messages"].([]map[string]any)
	if !ok || len(messages) == 0 {
		t.Fatalf("unexpected Claude test messages: %#v", payload["messages"])
	}
	content, ok := messages[0]["content"].([]map[string]any)
	if !ok || len(content) == 0 {
		t.Fatalf("unexpected Claude test content: %#v", messages[0]["content"])
	}
	prompt, _ := content[0]["text"].(string)
	return prompt
}
