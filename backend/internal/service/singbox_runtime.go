package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultSingBoxMaxInstances     = 64
	defaultSingBoxMaxInstancesUser = 16
	maximumSingBoxRuntimeLimit     = 1024
)

var (
	defaultSingBoxRuntimeOnce sync.Once
	defaultSingBoxRuntime     *SingBoxRuntimeManager
)

// DefaultSingBoxRuntimeManager runs protocols that are not implemented by Xray.
func DefaultSingBoxRuntimeManager() *SingBoxRuntimeManager {
	defaultSingBoxRuntimeOnce.Do(func() {
		defaultSingBoxRuntime = NewSingBoxRuntimeManager(os.Getenv("SING_BOX_BIN"), os.Getenv("SING_BOX_WORK_DIR"))
	})
	return defaultSingBoxRuntime
}

type singBoxRuntimeSpec struct {
	Outbound map[string]any
	Endpoint map[string]any
}

type SingBoxRuntimeManager struct {
	mu                  sync.Mutex
	bin                 string
	workDir             string
	instances           map[int64]*singBoxRuntimeInstance
	maxInstances        int
	maxInstancesPerUser int
	idleTTL             time.Duration
	closed              bool
	commandFactory      func(bin, configPath string) *exec.Cmd
}

type singBoxRuntimeInstance struct {
	ownerUserID *int64
	hash        string
	port        int
	cmd         *exec.Cmd
	configPath  string
	logPath     string
	done        chan error
	lastUsed    time.Time
}

func NewSingBoxRuntimeManager(bin, workDir string) *SingBoxRuntimeManager {
	if strings.TrimSpace(workDir) == "" {
		workDir = filepath.Join(os.TempDir(), "sub2api-sing-box")
	}
	maxInstances := parseSingBoxLimit(os.Getenv("SING_BOX_MAX_INSTANCES"), defaultSingBoxMaxInstances)
	maxInstancesPerUser := parseSingBoxLimit(os.Getenv("SING_BOX_MAX_INSTANCES_PER_USER"), defaultSingBoxMaxInstancesUser)
	if maxInstancesPerUser > maxInstances {
		maxInstancesPerUser = maxInstances
	}
	return &SingBoxRuntimeManager{
		bin:                 strings.TrimSpace(bin),
		workDir:             workDir,
		instances:           map[int64]*singBoxRuntimeInstance{},
		maxInstances:        maxInstances,
		maxInstancesPerUser: maxInstancesPerUser,
		idleTTL:             parseProxyRuntimeIdleTTL(os.Getenv("SING_BOX_RUNTIME_IDLE_TTL")),
	}
}

func parseSingBoxLimit(raw string, fallback int) int {
	limit, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || limit <= 0 {
		return fallback
	}
	if limit > maximumSingBoxRuntimeLimit {
		return maximumSingBoxRuntimeLimit
	}
	return limit
}

func requiresSingBoxRuntime(p *Proxy) bool {
	if p == nil || !strings.EqualFold(p.Kind, "xray") {
		return false
	}
	protocol := canonicalSingBoxProtocol(p.Protocol)
	if raw := xrayRawNode(p); raw != "" {
		if u, err := url.Parse(raw); err == nil {
			protocol = canonicalSingBoxProtocol(u.Scheme)
		}
	}
	switch protocol {
	case "ss", "shadowsocks", "hysteria", "hysteria2", "tuic", "anytls", "naive", "wireguard":
		return true
	default:
		return false
	}
}

func canonicalSingBoxProtocol(protocol string) string {
	switch strings.ToLower(strings.TrimSpace(protocol)) {
	case "hysteria":
		return "hysteria"
	case "hy2", "hysteria2":
		return "hysteria2"
	case "tuic":
		return "tuic"
	case "anytls", "any-tls":
		return "anytls"
	case "naive", "naive+https", "naive+quic":
		return "naive"
	case "wg", "wireguard":
		return "wireguard"
	case "ss", "shadowsocks":
		return "shadowsocks"
	default:
		return strings.ToLower(strings.TrimSpace(protocol))
	}
}

func (m *SingBoxRuntimeManager) ProxyURL(ctx context.Context, p *Proxy) (string, error) {
	if p == nil {
		return "", errors.New("proxy is nil")
	}
	spec, err := buildSingBoxRuntimeSpec(xrayRawNode(p), p)
	if err != nil {
		return "", err
	}
	if p.OwnerUserID != nil {
		if err := pinUserOwnedSingBoxSpec(ctx, &spec); err != nil {
			return "", fmt.Errorf("sing-box endpoint is not public: %w", err)
		}
	}
	fingerprint := singBoxInstanceHash(xrayRawNode(p), p, spec)

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return "", errors.New("sing-box runtime manager is closed")
	}
	if inst := m.instances[p.ID]; inst != nil && inst.hash == fingerprint && inst.alive() {
		inst.lastUsed = time.Now()
		return localSocksURL(inst.port), nil
	}
	if old := m.instances[p.ID]; old != nil {
		delete(m.instances, p.ID)
		_ = old.stop()
	}
	m.pruneInactiveLocked(time.Now())
	if len(m.instances) >= m.maxInstances {
		return "", fmt.Errorf("sing-box runtime instance limit reached (%d)", m.maxInstances)
	}
	if p.OwnerUserID != nil {
		ownerInstances := 0
		for _, inst := range m.instances {
			if inst != nil && inst.ownerUserID != nil && *inst.ownerUserID == *p.OwnerUserID && inst.alive() {
				ownerInstances++
			}
		}
		if ownerInstances >= m.maxInstancesPerUser {
			return "", fmt.Errorf("sing-box runtime per-user instance limit reached (%d)", m.maxInstancesPerUser)
		}
	}

	inst, err := m.start(ctx, p.ID, p.OwnerUserID, fingerprint, spec, p.OwnerUserID != nil)
	if err != nil {
		return "", err
	}
	m.instances[p.ID] = inst
	return localSocksURL(inst.port), nil
}

func (m *SingBoxRuntimeManager) pruneInactiveLocked(now time.Time) {
	for id, inst := range m.instances {
		if inst != nil && inst.alive() && !proxyRuntimeIsIdle(inst.lastUsed, now, m.idleTTL) {
			continue
		}
		delete(m.instances, id)
		_ = inst.stop()
	}
}

func (m *SingBoxRuntimeManager) Stop(proxyID int64) error {
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

func (m *SingBoxRuntimeManager) Close() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	instances := make([]*singBoxRuntimeInstance, 0, len(m.instances))
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

func (m *SingBoxRuntimeManager) start(ctx context.Context, proxyID int64, ownerUserID *int64, hash string, spec singBoxRuntimeSpec, blockPrivateDestinations bool) (*singBoxRuntimeInstance, error) {
	bin, err := m.resolveBinary()
	if err != nil {
		return nil, err
	}
	port, err := reserveLocalPort()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(m.workDir, 0o700); err != nil { //nolint:gosec // G703: workDir 仅来自启动环境变量 SING_BOX_WORK_DIR（运维配置）或 os.TempDir()，非请求输入
		return nil, err
	}
	rawConfig, err := json.MarshalIndent(buildSingBoxRuntimeConfig(port, spec, blockPrivateDestinations), "", "  ")
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
	cmd := exec.CommandContext(context.Background(), bin, "run", "-c", configPath) //nolint:gosec // G702: bin 仅来自启动环境变量 SING_BOX_BIN（运维配置）或 exec.LookPath("sing-box")，参数为固定字面量 + 上面自建的 configPath，非请求输入
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
	inst := &singBoxRuntimeInstance{
		ownerUserID: cloneXrayOwnerID(ownerUserID), hash: hash, port: port, cmd: cmd,
		configPath: configPath, logPath: logPath, done: make(chan error, 1), lastUsed: time.Now(),
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
		return nil, fmt.Errorf("sing-box runtime did not become ready: %w", err)
	}
	return inst, nil
}

func (m *SingBoxRuntimeManager) resolveBinary() (string, error) {
	if m.bin != "" {
		return m.bin, nil
	}
	if path, err := exec.LookPath("sing-box"); err == nil {
		m.bin = path
		return path, nil
	}
	return "", errors.New("sing-box binary not found; set SING_BOX_BIN")
}

func (i *singBoxRuntimeInstance) alive() bool {
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

func (i *singBoxRuntimeInstance) stop() error {
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

func singBoxInstanceHash(raw string, p *Proxy, spec singBoxRuntimeSpec) string {
	h := sha256.New()
	_, _ = h.Write([]byte(raw))
	_, _ = h.Write([]byte(canonicalSingBoxProtocol(p.Protocol)))
	if p.OwnerUserID != nil {
		_, _ = h.Write([]byte("owner:" + strconv.FormatInt(*p.OwnerUserID, 10)))
	} else {
		_, _ = h.Write([]byte("owner:system"))
	}
	if encoded, err := json.Marshal(spec); err == nil {
		_, _ = h.Write(encoded)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func buildSingBoxRuntimeConfig(port int, spec singBoxRuntimeSpec, blockPrivateDestinations bool) map[string]any {
	config := map[string]any{
		"log": map[string]any{"level": "warn", "timestamp": true},
		"inbounds": []map[string]any{{
			"type": "socks", "tag": "sub2api-in", "listen": "127.0.0.1", "listen_port": port,
		}},
		"route": map[string]any{"final": "sub2api-out"},
	}
	if spec.Outbound != nil {
		config["outbounds"] = []map[string]any{spec.Outbound}
	}
	if spec.Endpoint != nil {
		config["endpoints"] = []map[string]any{spec.Endpoint}
	}
	if blockPrivateDestinations {
		config["route"] = map[string]any{
			"final": "sub2api-out",
			"rules": []map[string]any{{
				"ip_cidr": append([]string(nil), xrayBlockedDestinationCIDRs...), "action": "reject",
			}},
		}
	}
	return config
}

func buildSingBoxRuntimeSpec(raw string, p *Proxy) (singBoxRuntimeSpec, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return singBoxRuntimeSpec{}, errors.New("missing sing-box node uri")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" {
		return singBoxRuntimeSpec{}, errors.New("invalid sing-box node uri")
	}
	protocol := canonicalSingBoxProtocol(u.Scheme)
	if protocol == "shadowsocks" {
		return buildSingBoxShadowsocksSpec(raw)
	}
	host := strings.TrimSpace(u.Hostname())
	port := portFromURL(u)
	if host == "" || port <= 0 {
		return singBoxRuntimeSpec{}, errors.New("sing-box node missing address or port")
	}
	q := u.Query()
	tag := "sub2api-out"
	switch protocol {
	case "hysteria":
		auth := decodedURLUserInfo(u)
		if auth == "" {
			auth = firstQuery(q, "auth", "auth_str", "password")
		}
		upMbps := intQuery(q, "upmbps", "up_mbps", "up")
		downMbps := intQuery(q, "downmbps", "down_mbps", "down")
		if auth == "" || upMbps <= 0 || downMbps <= 0 {
			return singBoxRuntimeSpec{}, errors.New("hysteria node missing authentication or bandwidth")
		}
		out := map[string]any{
			"type": protocol, "tag": tag, "server": host, "server_port": port,
			"auth_str": auth, "up_mbps": upMbps, "down_mbps": downMbps,
			"tls": singBoxTLSOptions(u, true),
		}
		copyStringQuery(out, q, "obfs", "obfs")
		copyIntQuery(out, q, "recv_window_conn", "recv_window_conn", "recv-window-conn")
		copyIntQuery(out, q, "recv_window", "recv_window", "recv-window")
		applySingBoxPortHopping(out, q)
		return singBoxRuntimeSpec{Outbound: out}, nil
	case "hysteria2":
		password := decodedURLUserInfo(u)
		if password == "" {
			password = firstQuery(q, "auth", "password")
		}
		if password == "" {
			return singBoxRuntimeSpec{}, errors.New("hysteria2 node missing password")
		}
		out := map[string]any{
			"type": protocol, "tag": tag, "server": host, "server_port": port,
			"password": password, "tls": singBoxTLSOptions(u, true),
		}
		copyIntQuery(out, q, "up_mbps", "upmbps", "up")
		copyIntQuery(out, q, "down_mbps", "downmbps", "down")
		applySingBoxPortHopping(out, q)
		if obfsType := firstQuery(q, "obfs"); obfsType != "" && obfsType != "none" {
			obfsPassword := firstQuery(q, "obfs-password", "obfs_password")
			if obfsPassword == "" {
				return singBoxRuntimeSpec{}, errors.New("hysteria2 node missing obfuscation password")
			}
			out["obfs"] = map[string]any{"type": obfsType, "password": obfsPassword}
		}
		return singBoxRuntimeSpec{Outbound: out}, nil
	case "tuic":
		uuid := ""
		password := ""
		if u.User != nil {
			uuid = u.User.Username()
			password, _ = u.User.Password()
		}
		if uuid == "" || password == "" {
			return singBoxRuntimeSpec{}, errors.New("tuic node missing uuid or password")
		}
		out := map[string]any{
			"type": protocol, "tag": tag, "server": host, "server_port": port,
			"uuid": uuid, "password": password, "tls": singBoxTLSOptions(u, true),
		}
		copyStringQuery(out, q, "congestion_control", "congestion_control", "congestion-control")
		copyStringQuery(out, q, "udp_relay_mode", "udp_relay_mode", "udp-relay-mode")
		copyStringQuery(out, q, "heartbeat", "heartbeat")
		if value := firstQuery(q, "zero_rtt_handshake", "zero-rtt-handshake", "reduce-rtt"); value != "" {
			out["zero_rtt_handshake"] = boolString(value)
		}
		return singBoxRuntimeSpec{Outbound: out}, nil
	case "anytls":
		password := decodedURLUserInfo(u)
		if password == "" {
			return singBoxRuntimeSpec{}, errors.New("anytls node missing password")
		}
		out := map[string]any{
			"type": protocol, "tag": tag, "server": host, "server_port": port,
			"password": password, "tls": singBoxTLSOptions(u, true),
		}
		copyStringQuery(out, q, "idle_session_check_interval", "idle_session_check_interval")
		copyStringQuery(out, q, "idle_session_timeout", "idle_session_timeout")
		copyIntQuery(out, q, "min_idle_session", "min_idle_session")
		return singBoxRuntimeSpec{Outbound: out}, nil
	case "naive":
		username := ""
		password := ""
		if u.User != nil {
			username = u.User.Username()
			password, _ = u.User.Password()
			if password == "" {
				password = username
				username = ""
			}
		}
		if password == "" {
			return singBoxRuntimeSpec{}, errors.New("naive node missing password")
		}
		out := map[string]any{
			"type": protocol, "tag": tag, "server": host, "server_port": port,
			"username": username, "password": password, "tls": singBoxTLSOptions(u, true),
			"quic": strings.EqualFold(u.Scheme, "naive+quic"),
		}
		copyIntQuery(out, q, "insecure_concurrency", "insecure-concurrency", "insecure_concurrency")
		copyStringQuery(out, q, "quic_congestion_control", "congestion_control", "quic_congestion_control")
		return singBoxRuntimeSpec{Outbound: out}, nil
	case "wireguard":
		privateKey := decodedURLUserInfo(u)
		publicKey := firstQuery(q, "publickey", "public_key", "peer_public_key")
		addresses := splitCSV(firstQuery(q, "address", "local_address", "ip"))
		if privateKey == "" || publicKey == "" || len(addresses) == 0 {
			return singBoxRuntimeSpec{}, errors.New("wireguard node missing private key, public key or interface address")
		}
		peer := map[string]any{
			"address": host, "port": port, "public_key": publicKey,
			"allowed_ips": []string{"0.0.0.0/0", "::/0"},
		}
		if value := firstQuery(q, "presharedkey", "pre_shared_key"); value != "" {
			peer["pre_shared_key"] = value
		}
		if allowed := splitCSV(firstQuery(q, "allowedips", "allowed_ips")); len(allowed) > 0 {
			peer["allowed_ips"] = allowed
		}
		if reserved := parseReservedBytes(firstQuery(q, "reserved")); len(reserved) > 0 {
			peer["reserved"] = reserved
		}
		endpoint := map[string]any{
			"type": protocol, "tag": tag, "address": addresses, "private_key": privateKey,
			"peers": []map[string]any{peer},
		}
		if mtu := intQuery(q, "mtu"); mtu > 0 {
			endpoint["mtu"] = mtu
		}
		return singBoxRuntimeSpec{Endpoint: endpoint}, nil
	default:
		return singBoxRuntimeSpec{}, fmt.Errorf("unsupported sing-box node protocol %q", u.Scheme)
	}
}

func buildSingBoxShadowsocksSpec(raw string) (singBoxRuntimeSpec, error) {
	method, password, host, port, err := parseShadowsocksShare(raw)
	if err != nil {
		return singBoxRuntimeSpec{}, err
	}
	out := map[string]any{
		"type": "shadowsocks", "tag": "sub2api-out", "server": host, "server_port": port,
		"method": method, "password": password,
	}

	if u, parseErr := url.Parse(raw); parseErr == nil {
		q := u.Query()
		if plugin := firstQuery(q, "plugin"); plugin != "" {
			name, options, _ := strings.Cut(plugin, ";")
			name, options, err = normalizeSingBoxShadowsocksPlugin(name, options)
			if err != nil {
				return singBoxRuntimeSpec{}, err
			}
			if name != "" {
				out["plugin"] = name
				if options = strings.TrimSpace(options); options != "" {
					out["plugin_opts"] = options
				}
			}
		}
		if network := strings.ToLower(firstQuery(q, "network")); network == "tcp" || network == "udp" {
			out["network"] = network
		}
	}

	return singBoxRuntimeSpec{Outbound: out}, nil
}

func normalizeSingBoxShadowsocksPlugin(name, options string) (string, string, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	switch name {
	case "v2ray-plugin":
		return name, strings.TrimSpace(options), nil
	case "obfs", "simple-obfs", "obfs-local":
		// Normalize below.
	default:
		return "", "", errors.New("unsupported shadowsocks plugin")
	}

	// Clash names the simple-obfs plugin "obfs" and uses mode/host keys;
	// sing-box exposes the same plugin as obfs-local with SIP003 keys.
	values := make(map[string]string)
	for _, item := range strings.Split(options, ";") {
		key, value, ok := strings.Cut(item, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key != "" && value != "" {
			values[key] = value
		}
	}
	if mode := values["mode"]; mode != "" && values["obfs"] == "" {
		values["obfs"] = mode
		delete(values, "mode")
	}
	if host := values["host"]; host != "" && values["obfs-host"] == "" {
		values["obfs-host"] = host
		delete(values, "host")
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+values[key])
	}
	return "obfs-local", strings.Join(parts, ";"), nil
}

func applySingBoxPortHopping(out map[string]any, q url.Values) {
	copyStringQuery(out, q, "hop_interval", "hop_interval", "hop-interval")
	if ports := firstQuery(q, "mport", "server_ports", "ports"); ports != "" {
		out["server_ports"] = splitCSV(strings.ReplaceAll(ports, "-", ":"))
		delete(out, "server_port")
	}
}

func decodedURLUserInfo(u *url.URL) string {
	if u == nil || u.User == nil {
		return ""
	}
	decoded, err := url.PathUnescape(u.User.String())
	if err != nil {
		return ""
	}
	return decoded
}

func singBoxTLSOptions(u *url.URL, required bool) map[string]any {
	q := u.Query()
	tls := map[string]any{"enabled": required}
	serverName := firstQuery(q, "sni", "peer", "server_name", "servername")
	if serverName == "" && net.ParseIP(u.Hostname()) == nil {
		serverName = u.Hostname()
	}
	if serverName != "" {
		tls["server_name"] = serverName
	}
	if value := firstQuery(q, "insecure", "allowInsecure", "allow_insecure", "skip-cert-verify"); value != "" {
		tls["insecure"] = boolString(value)
	}
	if alpn := splitCSV(firstQuery(q, "alpn")); len(alpn) > 0 {
		tls["alpn"] = alpn
	}
	if fp := firstQuery(q, "fp", "fingerprint", "client-fingerprint"); fp != "" && fp != "none" {
		tls["utls"] = map[string]any{"enabled": true, "fingerprint": fp}
	}
	return tls
}

func pinUserOwnedSingBoxSpec(ctx context.Context, spec *singBoxRuntimeSpec) error {
	if spec == nil {
		return errors.New("sing-box runtime spec is missing")
	}
	pinned := 0
	if spec.Outbound != nil {
		host := strings.TrimSpace(stringFromMap(spec.Outbound, "server"))
		if host != "" {
			ip, err := resolveSingleExternalIP(ctx, host)
			if err != nil {
				return err
			}
			preserveSingBoxTLSServerName(spec.Outbound, host)
			spec.Outbound["server"] = ip.String()
			pinned++
		}
	}
	if spec.Endpoint != nil {
		for _, peer := range mapSliceFromAny(spec.Endpoint["peers"]) {
			host := strings.TrimSpace(urAsString(peer["address"]))
			if host == "" {
				continue
			}
			ip, err := resolveSingleExternalIP(ctx, host)
			if err != nil {
				return err
			}
			peer["address"] = ip.String()
			pinned++
		}
	}
	if pinned == 0 {
		return errors.New("sing-box node has no server address")
	}
	return nil
}

func resolveSingleExternalIP(ctx context.Context, host string) (net.IP, error) {
	ips, err := resolveExternalHostIPs(ctx, host)
	if err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("endpoint %q resolved to no addresses", host)
	}
	return ips[0], nil
}

func preserveSingBoxTLSServerName(outbound map[string]any, host string) {
	if net.ParseIP(host) != nil {
		return
	}
	tls, _ := outbound["tls"].(map[string]any)
	if tls != nil && strings.TrimSpace(stringFromMap(tls, "server_name")) == "" {
		tls["server_name"] = host
	}
}

func validateUserSingBoxSpecHosts(ctx context.Context, spec singBoxRuntimeSpec) error {
	return pinUserOwnedSingBoxSpec(ctx, &spec)
}

func copyStringQuery(out map[string]any, q url.Values, target string, keys ...string) {
	if value := firstQuery(q, keys...); value != "" {
		out[target] = value
	}
}

func copyIntQuery(out map[string]any, q url.Values, target string, keys ...string) {
	if value := intQuery(q, keys...); value > 0 {
		out[target] = value
	}
}

func intQuery(q url.Values, keys ...string) int {
	value, _ := strconv.Atoi(firstQuery(q, keys...))
	return value
}

func parseReservedBytes(raw string) []int {
	parts := strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == '.' || r == '-' || r == ' ' })
	if len(parts) != 3 {
		return nil
	}
	result := make([]int, 0, 3)
	for _, part := range parts {
		value, err := strconv.Atoi(part)
		if err != nil || value < 0 || value > 255 {
			return nil
		}
		result = append(result, value)
	}
	return result
}
