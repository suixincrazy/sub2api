package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// 派生定价：运营只维护一个提示价，补全/缓存创建/缓存读取按倍率跟着走。
//
// 目标口径（$/MTok）：提示 4 → 补全 4×5=20、缓存创建 4×1.25=5、缓存读取 4×0.2=0.8。
// 这些常量在本文件里反复出现，抽出来是为了让"改一个基数、三档全跟上"这件事在测试里
// 也只有一处可改。
const (
	derivedPromptPerToken     = 4e-6 // $4 / 1M tokens
	derivedCompletionRatio    = 5.0
	derivedCacheCreationRatio = 1.25
	derivedCacheReadRatio     = 0.2
	derivedCompletionPerToken = 20e-6  // $20 / 1M tokens
	derivedCacheWritePerToken = 5e-6   // $5  / 1M tokens
	derivedCacheReadPerToken  = 0.8e-6 // $0.80 / 1M tokens
)

// derivedRatioPricing 返回一份"只配了提示价 + 三个倍率"的定价条目，即目标配置形态。
func derivedRatioPricing() *ChannelModelPricing {
	return &ChannelModelPricing{
		BillingMode:             BillingModeToken,
		InputPrice:              pricingMultiplier(derivedPromptPerToken),
		CompletionMultiplier:    pricingMultiplier(derivedCompletionRatio),
		CacheCreationMultiplier: pricingMultiplier(derivedCacheCreationRatio),
		CacheReadMultiplier:     pricingMultiplier(derivedCacheReadRatio),
	}
}

func TestDerivedTokenPricesFollowPromptPrice(t *testing.T) {
	// 目录价故意与目标价完全不同（$5/$25/$6.25/$0.50，即 opus-5 的回退价），
	// 用来证明四档最终都由"提示价 × 倍率"决定，而不是恰好等于目录价。
	pricing := &ModelPricing{
		InputPricePerToken:         5e-6,
		OutputPricePerToken:        25e-6,
		CacheCreationPricePerToken: 6.25e-6,
		CacheCreation5mPrice:       6.25e-6,
		CacheCreation1hPrice:       12.5e-6,
		CacheReadPricePerToken:     0.5e-6,
	}
	applyChannelTokenPriceOverrides(pricing, derivedRatioPricing())

	require.InDelta(t, derivedPromptPerToken, pricing.InputPricePerToken, 1e-18)
	require.InDelta(t, derivedCompletionPerToken, pricing.OutputPricePerToken, 1e-18)
	require.InDelta(t, derivedCacheWritePerToken, pricing.CacheCreationPricePerToken, 1e-18)
	require.InDelta(t, derivedCacheReadPerToken, pricing.CacheReadPricePerToken, 1e-18)

	// 缓存创建必须与"填绝对价"那条路径同口径：显式标记 + 5m/1h 两档一起跟上，
	// 否则 computeCacheCreationCost 会拿目录里的 1h 价（这里是 $12.5）去算 1h 缓存。
	require.True(t, pricing.CacheCreationPriceExplicit)
	require.InDelta(t, derivedCacheWritePerToken, pricing.CacheCreation5mPrice, 1e-18)
	require.InDelta(t, derivedCacheWritePerToken, pricing.CacheCreation1hPrice, 1e-18)
}

// 运营给出的算例，逐位对上：
//
//	提示 26 × $4 + 缓存 0 × $0.80 + 缓存创建 75837 × $5 + 补全 3 × $20 = $0.379349
//
// 顺带钉住分组倍率的作用位置：它乘的是**总额**，不是只乘补全那一项。运营给的公式里
// "× 分组倍率" 写在补全后面，容易读成只作用于补全；用 0.5 反算能证明是总额 ——
// 0.379349 × 0.5 = 0.1896745，与运营看到的 $0.189674 一致；若只乘补全则是 $0.379319。
func TestDerivedTokenPricesReproduceQuotedInvoice(t *testing.T) {
	pricing := &ModelPricing{}
	applyChannelTokenPriceOverrides(pricing, derivedRatioPricing())

	tokens := UsageTokens{InputTokens: 26, CacheReadTokens: 0, CacheCreationTokens: 75837, OutputTokens: 3}
	const wantTotal = 0.379349

	t.Run("分组倍率默认 1 倍", func(t *testing.T) {
		bd := (&BillingService{}).computeTokenBreakdown(pricing, tokens, 1, "", false)
		require.InDelta(t, 26*derivedPromptPerToken, bd.InputCost, 1e-12)
		require.Zero(t, bd.CacheReadCost)
		require.InDelta(t, 75837*derivedCacheWritePerToken, bd.CacheCreationCost, 1e-12)
		require.InDelta(t, 3*derivedCompletionPerToken, bd.OutputCost, 1e-12)
		require.InDelta(t, wantTotal, bd.TotalCost, 1e-9)
		require.InDelta(t, wantTotal, bd.ActualCost, 1e-9, "倍率 1 时实收等于总额")
	})

	t.Run("分组倍率乘的是总额", func(t *testing.T) {
		bd := (&BillingService{}).computeTokenBreakdown(pricing, tokens, 0.5, "", false)
		require.InDelta(t, wantTotal, bd.TotalCost, 1e-9, "总额不受分组倍率影响")
		require.InDelta(t, 0.1896745, bd.ActualCost, 1e-9)
	})
}

func TestDerivedTokenPricesYieldToExplicitPrices(t *testing.T) {
	// 绝对价与倍率同时存在时绝对价赢：前端不该同时提交两个值，但后端不能靠前端自觉。
	config := derivedRatioPricing()
	config.OutputPrice = pricingMultiplier(30e-6)
	config.CacheReadPrice = pricingMultiplier(0)

	pricing := &ModelPricing{}
	applyChannelTokenPriceOverrides(pricing, config)

	require.InDelta(t, 30e-6, pricing.OutputPricePerToken, 1e-18, "绝对补全价优先于补全倍率")
	require.Zero(t, pricing.CacheReadPricePerToken, "显式填 0 也算配置过，不该被倍率顶掉")
	require.InDelta(t, derivedCacheWritePerToken, pricing.CacheCreationPricePerToken, 1e-18,
		"没配绝对价的那档照旧派生")
}

// 没有可乘的基数时不派生。否则一个缺目录价、又没填提示价的模型会被派生成 0，
// 变成静默白送算力 —— 宁可保留目录价（这里目录价也没有，那就保持原样）。
func TestDerivedTokenPricesRequirePromptPrice(t *testing.T) {
	config := &ChannelModelPricing{
		BillingMode:          BillingModeToken,
		CompletionMultiplier: pricingMultiplier(derivedCompletionRatio),
	}
	pricing := &ModelPricing{OutputPricePerToken: 25e-6}
	applyChannelTokenPriceOverrides(pricing, config)

	require.Zero(t, pricing.InputPricePerToken)
	require.InDelta(t, 25e-6, pricing.OutputPricePerToken, 1e-18, "提示价为 0 时保留目录补全价")
}

// 三个倍率都没配时，行为必须与引入派生定价之前逐位相同。
func TestDerivedTokenPricesNoOpWithoutMultipliers(t *testing.T) {
	catalog := ModelPricing{
		InputPricePerToken:         5e-6,
		OutputPricePerToken:        25e-6,
		CacheCreationPricePerToken: 6.25e-6,
		CacheCreation1hPrice:       12.5e-6,
		CacheReadPricePerToken:     0.5e-6,
	}
	pricing := catalog
	config := &ChannelModelPricing{BillingMode: BillingModeToken}
	require.False(t, config.HasDerivedTokenPrices())

	applyChannelTokenPriceOverrides(&pricing, config)
	require.Equal(t, catalog, pricing)
}

// 派生价与绝对价覆盖走同一个 channelTierOverridePrice 口径：目录里有 Fast/Priority
// 档时按同一比例跟着走，没有则归 0 交给通用 service-tier 默认值。
func TestDerivedTokenPricesPreserveCatalogFastRatio(t *testing.T) {
	pricing := &ModelPricing{
		InputPricePerToken:                 5e-6,
		InputPricePerTokenPriority:         10e-6,
		OutputPricePerToken:                25e-6,
		OutputPricePerTokenPriority:        50e-6,
		CacheCreationPricePerToken:         6.25e-6,
		CacheCreationPricePerTokenPriority: 12.5e-6,
		CacheReadPricePerToken:             0.5e-6,
	}
	applyChannelTokenPriceOverrides(pricing, derivedRatioPricing())

	require.InDelta(t, 2, pricing.OutputPricePerTokenPriority/pricing.OutputPricePerToken, 1e-9)
	require.InDelta(t, 2, pricing.CacheCreationPricePerTokenPriority/pricing.CacheCreationPricePerToken, 1e-9)
	require.Zero(t, pricing.CacheReadPricePerTokenPriority, "目录无缓存读取 Priority 档时归 0")
}

// 解析链必须真的把派生逻辑接上，且不能污染目录里的共享指针。
func TestResolverAppliesDerivedTokenPrices(t *testing.T) {
	catalog := &ModelPricing{InputPricePerToken: 5e-6, OutputPricePerToken: 25e-6}
	resolved := &ResolvedPricing{BasePricing: catalog}

	(&ModelPricingResolver{}).applyTokenOverrides(derivedRatioPricing(), resolved)

	require.InDelta(t, derivedCompletionPerToken, resolved.BasePricing.OutputPricePerToken, 1e-18)
	require.InDelta(t, 25e-6, catalog.OutputPricePerToken, 1e-18, "目录共享指针不得被改写")
}

// 区间倍率叠在派生价之上：区间乘的是"各档自己的基础价"，而那个基础价此刻已经是派生结果。
func TestIntervalMultipliersStackOnDerivedPrices(t *testing.T) {
	resolved := &ResolvedPricing{BasePricing: &ModelPricing{}}
	(&ModelPricingResolver{}).applyTokenOverrides(derivedRatioPricing(), resolved)
	resolved.Intervals = []PricingInterval{{
		MinTokens:        200000,
		OutputMultiplier: pricingMultiplier(1.5),
	}}
	resolved.longContextPricingEnabled = true

	pricing := (&ModelPricingResolver{}).GetIntervalPricing(resolved, 200001)
	require.InDelta(t, derivedCompletionPerToken*1.5, pricing.OutputPricePerToken, 1e-18)
}

// 展示与计费必须同口径。派生价只存倍率、不存绝对价，广场/可用渠道页若不落地就会
// 展示成 "-"（或 LiteLLM 原价），而用户实际按派生价扣费。
func TestDisplayPricingMaterializesDerivedPrices(t *testing.T) {
	// 直接构造而不用 channel_available_test.go 里的 newStubPricingServiceFromMap：
	// 那个文件带 //go:build unit，本文件不带标签，两种构建模式下都要能跑。
	//
	// 目录里的补全/缓存价故意选成与"提示价 × 倍率"算不出来的数（30/8/0.5 而非 25/6.25/1），
	// 否则派生值与 LiteLLM 值撞在一起，断言就分不出展示的是哪一个。
	svc := &ChannelService{pricingService: &PricingService{pricingData: map[string]*LiteLLMModelPricing{
		"claude-opus-5": {
			Mode:                        "chat",
			InputCostPerToken:           5e-6,
			OutputCostPerToken:          30e-6,
			CacheCreationInputTokenCost: 8e-6,
			CacheReadInputTokenCost:     0.5e-6,
		},
	}}}

	t.Run("提示价来自渠道配置", func(t *testing.T) {
		models := []SupportedModel{{Name: "claude-opus-5", Platform: "anthropic", Pricing: derivedRatioPricing()}}
		svc.fillGlobalPricingFallback(models)

		got := models[0].Pricing
		require.NotNil(t, got.OutputPrice)
		require.InDelta(t, derivedCompletionPerToken, *got.OutputPrice, 1e-18)
		require.InDelta(t, derivedCacheWritePerToken, *got.CacheWritePrice, 1e-18)
		require.InDelta(t, derivedCacheReadPerToken, *got.CacheReadPrice, 1e-18)
	})

	t.Run("提示价回落到目录价", func(t *testing.T) {
		// 只配倍率、不配提示价：计费侧的有效提示价是目录价 $5，展示必须用同一个基数，
		// 而不是把 LiteLLM 的 $25 补全价原样展示出来。
		config := derivedRatioPricing()
		config.InputPrice = nil
		models := []SupportedModel{{Name: "claude-opus-5", Platform: "anthropic", Pricing: config}}
		svc.fillGlobalPricingFallback(models)

		got := models[0].Pricing
		require.InDelta(t, 5e-6, *got.InputPrice, 1e-18, "提示价由全局回落补上")
		require.InDelta(t, 25e-6, *got.OutputPrice, 1e-18, "5 × 5e-6，不是 LiteLLM 的 30e-6")
		require.InDelta(t, 6.25e-6, *got.CacheWritePrice, 1e-18, "1.25 × 5e-6，不是 LiteLLM 的 8e-6")
		require.InDelta(t, 1e-6, *got.CacheReadPrice, 1e-18, "0.2 × 5e-6，不是 LiteLLM 的 0.5e-6")
	})
}

func TestDerivedMultipliersRejectNegativeValues(t *testing.T) {
	negative := -1.0
	zero := 0.0

	require.Error(t, checkPricesNotNegative(ChannelModelPricing{CompletionMultiplier: &negative}))
	require.Error(t, checkPricesNotNegative(ChannelModelPricing{CacheCreationMultiplier: &negative}))
	require.Error(t, checkPricesNotNegative(ChannelModelPricing{CacheReadMultiplier: &negative}))

	// 0 是合法配置（例如缓存读取白送），与 fast/flex 要求严格 > 0 不同。
	require.NoError(t, checkPricesNotNegative(ChannelModelPricing{CacheReadMultiplier: &zero}))
}

// 生产形态：分组卡只填提示价 + 三个倍率，Opus 5 的长上下文阶梯由模型策略补齐，
// 两者必须叠加 —— 阶梯乘的是派生后的价格，不是目录价。
//
// 目录价（$5/$25/$6.25/$0.50）与派生价（$4/$20/$5/$0.80）刻意全不相同，
// 断言才能区分阶梯到底乘在哪个基数上。
func TestClaudeOpus5LongContextStacksOnDerivedPrices(t *testing.T) {
	newResolver := func() (*BillingService, *ModelPricingResolver) {
		bs := &BillingService{fallbackPrices: map[string]*ModelPricing{
			"claude-opus-5": {
				InputPricePerToken:         5e-6,
				OutputPricePerToken:        25e-6,
				CacheCreationPricePerToken: 6.25e-6,
				CacheReadPricePerToken:     0.5e-6,
			},
		}}
		return bs, NewModelPricingResolver(nil, bs)
	}
	groupWithCard := func(longContextEnabled bool) *Group {
		card := *derivedRatioPricing()
		card.Models = []string{"claude-opus-5"}
		return &Group{
			ID:                        100,
			ModelPricing:              []ChannelModelPricing{card},
			LongContextPricingEnabled: longContextEnabled,
		}
	}
	// input + cache_creation + cache_read = 220000 > 200000 阈值。
	overThreshold := UsageTokens{
		InputTokens:         150000,
		CacheCreationTokens: 40000,
		CacheReadTokens:     30000,
		OutputTokens:        2000,
	}
	cost := func(group *Group, tokens UsageTokens) *CostBreakdown {
		bs, r := newResolver()
		bd, err := bs.CalculateCostUnified(CostInput{
			Ctx: context.Background(), Model: "claude-opus-5", Group: group,
			Tokens: tokens, RateMultiplier: 1, Resolver: r,
		})
		require.NoError(t, err)
		require.NotNil(t, bd)
		return bd
	}

	t.Run("超阈值：阶梯乘派生价", func(t *testing.T) {
		bd := cost(groupWithCard(true), overThreshold)

		// input / 缓存两档 ×2，补全 ×1.5。
		require.InDelta(t, 150000*derivedPromptPerToken*2, bd.InputCost, 1e-9)
		require.InDelta(t, 40000*derivedCacheWritePerToken*2, bd.CacheCreationCost, 1e-9)
		require.InDelta(t, 30000*derivedCacheReadPerToken*2, bd.CacheReadCost, 1e-9)
		require.InDelta(t, 2000*derivedCompletionPerToken*1.5, bd.OutputCost, 1e-9)
		require.InDelta(t, 1.708, bd.TotalCost, 1e-9)
		require.True(t, bd.LongContextBillingApplied)
	})

	t.Run("未超阈值：仍是派生价", func(t *testing.T) {
		bd := cost(groupWithCard(true), UsageTokens{InputTokens: 100000, OutputTokens: 2000})

		require.InDelta(t, 100000*derivedPromptPerToken, bd.InputCost, 1e-9)
		require.InDelta(t, 2000*derivedCompletionPerToken, bd.OutputCost, 1e-9)
		require.False(t, bd.LongContextBillingApplied)
	})

	t.Run("分组关掉长上下文开关时阶梯不生效", func(t *testing.T) {
		bd := cost(groupWithCard(false), overThreshold)

		require.InDelta(t, 150000*derivedPromptPerToken, bd.InputCost, 1e-9)
		require.InDelta(t, 40000*derivedCacheWritePerToken, bd.CacheCreationCost, 1e-9)
		require.InDelta(t, 30000*derivedCacheReadPerToken, bd.CacheReadCost, 1e-9)
		require.InDelta(t, 2000*derivedCompletionPerToken, bd.OutputCost, 1e-9)
		require.False(t, bd.LongContextBillingApplied)
	})
}
