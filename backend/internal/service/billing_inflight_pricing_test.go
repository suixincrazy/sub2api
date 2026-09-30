//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestInflightEstimate_UsesBillingTokenPricing(t *testing.T) {
	for _, tc := range []struct {
		name  string
		model string
		tier  string
	}{
		{"astra ultrafast", "gpt-6-astra", "ultrafast"},
		{"sol priority", "gpt-6-sol", "priority"},
		{"sol fast alias", "gpt-6-sol", "fast"},
		{"sol flex", "gpt-6-sol", "flex"},
		{"sol standard", "gpt-6-sol", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := newInflightEstimateGateway(t, nil)
			group := &Group{ID: 71, Platform: PlatformOpenAI, RateMultiplier: 1.5}
			key := &APIKey{User: &User{ID: 1}, GroupID: &group.ID, Group: group}
			ctx := context.Background()
			estimate, priced := svc.EstimateInflightReservation(ctx, key, InflightEstimateRequest{
				Model: tc.model, BodyBytes: 4000, MaxTokens: 1000, ServiceTier: tc.tier,
			})
			require.True(t, priced)
			cost, err := svc.billingService.CalculateCostUnified(CostInput{
				Ctx: ctx, Model: tc.model, GroupID: key.GroupID, Group: group,
				Tokens:         UsageTokens{InputTokens: 1000, OutputTokens: 1000},
				RateMultiplier: group.RateMultiplier, ServiceTier: tc.tier, Resolver: svc.resolver,
			})
			require.NoError(t, err)
			require.InDelta(t, cost.ActualCost, estimate, 1e-12)
		})
	}
}

func TestInflightEstimate_UsesConfiguredTokenPricing(t *testing.T) {
	svc := newInflightEstimateGateway(t, nil)
	in, out, fast := 1e-6, 2e-6, 3.0
	group := &Group{ID: 72, Platform: PlatformOpenAI, RateMultiplier: 2,
		ModelPricing: []ChannelModelPricing{{
			Models: []string{"custom-inflight"}, BillingMode: BillingModeToken,
			InputPrice: &in, OutputPrice: &out, FastMultiplier: &fast,
			ReasoningEffortMultipliers: map[string]float64{"high": 1.5},
		}},
	}
	key := &APIKey{User: &User{ID: 1}, GroupID: &group.ID, Group: group}
	ctx := context.Background()
	req := InflightEstimateRequest{Model: "custom-inflight", BodyBytes: 4000, MaxTokens: 1000, ServiceTier: "priority", ReasoningEffort: "high"}
	estimate, priced := svc.EstimateInflightReservation(ctx, key, req)
	require.True(t, priced)
	cost, err := svc.billingService.CalculateCostUnified(CostInput{
		Ctx: ctx, Model: req.Model, GroupID: key.GroupID, Group: group,
		Tokens:         UsageTokens{InputTokens: 1000, OutputTokens: 1000},
		RateMultiplier: group.RateMultiplier, ServiceTier: req.ServiceTier,
		ReasoningEffort: req.ReasoningEffort, Resolver: svc.resolver, PricingAt: time.Now(),
	})
	require.NoError(t, err)
	require.InDelta(t, cost.ActualCost, estimate, 1e-12)
	require.InDelta(t, (1000*in+1000*out)*2*3*1.5, estimate, 1e-12)
}

func TestInflightEstimate_UsesFrozenPricingTime(t *testing.T) {
	in, out := 1e-6, 2e-6
	group := &Group{ID: 73, Platform: PlatformOpenAI, RateMultiplier: 1}
	channel := Channel{ID: 1, Status: StatusActive, GroupIDs: []int64{group.ID}, ModelPricing: []ChannelModelPricing{{
		Platform: PlatformOpenAI, Models: []string{"frozen-inflight"}, BillingMode: BillingModeToken,
		InputPrice: &in, OutputPrice: &out,
		TimePricing: &ChannelTimePricing{Timezone: "UTC", Periods: []ChannelTimePricingPeriod{{
			StartTime: "00:00", EndTime: "02:00", Multiplier: 3,
		}}},
	}}}
	cs := newTestChannelService(makeStandardRepo(channel, map[int64]string{group.ID: PlatformOpenAI}))
	svc := newInflightEstimateGateway(t, cs)
	key := &APIKey{User: &User{ID: 1}, GroupID: &group.ID, Group: group}
	at := time.Date(2026, 8, 17, 1, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		ctx  context.Context
	}{
		{"shared gateway", context.WithValue(context.Background(), gatewayTokenRequestPricingAtCtxKey{}, at)},
		{"OpenAI gateway", context.WithValue(context.Background(), openAIPricingAtCtxKey{}, at)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			estimate, priced := svc.EstimateInflightReservation(tc.ctx, key, InflightEstimateRequest{
				Model: "frozen-inflight", BodyBytes: 4000, MaxTokens: 1000,
			})
			require.True(t, priced)
			require.InDelta(t, (1000*in+1000*out)*3, estimate, 1e-12)
		})
	}
}

func TestInflightEstimate_AnthropicSpeed(t *testing.T) {
	for _, tc := range []struct {
		model    string
		platform string
		fast     bool
	}{
		{"claude-opus-5", PlatformAnthropic, true},
		{"claude-opus-4-8", PlatformAnthropic, true},
		{"claude-opus-4-7", PlatformAnthropic, false},
		{"claude-sonnet-4-5", PlatformAnthropic, false},
		{"us.anthropic.claude-opus-5-v1:0", PlatformAnthropic, false},
		{"claude-opus-5", PlatformAntigravity, false},
	} {
		t.Run(tc.model+"/"+tc.platform, func(t *testing.T) {
			svc := newInflightEstimateGateway(t, nil)
			in, out := 1e-6, 2e-6
			group := &Group{ID: 74, Platform: tc.platform, RateMultiplier: 1, ModelPricing: []ChannelModelPricing{{
				Models: []string{tc.model}, InputPrice: &in, OutputPrice: &out,
			}}}
			key := &APIKey{User: &User{ID: 1}, GroupID: &group.ID, Group: group}
			estimate, priced := svc.EstimateInflightReservation(context.Background(), key, InflightEstimateRequest{
				Model: tc.model, BodyBytes: 4000, MaxTokens: 1000, Speed: "fast",
			})
			require.True(t, priced)
			want := (1000*in + 1000*out)
			if tc.fast {
				want *= 2
			}
			require.InDelta(t, want, estimate, 1e-12)
		})
	}
}

func TestInflightEstimate_PerRequestReasoningMultiplier(t *testing.T) {
	for _, mode := range []BillingMode{BillingModePerRequest, BillingModeImage, BillingModeVideo} {
		t.Run(string(mode), func(t *testing.T) {
			svc := newInflightEstimateGateway(t, nil)
			price, tierPrice := 0.1, 0.25
			group := &Group{ID: 75, Platform: PlatformOpenAI, RateMultiplier: 2, ModelPricing: []ChannelModelPricing{{
				Models: []string{"per-request-inflight"}, BillingMode: mode, PerRequestPrice: &price,
				Intervals:                  []PricingInterval{{MinTokens: 2000, PerRequestPrice: &tierPrice}},
				ReasoningEffortMultipliers: map[string]float64{"high": 3},
			}}}
			key := &APIKey{User: &User{ID: 1}, GroupID: &group.ID, Group: group}
			estimate, priced := svc.EstimateInflightReservation(context.Background(), key, InflightEstimateRequest{
				Model: "per-request-inflight", MaxTokens: 1000, Units: 2, ReasoningEffort: "high",
			})
			require.True(t, priced)
			require.InDelta(t, tierPrice*2*2*3, estimate, 1e-12)
		})
	}
}

func TestInflightEstimate_OpenAIFastPolicies(t *testing.T) {
	for _, tc := range []struct {
		name        string
		force, free bool
		tier        string
		rules       []OpenAIFastPolicyRule
		wantTier    string
	}{
		{name: "forced fast", force: true, wantTier: "priority"},
		{name: "free forced fast", force: true, free: true},
		{name: "free priority", free: true, tier: "priority"},
		{name: "ultrafast remains billable", free: true, tier: "ultrafast", wantTier: "ultrafast"},
		{name: "policy upgrades missing", rules: []OpenAIFastPolicyRule{{ServiceTier: "missing", Scope: "all", Action: "force_priority"}}, wantTier: "priority"},
		{name: "policy upgrades flex", tier: "flex", rules: []OpenAIFastPolicyRule{{ServiceTier: "flex", Scope: "all", Action: "force_priority"}}, wantTier: "priority"},
		{name: "policy filters tier", tier: "priority", rules: []OpenAIFastPolicyRule{{ServiceTier: "priority", Scope: "all", Action: "filter"}}},
		{name: "policy filters forced tier", force: true, rules: []OpenAIFastPolicyRule{{ServiceTier: "priority", Scope: "all", Action: "filter"}}},
		{name: "conservative across credential types", rules: []OpenAIFastPolicyRule{{ServiceTier: "missing", Scope: "oauth", Action: "force_priority"}}, wantTier: "priority"},
		{name: "user rule applies", rules: []OpenAIFastPolicyRule{{ServiceTier: "missing", Scope: "all", UserIDs: []int64{1}, Action: "force_priority"}}, wantTier: "priority"},
		{name: "other user rule ignored", rules: []OpenAIFastPolicyRule{{ServiceTier: "missing", Scope: "all", UserIDs: []int64{2}, Action: "force_priority"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := newInflightEstimateGateway(t, nil)
			svc := &OpenAIGatewayService{cfg: base.cfg, billingService: base.billingService, resolver: base.resolver}
			group := &Group{ID: 76, Hydrated: true, Status: StatusActive, Platform: PlatformOpenAI, RateMultiplier: 1, ForceOpenAIFast: tc.force, FreeOpenAIFast: tc.free}
			key := &APIKey{User: &User{ID: 1}, GroupID: &group.ID, Group: group}
			ctx := withOpenAIFastPolicyContext(context.Background(), &OpenAIFastPolicySettings{Rules: tc.rules})
			estimate, priced := svc.EstimateInflightReservation(ctx, key, InflightEstimateRequest{Model: "gpt-6-astra", BodyBytes: 4000, MaxTokens: 1000, ServiceTier: tc.tier})
			require.True(t, priced)
			want, err := base.billingService.CalculateCostWithServiceTier("gpt-6-astra", UsageTokens{InputTokens: 1000, OutputTokens: 1000}, 1, tc.wantTier)
			require.NoError(t, err)
			require.InDelta(t, want.ActualCost, estimate, 1e-12)
		})
	}
}

func TestInflightEstimate_FastPolicyUsesUpstreamModelWithRequestedPricing(t *testing.T) {
	for _, accountMapping := range []bool{false, true} {
		for _, platform := range []string{PlatformOpenAI, PlatformAnthropic} {
			t.Run(platform+map[bool]string{false: "/channel", true: "/account"}[accountMapping], func(t *testing.T) {
				groupID := int64(77)
				in, out, fast := 1e-6, 2e-6, 3.0
				upstreamModel := "gpt-6-astra"
				if platform == PlatformAnthropic {
					upstreamModel = "claude-opus-5"
				}
				channel := Channel{
					ID: 1, Status: StatusActive, GroupIDs: []int64{groupID}, BillingModelSource: BillingModelSourceRequested,
				}
				if !accountMapping {
					channel.ModelMapping = map[string]map[string]string{platform: {"priced-alias": upstreamModel}}
				}
				cs := newTestChannelService(makeStandardRepo(channel, map[int64]string{groupID: platform}))
				base := newInflightEstimateGateway(t, cs)
				if accountMapping {
					attachInflightSnapshot(base, &inflightSnapshotCacheStub{byBucket: map[string][]Account{inflightBucketKey(groupID, platform): {
						{ID: 1, Platform: platform, Credentials: map[string]any{"model_mapping": map[string]any{"priced-alias": upstreamModel}}},
					}}})
				}
				group := &Group{ID: groupID, Platform: platform, RateMultiplier: 1, ModelPricing: []ChannelModelPricing{{
					Models: []string{"priced-alias"}, InputPrice: &in, OutputPrice: &out, FastMultiplier: &fast,
				}}}
				key := &APIKey{User: &User{ID: 1}, GroupID: &groupID, Group: group}
				ctx := withOpenAIFastPolicyContext(context.Background(), &OpenAIFastPolicySettings{Rules: []OpenAIFastPolicyRule{{
					ServiceTier: "missing", Scope: "all", Action: "force_priority", ModelWhitelist: []string{upstreamModel}, FallbackAction: "pass",
				}}})
				req := InflightEstimateRequest{Model: "priced-alias", BodyBytes: 4000, MaxTokens: 1000}
				var estimate float64
				var priced bool
				if platform == PlatformOpenAI {
					svc := &OpenAIGatewayService{cfg: base.cfg, billingService: base.billingService, resolver: base.resolver, channelService: cs, schedulerSnapshot: base.schedulerSnapshot}
					estimate, priced = svc.EstimateInflightReservation(ctx, key, req)
				} else {
					req.Speed = "fast"
					estimate, priced = base.EstimateInflightReservation(ctx, key, req)
				}
				require.True(t, priced)
				require.InDelta(t, (1000*in+1000*out)*fast, estimate, 1e-12)
			})
		}
	}
}

func TestInflightEstimate_FastPolicyComparesActualPrices(t *testing.T) {
	base := newInflightEstimateGateway(t, nil)
	svc := &OpenAIGatewayService{cfg: base.cfg, billingService: base.billingService, resolver: base.resolver}
	in, out, fast := 1e-6, 2e-6, 0.5
	group := &Group{ID: 78, Platform: PlatformOpenAI, RateMultiplier: 1, ModelPricing: []ChannelModelPricing{{
		Models: []string{"custom-fast-price"}, InputPrice: &in, OutputPrice: &out, FastMultiplier: &fast,
	}}}
	key := &APIKey{User: &User{ID: 1}, GroupID: &group.ID, Group: group}
	ctx := withOpenAIFastPolicyContext(context.Background(), &OpenAIFastPolicySettings{Rules: []OpenAIFastPolicyRule{{
		ServiceTier: "missing", Scope: "oauth", Action: "force_priority",
	}}})
	estimate, priced := svc.EstimateInflightReservation(ctx, key, InflightEstimateRequest{Model: "custom-fast-price", BodyBytes: 4000, MaxTokens: 1000})
	require.True(t, priced)
	require.InDelta(t, 1000*in+1000*out, estimate, 1e-12, "standard API key price exceeds the configured OAuth Fast price")
}
