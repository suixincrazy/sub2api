//go:build unit

package service

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"strings"
	"testing"
)

func importedProxyForRuntime(t *testing.T, content string) *Proxy {
	t.Helper()
	nodes := parseProxyNodeLines(content)
	if len(nodes) != 1 || nodes[0].Err != "" {
		t.Fatalf("import failed: %#v", nodes)
	}
	node := nodes[0]
	return &Proxy{ID: 7, Kind: node.Kind, Protocol: node.Protocol, Host: node.Host, Port: node.Port, Username: node.Username, Password: node.Password, Extra: map[string]any{"raw": node.Raw}}
}

func importedSingBoxSpec(t *testing.T, content string) singBoxRuntimeSpec {
	t.Helper()
	p := importedProxyForRuntime(t, content)
	spec, err := buildSingBoxRuntimeSpec(xrayRawNode(p), p)
	if err != nil {
		t.Fatal(err)
	}
	return spec
}

func TestProxyImportRuntimeRoundTrip(t *testing.T) {
	for _, protocol := range []string{"vmess", "vless", "trojan"} {
		t.Run("clash_tls_options_"+protocol, func(t *testing.T) {
			content := `{"proxies":[{"type":"` + protocol + `","name":"tls","server":"proxy.example.com","port":443,"uuid":"00000000-0000-4000-8000-000000000001","password":"fixture","tls":true,"skip-cert-verify":true,"alpn":["h2","http/1.1"]}]}`
			p := importedProxyForRuntime(t, content)
			out, err := buildXrayOutbound(xrayRawNode(p), p)
			if err != nil {
				t.Fatal(err)
			}
			stream, _ := out["streamSettings"].(map[string]any)
			tls, _ := stream["tlsSettings"].(map[string]any)
			alpn, _ := json.Marshal(tls["alpn"])
			if string(alpn) != `["h2","http/1.1"]` || tls[xrayLegacyInsecureMarker] != true {
				t.Fatalf("Clash TLS options lost: %#v", tls)
			}
		})
	}
	t.Run("trojan_defaults_to_tls", func(t *testing.T) {
		p := importedProxyForRuntime(t, "trojan://fixture@proxy.example.com:443?sni=origin.example.com")
		out, err := buildXrayOutbound(xrayRawNode(p), p)
		if err != nil {
			t.Fatal(err)
		}
		if out["streamSettings"].(map[string]any)["security"] != "tls" {
			t.Fatalf("missing Trojan TLS: %#v", out)
		}
	})
	t.Run("clash_reality_with_tls", func(t *testing.T) {
		p := importedProxyForRuntime(t, `{"proxies":[{"name":"reality","type":"vless","server":"proxy.example.com","port":443,"uuid":"00000000-0000-4000-8000-000000000001","tls":true,"reality-opts":{"public-key":"fixture-public-key","short-id":"abcd"}}]}`)
		out, err := buildXrayOutbound(xrayRawNode(p), p)
		if err != nil {
			t.Fatal(err)
		}
		if out["streamSettings"].(map[string]any)["security"] != "reality" {
			t.Fatalf("Reality was lost: %#v", out)
		}
	})
	t.Run("base64_singbox_shadowsocks_plugin", func(t *testing.T) {
		input := `{"outbounds":[{"type":"shadowsocks","tag":"ss","server":"proxy.example.com","server_port":443,"method":"aes-128-gcm","password":"fixture","plugin":"v2ray-plugin","plugin_opts":"mode=websocket;host=cdn.example.com;tls"}]}`
		spec := importedSingBoxSpec(t, base64.StdEncoding.EncodeToString([]byte(input)))
		if spec.Outbound["plugin"] != "v2ray-plugin" || !strings.Contains(urAsString(spec.Outbound["plugin_opts"]), "host=cdn.example.com") {
			t.Fatalf("plugin settings lost: %#v", spec.Outbound)
		}
	})
	t.Run("wireguard_escaped_key", func(t *testing.T) {
		key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0xfb}, 32))
		u := url.URL{Scheme: "wireguard", Host: "proxy.example.com:51820", User: url.User(key), RawQuery: url.Values{"publickey": {key}, "address": {"172.16.0.2/32"}}.Encode()}
		spec := importedSingBoxSpec(t, u.String())
		peer := mapSliceFromAny(spec.Endpoint["peers"])[0]
		if peer["public_key"] != key {
			t.Fatalf("public key was decoded twice: %q", peer["public_key"])
		}
	})
	t.Run("clash_wireguard_addresses", func(t *testing.T) {
		spec := importedSingBoxSpec(t, `{"proxies":[{"type":"wireguard","name":"wg","server":"proxy.example.com","port":51820,"private-key":"fixture","public-key":"fixture","ip":"172.16.0.2","ipv6":"fd00::2"}]}`)
		addresses := spec.Endpoint["address"].([]string)
		if strings.Join(addresses, ",") != "172.16.0.2/32,fd00::2/128" {
			t.Fatalf("interface prefixes lost: %#v", addresses)
		}
	})
	t.Run("singbox_wireguard_reserved", func(t *testing.T) {
		spec := importedSingBoxSpec(t, `{"endpoints":[{"type":"wireguard","tag":"wg","address":["172.16.0.2/32"],"private_key":"fixture","peers":[{"address":"proxy.example.com","port":51820,"public_key":"fixture","reserved":[1,2,3]}]}]}`)
		peer := mapSliceFromAny(spec.Endpoint["peers"])[0]
		reserved, _ := json.Marshal(peer["reserved"])
		if string(reserved) != "[1,2,3]" {
			t.Fatalf("reserved bytes lost: %s", reserved)
		}
	})
	t.Run("clash_https", func(t *testing.T) {
		p := importedProxyForRuntime(t, `{"proxies":[{"type":"http","name":"secure","server":"proxy.example.com","port":443,"tls":true}]}`)
		if !strings.HasPrefix(p.StandardURL(), "https://") {
			t.Fatalf("HTTPS was downgraded: %s", p.StandardURL())
		}
	})
	t.Run("hysteria_hop_duration", func(t *testing.T) {
		spec := importedSingBoxSpec(t, `{"proxies":[{"type":"hysteria2","name":"h2","server":"proxy.example.com","port":443,"password":"fixture","ports":"443-445","hop-interval":30}]}`)
		if spec.Outbound["hop_interval"] != "30s" {
			t.Fatalf("invalid hop duration: %#v", spec.Outbound)
		}
	})
}
