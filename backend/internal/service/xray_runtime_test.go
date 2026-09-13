package service

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func requireXrayMap(t *testing.T, value any, name string) map[string]any {
	t.Helper()
	result, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("%s has unexpected type %T", name, value)
	}
	return result
}

func requireXrayMapSlice(t *testing.T, value any, name string) []map[string]any {
	t.Helper()
	result, ok := value.([]map[string]any)
	if !ok {
		t.Fatalf("%s has unexpected type %T", name, value)
	}
	return result
}

func TestBuildXrayOutboundVMess(t *testing.T) {
	node := map[string]any{
		"add":  "vmess.example.com",
		"port": "443",
		"id":   "11111111-1111-1111-1111-111111111111",
		"aid":  0,
		"scy":  "auto",
		"net":  "ws",
		"type": "none",
		"host": "cdn.example.com",
		"path": "/ws",
		"tls":  "tls",
		"sni":  "sni.example.com",
	}
	raw, _ := json.Marshal(node)
	out, err := buildXrayOutbound("vmess://"+base64.RawStdEncoding.EncodeToString(raw), &Proxy{Kind: "xray"})
	if err != nil {
		t.Fatalf("buildXrayOutbound returned error: %v", err)
	}
	if out["protocol"] != "vmess" {
		t.Fatalf("protocol mismatch: %v", out["protocol"])
	}
	stream := requireXrayMap(t, out["streamSettings"], "streamSettings")
	if stream["network"] != "ws" || stream["security"] != "tls" {
		t.Fatalf("stream settings mismatch: %#v", stream)
	}
}

func TestBuildXrayOutboundVLESSReality(t *testing.T) {
	out, err := buildXrayOutbound("vless://11111111-1111-1111-1111-111111111111@vless.example.com:443?security=reality&type=grpc&sni=sni.example.com&pbk=pub&sid=abc&serviceName=svc", &Proxy{Kind: "xray"})
	if err != nil {
		t.Fatalf("buildXrayOutbound returned error: %v", err)
	}
	if out["protocol"] != "vless" {
		t.Fatalf("protocol mismatch: %v", out["protocol"])
	}
	stream := requireXrayMap(t, out["streamSettings"], "streamSettings")
	if stream["network"] != "grpc" || stream["security"] != "reality" {
		t.Fatalf("stream settings mismatch: %#v", stream)
	}
	reality := requireXrayMap(t, stream["realitySettings"], "realitySettings")
	if reality["publicKey"] != "pub" || reality["shortId"] != "abc" {
		t.Fatalf("reality settings mismatch: %#v", reality)
	}
}

func TestBuildXrayOutboundVLESSXHTTP(t *testing.T) {
	extra := `{"downloadSettings":{"server":"203.0.113.20","port":443,"servername":"download.example.com","path":"/download"}}`
	query := url.Values{
		"type":          {"xhttp"},
		"security":      {"tls"},
		"sni":           {"edge.example.com"},
		"allowInsecure": {"1"},
		"path":          {"/path"},
		"mode":          {"stream-up"},
		"extra":         {extra},
	}
	out, err := buildXrayOutbound("vless://11111111-1111-1111-1111-111111111111@203.0.113.10:443?"+query.Encode(), &Proxy{Kind: "xray"})
	if err != nil {
		t.Fatalf("build xhttp outbound: %v", err)
	}
	stream := requireXrayMap(t, out["streamSettings"], "streamSettings")
	if stream["network"] != "xhttp" {
		t.Fatalf("xhttp network mismatch: %#v", stream)
	}
	tls := requireXrayMap(t, stream["tlsSettings"], "tlsSettings")
	if _, exists := tls["allowInsecure"]; exists {
		t.Fatalf("removed Xray allowInsecure option was emitted: %#v", tls)
	}
	xhttp := requireXrayMap(t, stream["xhttpSettings"], "xhttpSettings")
	if xhttp["path"] != "/path" || xhttp["mode"] != "stream-up" {
		t.Fatalf("xhttp settings mismatch: %#v", xhttp)
	}
	xhttpExtra := requireXrayMap(t, xhttp["extra"], "xhttpSettings.extra")
	download := requireXrayMap(t, xhttpExtra["downloadSettings"], "downloadSettings")
	downloadXHTTP := requireXrayMap(t, download["xhttpSettings"], "downloadSettings.xhttpSettings")
	downloadTLS := requireXrayMap(t, download["tlsSettings"], "downloadSettings.tlsSettings")
	if download["address"] != "203.0.113.20" || download["network"] != "xhttp" || downloadXHTTP["path"] != "/download" || downloadTLS["serverName"] != "download.example.com" {
		t.Fatalf("xhttp download settings were not preserved: %#v", download)
	}
}

func TestXrayStreamSettingsSupportsCompatibilityAliasesAndKCP(t *testing.T) {
	httpUpgrade, err := xrayStreamSettings(url.Values{
		"type": {"http-upgrade"},
		"path": {"/upgrade"},
		"host": {"edge.example.com"},
	})
	if err != nil || httpUpgrade["network"] != "httpupgrade" {
		t.Fatalf("http-upgrade alias was not normalized: settings=%#v err=%v", httpUpgrade, err)
	}

	kcp, err := xrayStreamSettings(url.Values{
		"type":       {"mkcp"},
		"headerType": {"srtp"},
		"seed":       {"compat-seed"},
		"mtu":        {"1200"},
	})
	if err != nil || kcp["network"] != "kcp" {
		t.Fatalf("mkcp alias was not normalized: settings=%#v err=%v", kcp, err)
	}
	finalMask := requireXrayMap(t, kcp["finalmask"], "finalmask")
	udp := requireXrayMapSlice(t, finalMask["udp"], "finalmask.udp")
	if len(udp) != 2 || udp[0]["type"] != "header-srtp" || udp[1]["type"] != "mkcp-aes128gcm" {
		t.Fatalf("legacy KCP header/seed were not migrated: %#v", finalMask)
	}
	kcpSettings := requireXrayMap(t, kcp["kcpSettings"], "kcpSettings")
	if kcpSettings["mtu"] != 1200 {
		t.Fatalf("KCP tuning was not preserved: %#v", kcp["kcpSettings"])
	}
}

func TestXrayStreamSettingsRejectsRemovedTransports(t *testing.T) {
	for _, network := range []string{"http", "h2", "quic"} {
		if _, err := xrayStreamSettings(url.Values{"type": {network}}); err == nil {
			t.Fatalf("removed Xray transport %q was accepted", network)
		}
	}
}

func TestBuildXrayOutboundRejectsMalformedXHTTPDownloadSettings(t *testing.T) {
	query := url.Values{
		"type":  {"xhttp"},
		"extra": {`{"downloadSettings":{"server":"download.example.com"}}`},
	}
	if _, err := buildXrayOutbound("vless://11111111-1111-1111-1111-111111111111@203.0.113.10:443?"+query.Encode(), &Proxy{Kind: "xray"}); err == nil {
		t.Fatal("expected malformed xhttp download settings to be rejected")
	}
}

func TestPinLegacyInsecureCertificateCreatesXrayPin(t *testing.T) {
	server := httptest.NewTLSServer(nil)
	defer server.Close()
	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse TLS server URL: %v", err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("parse TLS server port: %v", err)
	}
	tlsSettings := map[string]any{xrayLegacyInsecureMarker: true, "serverName": "mismatched.example.com"}
	if err := pinLegacyInsecureCertificate(context.Background(), tlsSettings, u.Hostname(), port); err != nil {
		t.Fatalf("pin legacy insecure certificate: %v", err)
	}
	pin := stringFromMap(tlsSettings, "pinnedPeerCertSha256")
	if len(pin) != sha256.Size*2 {
		t.Fatalf("unexpected certificate pin: %q", pin)
	}
	if _, exists := tlsSettings[xrayLegacyInsecureMarker]; exists {
		t.Fatal("internal legacy marker was not removed")
	}
}

func TestPinLegacyInsecureCertificateRejectsUnavailableEndpoint(t *testing.T) {
	tlsSettings := map[string]any{xrayLegacyInsecureMarker: true}
	err := pinLegacyInsecureCertificate(context.Background(), tlsSettings, "127.0.0.1", 1)
	if err == nil {
		t.Fatal("expected unavailable legacy insecure endpoint to be rejected")
	}
	if _, exists := tlsSettings["allowInsecure"]; exists {
		t.Fatalf("removed allowInsecure field must not be restored: %#v", tlsSettings)
	}
	if _, exists := tlsSettings["pinnedPeerCertSha256"]; exists {
		t.Fatalf("unexpected certificate pin after failed preflight: %#v", tlsSettings)
	}
	if _, exists := tlsSettings[xrayLegacyInsecureMarker]; exists {
		t.Fatal("internal legacy marker was not removed")
	}
}

func TestPrepareXrayTLSCompatibilityPinsMainAndXHTTPDownload(t *testing.T) {
	server := httptest.NewTLSServer(nil)
	defer server.Close()
	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse TLS server URL: %v", err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("parse TLS server port: %v", err)
	}
	mainTLS := map[string]any{xrayLegacyInsecureMarker: true, "serverName": "main.example.com"}
	downloadTLS := map[string]any{xrayLegacyInsecureMarker: true, "serverName": "download.example.com"}
	outbound := map[string]any{
		"settings": map[string]any{
			"vnext": []map[string]any{{"address": u.Hostname(), "port": port}},
		},
		"streamSettings": map[string]any{
			"tlsSettings": mainTLS,
			"xhttpSettings": map[string]any{
				"extra": map[string]any{
					"downloadSettings": map[string]any{
						"address": u.Hostname(), "port": port, "tlsSettings": downloadTLS,
					},
				},
			},
		},
	}
	if err := prepareXrayTLSCompatibility(context.Background(), outbound); err != nil {
		t.Fatalf("prepare xray TLS compatibility: %v", err)
	}
	for name, settings := range map[string]map[string]any{"main": mainTLS, "download": downloadTLS} {
		if pin := stringFromMap(settings, "pinnedPeerCertSha256"); len(pin) != sha256.Size*2 {
			t.Fatalf("%s TLS pin mismatch: %q", name, pin)
		}
		if _, exists := settings[xrayLegacyInsecureMarker]; exists {
			t.Fatalf("%s internal marker was not removed", name)
		}
	}
}

func TestTLSSettingsUsesXray26CertificateFields(t *testing.T) {
	settings := tlsSettings(url.Values{
		"allowInsecure":         {"1"},
		"pinnedPeerCertSha256":  {strings.Repeat("ab", sha256.Size)},
		"verifyPeerCertInNames": {"one.example.com,two.example.com"},
	})
	if _, ok := settings[xrayLegacyInsecureMarker].(bool); !ok {
		t.Fatal("legacy allowInsecure marker is missing")
	}
	if _, ok := settings["pinnedPeerCertSha256"].(string); !ok {
		t.Fatalf("Xray pin must be a comma-separated string: %#v", settings)
	}
	if settings["verifyPeerCertByName"] != "one.example.com,two.example.com" {
		t.Fatalf("legacy verify names were not migrated: %#v", settings)
	}
	if _, exists := settings["verifyPeerCertInNames"]; exists {
		t.Fatalf("removed Xray verifyPeerCertInNames field was emitted: %#v", settings)
	}
}

func TestBuildXrayOutboundShadowsocks(t *testing.T) {
	userInfo := base64.RawURLEncoding.EncodeToString([]byte("aes-128-gcm:secret"))
	out, err := buildXrayOutbound("ss://"+userInfo+"@ss.example.com:8388#node", &Proxy{Kind: "xray"})
	if err != nil {
		t.Fatalf("buildXrayOutbound returned error: %v", err)
	}
	if out["protocol"] != "shadowsocks" {
		t.Fatalf("protocol mismatch: %v", out["protocol"])
	}
}

func TestPinUserOwnedXrayOutboundRejectsPrivateEndpoint(t *testing.T) {
	outbound := map[string]any{
		"settings": map[string]any{
			"servers": []map[string]any{{"address": "127.0.0.1", "port": 443}},
		},
	}
	if err := pinUserOwnedXrayOutbound(context.Background(), outbound); err == nil {
		t.Fatal("expected private xray endpoint to be rejected")
	}
}

func TestPinUserOwnedXrayOutboundPinsPublicLiteral(t *testing.T) {
	server := map[string]any{"address": "8.8.8.8", "port": 443}
	outbound := map[string]any{
		"settings": map[string]any{"servers": []map[string]any{server}},
	}
	if err := pinUserOwnedXrayOutbound(context.Background(), outbound); err != nil {
		t.Fatalf("pin public xray endpoint: %v", err)
	}
	if server["address"] != "8.8.8.8" {
		t.Fatalf("unexpected pinned address: %v", server["address"])
	}
}

func TestPinUserOwnedXrayOutboundRejectsPrivateXHTTPDownloadEndpoint(t *testing.T) {
	outbound := map[string]any{
		"settings": map[string]any{
			"vnext": []map[string]any{{"address": "8.8.8.8", "port": 443}},
		},
		"streamSettings": map[string]any{
			"xhttpSettings": map[string]any{
				"extra": map[string]any{
					"downloadSettings": map[string]any{"server": "127.0.0.1", "port": 443},
				},
			},
		},
	}
	if err := pinUserOwnedXrayOutbound(context.Background(), outbound); err == nil {
		t.Fatal("expected private xhttp download endpoint to be rejected")
	}
}

func TestPinUserOwnedXrayOutboundPinsPublicXHTTPDownloadEndpoint(t *testing.T) {
	download := map[string]any{"server": "8.8.4.4", "port": 443}
	outbound := map[string]any{
		"settings": map[string]any{
			"vnext": []map[string]any{{"address": "8.8.8.8", "port": 443}},
		},
		"streamSettings": map[string]any{
			"xhttpSettings": map[string]any{
				"extra": map[string]any{"downloadSettings": download},
			},
		},
	}
	if err := pinUserOwnedXrayOutbound(context.Background(), outbound); err != nil {
		t.Fatalf("pin public xhttp download endpoint: %v", err)
	}
	if download["server"] != "8.8.4.4" {
		t.Fatalf("unexpected pinned xhttp endpoint: %#v", download)
	}
}

func TestBuildXrayRuntimeConfigBlocksPrivateDestinationsForUserResources(t *testing.T) {
	outbound := map[string]any{
		"tag":      "sub2api-out",
		"protocol": "socks",
		"settings": map[string]any{},
	}
	config := buildXrayRuntimeConfig(1080, outbound, true)
	routing, ok := config["routing"].(map[string]any)
	if !ok || routing["domainStrategy"] != "IPOnDemand" {
		t.Fatalf("protected runtime is missing DNS-aware routing: %#v", config["routing"])
	}
	rules, ok := routing["rules"].([]map[string]any)
	if !ok || len(rules) != 1 || rules[0]["outboundTag"] != "sub2api-block" {
		t.Fatalf("protected runtime is missing the private destination rule: %#v", routing["rules"])
	}
	cidrs, ok := rules[0]["ip"].([]string)
	if !ok || !containsString(cidrs, "10.0.0.0/8") || !containsString(cidrs, "172.16.0.0/12") || !containsString(cidrs, "192.168.0.0/16") {
		t.Fatalf("protected runtime does not block private IPv4 ranges: %#v", rules[0]["ip"])
	}
	outbounds, ok := config["outbounds"].([]map[string]any)
	if !ok || len(outbounds) != 2 || outbounds[1]["protocol"] != "blackhole" {
		t.Fatalf("protected runtime is missing the blackhole outbound: %#v", config["outbounds"])
	}

	systemConfig := buildXrayRuntimeConfig(1081, outbound, false)
	if _, ok := systemConfig["routing"]; ok {
		t.Fatalf("system runtime unexpectedly received user-only routing restrictions: %#v", systemConfig["routing"])
	}
}

func TestXrayInstanceHashIncludesOwnerScope(t *testing.T) {
	ownerA := int64(7)
	ownerB := int64(8)
	outbound := map[string]any{"protocol": "socks", "settings": map[string]any{}}
	a := xrayInstanceHash("node", &Proxy{ID: 1, Kind: "xray", OwnerUserID: &ownerA}, outbound)
	b := xrayInstanceHash("node", &Proxy{ID: 1, Kind: "xray", OwnerUserID: &ownerB}, outbound)
	system := xrayInstanceHash("node", &Proxy{ID: 1, Kind: "xray"}, outbound)
	if a == b || a == system || b == system {
		t.Fatalf("owner scopes produced the same runtime hash: a=%s b=%s system=%s", a, b, system)
	}
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func TestXrayRuntimeManagerStartsRealProcess(t *testing.T) {
	if !strings.EqualFold(os.Getenv("XRAY_RUNTIME_E2E"), "true") {
		t.Skip("set XRAY_RUNTIME_E2E=true and XRAY_BIN to run the real xray runtime test")
	}
	bin := strings.TrimSpace(os.Getenv("XRAY_BIN"))
	if bin == "" {
		t.Skip("XRAY_BIN is required for the real xray runtime test")
	}

	manager := NewXrayRuntimeManager(bin, t.TempDir())
	defer func() { _ = manager.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	proxy := &Proxy{
		ID:   time.Now().UnixNano(),
		Kind: "xray",
		Extra: map[string]any{
			"outbound": map[string]any{
				"tag":      "direct",
				"protocol": "freedom",
				"settings": map[string]any{},
			},
		},
	}
	proxyURL, err := manager.ProxyURL(ctx, proxy)
	if err != nil {
		t.Fatalf("ProxyURL returned error: %v", err)
	}
	if !strings.HasPrefix(proxyURL, "socks5h://127.0.0.1:") {
		t.Fatalf("unexpected local SOCKS URL: %s", proxyURL)
	}

	hostPort := strings.TrimPrefix(proxyURL, "socks5h://")
	conn, err := net.DialTimeout("tcp", hostPort, time.Second)
	if err != nil {
		t.Fatalf("xray local SOCKS port is not reachable: %v", err)
	}
	_ = conn.Close()

	secondURL, err := manager.ProxyURL(ctx, proxy)
	if err != nil {
		t.Fatalf("second ProxyURL returned error: %v", err)
	}
	if secondURL != proxyURL {
		t.Fatalf("xray runtime did not reuse the live instance: first=%s second=%s", proxyURL, secondURL)
	}
}

func TestXrayXHTTPConfigPassesBinaryCheck(t *testing.T) {
	bin := strings.TrimSpace(os.Getenv("XRAY_BIN"))
	if bin == "" {
		t.Skip("set XRAY_BIN to run real xray config checks")
	}
	query := url.Values{
		"type":          {"xhttp"},
		"security":      {"tls"},
		"sni":           {"edge.example.com"},
		"allowInsecure": {"1"},
		"path":          {"/path"},
		"mode":          {"stream-up"},
		"extra":         {`{"downloadSettings":{"server":"203.0.113.20","port":443,"servername":"download.example.com"}}`},
	}
	outbound, err := buildXrayOutbound("vless://11111111-1111-1111-1111-111111111111@203.0.113.10:443?"+query.Encode(), &Proxy{Kind: "xray"})
	if err != nil {
		t.Fatalf("build xhttp outbound: %v", err)
	}
	config, err := json.Marshal(buildXrayRuntimeConfig(1080, outbound, true))
	if err != nil {
		t.Fatalf("marshal xhttp config: %v", err)
	}
	path := filepath.Join(t.TempDir(), "xhttp.json")
	if err := os.WriteFile(path, config, 0o600); err != nil {
		t.Fatalf("write xhttp config: %v", err)
	}
	if output, err := exec.Command(bin, "run", "-test", "-config", path).CombinedOutput(); err != nil {
		t.Fatalf("xray rejected generated xhttp config: %v: %s", err, strings.TrimSpace(string(output)))
	}
}

func TestXrayRuntimeConfigsPassBinaryCheck(t *testing.T) {
	bin := strings.TrimSpace(os.Getenv("XRAY_BIN"))
	if bin == "" {
		t.Skip("set XRAY_BIN to run real xray config checks")
	}

	vmessNode := func(network, security string) string {
		node := map[string]any{
			"add":  "203.0.113.10",
			"port": "443",
			"id":   "11111111-1111-1111-1111-111111111111",
			"aid":  0,
			"scy":  "auto",
			"net":  network,
			"type": "none",
			"host": "edge.example.com",
			"path": "/proxy",
			"tls":  security,
			"sni":  "edge.example.com",
		}
		raw, err := json.Marshal(node)
		if err != nil {
			t.Fatalf("marshal vmess node: %v", err)
		}
		return "vmess://" + base64.RawStdEncoding.EncodeToString(raw)
	}

	tests := map[string]string{
		"http":                  "http://user:secret@203.0.113.2:8080",
		"socks5":                "socks5://user:secret@203.0.113.3:1080",
		"vmess-tcp":             vmessNode("tcp", ""),
		"vmess-websocket-tls":   vmessNode("ws", "tls"),
		"vmess-grpc-tls":        vmessNode("grpc", "tls"),
		"vmess-kcp":             vmessNode("kcp", ""),
		"vless-tcp-tls":         "vless://11111111-1111-1111-1111-111111111111@203.0.113.11:443?security=tls&type=tcp&sni=edge.example.com",
		"vless-websocket-tls":   "vless://11111111-1111-1111-1111-111111111111@203.0.113.12:443?security=tls&type=ws&sni=edge.example.com&host=edge.example.com&path=%2Fproxy",
		"vless-grpc-reality":    "vless://11111111-1111-1111-1111-111111111111@203.0.113.13:443?security=reality&type=grpc&sni=edge.example.com&pbk=AgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgI&sid=0123456789abcdef&serviceName=proxy",
		"vless-httpupgrade-tls": "vless://11111111-1111-1111-1111-111111111111@203.0.113.14:443?security=tls&type=httpupgrade&sni=edge.example.com&host=edge.example.com&path=%2Fproxy",
		"vless-xhttp-tls":       "vless://11111111-1111-1111-1111-111111111111@203.0.113.15:443?security=tls&type=xhttp&sni=edge.example.com&host=edge.example.com&path=%2Fproxy&mode=auto",
		"trojan-tcp-tls":        "trojan://secret@203.0.113.21:443?security=tls&type=tcp&sni=edge.example.com",
		"trojan-websocket-tls":  "trojan://secret@203.0.113.22:443?security=tls&type=ws&sni=edge.example.com&host=edge.example.com&path=%2Fproxy",
		"trojan-grpc-tls":       "trojan://secret@203.0.113.23:443?security=tls&type=grpc&sni=edge.example.com&serviceName=proxy",
	}

	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			outbound, err := buildXrayOutbound(raw, &Proxy{Kind: "xray"})
			if err != nil {
				t.Fatalf("build xray outbound: %v", err)
			}
			config, err := json.Marshal(buildXrayRuntimeConfig(1080, outbound, true))
			if err != nil {
				t.Fatalf("marshal xray config: %v", err)
			}
			path := filepath.Join(t.TempDir(), name+".json")
			if err := os.WriteFile(path, config, 0o600); err != nil {
				t.Fatalf("write xray config: %v", err)
			}
			if output, err := exec.Command(bin, "run", "-test", "-config", path).CombinedOutput(); err != nil {
				t.Fatalf("xray rejected generated config: %v: %s", err, strings.TrimSpace(string(output)))
			}
		})
	}
}

func TestXrayRuntimeManagerConcurrentStartAndClose(t *testing.T) {
	workDir := t.TempDir()
	manager := NewXrayRuntimeManager("xray-test-helper", workDir)
	var starts atomic.Int32
	manager.commandFactory = func(_, configPath string) *exec.Cmd {
		starts.Add(1)
		cmd := exec.Command(os.Args[0], "-test.run=^TestXrayRuntimeHelperProcess$")
		cmd.Env = append(os.Environ(),
			"SUB2API_XRAY_HELPER=1",
			"SUB2API_XRAY_HELPER_CONFIG="+configPath,
		)
		return cmd
	}

	proxy := &Proxy{
		ID:   91234,
		Kind: "xray",
		Extra: map[string]any{
			"outbound": map[string]any{
				"tag":      "direct",
				"protocol": "freedom",
				"settings": map[string]any{},
			},
		},
	}

	const callers = 12
	start := make(chan struct{})
	urls := make(chan string, callers)
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			got, err := manager.ProxyURL(ctx, proxy)
			if err != nil {
				errs <- err
				return
			}
			urls <- got
		}()
	}
	close(start)
	wg.Wait()
	close(urls)
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent ProxyURL returned error: %v", err)
	}
	var first string
	for got := range urls {
		if first == "" {
			first = got
		}
		if got != first {
			t.Fatalf("concurrent calls returned different runtimes: first=%s got=%s", first, got)
		}
	}
	if got := starts.Load(); got != 1 {
		t.Fatalf("expected exactly one xray process, got %d", got)
	}

	if err := manager.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}
	entries, err := os.ReadDir(workDir)
	if err != nil {
		t.Fatalf("read work dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("xray secret files were not removed: %#v", entries)
	}
	if _, err := manager.ProxyURL(context.Background(), proxy); err == nil {
		t.Fatal("closed manager unexpectedly accepted a new runtime")
	}
}

func TestXrayRuntimeManagerEnforcesInstanceLimit(t *testing.T) {
	manager := NewXrayRuntimeManager("xray-test-helper", t.TempDir())
	manager.maxInstances = 1
	manager.commandFactory = func(_, configPath string) *exec.Cmd {
		cmd := exec.Command(os.Args[0], "-test.run=^TestXrayRuntimeHelperProcess$")
		cmd.Env = append(os.Environ(),
			"SUB2API_XRAY_HELPER=1",
			"SUB2API_XRAY_HELPER_CONFIG="+configPath,
		)
		return cmd
	}
	defer func() { _ = manager.Close() }()

	proxy := func(id int64) *Proxy {
		return &Proxy{
			ID:   id,
			Kind: "xray",
			Extra: map[string]any{
				"outbound": map[string]any{
					"tag":      "direct",
					"protocol": "freedom",
					"settings": map[string]any{},
				},
			},
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := manager.ProxyURL(ctx, proxy(1)); err != nil {
		t.Fatalf("start first runtime: %v", err)
	}
	if _, err := manager.ProxyURL(ctx, proxy(2)); err == nil || !strings.Contains(err.Error(), "instance limit") {
		t.Fatalf("expected instance limit error, got %v", err)
	}
	if err := manager.Stop(1); err != nil {
		t.Fatalf("stop first runtime: %v", err)
	}
	if _, err := manager.ProxyURL(ctx, proxy(2)); err != nil {
		t.Fatalf("start runtime after releasing capacity: %v", err)
	}
}

func TestXrayRuntimeManagerPrunesIdleInstance(t *testing.T) {
	manager := NewXrayRuntimeManager("xray-test-helper", t.TempDir())
	manager.maxInstances = 1
	manager.idleTTL = time.Minute
	manager.commandFactory = func(_, configPath string) *exec.Cmd {
		cmd := exec.Command(os.Args[0], "-test.run=^TestXrayRuntimeHelperProcess$")
		cmd.Env = append(os.Environ(),
			"SUB2API_XRAY_HELPER=1",
			"SUB2API_XRAY_HELPER_CONFIG="+configPath,
		)
		return cmd
	}
	defer func() { _ = manager.Close() }()

	proxy := func(id int64) *Proxy {
		return &Proxy{
			ID: id, Kind: "xray",
			Extra: map[string]any{"outbound": map[string]any{
				"tag": "direct", "protocol": "freedom", "settings": map[string]any{},
			}},
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := manager.ProxyURL(ctx, proxy(1)); err != nil {
		t.Fatalf("start first Xray runtime: %v", err)
	}
	manager.mu.Lock()
	manager.instances[1].lastUsed = time.Now().Add(-2 * time.Minute)
	manager.mu.Unlock()
	if _, err := manager.ProxyURL(ctx, proxy(2)); err != nil {
		t.Fatalf("idle Xray runtime did not release capacity: %v", err)
	}
	manager.mu.Lock()
	_, oldExists := manager.instances[1]
	_, newExists := manager.instances[2]
	manager.mu.Unlock()
	if oldExists || !newExists {
		t.Fatalf("unexpected Xray runtime set after idle pruning: old=%t new=%t", oldExists, newExists)
	}
}

func TestXrayRuntimeManagerEnforcesPerUserInstanceLimit(t *testing.T) {
	manager := NewXrayRuntimeManager("xray-test-helper", t.TempDir())
	manager.maxInstances = 4
	manager.maxInstancesPerUser = 1
	manager.commandFactory = func(_, configPath string) *exec.Cmd {
		cmd := exec.Command(os.Args[0], "-test.run=^TestXrayRuntimeHelperProcess$")
		cmd.Env = append(os.Environ(),
			"SUB2API_XRAY_HELPER=1",
			"SUB2API_XRAY_HELPER_CONFIG="+configPath,
		)
		return cmd
	}
	defer func() { _ = manager.Close() }()

	proxy := func(id, ownerID int64) *Proxy {
		return &Proxy{
			ID: id, Kind: "xray", OwnerUserID: &ownerID,
			Extra: map[string]any{"outbound": map[string]any{
				"protocol": "socks",
				"settings": map[string]any{"servers": []map[string]any{{"address": "8.8.8.8", "port": 1080}}},
			}},
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := manager.ProxyURL(ctx, proxy(1, 7)); err != nil {
		t.Fatalf("start first user runtime: %v", err)
	}
	if _, err := manager.ProxyURL(ctx, proxy(2, 7)); err == nil || !strings.Contains(err.Error(), "per-user instance limit") {
		t.Fatalf("expected per-user instance limit error, got %v", err)
	}
	if _, err := manager.ProxyURL(ctx, proxy(3, 8)); err != nil {
		t.Fatalf("another owner should retain independent capacity: %v", err)
	}
}

func TestXrayRuntimeHelperProcess(t *testing.T) {
	if os.Getenv("SUB2API_XRAY_HELPER") != "1" {
		return
	}
	raw, err := os.ReadFile(os.Getenv("SUB2API_XRAY_HELPER_CONFIG"))
	if err != nil {
		t.Fatalf("read helper config: %v", err)
	}
	var cfg struct {
		Inbounds []struct {
			Port int `json:"port"`
		} `json:"inbounds"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil || len(cfg.Inbounds) == 0 {
		t.Fatalf("decode helper config: %v", err)
	}
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(cfg.Inbounds[0].Port)))
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
