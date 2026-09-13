//go:build e2e

package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

type userResourceE2EUser struct {
	Email    string
	Password string
	Token    string
	ID       int64
}

type userResourceE2EPage struct {
	Items    []map[string]any `json:"items"`
	Total    int64            `json:"total"`
	Page     int              `json:"page"`
	PageSize int              `json:"page_size"`
	Pages    int              `json:"pages"`
}

func TestUserResourcesFullFlow(t *testing.T) {
	if !strings.EqualFold(os.Getenv("USER_RESOURCE_E2E"), "true") {
		t.Skip("set USER_RESOURCE_E2E=true to run the user-resource black-box flow")
	}

	userA := userResourceE2ERequireUser(t, "USER_RESOURCE_E2E_A")
	userB := userResourceE2ERequireUser(t, "USER_RESOURCE_E2E_B")
	userA.Token, userA.ID = userResourceE2ELogin(t, userA)
	userB.Token, userB.ID = userResourceE2ELogin(t, userB)
	if userA.ID == userB.ID {
		t.Fatal("test users must be different accounts")
	}

	var feature struct {
		Enabled bool `json:"enabled"`
	}
	userResourceE2EJSON(t, http.MethodGet, "/api/v1/my/feature-status", userA.Token, nil, &feature, http.StatusOK)
	if !feature.Enabled {
		t.Fatal("enable_user_resources is disabled")
	}

	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	group := userResourceE2ECreateMap(t, userA.Token, "/api/v1/my/groups", map[string]any{
		"name":                  "e2e-user-group-" + suffix,
		"description":           "user resource e2e",
		"platform":              "anthropic",
		"subscription_type":     "subscription",
		"status":                "active",
		"default_validity_days": 1,
		"daily_limit_usd":       1,
		"weekly_limit_usd":      5,
		"monthly_limit_usd":     10,
		"rate_multiplier":       1,
	})
	groupID := userResourceE2EInt64(group["id"])
	if groupID <= 0 {
		t.Fatalf("created group has no id: %#v", group)
	}

	proxy := userResourceE2ECreateMap(t, userA.Token, "/api/v1/my/proxies", map[string]any{
		"name":     "e2e-user-proxy-" + suffix,
		"kind":     "standard",
		"protocol": "http",
		"host":     "203.0.113.10",
		"port":     18080,
		"username": "e2e-user",
		"password": "e2e-private-proxy-password",
		"status":   "active",
	})
	proxyID := userResourceE2EInt64(proxy["id"])
	if proxyID <= 0 {
		t.Fatalf("created proxy has no id: %#v", proxy)
	}

	account := userResourceE2ECreateMap(t, userA.Token, "/api/v1/my/accounts", map[string]any{
		"name":        "e2e-user-account-" + suffix,
		"platform":    "anthropic",
		"type":        "api_key",
		"proxy_id":    proxyID,
		"group_ids":   []int64{groupID},
		"concurrency": 1,
		"priority":    50,
		"credentials": map[string]any{
			"api_key":       "e2e-secret-api-key",
			"refresh_token": "e2e-secret-refresh-token",
		},
	})
	accountID := userResourceE2EInt64(account["id"])
	if accountID <= 0 {
		t.Fatalf("created account has no id: %#v", account)
	}
	userResourceE2EAssertAccountRedacted(t, account)

	var accountDetail map[string]any
	userResourceE2EJSON(t, http.MethodGet, fmt.Sprintf("/api/v1/my/accounts/%d", accountID), userA.Token, nil, &accountDetail, http.StatusOK)
	userResourceE2EAssertAccountRedacted(t, accountDetail)

	userResourceE2EExpectStatus(t, http.MethodGet, fmt.Sprintf("/api/v1/my/groups/%d", groupID), userB.Token, nil, http.StatusForbidden, http.StatusNotFound)
	userResourceE2EExpectStatus(t, http.MethodGet, fmt.Sprintf("/api/v1/my/accounts/%d", accountID), userB.Token, nil, http.StatusForbidden, http.StatusNotFound)
	userResourceE2EExpectStatus(t, http.MethodGet, fmt.Sprintf("/api/v1/my/proxies/%d", proxyID), userB.Token, nil, http.StatusForbidden, http.StatusNotFound)
	userResourceE2EExpectStatus(t, http.MethodPost, "/api/v1/my/accounts", userB.Token, map[string]any{
		"name":      "e2e-cross-bind-" + suffix,
		"platform":  "anthropic",
		"type":      "api_key",
		"group_ids": []int64{groupID},
	}, http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound)

	assigned := userResourceE2ECreateMap(t, userA.Token, "/api/v1/my/assigned-subscriptions", map[string]any{
		"email":         userB.Email,
		"group_id":      groupID,
		"validity_days": 1,
		"notes":         "e2e manual assign",
	})
	assignedID := userResourceE2EInt64(assigned["id"])
	if assignedID <= 0 {
		t.Fatalf("assigned subscription has no id: %#v", assigned)
	}
	if got := userResourceE2EInt64(assigned["managed_by_user_id"]); got != userA.ID {
		t.Fatalf("assigned subscription manager mismatch: got %d want %d", got, userA.ID)
	}

	subscription := userResourceE2EFindUserSubscription(t, userB.Token, groupID)
	subscriptionID := userResourceE2EInt64(subscription["id"])
	if subscriptionID <= 0 {
		t.Fatalf("subscription has no id: %#v", subscription)
	}
	userResourceE2EAssertPoolHealth(t, subscription, groupID)

	codes := []map[string]any{}
	userResourceE2EJSON(t, http.MethodPost, "/api/v1/my/redeem-codes", userA.Token, map[string]any{
		"group_id":      groupID,
		"validity_days": 1,
		"count":         1,
		"notes":         "e2e redeem",
	}, &codes, http.StatusCreated)
	if len(codes) != 1 {
		t.Fatalf("expected one redeem code, got %d", len(codes))
	}
	code := strings.TrimSpace(userResourceE2EString(codes[0]["code"]))
	if code == "" {
		t.Fatalf("generated redeem code is empty: %#v", codes[0])
	}

	var redeemed map[string]any
	userResourceE2EJSON(t, http.MethodPost, "/api/v1/redeem", userB.Token, map[string]any{"code": code}, &redeemed, http.StatusOK)
	if userResourceE2EString(redeemed["type"]) != "subscription" {
		t.Fatalf("redeemed code type mismatch: %#v", redeemed)
	}

	assignedPage := userResourceE2EAssignedPage(t, userA.Token, groupID, "")
	redeemManaged := false
	for _, item := range assignedPage.Items {
		if userResourceE2EInt64(item["user_id"]) != userB.ID || userResourceE2EInt64(item["group_id"]) != groupID {
			continue
		}
		if userResourceE2EString(item["source_type"]) == "redeem_code" || userResourceE2EInt64(item["source_redeem_code_id"]) > 0 {
			redeemManaged = true
			break
		}
	}
	if !redeemManaged {
		t.Fatalf("creator cannot see redeem-code attribution in assigned subscriptions: %#v", assignedPage.Items)
	}

	var unsubscribeResult map[string]any
	userResourceE2EJSON(t, http.MethodPost, fmt.Sprintf("/api/v1/subscriptions/%d/unsubscribe", subscriptionID), userB.Token, nil, &unsubscribeResult, http.StatusOK)

	activeSubs := []map[string]any{}
	userResourceE2EJSON(t, http.MethodGet, "/api/v1/subscriptions/active", userB.Token, nil, &activeSubs, http.StatusOK)
	for _, item := range activeSubs {
		if userResourceE2EInt64(item["id"]) == subscriptionID || userResourceE2EInt64(item["group_id"]) == groupID {
			t.Fatalf("unsubscribed subscription is still active: %#v", item)
		}
	}

	revokedPage := userResourceE2EAssignedPage(t, userA.Token, groupID, "revoked")
	foundRevoked := false
	for _, item := range revokedPage.Items {
		if userResourceE2EInt64(item["id"]) == subscriptionID {
			foundRevoked = true
			break
		}
	}
	if !foundRevoked {
		t.Fatalf("creator cannot see unsubscribed subscription as revoked: %#v", revokedPage.Items)
	}
}

func userResourceE2ERequireUser(t *testing.T, prefix string) userResourceE2EUser {
	t.Helper()
	email := strings.TrimSpace(os.Getenv(prefix + "_EMAIL"))
	password := os.Getenv(prefix + "_PASSWORD")
	if email == "" || password == "" {
		t.Skipf("set %s_EMAIL and %s_PASSWORD to run user-resource e2e", prefix, prefix)
	}
	return userResourceE2EUser{Email: email, Password: password}
}

func userResourceE2ELogin(t *testing.T, user userResourceE2EUser) (string, int64) {
	t.Helper()
	var out struct {
		AccessToken string `json:"access_token"`
		User        struct {
			ID int64 `json:"id"`
		} `json:"user"`
	}
	userResourceE2EJSON(t, http.MethodPost, "/api/v1/auth/login", "", map[string]any{
		"email":    user.Email,
		"password": user.Password,
	}, &out, http.StatusOK)
	if out.AccessToken == "" {
		t.Fatal("login response did not include access_token")
	}
	if out.User.ID <= 0 {
		t.Fatal("login response did not include user.id")
	}
	return out.AccessToken, out.User.ID
}

func userResourceE2ECreateMap(t *testing.T, token, path string, payload map[string]any) map[string]any {
	t.Helper()
	var out map[string]any
	userResourceE2EJSON(t, http.MethodPost, path, token, payload, &out, http.StatusCreated)
	return out
}

func userResourceE2EAssignedPage(t *testing.T, token string, groupID int64, status string) userResourceE2EPage {
	t.Helper()
	path := fmt.Sprintf("/api/v1/my/assigned-subscriptions?group_id=%d&page_size=100", groupID)
	if status != "" {
		path += "&status=" + status
	}
	var page userResourceE2EPage
	userResourceE2EJSON(t, http.MethodGet, path, token, nil, &page, http.StatusOK)
	return page
}

func userResourceE2EFindUserSubscription(t *testing.T, token string, groupID int64) map[string]any {
	t.Helper()
	var subs []map[string]any
	userResourceE2EJSON(t, http.MethodGet, "/api/v1/subscriptions", token, nil, &subs, http.StatusOK)
	for _, sub := range subs {
		if userResourceE2EInt64(sub["group_id"]) == groupID {
			return sub
		}
	}
	t.Fatalf("subscription for group %d was not returned: %#v", groupID, subs)
	return nil
}

func userResourceE2EAssertAccountRedacted(t *testing.T, item map[string]any) {
	t.Helper()
	if redacted, ok := item["credentials_redacted"].(bool); !ok || !redacted {
		t.Fatalf("account credentials_redacted flag missing: %#v", item)
	}
	if has, ok := item["has_credentials"].(bool); !ok || !has {
		t.Fatalf("account has_credentials flag missing: %#v", item)
	}
	if creds, ok := item["credentials"].(map[string]any); !ok || len(creds) != 0 {
		t.Fatalf("account credentials were not redacted: %#v", item["credentials"])
	}
	raw, _ := json.Marshal(item)
	if strings.Contains(string(raw), "e2e-secret") {
		t.Fatal("account response leaked secret credential content")
	}
}

func userResourceE2EAssertPoolHealth(t *testing.T, sub map[string]any, groupID int64) {
	t.Helper()
	health, ok := sub["pool_health"].(map[string]any)
	if !ok {
		t.Fatalf("subscription for group %d does not include pool_health: %#v", groupID, sub)
	}
	if got := userResourceE2EInt64(health["group_id"]); got != groupID {
		t.Fatalf("pool_health group mismatch: got %d want %d", got, groupID)
	}
	if total := userResourceE2EInt64(health["total"]); total < 1 {
		t.Fatalf("pool_health total should include the created account: %#v", health)
	}
}

func userResourceE2EExpectStatus(t *testing.T, method, path, token string, payload any, statuses ...int) {
	t.Helper()
	status, _ := userResourceE2ERequest(t, method, path, token, payload)
	for _, want := range statuses {
		if status == want {
			return
		}
	}
	t.Fatalf("%s %s returned HTTP %d, want one of %v", method, path, status, statuses)
}

func userResourceE2EJSON(t *testing.T, method, path, token string, payload any, out any, statuses ...int) {
	t.Helper()
	status, body := userResourceE2ERequest(t, method, path, token, payload)
	ok := false
	for _, want := range statuses {
		if status == want {
			ok = true
			break
		}
	}
	if !ok {
		t.Fatalf("%s %s returned HTTP %d: %s", method, path, status, userResourceE2ESafeBody(body))
	}
	var env struct {
		Code    int             `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode response envelope: %v; body=%s", err, userResourceE2ESafeBody(body))
	}
	if env.Code != 0 {
		t.Fatalf("response code is %d: %s", env.Code, env.Message)
	}
	if out == nil {
		return
	}
	if len(env.Data) == 0 || string(env.Data) == "null" {
		t.Fatalf("response did not include data: %s", userResourceE2ESafeBody(body))
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		t.Fatalf("decode response data: %v; data=%s", err, userResourceE2ESafeBody(env.Data))
	}
}

func userResourceE2ERequest(t *testing.T, method, path, token string, payload any) (int, []byte) {
	t.Helper()
	var reader io.Reader
	if payload != nil {
		body, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal request payload: %v", err)
		}
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, baseURL+path, reader)
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("%s %s request failed: %v", method, path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	return resp.StatusCode, body
}

func userResourceE2EInt64(v any) int64 {
	switch t := v.(type) {
	case int64:
		return t
	case int:
		return int64(t)
	case float64:
		return int64(t)
	case json.Number:
		i, _ := t.Int64()
		return i
	case string:
		i, _ := strconv.ParseInt(t, 10, 64)
		return i
	default:
		return 0
	}
}

func userResourceE2EString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

func userResourceE2ESafeBody(body []byte) string {
	text := string(body)
	for _, marker := range []string{"access_token", "refresh_token", "api_key", "password", "authorization"} {
		text = strings.ReplaceAll(text, marker, marker[:1]+"***")
	}
	if len(text) > 2000 {
		return text[:2000] + "...<truncated>"
	}
	return text
}
