// Package codexhistory makes complete Responses history portable across accounts.
package codexhistory

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
)

const Version = 2
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
	Body                  []byte
	RemovedReasoningItems int
	RemovedItemIDs        int
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
			case "message", "function_call", "function_call_output", "custom_tool_call", "custom_tool_call_output":
				portable = true
			}
			if _, hasID := item["id"]; hasID && portable {
				delete(item, "id")
				raw, _ = json.Marshal(item)
				result.RemovedItemIDs++
			}
			kept = append(kept, raw)
		}
		fields["input"], _ = json.Marshal(kept)
	}
	var err error
	result.Body, err = json.Marshal(fields)
	return result, err
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
