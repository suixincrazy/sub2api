package dto

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

func TestProxyMapperKeepsXraySecretsAdminOnly(t *testing.T) {
	proxy := &service.Proxy{
		ID:       7,
		Name:     "private node",
		Kind:     "xray",
		IsPublic: true,
		Protocol: "vless",
		Password: "proxy-password",
		Extra:    map[string]any{"raw": "vless://node-secret@example.com:443"},
	}

	publicJSON, err := json.Marshal(ProxyFromService(proxy))
	if err != nil {
		t.Fatal(err)
	}
	publicText := string(publicJSON)
	for _, secret := range []string{"proxy-password", "node-secret", `"extra"`} {
		if strings.Contains(publicText, secret) {
			t.Fatalf("user-facing proxy DTO leaked %q: %s", secret, publicText)
		}
	}
	if !strings.Contains(publicText, `"kind":"xray"`) || !strings.Contains(publicText, `"is_public":true`) {
		t.Fatalf("user-facing proxy DTO omitted non-secret mode fields: %s", publicText)
	}

	adminJSON, err := json.Marshal(ProxyFromServiceAdmin(proxy))
	if err != nil {
		t.Fatal(err)
	}
	adminText := string(adminJSON)
	if !strings.Contains(adminText, "proxy-password") || !strings.Contains(adminText, "node-secret") {
		t.Fatalf("admin proxy DTO omitted editable proxy credentials: %s", adminText)
	}
}
