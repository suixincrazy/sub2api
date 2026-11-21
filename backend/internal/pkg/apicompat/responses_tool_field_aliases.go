package apicompat

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// ResponsesToolFieldAliases is request-local. No account identity or provider
// configuration is needed to reverse a compatibility retry.
type ResponsesToolFieldAliases map[string]map[string]string

// AliasResponsesToolMediaFields handles providers that mistake schema property
// names for media. Only declared function schemas and their argument history are
// changed; content blocks and tool results are never treated as schemas.
func AliasResponsesToolMediaFields(req map[string]any) (ResponsesToolFieldAliases, bool, error) {
	aliases := make(ResponsesToolFieldAliases)
	var declarations []map[string]any
	var collect func(any)
	collect = func(value any) {
		items, _ := value.([]any)
		for _, raw := range items {
			tool, _ := raw.(map[string]any)
			switch stringValue(tool["type"]) {
			case "namespace":
				collect(tool["tools"])
			case "function":
				declarations = append(declarations, tool)
			}
		}
	}
	collect(req["tools"])
	for _, tool := range declarations {
		name := stringValue(tool["name"])
		schema, _ := tool["parameters"].(map[string]any)
		if name == "" || schema == nil || !hasMediaSchemaProperty(schema) {
			continue
		}
		// Regex property constraints and remote references cannot be renamed
		// without changing their meaning. Leave the original rejection intact.
		if !aliasableToolSchema(schema) {
			return nil, false, nil
		}
		if _, duplicate := aliases[name]; duplicate {
			return nil, false, nil
		}
		aliases[name] = map[string]string{"audio": "__s2a_audio", "video": "__s2a_video"}
	}
	if len(aliases) == 0 {
		return nil, false, nil
	}
	// A reserved alias anywhere in this request is ambiguous, including inside
	// string-encoded historical arguments. Fail closed rather than overwrite.
	encoded, err := json.Marshal(req)
	if err != nil {
		return nil, false, err
	}
	if bytes.Contains(encoded, []byte("__s2a_audio")) || bytes.Contains(encoded, []byte("__s2a_video")) {
		return nil, false, nil
	}
	for _, tool := range declarations {
		if names := aliases[stringValue(tool["name"])]; names != nil {
			renameToolSchema(tool["parameters"], names)
		}
	}
	items, _ := req["input"].([]any)
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		if stringValue(item["type"]) != "function_call" {
			continue
		}
		if names := aliases[stringValue(item["name"])]; names != nil {
			arguments, err := renameToolArguments(stringValue(item["arguments"]), names)
			if err != nil {
				return nil, false, err
			}
			item["arguments"] = arguments
		}
	}
	return aliases, true, nil
}

func hasMediaSchemaProperty(value any) bool {
	switch v := value.(type) {
	case map[string]any:
		if properties, ok := v["properties"].(map[string]any); ok {
			if _, ok := properties["audio"]; ok {
				return true
			}
			if _, ok := properties["video"]; ok {
				return true
			}
		}
		for _, child := range v {
			if hasMediaSchemaProperty(child) {
				return true
			}
		}
	case []any:
		for _, child := range v {
			if hasMediaSchemaProperty(child) {
				return true
			}
		}
	}
	return false
}

func aliasableToolSchema(value any) bool {
	switch v := value.(type) {
	case map[string]any:
		for key, child := range v {
			switch key {
			case "patternProperties", "propertyNames", "$id", "$dynamicRef", "$recursiveRef":
				return false
			case "$ref":
				if ref, ok := child.(string); !ok || !strings.HasPrefix(ref, "#") {
					return false
				}
			}
			if !aliasableToolSchema(child) {
				return false
			}
		}
	case []any:
		for _, child := range v {
			if !aliasableToolSchema(child) {
				return false
			}
		}
	}
	return true
}

func renamePropertyList(value any, names map[string]string) {
	list, _ := value.([]any)
	for i, raw := range list {
		if name, ok := raw.(string); ok && names[name] != "" {
			list[i] = names[name]
		}
	}
}

func renameToolSchema(value any, names map[string]string) {
	schema, ok := value.(map[string]any)
	if !ok {
		return
	}
	for key, child := range schema {
		switch key {
		case "properties", "$defs", "definitions", "dependentSchemas", "dependencies":
			if children, ok := child.(map[string]any); ok {
				for _, nested := range children {
					if key == "dependencies" {
						renamePropertyList(nested, names)
					}
					renameToolSchema(nested, names)
				}
				renameObjectKeys(children, names)
			}
		case "required":
			renamePropertyList(child, names)
		case "dependentRequired":
			if children, ok := child.(map[string]any); ok {
				for _, nested := range children {
					renamePropertyList(nested, names)
				}
				renameObjectKeys(children, names)
			}
		case "$ref":
			if ref, ok := child.(string); ok && strings.HasPrefix(ref, "#/") {
				parts := strings.Split(ref, "/")
				for i := 1; i < len(parts); i++ {
					if i > 1 {
						switch parts[i-1] {
						case "properties", "$defs", "definitions", "dependentSchemas", "dependencies":
							if alias := names[parts[i]]; alias != "" {
								parts[i] = alias
							}
						}
					}
				}
				schema[key] = strings.Join(parts, "/")
			}
		case "items", "prefixItems", "allOf", "anyOf", "oneOf":
			if children, ok := child.([]any); ok {
				for _, nested := range children {
					renameToolSchema(nested, names)
				}
			} else {
				renameToolSchema(child, names)
			}
		case "additionalProperties", "unevaluatedProperties", "additionalItems", "unevaluatedItems", "contains", "not", "if", "then", "else":
			renameToolSchema(child, names)
		case "default", "const", "enum", "examples":
			renameToolData(child, names)
		}
	}
}

func renameToolData(value any, names map[string]string) {
	switch v := value.(type) {
	case map[string]any:
		for _, child := range v {
			renameToolData(child, names)
		}
		renameObjectKeys(v, names)
	case []any:
		for _, child := range v {
			renameToolData(child, names)
		}
	}
}

func renameObjectKeys(object map[string]any, names map[string]string) {
	for original, alias := range names {
		if value, exists := object[original]; exists {
			delete(object, original)
			object[alias] = value
		}
	}
}

func renameToolArguments(arguments string, names map[string]string) (string, error) {
	if arguments == "" || len(names) == 0 {
		return arguments, nil
	}
	decoder := json.NewDecoder(strings.NewReader(arguments))
	decoder.UseNumber()
	var value any
	if !json.Valid([]byte(arguments)) {
		return "", fmt.Errorf("cannot alias incomplete tool arguments")
	}
	if err := decoder.Decode(&value); err != nil {
		return "", err
	}
	var visit func(any) error
	visit = func(value any) error {
		switch v := value.(type) {
		case map[string]any:
			for original, alias := range names {
				if _, exists := v[original]; exists {
					if _, collision := v[alias]; collision {
						return fmt.Errorf("ambiguous tool field alias")
					}
				}
			}
			for _, child := range v {
				if err := visit(child); err != nil {
					return err
				}
			}
			renameObjectKeys(v, names)
		case []any:
			for _, child := range v {
				if err := visit(child); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := visit(value); err != nil {
		return "", err
	}
	encoded, err := json.Marshal(value)
	return string(encoded), err
}

func (a ResponsesToolFieldAliases) restore(name, arguments string) (string, error) {
	if len(a[name]) == 0 {
		return arguments, nil
	}
	return renameToolArguments(arguments, a.reverse(name))
}

func (a ResponsesToolFieldAliases) reverse(name string) map[string]string {
	names := a[name]
	reverse := make(map[string]string, len(names))
	for original, alias := range names {
		reverse[alias] = original
	}
	return reverse
}

func restoreToolFieldValue(value any, aliases ResponsesToolFieldAliases) error {
	switch v := value.(type) {
	case map[string]any:
		if stringValue(v["type"]) == "function" && aliases[stringValue(v["name"])] != nil {
			renameToolSchema(v["parameters"], aliases.reverse(stringValue(v["name"])))
		}
		if stringValue(v["type"]) == "function_call" && aliases[stringValue(v["name"])] != nil {
			arguments, err := aliases.restore(stringValue(v["name"]), stringValue(v["arguments"]))
			if err != nil {
				return err
			}
			v["arguments"] = arguments
		}
		for _, child := range v {
			if err := restoreToolFieldValue(child, aliases); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range v {
			if err := restoreToolFieldValue(child, aliases); err != nil {
				return err
			}
		}
	}
	return nil
}
