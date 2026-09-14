//go:build unit

package service

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSingBoxNativeImportRetainsConnectionOptions(t *testing.T) {
	for _, content := range []string{
		`{"type":"vless","tag":"v","server":"proxy.example.com","server_port":443,"uuid":"00000000-0000-4000-8000-000000000001","tls":{"enabled":true,"insecure":true,"alpn":["h2"],"utls":{"enabled":true,"fingerprint":"firefox"}},"transport":{"type":"httpupgrade","host":"cdn.example.com","path":"/ws"}}`,
		`{"type":"hysteria","server":"proxy.example.com","server_port":443,"auth":"c2VjcmV0","up_mbps":20,"down_mbps":100,"tls":{"enabled":true}}`,
		`{"type":"anytls","server":"proxy.example.com","server_port":443,"password":"fixture","tls":{"enabled":true,"certificate":["inline-certificate"]},"idle_session_timeout":"40s"}`,
	} {
		var expected map[string]any
		require.NoError(t, json.Unmarshal([]byte(content), &expected))
		expected["tag"] = "sub2api-out"
		p := importedProxyForRuntime(t, content)
		require.True(t, requiresSingBoxRuntime(p))
		spec, err := buildSingBoxRuntimeSpec(xrayRawNode(p), p)
		require.NoError(t, err)
		require.Equal(t, expected, spec.Outbound)
	}
}

func TestSingBoxWireGuardLegacyAndMultiplePeers(t *testing.T) {
	legacy := importedSingBoxSpec(t, `{"type":"wireguard","server":"proxy.example.com","server_port":51820,"local_address":["172.16.0.2/32"],"private_key":"fixture","peer_public_key":"public","reserved":[1,2,3]}`)
	require.Equal(t, "public", mapSliceFromAny(legacy.Endpoint["peers"])[0]["public_key"])
	require.NotContains(t, legacy.Endpoint, "local_address")
	multiple := importedSingBoxSpec(t, `{"type":"wireguard","address":["172.16.0.2/32"],"private_key":"fixture","peers":[{"address":"private.example.com","port":51820,"public_key":"private","allowed_ips":["10.0.0.0/8"]},{"address":"public.example.com","port":51820,"public_key":"public","allowed_ips":["0.0.0.0/0"]}]}`)
	require.Len(t, mapSliceFromAny(multiple.Endpoint["peers"]), 2)
}
