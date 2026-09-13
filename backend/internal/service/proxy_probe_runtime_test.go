package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

type proxyProbeRuntimeStub struct {
	mu         sync.Mutex
	proxyIDs   []int64
	stopCounts map[int64]int
	err        error
	mutate     bool
}

func (s *proxyProbeRuntimeStub) ProxyURL(_ context.Context, proxy *Proxy) (string, error) {
	s.mu.Lock()
	s.proxyIDs = append(s.proxyIDs, proxy.ID)
	if s.mutate {
		proxy.Extra["raw"] = "changed"
		if nested, ok := proxy.Extra["nested"].(map[string]any); ok {
			nested["value"] = "changed"
		}
	}
	err := s.err
	s.mu.Unlock()
	if err != nil {
		return "", err
	}
	return "socks5h://127.0.0.1:1080", nil
}

func (s *proxyProbeRuntimeStub) Stop(proxyID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopCounts == nil {
		s.stopCounts = make(map[int64]int)
	}
	s.stopCounts[proxyID]++
	return nil
}

func (s *proxyProbeRuntimeStub) snapshot() ([]int64, map[int64]int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := append([]int64(nil), s.proxyIDs...)
	stops := make(map[int64]int, len(s.stopCounts))
	for id, count := range s.stopCounts {
		stops[id] = count
	}
	return ids, stops
}

func TestProxyProbeRuntimeResolverRoutesProtocols(t *testing.T) {
	xray := &proxyProbeRuntimeStub{}
	singBox := &proxyProbeRuntimeStub{}
	resolver := NewProxyProbeRuntimeResolver(xray, singBox)

	standardURL, cleanup, err := resolver.Resolve(context.Background(), &Proxy{
		Kind: "standard", Protocol: "socks5", Host: "127.0.0.1", Port: 1080,
	})
	if err != nil {
		t.Fatalf("resolve standard proxy: %v", err)
	}
	cleanup()
	if standardURL != "socks5://127.0.0.1:1080" {
		t.Fatalf("unexpected standard proxy URL: %s", standardURL)
	}

	_, xrayCleanup, err := resolver.Resolve(context.Background(), &Proxy{
		ID: 1, Kind: "xray", Protocol: "vless", Extra: map[string]any{"raw": "vless://node"},
	})
	if err != nil {
		t.Fatalf("resolve xray protocol: %v", err)
	}
	xrayCleanup()

	_, singBoxCleanup, err := resolver.Resolve(context.Background(), &Proxy{
		ID: 2, Kind: "xray", Protocol: "hysteria2", Extra: map[string]any{"raw": "hy2://node"},
	})
	if err != nil {
		t.Fatalf("resolve sing-box protocol: %v", err)
	}
	singBoxCleanup()

	_, shadowsocksCleanup, err := resolver.Resolve(context.Background(), &Proxy{
		ID: 3, Kind: "xray", Protocol: "ss", Extra: map[string]any{"raw": "ss://node"},
	})
	if err != nil {
		t.Fatalf("resolve shadowsocks protocol: %v", err)
	}
	shadowsocksCleanup()

	xrayIDs, _ := xray.snapshot()
	singBoxIDs, _ := singBox.snapshot()
	if len(xrayIDs) != 1 || len(singBoxIDs) != 2 {
		t.Fatalf("unexpected runtime routing: xray=%v sing-box=%v", xrayIDs, singBoxIDs)
	}
}

func TestProxyProbeRuntimeResolverUsesCloneAndCleansUpOnce(t *testing.T) {
	xray := &proxyProbeRuntimeStub{mutate: true}
	resolver := NewProxyProbeRuntimeResolver(xray, &proxyProbeRuntimeStub{})
	ownerID := int64(77)
	proxy := &Proxy{
		ID:          42,
		OwnerUserID: &ownerID,
		Kind:        "xray",
		Protocol:    "vless",
		Extra: map[string]any{
			"raw":    "vless://original",
			"nested": map[string]any{"value": "original"},
		},
	}

	_, cleanup, err := resolver.Resolve(context.Background(), proxy)
	if err != nil {
		t.Fatalf("resolve proxy: %v", err)
	}
	cleanup()
	cleanup()

	ids, stops := xray.snapshot()
	if len(ids) != 1 || ids[0] <= firstProxyProbeRuntimeID || ids[0] == proxy.ID {
		t.Fatalf("resolver did not assign a temporary high ID: original=%d received=%v", proxy.ID, ids)
	}
	if stops[ids[0]] != 1 {
		t.Fatalf("cleanup executed %d times, want 1", stops[ids[0]])
	}
	if proxy.ID != 42 || proxy.Extra["raw"] != "vless://original" {
		t.Fatalf("resolver mutated the source proxy: %#v", proxy)
	}
	nested, ok := proxy.Extra["nested"].(map[string]any)
	if !ok {
		t.Fatalf("resolver returned invalid nested proxy data: %#v", proxy.Extra["nested"])
	}
	if nested["value"] != "original" {
		t.Fatalf("resolver shallow-copied nested proxy data: %#v", nested)
	}
}

func TestProxyProbeRuntimeResolverConcurrentCallsUseDistinctIDs(t *testing.T) {
	xray := &proxyProbeRuntimeStub{}
	resolver := NewProxyProbeRuntimeResolver(xray, &proxyProbeRuntimeStub{})
	proxy := &Proxy{ID: 9, Kind: "xray", Protocol: "vmess", Extra: map[string]any{"raw": "vmess://node"}}

	const callers = 32
	start := make(chan struct{})
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, cleanup, err := resolver.Resolve(context.Background(), proxy)
			if err != nil {
				errs <- err
				return
			}
			cleanup()
			cleanup()
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent resolve failed: %v", err)
	}

	ids, stops := xray.snapshot()
	if len(ids) != callers {
		t.Fatalf("received %d probe calls, want %d", len(ids), callers)
	}
	unique := make(map[int64]struct{}, callers)
	for _, id := range ids {
		if id <= firstProxyProbeRuntimeID {
			t.Fatalf("temporary ID is not in the reserved high range: %d", id)
		}
		if _, exists := unique[id]; exists {
			t.Fatalf("temporary ID was reused concurrently: %d", id)
		}
		unique[id] = struct{}{}
		if stops[id] != 1 {
			t.Fatalf("probe %d stopped %d times, want 1", id, stops[id])
		}
	}
	if proxy.ID != 9 {
		t.Fatalf("concurrent probes mutated source ID: %d", proxy.ID)
	}
}

func TestProxyProbeRuntimeResolverCleansUpStartFailure(t *testing.T) {
	xray := &proxyProbeRuntimeStub{err: errors.New("runtime start failed")}
	resolver := NewProxyProbeRuntimeResolver(xray, &proxyProbeRuntimeStub{})

	_, cleanup, err := resolver.Resolve(context.Background(), &Proxy{
		ID: 5, Kind: "xray", Protocol: "trojan", Extra: map[string]any{"raw": "trojan://node"},
	})
	if err == nil || err.Error() != "xray probe runtime failed: runtime start failed" {
		t.Fatalf("unexpected resolve error: %v", err)
	}
	cleanup()

	ids, stops := xray.snapshot()
	if len(ids) != 1 || stops[ids[0]] != 1 {
		t.Fatalf("failed runtime cleanup mismatch: ids=%v stops=%v", ids, stops)
	}
}

func TestProxyProbeRuntimeResolverRejectsMissingRuntime(t *testing.T) {
	resolver := NewProxyProbeRuntimeResolver(nil, nil)
	_, _, err := resolver.Resolve(context.Background(), &Proxy{Kind: "xray", Protocol: "vmess"})
	if err == nil || err.Error() != fmt.Sprintf("%s probe runtime is unavailable", "xray") {
		t.Fatalf("unexpected missing runtime error: %v", err)
	}
}

func TestValidateAdminProxyModeSupportsExtendedProtocols(t *testing.T) {
	tests := map[string]string{
		"hysteria":  "hysteria://secret@203.0.113.5:443?sni=edge.example.com&upmbps=20&downmbps=100",
		"hysteria2": "hy2://secret@203.0.113.10:443?sni=edge.example.com&obfs=salamander&obfs-password=mask",
		"tuic":      "tuic://11111111-1111-1111-1111-111111111111:secret@203.0.113.20:443?sni=edge.example.com",
		"anytls":    "anytls://secret@203.0.113.30:443?sni=edge.example.com",
		"naive":     "naive+https://user:secret@203.0.113.40:443?sni=edge.example.com",
		"wireguard": "wireguard://YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWE%3D@203.0.113.50:51820?publickey=YmJiYmJiYmJiYmJiYmJiYmJiYmJiYmJiYmJiYmJiYmI%3D&address=172.16.0.2%2F32",
	}
	for protocol, raw := range tests {
		t.Run(protocol, func(t *testing.T) {
			if err := validateAdminProxyMode("xray", protocol, map[string]any{"raw": raw}); err != nil {
				t.Fatalf("validate extended admin proxy: %v", err)
			}
		})
	}
}

func TestValidateAdminProxyModeHidesInvalidNodeSecrets(t *testing.T) {
	err := validateAdminProxyMode("xray", "tuic", map[string]any{
		"raw": "tuic://user:super-secret-token@example.com",
	})
	if err == nil {
		t.Fatal("expected malformed TUIC node to be rejected")
	}
	if message := err.Error(); message == "" || strings.Contains(message, "super-secret-token") {
		t.Fatalf("validation error leaked node credentials: %q", message)
	}
}
