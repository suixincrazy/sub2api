package service

import (
	"bytes"
	"encoding/json"
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func prepareOpenAIMultimodalSchemaRetry(c *gin.Context, status int, body, failure []byte) ([]byte, bool) {
	if status != http.StatusBadRequest || extractUpstreamErrorCode(failure) != "multimodal_forbidden" ||
		extractUpstreamErrorMessage(failure) != "audio/video content is not allowed" ||
		hasOpenAIRequestMedia(gjson.ParseBytes(body)) || c == nil {
		return nil, false
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var req map[string]any
	if !json.Valid(body) || decoder.Decode(&req) != nil {
		return nil, false
	}
	// Compacted conversations can carry tool declarations in input items.
	if _, err := liftResponsesAdditionalTools(req); err != nil {
		return nil, false
	}
	// Flatten namespaces before building aliases so same-named tools in
	// different namespaces retain separate reversible identities.
	mapping, _, err := apicompat.AdaptResponsesClientTools(req)
	if err != nil {
		return nil, false
	}
	aliases, changed, err := apicompat.AliasResponsesToolMediaFields(req)
	if err != nil || !changed {
		return nil, false
	}
	encoded, err := marshalOpenAIUpstreamJSON(req)
	if err != nil {
		return nil, false
	}
	prior, _ := openAIResponsesClientToolMapping(c)
	if prior.CustomTools != nil {
		mapping.CustomTools = prior.CustomTools
	}
	mapping.ToolSearch = mapping.ToolSearch || prior.ToolSearch
	if mapping.NamespaceTools == nil {
		mapping.NamespaceTools = prior.NamespaceTools
	} else {
		for name, identity := range prior.NamespaceTools {
			mapping.NamespaceTools[name] = identity
		}
	}
	mapping.FieldAliases = aliases
	setOpenAIResponsesClientToolMapping(c, mapping)
	return encoded, true
}

// Skip declarations and opaque text/argument strings. Actual media remains
// unchanged and must continue to receive the provider's original rejection.
func hasOpenAIRequestMedia(value gjson.Result) bool {
	if value.IsObject() {
		switch value.Get("type").String() {
		case "input_audio", "audio", "audio_url", "input_video", "video", "video_url":
			return true
		}
	}
	found := false
	if value.IsObject() || value.IsArray() {
		value.ForEach(func(key, child gjson.Result) bool {
			switch key.String() {
			case "tools", "parameters":
				return true
			case "audio", "video", "input_audio", "input_video", "audio_url", "video_url":
				found = true
			default:
				found = hasOpenAIRequestMedia(child)
			}
			return !found
		})
	}
	return found
}
