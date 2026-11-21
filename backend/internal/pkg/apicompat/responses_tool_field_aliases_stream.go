package apicompat

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

const maxResponsesAliasedArguments = 4 << 20

type responsesFieldAliasStreamCall struct {
	name      string
	arguments strings.Builder
	complete  string
	emitted   bool
}

// Buffer only affected argument deltas. JSON keys can cross arbitrary SSE
// boundaries, so replacing substrings in individual deltas is not reversible.
func (r *ResponsesClientToolStreamRestorer) restoreFieldAliasEvent(payload []byte) ([][]byte, bool, error) {
	if len(payload) == 0 {
		return nil, false, nil
	}
	var event map[string]any
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	if err := decoder.Decode(&event); err != nil {
		return nil, false, err
	}
	typ := stringValue(event["type"])
	if !clientToolLifecycleEvent(typ) {
		return [][]byte{payload}, false, nil
	}
	if r.aliasCalls == nil {
		r.aliasCalls = make(map[string]*responsesFieldAliasStreamCall)
	}
	keys := []string{}
	addKey := func(prefix string, raw any) {
		if value, ok := raw.(string); ok && value != "" {
			keys = append(keys, prefix+value)
		}
	}
	addKey("item:", event["item_id"])
	addKey("call:", event["call_id"])
	if index, ok := event["output_index"].(json.Number); ok {
		keys = append(keys, "output:"+index.String())
	}
	item, _ := event["item"].(map[string]any)
	name := stringValue(event["name"])
	if item != nil {
		if stringValue(item["type"]) != "function_call" {
			return [][]byte{payload}, false, nil
		}
		name = stringValue(item["name"])
		addKey("item:", item["id"])
		addKey("call:", item["call_id"])
	}
	var call *responsesFieldAliasStreamCall
	for _, key := range keys {
		if known := r.aliasCalls[key]; known != nil {
			call = known
			break
		}
	}
	if call == nil && r.adapter.FieldAliases[name] != nil {
		call = &responsesFieldAliasStreamCall{name: name}
	}
	if call == nil {
		return [][]byte{payload}, false, nil
	}
	for _, key := range keys {
		r.aliasCalls[key] = call
	}
	if typ == "response.function_call_arguments.delta" {
		if call.emitted {
			return nil, false, fmt.Errorf("tool arguments continued after completion")
		}
		if call.arguments.Len()+len(stringValue(event["delta"])) > maxResponsesAliasedArguments {
			return nil, false, fmt.Errorf("aliased tool arguments exceed buffer limit")
		}
		_, _ = call.arguments.WriteString(stringValue(event["delta"]))
		return nil, true, nil
	}
	var out [][]byte
	encode := func(value map[string]any) error {
		encoded, err := json.Marshal(value)
		if err == nil {
			out = append(out, encoded)
		}
		return err
	}
	arguments := stringValue(event["arguments"])
	if item != nil {
		arguments = stringValue(item["arguments"])
	}
	if arguments == "" {
		arguments = call.arguments.String()
		if arguments == "" {
			arguments = call.complete
		}
	}
	if len(arguments) > maxResponsesAliasedArguments {
		return nil, false, fmt.Errorf("aliased tool arguments exceed buffer limit")
	}
	restored, err := r.adapter.FieldAliases.restore(call.name, arguments)
	if err != nil {
		return nil, false, err
	}
	if typ == "response.function_call_arguments.done" || typ == "response.output_item.done" {
		if !call.emitted && restored != "" {
			delta := copyClientTool(event)
			delete(delta, "arguments")
			delete(delta, "item")
			delta["type"], delta["delta"] = "response.function_call_arguments.delta", restored
			if item != nil {
				delta["item_id"], delta["call_id"], delta["name"] = item["id"], item["call_id"], item["name"]
			}
			if err := encode(delta); err != nil {
				return nil, false, err
			}
			call.emitted = true
		}
		call.arguments.Reset()
		call.complete = arguments
	}
	if item != nil {
		item["arguments"] = restored
	} else {
		event["arguments"] = restored
	}
	if typ == "response.output_item.added" && restored != "" {
		// Some providers send the entire call in the added event.
		call.emitted = true
		call.complete = arguments
	}
	if err := encode(event); err != nil {
		return nil, false, err
	}
	if typ == "response.output_item.done" {
		for key, known := range r.aliasCalls {
			if known == call {
				delete(r.aliasCalls, key)
			}
		}
	}
	return out, true, nil
}
