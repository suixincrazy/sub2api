//go:build unit

package service

import (
	"encoding/json"
	"testing"
)

func TestSingBoxSpecProducesValidJSON(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		protocol string
		checkFn  func(t *testing.T, outbound map[string]any)
	}{
		{
			name:     "shadowsocks_basic",
			raw:      "ss://YWVzLTI1Ni1nY206cGFzc3dvcmQ=@example.com:8388",
			protocol: "ss",
			checkFn: func(t *testing.T, outbound map[string]any) {
				if outbound["type"] != "shadowsocks" {
					t.Errorf("type = %v, want shadowsocks", outbound["type"])
				}
				if outbound["server"] != "example.com" {
					t.Errorf("server = %v, want example.com", outbound["server"])
				}
				if outbound["method"] != "aes-256-gcm" {
					t.Errorf("method = %v, want aes-256-gcm", outbound["method"])
				}
			},
		},
		{
			name:     "shadowsocks_with_obfs_plugin",
			raw:      "ss://YWVzLTI1Ni1nY206cGFzc3dvcmQ=@example.com:8388?plugin=obfs%3Bhost%3Dcdn.example.com%3Bmode%3Dhttp",
			protocol: "ss",
			checkFn: func(t *testing.T, outbound map[string]any) {
				if outbound["plugin"] != "obfs-local" {
					t.Errorf("plugin = %v, want obfs-local", outbound["plugin"])
				}
				if outbound["plugin_opts"] != "obfs=http;obfs-host=cdn.example.com" {
					t.Errorf("plugin_opts = %v, want obfs=http;obfs-host=cdn.example.com", outbound["plugin_opts"])
				}
			},
		},
		{
			name:     "hysteria2",
			raw:      "hysteria2://password@example.com:443",
			protocol: "hysteria2",
			checkFn: func(t *testing.T, outbound map[string]any) {
				if outbound["type"] != "hysteria2" {
					t.Errorf("type = %v, want hysteria2", outbound["type"])
				}
				if outbound["server"] != "example.com" {
					t.Errorf("server = %v, want example.com", outbound["server"])
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec, err := buildSingBoxRuntimeSpec(tt.raw, &Proxy{ID: 1, Kind: "xray", Protocol: tt.protocol})
			if err != nil {
				t.Fatalf("buildSingBoxRuntimeSpec failed: %v", err)
			}

			// Verify the spec can be marshaled to valid JSON
			jsonBytes, err := json.MarshalIndent(spec.Outbound, "", "  ")
			if err != nil {
				t.Fatalf("failed to marshal outbound to JSON: %v", err)
			}
			t.Logf("Generated outbound JSON:\n%s", string(jsonBytes))

			// Run protocol-specific checks
			tt.checkFn(t, spec.Outbound)
		})
	}
}
