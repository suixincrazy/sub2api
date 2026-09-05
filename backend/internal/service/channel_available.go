package service

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// AvailableGroupRef 渠道视图中关联分组的简要信息。
//
// 用户侧「可用渠道」页面据此展示：专属分组 vs 公开分组（IsExclusive）、
// 订阅 vs 标准（SubscriptionType）、默认倍率（RateMultiplier）与高峰倍率规则。
// 用户专属倍率不在这里暴露，前端自己通过 /groups/rates 拉取，和 API 密钥页面保持一致。
type AvailableGroupRef struct {
	ID                 int64
	Name               string
	Platform           string
	SubscriptionType   string
	RateMultiplier     float64
	PeakRateEnabled    bool
	PeakStart          string
	PeakEnd            string
	PeakRateMultiplier float64
	IsExclusive        bool
}

// AvailableChannel 可用渠道视图：用于「可用渠道」页面展示渠道基础信息 +
// 关联的分组 + 推导出的支持模型列表（无通配符）。
type AvailableChannel struct {
	ID                 int64
	Name               string
	Description        string
	Status             string
	BillingModelSource string
	RestrictModels     bool
	Groups             []AvailableGroupRef
	SupportedModels    []SupportedModel
}

// ListAvailable 返回所有渠道的可用视图：每个渠道附带关联分组信息与支持模型列表。
//
// 支持模型通过 (*Channel).SupportedModels() 计算（mapping ∪ pricing 并联）。
// 对于渠道未配置定价的模型，进一步用 PricingService 的全局 LiteLLM 数据合成
// 一份展示用定价，让用户看到默认价格而非"未配置"。
//
// 关联分组信息通过 groupRepo.ListActive 查询后按 ID 映射；渠道 GroupIDs 中未在活跃列表中
// 的分组（已停用或删除）会被忽略。
//
// 前置条件：s.groupRepo 必须非 nil（由 wire DI 保证）。直接 nil-deref 用于 fail-fast，
// 避免静默掩盖注入缺失。
func (s *ChannelService) ListAvailable(ctx context.Context) ([]AvailableChannel, error) {
	channels, err := s.repo.ListAll(ctx)
	if err != nil {
		return nil, fmt.Errorf("list channels: %w", err)
	}

	groups, err := s.groupRepo.ListActive(ctx)
	if err != nil {
		return nil, fmt.Errorf("list active groups: %w", err)
	}
	groupByID := make(map[int64]AvailableGroupRef, len(groups))
	for i := range groups {
		g := groups[i]
		groupByID[g.ID] = AvailableGroupRef{
			ID:                 g.ID,
			Name:               g.Name,
			Platform:           g.Platform,
			SubscriptionType:   g.SubscriptionType,
			RateMultiplier:     g.RateMultiplier,
			PeakRateEnabled:    g.PeakRateEnabled,
			PeakStart:          g.PeakStart,
			PeakEnd:            g.PeakEnd,
			PeakRateMultiplier: g.PeakRateMultiplier,
			IsExclusive:        g.IsExclusive,
		}
	}

	out := make([]AvailableChannel, 0, len(channels))
	for i := range channels {
		ch := &channels[i]
		groups := make([]AvailableGroupRef, 0, len(ch.GroupIDs))
		for _, gid := range ch.GroupIDs {
			if ref, ok := groupByID[gid]; ok {
				groups = append(groups, ref)
			}
		}
		sort.SliceStable(groups, func(i, j int) bool { return groups[i].Name < groups[j].Name })

		ch.normalizeBillingModelSource()

		supported := ch.SupportedModels()
		fillGlobalPricingFallback(s.pricingService, supported)

		out = append(out, AvailableChannel{
			ID:                 ch.ID,
			Name:               ch.Name,
			Description:        ch.Description,
			Status:             ch.Status,
			BillingModelSource: ch.BillingModelSource,
			RestrictModels:     ch.RestrictModels,
			Groups:             groups,
			SupportedModels:    supported,
		})
	}

	sort.SliceStable(out, func(i, j int) bool {
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, nil
}

// fillGlobalPricingFallback 对未命中渠道定价的支持模型，从全局 LiteLLM 数据合成一份
// 展示用定价。仅用于「可用渠道」展示，不影响真实计费链路。
//
// 触发条件：
//  1. Pricing == nil（渠道完全没声明该模型的定价条目）
//  2. Pricing 非 nil 但所有价格字段为空（admin UI 建了条目但没填价格）
//
// 当 pricingService 为 nil（测试场景），跳过价格回落，但仍补充内置模型倍率。
// 可用渠道与模型广场共用。
func fillGlobalPricingFallback(pricingService *PricingService, models []SupportedModel) {
	for i := range models {
		// 派生倍率必须**先**按「运营是否显式配过该档」定案，再让全局回落去补 nil 字段。
		// 顺序反了的话回落补上的 LiteLLM 补全价会挡住本该派生出来的价，
		// 于是广场展示 LiteLLM 价、实际扣费按派生价，两个口径打架。
		derived := computeDerivedDisplayPrices(pricingService, models[i].Name, models[i].Pricing)
		if pricingService != nil && pricingNeedsFallback(models[i].Pricing) {
			if lp := pricingService.GetModelPricing(models[i].Name); lp != nil {
				models[i].Pricing = synthesizePricingFromLiteLLM(lp, models[i].Pricing)
			}
		}
		// 两次覆盖动的是不相交字段：派生管 output/cacheWrite/cacheRead，
		// 上游这道只管 MaxReasoningEffortMultiplier，故先后顺序不影响结果。
		models[i].Pricing = derived.applyTo(models[i].Pricing)
		models[i].Pricing = withDefaultMaxReasoningEffortMultiplier(models[i].Pricing, models[i].Name)
	}
}

// derivedTokenDisplayPrices 是按派生倍率算出的展示价，字段为 nil 表示该档不派生。
type derivedTokenDisplayPrices struct {
	output     *float64
	cacheWrite *float64
	cacheRead  *float64
}

func (d derivedTokenDisplayPrices) empty() bool {
	return d.output == nil && d.cacheWrite == nil && d.cacheRead == nil
}

// applyTo 把派生价盖到展示定价上（返回克隆，不改入参——渠道定价指针指向缓存共享数据）。
//
// 这里是**覆盖**而不是填空：某档要不要派生，在 computeDerivedDisplayPrices 里已经按
// 运营配置定过案了，此刻 p 上的值可能是全局回落刚补进来的 LiteLLM 价，不是运营配置。
func (d derivedTokenDisplayPrices) applyTo(p *ChannelModelPricing) *ChannelModelPricing {
	if p == nil || d.empty() {
		return p
	}
	clone := *p
	if d.output != nil {
		clone.OutputPrice = d.output
	}
	if d.cacheWrite != nil {
		clone.CacheWritePrice = d.cacheWrite
	}
	if d.cacheRead != nil {
		clone.CacheReadPrice = d.cacheRead
	}
	return &clone
}

// computeDerivedDisplayPrices 复刻计费侧 applyDerivedTokenPrices 的口径，算出展示用派生价。
// 有效提示价 = 渠道价优先，否则模型目录官方价；为 0 时不派生（与计费侧一致）。
func computeDerivedDisplayPrices(pricingService *PricingService, model string, p *ChannelModelPricing) derivedTokenDisplayPrices {
	var out derivedTokenDisplayPrices
	if p == nil || !p.HasDerivedTokenPrices() {
		return out
	}
	switch p.BillingMode {
	case BillingModeImage, BillingModePerRequest, BillingModeVideo:
		return out
	}
	base := p.InputPrice
	if base == nil && pricingService != nil {
		if lp := pricingService.GetModelPricing(model); lp != nil {
			base = nonZeroPtr(lp.InputCostPerToken)
		}
	}
	if base == nil || *base <= 0 {
		return out
	}
	derive := func(configured, multiplier *float64) *float64 {
		if configured != nil || multiplier == nil {
			return nil
		}
		v := *base * *multiplier
		return &v
	}
	out.output = derive(p.OutputPrice, p.CompletionMultiplier)
	out.cacheWrite = derive(p.CacheWritePrice, p.CacheCreationMultiplier)
	out.cacheRead = derive(p.CacheReadPrice, p.CacheReadMultiplier)
	return out
}

// pricingNeedsFallback 判定一个 ChannelModelPricing 是否需要走全局回落。
// 价格全部缺失（无 flat 字段且无任何带价 interval）即视为未配置。
func pricingNeedsFallback(p *ChannelModelPricing) bool {
	if p == nil {
		return true
	}
	if p.InputPrice != nil || p.OutputPrice != nil ||
		p.CacheWritePrice != nil || p.CacheWrite1hPrice != nil || p.CacheReadPrice != nil ||
		p.ImageOutputPrice != nil || p.PerRequestPrice != nil {
		return false
	}
	for _, iv := range p.Intervals {
		if iv.InputPrice != nil || iv.OutputPrice != nil ||
			iv.CacheWritePrice != nil || iv.CacheWrite1hPrice != nil || iv.CacheReadPrice != nil ||
			iv.PerRequestPrice != nil {
			return false
		}
	}
	return true
}

// synthesizePricingFromLiteLLM 把 LiteLLM 的定价数据转成 ChannelModelPricing 形态，
// 仅用于展示。
//
// 计费模式优先级：
//  1. 渠道已选 BillingMode（admin 在 UI 里选了 image / per_request 但没填价的场景，
//     按选定模式合成对应字段）
//  2. LiteLLM mode="image_generation" → image
//  3. 默认 token
//
// LiteLLM 中字段 0 视为未配置，不带入展示。
func synthesizePricingFromLiteLLM(lp *LiteLLMModelPricing, existing *ChannelModelPricing) *ChannelModelPricing {
	if lp == nil {
		return existing
	}

	mode := BillingModeToken
	switch {
	case existing != nil && existing.BillingMode != "":
		mode = existing.BillingMode
	case lp.Mode == "image_generation":
		mode = BillingModeImage
	}

	if mode == BillingModeImage || mode == BillingModePerRequest {
		return &ChannelModelPricing{
			BillingMode:                  mode,
			PerRequestPrice:              nonZeroPtr(lp.OutputCostPerImage),
			ImageOutputPrice:             nonZeroPtr(lp.OutputCostPerImageToken),
			InputPrice:                   nonZeroPtr(lp.InputCostPerToken),
			OutputPrice:                  nonZeroPtr(lp.OutputCostPerToken),
			MaxReasoningEffortMultiplier: maxReasoningEffortMultiplierFromPricing(existing),
		}
	}
	return &ChannelModelPricing{
		BillingMode:                  mode,
		InputPrice:                   nonZeroPtr(lp.InputCostPerToken),
		OutputPrice:                  nonZeroPtr(lp.OutputCostPerToken),
		CacheWritePrice:              nonZeroPtr(lp.CacheCreationInputTokenCost),
		CacheWrite1hPrice:            nonZeroPtr(lp.CacheCreationInputTokenCostAbove1hr),
		CacheReadPrice:               nonZeroPtr(lp.CacheReadInputTokenCost),
		ImageOutputPrice:             nonZeroPtr(lp.OutputCostPerImageToken),
		MaxReasoningEffortMultiplier: maxReasoningEffortMultiplierFromPricing(existing),
	}
}

func maxReasoningEffortMultiplierFromPricing(pricing *ChannelModelPricing) *float64 {
	if pricing == nil {
		return nil
	}
	return pricing.MaxReasoningEffortMultiplier
}

func nonZeroPtr(v float64) *float64 {
	if v == 0 {
		return nil
	}
	return &v
}
