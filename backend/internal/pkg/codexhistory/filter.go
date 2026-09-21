// Package codexhistory makes complete Responses history portable across accounts.
package codexhistory

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
)

const Version = 3
const MaxBodySize int64 = 64 << 20

type enabledKey struct{}

func WithEnabled(ctx context.Context) context.Context {
	return context.WithValue(ctx, enabledKey{}, true)
}

func Enabled(ctx context.Context) bool {
	return ctx != nil && ctx.Value(enabledKey{}) == true
}

type Error struct {
	Code    string
	Message string
	Status  int
}

func (e *Error) Error() string { return e.Message }

func Invalid(code, message string) *Error {
	return &Error{Code: code, Message: message, Status: http.StatusBadRequest}
}

type Result struct {
	Body                     []byte
	RemovedReasoningItems    int
	RemovedItemIDs           int
	NormalizedAgentTextParts int
}

// Filter only removes replayable item IDs, never call_id or nested resource IDs.
// RawMessage retains opaque tool contents and large numeric values losslessly.
func Filter(body []byte) (Result, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil || fields == nil {
		return Result{}, Invalid("invalid_request_json", "Responses request must be a JSON object.")
	}
	if truthy(fields["previous_response_id"]) || truthy(fields["conversation"]) {
		return Result{}, Invalid("response_reference_not_portable", "Complete message/tool history is required, not previous_response_id or conversation references. Start a new session with a text summary.")
	}
	if include, ok := fields["include"]; ok && !isArray(include) {
		return Result{}, Invalid("invalid_include", "include must be an array.")
	}
	if input, ok := fields["input"]; ok && !isArray(input) && !isString(input) {
		return Result{}, Invalid("invalid_input", "input must be a string or an array.")
	}
	if isArray(fields["context_management"]) {
		// Non-object entries are allowed by the original filter and handled later.
		var rawItems []json.RawMessage
		_ = json.Unmarshal(fields["context_management"], &rawItems)
		for _, raw := range rawItems {
			var item map[string]json.RawMessage
			_ = json.Unmarshal(raw, &item)
			if stringValue(item["type"]) == "compaction" {
				return Result{}, Invalid("encrypted_compaction_disabled", "Encrypted compaction is disabled by this filter. Use a text summary and start a new session.")
			}
		}
	}
	result := Result{}
	fields["store"] = json.RawMessage("false")
	if include, ok := fields["include"]; ok {
		var values []json.RawMessage
		_ = json.Unmarshal(include, &values)
		kept := make([]json.RawMessage, 0, len(values))
		for _, value := range values {
			if stringValue(value) != "reasoning.encrypted_content" {
				kept = append(kept, value)
			}
		}
		fields["include"], _ = json.Marshal(kept)
	}
	if input := fields["input"]; isArray(input) {
		var values []json.RawMessage
		_ = json.Unmarshal(input, &values)
		kept := make([]json.RawMessage, 0, len(values))
		for _, raw := range values {
			var item map[string]json.RawMessage
			if json.Unmarshal(raw, &item) != nil || item == nil {
				kept = append(kept, raw)
				continue
			}
			kind := stringValue(item["type"])
			if kind == "reasoning" {
				result.RemovedReasoningItems++
				continue
			}
			if kind == "compaction" || kind == "item_reference" || truthy(item["encrypted_content"]) {
				return Result{}, Invalid("encrypted_context_not_portable", "Encrypted compaction/context references cannot safely cross accounts. Start a new session with a text summary; history has NOT been silently discarded.")
			}
			_, hasType := item["type"]
			_, hasContent := item["content"]
			portable := !hasType && isString(item["role"]) && hasContent
			switch kind {
			case "message", "function_call", "function_call_output", "custom_tool_call", "custom_tool_call_output",
				"web_search_call", "agent_message", "tool_search_call", "tool_search_output":
				portable = true
			}
			changed := false
			if _, hasID := item["id"]; hasID && portable {
				delete(item, "id")
				result.RemovedItemIDs++
				changed = true
			}
			if kind == "agent_message" {
				if count := normalizeAgentTextContent(item); count > 0 {
					result.NormalizedAgentTextParts += count
					changed = true
				}
			}
			if changed {
				raw, _ = json.Marshal(item)
			}
			kept = append(kept, raw)
		}
		fields["input"], _ = json.Marshal(kept)
	}
	var err error
	result.Body, err = json.Marshal(fields)
	return result, err
}

// Some custom providers store visible agent text in encrypted_content parts.
// Opaque agent payloads carry task/result content, so retain them losslessly.
func normalizeAgentTextContent(item map[string]json.RawMessage) int {
	if !isArray(item["content"]) {
		return 0
	}
	var parts []json.RawMessage
	_ = json.Unmarshal(item["content"], &parts)
	count := 0
	for i, raw := range parts {
		var part map[string]json.RawMessage
		if json.Unmarshal(raw, &part) != nil || stringValue(part["type"]) != "encrypted_content" || !isString(part["encrypted_content"]) {
			continue
		}
		text := stringValue(part["encrypted_content"])
		if opaqueAgentContent(text) {
			continue
		}
		part["type"] = json.RawMessage(`"input_text"`)
		part["text"] = part["encrypted_content"]
		delete(part, "encrypted_content")
		parts[i], _ = json.Marshal(part)
		count++
	}
	if count > 0 {
		item["content"], _ = json.Marshal(parts)
	}
	return count
}

func opaqueAgentContent(text string) bool {
	text = strings.TrimSpace(text)
	if strings.HasPrefix(text, "gAAAA") {
		return true
	}
	// Preserve unknown encoded tokens instead of guessing they are visible text.
	return len(text) >= 64 && strings.IndexFunc(text, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("_-/+=.:", r))
	}) == -1
}

// Apply enforces the same policy after account-specific request normalization.
func Apply(ctx context.Context, body []byte) ([]byte, error) {
	if !Enabled(ctx) {
		return body, nil
	}
	result, err := Filter(body)
	return result.Body, err
}

func isArray(raw json.RawMessage) bool {
	return bytes.HasPrefix(bytes.TrimSpace(raw), []byte("["))
}

func isString(raw json.RawMessage) bool {
	return bytes.HasPrefix(bytes.TrimSpace(raw), []byte(`"`))
}

func stringValue(raw json.RawMessage) string {
	var value string
	_ = json.Unmarshal(raw, &value)
	return value
}

func truthy(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) || bytes.Equal(raw, []byte("false")) {
		return false
	}
	if isString(raw) {
		return stringValue(raw) != ""
	}
	var number float64
	if json.Unmarshal(raw, &number) == nil {
		return number != 0
	}
	return true
}
