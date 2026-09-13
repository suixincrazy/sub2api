package service

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type outboundNetworkPolicyRecorder struct {
	lastReq *http.Request
}

func (r *outboundNetworkPolicyRecorder) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	r.lastReq = req
	return nil, context.Canceled
}

func (r *outboundNetworkPolicyRecorder) DoWithTLS(req *http.Request, proxyURL string, accountID int64, accountConcurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return r.Do(req, proxyURL, accountID, accountConcurrency)
}

func requireOwnershipNetworkPolicy(
	t *testing.T,
	newAccount func() *Account,
	invoke func(*Account, HTTPUpstream) error,
) {
	t.Helper()
	for _, tc := range []struct {
		name      string
		userOwned bool
	}{
		{name: "user_owned", userOwned: true},
		{name: "system", userOwned: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := newAccount()
			if tc.userOwned {
				ownerID := int64(42)
				account.OwnerUserID = &ownerID
			}
			upstream := &outboundNetworkPolicyRecorder{}
			require.Error(t, invoke(account, upstream))
			require.NotNil(t, upstream.lastReq)
			policy := HTTPUpstreamNetworkPolicyFromContext(upstream.lastReq.Context())
			require.Equal(t, tc.userOwned, policy.PublicOnly)
		})
	}
}

func newOutboundPolicyGinContext(path string, body []byte) *gin.Context {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	return c
}

func newOpenAIInputTokensPolicyAccount() *Account {
	return &Account{
		ID:          1001,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{"api_key": "sk-test"},
		Status:      StatusActive,
		Schedulable: true,
	}
}

func TestOpenAICountTokensOutboundRequestsApplyOwnershipNetworkPolicy(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("responses_input_tokens", func(t *testing.T) {
		body := []byte(`{"model":"gpt-5","input":"hello"}`)
		requireOwnershipNetworkPolicy(t, newOpenAIInputTokensPolicyAccount, func(account *Account, upstream HTTPUpstream) error {
			svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
			return svc.ForwardResponsesInputTokens(
				context.Background(),
				newOutboundPolicyGinContext("/v1/responses/input_tokens", body),
				account,
				body,
			)
		})
	})

	t.Run("anthropic_count_tokens_bridge", func(t *testing.T) {
		body := []byte(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hello"}]}`)
		requireOwnershipNetworkPolicy(t, newOpenAIInputTokensPolicyAccount, func(account *Account, upstream HTTPUpstream) error {
			svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
			return svc.ForwardCountTokensAsAnthropic(
				context.Background(),
				newOutboundPolicyGinContext("/v1/messages/count_tokens", body),
				account,
				body,
				"gpt-5",
			)
		})
	})
}

func newNativeAnthropicPolicyAccount() *Account {
	return &Account{
		ID:          1002,
		Platform:    PlatformKimi,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{
			"account_mode": AccountModePayG,
			"api_key":      "sk-test",
			"api_protocol": APIProtocolAnthropic,
			"base_url":     "https://api.moonshot.cn/anthropic",
		},
		Status:      StatusActive,
		Schedulable: true,
	}
}

func TestNativeAnthropicOutboundRequestsApplyOwnershipNetworkPolicy(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name   string
		path   string
		body   []byte
		invoke func(*OpenAIGatewayService, context.Context, *gin.Context, *Account, []byte) error
	}{
		{
			name: "messages",
			path: "/v1/messages",
			body: []byte(`{"model":"claude-sonnet-4-5","max_tokens":16,"messages":[{"role":"user","content":"hello"}],"stream":false}`),
			invoke: func(svc *OpenAIGatewayService, ctx context.Context, c *gin.Context, account *Account, body []byte) error {
				_, err := svc.forwardAnthropicViaNativeAnthropicEndpoint(ctx, c, account, body, "")
				return err
			},
		},
		{
			name: "chat_completions",
			path: "/v1/chat/completions",
			body: []byte(`{"model":"gpt-5","messages":[{"role":"user","content":"hello"}],"stream":false}`),
			invoke: func(svc *OpenAIGatewayService, ctx context.Context, c *gin.Context, account *Account, body []byte) error {
				_, err := svc.forwardChatCompletionsViaNativeAnthropic(ctx, c, account, body, "")
				return err
			},
		},
		{
			name: "responses",
			path: "/v1/responses",
			body: []byte(`{"model":"gpt-5","input":[{"role":"user","content":[{"type":"input_text","text":"hello"}]}],"stream":false}`),
			invoke: func(svc *OpenAIGatewayService, ctx context.Context, c *gin.Context, account *Account, body []byte) error {
				_, err := svc.forwardResponsesViaNativeAnthropic(ctx, c, account, body, "")
				return err
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			requireOwnershipNetworkPolicy(t, newNativeAnthropicPolicyAccount, func(account *Account, upstream HTTPUpstream) error {
				svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
				return tc.invoke(svc, context.Background(), newOutboundPolicyGinContext(tc.path, tc.body), account, tc.body)
			})
		})
	}
}

func TestCNProviderProbeOutboundRequestsApplyOwnershipNetworkPolicy(t *testing.T) {
	t.Run("balance", func(t *testing.T) {
		requireOwnershipNetworkPolicy(t, func() *Account {
			return &Account{
				ID:          1003,
				Platform:    PlatformDeepseek,
				Type:        AccountTypeAPIKey,
				Concurrency: 1,
				Credentials: map[string]any{"account_mode": AccountModePayG, "api_key": "sk-test"},
				Status:      StatusActive,
			}
		}, func(account *Account, upstream HTTPUpstream) error {
			svc := NewCNProviderBalanceService(nil, nil, upstream, nil)
			_, err := svc.queryBalanceForAccount(context.Background(), account)
			return err
		})
	})

	t.Run("quota", func(t *testing.T) {
		requireOwnershipNetworkPolicy(t, func() *Account {
			return &Account{
				ID:          1004,
				Platform:    PlatformKimi,
				Type:        AccountTypeAPIKey,
				Concurrency: 1,
				Credentials: map[string]any{"account_mode": AccountModeCoding, "api_key": "sk-test"},
				Status:      StatusActive,
			}
		}, func(account *Account, upstream HTTPUpstream) error {
			svc := NewCNProviderQuotaService(nil, nil, upstream, nil)
			_, err := svc.queryUsageForAccount(context.Background(), account)
			return err
		})
	})
}
