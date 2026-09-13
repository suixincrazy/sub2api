//go:build unit

package service

import (
	"testing"
)

func TestRequiresSingBoxRuntime(t *testing.T) {
	tests := []struct {
		protocol string
		want     bool
	}{
		{"vless", false},
		{"vmess", false},
		{"trojan", false},
		{"ss", true},
		{"shadowsocks", true},
		{"hysteria", true},
		{"hysteria2", true},
		{"tuic", true},
		{"naive", true},
		{"wireguard", true},
		{"anytls", true},
		{"http", false},
		{"socks5", false},
	}

	for _, tt := range tests {
		proxy := &Proxy{Kind: "xray", Protocol: tt.protocol}
		got := requiresSingBoxRuntime(proxy)
		if got != tt.want {
			t.Errorf("requiresSingBoxRuntime(%q) = %v, want %v", tt.protocol, got, tt.want)
		}
	}

	if requiresSingBoxRuntime(&Proxy{Kind: "standard", Protocol: "ss"}) {
		t.Error("standard proxy should not require sing-box runtime")
	}
	if requiresSingBoxRuntime(nil) {
		t.Error("nil proxy should not require sing-box runtime")
	}
}

func TestBuildSingBoxRuntimeSpecReturnsStructWithOutbound(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		protocol string
	}{
		{"shadowsocks", "ss://YWVzLTI1Ni1nY206cGFzc3dvcmQ=@example.com:8388", "ss"},
		{"hysteria2", "hysteria2://password@example.com:443", "hysteria2"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec, err := buildSingBoxRuntimeSpec(tt.raw, &Proxy{ID: 1, Kind: "xray", Protocol: tt.protocol})
			if err != nil {
				t.Fatalf("buildSingBoxRuntimeSpec failed: %v", err)
			}
			if spec == nil {
				t.Fatal("spec is nil")
			}
			if spec.Outbound == nil {
				t.Fatal("spec.Outbound is nil")
			}
			if tag := stringFromMap(spec.Outbound, "tag"); tag != "proxy-1" {
				t.Errorf("tag = %q, want %q", tag, "proxy-1")
			}
		})
	}
}

func TestShadowsocksClashPluginTranslation(t *testing.T) {
	raw := "ss://YWVzLTI1Ni1nY206cGFzc3dvcmQ=@example.com:8388?plugin=obfs%3Bhost%3Dcdn.example.com%3Bmode%3Dhttp"
	spec, err := buildSingBoxRuntimeSpec(raw, &Proxy{ID: 1, Kind: "xray", Protocol: "ss"})
	if err != nil {
		t.Fatalf("buildSingBoxRuntimeSpec failed: %v", err)
	}

	plugin := stringFromMap(spec.Outbound, "plugin")
	pluginOpts := stringFromMap(spec.Outbound, "plugin_opts")

	if plugin != "obfs-local" {
		t.Errorf("plugin = %q, want %q", plugin, "obfs-local")
	}
	expectedOpts := "obfs=http;obfs-host=cdn.example.com"
	if pluginOpts != expectedOpts {
		t.Errorf("plugin_opts = %q, want %q", pluginOpts, expectedOpts)
	}
}
