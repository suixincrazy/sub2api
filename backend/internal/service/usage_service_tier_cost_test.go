//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOpenAIRecordUsageServiceTierCustomerCharge(t *testing.T) {
	const model = "gpt-6-astra"
	const responseModel = "gpt-5.5"
	for _, tc := range []struct {
		name, requested, observed string
		ultra, fast, flex         float64
		freeFast                  bool
		responseUltra             float64
		wantTier                  string
		wantTotal, wantCharge     float64
	}{
		{"reverse rates", "ultrafast", "priority", 1.25, 2.5, 0.5, false, 0, "ultrafast", 1.25, 0.625},
		{"cheaper fast", "ultrafast", "priority", 6, 2.5, 0.5, false, 0, "priority", 2.5, 1.25},
		{"free fast makes observed cheaper", "ultrafast", "priority", 1.25, 2.5, 0.5, true, 0, "priority", 2.5, 0.5},
		{"free fast makes requested cheaper", "priority", "flex", 6, 2.5, 1.5, true, 0, "priority", 2.5, 0.5},
		{"equal charge accepts observed", "ultrafast", "default", 1, 2.5, 0.5, false, 0, "default", 1, 0.5},
		{"compare both tiers on adopted response model", "ultrafast", "priority", 6, 2.5, 0.5, false, 1.25, "ultrafast", 0.625, 0.3125},
		{"keep a cheaper reported model at the observed tier", "ultrafast", "priority", 3, 2.5, 0.5, false, 10, "priority", 1.25, 0.625},
		{"free fast protects baseline charge across models", "priority", "flex", 6, 3, 2, true, 1.25, "priority", 3, 0.5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := &openAIRecordUsageLogRepoStub{inserted: true}
			users := &openAIRecordUsageUserRepoStub{}
			svc := newOpenAIRecordUsageServiceForTest(logs, users, &openAIRecordUsageSubRepoStub{}, nil)
			group := &Group{ID: 77, Platform: PlatformOpenAI, RateMultiplier: 0.5, FreeOpenAIFast: tc.freeFast}
			card := tokenPricingForModels([]string{model}, 1000)
			card.UltrafastMultiplier, card.FastMultiplier, card.FlexMultiplier = &tc.ultra, &tc.fast, &tc.flex
			cards := []ChannelModelPricing{card}
			result := &OpenAIForwardResult{Model: model, ServiceTier: &tc.requested, UpstreamResponseServiceTier: tc.observed, Usage: OpenAIUsage{InputTokens: 1000}}
			fields := ChannelUsageFields{}
			if tc.responseUltra > 0 {
				responseCard := tokenPricingForModels([]string{responseModel}, 500)
				responseCard.UltrafastMultiplier, responseCard.FastMultiplier = &tc.responseUltra, pricingMultiplier(2.5)
				if tc.freeFast {
					responseCard.InputPrice = pricingMultiplier(0.002)
					responseCard.FlexMultiplier = pricingMultiplier(0.75)
				}
				cards = append(cards, responseCard)
				result.UpstreamResponseModel = responseModel
				fields.BillingModelSource = BillingModelSourceResponse
			}
			svc.channelService = newChannelServiceWithPricings(group.ID, cards)
			svc.resolver = NewModelPricingResolver(svc.channelService, svc.billingService)
			err := svc.RecordUsage(context.Background(), &OpenAIRecordUsageInput{
				Result: result, APIKey: &APIKey{ID: 2, GroupID: &group.ID, Group: group}, User: &User{ID: 1},
				Account: &Account{ID: 3, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}, ChannelUsageFields: fields,
			})
			require.NoError(t, err)
			require.Equal(t, 1, users.deductCalls)
			require.InDelta(t, tc.wantCharge, users.lastAmount, 1e-12)
			require.NotNil(t, logs.lastLog)
			require.InDelta(t, tc.wantCharge, logs.lastLog.ActualCost, 1e-12)
			require.InDelta(t, tc.wantTotal, logs.lastLog.InputCost, 1e-12)
			require.InDelta(t, tc.wantTotal, logs.lastLog.TotalCost, 1e-12)
			require.Equal(t, tc.wantTier, optionalStringValue(logs.lastLog.ServiceTier))
		})
	}
}

func TestGatewayRecordUsageServiceTierCustomerCharge(t *testing.T) {
	const model = "claude-sonnet-4"
	for _, tc := range []struct {
		name      string
		fast      float64
		wantTier  string
		wantTotal float64
	}{
		{"discounted fast", 0.75, "fast", 0.75},
		{"cheaper standard", 2.5, "standard", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := &openAIRecordUsageLogRepoStub{inserted: true}
			users := &openAIRecordUsageUserRepoStub{}
			svc := newGatewayRecordUsageServiceForTest(logs, users, &openAIRecordUsageSubRepoStub{})
			card := ChannelModelPricing{Platform: PlatformAnthropic, Models: []string{model}, BillingMode: BillingModeToken, InputPrice: pricingMultiplier(0.001), FastMultiplier: &tc.fast}
			group := &Group{ID: 77, Platform: PlatformAnthropic, RateMultiplier: 0.5, Hydrated: true, ModelPricing: []ChannelModelPricing{card}}
			svc.resolver = NewModelPricingResolver(nil, svc.billingService)
			requested := "fast"
			err := svc.RecordUsage(context.Background(), &RecordUsageInput{
				Result: &ForwardResult{Model: model, ServiceTier: &requested, UpstreamResponseServiceTier: "standard", Usage: ClaudeUsage{InputTokens: 1000}},
				APIKey: &APIKey{ID: 2, GroupID: &group.ID, Group: group}, User: &User{ID: 1}, Account: &Account{ID: 3, Platform: PlatformAnthropic},
			})
			require.NoError(t, err)
			require.Equal(t, 1, users.deductCalls)
			require.InDelta(t, tc.wantTotal*group.RateMultiplier, users.lastAmount, 1e-12)
			require.NotNil(t, logs.lastLog)
			require.InDelta(t, users.lastAmount, logs.lastLog.ActualCost, 1e-12)
			require.InDelta(t, tc.wantTotal, logs.lastLog.InputCost, 1e-12)
			require.InDelta(t, tc.wantTotal, logs.lastLog.TotalCost, 1e-12)
			require.Equal(t, tc.wantTier, optionalStringValue(logs.lastLog.ServiceTier))
		})
	}
}
