package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	defaultSingBoxMaxInstances     = 64
	defaultSingBoxMaxInstancesUser = 16
)

var (
	defaultSingBoxRuntimeOnce sync.Once
	defaultSingBoxRuntime     *SingBoxRuntimeManager
)

func DefaultSingBoxRuntimeManager() *SingBoxRuntimeManager {
	defaultSingBoxRuntimeOnce.Do(func() {
		defaultSingBoxRuntime = NewSingBoxRuntimeManager(
			os.Getenv("SING_BOX_BIN"),
			os.Getenv("SING_BOX_WORK_DIR"),
		)
	})
	return defaultSingBoxRuntime
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

func parseSingBoxLimit(raw string, defaultValue int) int {
	limit, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || limit <= 0 {
		return defaultValue
	}
	if limit > 1024 {
		return 1024
	}
	return limit
}

func (m *SingBoxRuntimeManager) ProxyURL(ctx context.Context, p *Proxy) (string, error) {
	if p == nil {
		return "", errors.New("proxy is nil")
	}
	if !strings.EqualFold(p.Kind, "xray") {
		return p.StandardURL(), nil
	}
	if !requiresSingBoxRuntime(p) {
		return "", errors.New("proxy does not require sing-box runtime")
	}

	raw := xrayRawNode(p)
	spec, err := buildSingBoxRuntimeSpec(raw, p)
	if err != nil {
		return "", err
	}
	sourceHash := singBoxInstanceHash(raw, p, spec)

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return "", errors.New("sing-box runtime manager is closed")
	}
	if inst := m.instances[p.ID]; inst != nil && inst.sourceHash == sourceHash && inst.alive() {
		inst.lastUsed = time.Now()
		proxyURL := localSocksURL(inst.port)
		m.mu.Unlock()
		return proxyURL, nil
	}
	m.mu.Unlock()

	if p.OwnerUserID != nil {
		if err := validateUserSingBoxSpecHosts(ctx, spec); err != nil {
			return "", fmt.Errorf("sing-box endpoint is not public: %w", err)
		}
	}
	fingerprint := singBoxInstanceHash(raw, p, spec)

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return "", errors.New("sing-box runtime manager is closed")
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
	inst.sourceHash = sourceHash

	m.instances[p.ID] = inst
	return localSocksURL(inst.port), nil
}

func (m *SingBoxRuntimeManager) Stop(proxyID int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	inst := m.instances[proxyID]
	if inst == nil {
		return nil
	}
	delete(m.instances, proxyID)
	return inst.stop()
}

func (m *SingBoxRuntimeManager) start(ctx context.Context, proxyID int64, ownerUserID *int64, hash string, spec *singBoxRuntimeSpec, userOwned bool) (*singBoxRuntimeInstance, error) {
	bin, err := m.resolveBinary()
	if err != nil {
		return nil, err
	}
	port, err := reserveLocalPort()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(m.workDir, 0o700); err != nil {
		return nil, err
	}
	config := buildSingBoxRuntimeConfig(port, spec, userOwned)
	rawConfig, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return nil, err
	}
	prefix := fmt.Sprintf("proxy-%d-%s", proxyID, hash[:12])
	configPath := filepath.Join(m.workDir, prefix+".json")
	logPath := filepath.Join(m.workDir, prefix+".log")
	if err := os.WriteFile(configPath, rawConfig, 0o600); err != nil {
		return nil, err
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		_ = os.Remove(configPath)
		return nil, err
	}
	cmd := exec.CommandContext(context.Background(), bin, "run", "-c", configPath)
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
		proxyID:     proxyID,
		ownerUserID: cloneSingBoxOwnerID(ownerUserID),
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
		return nil, fmt.Errorf("sing-box runtime did not become ready: %w", err)
	}
	return inst, nil
}

func (m *SingBoxRuntimeManager) pruneInactiveLocked(now time.Time) {
	if m.idleTTL <= 0 {
		return
	}
	for id, inst := range m.instances {
		if inst != nil && now.Sub(inst.lastUsed) > m.idleTTL && inst.alive() {
			delete(m.instances, id)
			_ = inst.stop()
		}
	}
}

func (inst *singBoxRuntimeInstance) alive() bool {
	if inst == nil || inst.cmd == nil || inst.cmd.Process == nil {
		return false
	}
	select {
	case <-inst.done:
		return false
	default:
		return true
	}
}

func (inst *singBoxRuntimeInstance) stop() error {
	if inst == nil || inst.cmd == nil || inst.cmd.Process == nil {
		return nil
	}
	_ = inst.cmd.Process.Kill()
	select {
	case <-inst.done:
	case <-time.After(3 * time.Second):
	}
	return nil
}

func singBoxInstanceHash(raw string, p *Proxy, spec *singBoxRuntimeSpec) string {
	return fmt.Sprintf("singbox:%d:%s", p.ID, raw)
}

func requiresSingBoxRuntime(p *Proxy) bool {
	if p == nil || !strings.EqualFold(p.Kind, "xray") {
		return false
	}
	protocol := strings.ToLower(strings.TrimSpace(p.Protocol))
	switch protocol {
	case "hysteria", "hysteria2", "tuic", "naive", "ss", "shadowsocks", "wireguard", "anytls":
		return true
	default:
		return false
	}
}

type singBoxRuntimeSpec struct {
	Outbound map[string]any
	Endpoint map[string]any
}

func buildSingBoxRuntimeSpec(raw string, p *Proxy) (*singBoxRuntimeSpec, error) {
	if p == nil {
		return nil, errors.New("proxy is nil")
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("sing-box node is empty")
	}

	protocol := strings.ToLower(strings.TrimSpace(p.Protocol))
	if protocol == "ss" {
		protocol = "shadowsocks"
	}
	if protocol == "shadowsocks" {
		return buildSingBoxShadowsocksSpec(raw, p)
	}

	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parse sing-box node: %w", err)
	}
	host := u.Hostname()
	port := portFromURL(u)
	if port <= 0 {
		port = p.Port
	}
	if host == "" || port <= 0 {
		return nil, errors.New("sing-box node missing server or port")
	}

	outbound := map[string]any{
		"type":        protocol,
		"tag":         fmt.Sprintf("proxy-%d", p.ID),
		"server":      host,
		"server_port": port,
	}

	q := u.Query()

	tls := map[string]any{}
	if sni := q.Get("sni"); sni != "" {
		tls["server_name"] = sni
	}
	if q.Get("insecure") == "1" {
		tls["insecure"] = true
	}
	if alpn := q.Get("alpn"); alpn != "" {
		tls["alpn"] = strings.Split(alpn, ",")
	}
	if fp := q.Get("fp"); fp != "" {
		tls["utls"] = map[string]any{
			"enabled":     true,
			"fingerprint": fp,
		}
	}
	if len(tls) > 0 {
		tls["enabled"] = true
		outbound["tls"] = tls
	}

	switch protocol {
	case "hysteria":
		if u.User != nil && u.User.Username() != "" {
			outbound["auth_str"] = u.User.Username()
		}
		if v := q.Get("upmbps"); v != "" {
			outbound["up_mbps"], _ = strconv.Atoi(v)
		}
		if v := q.Get("downmbps"); v != "" {
			outbound["down_mbps"], _ = strconv.Atoi(v)
		}
		if v := q.Get("obfs"); v != "" {
			outbound["obfs"] = v
		}
		if v := q.Get("recv_window_conn"); v != "" {
			outbound["recv_window_conn"], _ = strconv.ParseUint(v, 10, 64)
		}
		if v := q.Get("recv_window"); v != "" {
			outbound["recv_window"], _ = strconv.ParseUint(v, 10, 64)
		}
		if mport := q.Get("mport"); mport != "" {
			outbound["server_ports"] = strings.Split(mport, ",")
		}
		if v := q.Get("hop_interval"); v != "" {
			outbound["hop_interval"] = v
		}

	case "hysteria2":
		if u.User != nil && u.User.Username() != "" {
			outbound["password"] = u.User.Username()
		}
		if obfsType := q.Get("obfs"); obfsType != "" {
			outbound["obfs"] = map[string]any{
				"type":     obfsType,
				"password": q.Get("obfs-password"),
			}
		}
		if v := q.Get("upmbps"); v != "" {
			outbound["up_mbps"], _ = strconv.Atoi(v)
		}
		if v := q.Get("downmbps"); v != "" {
			outbound["down_mbps"], _ = strconv.Atoi(v)
		}
		if mport := q.Get("mport"); mport != "" {
			outbound["server_ports"] = strings.Split(mport, ",")
		}
		if v := q.Get("hop_interval"); v != "" {
			outbound["hop_interval"] = v
		}

	case "tuic":
		if u.User != nil {
			outbound["uuid"] = u.User.Username()
			if pw, ok := u.User.Password(); ok {
				outbound["password"] = pw
			}
		}
		if v := q.Get("congestion_control"); v != "" {
			outbound["congestion_control"] = v
		}
		if v := q.Get("udp_relay_mode"); v != "" {
			outbound["udp_relay_mode"] = v
		}

	case "anytls":
		if u.User != nil && u.User.Username() != "" {
			outbound["password"] = u.User.Username()
		}

	case "naive":
		scheme := u.Scheme
		if scheme == "naive+quic" {
			outbound["quic"] = true
		}
		if u.User != nil {
			pw, hasPw := u.User.Password()
			if hasPw {
				outbound["username"] = u.User.Username()
				outbound["password"] = pw
			} else {
				outbound["password"] = u.User.Username()
			}
		}

	case "wireguard":
		spec := &singBoxRuntimeSpec{Outbound: outbound}
		if u.User != nil && u.User.Username() != "" {
			outbound["private_key"] = u.User.Username()
		}
		peer := map[string]any{
			"address": host,
			"port":    port,
		}
		if pk := q.Get("publickey"); pk != "" {
			peer["public_key"] = pk
		}
		if psk := q.Get("presharedkey"); psk != "" {
			peer["pre_shared_key"] = psk
		}
		if allowedIPs := q.Get("allowedips"); allowedIPs != "" {
			peer["allowed_ips"] = strings.Split(allowedIPs, ",")
		}
		outbound["peers"] = []map[string]any{peer}
		if address := q.Get("address"); address != "" {
			outbound["address"] = strings.Split(address, ",")
		}
		if mtu := q.Get("mtu"); mtu != "" {
			outbound["mtu"], _ = strconv.Atoi(mtu)
		}
		spec.Endpoint = peer
		return spec, nil

	default:
		return nil, fmt.Errorf("protocol %s does not require sing-box runtime", protocol)
	}

	return &singBoxRuntimeSpec{Outbound: outbound}, nil
}

func validateUserSingBoxSpecHosts(ctx context.Context, spec *singBoxRuntimeSpec) error {
	if spec == nil || spec.Outbound == nil {
		return errors.New("sing-box spec is missing")
	}

	host := strings.TrimSpace(stringFromMap(spec.Outbound, "server"))
	if host == "" {
		return nil
	}

	ips, err := resolveExternalHostIPs(ctx, host)
	if err != nil {
		return fmt.Errorf("sing-box endpoint is not public: %w", err)
	}
	if len(ips) == 0 {
		return fmt.Errorf("sing-box endpoint %q resolved to no addresses", host)
	}
	return nil
}

func (m *SingBoxRuntimeManager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil
	}
	m.closed = true
	var errs []error
	for _, inst := range m.instances {
		if err := inst.stop(); err != nil {
			errs = append(errs, err)
		}
	}
	m.instances = map[int64]*singBoxRuntimeInstance{}
	return errors.Join(errs...)
}

func (m *SingBoxRuntimeManager) resolveBinary() (string, error) {
	if m.bin != "" {
		return m.bin, nil
	}
	bin, err := exec.LookPath("sing-box")
	if err != nil {
		return "", errors.New("sing-box binary not found: set SING_BOX_BIN or install sing-box")
	}
	return bin, nil
}

func cloneSingBoxOwnerID(value *int64) *int64 {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}

func buildSingBoxRuntimeConfig(port int, spec *singBoxRuntimeSpec, blockPrivateDestinations bool) map[string]any {
	config := map[string]any{
		"log": map[string]any{
			"level": "warn",
		},
		"inbounds": []map[string]any{
			{
				"tag":      "sub2api-in",
				"type":     "socks",
				"listen":   "127.0.0.1",
				"listen_port": port,
			},
		},
		"outbounds": []map[string]any{spec.Outbound},
	}
	if blockPrivateDestinations {
		config["outbounds"] = []map[string]any{
			spec.Outbound,
			{
				"tag":  "sub2api-block",
				"type": "block",
			},
		}
		config["route"] = map[string]any{
			"rules": []map[string]any{
				{
					"ip_cidr": []string{
						"0.0.0.0/8",
						"10.0.0.0/8",
						"100.64.0.0/10",
						"127.0.0.0/8",
						"169.254.0.0/16",
						"172.16.0.0/12",
						"192.0.0.0/24",
						"192.0.2.0/24",
						"192.168.0.0/16",
						"198.18.0.0/15",
						"198.51.100.0/24",
						"203.0.113.0/24",
						"224.0.0.0/4",
						"240.0.0.0/4",
						"255.255.255.255/32",
						"::1/128",
						"fc00::/7",
						"fe80::/10",
					},
					"outbound": "sub2api-block",
				},
			},
		}
	}
	return config
}

func canonicalSingBoxProtocol(raw string) string {
	protocol := strings.ToLower(strings.TrimSpace(raw))
	switch protocol {
	case "hysteria", "hysteria2", "tuic", "naive", "anytls", "wireguard":
		return protocol
	default:
		return raw
	}
}

func buildSingBoxShadowsocksSpec(raw string, p *Proxy) (*singBoxRuntimeSpec, error) {
	method, password, host, port, err := parseShadowsocksShare(raw)
	if err != nil {
		return nil, fmt.Errorf("parse shadowsocks URI: %w", err)
	}
	if host == "" || port <= 0 {
		return nil, errors.New("shadowsocks node missing server or port")
	}

	outbound := map[string]any{
		"type":        "shadowsocks",
		"tag":         fmt.Sprintf("proxy-%d", p.ID),
		"server":      host,
		"server_port": port,
		"method":      method,
		"password":    password,
	}

	u, err := url.Parse(raw)
	if err == nil && u != nil {
		if clashPlugin := u.Query().Get("plugin"); clashPlugin != "" {
			plugin, opts := translateClashPluginToSingBox(clashPlugin)
			if plugin != "" {
				outbound["plugin"] = plugin
			}
			if opts != "" {
				outbound["plugin_opts"] = opts
			}
		}
	}

	return &singBoxRuntimeSpec{Outbound: outbound}, nil
}

func translateClashPluginToSingBox(clashPlugin string) (plugin, opts string) {
	parts := strings.Split(clashPlugin, ";")
	if len(parts) == 0 {
		return "", ""
	}

	pluginName := strings.TrimSpace(parts[0])
	if pluginName != "obfs" {
		return "", ""
	}

	var obfsMode, obfsHost string
	for _, part := range parts[1:] {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			continue
		}
		key := strings.TrimSpace(kv[0])
		val := strings.TrimSpace(kv[1])
		switch key {
		case "mode":
			obfsMode = val
		case "host":
			obfsHost = val
		}
	}

	if obfsMode == "" {
		return "", ""
	}

	singBoxOpts := "obfs=" + obfsMode
	if obfsHost != "" {
		singBoxOpts += ";obfs-host=" + obfsHost
	}

	return "obfs-local", singBoxOpts
}
