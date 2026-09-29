package service

import (
	"context"
	"fmt"
	"strconv"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestCompositeAccountModelRouteRebindsFormerOwner(t *testing.T) {
	for _, mode := range []struct {
		name     string
		advanced bool
		weighted bool
	}{
		{name: "advanced", advanced: true},
		{name: "weighted", advanced: true, weighted: true},
		{name: "legacy"},
	} {
		for _, loadBatch := range []bool{false, true} {
			for _, profitGate := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/load_batch=%t/profit_gate=%t", mode.name, loadBatch, profitGate), func(t *testing.T) {
					resetOpenAIAdvancedSchedulerSettingCacheForTest()
					groupID := int64(1010720)
					rate := 0.3
					owner := Account{
						ID: 371051, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
						Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 10,
						GroupIDs: []int64{groupID}, RateMultiplier: &rate,
						Credentials: map[string]any{"model_mapping": map[string]any{"team-alias": "gpt-5.1"}},
					}
					formerOwner := Account{
						ID: 371052, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
						Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 0,
						GroupIDs: []int64{groupID}, RateMultiplier: &rate,
					}
					const sessionHash = "former-alias-owner"
					const cacheKey = "openai:" + sessionHash
					cache := &schedulerTestGatewayCache{sessionBindings: map[string]int64{cacheKey: formerOwner.ID}}
					cfg := &config.Config{}
					cfg.Gateway.Scheduling.LoadBatchEnabled = loadBatch
					svc := &OpenAIGatewayService{
						accountRepo:        schedulerTestOpenAIAccountRepo{accounts: []Account{owner, formerOwner}},
						cache:              cache,
						cfg:                cfg,
						rateLimitService:   newOpenAIAdvancedSchedulerRateLimitService(strconv.FormatBool(mode.advanced), strconv.FormatBool(mode.weighted)),
						concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
					}
					ctx := WithCompositeRouteDecision(context.Background(), CompositeRouteDecision{
						Matched: true, Source: CompositeRouteSourceAccount, GroupID: groupID,
						PublicModel: "team-alias", TargetPlatform: PlatformOpenAI, UpstreamModel: "team-alias",
					})
					if profitGate {
						ctx = context.WithValue(ctx, openAIProfitControlGateCtxKey{}, &openAIProfitControlGate{
							groupID: groupID, platform: PlatformOpenAI, threshold: 0.5,
						})
					}
					selection, _, err := svc.SelectAccountWithScheduler(
						ctx, &groupID, "", sessionHash, "team-alias", nil, OpenAIUpstreamTransportAny, false,
					)
					require.NoError(t, err)
					require.NotNil(t, selection)
					require.Equal(t, owner.ID, selection.Account.ID)
					if selection.ReleaseFunc != nil {
						selection.ReleaseFunc()
					}
					require.Equal(t, profitGate, selection.ProfitGateActive())
					if !profitGate {
						require.Equal(t, owner.ID, cache.sessionBindings[cacheKey], "selection must replace a deterministically invalid binding")
					}
					require.NoError(t, svc.BindStickySessionAfterProfitAdmission(
						ContextWithSelectionProfitGate(ctx, selection), &groupID, sessionHash, owner.ID,
					))
					require.Equal(t, owner.ID, cache.sessionBindings[cacheKey], "terminal admission must bind the current alias owner")
					next, decision, err := svc.SelectAccountWithScheduler(
						ctx, &groupID, "", sessionHash, "team-alias", nil, OpenAIUpstreamTransportAny, false,
					)
					require.NoError(t, err)
					require.NotNil(t, next)
					if next.ReleaseFunc != nil {
						defer next.ReleaseFunc()
					}
					require.Equal(t, owner.ID, next.Account.ID)
					require.True(t, decision.StickySessionHit, "subsequent requests must use the replacement binding")
				})
			}
		}
	}
}

func TestCompositeAccountModelRoutePreservesGuardianParentBinding(t *testing.T) {
	for _, advanced := range []string{"true", "false"} {
		t.Run("advanced="+advanced, func(t *testing.T) {
			groupID := int64(1010721)
			parentID := "66666666-6666-4666-8666-666666666666"
			parentHash := DeriveSessionHashFromSeed(parentID)
			accounts := []Account{
				{ID: 371061, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive,
					Schedulable: true, Concurrency: 1, GroupIDs: []int64{groupID}},
				{ID: 371062, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive,
					Schedulable: true, Concurrency: 1, GroupIDs: []int64{groupID},
					Credentials: map[string]any{"model_mapping": map[string]any{codexAutoReviewModel: "gpt-5.1"}}},
			}
			cacheKey := "openai:" + parentHash
			cache := &schedulerTestGatewayCache{sessionBindings: map[string]int64{cacheKey: accounts[0].ID}}
			svc := &OpenAIGatewayService{
				accountRepo:        schedulerTestOpenAIAccountRepo{accounts: accounts},
				cache:              cache,
				cfg:                &config.Config{},
				rateLimitService:   newOpenAIAdvancedSchedulerRateLimitService(advanced),
				concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
			}
			ctx := guardianAffinityTestContext(t, codexAutoReviewModel, "guardian", parentID, "")
			ctx = WithCompositeRouteDecision(ctx, CompositeRouteDecision{
				Matched: true, Source: CompositeRouteSourceAccount, GroupID: groupID,
				PublicModel: codexAutoReviewModel, TargetPlatform: PlatformOpenAI, UpstreamModel: codexAutoReviewModel,
			})
			selection, _, err := svc.SelectAccountWithScheduler(
				ctx, &groupID, "", parentHash, codexAutoReviewModel, nil, OpenAIUpstreamTransportAny, false,
			)
			require.NoError(t, err)
			require.NotNil(t, selection)
			if selection.ReleaseFunc != nil {
				defer selection.ReleaseFunc()
			}
			require.Equal(t, accounts[1].ID, selection.Account.ID)
			require.NoError(t, svc.BindStickySessionAfterProfitAdmission(ctx, &groupID, parentHash, selection.Account.ID))
			require.Equal(t, accounts[0].ID, cache.sessionBindings[cacheKey])
			require.Zero(t, cache.deletedSessions[cacheKey], "a child alias must not invalidate its parent's binding")
		})
	}
}
