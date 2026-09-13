package service

import (
	"testing"
)

func TestParseUserCodexContentSupportsArraysAndLineTokens(t *testing.T) {
	values, err := parseUserCodexContent(`[
  {"tokens":{"access_token":"token-a","refresh_token":"refresh-a"},"email":"a@example.com"},
  {"accessToken":"token-b"}
]`)
	if err != nil {
		t.Fatalf("parse JSON: %v", err)
	}
	if len(values) != 2 {
		t.Fatalf("expected two JSON entries, got %d", len(values))
	}
	first, err := normalizeUserCodexSession(values[0])
	if err != nil {
		t.Fatalf("normalize first entry: %v", err)
	}
	if first.AccessToken != "token-a" || first.RefreshToken != "refresh-a" || first.Email != "a@example.com" {
		t.Fatalf("unexpected first entry: %#v", first)
	}

	values, err = parseUserCodexContent("raw-token-one\nraw-token-two")
	if err != nil || len(values) != 2 {
		t.Fatalf("parse line tokens: values=%#v err=%v", values, err)
	}
}

func TestNormalizeUserCodexSessionNeverTreatsSessionTokenAsRefreshToken(t *testing.T) {
	session, err := normalizeUserCodexSession(map[string]any{
		"access_token":  "access-token",
		"session_token": "must-not-be-stored",
	})
	if err != nil {
		t.Fatalf("normalize session: %v", err)
	}
	if session.RefreshToken != "" {
		t.Fatalf("session token was treated as refresh token: %#v", session)
	}
}

func TestSanitizeUserCodexCredentialExtrasProtectsValidatedTokens(t *testing.T) {
	extras := sanitizeUserCodexCredentialExtras(map[string]any{
		"access_token":  "attacker-value",
		"refresh_token": "attacker-refresh",
		"model_mapping": map[string]any{"gpt": "gpt-upstream"},
	})
	if _, ok := extras["access_token"]; ok {
		t.Fatal("access token override was not removed")
	}
	if _, ok := extras["refresh_token"]; ok {
		t.Fatal("refresh token override was not removed")
	}
	if _, ok := extras["model_mapping"]; !ok {
		t.Fatal("non-token credential metadata should be preserved")
	}
}

func TestProxyFromResourceMapPreservesOwnerForXrayRuntimeChecks(t *testing.T) {
	proxy := proxyFromResourceMap(map[string]any{
		"id": int64(3), "owner_user_id": int64(9), "is_public": false,
		"kind": "xray", "protocol": "vless", "host": "example.com", "port": 443,
	})
	if proxy.OwnerUserID == nil || *proxy.OwnerUserID != 9 {
		t.Fatalf("proxy owner was lost: %#v", proxy)
	}
}
