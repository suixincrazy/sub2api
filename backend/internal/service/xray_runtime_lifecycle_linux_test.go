//go:build linux

package service

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func lifecycleXrayManager(t *testing.T) *XrayRuntimeManager {
	t.Helper()
	bin := os.Getenv("XRAY_BIN")
	if bin == "" {
		var err error
		bin, err = exec.LookPath("xray")
		if err != nil {
			t.Skip("XRAY_BIN is required for real process lifecycle tests")
		}
	}
	manager := NewXrayRuntimeManager(bin, t.TempDir())
	t.Cleanup(func() { require.NoError(t, manager.Close()) })
	return manager
}

func lifecycleXrayProxy(id int64) *Proxy {
	return &Proxy{ID: id, Kind: "xray", Status: StatusActive, Extra: map[string]any{
		"outbound": map[string]any{"protocol": "freedom", "settings": map[string]any{}},
	}}
}

func lifecycleTLSOrigin(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "runtime-ok")
	}))
	t.Cleanup(server.Close)
	return server
}

func lifecycleClient(t *testing.T, proxyURL string, server *httptest.Server) *http.Client {
	t.Helper()
	parsed, err := url.Parse(proxyURL)
	require.NoError(t, err)
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	transport := &http.Transport{
		Proxy: http.ProxyURL(parsed), TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
		DisableKeepAlives: true,
	}
	t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{Transport: transport, Timeout: 2 * time.Second}
}

func lifecycleRequest(t *testing.T, proxyURL string, server *httptest.Server) {
	t.Helper()
	response, err := lifecycleClient(t, proxyURL, server).Get(server.URL)
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.Equal(t, "runtime-ok", string(body))
}

func lifecycleInstance(t *testing.T, manager *XrayRuntimeManager, id int64) *xrayRuntimeInstance {
	t.Helper()
	manager.mu.Lock()
	defer manager.mu.Unlock()
	instance := manager.instances[id]
	require.NotNil(t, instance)
	return instance
}

func TestXrayLifecycleKilledProcessRecovers(t *testing.T) {
	server := lifecycleTLSOrigin(t)
	manager := lifecycleXrayManager(t)
	proxy := lifecycleXrayProxy(801)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for cycle := 0; cycle < 20; cycle++ {
		proxyURL, err := manager.ProxyURL(ctx, proxy)
		require.NoError(t, err)
		lifecycleRequest(t, proxyURL, server)
		old := lifecycleInstance(t, manager, proxy.ID)
		require.NoError(t, old.cmd.Process.Kill())
		select {
		case <-old.done:
		case <-time.After(3 * time.Second):
			t.Fatal("killed runtime did not exit")
		}
		nextURL, err := manager.ProxyURL(ctx, proxy)
		require.NoError(t, err, "restart cycle %d", cycle)
		next := lifecycleInstance(t, manager, proxy.ID)
		require.NotEqual(t, old.cmd.Process.Pid, next.cmd.Process.Pid)
		lifecycleRequest(t, nextURL, server)
		t.Logf("cycle=%d old_pid=%d replacement_pid=%d TLS_request=200", cycle, old.cmd.Process.Pid, next.cmd.Process.Pid)
	}
}

func TestXrayLifecycleConcurrentRestartAfterExit(t *testing.T) {
	server := lifecycleTLSOrigin(t)
	manager := lifecycleXrayManager(t)
	proxy := lifecycleXrayProxy(802)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err := manager.ProxyURL(ctx, proxy)
	require.NoError(t, err)
	old := lifecycleInstance(t, manager, proxy.ID)
	require.NoError(t, old.cmd.Process.Kill())
	select {
	case <-old.done:
	case <-ctx.Done():
		t.Fatal("runtime exit was not observed")
	}
	type result struct {
		url string
		err error
	}
	results := make(chan result, 24)
	var ready sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < cap(results); i++ {
		ready.Add(1)
		go func() {
			ready.Done()
			<-start
			value, err := manager.ProxyURL(ctx, proxy)
			results <- result{value, err}
		}()
	}
	ready.Wait()
	close(start)
	var shared string
	for i := 0; i < cap(results); i++ {
		got := <-results
		require.NoError(t, got.err)
		if shared == "" {
			shared = got.url
		}
		require.Equal(t, shared, got.url)
	}
	lifecycleRequest(t, shared, server)
	require.Len(t, manager.instances, 1)
	t.Logf("concurrent_callers=24 replacement_pid=%d TLS_request=200", lifecycleInstance(t, manager, proxy.ID).cmd.Process.Pid)
}

func TestXrayLifecycleProbeCleanupPreservesAccountRuntime(t *testing.T) {
	server := lifecycleTLSOrigin(t)
	accountManager := lifecycleXrayManager(t)
	probeManager := lifecycleXrayManager(t)
	resolver := NewProxyProbeRuntimeResolver(probeManager, nil)
	proxy := lifecycleXrayProxy(803)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	accountURL, err := accountManager.ProxyURL(ctx, proxy)
	require.NoError(t, err)
	accountPID := lifecycleInstance(t, accountManager, proxy.ID).cmd.Process.Pid
	for i := 0; i < 12; i++ {
		probeURL, cleanup, err := resolver.Resolve(ctx, proxy)
		require.NoError(t, err)
		lifecycleRequest(t, probeURL, server)
		cleanup()
		require.Empty(t, probeManager.instances)
		require.Equal(t, accountPID, lifecycleInstance(t, accountManager, proxy.ID).cmd.Process.Pid)
		lifecycleRequest(t, accountURL, server)
	}
	t.Logf("probe_processes_cleaned=12 account_pid=%d account_TLS_requests=12", accountPID)
}

func TestXrayLifecycleDefaultIdlePolicyPreservesOldRuntime(t *testing.T) {
	t.Setenv("XRAY_RUNTIME_IDLE_TTL", "0")
	server := lifecycleTLSOrigin(t)
	manager := lifecycleXrayManager(t)
	require.Zero(t, manager.idleTTL)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	first := lifecycleXrayProxy(804)
	firstURL, err := manager.ProxyURL(ctx, first)
	require.NoError(t, err)
	instance := lifecycleInstance(t, manager, first.ID)
	manager.mu.Lock()
	instance.lastUsed = time.Now().Add(-48 * time.Hour)
	manager.mu.Unlock()
	_, err = manager.ProxyURL(ctx, lifecycleXrayProxy(805))
	require.NoError(t, err)
	require.True(t, instance.alive())
	lifecycleRequest(t, firstURL, server)
	t.Log("idle_TTL=0 simulated_time_since_last_use=48h original_runtime_survived=true")
}

func TestXrayLifecycleStoppedProcessDiffersFromExitedProcess(t *testing.T) {
	server := lifecycleTLSOrigin(t)
	manager := lifecycleXrayManager(t)
	probeManager := lifecycleXrayManager(t)
	proxy := lifecycleXrayProxy(806)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	accountURL, err := manager.ProxyURL(ctx, proxy)
	require.NoError(t, err)
	lifecycleRequest(t, accountURL, server)
	instance := lifecycleInstance(t, manager, proxy.ID)
	require.NoError(t, instance.cmd.Process.Signal(syscall.SIGSTOP))
	t.Cleanup(func() { _ = instance.cmd.Process.Signal(syscall.SIGCONT) })
	stillURL, err := manager.ProxyURL(ctx, proxy)
	require.NoError(t, err)
	require.Equal(t, accountURL, stillURL)
	started := time.Now()
	response, err := lifecycleClient(t, stillURL, server).Get(server.URL)
	if response != nil {
		_ = response.Body.Close()
	}
	require.Error(t, err)
	require.True(t, instance.alive())
	t.Logf("SIGSTOP pid=%d process_alive=true request_error=%v elapsed=%s", instance.cmd.Process.Pid, err, time.Since(started))
	resolver := NewProxyProbeRuntimeResolver(probeManager, nil)
	probeURL, cleanup, err := resolver.Resolve(ctx, proxy)
	require.NoError(t, err)
	defer cleanup()
	lifecycleRequest(t, probeURL, server)
	require.NoError(t, instance.cmd.Process.Signal(syscall.SIGCONT))
	lifecycleRequest(t, accountURL, server)
	t.Log("independent_proxy_test=200 SIGCONT_original_runtime=200")
}

func TestXrayLifecycleConcurrentStopStartKeepsReplacementFiles(t *testing.T) {
	server := lifecycleTLSOrigin(t)
	manager := lifecycleXrayManager(t)
	proxy := lifecycleXrayProxy(807)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	for cycle := 0; cycle < 100; cycle++ {
		_, err := manager.ProxyURL(ctx, proxy)
		require.NoError(t, err)
		stopped := make(chan error, 1)
		go func() { stopped <- manager.Stop(proxy.ID) }()
		_, _ = manager.ProxyURL(ctx, proxy)
		require.NoError(t, <-stopped)
		currentURL, err := manager.ProxyURL(ctx, proxy)
		require.NoError(t, err, "stop/start cycle %d", cycle)
		current := lifecycleInstance(t, manager, proxy.ID)
		raw, err := os.ReadFile(current.configPath)
		require.NoError(t, err, "replacement configuration removed in cycle %d", cycle)
		require.True(t, strings.Contains(string(raw), "freedom"))
		lifecycleRequest(t, currentURL, server)
	}
	t.Log("concurrent_stop_start_cycles=100 replacement_TLS_requests=100")
}

func TestXrayLifecycleReplacementSurvivesOldCleanup(t *testing.T) {
	server := lifecycleTLSOrigin(t)
	manager := lifecycleXrayManager(t)
	proxy := lifecycleXrayProxy(808)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := manager.ProxyURL(ctx, proxy)
	require.NoError(t, err)
	old := lifecycleInstance(t, manager, proxy.ID)
	t.Cleanup(func() { _ = old.stop() })
	gate := filepath.Join(t.TempDir(), "start")
	created := make(chan string, 1)
	manager.commandFactory = func(bin, configPath string) *exec.Cmd {
		created <- configPath
		//nolint:gosec // G702: fixed gate script uses the trusted test XRAY_BIN and private temp paths.
		return exec.Command("sh", "-c", `while [ ! -f "$1" ]; do sleep 0.01; done; exec "$2" run -config "$3"`, "xray-delayed-start", gate, bin, configPath)
	}
	// Stop detaches under the lock, then reaps outside it. Reproduce a second
	// request starting its replacement before the detached process is reaped.
	manager.mu.Lock()
	delete(manager.instances, proxy.ID)
	manager.mu.Unlock()
	type result struct {
		url string
		err error
	}
	started := make(chan result, 1)
	go func() {
		value, err := manager.ProxyURL(ctx, proxy)
		started <- result{value, err}
	}()
	var replacementConfig string
	select {
	case replacementConfig = <-created:
	case <-ctx.Done():
		t.Fatal("replacement start was not reached")
	}
	require.FileExists(t, replacementConfig)
	require.NoError(t, old.stop())
	_, fileErr := os.Stat(replacementConfig)
	require.NoError(t, os.WriteFile(gate, []byte("start"), 0o600))
	got := <-started
	t.Logf("old_cleanup_removed_replacement_config=%t replacement_start_error=%v", os.IsNotExist(fileErr), got.err)
	require.NoError(t, fileErr, "old process cleanup removed the replacement configuration")
	require.NoError(t, got.err)
	lifecycleRequest(t, got.url, server)
}

func TestXrayLifecycleStoredNodesAfterIdleAndExit(t *testing.T) {
	fixturePath := os.Getenv("XRAY_LIFECYCLE_FIXTURES")
	if fixturePath == "" {
		t.Skip("XRAY_LIFECYCLE_FIXTURES is required for stored node reproduction")
	}
	raw, err := os.ReadFile(fixturePath)
	require.NoError(t, err)
	var proxies []*Proxy
	require.NoError(t, json.Unmarshal(raw, &proxies))
	require.Len(t, proxies, 2)
	manager := lifecycleXrayManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 280*time.Second)
	defer cancel()
	check := func(proxy *Proxy, phase string) {
		t.Helper()
		proxyURL, err := manager.ProxyURL(ctx, proxy)
		require.NoError(t, err, "proxy %d phase %s startup", proxy.ID, phase)
		parsed, err := url.Parse(proxyURL)
		require.NoError(t, err)
		transport := &http.Transport{Proxy: http.ProxyURL(parsed), DisableKeepAlives: true, TLSHandshakeTimeout: 10 * time.Second}
		defer transport.CloseIdleConnections()
		client := &http.Client{Transport: transport, Timeout: 10 * time.Second}
		success := false
		for _, target := range []string{"http://ip-api.com/json/?lang=zh-CN", "http://api64.ipify.org?format=json"} {
			before := time.Now()
			response, err := client.Get(target)
			if err != nil {
				t.Logf("proxy=%d phase=%s target=%s error=%v", proxy.ID, phase, target, err)
				continue
			}
			body, readErr := io.ReadAll(io.LimitReader(response.Body, 8192))
			_ = response.Body.Close()
			var fields map[string]any
			decodeErr := json.Unmarshal(body, &fields)
			address, _ := fields["query"].(string)
			if address == "" {
				address, _ = fields["ip"].(string)
			}
			success = readErr == nil && decodeErr == nil && response.StatusCode == 200 && net.ParseIP(address) != nil
			t.Logf("proxy=%d phase=%s pid=%d target=%s status=%d valid_ip=%t elapsed=%s", proxy.ID, phase, lifecycleInstance(t, manager, proxy.ID).cmd.Process.Pid, target, response.StatusCode, success, time.Since(before))
			if success {
				break
			}
		}
		require.True(t, success, "proxy %d phase %s failed both production probe targets", proxy.ID, phase)
		response, err := client.Get("https://api.anthropic.com/v1/models")
		require.NoError(t, err, "proxy %d phase %s TLS", proxy.ID, phase)
		_ = response.Body.Close()
		require.GreaterOrEqual(t, response.StatusCode, 200)
		require.Less(t, response.StatusCode, 500)
		t.Logf("proxy=%d phase=%s TLS_status=%d", proxy.ID, phase, response.StatusCode)
	}
	originalPIDs := make(map[int64]int, len(proxies))
	for _, proxy := range proxies {
		check(proxy, "cold")
		originalPIDs[proxy.ID] = lifecycleInstance(t, manager, proxy.ID).cmd.Process.Pid
	}
	t.Log("real_idle_seconds=120 no_requests_during_idle=true")
	time.Sleep(120 * time.Second)
	for _, proxy := range proxies {
		check(proxy, "after_idle")
		old := lifecycleInstance(t, manager, proxy.ID)
		require.Equal(t, originalPIDs[proxy.ID], old.cmd.Process.Pid, "runtime restarted during idle")
		require.NoError(t, old.cmd.Process.Kill())
		select {
		case <-old.done:
		case <-ctx.Done():
			t.Fatal("stored node process did not exit")
		}
		check(proxy, "after_kill")
		require.NotEqual(t, old.cmd.Process.Pid, lifecycleInstance(t, manager, proxy.ID).cmd.Process.Pid)
	}
}
