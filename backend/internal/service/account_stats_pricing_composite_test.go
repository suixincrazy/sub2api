//go:build unit

package service

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestResolveAccountStatsCost_CompositePlatformIsolation(t *testing.T) {
	channel := &Channel{
		ID: 2, Status: StatusActive, ApplyPricingToAccountStats: true,
		AccountStatsPricingRules: []AccountStatsPricingRule{{
			AccountIDs: []int64{6},
			Pricing: []ChannelModelPricing{
				{Platform: PlatformAnthropic, Models: []string{"shared-model"}, InputPrice: testPtrFloat64(0.000020)},
				{Platform: PlatformOpenAI, Models: []string{"shared-model"}, InputPrice: testPtrFloat64(0.000004)},
			},
		}},
	}
	for _, tc := range []struct {
		platform string
		want     float64
	}{
		{PlatformOpenAI, 0.004},
		{PlatformAnthropic, 0.020},
		{PlatformAntigravity, 1},
		{"", 1},
	} {
		t.Run(tc.platform, func(t *testing.T) {
			cs := newTestChannelServiceForStats(t, channel, 2, PlatformComposite)
			ctx := WithResolvedTargetPlatform(context.Background(), tc.platform)
			cost := resolveAccountStatsCost(ctx, cs, nil, 6, 2, "shared-model",
				UsageTokens{InputTokens: 1000}, 1, 1, "", time.Time{}, false, 1)
			require.NotNil(t, cost)
			require.InDelta(t, tc.want, *cost, 1e-12)
		})
	}
}

func TestOpenAIGatewayServiceRecordUsage_CompositeAccountStats(t *testing.T) {
	for _, accountID := range []int64{2, 6, 12, 13, 15} {
		t.Run(fmt.Sprint(accountID), func(t *testing.T) {
			inputPrice, outputPrice := 0.000002, 0.000010
			if accountID == 6 || accountID == 13 {
				inputPrice, outputPrice = 0.000004, 0.000020
			}
			channel := &Channel{
				ID: 2, Status: StatusActive, ApplyPricingToAccountStats: true,
				AccountStatsPricingRules: []AccountStatsPricingRule{{
					AccountIDs: []int64{accountID},
					Pricing: []ChannelModelPricing{{
						Platform: PlatformOpenAI, Models: []string{"gpt-6-astra"},
						InputPrice: &inputPrice, OutputPrice: &outputPrice,
						CacheWritePrice: &inputPrice, CacheReadPrice: testPtrFloat64(0.0000004),
					}},
				}},
			}
			usageRepo := &openAIRecordUsageLogRepoStub{inserted: true}
			svc := newOpenAIRecordUsageServiceForTest(usageRepo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{}, nil)
			svc.channelService = newTestChannelServiceForStats(t, channel, 2, PlatformComposite)
			svc.billingService = newTestBillingServiceWithPrices(map[string]*ModelPricing{
				"gpt-6-astra": {InputPricePerToken: 0.000010, OutputPricePerToken: 0.000050, CacheCreationPricePerToken: 0.0000125, CacheReadPricePerToken: 0.000001},
			})
			svc.resolver = NewModelPricingResolver(svc.channelService, svc.billingService)
			err := svc.RecordUsage(context.Background(), &OpenAIRecordUsageInput{
				Result: &OpenAIForwardResult{
					RequestID: fmt.Sprintf("composite_cost_%d", accountID), Model: "gpt-6-astra", UpstreamModel: "gpt-6-astra", Duration: time.Second,
					Usage: OpenAIUsage{InputTokens: 223959, OutputTokens: 2900, CacheCreationInputTokens: 2282, CacheReadInputTokens: 221674},
				},
				APIKey: &APIKey{ID: 100, GroupID: i64p(2), Group: &Group{ID: 2, Platform: PlatformComposite, RateMultiplier: 1}},
				User:   &User{ID: 1}, Account: &Account{ID: accountID, Platform: PlatformOpenAI},
			})
			require.NoError(t, err)
			require.NotNil(t, usageRepo.lastLog)
			require.InDelta(t, 0.395229, usageRepo.lastLog.TotalCost, 1e-10)
			require.InDelta(t, 0.395229, usageRepo.lastLog.ActualCost, 1e-10)
			require.NotNil(t, usageRepo.lastLog.AccountStatsCost)
			want := 2285*inputPrice + 2900*outputPrice + 221674*0.0000004
			require.InDelta(t, want, *usageRepo.lastLog.AccountStatsCost, 1e-10)
		})
	}
}

func TestGatewayServiceRecordUsage_CompositeAnthropicAccountStats(t *testing.T) {
	for _, accountID := range []int64{8, 14} {
		for _, hourlyPrice := range []float64{0.000005, 0.000010} {
			t.Run(fmt.Sprintf("%d/hourly=%g", accountID, hourlyPrice), func(t *testing.T) {
				channel := &Channel{
					ID: 2, Status: StatusActive, ApplyPricingToAccountStats: true,
					AccountStatsPricingRules: []AccountStatsPricingRule{{
						AccountIDs: []int64{8, 14},
						Pricing: []ChannelModelPricing{{
							Platform: PlatformAnthropic, Models: []string{"claude-opus-5-5"},
							InputPrice: testPtrFloat64(0.000004), OutputPrice: testPtrFloat64(0.000020),
							CacheWritePrice: testPtrFloat64(0.000005), CacheWrite1hPrice: &hourlyPrice, CacheReadPrice: testPtrFloat64(0.0000004),
						}},
					}},
				}
				usageRepo := &openAIRecordUsageLogRepoStub{inserted: true}
				svc := newGatewayRecordUsageServiceForTest(usageRepo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{})
				svc.channelService = newTestChannelServiceForStats(t, channel, 2, PlatformComposite)
				input := &RecordUsageInput{
					Result: &ForwardResult{
						RequestID: fmt.Sprintf("anthropic_cost_%d_%g", accountID, hourlyPrice), Model: "claude-opus-4-8", UpstreamModel: "claude-opus-5-5", Duration: time.Second,
						Usage: ClaudeUsage{InputTokens: 100, OutputTokens: 50, CacheCreationInputTokens: 30, CacheCreation5mTokens: 10, CacheCreation1hTokens: 20, CacheReadInputTokens: 200},
					},
					APIKey: &APIKey{ID: 100, GroupID: i64p(2), Group: &Group{ID: 2, Platform: PlatformComposite, RateMultiplier: 1}},
					User:   &User{ID: 1}, Account: &Account{ID: accountID, Platform: PlatformAnthropic},
				}
				customerCost := svc.QuoteUsageCost(context.Background(), input)
				err := svc.RecordUsage(context.Background(), input)
				require.NoError(t, err)
				require.NotNil(t, usageRepo.lastLog)
				require.InDelta(t, customerCost, usageRepo.lastLog.ActualCost, 1e-12)
				require.NotNil(t, usageRepo.lastLog.AccountStatsCost)
				want := 100*0.000004 + 50*0.000020 + 10*0.000005 + 20*hourlyPrice + 200*0.0000004
				require.InDelta(t, want, *usageRepo.lastLog.AccountStatsCost, 1e-12)
			})
		}
	}
}
