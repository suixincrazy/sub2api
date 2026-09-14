//go:build unit

package service

import (
	"context"
	"net/http"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestProxyDisabledNeverResolvesToDirect(t *testing.T) {
	for _, kind := range []string{"standard", "xray"} {
		p := &Proxy{ID: 4, Kind: kind, Protocol: "http", Host: "proxy.example.com", Port: 8080, Status: "disabled"}
		_, err := p.ResolveURL(context.Background())
		require.Error(t, err)
		require.NotEmpty(t, p.URL(), "empty URL would silently select direct egress")
	}
}

func TestProxyConnectionKeyTracksSettingsWithoutRestartingForMetadata(t *testing.T) {
	p := &Proxy{Kind: "xray", Protocol: "trojan", Status: StatusActive, Extra: map[string]any{"raw": "trojan://fixture@proxy.example.com:443?sni=a.example.com"}}
	before := ProxyConnectionKey(p)
	p.Name = "new display name"
	p.Extra["source_id"] = 123
	require.Equal(t, before, ProxyConnectionKey(p))
	p.Extra["raw"] = "trojan://fixture@proxy.example.com:443?sni=b.example.com"
	require.NotEqual(t, before, ProxyConnectionKey(p))
	require.Zero(t, parseProxyRuntimeIdleTTL(""), "a long stream is not an idle process")
}

func TestProxySourceDeleteUsesEmptyArrayAndInvalidatesAllAccounts(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectQuery("UPDATE proxies").WithArgs(nil, "9", "{}").WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(77))
	mock.ExpectQuery("UPDATE accounts").WithArgs("{77}").WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(3).AddRow(4))
	svc := &ProxySourceService{db: db}
	proxies, accounts, err := svc.disableMissingProxySourceNodesWith(context.Background(), db, nil, 9, nil)
	require.NoError(t, err)
	require.Equal(t, []int64{77}, proxies)
	require.Equal(t, []int64{3, 4}, accounts)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestOpenAIWSProxyChangeCannotReusePreviousEgress(t *testing.T) {
	cfg := &config.Config{}
	cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 3
	cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 3
	pool := newOpenAIWSConnPool(cfg)
	defer pool.Close()
	dialer := &openAIWSCountingDialer{}
	pool.setClientDialerForTest(dialer)
	req := openAIWSAcquireRequest{Account: &Account{ID: 501, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}, WSURL: "wss://example.com/v1/responses", Headers: http.Header{}}
	first, err := pool.Acquire(context.Background(), req)
	require.NoError(t, err)
	firstID := first.ConnID()
	first.Release()
	req.ProxyURL = "socks5h://127.0.0.1:19001"
	second, err := pool.Acquire(context.Background(), req)
	require.NoError(t, err)
	require.False(t, second.Reused())
	require.NotEqual(t, firstID, second.ConnID())
	second.Release()
	req.WSURL = "wss://another.example.com/v1/responses"
	third, err := pool.Acquire(context.Background(), req)
	require.NoError(t, err)
	require.False(t, third.Reused())
	third.Release()
	require.Equal(t, 3, dialer.DialCount())
}
