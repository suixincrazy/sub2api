package service

import (
	"context"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

type grokTestNetworkPolicyRecorder struct {
	request *http.Request
}

func (r *grokTestNetworkPolicyRecorder) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	r.request = req
	return nil, context.Canceled
}

func (r *grokTestNetworkPolicyRecorder) DoWithTLS(req *http.Request, proxyURL string, accountID int64, accountConcurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return r.Do(req, proxyURL, accountID, accountConcurrency)
}

func TestAccountTestGrokUpstreamAppliesOwnershipNetworkPolicy(t *testing.T) {
	for _, tc := range []struct {
		name      string
		userOwned bool
	}{
		{name: "user_owned", userOwned: true},
		{name: "system", userOwned: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := &Account{ID: 1001, Concurrency: 1}
			if tc.userOwned {
				ownerID := int64(42)
				account.OwnerUserID = &ownerID
			}
			recorder := &grokTestNetworkPolicyRecorder{}
			service := &AccountTestService{httpUpstream: recorder}
			req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://example.com", nil)
			require.NoError(t, err)

			_, err = service.doGrokTestUpstream(req, account)

			require.ErrorIs(t, err, context.Canceled)
			require.NotNil(t, recorder.request)
			policy := HTTPUpstreamNetworkPolicyFromContext(recorder.request.Context())
			require.Equal(t, tc.userOwned, policy.PublicOnly)
		})
	}
}
