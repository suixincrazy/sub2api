//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestInflightEstimate_ForcedUltrafastMappedModels(t *testing.T) {
	for _, accountMapping := range []bool{false, true} {
		t.Run(map[bool]string{false: "channel", true: "account"}[accountMapping], func(t *testing.T) {
			group := &Group{ID: 79, Hydrated: true, Status: StatusActive, Platform: PlatformOpenAI, RateMultiplier: 1, ForceOpenAIUltrafast: true}
			channel := Channel{ID: 1, Status: StatusActive, GroupIDs: []int64{group.ID}}
			if !accountMapping {
				channel.ModelMapping = map[string]map[string]string{PlatformOpenAI: {"gateway-sol": "gpt-6.1-sol"}}
			}
			cs := newTestChannelService(makeStandardRepo(channel, map[int64]string{group.ID: PlatformOpenAI}))
			base := newInflightEstimateGateway(t, cs)
			if accountMapping {
				attachInflightSnapshot(base, &inflightSnapshotCacheStub{byBucket: map[string][]Account{inflightBucketKey(group.ID, PlatformOpenAI): {
					{ID: 1, Platform: PlatformOpenAI, Status: StatusActive, Schedulable: true, Credentials: map[string]any{"model_mapping": map[string]any{"gateway-sol": "gpt-6.1-sol"}}},
				}}})
			}
			svc := &OpenAIGatewayService{cfg: base.cfg, billingService: base.billingService, resolver: base.resolver, channelService: cs, schedulerSnapshot: base.schedulerSnapshot}
			key := &APIKey{User: &User{ID: 1}, GroupID: &group.ID, Group: group}
			estimate, priced := svc.EstimateInflightReservation(context.Background(), key, InflightEstimateRequest{Model: "gateway-sol", BodyBytes: 4000, MaxTokens: 1000})
			require.True(t, priced)
			want, err := base.billingService.CalculateCostWithServiceTier("gpt-6.1-sol", UsageTokens{InputTokens: 1000, OutputTokens: 1000}, 1, "ultrafast")
			require.NoError(t, err)
			require.InDelta(t, want.ActualCost, estimate, 1e-12)
		})
	}
}

func TestInflightEstimate_VertexEndpointPrices(t *testing.T) {
	for _, tc := range []struct {
		name, location, cacheLocation, model, accountType string
		configured                                        bool
		factor                                            float64
	}{
		{name: "us default", location: "us", factor: 1.1},
		{name: "eu default", location: "eu", factor: 1.1},
		{name: "global default", location: "global", factor: 1},
		{name: "regional default", location: "us-central1", factor: 1},
		{name: "unsupported model", location: "us", model: "gemini-3.1-pro-preview", factor: 1},
		{name: "configured final price", location: "us", configured: true, factor: 1},
		{name: "ai studio", location: "us", accountType: AccountTypeAPIKey, factor: 1},
		{name: "cache routes to us", location: "global", cacheLocation: "us", factor: 1.1},
		{name: "cache routes to global", location: "us", cacheLocation: "global", factor: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := tc.model
			if model == "" {
				model = "gemini-3.7-flash"
			}
			accountType := tc.accountType
			if accountType == "" {
				accountType = AccountTypeServiceAccount
			}
			group := &Group{ID: 80, Platform: PlatformGemini, RateMultiplier: 1}
			if tc.configured {
				in, out := 1e-6, 2e-6
				group.ModelPricing = []ChannelModelPricing{{Models: []string{model}, InputPrice: &in, OutputPrice: &out}}
			}
			svc := newInflightEstimateGateway(t, nil)
			attachInflightSnapshot(svc, &inflightSnapshotCacheStub{byBucket: map[string][]Account{inflightBucketKey(group.ID, PlatformGemini): {
				{ID: 1, Platform: PlatformGemini, Type: accountType, Status: StatusActive, Schedulable: true, Credentials: map[string]any{"project_id": "test-project", "location": tc.location}},
			}}})
			key := &APIKey{User: &User{ID: 1}, GroupID: &group.ID, Group: group}
			req := InflightEstimateRequest{Model: model, BodyBytes: 4000, MaxTokens: 1000, CachedContentLocation: tc.cacheLocation}
			estimate, priced := svc.EstimateInflightReservation(context.Background(), key, req)
			require.True(t, priced)
			want, err := svc.billingService.CalculateCostUnified(CostInput{Ctx: context.Background(), Model: model, GroupID: key.GroupID, Group: group, Tokens: UsageTokens{InputTokens: 1000, OutputTokens: 1000}, RateMultiplier: 1, Resolver: svc.resolver})
			require.NoError(t, err)
			require.InDelta(t, want.ActualCost*tc.factor, estimate, 1e-12)
		})
	}
}
