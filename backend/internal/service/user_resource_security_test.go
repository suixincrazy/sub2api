package service

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/lib/pq"
)

func TestRedactPublicProxyHidesCredentialsAndXraySecrets(t *testing.T) {
	item := map[string]any{
		"id":              int64(10),
		"owner_user_id":   int64(77),
		"is_public":       true,
		"host":            "proxy-owner.example.com",
		"port":            443,
		"ip_address":      "203.0.113.20",
		"latency_message": "dial tcp 203.0.113.20:443: refused",
		"has_auth":        true,
		"username":        "proxy-user",
		"password":        "proxy-pass",
		"extra": map[string]any{
			"raw":           "vless://uuid@example.com:443?pbk=secret",
			"alt":           "hy2://secret@example.com:443",
			"xray_outbound": map[string]any{"settings": "secret"},
			"share_link":    "trojan://secret@example.com:443",
			"transport": map[string]any{
				"password": "nested-secret",
				"users":    []any{map[string]any{"private_key": "nested-key"}},
			},
			"region": "us",
		},
	}

	redactPublicProxy(item, 99)

	for _, key := range []string{
		"owner_user_id", "host", "port", "username", "password", "has_auth",
		"backup_proxy_id", "ip_address", "latency_message", "extra",
	} {
		if _, exists := item[key]; exists {
			t.Fatalf("public proxy private field %q was returned: %#v", key, item)
		}
	}
	if item["is_owned"] != false || item["details_hidden"] != true {
		t.Fatalf("public proxy ownership metadata was not redacted: %#v", item)
	}
	for _, key := range []string{"id", "is_public", "kind", "name", "protocol", "status"} {
		if _, exists := item[key]; !exists && key != "kind" && key != "name" && key != "protocol" && key != "status" {
			t.Fatalf("public proxy metadata field %q is missing: %#v", key, item)
		}
	}
}

func TestRedactProxyExtraValueCoversModernShareSchemes(t *testing.T) {
	for _, value := range []string{
		"hysteria://secret@example.com:443",
		"hysteria2://secret@example.com:443",
		"hy2://secret@example.com:443",
		"tuic://uuid:secret@example.com:443",
		"anytls://secret@example.com:443",
		"naive://user:secret@example.com:443",
		"wireguard://private-key@example.com:51820",
	} {
		clean, redacted := redactProxyExtraValue(value)
		if !redacted || clean != "" {
			t.Fatalf("share URI was not redacted: %q -> %#v", value, clean)
		}
	}
}

func TestRedactPublicProxyKeepsOwnerPrivateFieldsForOwner(t *testing.T) {
	item := map[string]any{
		"id":            int64(10),
		"owner_user_id": int64(99),
		"is_public":     true,
		"username":      "proxy-user",
		"password":      "proxy-pass",
		"extra":         map[string]any{"raw": "vless://secret"},
	}

	redactPublicProxy(item, 99)

	if item["username"] != "proxy-user" || item["password"] != "proxy-pass" {
		t.Fatalf("owner proxy credentials should not be redacted: %#v", item)
	}
	if item["is_owned"] != true {
		t.Fatalf("owner marker missing from owned proxy: %#v", item)
	}
	extra, ok := item["extra"].(map[string]any)
	if !ok {
		t.Fatalf("unexpected owner proxy metadata: %#v", item["extra"])
	}
	if extra["raw"] != "vless://secret" {
		t.Fatalf("owner xray raw node should not be redacted: %#v", extra)
	}
}

func TestRedactProxyForUserResponseHidesOwnedCredentials(t *testing.T) {
	item := map[string]any{
		"owner_user_id": int64(99),
		"username":      "proxy-user",
		"password":      "proxy-pass",
		"extra": map[string]any{
			"raw":    "vless://secret@example.com:443",
			"region": "us",
		},
	}

	RedactProxyForUserResponse(item)

	if item["username"] != "" || item["password"] != "" {
		t.Fatalf("owned proxy response leaked credentials: %#v", item)
	}
	extra, ok := item["extra"].(map[string]any)
	if !ok {
		t.Fatalf("unexpected owned proxy metadata: %#v", item["extra"])
	}
	if extra["raw"] != "" || extra["redacted"] != true {
		t.Fatalf("owned proxy response leaked xray node: %#v", extra)
	}
	if extra["region"] != "us" {
		t.Fatalf("non-sensitive proxy metadata was removed: %#v", extra)
	}
}

func TestRedactProxyForUserResponsePreservesForeignPublicProxyShape(t *testing.T) {
	item := map[string]any{
		"id":             int64(10),
		"name":           "public-proxy",
		"protocol":       "http",
		"status":         StatusActive,
		"is_public":      true,
		"is_owned":       false,
		"details_hidden": true,
	}

	RedactProxyForUserResponse(item)

	for _, key := range []string{"username", "password", "extra", "host", "port", "owner_user_id"} {
		if _, exists := item[key]; exists {
			t.Fatalf("foreign public proxy redaction added private field %q: %#v", key, item)
		}
	}
	if item["details_hidden"] != true || item["is_owned"] != false {
		t.Fatalf("foreign public proxy redaction changed visibility markers: %#v", item)
	}
}

func TestTranslateUserResourceProxyConstraintError(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantReason string
		wantSame   bool
	}{
		{
			name:       "public proxy usage trigger",
			err:        &pq.Error{Code: "23514", Message: "public proxy is still used by another user resource"},
			wantReason: "PROXY_PUBLIC_IN_USE",
		},
		{
			name:       "wrapped public proxy usage trigger",
			err:        fmt.Errorf("update failed: %w", &pq.Error{Code: "23514", Message: "public proxy is still used by another user resource"}),
			wantReason: "PROXY_PUBLIC_IN_USE",
		},
		{
			name:     "different check constraint",
			err:      &pq.Error{Code: "23514", Message: "account ownership check failed"},
			wantSame: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := translateUserResourceProxyConstraintError(tt.err)
			if tt.wantSame && got != tt.err {
				t.Fatalf("different constraint was translated: got %#v, want original %#v", got, tt.err)
			}
			if tt.wantReason != "" {
				if !infraerrors.IsConflict(got) || infraerrors.Reason(got) != tt.wantReason {
					t.Fatalf("unexpected translated error: %#v", got)
				}
				if strings.Contains(infraerrors.Message(got), "23514") || strings.Contains(infraerrors.Message(got), "another user resource") {
					t.Fatalf("translated error exposed database details: %#v", got)
				}
			}
		})
	}
}

func TestRedactProxyImportResultForUserResponseHidesAllNodeSecrets(t *testing.T) {
	result := &ProxyImportResult{
		Created: []map[string]any{{
			"username": "created-user",
			"password": "created-pass",
			"extra":    map[string]any{"raw": "vless://created-secret"},
		}},
		Updated: []map[string]any{{
			"username": "updated-user",
			"password": "updated-pass",
			"extra":    map[string]any{"raw": "tuic://updated-secret"},
		}},
		Errors: []string{
			"entry 1: vless://11111111-2222-3333-4444-555555555555@node-secret.example.com:443?token=node-token-secret",
			"entry 2: proxy-user:proxy-pass@proxy-secret.example.com:1080 Authorization: Bearer bearer-secret-value-12345678901234567890",
		},
	}

	RedactProxyImportResultForUserResponse(result)

	for _, item := range append(result.Created, result.Updated...) {
		if item["username"] != "" || item["password"] != "" {
			t.Fatalf("proxy import response leaked credentials: %#v", item)
		}
		extra, ok := item["extra"].(map[string]any)
		if !ok {
			t.Fatalf("unexpected proxy import metadata: %#v", item["extra"])
		}
		if extra["raw"] != "" || extra["redacted"] != true {
			t.Fatalf("proxy import response leaked node configuration: %#v", extra)
		}
	}
	for _, secret := range []string{
		"11111111-2222-3333-4444-555555555555", "node-secret.example.com", "node-token-secret",
		"proxy-user", "proxy-pass", "proxy-secret.example.com", "bearer-secret-value",
	} {
		if strings.Contains(strings.Join(result.Errors, "\n"), secret) {
			t.Fatalf("proxy import error leaked %q: %#v", secret, result.Errors)
		}
	}
}

func TestRedactProxySourceSyncResultForUserResponseHidesAllNodeSecrets(t *testing.T) {
	result := &ProxySourceSyncResult{
		Created: []map[string]any{{
			"password": "created-pass",
			"extra":    map[string]any{"raw": "hysteria2://created-secret"},
		}},
		Updated: []map[string]any{{
			"password": "updated-pass",
			"extra":    map[string]any{"raw": "anytls://updated-secret"},
		}},
		Errors: []string{
			"subscription_url=https://airport-secret.example.com/sub?token=subscription-token-secret",
			"dial tcp node-secret.example.com:443 with api_key=api-secret-value",
		},
	}

	RedactProxySourceSyncResultForUserResponse(result)

	for _, item := range append(result.Created, result.Updated...) {
		if item["password"] != "" {
			t.Fatalf("proxy source sync response leaked credentials: %#v", item)
		}
		extra, ok := item["extra"].(map[string]any)
		if !ok {
			t.Fatalf("unexpected proxy source metadata: %#v", item["extra"])
		}
		if extra["raw"] != "" || extra["redacted"] != true {
			t.Fatalf("proxy source sync response leaked node configuration: %#v", extra)
		}
	}
	for _, secret := range []string{
		"airport-secret.example.com", "subscription-token-secret", "node-secret.example.com", "api-secret-value",
	} {
		if strings.Contains(strings.Join(result.Errors, "\n"), secret) {
			t.Fatalf("proxy source sync error leaked %q: %#v", secret, result.Errors)
		}
	}
}

func TestProxyImportErrorRedactionPreservesEntryWithoutSecrets(t *testing.T) {
	raw := "entry 9: failed vless://aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee@hidden.example.com:443?token=opaque-secret Authorization: Bearer bearer-secret-value"
	got := redactProxyImportErrorText(raw)
	if !strings.Contains(got, "entry 9") || !strings.Contains(got, "failed") {
		t.Fatalf("redaction removed useful error context: %q", got)
	}
	for _, secret := range []string{
		"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "hidden.example.com", "opaque-secret", "bearer-secret-value",
	} {
		if strings.Contains(got, secret) {
			t.Fatalf("redacted import error leaked %q: %q", secret, got)
		}
	}
}

func TestSafeSyncErrorDoesNotPersistSubscriptionSecrets(t *testing.T) {
	err := fmt.Errorf("fetch https://airport.example.com/sub?token=subscription-secret with username=proxy-user password=proxy-pass")
	got := safeSyncError(err)
	for _, secret := range []string{"airport.example.com", "subscription-secret", "proxy-user", "proxy-pass"} {
		if strings.Contains(got, secret) {
			t.Fatalf("sync error leaked %q: %q", secret, got)
		}
	}
}

func TestRedactAccountForUserResponseHidesCredentials(t *testing.T) {
	item := map[string]any{
		"id": int64(42),
		"credentials": map[string]any{
			"api_key":       "sensitive",
			"refresh_token": "also-sensitive",
			"base_url":      "https://api.example.com",
		},
	}

	RedactAccountForUserResponse(item)

	if item["has_credentials"] != true {
		t.Fatalf("expected has_credentials marker: %#v", item)
	}
	status, ok := item["credentials_status"].(map[string]bool)
	if !ok || !status["has_api_key"] || !status["has_refresh_token"] {
		t.Fatalf("expected credential presence markers: %#v", item)
	}
	if item["credentials_redacted"] != true {
		t.Fatalf("expected credentials_redacted marker: %#v", item)
	}
	credentials, ok := item["credentials"].(map[string]any)
	if !ok || len(credentials) != 1 || credentials["base_url"] != "https://api.example.com" {
		t.Fatalf("credentials should only retain non-sensitive settings: %#v", item["credentials"])
	}
}

func TestUserResourceOAuthResultUsesCanonicalCredentials(t *testing.T) {
	result := userResourceClaudeOAuthResult(&TokenInfo{
		AccessToken:  "access-secret",
		RefreshToken: "refresh-secret",
		TokenType:    "Bearer",
		ExpiresIn:    3600,
		ExpiresAt:    123456,
		Scope:        "user:inference",
		OrgUUID:      "org-id",
		AccountUUID:  "account-id",
		EmailAddress: "owner@example.com",
	})

	if result.Credentials["access_token"] != "access-secret" || result.Credentials["refresh_token"] != "refresh-secret" {
		t.Fatalf("canonical credentials were not built: %#v", result.Credentials)
	}
	if _, exists := result.Credentials["email_address"]; exists {
		t.Fatalf("identity metadata should not be mixed into credentials: %#v", result.Credentials)
	}
	if result.Extra["org_uuid"] != "org-id" || result.Extra["account_uuid"] != "account-id" {
		t.Fatalf("expected account metadata in extra: %#v", result.Extra)
	}
	if result.SuggestedName != "owner@example.com" {
		t.Fatalf("unexpected suggested name: %q", result.SuggestedName)
	}
}

func TestUserResourceOAuthSessionIsBoundToOwnerAndExpires(t *testing.T) {
	svc := NewUserResourceService(nil, nil, nil, nil)
	svc.recordUserOAuthSession(10, PlatformOpenAI, "session-id")

	if err := svc.authorizeUserOAuthSession(10, PlatformOpenAI, "session-id"); err != nil {
		t.Fatalf("owner should be allowed to exchange its OAuth session: %v", err)
	}
	if err := svc.authorizeUserOAuthSession(11, PlatformOpenAI, "session-id"); err == nil {
		t.Fatal("another user must not exchange an OAuth session")
	}

	key := userOAuthSessionKey(PlatformOpenAI, "session-id")
	svc.oauthSessionMu.Lock()
	session := svc.oauthSessions[key]
	session.CreatedAt = time.Now().Add(-16 * time.Minute)
	svc.oauthSessions[key] = session
	svc.oauthSessionMu.Unlock()
	if err := svc.authorizeUserOAuthSession(10, PlatformOpenAI, "session-id"); err == nil {
		t.Fatal("expired OAuth session must be rejected")
	}
}

func TestUserAccountUsageFiltersDoNotExposeOtherUserIdentity(t *testing.T) {
	args, where, err := userAccountUsageWhere(10, UserResourceListOptions{UserID: 999})
	if err != nil {
		t.Fatalf("build usage filter: %v", err)
	}
	if !strings.Contains(strings.Join(where, " "), "1 = 0") {
		t.Fatalf("cross-user filter should be forced empty: where=%#v args=%#v", where, args)
	}
	for _, arg := range args {
		if id, ok := arg.(int64); ok && id == 999 {
			t.Fatalf("foreign user id should not be used as a query oracle: %#v", args)
		}
	}

	_, where, err = userAccountUsageWhere(10, UserResourceListOptions{APIKeyID: 55})
	if err != nil {
		t.Fatalf("build API key usage filter: %v", err)
	}
	joined := strings.Join(where, " ")
	if !strings.Contains(joined, "own_key.user_id") || !strings.Contains(joined, "own_key.deleted_at IS NULL") {
		t.Fatalf("API key filter is not owner scoped: %s", joined)
	}
}

func TestValidateGroupSubscriberIDsRejectsUnassignedUsers(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("create sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()
	mock.ExpectQuery(`SELECT DISTINCT us.user_id`).
		WithArgs(int64(77), int64(10)).
		WillReturnRows(sqlmock.NewRows([]string{"user_id"}).AddRow(int64(20)))

	svc := NewUserResourceService(db, nil, nil, nil)
	if err := svc.validateGroupSubscriberIDs(context.Background(), 10, 77, []int64{20}); err != nil {
		t.Fatalf("assigned subscriber should be accepted: %v", err)
	}

	mock.ExpectQuery(`SELECT DISTINCT us.user_id`).
		WithArgs(int64(77), int64(10)).
		WillReturnRows(sqlmock.NewRows([]string{"user_id"}).AddRow(int64(20)))
	if err := svc.validateGroupSubscriberIDs(context.Background(), 10, 77, []int64{999}); err == nil {
		t.Fatal("unassigned user should be rejected")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet SQL expectations: %v", err)
	}
}

func TestBulkAssignSubscriptionCollectsEmailLookupErrors(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("create sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()

	mock.ExpectQuery(`(?s)SELECT EXISTS.*FROM groups`).
		WithArgs(int64(100), int64(10)).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectQuery(`SELECT id FROM users WHERE email = \$1`).
		WithArgs("missing@example.com").
		WillReturnError(sql.ErrNoRows)

	svc := NewUserResourceService(db, nil, nil, nil)
	result, err := svc.BulkAssignSubscription(context.Background(), 10, UserSubscriptionBulkAssignInput{
		GroupID:      100,
		ValidityDays: 30,
		Emails:       []string{"missing@example.com"},
	})
	if err != nil {
		t.Fatalf("BulkAssignSubscription returned top-level error: %v", err)
	}
	if result.FailedCount != 1 || result.SuccessCount != 0 || len(result.Errors) != 1 || len(result.Items) != 0 {
		t.Fatalf("unexpected bulk result: %#v", result)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

func TestExtendAssignedSubscriptionRejectsNonPositiveDays(t *testing.T) {
	svc := NewUserResourceService(nil, nil, nil, nil)
	if _, err := svc.ExtendAssignedSubscription(context.Background(), 10, 20, -1); err == nil {
		t.Fatalf("expected negative extension days to be rejected")
	}
	if _, err := svc.ExtendAssignedSubscription(context.Background(), 10, 20, MaxValidityDays+1); err == nil {
		t.Fatalf("expected excessive extension days to be rejected")
	}
}

func TestParseProxyNodeErrorDoesNotEchoRawNodeSecret(t *testing.T) {
	raw := "not-a-node-with-secret-refresh-token-abc123"
	node := parseProxyNode(raw)
	if node.Err == "" {
		t.Fatalf("expected parse error")
	}
	if strings.Contains(node.Err, raw) || strings.Contains(node.Err, "secret-refresh-token") {
		t.Fatalf("parse error leaked raw node: %q", node.Err)
	}
}

func TestParseProxyNodeLinesSupportsV2RayNShareFormats(t *testing.T) {
	vmessJSON := []byte(`{"v":"2","ps":"vmess node","add":"vmess.example.com","port":"443","id":"11111111-1111-1111-1111-111111111111","aid":"0","net":"ws","tls":"tls"}`)
	vmess := "vmess://" + base64.RawStdEncoding.EncodeToString(vmessJSON)
	legacySS := "ss://" + base64.RawURLEncoding.EncodeToString([]byte("aes-128-gcm:ss-secret@ss.example.com:8388")) + "#ss-node"

	nodes := parseProxyNodeLines(vmess + "\n" + legacySS)
	if len(nodes) != 2 {
		t.Fatalf("expected two parsed nodes, got %#v", nodes)
	}
	if nodes[0].Err != "" || nodes[0].Protocol != "vmess" || nodes[0].Host != "vmess.example.com" || nodes[0].Port != 443 || nodes[0].Network != "ws" {
		t.Fatalf("unexpected vmess node: %#v", nodes[0])
	}
	if _, err := buildXrayOutbound(nodes[0].Raw, &Proxy{Kind: "xray", Protocol: "vmess"}); err != nil {
		t.Fatalf("parsed vmess node did not produce a valid outbound: %v", err)
	}
	if nodes[1].Err != "" || nodes[1].Protocol != "ss" || nodes[1].Username != "aes-128-gcm" || nodes[1].Password != "ss-secret" || nodes[1].Name != "ss-node" {
		t.Fatalf("unexpected shadowsocks node: %#v", nodes[1])
	}
	if _, err := buildXrayOutbound(nodes[1].Raw, &Proxy{Kind: "xray", Protocol: "ss"}); err != nil {
		t.Fatalf("parsed shadowsocks node did not produce a valid outbound: %v", err)
	}
}

func TestParseSingBoxProxyNodesCombinesOutboundsAndEndpoints(t *testing.T) {
	privateKey := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("a", 32)))
	publicKey := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("b", 32)))
	raw := fmt.Sprintf(`{
  "outbounds": [{"type":"direct","tag":"direct"}],
  "endpoints": [{"type":"wireguard","tag":"wg","address":["172.16.0.2/32"],"private_key":%q,"peers":[{"address":"wg.example.com","port":51820,"public_key":%q}]}]
}`, privateKey, publicKey)
	nodes := parseProxyNodeLines(raw)
	if len(nodes) != 1 || nodes[0].Err != "" || nodes[0].Protocol != "wireguard" {
		t.Fatalf("unexpected sing-box endpoint parse result: %#v", nodes)
	}
	if _, err := buildSingBoxRuntimeSpec(nodes[0].Raw, &Proxy{Kind: "xray", Protocol: "wireguard"}); err != nil {
		t.Fatalf("wireguard endpoint did not produce a valid runtime spec: %v", err)
	}
}

func TestParseProxyNodeLinesSupportsClashYAML(t *testing.T) {
	raw := `
proxies:
  - name: ss node
    type: ss
    server: ss.example.com
    port: 8388
    cipher: aes-128-gcm
    password: ss-secret
    plugin: obfs
    plugin-opts:
      mode: http
      host: cdn.example.com
  - name: vless node
    type: vless
    server: vless.example.com
    port: 443
    uuid: 11111111-1111-1111-1111-111111111111
    tls: true
    network: grpc
    servername: sni.example.com
    grpc-opts:
      grpc-service-name: svc
  - name: socks node
    type: socks5
    server: socks.example.com
    port: 1080
    username: user
    password: pass
  - name: hysteria2 node
    type: hysteria2
    server: hy.example.com
    port: 443
    password: secret-should-not-leak
  - name: hysteria node
    type: hysteria
    server: hy1.example.com
    port: 8443
    auth-str: hysteria-secret
    up: 20
    down: 100
`

	nodes := parseProxyNodeLines(raw)
	if len(nodes) != 5 {
		t.Fatalf("expected 5 parsed clash nodes, got %d: %#v", len(nodes), nodes)
	}
	if nodes[0].Name != "ss node" || nodes[0].Protocol != "ss" || nodes[0].Kind != "xray" {
		t.Fatalf("unexpected ss node: %#v", nodes[0])
	}
	if _, err := buildXrayOutbound(nodes[0].Raw, &Proxy{Kind: "xray"}); err != nil {
		t.Fatalf("ss clash node did not produce a valid xray outbound: %v", err)
	}
	ssURL, err := url.Parse(nodes[0].Raw)
	if err != nil || ssURL.Query().Get("plugin") != "obfs;host=cdn.example.com;mode=http" {
		t.Fatalf("clash shadowsocks plugin options were not preserved: %q", nodes[0].Raw)
	}
	ssSpec, err := buildSingBoxRuntimeSpec(nodes[0].Raw, &Proxy{Kind: "xray", Protocol: "ss"})
	if err != nil {
		t.Fatalf("clash shadowsocks node did not produce a sing-box runtime spec: %v", err)
	}
	if plugin := stringFromMap(ssSpec.Outbound, "plugin"); plugin != "obfs-local" || stringFromMap(ssSpec.Outbound, "plugin_opts") != "obfs=http;obfs-host=cdn.example.com" {
		t.Fatalf("clash shadowsocks plugin was not mapped for sing-box: %#v", ssSpec.Outbound)
	}
	if nodes[1].Name != "vless node" || nodes[1].Protocol != "vless" || nodes[1].Network != "grpc" {
		t.Fatalf("unexpected vless node: %#v", nodes[1])
	}
	if _, err := buildXrayOutbound(nodes[1].Raw, &Proxy{Kind: "xray"}); err != nil {
		t.Fatalf("vless clash node did not produce a valid xray outbound: %v", err)
	}
	if nodes[2].Kind != "standard" || nodes[2].Protocol != "socks5" || nodes[2].Username != "user" || nodes[2].Password != "pass" {
		t.Fatalf("unexpected socks node: %#v", nodes[2])
	}
	if nodes[3].Err != "" || nodes[3].Protocol != "hysteria2" || nodes[3].Kind != "xray" {
		t.Fatalf("unexpected hysteria2 node: %#v", nodes[3])
	}
	if _, err := buildSingBoxRuntimeSpec(nodes[3].Raw, &Proxy{Kind: "xray", Protocol: "hysteria2"}); err != nil {
		t.Fatalf("hysteria2 clash node did not produce a valid sing-box outbound: %v", err)
	}
	if nodes[4].Err != "" || nodes[4].Protocol != "hysteria" || nodes[4].Kind != "xray" {
		t.Fatalf("unexpected hysteria node: %#v", nodes[4])
	}
	if _, err := buildSingBoxRuntimeSpec(nodes[4].Raw, &Proxy{Kind: "xray", Protocol: "hysteria"}); err != nil {
		t.Fatalf("hysteria clash node did not produce a valid sing-box outbound: %v", err)
	}
}

func TestParseClashProxyNodesPreservesNestedTransportOptions(t *testing.T) {
	raw := `
proxies:
  - name: vless ws
    type: vless
    server: vless.example.com
    port: 443
    uuid: 11111111-1111-1111-1111-111111111111
    tls: true
    network: ws
    ws-opts:
      path: /vless
      headers:
        Host: vless-cdn.example.com
  - name: trojan httpupgrade
    type: trojan
    server: trojan.example.com
    port: 443
    password: trojan-secret
    tls: true
    network: httpupgrade
    http-upgrade-opts:
      path: /upgrade
      host: trojan-cdn.example.com
  - name: vless xhttp
    type: vless
    server: xhttp.example.com
    port: 443
    uuid: 22222222-2222-2222-2222-222222222222
    tls: true
    network: xhttp
    xhttp-opts:
      path: /xhttp
      mode: auto
      headers:
        Host: xhttp-cdn.example.com
      extra:
        downloadSettings:
          server: download.example.com
          port: 443
          servername: download-sni.example.com
  - name: vmess grpc
    type: vmess
    server: vmess-grpc.example.com
    port: 443
    uuid: 33333333-3333-3333-3333-333333333333
    tls: true
    network: grpc
    grpc-opts:
      grpc-service-name: vmess-service
  - name: vmess ws
    type: vmess
    server: vmess-ws.example.com
    port: 443
    uuid: 44444444-4444-4444-4444-444444444444
    tls: true
    network: ws
    ws-opts:
      path: /vmess
      headers:
        Host: vmess-cdn.example.com
`

	nodes := parseProxyNodeLines(raw)
	if len(nodes) != 5 {
		t.Fatalf("expected 5 transport nodes, got %d", len(nodes))
	}
	for index, node := range nodes {
		if node.Err != "" {
			t.Fatalf("transport node %d was rejected during import", index)
		}
		if _, err := buildXrayOutbound(node.Raw, &Proxy{Kind: "xray", Protocol: node.Protocol}); err != nil {
			t.Fatalf("transport node %d did not produce an xray outbound", index)
		}
	}

	for index, expected := range []struct {
		path string
		host string
		mode string
	}{
		{path: "/vless", host: "vless-cdn.example.com"},
		{path: "/upgrade", host: "trojan-cdn.example.com"},
		{path: "/xhttp", host: "xhttp-cdn.example.com", mode: "auto"},
	} {
		u, err := url.Parse(nodes[index].Raw)
		if err != nil {
			t.Fatalf("parse transport node %d", index)
		}
		if u.Query().Get("path") != expected.path || u.Query().Get("host") != expected.host || u.Query().Get("mode") != expected.mode {
			t.Fatalf("nested transport options were not preserved for node %d", index)
		}
	}
	var xhttpExtra map[string]any
	if err := json.Unmarshal([]byte(mustParseURL(t, nodes[2].Raw).Query().Get("extra")), &xhttpExtra); err != nil {
		t.Fatal("xhttp extra settings were not preserved")
	}
	if _, ok := xhttpExtra["downloadSettings"].(map[string]any); !ok {
		t.Fatal("xhttp download settings were not preserved")
	}

	for index, expected := range []struct {
		network string
		path    string
		host    string
	}{
		{network: "grpc", path: "vmess-service"},
		{network: "ws", path: "/vmess", host: "vmess-cdn.example.com"},
	} {
		payload := strings.TrimPrefix(nodes[index+3].Raw, "vmess://")
		decoded, ok := decodeShareBase64(payload)
		if !ok {
			t.Fatalf("decode vmess transport node %d", index)
		}
		var vmess map[string]any
		if err := json.Unmarshal([]byte(decoded), &vmess); err != nil {
			t.Fatalf("decode vmess transport payload %d", index)
		}
		if urAsString(vmess["net"]) != expected.network || urAsString(vmess["path"]) != expected.path || urAsString(vmess["host"]) != expected.host {
			t.Fatalf("nested vmess transport options were not preserved for node %d", index)
		}
	}
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal("parse proxy node URL")
	}
	return u
}

func TestParseProxyNodeLinesSupportsSingBoxJSON(t *testing.T) {
	raw := `{
  "outbounds": [
    {"type":"direct","tag":"direct"},
    {"type":"socks","tag":"local socks","server":"socks.example.com","server_port":1080,"username":"user","password":"pass"},
    {"type":"vless","tag":"vless reality","server":"vless.example.com","server_port":443,"uuid":"11111111-1111-1111-1111-111111111111","flow":"xtls-rprx-vision","transport":{"type":"grpc","service_name":"svc"},"tls":{"enabled":true,"server_name":"sni.example.com","reality":{"enabled":true,"public_key":"pub","short_id":"abc"}}},
    {"type":"tuic","tag":"tuic node","server":"tuic.example.com","server_port":443,"uuid":"22222222-2222-2222-2222-222222222222","password":"tuic-secret","congestion_control":"bbr","tls":{"enabled":true,"server_name":"tuic-sni.example.com"}}
  ]
}`
	nodes := parseProxyNodeLines(raw)
	if len(nodes) != 3 {
		t.Fatalf("expected 3 supported sing-box nodes, got %#v", nodes)
	}
	if nodes[0].Name != "local socks" || nodes[0].Kind != "standard" || nodes[0].Protocol != "socks5" {
		t.Fatalf("unexpected socks node: %#v", nodes[0])
	}
	if nodes[1].Name != "vless reality" || nodes[1].Kind != "xray" || nodes[1].Protocol != "vless" || nodes[1].Network != "grpc" {
		t.Fatalf("unexpected vless node: %#v", nodes[1])
	}
	if _, err := buildXrayOutbound(nodes[1].Raw, &Proxy{Kind: "xray"}); err != nil {
		t.Fatalf("sing-box vless node did not produce a valid xray outbound: %v", err)
	}
	if nodes[2].Protocol != "tuic" || nodes[2].Kind != "xray" || nodes[2].Err != "" {
		t.Fatalf("unexpected tuic node: %#v", nodes[2])
	}
	if _, err := buildSingBoxRuntimeSpec(nodes[2].Raw, &Proxy{Kind: "xray", Protocol: "tuic"}); err != nil {
		t.Fatalf("sing-box tuic node did not produce a valid runtime spec: %v", err)
	}
}

func TestValidateExternalHTTPURLRejectsLocalAndMetadataTargets(t *testing.T) {
	for _, raw := range []string{
		"http://127.0.0.1:8080/sub",
		"http://[::1]/sub",
		"http://169.254.169.254/latest/meta-data",
		"http://100.64.0.1/internal",
		"http://localhost/sub",
		"http://metadata.google.internal/computeMetadata/v1",
		"ftp://example.com/sub",
		"http://user:pass@example.com/sub",
	} {
		if err := validateExternalHTTPURL(context.Background(), raw); err == nil {
			t.Fatalf("expected %s to be rejected", raw)
		}
	}
}

func TestUserOwnedAccountAndProxyRejectInternalOutboundTargets(t *testing.T) {
	ctx := context.Background()
	if err := validateUserOwnedAccountURLs(ctx, map[string]any{
		"credentials": map[string]any{"base_url": "http://127.0.0.1:2375"},
	}); err == nil {
		t.Fatal("expected private account base_url to be rejected")
	}

	svc := NewUserResourceService(nil, nil, nil, nil)
	proxyPayload := map[string]any{
		"name": "private-target", "kind": "standard", "protocol": "http",
		"host": "10.0.0.1", "port": 8080, "status": StatusActive,
		"fallback_mode": FallbackModeNone, "expiry_warn_days": 7, "extra": map[string]any{},
	}
	if err := svc.normalizeAndValidateProxyPayload(ctx, 10, 0, nil, proxyPayload); err == nil {
		t.Fatal("expected private proxy target to be rejected")
	}

	xrayPayload := map[string]any{
		"name": "raw-outbound", "kind": "xray", "protocol": "socks5",
		"host": "203.0.113.10", "port": 1080, "status": StatusActive,
		"fallback_mode": FallbackModeNone, "expiry_warn_days": 7,
		"extra": map[string]any{"outbound": map[string]any{"protocol": "freedom"}},
	}
	if err := svc.normalizeAndValidateProxyPayload(ctx, 10, 0, nil, xrayPayload); err == nil {
		t.Fatal("expected raw xray outbound injection to be rejected")
	}
}

func TestUserResourceProxyValidationNormalizesLegacySocksAlias(t *testing.T) {
	svc := NewUserResourceService(nil, nil, nil, nil)
	payload := map[string]any{
		"name": "legacy-socks", "kind": "standard", "protocol": "socks",
		"host": "8.8.8.8", "port": 1080, "status": StatusActive,
		"fallback_mode": FallbackModeNone, "expiry_warn_days": 0,
	}
	if err := svc.normalizeAndValidateProxyPayload(context.Background(), 10, 0, nil, payload); err != nil {
		t.Fatalf("legacy SOCKS alias was rejected: %v", err)
	}
	if payload["protocol"] != "socks5h" {
		t.Fatalf("legacy SOCKS alias was not normalized: %#v", payload["protocol"])
	}
}

func TestPrivateStandardGroupsRequireOwnership(t *testing.T) {
	ownerID := int64(10)
	group := &Group{ID: 55, OwnerUserID: &ownerID, SubscriptionType: SubscriptionTypeStandard}
	svc := &APIKeyService{}
	if !svc.canUserBindGroupInternal(&User{ID: ownerID}, group, map[int64]bool{}) {
		t.Fatal("group owner should be able to bind the private group")
	}
	if svc.canUserBindGroupInternal(&User{ID: 20}, group, map[int64]bool{}) {
		t.Fatal("foreign user should not bind a private standard group")
	}
	if svc.canUserBindGroupInternal(&User{ID: 20}, group, map[int64]bool{group.ID: true}) {
		t.Fatal("a subscription must not make a private standard group shareable")
	}
}

func TestUserResourceBulkAssignmentHasHardLimit(t *testing.T) {
	input := UserSubscriptionBulkAssignInput{UserIDs: make([]int64, userResourceBatchMaxItems+1)}
	svc := NewUserResourceService(nil, nil, nil, nil)
	if _, err := svc.BulkAssignSubscription(context.Background(), 10, input); err == nil {
		t.Fatal("expected oversized bulk assignment to be rejected before database access")
	}
}

func TestGetProxyLooksUpByIDAndRedactsAnotherUsersPublicProxy(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("create sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()

	now := time.Now()
	rows := sqlmock.NewRows([]string{
		"id", "owner_user_id", "is_public", "kind", "name", "protocol", "host", "port",
		"username", "password", "has_auth", "status", "expires_at", "fallback_mode",
		"backup_proxy_id", "expiry_warn_days", "extra", "created_at", "updated_at", "account_count",
	}).AddRow(
		int64(1501), int64(77), true, "xray", "public-node", "socks5", "203.0.113.10", 1080,
		"proxy-user", "proxy-pass", true, StatusActive, nil, FallbackModeNone,
		nil, 7, `{"raw":"vless://secret","region":"us"}`, now, now, int64(0),
	)
	mock.ExpectQuery(`(?s)FROM proxies p.*WHERE p\.id = \$1.*p\.owner_user_id = \$2`).
		WithArgs(int64(1501), int64(99)).
		WillReturnRows(rows)

	svc := NewUserResourceService(db, nil, nil, nil)
	item, err := svc.GetProxy(context.Background(), 99, 1501)
	if err != nil {
		t.Fatalf("GetProxy returned error: %v", err)
	}
	for _, key := range []string{"username", "password", "host", "port", "owner_user_id", "extra"} {
		if _, exists := item[key]; exists {
			t.Fatalf("public proxy private field %q was returned: %#v", key, item)
		}
	}
	if item["is_owned"] != false || item["details_hidden"] != true {
		t.Fatalf("foreign public proxy endpoint was not hidden: %#v", item)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

func TestTestProxyDoesNotEchoPublicProxyCredentials(t *testing.T) {
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
		int64(1501), nil, true, "standard", "public-node", "socks5", "", 0,
		"proxy-user", "proxy-pass", true, StatusActive, nil, FallbackModeNone,
		nil, 7, `{}`, now, now,
	)
	mock.ExpectQuery(`(?s)FROM proxies p.*WHERE p\.id = \$1.*p\.owner_user_id = \$2`).
		WithArgs(int64(1501), int64(99)).
		WillReturnRows(rows)

	svc := NewUserResourceService(db, nil, nil, nil)
	result, err := svc.TestProxy(context.Background(), 99, 1501)
	if err != nil {
		t.Fatalf("TestProxy returned error: %v", err)
	}
	message := urAsString(result["message"])
	if strings.Contains(message, "proxy-user") || strings.Contains(message, "proxy-pass") {
		t.Fatalf("proxy test message leaked credentials: %q", message)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

func TestRedactUpstreamErrorsForUserScrubsSecretsAndForeignRequester(t *testing.T) {
	items := []map[string]any{{
		"user_id":                int64(77),
		"api_key_id":             int64(88),
		"user_agent":             "client access_token=secret-token",
		"message":                `{"error":"bad prompt: private customer text","api_key":"sk-secret"}`,
		"upstream_error_message": "customer@example.com Authorization: Bearer leaked-token",
		"request_path":           "/v1/messages?api_key=sk-path-secret",
		"client_request_id":      "req-secret",
		"upstream_errors": []any{
			map[string]any{
				"message":       "refresh_token=refresh-secret",
				"Authorization": "Bearer nested-secret",
			},
		},
	}}

	redactUpstreamErrorItemsForUser(items, 10)

	item := items[0]
	if item["user_id"] != nil || item["api_key_id"] != nil || item["user_agent"] != "" {
		t.Fatalf("foreign requester fields were not hidden: %#v", item)
	}
	if item["message"] != "upstream request failed" || item["upstream_error_message"] != "" || item["request_path"] != "" || item["client_request_id"] != "" {
		t.Fatalf("foreign request details were not removed: %#v", item)
	}
	upstreamErrors, ok := item["upstream_errors"].([]any)
	if !ok {
		t.Fatalf("unexpected upstream error collection: %#v", item["upstream_errors"])
	}
	if len(upstreamErrors) != 0 {
		t.Fatalf("foreign upstream error details were not removed: %#v", upstreamErrors)
	}
}

func TestRedactUsageLogsForUserHidesForeignRequesterNetworkFields(t *testing.T) {
	items := []map[string]any{{
		"user_id":    int64(22),
		"api_key_id": int64(33),
		"ip_address": "203.0.113.9",
		"user_agent": "client access_token=foreign-secret",
	}, {
		"user_id":    int64(10),
		"api_key_id": int64(44),
		"ip_address": "198.51.100.2",
		"user_agent": "client access_token=own-secret",
	}}

	redactUsageLogItemsForUser(items, 10)

	if items[0]["user_id"] != nil || items[0]["api_key_id"] != nil || items[0]["ip_address"] != "" || items[0]["user_agent"] != "" {
		t.Fatalf("foreign usage requester fields were not hidden: %#v", items[0])
	}
	if items[1]["user_id"] != int64(10) || items[1]["api_key_id"] != int64(44) || items[1]["ip_address"] != "198.51.100.2" {
		t.Fatalf("own requester fields should be preserved: %#v", items[1])
	}
	if strings.Contains(urAsString(items[1]["user_agent"]), "own-secret") {
		t.Fatalf("own user agent should still redact inline secrets: %#v", items[1]["user_agent"])
	}
}

func TestListAccountUsageLogsRedactsForeignRequesterFields(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("create sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()

	now := time.Now()
	mock.ExpectQuery(`(?s)SELECT COUNT\(\*\).*FROM usage_logs ul.*JOIN accounts a`).
		WithArgs(int64(10)).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(2)))
	rows := sqlmock.NewRows([]string{
		"id", "user_id", "api_key_id", "account_id", "account_name", "group_id",
		"group_name", "subscription_id", "request_id", "model", "requested_model",
		"upstream_model", "input_tokens", "output_tokens", "cache_creation_tokens",
		"cache_read_tokens", "total_cost", "actual_cost", "rate_multiplier",
		"account_rate_multiplier", "billing_type", "stream", "duration_ms",
		"first_token_ms", "user_agent", "ip_address", "created_at",
	}).
		AddRow(int64(1), int64(22), int64(33), int64(44), "owned-account", int64(55),
			"owned-group", int64(66), "req-foreign", "claude", "claude", "claude",
			int64(1), int64(2), int64(0), int64(0), float64(0.1), float64(0.1),
			float64(1), float64(1), "tokens", false, int64(120), int64(30),
			"client access_token=foreign-secret", "203.0.113.9", now).
		AddRow(int64(2), int64(10), int64(77), int64(44), "owned-account", int64(55),
			"owned-group", int64(66), "req-owner", "claude", "claude", "claude",
			int64(1), int64(2), int64(0), int64(0), float64(0.1), float64(0.1),
			float64(1), float64(1), "tokens", false, int64(120), int64(30),
			"client access_token=owner-secret", "198.51.100.2", now)
	mock.ExpectQuery(`(?s)SELECT ul\.id, ul\.user_id.*FROM usage_logs ul.*JOIN accounts a`).
		WithArgs(int64(10), 20, 0).
		WillReturnRows(rows)

	svc := NewUserResourceService(db, nil, nil, nil)
	page, err := svc.ListAccountUsageLogs(context.Background(), 10, UserResourceListOptions{})
	if err != nil {
		t.Fatalf("ListAccountUsageLogs returned error: %v", err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("expected two usage rows, got %#v", page.Items)
	}
	foreign := page.Items[0]
	if foreign["user_id"] != nil || foreign["api_key_id"] != nil || foreign["ip_address"] != "" || foreign["user_agent"] != "" {
		t.Fatalf("foreign requester fields were not redacted by list API: %#v", foreign)
	}
	owner := page.Items[1]
	if owner["user_id"] != int64(10) || owner["api_key_id"] != int64(77) || owner["ip_address"] != "198.51.100.2" {
		t.Fatalf("owner requester fields should be preserved: %#v", owner)
	}
	if strings.Contains(urAsString(owner["user_agent"]), "owner-secret") {
		t.Fatalf("owner user agent inline secret was not redacted: %#v", owner["user_agent"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

func TestListRedeemCodeUsagesRejectsNonOwner(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("create sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()

	mock.ExpectQuery(`(?s)SELECT COUNT\(\*\).*FROM redeem_code_usages rcu.*r\.owner_user_id = \$2`).
		WithArgs(int64(44), int64(10)).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(0)))
	mock.ExpectQuery(`(?s)SELECT rcu\.id.*FROM redeem_code_usages rcu.*r\.owner_user_id = \$2`).
		WithArgs(int64(44), int64(10), 20, 0).
		WillReturnRows(sqlmock.NewRows([]string{"id", "redeem_code_id", "user_id", "user_email", "username", "used_at"}))
	mock.ExpectQuery(`SELECT EXISTS\(SELECT 1 FROM redeem_codes WHERE id = \$1 AND owner_user_id = \$2\)`).
		WithArgs(int64(44), int64(10)).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))

	svc := NewUserResourceService(db, nil, nil, nil)
	_, err = svc.ListRedeemCodeUsages(context.Background(), 10, 44, UserResourceListOptions{})
	if err != ErrUserResourceNotFound {
		t.Fatalf("expected owner-scoped not found error, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

func TestListUpstreamErrorsRedactsMessagesAndForeignRequesterFields(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("create sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()

	now := time.Now()
	mock.ExpectQuery(`(?s)SELECT COUNT\(\*\).*FROM ops_error_logs e.*LEFT JOIN accounts a`).
		WithArgs(int64(10)).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(1)))
	rows := sqlmock.NewRows([]string{
		"id", "created_at", "request_id", "client_request_id", "user_id",
		"api_key_id", "account_id", "account_name", "group_id", "group_name",
		"platform", "model", "requested_model", "upstream_model", "phase", "type",
		"error_owner", "error_source", "severity", "status_code",
		"upstream_status_code", "upstream_error_message", "message", "request_path",
		"stream", "user_agent", "upstream_errors",
	}).AddRow(
		int64(1), now, "req-1", "client-secret-id", int64(22),
		int64(33), int64(44), "owned-account", int64(55), "owned-group",
		"anthropic", "claude", "claude", "claude", "upstream", "bad_request",
		"provider", "gateway", "error", int64(502),
		int64(401), "customer@example.com Authorization: Bearer leaked-token", `{"error":"private prompt text","api_key":"sk-secret"}`,
		"/v1/messages?api_key=sk-path-secret", false, "agent access_token=ua-secret",
		`[{"message":"refresh_token=refresh-secret","Authorization":"Bearer nested-secret"}]`,
	)
	mock.ExpectQuery(`(?s)SELECT e\.id, e\.created_at.*FROM ops_error_logs e.*LEFT JOIN accounts a`).
		WithArgs(int64(10), 20, 0).
		WillReturnRows(rows)

	svc := NewUserResourceService(db, nil, nil, nil)
	page, err := svc.ListUpstreamErrors(context.Background(), 10, UserResourceListOptions{})
	if err != nil {
		t.Fatalf("ListUpstreamErrors returned error: %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("expected one upstream error row, got %#v", page.Items)
	}
	item := page.Items[0]
	if item["user_id"] != nil || item["api_key_id"] != nil || item["user_agent"] != "" {
		t.Fatalf("foreign requester fields were not redacted by list API: %#v", item)
	}
	raw, _ := json.Marshal(item)
	text := string(raw)
	for _, secret := range []string{"customer@example.com", "private prompt text", "leaked-token", "sk-secret", "sk-path-secret", "ua-secret", "refresh-secret", "nested-secret"} {
		if strings.Contains(text, secret) {
			t.Fatalf("upstream error list leaked %q in %#v", secret, item)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

func TestRedactPoolHealthForSubscriberHidesAccountIdentity(t *testing.T) {
	health := &SubscriptionPoolHealth{
		GroupID:     9,
		Available:   1,
		RateLimited: 1,
		Total:       2,
		ByStatus:    map[string]int64{StatusActive: 2},
		Reasons: []PoolHealthReason{{
			AccountID: 123,
			Name:      "private-account-name",
			Status:    "error",
			Reason:    "upstream failed with api_key=sk-secret",
		}},
	}

	redacted := RedactPoolHealthForSubscriber(health)

	if redacted == health {
		t.Fatalf("subscriber redaction should return a copy")
	}
	if redacted.Reasons[0].AccountID != 0 || redacted.Reasons[0].Name != "" {
		t.Fatalf("subscriber pool health leaked account identity: %#v", redacted.Reasons[0])
	}
	if strings.Contains(redacted.Reasons[0].Reason, "sk-secret") {
		t.Fatalf("subscriber pool health leaked secret in reason: %#v", redacted.Reasons[0])
	}
	if health.Reasons[0].AccountID != 123 || health.Reasons[0].Name != "private-account-name" {
		t.Fatalf("original pool health should not be mutated: %#v", health.Reasons[0])
	}
}

func TestUserResourceOwnerValidationRejectsForeignReferences(t *testing.T) {
	tests := []struct {
		name string
		run  func(ctx context.Context, svc *UserResourceService, mock sqlmock.Sqlmock) error
	}{
		{
			name: "group ids must belong to current user",
			run: func(ctx context.Context, svc *UserResourceService, mock sqlmock.Sqlmock) error {
				mock.ExpectQuery(`SELECT COUNT\(\*\) FROM groups WHERE owner_user_id = \$1`).
					WithArgs(int64(10), sqlmock.AnyArg()).
					WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
				return svc.validateOwnedGroupIDs(ctx, 10, []int64{100, 101})
			},
		},
		{
			name: "account ids must belong to current user",
			run: func(ctx context.Context, svc *UserResourceService, mock sqlmock.Sqlmock) error {
				mock.ExpectQuery(`SELECT COUNT\(\*\) FROM accounts WHERE owner_user_id = \$1`).
					WithArgs(int64(10), sqlmock.AnyArg()).
					WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
				return svc.validateOwnedAccountIDs(ctx, 10, []int64{200})
			},
		},
		{
			name: "private proxy owned by another user is not selectable",
			run: func(ctx context.Context, svc *UserResourceService, mock sqlmock.Sqlmock) error {
				mock.ExpectQuery(`(?s)SELECT EXISTS \(\s+SELECT 1 FROM proxies`).
					WithArgs(int64(300), int64(10)).
					WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
				return svc.validateProxySelectable(ctx, 10, 300)
			},
		},
		{
			name: "redeem codes must target owned subscription groups",
			run: func(ctx context.Context, svc *UserResourceService, mock sqlmock.Sqlmock) error {
				mock.ExpectQuery(`(?s)SELECT EXISTS \(\s+SELECT 1 FROM groups`).
					WithArgs(int64(400), int64(10)).
					WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
				return svc.validateOwnedSubscriptionGroup(ctx, 10, 400)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatalf("create sqlmock: %v", err)
			}
			defer func() { _ = db.Close() }()

			svc := NewUserResourceService(db, nil, nil, nil)
			if err := tt.run(context.Background(), svc, mock); err == nil {
				t.Fatalf("expected foreign reference to be rejected")
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("unmet sql expectations: %v", err)
			}
		})
	}
}

func TestUserResourceCapacityLimitRejectsAdditionalResources(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("create sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()

	svc := NewUserResourceService(db, nil, nil, nil)
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM proxies WHERE owner_user_id IS NOT DISTINCT FROM \$1 AND deleted_at IS NULL`).
		WithArgs(int64(42)).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(userResourceMaxProxies))

	err = svc.ensureOwnedResourceCapacity(context.Background(), "proxies", 42, userResourceMaxProxies)
	if err == nil {
		t.Fatal("expected resource limit error")
	}
	if got := infraerrors.Code(err); got != http.StatusTooManyRequests {
		t.Fatalf("expected HTTP 429, got %d: %v", got, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

func TestUserResourceWritesRejectInvalidAdminAlignedFields(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("create sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()
	svc := NewUserResourceService(db, nil, nil, nil)
	ctx := context.Background()

	tests := []struct {
		name string
		run  func() error
	}{
		{
			name: "group rate multiplier must be positive",
			run: func() error {
				_, err := svc.CreateGroup(ctx, 10, map[string]any{"name": "bad-rate", "rate_multiplier": 0})
				return err
			},
		},
		{
			name: "batch hold cannot be below discount",
			run: func() error {
				_, err := svc.CreateGroup(ctx, 10, map[string]any{
					"name": "bad-batch", "batch_image_discount_multiplier": 0.8, "batch_image_hold_multiplier": 0.5,
				})
				return err
			},
		},
		{
			name: "peak window must be valid",
			run: func() error {
				_, err := svc.CreateGroup(ctx, 10, map[string]any{
					"name": "bad-peak", "subscription_type": SubscriptionTypeSubscription,
					"peak_rate_enabled": true, "peak_start": "20:00", "peak_end": "10:00", "peak_rate_multiplier": 2,
				})
				return err
			},
		},
		{
			name: "boolean scheduler state cannot be malformed",
			run: func() error {
				_, err := svc.CreateAccount(ctx, 10, map[string]any{
					"name": "bad-bool", "platform": PlatformOpenAI, "type": AccountTypeAPIKey, "schedulable": "sometimes",
				})
				return err
			},
		},
		{
			name: "load factor has an upper bound",
			run: func() error {
				_, err := svc.CreateAccount(ctx, 10, map[string]any{
					"name": "bad-load", "platform": PlatformOpenAI, "type": AccountTypeAPIKey, "load_factor": 10001,
				})
				return err
			},
		},
		{
			name: "account billing multiplier cannot be negative",
			run: func() error {
				_, err := svc.CreateAccount(ctx, 10, map[string]any{
					"name": "bad-account-rate", "platform": PlatformOpenAI, "type": AccountTypeAPIKey, "rate_multiplier": -0.1,
				})
				return err
			},
		},
		{
			name: "proxy kind is constrained",
			run: func() error {
				_, err := svc.CreateProxy(ctx, 10, map[string]any{
					"name": "bad-proxy", "kind": "shell", "protocol": "socks5", "host": "proxy.example.com", "port": 1080,
				})
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.run(); err == nil {
				t.Fatal("expected invalid payload to be rejected")
			}
		})
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("invalid payload unexpectedly reached SQL: %v", err)
	}
}

func TestNormalizeUserAccountTypeAliases(t *testing.T) {
	tests := map[string]string{
		"api_key":     AccountTypeAPIKey,
		" API_KEY ":   AccountTypeAPIKey,
		"setup_token": AccountTypeSetupToken,
		"SETUP_TOKEN": AccountTypeSetupToken,
		"oauth":       AccountTypeOAuth,
	}
	for input, want := range tests {
		if got := normalizeUserAccountType(input); got != want {
			t.Errorf("normalizeUserAccountType(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestUserOAuthSessionIsBoundToOwnerAndPlatform(t *testing.T) {
	svc := NewUserResourceService(nil, nil, nil, nil)
	svc.recordUserOAuthSession(10, PlatformOpenAI, "session-1")

	if err := svc.authorizeUserOAuthSession(10, PlatformOpenAI, "session-1"); err != nil {
		t.Fatalf("owner should be allowed to use OAuth session: %v", err)
	}
	if err := svc.authorizeUserOAuthSession(11, PlatformOpenAI, "session-1"); err == nil {
		t.Fatal("another user must not use the OAuth session")
	}
	if err := svc.authorizeUserOAuthSession(10, PlatformGemini, "session-1"); err == nil {
		t.Fatal("OAuth session must not be reused for another platform")
	}

	svc.forgetUserOAuthSession(PlatformOpenAI, "session-1")
	if err := svc.authorizeUserOAuthSession(10, PlatformOpenAI, "session-1"); err == nil {
		t.Fatal("consumed OAuth session must not remain available")
	}
}

func TestDeleteGroupUsesTransactionAndInvalidatesAffectedUsers(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("create sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()

	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE groups SET deleted_at`).
		WithArgs(int64(55), int64(10)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`DELETE FROM account_groups WHERE group_id = \$1`).
		WithArgs(int64(55)).
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectQuery(`(?s)UPDATE user_subscriptions.*RETURNING user_id`).
		WithArgs(int64(55), int64(10)).
		WillReturnRows(sqlmock.NewRows([]string{"user_id"}).AddRow(int64(21)).AddRow(int64(21)).AddRow(int64(22)))
	mock.ExpectExec(`INSERT INTO scheduler_outbox`).
		WithArgs(SchedulerOutboxEventGroupChanged, nil, int64(55), nil).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	svc := NewUserResourceService(db, nil, nil, nil)
	if err := svc.DeleteGroup(context.Background(), 10, 55); err != nil {
		t.Fatalf("DeleteGroup returned error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet SQL expectations: %v", err)
	}
}

func TestDeleteGroupRollsBackWhenBindingCleanupFails(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("create sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()

	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE groups SET deleted_at`).
		WithArgs(int64(55), int64(10)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`DELETE FROM account_groups WHERE group_id = \$1`).
		WithArgs(int64(55)).
		WillReturnError(sql.ErrConnDone)
	mock.ExpectRollback()

	svc := NewUserResourceService(db, nil, nil, nil)
	if err := svc.DeleteGroup(context.Background(), 10, 55); err == nil {
		t.Fatal("expected DeleteGroup to return the cleanup error")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet SQL expectations: %v", err)
	}
}

func TestCreateAccountRollsBackWhenGroupBindingFails(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("create sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()

	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM accounts`).
		WithArgs(int64(10)).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectBegin()
	mock.ExpectQuery(`INSERT INTO accounts`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(55)))
	mock.ExpectExec(`DELETE FROM account_groups WHERE account_id = \$1`).
		WithArgs(int64(55)).
		WillReturnError(sql.ErrConnDone)
	mock.ExpectRollback()

	svc := NewUserResourceService(db, nil, nil, nil)
	_, err = svc.CreateAccount(context.Background(), 10, map[string]any{
		"name":        "transactional-account",
		"platform":    PlatformOpenAI,
		"type":        AccountTypeAPIKey,
		"credentials": map[string]any{"api_key": "sk-test"},
	})
	if err == nil {
		t.Fatal("expected CreateAccount to return the binding error")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet SQL expectations: %v", err)
	}
}

func TestDeleteAccountRollsBackWhenBindingCleanupFails(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("create sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT group_id FROM account_groups WHERE account_id = \$1`).
		WithArgs(int64(55)).
		WillReturnRows(sqlmock.NewRows([]string{"group_id"}).AddRow(int64(77)))
	mock.ExpectExec(`UPDATE accounts SET deleted_at`).
		WithArgs(int64(55), int64(10)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`DELETE FROM account_groups WHERE account_id = \$1`).
		WithArgs(int64(55)).
		WillReturnError(sql.ErrConnDone)
	mock.ExpectRollback()

	svc := NewUserResourceService(db, nil, nil, nil)
	if err := svc.DeleteAccount(context.Background(), 10, 55); err == nil {
		t.Fatal("expected DeleteAccount to return the cleanup error")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet SQL expectations: %v", err)
	}
}

func TestUnsubscribeOwnSubscriptionWritesRevocationAtomically(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("create sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()

	mock.ExpectBegin()
	mock.ExpectQuery(`(?s)UPDATE user_subscriptions.*revoked_by_user_id.*RETURNING group_id`).
		WithArgs(int64(55), int64(10)).
		WillReturnRows(sqlmock.NewRows([]string{"group_id"}).AddRow(int64(77)))
	mock.ExpectCommit()

	svc := NewUserResourceService(db, nil, nil, nil)
	if err := svc.UnsubscribeOwnSubscription(context.Background(), 10, 55); err != nil {
		t.Fatalf("UnsubscribeOwnSubscription returned error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet SQL expectations: %v", err)
	}
}

func TestUnsubscribeOwnSubscriptionRollsBackWhenRevocationWriteFails(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("create sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()

	mock.ExpectBegin()
	mock.ExpectQuery(`(?s)UPDATE user_subscriptions.*revoked_by_user_id.*RETURNING group_id`).
		WithArgs(int64(55), int64(10)).
		WillReturnError(sql.ErrConnDone)
	mock.ExpectRollback()

	svc := NewUserResourceService(db, nil, nil, nil)
	if err := svc.UnsubscribeOwnSubscription(context.Background(), 10, 55); err == nil {
		t.Fatal("expected unsubscribe write to fail")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet SQL expectations: %v", err)
	}
}

func TestDeleteGroupRollsBackWhenSchedulerOutboxFails(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("create sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()

	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE groups SET deleted_at`).
		WithArgs(int64(55), int64(10)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`DELETE FROM account_groups WHERE group_id = \$1`).
		WithArgs(int64(55)).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`(?s)UPDATE user_subscriptions.*RETURNING user_id`).
		WithArgs(int64(55), int64(10)).
		WillReturnRows(sqlmock.NewRows([]string{"user_id"}))
	mock.ExpectExec(`INSERT INTO scheduler_outbox`).
		WithArgs(SchedulerOutboxEventGroupChanged, nil, int64(55), nil).
		WillReturnError(sql.ErrConnDone)
	mock.ExpectRollback()

	svc := NewUserResourceService(db, nil, nil, nil)
	if err := svc.DeleteGroup(context.Background(), 10, 55); err == nil {
		t.Fatal("expected outbox failure to roll back group deletion")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet SQL expectations: %v", err)
	}
}

func TestManualAssignmentCannotBypassSubscriberOptOut(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("create sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()

	now := time.Now()
	mock.ExpectBegin()
	mock.ExpectQuery(`(?s)SELECT id, expires_at.*deleted_at, revoked_by_user_id.*FROM user_subscriptions`).
		WithArgs(int64(21), int64(55)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "expires_at", "notes", "deleted_at", "revoked_by_user_id"}).
			AddRow(int64(77), now.Add(24*time.Hour), "", now, int64(21)))
	mock.ExpectRollback()

	svc := NewUserResourceService(db, nil, nil, nil)
	_, err = svc.assignOrExtendSubscription(context.Background(), 10, 21, 55, 30, "", "manual", nil)
	if infraerrors.Reason(err) != "SUBSCRIPTION_USER_UNSUBSCRIBED" {
		t.Fatalf("expected subscriber opt-out conflict, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet SQL expectations: %v", err)
	}
}

func TestClearAccountErrorRollsBackWhenSchedulerOutboxFails(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("create sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()

	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE accounts SET error_message = NULL`).
		WithArgs(int64(55), int64(10)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO scheduler_outbox`).
		WithArgs(SchedulerOutboxEventAccountChanged, int64(55), nil, nil).
		WillReturnError(sql.ErrConnDone)
	mock.ExpectRollback()

	svc := NewUserResourceService(db, nil, nil, nil)
	if _, err := svc.ClearAccountError(context.Background(), 10, 55); err == nil {
		t.Fatal("expected outbox failure to roll back account state update")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet SQL expectations: %v", err)
	}
}

func TestCreateAccountRollsBackWhenSchedulerOutboxFails(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("create sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()

	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM accounts`).
		WithArgs(int64(10)).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectBegin()
	mock.ExpectQuery(`INSERT INTO accounts .* RETURNING id`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(55)))
	mock.ExpectExec(`DELETE FROM account_groups WHERE account_id = \$1`).
		WithArgs(int64(55)).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`INSERT INTO scheduler_outbox`).
		WithArgs(SchedulerOutboxEventAccountChanged, int64(55), nil, nil).
		WillReturnError(sql.ErrConnDone)
	mock.ExpectRollback()

	svc := NewUserResourceService(db, nil, nil, nil)
	_, err = svc.CreateAccount(context.Background(), 10, map[string]any{
		"name":        "account",
		"platform":    PlatformAnthropic,
		"type":        AccountTypeAPIKey,
		"credentials": map[string]any{"api_key": "test-key"},
	})
	if err == nil {
		t.Fatal("expected outbox failure to roll back account creation")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet SQL expectations: %v", err)
	}
}

func TestRefreshAccountRollsBackWhenSchedulerOutboxFails(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("create sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()

	mock.ExpectQuery(`SELECT EXISTS \(SELECT 1 FROM accounts`).
		WithArgs(int64(55), int64(10)).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectBegin()
	mock.ExpectExec(`(?s)UPDATE accounts.*rate_limit_reset_at`).
		WithArgs(int64(55), int64(10)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO scheduler_outbox`).
		WithArgs(SchedulerOutboxEventAccountChanged, int64(55), nil, nil).
		WillReturnError(sql.ErrConnDone)
	mock.ExpectRollback()

	svc := NewUserResourceService(db, nil, nil, nil)
	if _, err := svc.RefreshAccount(context.Background(), 10, 55); err == nil {
		t.Fatal("expected outbox failure to roll back account refresh")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet SQL expectations: %v", err)
	}
}
