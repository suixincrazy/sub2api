package tlsfingerprint

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"testing"
	"time"
)

func TestHTTPProxyConnectHonorsCancellation(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	requestRead := make(chan struct{})
	go func() {
		_, _ = http.ReadRequest(bufio.NewReader(server))
		close(requestRead)
	}()
	u, _ := url.Parse("http://proxy.example.com:8080")
	dialer := NewHTTPProxyDialerWithBaseDialer(nil, u, func(context.Context, string, string) (net.Conn, error) { return client, nil })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { _, err := dialer.DialTLSContext(ctx, "tcp", "target.example.com:443"); result <- err }()
	<-requestRead
	cancel()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("expected canceled CONNECT")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("CONNECT ignored context cancellation")
	}
}

func TestSOCKSProxyPropagatesDialContext(t *testing.T) {
	u, _ := url.Parse("socks5://proxy.example.com:1080")
	dialer := NewSOCKS5ProxyDialerWithBaseDialer(nil, u, func(ctx context.Context, _, _ string) (net.Conn, error) {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("caller context was lost")
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := dialer.DialTLSContext(ctx, "tcp", "target.example.com:443")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected canceled context, got %v", err)
	}
}
