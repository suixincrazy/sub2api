package service

import (
	"context"
	"errors"
	"sync"
	"testing"
)

type connectionProbeProxyRepoStub struct {
	ProxyRepository
	proxy *Proxy
}

func (s *connectionProbeProxyRepoStub) GetByID(context.Context, int64) (*Proxy, error) {
	return s.proxy, nil
}

type connectionProbeResolverStub struct {
	mu           sync.Mutex
	active       int
	maxActive    int
	resolveCalls int
	cleanupCalls int
	limit        int
}

func (s *connectionProbeResolverStub) Resolve(context.Context, *Proxy) (string, func(), error) {
	s.mu.Lock()
	if s.limit > 0 && s.active >= s.limit {
		s.mu.Unlock()
		return "", func() {}, errors.New("sing-box runtime per-user instance limit reached (16)")
	}
	s.active++
	s.resolveCalls++
	if s.active > s.maxActive {
		s.maxActive = s.active
	}
	s.mu.Unlock()

	var once sync.Once
	cleanup := func() {
		once.Do(func() {
			s.mu.Lock()
			s.active--
			s.cleanupCalls++
			s.mu.Unlock()
		})
	}
	return "socks5h://127.0.0.1:1080", cleanup, nil
}

func (s *connectionProbeResolverStub) snapshot() (active, maxActive, resolveCalls, cleanupCalls int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active, s.maxActive, s.resolveCalls, s.cleanupCalls
}

type connectionProbeProberStub struct {
	err error
}

func (s *connectionProbeProberStub) ProbeProxy(context.Context, string) (*ProxyExitInfo, int64, error) {
	if s.err != nil {
		return nil, 9, s.err
	}
	return &ProxyExitInfo{Country: "Test"}, 9, nil
}

func TestAdminTestProxyReleasesProbeRuntimeAfterEveryRequest(t *testing.T) {
	resolver := &connectionProbeResolverStub{limit: 16}
	svc := &adminServiceImpl{
		proxyRepo: &connectionProbeProxyRepoStub{proxy: &Proxy{
			ID:       71,
			Kind:     "xray",
			Protocol: "hysteria2",
			Extra:    map[string]any{"raw": "hy2://node"},
		}},
		proxyProber:        &connectionProbeProberStub{},
		proxyProbeResolver: resolver,
	}

	for i := 0; i < 32; i++ {
		result, err := svc.TestProxy(context.Background(), 71)
		if err != nil {
			t.Fatalf("test proxy request %d returned error: %v", i+1, err)
		}
		if result == nil || !result.Success {
			t.Fatalf("test proxy request %d failed: %#v", i+1, result)
		}
	}

	active, maxActive, resolveCalls, cleanupCalls := resolver.snapshot()
	if active != 0 || maxActive != 1 || resolveCalls != 32 || cleanupCalls != 32 {
		t.Fatalf("probe runtime lifecycle mismatch: active=%d max=%d resolves=%d cleanups=%d", active, maxActive, resolveCalls, cleanupCalls)
	}
}
