package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
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

func TestUserTestProxyReleasesProbeRuntimeWhenProbeFails(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("create sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()

	now := time.Now()
	rows := sqlmock.NewRows([]string{
		"id", "owner_user_id", "is_public", "kind", "name", "protocol", "host", "port",
		"username", "password", "has_auth", "status", "expires_at", "fallback_mode",
		"backup_proxy_id", "expiry_warn_days", "extra", "created_at", "updated_at",
	}).AddRow(
		int64(81), int64(99), false, "xray", "owned-node", "hysteria2", "", 0,
		"", "", false, StatusActive, nil, FallbackModeNone,
		nil, 7, `{"raw":"hy2://node"}`, now, now,
	)
	mock.ExpectQuery(`(?s)FROM proxies p.*WHERE p\.id = \$1.*p\.owner_user_id = \$2`).
		WithArgs(int64(81), int64(99)).
		WillReturnRows(rows)

	resolver := &connectionProbeResolverStub{limit: 16}
	svc := NewUserResourceService(db, nil, nil, nil)
	svc.SetProxyProbeResolver(resolver)
	svc.SetProxyObservabilityServices(&connectionProbeProberStub{err: errors.New("probe failed")}, nil)

	result, err := svc.TestProxy(context.Background(), 99, 81)
	if err != nil {
		t.Fatalf("TestProxy returned error: %v", err)
	}
	if result["success"] != false {
		t.Fatalf("expected failed probe result, got %#v", result)
	}
	active, maxActive, resolveCalls, cleanupCalls := resolver.snapshot()
	if active != 0 || maxActive != 1 || resolveCalls != 1 || cleanupCalls != 1 {
		t.Fatalf("probe runtime lifecycle mismatch: active=%d max=%d resolves=%d cleanups=%d", active, maxActive, resolveCalls, cleanupCalls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet SQL expectations: %v", err)
	}
}
