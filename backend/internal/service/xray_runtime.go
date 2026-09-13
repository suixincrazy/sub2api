package service

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	xrayProxyUnavailableURL      = "xray://unavailable"
	defaultXrayMaxInstances      = 64
	defaultXrayMaxInstancesUser  = 16
	maximumXrayConfiguredRuntime = 1024
	xrayLegacyInsecureMarker     = "_sub2api_legacy_allow_insecure"
)

var xrayBlockedDestinationCIDRs = []string{
	"0.0.0.0/8",
	"10.0.0.0/8",
	"100.64.0.0/10",
	"127.0.0.0/8",
	"169.254.0.0/16",
	"172.16.0.0/12",
	"192.168.0.0/16",
	"224.0.0.0/4",
	"240.0.0.0/4",
	"::/128",
	"::1/128",
	"fc00::/7",
	"fe80::/10",
	"ff00::/8",
}

var (
	defaultXrayRuntimeOnce sync.Once
	defaultXrayRuntime     *XrayRuntimeManager
)

// DefaultXrayRuntimeManager returns the process manager used by Proxy.URL for kind=xray proxies.
func DefaultXrayRuntimeManager() *XrayRuntimeManager {
	defaultXrayRuntimeOnce.Do(func() {
		defaultXrayRuntime = NewXrayRuntimeManager(os.Getenv("XRAY_BIN"), os.Getenv("XRAY_WORK_DIR"))
	})
	return defaultXrayRuntime
}

type XrayRuntimeManager struct {
	mu                  sync.Mutex
	bin                 string
	workDir             string
	instances           map[int64]*xrayRuntimeInstance
	maxInstances        int
	maxInstancesPerUser int
	idleTTL             time.Duration
	closed              bool
	commandFactory      func(bin, configPath string) *exec.Cmd
}

type xrayRuntimeInstance struct {
	proxyID     int64
	ownerUserID *int64
	sourceHash  string
	hash        string
	port        int
	cmd         *exec.Cmd
	configPath  string
	logPath     string
	done        chan error
	lastUsed    time.Time
}

func NewXrayRuntimeManager(bin, workDir string) *XrayRuntimeManager {
	if strings.TrimSpace(workDir) == "" {
		workDir = filepath.Join(os.TempDir(), "sub2api-xray")
	}
	maxInstances := parseXrayMaxInstances(os.Getenv("XRAY_MAX_INSTANCES"))
	maxInstancesPerUser := parseXrayMaxInstancesPerUser(os.Getenv("XRAY_MAX_INSTANCES_PER_USER"))
	if maxInstancesPerUser > maxInstances {
		maxInstancesPerUser = maxInstances
	}
	return &XrayRuntimeManager{
		bin:                 strings.TrimSpace(bin),
		workDir:             workDir,
		instances:           map[int64]*xrayRuntimeInstance{},
		maxInstances:        maxInstances,
		maxInstancesPerUser: maxInstancesPerUser,
		idleTTL:             parseProxyRuntimeIdleTTL(os.Getenv("XRAY_RUNTIME_IDLE_TTL")),
	}
}

func parseXrayMaxInstances(raw string) int {
	limit, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || limit <= 0 {
		return defaultXrayMaxInstances
	}
	if limit > maximumXrayConfiguredRuntime {
		return maximumXrayConfiguredRuntime
	}
	return limit
}

func parseXrayMaxInstancesPerUser(raw string) int {
	limit, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || limit <= 0 {
		return defaultXrayMaxInstancesUser
	}
	if limit > maximumXrayConfiguredRuntime {
		return maximumXrayConfiguredRuntime
	}
	return limit
}

func (m *XrayRuntimeManager) ProxyURL(ctx context.Context, p *Proxy) (string, error) {
	if p == nil {
		return "", errors.New("proxy is nil")
	}
	if !strings.EqualFold(p.Kind, "xray") {
		return p.StandardURL(), nil
	}
	raw := xrayRawNode(p)
	outbound, err := buildXrayOutbound(raw, p)
	if err != nil {
		return "", err
	}
	sourceHash := xrayInstanceHash(raw, p, outbound)
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return "", errors.New("xray runtime manager is closed")
	}
	if inst := m.instances[p.ID]; inst != nil && inst.sourceHash == sourceHash && inst.alive() {
		inst.lastUsed = time.Now()
		proxyURL := localSocksURL(inst.port)
		m.mu.Unlock()
		return proxyURL, nil
	}
	m.mu.Unlock()

	if p.OwnerUserID != nil {
		if err := pinUserOwnedXrayOutbound(ctx, outbound); err != nil {
			return "", fmt.Errorf("xray outbound endpoint is not public: %w", err)
		}
		outbound["tag"] = "sub2api-out"
	}
	if err := prepareXrayTLSCompatibility(ctx, outbound); err != nil {
		return "", err
	}
	fingerprint := xrayInstanceHash(raw, p, outbound)

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return "", errors.New("xray runtime manager is closed")
	}
	if inst := m.instances[p.ID]; inst != nil && inst.sourceHash == sourceHash && inst.alive() {
		inst.lastUsed = time.Now()
		return localSocksURL(inst.port), nil
	}
	if old := m.instances[p.ID]; old != nil {
		delete(m.instances, p.ID)
		_ = old.stop()
	}
	m.pruneInactiveLocked(time.Now())
	if len(m.instances) >= m.maxInstances {
		return "", fmt.Errorf("xray runtime instance limit reached (%d)", m.maxInstances)
	}
	if p.OwnerUserID != nil {
		ownerInstances := 0
		for _, inst := range m.instances {
			if inst != nil && inst.ownerUserID != nil && *inst.ownerUserID == *p.OwnerUserID && inst.alive() {
				ownerInstances++
			}
		}
		if ownerInstances >= m.maxInstancesPerUser {
			return "", fmt.Errorf("xray runtime per-user instance limit reached (%d)", m.maxInstancesPerUser)
		}
	}

	inst, err := m.start(ctx, p.ID, p.OwnerUserID, fingerprint, outbound, p.OwnerUserID != nil)
	if err != nil {
		return "", err
	}
	inst.sourceHash = sourceHash

	m.instances[p.ID] = inst
	return localSocksURL(inst.port), nil
}

func (m *XrayRuntimeManager) pruneInactiveLocked(now time.Time) {
	for id, inst := range m.instances {
		if inst != nil && inst.alive() && !proxyRuntimeIsIdle(inst.lastUsed, now, m.idleTTL) {
			continue
		}
		delete(m.instances, id)
		_ = inst.stop()
	}
}

func pinUserOwnedXrayOutbound(ctx context.Context, outbound map[string]any) error {
	settings, _ := outbound["settings"].(map[string]any)
	if settings == nil {
		return errors.New("xray outbound settings are missing")
	}
	pinned := 0
	for _, key := range []string{"vnext", "servers"} {
		servers, ok := settings[key].([]map[string]any)
		if !ok {
			if rawServers, rawOK := settings[key].([]any); rawOK {
				servers = make([]map[string]any, 0, len(rawServers))
				for _, rawServer := range rawServers {
					if server, mapOK := rawServer.(map[string]any); mapOK {
						servers = append(servers, server)
					}
				}
			}
		}
		for _, server := range servers {
			host := strings.TrimSpace(stringFromMap(server, "address"))
			if host == "" {
				continue
			}
			ips, err := resolveExternalHostIPs(ctx, host)
			if err != nil {
				return err
			}
			if len(ips) == 0 {
				return fmt.Errorf("xray endpoint %q resolved to no addresses", host)
			}
			preserveXrayTLSServerName(outbound, host)
			server["address"] = ips[0].String()
			pinned++
		}
	}
	if err := pinUserOwnedXrayStreamEndpoints(ctx, outbound); err != nil {
		return err
	}
	if pinned == 0 {
		return errors.New("xray outbound has no server address")
	}
	return nil
}

func pinUserOwnedXrayStreamEndpoints(ctx context.Context, outbound map[string]any) error {
	stream, _ := outbound["streamSettings"].(map[string]any)
	xhttp, _ := stream["xhttpSettings"].(map[string]any)
	extra, _ := xhttp["extra"].(map[string]any)
	download, _ := extra["downloadSettings"].(map[string]any)
	if download == nil {
		return nil
	}

	key := "address"
	host := strings.TrimSpace(stringFromMap(download, key))
	if host == "" {
		key = "server"
		host = strings.TrimSpace(stringFromMap(download, key))
	}
	if host == "" {
		return nil
	}
	ips, err := resolveExternalHostIPs(ctx, host)
	if err != nil {
		return fmt.Errorf("xray xhttp download endpoint is not public: %w", err)
	}
	if len(ips) == 0 {
		return fmt.Errorf("xray xhttp download endpoint %q resolved to no addresses", host)
	}
	if net.ParseIP(host) == nil {
		tls, _ := download["tlsSettings"].(map[string]any)
		if tls != nil && strings.TrimSpace(stringFromMap(tls, "serverName")) == "" {
			tls["serverName"] = host
		} else if tls == nil && strings.TrimSpace(stringFromMap(download, "servername")) == "" {
			download["servername"] = host
		}
	}
	download[key] = ips[0].String()
	return nil
}

func prepareXrayTLSCompatibility(ctx context.Context, outbound map[string]any) error {
	stream, _ := outbound["streamSettings"].(map[string]any)
	if stream == nil {
		return nil
	}
	if tlsSettings, ok := stream["tlsSettings"].(map[string]any); ok {
		host, port := firstXrayOutboundEndpoint(outbound)
		if err := pinLegacyInsecureCertificate(ctx, tlsSettings, host, port); err != nil {
			return fmt.Errorf("prepare xray TLS certificate pin: %w", err)
		}
	}

	xhttp, _ := stream["xhttpSettings"].(map[string]any)
	extra, _ := xhttp["extra"].(map[string]any)
	download, _ := extra["downloadSettings"].(map[string]any)
	downloadTLS, _ := download["tlsSettings"].(map[string]any)
	if downloadTLS == nil {
		return nil
	}
	host := strings.TrimSpace(stringFromMap(download, "address"))
	port := intFromAnyValue(download["port"])
	if err := pinLegacyInsecureCertificate(ctx, downloadTLS, host, port); err != nil {
		return fmt.Errorf("prepare xray xhttp download certificate pin: %w", err)
	}
	return nil
}

func firstXrayOutboundEndpoint(outbound map[string]any) (string, int) {
	settings, _ := outbound["settings"].(map[string]any)
	for _, key := range []string{"vnext", "servers"} {
		for _, server := range mapSliceFromAny(settings[key]) {
			if host := strings.TrimSpace(stringFromMap(server, "address")); host != "" {
				return host, intFromAnyValue(server["port"])
			}
		}
	}
	return "", 0
}

func pinLegacyInsecureCertificate(ctx context.Context, tlsSettings map[string]any, host string, port int) error {
	legacy, _ := tlsSettings[xrayLegacyInsecureMarker].(bool)
	delete(tlsSettings, xrayLegacyInsecureMarker)
	if !legacy || strings.TrimSpace(stringFromMap(tlsSettings, "pinnedPeerCertSha256")) != "" {
		return nil
	}
	if strings.TrimSpace(host) == "" || port <= 0 || port > 65535 {
		return errors.New("legacy insecure TLS endpoint is missing")
	}
	serverName := strings.TrimSpace(stringFromMap(tlsSettings, "serverName"))
	if serverName == "" && net.ParseIP(host) == nil {
		serverName = host
	}
	pin, err := fetchPeerCertificateSHA256(ctx, host, port, serverName)
	if err != nil {
		// Xray 26 removed allowInsecure. Returning a generic error here avoids
		// emitting a config the runtime will reject and keeps the endpoint out of
		// user-visible diagnostics.
		return errors.New("legacy insecure TLS certificate could not be pinned")
	}
	tlsSettings["pinnedPeerCertSha256"] = pin
	return nil
}

func fetchPeerCertificateSHA256(ctx context.Context, host string, port int, serverName string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	dialCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	dialer := &tls.Dialer{
		NetDialer: &net.Dialer{Timeout: 8 * time.Second},
		// The resulting hash is enforced by Xray on the actual connection.
		Config: &tls.Config{ServerName: serverName, InsecureSkipVerify: true}, // #nosec G402
	}
	conn, err := dialer.DialContext(dialCtx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return "", errors.New("failed to obtain peer certificate")
	}
	defer func() { _ = conn.Close() }()
	tlsConn, ok := conn.(*tls.Conn)
	if !ok || len(tlsConn.ConnectionState().PeerCertificates) == 0 {
		return "", errors.New("peer did not provide a certificate")
	}
	hash := sha256.Sum256(tlsConn.ConnectionState().PeerCertificates[0].Raw)
	return hex.EncodeToString(hash[:]), nil
}

func preserveXrayTLSServerName(outbound map[string]any, host string) {
	if net.ParseIP(host) != nil {
		return
	}
	stream, _ := outbound["streamSettings"].(map[string]any)
	if stream == nil {
		return
	}
	for _, key := range []string{"tlsSettings", "realitySettings"} {
		settings, _ := stream[key].(map[string]any)
		if settings == nil || strings.TrimSpace(stringFromMap(settings, "serverName")) != "" {
			continue
		}
		settings["serverName"] = host
	}
}

// Stop terminates one proxy runtime and removes its on-disk secrets.
func (m *XrayRuntimeManager) Stop(proxyID int64) error {
	if m == nil || proxyID <= 0 {
		return nil
	}
	m.mu.Lock()
	inst := m.instances[proxyID]
	delete(m.instances, proxyID)
	m.mu.Unlock()
	if inst == nil {
		return nil
	}
	if err := inst.stop(); err != nil {
		m.mu.Lock()
		if !m.closed {
			if _, exists := m.instances[proxyID]; !exists {
				m.instances[proxyID] = inst
			}
		}
		m.mu.Unlock()
		return err
	}
	return nil
}

// Close terminates every managed runtime. A closed manager cannot be reused.
func (m *XrayRuntimeManager) Close() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	instances := make([]*xrayRuntimeInstance, 0, len(m.instances))
	for id, inst := range m.instances {
		instances = append(instances, inst)
		delete(m.instances, id)
	}
	m.mu.Unlock()

	var errs []error
	for _, inst := range instances {
		if err := inst.stop(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (m *XrayRuntimeManager) start(ctx context.Context, proxyID int64, ownerUserID *int64, hash string, outbound map[string]any, blockPrivateDestinations bool) (*xrayRuntimeInstance, error) {
	bin, err := m.resolveBinary()
	if err != nil {
		return nil, err
	}
	port, err := reserveLocalPort()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(m.workDir, 0o700); err != nil { //nolint:gosec // G703: workDir 仅来自启动环境变量 XRAY_WORK_DIR（运维配置）或 os.TempDir()，非请求输入
		return nil, err
	}
	config := buildXrayRuntimeConfig(port, outbound, blockPrivateDestinations)
	rawConfig, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return nil, err
	}
	prefix := fmt.Sprintf("proxy-%d-%s", proxyID, hash[:12])
	configPath := filepath.Join(m.workDir, prefix+".json")
	logPath := filepath.Join(m.workDir, prefix+".log")
	if err := os.WriteFile(configPath, rawConfig, 0o600); err != nil { //nolint:gosec // G703: 同上；文件名为 proxy-<int64>-<hash 前 12 位>，不含路径分隔符
		return nil, err
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // G703: 同上
	if err != nil {
		_ = os.Remove(configPath)
		return nil, err
	}
	cmd := exec.CommandContext(context.Background(), bin, "run", "-config", configPath) //nolint:gosec // G702: bin 仅来自启动环境变量 XRAY_BIN（运维配置）或 exec.LookPath("xray")，参数为固定字面量 + 上面自建的 configPath，非请求输入
	if m.commandFactory != nil {
		cmd = m.commandFactory(bin, configPath)
	}
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		_ = os.Remove(configPath)
		_ = os.Remove(logPath)
		return nil, err
	}
	inst := &xrayRuntimeInstance{
		proxyID:     proxyID,
		ownerUserID: cloneXrayOwnerID(ownerUserID),
		hash:        hash,
		port:        port,
		cmd:         cmd,
		configPath:  configPath,
		logPath:     logPath,
		done:        make(chan error, 1),
		lastUsed:    time.Now(),
	}
	go func() {
		inst.done <- cmd.Wait()
		close(inst.done)
		_ = logFile.Close()
		_ = os.Remove(configPath)
		_ = os.Remove(logPath)
	}()

	waitCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	if err := waitForLocalPort(waitCtx, port, inst.done); err != nil {
		_ = inst.stop()
		return nil, fmt.Errorf("xray runtime did not become ready: %w", err)
	}
	return inst, nil
}

func cloneXrayOwnerID(value *int64) *int64 {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func buildXrayRuntimeConfig(port int, outbound map[string]any, blockPrivateDestinations bool) map[string]any {
	removeXrayInternalTLSMarkers(outbound)
	config := map[string]any{
		"log": map[string]any{
			"loglevel": "warning",
		},
		"inbounds": []map[string]any{
			{
				"tag":      "sub2api-in",
				"listen":   "127.0.0.1",
				"port":     port,
				"protocol": "socks",
				"settings": map[string]any{
					"auth": "noauth",
					"udp":  true,
				},
			},
		},
		"outbounds": []map[string]any{outbound},
	}
	if blockPrivateDestinations {
		config["outbounds"] = []map[string]any{
			outbound,
			{
				"tag":      "sub2api-block",
				"protocol": "blackhole",
				"settings": map[string]any{},
			},
		}
		config["routing"] = map[string]any{
			"domainStrategy": "IPOnDemand",
			"rules": []map[string]any{
				{
					"type":        "field",
					"ip":          append([]string(nil), xrayBlockedDestinationCIDRs...),
					"outboundTag": "sub2api-block",
				},
			},
		}
	}
	return config
}

func removeXrayInternalTLSMarkers(outbound map[string]any) {
	stream, _ := outbound["streamSettings"].(map[string]any)
	if stream == nil {
		return
	}
	if tlsSettings, ok := stream["tlsSettings"].(map[string]any); ok {
		delete(tlsSettings, xrayLegacyInsecureMarker)
	}
	xhttp, _ := stream["xhttpSettings"].(map[string]any)
	extra, _ := xhttp["extra"].(map[string]any)
	download, _ := extra["downloadSettings"].(map[string]any)
	if tlsSettings, ok := download["tlsSettings"].(map[string]any); ok {
		delete(tlsSettings, xrayLegacyInsecureMarker)
	}
}

func (m *XrayRuntimeManager) resolveBinary() (string, error) {
	if m.bin != "" {
		return m.bin, nil
	}
	if path, err := exec.LookPath("xray"); err == nil {
		m.bin = path
		return path, nil
	}
	return "", errors.New("xray binary not found; set XRAY_BIN")
}

func (i *xrayRuntimeInstance) alive() bool {
	if i == nil || i.cmd == nil || i.cmd.Process == nil {
		return false
	}
	select {
	case <-i.done:
		return false
	default:
		return true
	}
}

func (i *xrayRuntimeInstance) stop() error {
	if i == nil {
		return nil
	}
	if i.cmd != nil && i.cmd.Process != nil && i.alive() {
		if err := i.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return err
		}
		select {
		case <-i.done:
		case <-time.After(2 * time.Second):
		}
	}
	var errs []error
	for _, path := range []string{i.configPath, i.logPath} {
		if path == "" {
			continue
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func reserveLocalPort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer func() { _ = l.Close() }()
	tcpAddr, ok := l.Addr().(*net.TCPAddr)
	if !ok {
		return 0, fmt.Errorf("unexpected listener address type %T", l.Addr())
	}
	return tcpAddr.Port, nil
}

func waitForLocalPort(ctx context.Context, port int, done <-chan error) error {
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var lastErr error
	for {
		select {
		case err := <-done:
			if err == nil {
				err = errors.New("xray exited")
			}
			return err
		case <-ctx.Done():
			if lastErr != nil {
				return lastErr
			}
			return ctx.Err()
		case <-ticker.C:
			conn, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
			if err == nil {
				_ = conn.Close()
				return nil
			}
			lastErr = err
		}
	}
}

func localSocksURL(port int) string {
	return "socks5h://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
}

func xrayRawNode(p *Proxy) string {
	if p == nil || p.Extra == nil {
		return ""
	}
	for _, key := range []string{"raw", "uri", "node", "node_uri", "share_link"} {
		if v, ok := p.Extra[key].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func xrayInstanceHash(raw string, p *Proxy, outbound map[string]any) string {
	h := sha256.New()
	_, _ = h.Write([]byte(raw))
	_, _ = h.Write([]byte(p.Protocol))
	_, _ = h.Write([]byte(p.Host))
	_, _ = h.Write([]byte(strconv.Itoa(p.Port)))
	if p.OwnerUserID != nil {
		_, _ = h.Write([]byte("owner:" + strconv.FormatInt(*p.OwnerUserID, 10)))
	} else {
		_, _ = h.Write([]byte("owner:system"))
	}
	if encoded, err := json.Marshal(outbound); err == nil {
		_, _ = h.Write(encoded)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func buildXrayOutbound(raw string, p *Proxy) (map[string]any, error) {
	if p != nil && p.Extra != nil {
		if outbound, ok := p.Extra["outbound"].(map[string]any); ok && len(outbound) > 0 {
			return outbound, nil
		}
		if outbound, ok := p.Extra["xray_outbound"].(map[string]any); ok && len(outbound) > 0 {
			return outbound, nil
		}
	}
	raw = strings.TrimSpace(raw)
	if raw == "" && p != nil {
		return buildXrayStandardOutbound(p)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" {
		return nil, fmt.Errorf("invalid xray node uri")
	}
	switch strings.ToLower(u.Scheme) {
	case "vmess":
		return buildVMessOutbound(raw)
	case "vless":
		return buildVLESSOutbound(u)
	case "trojan":
		return buildTrojanOutbound(u)
	case "ss", "shadowsocks":
		return buildShadowsocksOutbound(raw)
	case "socks", "socks5", "socks5h", "http", "https":
		return buildStandardURLOutbound(u)
	default:
		return nil, fmt.Errorf("unsupported xray node protocol %q", u.Scheme)
	}
}

func buildXrayStandardOutbound(p *Proxy) (map[string]any, error) {
	if p == nil || p.Host == "" || p.Port <= 0 {
		return nil, errors.New("missing xray proxy endpoint")
	}
	u := &url.URL{Scheme: p.Protocol, Host: net.JoinHostPort(p.Host, strconv.Itoa(p.Port))}
	if p.Username != "" || p.Password != "" {
		u.User = url.UserPassword(p.Username, p.Password)
	}
	return buildStandardURLOutbound(u)
}

func buildStandardURLOutbound(u *url.URL) (map[string]any, error) {
	protocol := strings.ToLower(u.Scheme)
	if protocol == "socks5" || protocol == "socks5h" {
		protocol = "socks"
	}
	if protocol == "https" {
		protocol = "http"
	}
	port := portFromURL(u)
	if u.Hostname() == "" || port <= 0 {
		return nil, errors.New("missing proxy host or port")
	}
	server := map[string]any{"address": u.Hostname(), "port": port}
	if u.User != nil {
		user := u.User.Username()
		pass, _ := u.User.Password()
		if user != "" || pass != "" {
			server["users"] = []map[string]any{{"user": user, "pass": pass}}
		}
	}
	return taggedOutbound(protocol, map[string]any{"servers": []map[string]any{server}}, nil), nil
}

func buildVMessOutbound(raw string) (map[string]any, error) {
	payload := strings.TrimSpace(raw)
	if len(payload) >= len("vmess://") && strings.EqualFold(payload[:len("vmess://")], "vmess://") {
		payload = payload[len("vmess://"):]
	}
	decoded, ok := decodeShareBase64(payload)
	if !ok {
		return nil, errors.New("invalid vmess payload")
	}
	var node map[string]any
	if err := json.Unmarshal([]byte(decoded), &node); err != nil {
		return nil, err
	}
	address := stringFromMap(node, "add")
	port := intFromAnyValue(node["port"])
	id := stringFromMap(node, "id")
	if address == "" || port <= 0 || id == "" {
		return nil, errors.New("vmess node missing address, port or id")
	}
	user := map[string]any{"id": id, "alterId": intFromAnyValue(node["aid"])}
	if security := stringFromMap(node, "scy"); security != "" {
		user["security"] = security
	} else {
		user["security"] = "auto"
	}
	settings := map[string]any{
		"vnext": []map[string]any{{
			"address": address,
			"port":    port,
			"users":   []map[string]any{user},
		}},
	}
	q := url.Values{}
	copyValue(q, "type", stringFromMap(node, "net"))
	copyValue(q, "headerType", stringFromMap(node, "type"))
	copyValue(q, "host", stringFromMap(node, "host"))
	copyValue(q, "path", stringFromMap(node, "path"))
	copyValue(q, "security", stringFromMap(node, "tls"))
	copyValue(q, "sni", stringFromMap(node, "sni"))
	copyValue(q, "fp", stringFromMap(node, "fp"))
	copyValue(q, "alpn", stringFromMap(node, "alpn"))
	copyValue(q, "allowInsecure", stringFromMap(node, "allowInsecure"))
	stream, err := xrayStreamSettings(q)
	if err != nil {
		return nil, err
	}
	return taggedOutbound("vmess", settings, stream), nil
}

func buildVLESSOutbound(u *url.URL) (map[string]any, error) {
	port := portFromURL(u)
	id := u.User.Username()
	if u.Hostname() == "" || port <= 0 || id == "" {
		return nil, errors.New("vless node missing address, port or uuid")
	}
	q := u.Query()
	user := map[string]any{"id": id, "encryption": firstQuery(q, "encryption")}
	if user["encryption"] == "" {
		user["encryption"] = "none"
	}
	if flow := firstQuery(q, "flow"); flow != "" {
		user["flow"] = flow
	}
	settings := map[string]any{
		"vnext": []map[string]any{{
			"address": u.Hostname(),
			"port":    port,
			"users":   []map[string]any{user},
		}},
	}
	stream, err := xrayStreamSettings(q)
	if err != nil {
		return nil, err
	}
	return taggedOutbound("vless", settings, stream), nil
}

func buildTrojanOutbound(u *url.URL) (map[string]any, error) {
	port := portFromURL(u)
	password := u.User.Username()
	if u.Hostname() == "" || port <= 0 || password == "" {
		return nil, errors.New("trojan node missing address, port or password")
	}
	server := map[string]any{"address": u.Hostname(), "port": port, "password": password}
	if flow := firstQuery(u.Query(), "flow"); flow != "" {
		server["flow"] = flow
	}
	settings := map[string]any{"servers": []map[string]any{server}}
	stream, err := xrayStreamSettings(u.Query())
	if err != nil {
		return nil, err
	}
	return taggedOutbound("trojan", settings, stream), nil
}

func buildShadowsocksOutbound(raw string) (map[string]any, error) {
	method, password, host, port, err := parseShadowsocksShare(raw)
	if err != nil {
		return nil, err
	}
	settings := map[string]any{
		"servers": []map[string]any{{
			"address":  host,
			"port":     port,
			"method":   method,
			"password": password,
		}},
	}
	return taggedOutbound("shadowsocks", settings, nil), nil
}

func taggedOutbound(protocol string, settings map[string]any, stream map[string]any) map[string]any {
	out := map[string]any{
		"tag":      "sub2api-out",
		"protocol": protocol,
		"settings": settings,
	}
	if len(stream) > 0 {
		out["streamSettings"] = stream
	}
	return out
}

func xrayStreamSettings(q url.Values) (map[string]any, error) {
	network, err := canonicalXrayNetwork(firstQuery(q, "type", "network"))
	if err != nil {
		return nil, err
	}
	security := firstQuery(q, "security", "tls")
	if security == "none" {
		security = ""
	}
	out := map[string]any{"network": network}
	if security != "" {
		out["security"] = security
	}
	switch security {
	case "tls":
		if tls := tlsSettings(q); len(tls) > 0 {
			out["tlsSettings"] = tls
		}
	case "reality":
		if reality := realitySettings(q); len(reality) > 0 {
			out["realitySettings"] = reality
		}
	}
	switch network {
	case "ws", "websocket":
		out["network"] = "ws"
		ws := map[string]any{}
		if path := firstQuery(q, "path"); path != "" {
			ws["path"] = path
		}
		if host := firstQuery(q, "host"); host != "" {
			ws["headers"] = map[string]any{"Host": host}
		}
		if maxEarlyData := intQuery(q, "ed", "maxEarlyData", "max_early_data"); maxEarlyData > 0 {
			ws["maxEarlyData"] = maxEarlyData
		}
		if earlyDataHeader := firstQuery(q, "eh", "earlyDataHeaderName", "early_data_header_name"); earlyDataHeader != "" {
			ws["earlyDataHeaderName"] = earlyDataHeader
		}
		out["wsSettings"] = ws
	case "grpc":
		grpc := map[string]any{}
		if serviceName := firstQuery(q, "serviceName", "service_name", "path"); serviceName != "" {
			grpc["serviceName"] = strings.TrimPrefix(serviceName, "/")
		}
		if authority := firstQuery(q, "authority", "host"); authority != "" {
			grpc["authority"] = authority
		}
		mode := strings.ToLower(firstQuery(q, "mode", "grpcMode", "grpc_mode"))
		if mode == "multi" || boolString(firstQuery(q, "multiMode", "multi_mode")) {
			grpc["multiMode"] = true
		}
		out["grpcSettings"] = grpc
	case "httpupgrade":
		httpUpgrade := map[string]any{}
		if path := firstQuery(q, "path"); path != "" {
			httpUpgrade["path"] = path
		}
		if host := firstQuery(q, "host"); host != "" {
			httpUpgrade["host"] = host
		}
		out["httpupgradeSettings"] = httpUpgrade
	case "xhttp":
		xhttp := map[string]any{}
		for _, key := range []string{"path", "host", "mode"} {
			if value := firstQuery(q, key); value != "" {
				xhttp[key] = value
			}
		}
		if rawExtra := strings.TrimSpace(q.Get("extra")); rawExtra != "" {
			var extra map[string]any
			if err := json.Unmarshal([]byte(rawExtra), &extra); err != nil {
				return nil, errors.New("invalid xhttp extra settings")
			}
			if len(extra) > 0 {
				if err := normalizeXHTTPDownloadSettings(extra, boolString(firstQuery(q, "allowInsecure", "allow_insecure"))); err != nil {
					return nil, err
				}
				xhttp["extra"] = extra
			}
		}
		out["xhttpSettings"] = xhttp
	case "kcp":
		kcpSettings, finalMask, err := xrayKCPCompatibilitySettings(q)
		if err != nil {
			return nil, err
		}
		out["kcpSettings"] = kcpSettings
		out["finalmask"] = finalMask
	case "tcp", "raw":
		headerType := firstQuery(q, "headerType", "header")
		if headerType != "" && headerType != "none" {
			settingsKey := "tcpSettings"
			if network == "raw" {
				settingsKey = "rawSettings"
			}
			out[settingsKey] = map[string]any{"header": map[string]any{"type": headerType}}
		}
	}
	return out, nil
}

func canonicalXrayNetwork(raw string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "tcp":
		return "tcp", nil
	case "raw":
		return "raw", nil
	case "mkcp", "kcp":
		return "kcp", nil
	case "websocket", "ws":
		return "ws", nil
	case "grpc":
		return "grpc", nil
	case "httpupgrade", "http-upgrade", "http_upgrade":
		return "httpupgrade", nil
	case "xhttp", "splithttp":
		return "xhttp", nil
	case "http", "h2", "http2":
		return "", errors.New("xray HTTP/H2 transport was removed; use an XHTTP node")
	case "quic":
		return "", errors.New("xray QUIC transport was removed; use an XHTTP/H3 node")
	default:
		return "", errors.New("unsupported xray transport")
	}
}

func xrayKCPCompatibilitySettings(q url.Values) (map[string]any, map[string]any, error) {
	settings := map[string]any{}
	for target, keys := range map[string][]string{
		"mtu":              {"mtu"},
		"tti":              {"tti"},
		"uplinkCapacity":   {"uplinkCapacity", "uplink_capacity", "up"},
		"downlinkCapacity": {"downlinkCapacity", "downlink_capacity", "down"},
		"readBufferSize":   {"readBufferSize", "read_buffer_size"},
		"writeBufferSize":  {"writeBufferSize", "write_buffer_size"},
	} {
		if value := intQuery(q, keys...); value > 0 {
			settings[target] = value
		}
	}
	if raw := firstQuery(q, "congestion"); raw != "" {
		settings["congestion"] = boolString(raw)
	}

	udpMasks := make([]map[string]any, 0, 2)
	headerType := strings.ToLower(firstQuery(q, "headerType", "header"))
	if headerType != "" && headerType != "none" {
		maskType, ok := map[string]string{
			"srtp":         "header-srtp",
			"utp":          "header-utp",
			"wechat-video": "header-wechat",
			"wechat":       "header-wechat",
			"dtls":         "header-dtls",
			"wireguard":    "header-wireguard",
		}[headerType]
		if !ok {
			return nil, nil, errors.New("unsupported xray KCP header type")
		}
		udpMasks = append(udpMasks, map[string]any{"type": maskType})
	}
	if seed := firstQuery(q, "seed", "path"); seed != "" {
		udpMasks = append(udpMasks, map[string]any{
			"type":     "mkcp-aes128gcm",
			"settings": map[string]any{"password": seed},
		})
	} else {
		udpMasks = append(udpMasks, map[string]any{"type": "mkcp-original"})
	}
	return settings, map[string]any{"udp": udpMasks}, nil
}

func normalizeXHTTPDownloadSettings(extra map[string]any, legacyInsecure bool) error {
	raw, exists := extra["downloadSettings"]
	if !exists || raw == nil {
		return nil
	}
	compact, ok := raw.(map[string]any)
	if !ok {
		return errors.New("invalid xhttp download settings")
	}
	address := strings.TrimSpace(stringFromMap(compact, "address"))
	if address == "" {
		address = strings.TrimSpace(stringFromMap(compact, "server"))
	}
	port := intFromAnyValue(compact["port"])
	if address == "" || port <= 0 || port > 65535 {
		return errors.New("xhttp download settings require an address and port")
	}

	xhttp := map[string]any{}
	if existing, ok := compact["xhttpSettings"].(map[string]any); ok {
		for key, value := range existing {
			xhttp[key] = value
		}
	}
	for _, key := range []string{"path", "host"} {
		if value := strings.TrimSpace(stringFromMap(compact, key)); value != "" {
			xhttp[key] = value
		}
	}

	normalized := map[string]any{
		"address":       address,
		"port":          port,
		"network":       "xhttp",
		"xhttpSettings": xhttp,
	}
	security := strings.ToLower(strings.TrimSpace(stringFromMap(compact, "security")))
	serverName := strings.TrimSpace(stringFromMap(compact, "servername"))
	if serverName == "" {
		serverName = strings.TrimSpace(stringFromMap(compact, "serverName"))
	}
	tls, _ := compact["tlsSettings"].(map[string]any)
	if security == "" && (serverName != "" || tls != nil || port == 443) {
		security = "tls"
	}
	if security != "" && security != "none" {
		normalized["security"] = security
	}
	if security == "tls" {
		sanitizedTLS := make(map[string]any, len(tls)+1)
		for key, value := range tls {
			sanitizedTLS[key] = value
		}
		delete(sanitizedTLS, "allowInsecure")
		delete(sanitizedTLS, "allow_insecure")
		if serverName != "" {
			sanitizedTLS["serverName"] = serverName
		}
		if legacyInsecure {
			sanitizedTLS[xrayLegacyInsecureMarker] = true
		}
		normalized["tlsSettings"] = sanitizedTLS
	}
	if security == "reality" {
		if reality, ok := compact["realitySettings"].(map[string]any); ok && len(reality) > 0 {
			normalized["realitySettings"] = reality
		}
	}
	extra["downloadSettings"] = normalized
	return nil
}

func tlsSettings(q url.Values) map[string]any {
	out := map[string]any{}
	if sni := firstQuery(q, "sni", "serverName", "peer"); sni != "" {
		out["serverName"] = sni
	}
	if fp := firstQuery(q, "fp", "fingerprint"); fp != "" {
		out["fingerprint"] = fp
	}
	if alpn := firstQuery(q, "alpn"); alpn != "" {
		out["alpn"] = splitCSV(alpn)
	}
	if pins := splitCSV(firstQuery(q, "pinnedPeerCertSha256", "pinned_peer_cert_sha256")); len(pins) > 0 {
		out["pinnedPeerCertSha256"] = strings.Join(pins, ",")
	}
	if names := splitCSV(firstQuery(q, "verifyPeerCertByName", "verifyPeerCertInNames", "verify_peer_cert_by_name", "verify_peer_cert_in_names")); len(names) > 0 {
		out["verifyPeerCertByName"] = strings.Join(names, ",")
	}
	if boolString(firstQuery(q, "allowInsecure", "allow_insecure")) {
		out[xrayLegacyInsecureMarker] = true
	}
	return out
}

func realitySettings(q url.Values) map[string]any {
	out := tlsSettings(q)
	delete(out, xrayLegacyInsecureMarker)
	if publicKey := firstQuery(q, "pbk", "publicKey"); publicKey != "" {
		out["publicKey"] = publicKey
	}
	if shortID := firstQuery(q, "sid", "shortId", "shortID"); shortID != "" {
		out["shortId"] = shortID
	}
	if spiderX := firstQuery(q, "spx", "spiderX"); spiderX != "" {
		out["spiderX"] = spiderX
	}
	return out
}

func parseShadowsocksShare(raw string) (method, password, host string, port int, err error) {
	withoutScheme := strings.TrimPrefix(strings.TrimSpace(raw), "ss://")
	mainPart := withoutScheme
	if idx := strings.IndexAny(mainPart, "?#"); idx >= 0 {
		mainPart = mainPart[:idx]
	}
	if !strings.Contains(mainPart, "@") {
		decoded, ok := decodeShareBase64(mainPart)
		if !ok {
			return "", "", "", 0, errors.New("invalid shadowsocks payload")
		}
		mainPart = decoded
	}
	u, err := url.Parse("ss://" + mainPart)
	if err != nil {
		return "", "", "", 0, err
	}
	host = u.Hostname()
	port = portFromURL(u)
	userInfo := u.User.String()
	if decoded, ok := decodeShareBase64(userInfo); ok && strings.Contains(decoded, ":") {
		userInfo = decoded
	} else if password, hasPassword := u.User.Password(); hasPassword {
		userInfo = u.User.Username() + ":" + password
	} else if decoded, decodeErr := url.PathUnescape(userInfo); decodeErr == nil {
		userInfo = decoded
	}
	parts := strings.SplitN(userInfo, ":", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || host == "" || port <= 0 {
		return "", "", "", 0, errors.New("shadowsocks node missing method, password, host or port")
	}
	method, password = parts[0], parts[1]
	return method, password, host, port, nil
}

func decodeShareBase64(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", false
	}
	s = strings.ReplaceAll(s, "\n", "")
	s = strings.ReplaceAll(s, "\r", "")
	encodings := []*base64.Encoding{
		base64.RawURLEncoding,
		base64.URLEncoding,
		base64.RawStdEncoding,
		base64.StdEncoding,
	}
	for _, enc := range encodings {
		if decoded, err := enc.DecodeString(s); err == nil && len(decoded) > 0 {
			return string(decoded), true
		}
	}
	if padded := padBase64(s); padded != s {
		for _, enc := range []*base64.Encoding{base64.URLEncoding, base64.StdEncoding} {
			if decoded, err := enc.DecodeString(padded); err == nil && len(decoded) > 0 {
				return string(decoded), true
			}
		}
	}
	return "", false
}

func padBase64(s string) string {
	if rem := len(s) % 4; rem != 0 {
		return s + strings.Repeat("=", 4-rem)
	}
	return s
}

func firstQuery(q url.Values, keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(q.Get(key)); value != "" {
			if decoded, err := url.QueryUnescape(value); err == nil {
				return decoded
			}
			return value
		}
	}
	return ""
}

<<<<<<< HEAD
func intQuery(q url.Values, keys ...string) int {
	raw := firstQuery(q, keys...)
	if raw == "" {
		return 0
	}
	value, _ := strconv.Atoi(raw)
	return value
}

=======
>>>>>>> xray/main
func copyValue(q url.Values, key, value string) {
	value = strings.TrimSpace(value)
	if value != "" {
		q.Set(key, value)
	}
}

func splitCSV(s string) []string {
	parts := strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' })
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func boolString(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "y":
		return true
	default:
		return false
	}
}

func stringFromMap(m map[string]any, key string) string {
	v, ok := m[key]
	if !ok || v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case json.Number:
		return t.String()
	default:
		return strings.TrimSpace(fmt.Sprintf("%v", t))
	}
}

func intFromAnyValue(v any) int {
	switch t := v.(type) {
	case int:
		return t
	case int64:
		return int(t)
	case float64:
		return int(t)
	case json.Number:
		i, _ := t.Int64()
		return int(i)
	case string:
		i, _ := strconv.Atoi(strings.TrimSpace(t))
		return i
	default:
		return 0
	}
}

func portFromURL(u *url.URL) int {
	if u == nil {
		return 0
	}
	port, _ := strconv.Atoi(u.Port())
	return port
}
