//go:build unit

package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPrepareUserResourceCodexExtraForCreateOwnsSeed(t *testing.T) {
	payload := map[string]any{
		"platform": PlatformOpenAI,
		"type":     AccountTypeOAuth,
		"extra": map[string]any{
			codexFingerprintModeExtraKey: "session",
			codexFingerprintSeedExtraKey: "11111111-1111-1111-1111-111111111111",
		},
	}

	prepareUserResourceCodexExtraForCreate(payload)
	extra := payload["extra"].(map[string]any)
	seed, ok := codexFingerprintSeed(extra)
	require.True(t, ok)
	require.NotEqual(t, "11111111-1111-1111-1111-111111111111", seed)
}

func TestPrepareUserResourceCodexExtraForUpdatePreservesExistingSeed(t *testing.T) {
	const seed = "22222222-2222-2222-2222-222222222222"
	existing := map[string]any{
		"platform": PlatformOpenAI,
		"type":     AccountTypeOAuth,
		"extra": map[string]any{
			codexFingerprintModeExtraKey: "session",
			codexFingerprintSeedExtraKey: seed,
		},
	}
	payload := map[string]any{
		"extra": map[string]any{
			codexFingerprintModeExtraKey: "full",
			codexFingerprintSeedExtraKey: "33333333-3333-3333-3333-333333333333",
		},
	}

	prepareUserResourceCodexExtraForUpdate(existing, payload)
	extra := payload["extra"].(map[string]any)
	require.Equal(t, seed, extra[codexFingerprintSeedExtraKey])
	require.Equal(t, "full", extra[codexFingerprintModeExtraKey])
}

func TestPrepareUserResourceCodexExtraForUpdateGeneratesSeedWhenEnabling(t *testing.T) {
	existing := map[string]any{
		"platform": PlatformOpenAI,
		"type":     AccountTypeOAuth,
		"extra":    map[string]any{},
	}
	payload := map[string]any{
		"extra": map[string]any{codexFingerprintModeExtraKey: "device"},
	}

	prepareUserResourceCodexExtraForUpdate(existing, payload)
	seed, ok := codexFingerprintSeed(payload["extra"].(map[string]any))
	require.True(t, ok)
	require.NotEmpty(t, seed)
}
