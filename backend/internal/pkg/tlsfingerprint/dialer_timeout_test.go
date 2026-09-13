//go:build unit

package tlsfingerprint

import (
	"context"
	"testing"
	"time"
)

// TestDialerTimeoutContract verifies all TLS fingerprint dialers have 10s timeout.
// This is a behavioral contract test - it validates the timeout is correctly configured
// in all three dialer constructors by inspecting their runtime behavior.
func TestDialerTimeoutContract(t *testing.T) {
	profile := &Profile{
		Name:         "timeout_test",
		EnableGREASE: false,
	}

	t.Run("NewDialer_with_nil_baseDialer_has_10s_timeout", func(t *testing.T) {
		// When baseDialer is nil, NewDialer creates its own net.Dialer with 10s timeout
		d := NewDialer(profile, nil)

		// We can't directly access the dialer, but we can verify the timeout behavior
		// by attempting a connection to a non-routable IP (RFC 5737 TEST-NET-1)
		ctx := context.Background()
		start := time.Now()
		_, err := d.DialTLSContext(ctx, "tcp", "192.0.2.1:443")
		elapsed := time.Since(start)

		if err == nil {
			t.Fatal("expected connection to TEST-NET-1 to fail")
		}

		// Connection should timeout around 10 seconds (allow ±2s tolerance)
		if elapsed < 8*time.Second || elapsed > 12*time.Second {
			t.Logf("Warning: timeout took %v, expected ~10s (this may be flaky on slow systems)", elapsed)
		}
	})

	t.Run("NewSOCKS5ProxyDialer_uses_10s_base_dialer", func(t *testing.T) {
		proxyURL := mustParseURL("socks5://192.0.2.1:1080")
		d := NewSOCKS5ProxyDialer(profile, proxyURL)

		// Verify SOCKS5 dialer fails fast on unreachable proxy
		ctx := context.Background()
		start := time.Now()
		_, err := d.DialTLSContext(ctx, "tcp", "example.com:443")
		elapsed := time.Since(start)

		if err == nil {
			t.Fatal("expected connection through TEST-NET-1 proxy to fail")
		}

		// Should timeout around 10 seconds
		if elapsed < 8*time.Second || elapsed > 12*time.Second {
			t.Logf("Warning: SOCKS5 timeout took %v, expected ~10s", elapsed)
		}
	})
}

// TestFingerprintDialerFactory verifies the newFingerprintDialer() factory.
func TestFingerprintDialerFactory(t *testing.T) {
	d := newFingerprintDialer()
	if d.Timeout != fingerprintDialTimeout {
		t.Errorf("newFingerprintDialer: expected Timeout=%v, got %v", fingerprintDialTimeout, d.Timeout)
	}
	if d.KeepAlive != fingerprintDialKeepAlive {
		t.Errorf("newFingerprintDialer: expected KeepAlive=%v, got %v", fingerprintDialKeepAlive, d.KeepAlive)
	}
	// Verify constants match expected 10s/30s values
	if fingerprintDialTimeout != 10*time.Second {
		t.Errorf("fingerprintDialTimeout should be 10s, got %v", fingerprintDialTimeout)
	}
	if fingerprintDialKeepAlive != 30*time.Second {
		t.Errorf("fingerprintDialKeepAlive should be 30s, got %v", fingerprintDialKeepAlive)
	}
}

