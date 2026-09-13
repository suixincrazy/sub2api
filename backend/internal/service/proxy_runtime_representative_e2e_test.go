package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	_ "github.com/lib/pq"
	xproxy "golang.org/x/net/proxy"
)

func TestStoredProxyRuntimeRepresentativeE2E(t *testing.T) {
	if !strings.EqualFold(strings.TrimSpace(os.Getenv("PROXY_RUNTIME_DB_E2E")), "true") {
		t.Skip("set PROXY_RUNTIME_DB_E2E=true to test stored proxy runtimes")
	}

	db, err := sql.Open("postgres", storedProxyRuntimeDSN())
	if err != nil {
		t.Fatal("open representative proxy database")
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close representative proxy database: %v", err)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Fatal("connect representative proxy database")
	}

	ids := storedProxyRuntimeIDs(t)
	for _, id := range ids {
		id := id
		t.Run(strconv.FormatInt(id, 10), func(t *testing.T) {
			proxy := loadStoredProxyRuntimeFixture(t, ctx, db, id)
			proxyURL, closeRuntime := startStoredProxyRuntime(t, ctx, proxy)
			defer closeRuntime()

			status := requestThroughStoredProxyRuntime(t, ctx, proxyURL)
			t.Logf("proxy %d (%s) long-lived runtime passed with HTTP %d", id, strings.ToLower(proxy.Protocol), status)
		})
	}
}

func TestStoredProxyRuntimeConfigCheck(t *testing.T) {
	if !strings.EqualFold(strings.TrimSpace(os.Getenv("PROXY_RUNTIME_DB_CONFIG_CHECK")), "true") {
		t.Skip("set PROXY_RUNTIME_DB_CONFIG_CHECK=true to validate stored proxy configs")
	}

	db, err := sql.Open("postgres", storedProxyRuntimeDSN())
	if err != nil {
		t.Fatal("open representative proxy database")
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close representative proxy database: %v", err)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Fatal("connect representative proxy database")
	}

	bin := strings.TrimSpace(os.Getenv("XRAY_BIN"))
	if bin == "" {
		t.Fatal("XRAY_BIN is required")
	}
	for _, id := range storedProxyRuntimeIDs(t) {
		proxy := loadStoredProxyRuntimeFixture(t, ctx, db, id)
		if requiresSingBoxRuntime(proxy) {
			continue
		}
		outbound, err := buildXrayOutbound(xrayRawNode(proxy), proxy)
		if err != nil {
			t.Fatalf("build stored proxy %d config: %v", id, err)
		}
		if proxy.OwnerUserID != nil {
			if err := pinUserOwnedXrayOutbound(ctx, outbound); err != nil {
				t.Fatalf("pin stored proxy %d config: %v", id, err)
			}
			outbound["tag"] = "sub2api-out"
		}
		if err := prepareXrayTLSCompatibility(ctx, outbound); err != nil {
			t.Fatalf("prepare stored proxy %d TLS config: %v", id, err)
		}
		config, err := json.Marshal(buildXrayRuntimeConfig(1080, outbound, proxy.OwnerUserID != nil))
		if err != nil {
			t.Fatalf("marshal stored proxy %d config: %v", id, err)
		}
		path := filepath.Join(t.TempDir(), strconv.FormatInt(id, 10)+".json")
		if err := os.WriteFile(path, config, 0o600); err != nil {
			t.Fatalf("write stored proxy %d config: %v", id, err)
		}
		if output, err := exec.Command(bin, "run", "-test", "-config", path).CombinedOutput(); err != nil { //nolint:gosec // G702: bin 来自测试者自设的环境变量，path 为 t.TempDir() + 数字文件名，非请求输入
			t.Fatalf("xray rejected stored proxy %d config: %v: %s", id, err, strings.TrimSpace(string(output)))
		}
	}
}

func storedProxyRuntimeDSN() string {
	host := strings.TrimSpace(os.Getenv("POSTGRES_HOST"))
	if host == "" {
		host = "postgres"
	}
	port := strings.TrimSpace(os.Getenv("POSTGRES_PORT"))
	if port == "" {
		port = "5432"
	}
	return fmt.Sprintf(
		"host=%s port=%s user=%s password=%s dbname=%s sslmode=disable",
		host,
		port,
		os.Getenv("POSTGRES_USER"),
		os.Getenv("POSTGRES_PASSWORD"),
		os.Getenv("POSTGRES_DB"),
	)
}

func storedProxyRuntimeIDs(t *testing.T) []int64 {
	t.Helper()
	parts := strings.Split(strings.TrimSpace(os.Getenv("PROXY_RUNTIME_E2E_IDS")), ",")
	ids := make([]int64, 0, len(parts))
	for _, part := range parts {
		id, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64)
		if err != nil || id <= 0 {
			continue
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		t.Fatal("PROXY_RUNTIME_E2E_IDS must contain representative proxy IDs")
	}
	return ids
}

func loadStoredProxyRuntimeFixture(t *testing.T, ctx context.Context, db *sql.DB, id int64) *Proxy {
	t.Helper()
	var (
		proxy   Proxy
		ownerID sql.NullInt64
		extra   []byte
	)
	err := db.QueryRowContext(ctx, `
		SELECT id, owner_user_id, kind, protocol, host, port, username, password, status, extra
		FROM proxies
		WHERE id = $1 AND deleted_at IS NULL
	`, id).Scan(
		&proxy.ID,
		&ownerID,
		&proxy.Kind,
		&proxy.Protocol,
		&proxy.Host,
		&proxy.Port,
		&proxy.Username,
		&proxy.Password,
		&proxy.Status,
		&extra,
	)
	if err != nil {
		t.Fatalf("load representative proxy %d", id)
	}
	if ownerID.Valid {
		proxy.OwnerUserID = &ownerID.Int64
	}
	if len(extra) > 0 {
		if err := json.Unmarshal(extra, &proxy.Extra); err != nil {
			t.Fatalf("decode representative proxy %d metadata", id)
		}
	}
	return &proxy
}

func startStoredProxyRuntime(t *testing.T, ctx context.Context, proxy *Proxy) (string, func()) {
	t.Helper()
	if requiresSingBoxRuntime(proxy) {
		manager := NewSingBoxRuntimeManager(os.Getenv("SING_BOX_BIN"), t.TempDir())
		proxyURL, err := manager.ProxyURL(ctx, proxy)
		if err != nil {
			_ = manager.Close()
			t.Fatalf("start long-lived sing-box runtime for proxy %d", proxy.ID)
		}
		secondURL, err := manager.ProxyURL(ctx, proxy)
		if err != nil || secondURL != proxyURL {
			_ = manager.Close()
			t.Fatalf("reuse long-lived sing-box runtime for proxy %d", proxy.ID)
		}
		return proxyURL, func() { _ = manager.Close() }
	}

	manager := NewXrayRuntimeManager(os.Getenv("XRAY_BIN"), t.TempDir())
	proxyURL, err := manager.ProxyURL(ctx, proxy)
	if err != nil {
		_ = manager.Close()
		t.Fatalf("start long-lived xray runtime for proxy %d", proxy.ID)
	}
	secondURL, err := manager.ProxyURL(ctx, proxy)
	if err != nil || secondURL != proxyURL {
		_ = manager.Close()
		t.Fatalf("reuse long-lived xray runtime for proxy %d", proxy.ID)
	}
	return proxyURL, func() { _ = manager.Close() }
}

func requestThroughStoredProxyRuntime(t *testing.T, ctx context.Context, proxyURL string) int {
	t.Helper()
	parsed, err := url.Parse(proxyURL)
	if err != nil || parsed.Host == "" {
		t.Fatal("long-lived runtime returned an invalid local proxy URL")
	}
	dialer, err := xproxy.SOCKS5("tcp", parsed.Host, nil, &net.Dialer{Timeout: 12 * time.Second})
	if err != nil {
		t.Fatal("create local SOCKS dialer")
	}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			return dialer.Dial(network, address)
		},
		TLSHandshakeTimeout: 12 * time.Second,
	}
	defer transport.CloseIdleConnections()

	target := strings.TrimSpace(os.Getenv("PROXY_RUNTIME_E2E_URL"))
	if target == "" {
		target = "https://www.cloudflare.com/cdn-cgi/trace"
	}
	requestCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, target, nil)
	if err != nil {
		t.Fatal("create representative proxy request")
	}
	resp, err := (&http.Client{Transport: transport}).Do(req)
	if err != nil {
		t.Fatal("representative proxy request failed")
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("close representative proxy response: %v", err)
		}
	}()
	if resp.StatusCode < 200 || resp.StatusCode >= 500 {
		t.Fatalf("representative proxy request returned HTTP %d", resp.StatusCode)
	}
	return resp.StatusCode
}
