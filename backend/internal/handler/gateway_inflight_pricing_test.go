package handler

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTokenInflightEstimate_PreservesPricingInputs(t *testing.T) {
	body := []byte(`{"model":"gpt-6-astra","service_tier":"ultrafast","reasoning":{"effort":"high"},"max_output_tokens":4096}`)
	req := tokenInflightEstimate("gpt-6-astra", body)
	require.Equal(t, "ultrafast", req.ServiceTier)
	require.Equal(t, "high", req.ReasoningEffort)
	require.Equal(t, 4096, req.MaxTokens)
	require.Equal(t, len(body), req.BodyBytes)
}

func TestTokenInflightEstimate_PreservesAnthropicSpeed(t *testing.T) {
	req := tokenInflightEstimate("claude-opus-5", []byte(`{"speed":"fast"}`))
	require.Equal(t, "fast", req.Speed)
}
