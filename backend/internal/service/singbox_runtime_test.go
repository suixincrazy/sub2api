package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestBuildSingBoxRuntimeSpecSupportsExtendedProtocols(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		protocol string
		endpoint bool
	}{
		{
			name:     "hysteria",
			raw:      "hysteria://secret@hy.example.com:443?sni=edge.example.com&upmbps=20&downmbps=100&obfs=mask",
			protocol: "hysteria",
		},
		{
			name:     "hysteria2",
			raw:      "hy2://secret@hy.example.com:443?sni=edge.example.com&obfs=salamander&obfs-password=mask&upmbps=20&downmbps=100",
			protocol: "hysteria2",
		},
		{
			name:     "tuic",
			raw:      "tuic://11111111-1111-1111-1111-111111111111:secret@tuic.example.com:443?sni=edge.example.com&congestion_control=bbr",
			protocol: "tuic",
		},
		{
			name:     "anytls",
			raw:      "anytls://secret@anytls.example.com:443?sni=edge.example.com",
			protocol: "anytls",
		},
		{
			name:     "naive quic",
			raw:      "naive+quic://user:secret@naive.example.com:443?sni=edge.example.com&congestion_control=bbr",
			protocol: "naive",
		},
		{
			name:     "wireguard",
			raw:      "wireguard://private-key@wg.example.com:51820?publickey=public-key&address=172.16.0.2%2F32&reserved=1%2C2%2C3",
			protocol: "wireguard",
			endpoint: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			spec, err := buildSingBoxRuntimeSpec(test.raw, &Proxy{Kind: "xray", Protocol: test.protocol})
			if err != nil {
				t.Fatalf("build runtime spec: %v", err)
			}
			if test.endpoint {
				if spec.Endpoint == nil || spec.Endpoint["type"] != test.protocol {
					t.Fatalf("unexpected endpoint: %#v", spec.Endpoint)
				}
				return
			}
			if spec.Outbound == nil || spec.Outbound["type"] != test.protocol {
				t.Fatalf("unexpected outbound: %#v", spec.Outbound)
			}
		})
	}
}

func TestBuildSingBoxRuntimeSpecSupportsShadowsocksFormats(t *testing.T) {
	userinfo := base64.RawURLEncoding.EncodeToString([]byte("chacha20-ietf:secret"))
	legacyPayload := base64.RawStdEncoding.EncodeToString([]byte("aes-256-cfb:secret@legacy.example.com:8389"))
	tests := []struct {
		name       string
		raw        string
		method     string
		password   string
		host       string
		port       int
		plugin     string
		pluginOpts string
	}{
		{
			name:       "sip002 with plugin",
			raw:        "ss://" + userinfo + "@ss.example.com:8388?plugin=v2ray-plugin%3Bmode%3Dwebsocket%3Bhost%3Dcdn.example.com#node",
			method:     "chacha20-ietf",
			password:   "secret",
			host:       "ss.example.com",
			port:       8388,
			plugin:     "v2ray-plugin",
			pluginOpts: "mode=websocket;host=cdn.example.com",
		},
		{
			name:       "clash simple obfs plugin",
			raw:        "ss://" + userinfo + "@ss.example.com:8388?plugin=obfs%3Bmode%3Dhttp%3Bhost%3Dcdn.example.com#node",
			method:     "chacha20-ietf",
			password:   "secret",
			host:       "ss.example.com",
			port:       8388,
			plugin:     "obfs-local",
			pluginOpts: "obfs=http;obfs-host=cdn.example.com",
		},
		{
			name:     "legacy full payload",
			raw:      "ss://" + legacyPayload + "#legacy",
			method:   "aes-256-cfb",
			password: "secret",
			host:     "legacy.example.com",
			port:     8389,
		},
		{
			name:     "escaped password",
			raw:      "ss://aes-128-gcm:pa%3Ass%40word@escaped.example.com:8390#escaped",
			method:   "aes-128-gcm",
			password: "pa:ss@word",
			host:     "escaped.example.com",
			port:     8390,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			spec, err := buildSingBoxRuntimeSpec(test.raw, &Proxy{Kind: "xray", Protocol: "ss"})
			if err != nil {
				t.Fatalf("build shadowsocks runtime spec: %v", err)
			}
			out := spec.Outbound
			if out["type"] != "shadowsocks" || out["method"] != test.method || out["server"] != test.host || out["server_port"] != test.port {
				t.Fatalf("unexpected shadowsocks outbound: %#v", out)
			}
			if out["password"] != test.password || stringFromMap(out, "plugin") != test.plugin || stringFromMap(out, "plugin_opts") != test.pluginOpts {
				t.Fatalf("shadowsocks credentials or plugin mismatch: %#v", out)
			}
		})
	}
}

func TestBuildSingBoxRuntimeSpecRejectsUnavailableShadowsocksPlugin(t *testing.T) {
	userinfo := base64.RawURLEncoding.EncodeToString([]byte("chacha20-ietf-poly1305:secret"))
	raw := "ss://" + userinfo + "@ss.example.com:8388?plugin=shadow-tls%3Bpassword%3Dsecret"
	_, err := buildSingBoxRuntimeSpec(raw, &Proxy{Kind: "xray", Protocol: "ss"})
	if err == nil || !strings.Contains(err.Error(), "unsupported shadowsocks plugin") {
		t.Fatalf("unsupported plugin was not rejected: %v", err)
	}
	if strings.Contains(err.Error(), "secret") {
		t.Fatalf("plugin validation error leaked credentials: %v", err)
	}
}

func TestRequiresSingBoxRuntimeRoutesShadowsocks(t *testing.T) {
	proxy := &Proxy{
		Kind: "xray", Protocol: "ss",
		Extra: map[string]any{"raw": "ss://Y2hhY2hhMjAtaWV0ZjpzZWNyZXQ@ss.example.com:8388"},
	}
	if !requiresSingBoxRuntime(proxy) {
		t.Fatal("shadowsocks nodes must use sing-box for legacy cipher compatibility")
	}
}

func TestBuildSingBoxRuntimeConfigBlocksPrivateDestinations(t *testing.T) {
	spec := singBoxRuntimeSpec{Outbound: map[string]any{
		"type": "anytls", "tag": "sub2api-out", "server": "203.0.113.10", "server_port": 443,
		"password": "secret", "tls": map[string]any{"enabled": true},
	}}
	config := buildSingBoxRuntimeConfig(1080, spec, true)
	route, ok := config["route"].(map[string]any)
	if !ok {
		t.Fatalf("missing route config: %#v", config["route"])
	}
	rules, ok := route["rules"].([]map[string]any)
	if !ok || len(rules) != 1 || rules[0]["action"] != "reject" {
		t.Fatalf("missing private destination rejection: %#v", route["rules"])
	}
	cidrs, ok := rules[0]["ip_cidr"].([]string)
	if !ok || !containsString(cidrs, "10.0.0.0/8") || !containsString(cidrs, "169.254.0.0/16") {
		t.Fatalf("protected ranges are incomplete: %#v", rules[0]["ip_cidr"])
	}
}

func TestBuildSingBoxRuntimeSpecErrorsDoNotLeakSecrets(t *testing.T) {
	raw := "tuic://user:super-secret-token@example.com"
	_, err := buildSingBoxRuntimeSpec(raw, &Proxy{Kind: "xray", Protocol: "tuic"})
	if err == nil {
		t.Fatal("expected missing TUIC port to be rejected by the runtime")
	}
	if strings.Contains(err.Error(), "super-secret-token") {
		t.Fatalf("runtime error leaked credentials: %v", err)
	}
}

func TestSingBoxRuntimeConfigsPassBinaryCheck(t *testing.T) {
	bin := strings.TrimSpace(os.Getenv("SING_BOX_BIN"))
	if bin == "" {
		t.Skip("set SING_BOX_BIN to run real sing-box config checks")
	}
	privateKey := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("a", 32)))
	publicKey := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("b", 32)))
	wireGuard := (&url.URL{
		Scheme: "wireguard",
		User:   url.User(privateKey),
		Host:   "203.0.113.50:51820",
		RawQuery: url.Values{
			"publickey": {publicKey},
			"address":   {"172.16.0.2/32"},
		}.Encode(),
	}).String()
	tests := map[string]string{
		"shadowsocks":              "ss://Y2hhY2hhMjAtaWV0ZjpzZWNyZXQ@203.0.113.4:8388",
		"shadowsocks-obfs":         "ss://Y2hhY2hhMjAtaWV0ZjpzZWNyZXQ@203.0.113.4:8388?plugin=obfs%3Bmode%3Dhttp%3Bhost%3Dcdn.example.com",
		"shadowsocks-v2ray-plugin": "ss://Y2hhY2hhMjAtaWV0ZjpzZWNyZXQ@203.0.113.4:8388?plugin=v2ray-plugin%3Bmode%3Dwebsocket%3Bhost%3Dcdn.example.com%3Bpath%3D%2Fproxy",
		"hysteria":                 "hysteria://secret@203.0.113.5:443?sni=edge.example.com&upmbps=20&downmbps=100",
		"hysteria2":                "hy2://secret@203.0.113.10:443?sni=edge.example.com&obfs=salamander&obfs-password=mask",
		"tuic":                     "tuic://11111111-1111-1111-1111-111111111111:secret@203.0.113.20:443?sni=edge.example.com",
		"anytls":                   "anytls://secret@203.0.113.30:443?sni=edge.example.com",
		"naive":                    "naive+https://user:secret@203.0.113.40:443?sni=edge.example.com",
		"wireguard":                wireGuard,
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			spec, err := buildSingBoxRuntimeSpec(raw, &Proxy{Kind: "xray", Protocol: name})
			if err != nil {
				t.Fatalf("build runtime spec: %v", err)
			}
			config, err := json.Marshal(buildSingBoxRuntimeConfig(1080, spec, true))
			if err != nil {
				t.Fatalf("marshal config: %v", err)
			}
			path := filepath.Join(t.TempDir(), name+".json")
			if err := os.WriteFile(path, config, 0o600); err != nil {
				t.Fatalf("write config: %v", err)
			}
			if output, err := exec.Command(bin, "check", "-c", path).CombinedOutput(); err != nil {
				t.Fatalf("sing-box rejected generated config: %v: %s", err, strings.TrimSpace(string(output)))
			}
		})
	}
}

func TestSingBoxRuntimeManagerPrunesIdleInstance(t *testing.T) {
	manager := NewSingBoxRuntimeManager("sing-box-test-helper", t.TempDir())
	manager.maxInstances = 1
	manager.idleTTL = time.Minute
	manager.commandFactory = func(_, configPath string) *exec.Cmd {
		cmd := exec.Command(os.Args[0], "-test.run=^TestSingBoxRuntimeHelperProcess$")
		cmd.Env = append(os.Environ(),
			"SUB2API_SING_BOX_HELPER=1",
			"SUB2API_SING_BOX_HELPER_CONFIG="+configPath,
		)
		return cmd
	}
	defer func() { _ = manager.Close() }()

	proxy := func(id int64) *Proxy {
		return &Proxy{
			ID: id, Kind: "xray", Protocol: "anytls",
			Extra: map[string]any{"raw": "anytls://secret@203.0.113.10:443?sni=edge.example.com"},
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := manager.ProxyURL(ctx, proxy(1)); err != nil {
		t.Fatalf("start first sing-box runtime: %v", err)
	}
	manager.mu.Lock()
	manager.instances[1].lastUsed = time.Now().Add(-2 * time.Minute)
	manager.mu.Unlock()
	if _, err := manager.ProxyURL(ctx, proxy(2)); err != nil {
		t.Fatalf("idle sing-box runtime did not release capacity: %v", err)
	}
	manager.mu.Lock()
	_, oldExists := manager.instances[1]
	_, newExists := manager.instances[2]
	manager.mu.Unlock()
	if oldExists || !newExists {
		t.Fatalf("unexpected sing-box runtime set after idle pruning: old=%t new=%t", oldExists, newExists)
	}
}

func TestSingBoxRuntimeHelperProcess(t *testing.T) {
	if os.Getenv("SUB2API_SING_BOX_HELPER") != "1" {
		return
	}
	raw, err := os.ReadFile(os.Getenv("SUB2API_SING_BOX_HELPER_CONFIG"))
	if err != nil {
		t.Fatalf("read helper config: %v", err)
	}
	var cfg struct {
		Inbounds []struct {
			ListenPort int `json:"listen_port"`
		} `json:"inbounds"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil || len(cfg.Inbounds) == 0 {
		t.Fatalf("decode helper config: %v", err)
	}
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(cfg.Inbounds[0].ListenPort)))
	if err != nil {
		t.Fatalf("listen helper port: %v", err)
	}
	defer func() { _ = listener.Close() }()
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		_ = conn.Close()
	}
}
