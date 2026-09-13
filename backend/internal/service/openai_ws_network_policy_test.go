package service

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

type openAIWSNetworkPolicyCaptureDialer struct {
	policies []HTTPUpstreamNetworkPolicy
}

func (d *openAIWSNetworkPolicyCaptureDialer) Dial(
	ctx context.Context,
	_ string,
	_ http.Header,
	_ string,
) (openAIWSClientConn, int, http.Header, error) {
	d.policies = append(d.policies, HTTPUpstreamNetworkPolicyFromContext(ctx))
	return &openAIWSFakeConn{}, 0, nil, nil
}

func TestOpenAIWSConnPoolDialConnAppliesOwnerNetworkPolicy(t *testing.T) {
	dialer := &openAIWSNetworkPolicyCaptureDialer{}
	pool := &openAIWSConnPool{clientDialer: dialer}
	ownerID := int64(7)
	account := &Account{
		ID:          11,
		OwnerUserID: &ownerID,
		Proxy:       &Proxy{Kind: "xray"},
	}
	proxyURL := "socks5://127.0.0.1:32123"

	conn, err := pool.dialConn(context.Background(), openAIWSAcquireRequest{
		Account:  account,
		WSURL:    "wss://api.openai.com/v1/responses",
		ProxyURL: proxyURL,
	})
	require.NoError(t, err)
	defer conn.close()
	require.Len(t, dialer.policies, 1)
	require.True(t, dialer.policies[0].PublicOnly)
	require.Equal(t, []string{"127.0.0.1:32123"}, dialer.policies[0].AllowedDialAddresses)
}

func TestOpenAIWSConnPoolDialConnLeavesSystemAccountUnrestricted(t *testing.T) {
	dialer := &openAIWSNetworkPolicyCaptureDialer{}
	pool := &openAIWSConnPool{clientDialer: dialer}

	conn, err := pool.dialConn(context.Background(), openAIWSAcquireRequest{
		Account: &Account{ID: 12},
		WSURL:   "wss://api.openai.com/v1/responses",
	})
	require.NoError(t, err)
	defer conn.close()
	require.Len(t, dialer.policies, 1)
	require.False(t, dialer.policies[0].PublicOnly)
}
