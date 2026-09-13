package service

import (
	"encoding/base64"
	"encoding/json"
	"net/url"
	"strings"
	"testing"
)

func TestParseProxyNodeLinesAcceptsBase64SingBoxJSON(t *testing.T) {
	document := map[string]any{
		"outbounds": []map[string]any{{
			"type":        "hysteria2",
			"tag":         "hy2-node",
			"server":      "edge.example.com",
			"server_port": 443,
			"password":    "secret",
			"tls":         map[string]any{"enabled": true, "server_name": "edge.example.com"},
		}},
	}
	rawJSON, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	raw := base64.StdEncoding.EncodeToString(rawJSON)
	nodes := parseProxyNodeLines(raw)
	if len(nodes) != 1 {
		t.Fatalf("got %d nodes, want 1: %#v", len(nodes), nodes)
	}
	if nodes[0].Err != "" || nodes[0].Protocol != "hysteria2" || nodes[0].Host != "edge.example.com" {
		t.Fatalf("unexpected parsed node: %#v", nodes[0])
	}
}

func TestParseClashSocksNormalizesToSocks5h(t *testing.T) {
	raw := "proxies:\n  - name: office\n    type: socks\n    server: proxy.example.com\n    port: 1080\n"
	nodes := parseProxyNodeLines(raw)
	if len(nodes) != 1 {
		t.Fatalf("got %d nodes, want 1: %#v", len(nodes), nodes)
	}
	if nodes[0].Err != "" || nodes[0].Kind != "standard" || nodes[0].Protocol != "socks5h" {
		t.Fatalf("unexpected SOCKS node: %#v", nodes[0])
	}
}

func TestParseClashHysteriaBandwidthUnits(t *testing.T) {
	raw := "proxies:\n  - name: hy\n    type: hysteria\n    server: hy.example.com\n    port: 443\n    auth-str: secret\n    up: 50 Mbps\n    down: 1 Gbps\n"
	nodes := parseProxyNodeLines(raw)
	if len(nodes) != 1 {
		t.Fatalf("got %d nodes, want 1: %#v", len(nodes), nodes)
	}
	if nodes[0].Err != "" {
		t.Fatalf("hysteria node rejected: %#v", nodes[0])
	}
	u, err := url.Parse(nodes[0].Raw)
	if err != nil {
		t.Fatal(err)
	}
	if got := u.Query().Get("upmbps"); got != "50" {
		t.Fatalf("upmbps = %q, want 50", got)
	}
	if got := u.Query().Get("downmbps"); got != "1000" {
		t.Fatalf("downmbps = %q, want 1000", got)
	}
	if strings.Contains(nodes[0].Raw, "Mbps") || strings.Contains(nodes[0].Raw, "Gbps") {
		t.Fatalf("bandwidth units were not normalized: %s", nodes[0].Raw)
	}
}
