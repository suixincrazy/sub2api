package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Native nodes retain TLS trust, transport, authentication and peer settings
// that cannot be represented losslessly by a share URI.
func normalizeNativeSingBoxNode(input map[string]any) (map[string]any, error) {
	node := cloneProxyProbeMap(input)
	protocol := canonicalSingBoxProtocol(urAsString(node["type"]))
	switch protocol {
	case "http", "socks", "vmess", "vless", "trojan", "shadowsocks", "hysteria", "hysteria2", "tuic", "anytls", "naive", "wireguard":
	default:
		return nil, fmt.Errorf("unsupported sing-box node type %q", protocol)
	}
	node["type"] = protocol
	if protocol == "wireguard" {
		if _, exists := node["peers"]; !exists {
			peer := map[string]any{
				"address": node["server"], "port": node["server_port"],
				"public_key":  node["peer_public_key"],
				"allowed_ips": []string{"0.0.0.0/0", "::/0"},
			}
			for _, key := range []string{"pre_shared_key", "reserved", "allowed_ips"} {
				if value, ok := node[key]; ok {
					peer[key] = value
				}
			}
			node["peers"] = []any{peer}
			node["address"] = node["local_address"]
			for _, key := range []string{"server", "server_port", "peer_public_key", "local_address", "pre_shared_key", "reserved", "allowed_ips", "gso", "network", "workers"} {
				delete(node, key)
			}
		}
	}
	if protocol == "shadowsocks" {
		plugin := urAsString(node["plugin"])
		if plugin != "" && plugin != "obfs-local" && plugin != "v2ray-plugin" {
			return nil, errors.New("unsupported Shadowsocks plugin")
		}
	}
	return node, nil
}

func nativeSingBoxRuntimeSpec(raw string) (singBoxRuntimeSpec, error) {
	var input map[string]any
	if err := json.Unmarshal([]byte(raw), &input); err != nil {
		return singBoxRuntimeSpec{}, errors.New("invalid sing-box node JSON")
	}
	node, err := normalizeNativeSingBoxNode(input)
	if err != nil {
		return singBoxRuntimeSpec{}, err
	}
	node["tag"] = "sub2api-out"
	if node["type"] == "wireguard" {
		return singBoxRuntimeSpec{Endpoint: node}, nil
	}
	return singBoxRuntimeSpec{Outbound: node}, nil
}

func isNativeSingBoxNode(raw string) bool {
	return strings.HasPrefix(strings.TrimSpace(raw), "{")
}
