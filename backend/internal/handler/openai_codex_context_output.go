package handler

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
)

// codexContextNamespace is the namespace Codex Desktop uses for client-side
// context snapshots (for example codex_apps_open_page and
// codex_apps_selected_page_part_N_of_M). They are sent on every turn as
// function_call_output items with id/name/namespace but no call_id and no
// preceding function_call, so the content stays in the untrusted tool-output
// role instead of being promoted to user text.
const codexContextNamespace = "agent"

// codexContextCallIDPrefix marks call IDs synthesized by the gateway.
const codexContextCallIDPrefix = "call_ctx_"

// isCodexContextCallOutput reports whether item is a client-injected context
// snapshot that HTTP Responses upstreams cannot accept as-is.
func isCodexContextCallOutput(item map[string]any) bool {
	if stringField(item, "type") != "function_call_output" ||
		stringField(item, "namespace") != codexContextNamespace {
		return false
	}
	name := stringField(item, "name")
	if name == "" || strings.TrimSpace(name) != name {
		return false
	}
	callIDValue, exists := item["call_id"]
	if !exists {
		return true
	}
	callID, ok := callIDValue.(string)
	return ok && strings.TrimSpace(callID) == ""
}

// pairCodexContextCallOutputs rewrites each Codex context snapshot into a
// function_call + function_call_output pair sharing a deterministic call_id.
// The HTTP Responses API rejects outputs without call_id, and an output whose
// call_id has no matching call is rejected upstream as an orphan. Call IDs are
// derived from the item content so replayed history stays byte-stable for
// prompt caching. Returns the number of pairs created.
func pairCodexContextCallOutputs(body []byte) ([]byte, int) {
	if !mayContainCodexContextCallOutput(body) || !hasUniqueJSONMembers(body) {
		return body, 0
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var request map[string]any
	if err := decoder.Decode(&request); err != nil {
		return body, 0
	}
	input, ok := request["input"].([]any)
	if !ok {
		return body, 0
	}

	usedCallIDs := make(map[string]struct{})
	candidates := 0
	for _, raw := range input {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if isCodexContextCallOutput(item) {
			candidates++
			continue
		}
		if callID := strings.TrimSpace(stringField(item, "call_id")); callID != "" {
			usedCallIDs[callID] = struct{}{}
		}
	}
	if candidates == 0 {
		return body, 0
	}

	rewritten := make([]any, 0, len(input)+candidates)
	paired := 0
	for _, raw := range input {
		item, ok := raw.(map[string]any)
		if !ok || !isCodexContextCallOutput(item) {
			rewritten = append(rewritten, raw)
			continue
		}
		name := stringField(item, "name")
		callID := codexContextCallID(item, usedCallIDs)
		usedCallIDs[callID] = struct{}{}

		output := make(map[string]any, len(item))
		for key, value := range item {
			switch key {
			case "id", "name", "namespace", "call_id":
				continue
			}
			output[key] = value
		}
		output["call_id"] = callID

		rewritten = append(rewritten,
			map[string]any{
				"type":      "function_call",
				"call_id":   callID,
				"name":      name,
				"namespace": codexContextNamespace,
				"arguments": "{}",
			},
			output,
		)
		paired++
	}

	request["input"] = rewritten
	normalized, err := json.Marshal(request)
	if err != nil {
		return body, 0
	}
	return normalized, paired
}

// mayContainCodexContextCallOutput is a cheap necessary condition checked
// before decoding; a \u escape could spell the markers, so it defers to the
// full scan in that case.
func mayContainCodexContextCallOutput(body []byte) bool {
	if bytes.Contains(body, []byte(`\u`)) {
		return true
	}
	return bytes.Contains(body, []byte("function_call_output")) &&
		bytes.Contains(body, []byte(`"`+codexContextNamespace+`"`))
}

func codexContextCallID(item map[string]any, used map[string]struct{}) string {
	hash := sha256.New()
	hash.Write([]byte(stringField(item, "name")))
	hash.Write([]byte{0})
	if id := strings.TrimSpace(stringField(item, "id")); id != "" {
		hash.Write([]byte("id:" + id))
	} else {
		encoded, _ := json.Marshal(item["output"])
		hash.Write([]byte("output:"))
		hash.Write(encoded)
	}
	base := codexContextCallIDPrefix + hex.EncodeToString(hash.Sum(nil))[:24]
	candidate := base
	for n := 2; ; n++ {
		if _, taken := used[candidate]; !taken {
			return candidate
		}
		candidate = base + "_" + strconv.Itoa(n)
	}
}
