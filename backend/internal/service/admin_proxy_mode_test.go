package service

import "testing"

func TestValidateAdminProxyMode(t *testing.T) {
	tests := []struct {
		name     string
		kind     string
		protocol string
		extra    map[string]any
		wantErr  bool
	}{
		{name: "standard socks", kind: "standard", protocol: "socks5", wantErr: false},
		{name: "standard rejects xray protocol", kind: "standard", protocol: "vless", wantErr: true},
		{name: "xray vless", kind: "xray", protocol: "vless", extra: map[string]any{"raw": "vless://secret@example.com:443"}, wantErr: false},
		{name: "xray rejects standard protocol", kind: "xray", protocol: "http", extra: map[string]any{"raw": "http://example.com"}, wantErr: true},
		{name: "xray requires raw node", kind: "xray", protocol: "trojan", extra: map[string]any{}, wantErr: true},
		{name: "unknown kind", kind: "wireguard", protocol: "socks5", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateAdminProxyMode(tt.kind, tt.protocol, tt.extra)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateAdminProxyMode() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestNormalizeAdminProxyKind(t *testing.T) {
	if got := normalizeAdminProxyKind(""); got != "standard" {
		t.Fatalf("empty kind normalized to %q", got)
	}
	if got := normalizeAdminProxyKind(" XRAY "); got != "xray" {
		t.Fatalf("xray kind normalized to %q", got)
	}
}
