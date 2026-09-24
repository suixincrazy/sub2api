package service

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// Tool names and schemas follow the isolated Claude CLI 2.1.281 capture.
// Only declarations are sent; account tests never execute tool calls.
//
//go:embed account_test_claude_tools.json
var accountTestClaudeToolsJSON []byte

func accountTestTextPrompt(prompt string) string {
	if value := strings.TrimSpace(prompt); value != "" {
		return value
	}
	return "Reply with exactly: OK"
}

func accountTestClaudeTools() ([]map[string]any, error) {
	var tools []map[string]any
	if err := json.Unmarshal(accountTestClaudeToolsJSON, &tools); err != nil {
		return nil, fmt.Errorf("load Claude account test tools: %w", err)
	}
	return tools, nil
}

func accountTestOpenAITools(chatCompletions bool) []map[string]any {
	tools := []map[string]any{
		{"type": "function", "name": "exec_command", "description": "Execute a shell command.",
			"parameters": map[string]any{"type": "object", "properties": map[string]any{"cmd": map[string]any{"type": "string"}}, "required": []string{"cmd"}, "additionalProperties": false}},
		{"type": "function", "name": "apply_patch", "description": "Apply a patch to files.",
			"parameters": map[string]any{"type": "object", "properties": map[string]any{"patch": map[string]any{"type": "string"}}, "required": []string{"patch"}, "additionalProperties": false}},
	}
	if chatCompletions {
		for i, tool := range tools {
			delete(tool, "type")
			tools[i] = map[string]any{"type": "function", "function": tool}
		}
	}
	return tools
}

// Keep the lightweight quota probe's payload unchanged; only interactive and
// scheduled account tests use the client tool declarations.
//
// Codex-gated relays (new-api "invalid codex request") reject Responses bodies
// that lack reasoning + include; the full Codex CLI field set is required.
func createOpenAIClientTestPayload(modelID string, isOAuth bool, prompt string) map[string]any {
	payload := createOpenAITestPayload(modelID, isOAuth)
	payload["input"] = []map[string]any{{"type": "message", "role": "user", "content": []map[string]any{{"type": "input_text", "text": accountTestTextPrompt(prompt)}}}}
	tools := accountTestOpenAITools(false)
	for _, tool := range tools {
		tool["strict"] = false
	}
	payload["tools"] = tools
	payload["tool_choice"] = "auto"
	payload["parallel_tool_calls"] = false
	payload["reasoning"] = map[string]any{"effort": "medium", "summary": "auto"}
	payload["store"] = false
	payload["include"] = []string{"reasoning.encrypted_content"}
	payload["prompt_cache_key"] = uuid.NewString()
	return payload
}
