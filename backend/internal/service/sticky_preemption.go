package service

import (
	"context"
	"log/slog"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

// 粘性会话抢占（sticky preemption）。
//
// 背景：粘性会话命中路径在 gate 全通过后无条件返回绑定账号，绑定本身又有 TTL 续期，
// 因此一旦主号临时不可用（safeguards / 429 / 槽位满）导致绑定落到副号，
// 长会话就会永久钉死在副号上：
//   - 主号冷却恢复后永不再被评估 → 不切回主号；
//   - 同一优先级的多个副号里只有被绑上的那个会被使用 → 不轮询。
//
// 修复思路：命中粘性绑定后，先判断候选池里是否存在「严格更优」的账号。
// 若存在则删除绑定并放弃这次粘性命中，让后续负载感知层（优先级 → 负载率 → LRU）重新选号。
// 判定分两档，各由独立开关控制：
//   - 更高优先级恢复（priority 数值更小）→ StickyPreemptHigherPriority
//   - 同优先级但更久未使用（LRU 更旧，且超过 MinIdle）→ StickyPreemptSamePriorityRotate
//
// 只在「能确定存在更优账号」时才放弃绑定，其余情况一律保持原有粘性行为不变。

// stickyPreemptionDecision 描述一次抢占判定结果。
type stickyPreemptionDecision struct {
	preempt bool
	reason  string
	betterA int64 // 触发抢占的更优账号 ID，仅用于日志
}

// stickyPreemptionCandidate 是抢占判定所需的账号最小信息，
// 使 anthropic / openai 两条路径可以复用同一套判定逻辑。
type stickyPreemptionCandidate struct {
	ID         int64
	Priority   int
	LastUsedAt *time.Time
}

// evaluateStickyPreemption 判断是否应放弃 bound 这次粘性命中。
//
// bound 为当前绑定账号；candidates 为本次请求已通过全部可调度性过滤的候选账号
// （必须已排除 excluded / 不可调度 / 平台或模型不匹配的账号，否则会把请求推给一个选不中的号，
// 导致回落到 Layer 2 后又重新绑回同一个副号，白跑一轮）。
func evaluateStickyPreemption(
	bound stickyPreemptionCandidate,
	candidates []stickyPreemptionCandidate,
	higherPriorityEnabled bool,
	samePriorityRotateEnabled bool,
	minIdle time.Duration,
	now time.Time,
) stickyPreemptionDecision {
	if !higherPriorityEnabled && !samePriorityRotateEnabled {
		return stickyPreemptionDecision{}
	}
	if bound.ID <= 0 {
		return stickyPreemptionDecision{}
	}

	var bestHigher, bestRotate *stickyPreemptionCandidate
	for i := range candidates {
		cand := &candidates[i]
		if cand.ID == bound.ID || cand.ID <= 0 {
			continue
		}

		if higherPriorityEnabled && cand.Priority < bound.Priority {
			// 更高优先级（数值更小）的账号已恢复，立即抢占，不受 MinIdle 约束：
			// 主号恢复就该马上切回，等待只会继续消耗副号额度。
			// 这条同时兜住「副号之间正在轮换时主号恢复」的场景——
			// bestHigher 的判定优先于同级轮换，轮换过程中随时可以切回主号。
			if bestHigher == nil || cand.Priority < bestHigher.Priority {
				bestHigher = cand
			}
			continue
		}

		if samePriorityRotateEnabled && cand.Priority == bound.Priority {
			// 同优先级轮询：仅当候选账号比绑定账号更久未使用时才换，
			// 保证轮换方向单调（否则两个号会在同一会话里来回抖动）。
			if !stickyCandidateIdleLonger(cand, &bound) {
				continue
			}
			// MinIdle 防抖：要求候选账号比绑定账号「旧出」至少 minIdle。
			//
			// 这里必须比较两者 LastUsedAt 的差值，不能看绑定账号自身空闲多久：
			// 连续流量下绑定账号每个请求都在刷新 LastUsedAt，自身空闲时间恒为 0，
			// 用它做门控会让轮询永远不触发（线上表现正是「只请求一个号」）。
			// 用差值同时天然防抖：换过去之后原账号才开始变旧，
			// 要再过 minIdle 才会换回来，轮询周期 = minIdle。
			if minIdle > 0 && stickyCandidateStarvedFor(cand, &bound, now) < minIdle {
				continue
			}
			if bestRotate == nil || stickyCandidateIdleLonger(cand, bestRotate) {
				bestRotate = cand
			}
		}
	}

	// 更高优先级恢复优先于同优先级轮询。
	if bestHigher != nil {
		return stickyPreemptionDecision{preempt: true, reason: "higher_priority_recovered", betterA: bestHigher.ID}
	}
	if bestRotate != nil {
		// betterA 是「触发抢占的证据账号」（同级里最久未使用的那个），不是最终选号结果：
		// 抢占只负责删掉绑定，最终选谁由 Layer 2 负载感知层重新决定，
		// 而 Layer 2 会在「同优先级 + 同负载 + 最近使用时间相近」的组内随机打散
		// （见 sameLastUsedAt / lastUsedGroupTolerance），所以多个同级账号之间是随机轮换。
		return stickyPreemptionDecision{preempt: true, reason: "same_priority_rotate", betterA: bestRotate.ID}
	}
	return stickyPreemptionDecision{}
}

// stickyIdleGapInfinite 表示候选账号从未使用过，视为无限久未被调度。
const stickyIdleGapInfinite = time.Duration(1) << 62

// stickyCandidateStarvedFor 返回候选账号比绑定账号「多饿了多久」，
// 即两者 LastUsedAt 的差值。绑定账号从未使用过时退化为相对 now 计算。
func stickyCandidateStarvedFor(cand, bound *stickyPreemptionCandidate, now time.Time) time.Duration {
	if cand.LastUsedAt == nil {
		return stickyIdleGapInfinite
	}
	ref := now
	if bound.LastUsedAt != nil {
		ref = *bound.LastUsedAt
	}
	return ref.Sub(*cand.LastUsedAt)
}

// stickyCandidateIdleLonger 判断 a 是否比 b 更久未使用（LRU 更旧）。
// nil 表示从未使用过，视为最旧。
func stickyCandidateIdleLonger(a, b *stickyPreemptionCandidate) bool {
	switch {
	case a.LastUsedAt == nil && b.LastUsedAt == nil:
		return false
	case a.LastUsedAt == nil:
		return true
	case b.LastUsedAt == nil:
		return false
	default:
		return a.LastUsedAt.Before(*b.LastUsedAt)
	}
}

// logStickyPreemption 输出结构化日志，便于线上核对切回/轮询是否真的发生。
func logStickyPreemption(groupID int64, sessionHash string, bound int64, decision stickyPreemptionDecision) {
	slog.Info("sticky.preempted",
		"group_id", groupID,
		"session", shortSessionHash(sessionHash),
		"bound_account_id", bound,
		"better_account_id", decision.betterA,
		"reason", decision.reason,
	)
}

// stickyPreemptionCandidatesFromAccounts 由 []*Account 构造判定输入。
func stickyPreemptionCandidatesFromAccounts(accounts []*Account) []stickyPreemptionCandidate {
	out := make([]stickyPreemptionCandidate, 0, len(accounts))
	for _, acc := range accounts {
		if acc == nil {
			continue
		}
		out = append(out, stickyPreemptionCandidate{ID: acc.ID, Priority: acc.Priority, LastUsedAt: acc.LastUsedAt})
	}
	return out
}

// stickyPreemptionCandidateFromAccount 由单个 *Account 构造绑定账号信息。
func stickyPreemptionCandidateFromAccount(acc *Account) stickyPreemptionCandidate {
	if acc == nil {
		return stickyPreemptionCandidate{}
	}
	return stickyPreemptionCandidate{ID: acc.ID, Priority: acc.Priority, LastUsedAt: acc.LastUsedAt}
}

// stickyPreemptionConfig 从调度配置读取三个开关，集中一处便于两条路径共用。
type stickyPreemptionConfig struct {
	higherPriority     bool
	samePriorityRotate bool
	minIdle            time.Duration
}

// gatewayStickyPreemptionCandidates 收集 anthropic 侧「本次请求真正可选」的账号。
//
// 过滤链必须与 Layer 2 负载感知选择保持一致，否则可能因为一个实际选不中的账号放弃绑定，
// 回落后又绑回原账号，白跑一轮且丢掉粘性收益。
func (s *GatewayService) gatewayStickyPreemptionCandidates(
	ctx context.Context,
	accounts []Account,
	boundAccountID int64,
	isExcluded func(int64) bool,
	platform string,
	useMixed bool,
	requestedModel string,
) []stickyPreemptionCandidate {
	out := make([]stickyPreemptionCandidate, 0, len(accounts))
	for i := range accounts {
		acc := &accounts[i]
		if acc.ID == boundAccountID {
			continue
		}
		if isExcluded != nil && isExcluded(acc.ID) {
			continue
		}
		if !s.isAccountSchedulableForSelection(acc) {
			continue
		}
		if !s.isGatewayAccountProfitEligible(ctx, acc) {
			continue
		}
		if !s.isAccountAllowedForPlatform(acc, platform, useMixed) {
			continue
		}
		if requestedModel != "" && !s.isModelSupportedByAccountWithContext(ctx, acc, requestedModel) {
			continue
		}
		if !s.isAccountSchedulableForModelSelection(ctx, acc, requestedModel) {
			continue
		}
		if !s.isAccountSchedulableForQuota(acc) {
			continue
		}
		// 非粘性口径：抢占后该账号会走 Layer 2，必须按非粘性阈值判断。
		if !s.isAccountSchedulableForWindowCost(ctx, acc, false) {
			continue
		}
		if !s.isAccountSchedulableForRPM(ctx, acc, false) {
			continue
		}
		out = append(out, stickyPreemptionCandidate{ID: acc.ID, Priority: acc.Priority, LastUsedAt: acc.LastUsedAt})
	}
	return out
}

// maybePreemptGatewaySticky 判定并执行 anthropic 侧的粘性抢占。
// 返回 true 表示绑定已删除，调用方应放弃这次粘性命中，回落到负载感知选择。
func (s *GatewayService) maybePreemptGatewaySticky(
	ctx context.Context,
	groupID *int64,
	sessionHash string,
	boundAccount *Account,
	accounts []Account,
	isExcluded func(int64) bool,
	platform string,
	useMixed bool,
	requestedModel string,
	pc stickyPreemptionConfig,
) bool {
	if boundAccount == nil || s.cache == nil || sessionHash == "" {
		return false
	}
	if !pc.higherPriority && !pc.samePriorityRotate {
		return false
	}
	candidates := s.gatewayStickyPreemptionCandidates(ctx, accounts, boundAccount.ID, isExcluded, platform, useMixed, requestedModel)
	if len(candidates) == 0 {
		return false
	}
	decision := evaluateStickyPreemption(
		stickyPreemptionCandidateFromAccount(boundAccount),
		candidates,
		pc.higherPriority,
		pc.samePriorityRotate,
		pc.minIdle,
		time.Now(),
	)
	if !decision.preempt {
		return false
	}
	// 删除绑定，让 Layer 2 重新选号；Layer 2 选中后会重新写入绑定。
	if err := s.cache.DeleteSessionAccountID(ctx, derefGroupID(groupID), sessionHash); err != nil {
		slog.Warn("sticky.preempt_delete_failed",
			"group_id", derefGroupID(groupID),
			"bound_account_id", boundAccount.ID,
			"error", err,
		)
		return false
	}
	logStickyPreemption(derefGroupID(groupID), sessionHash, boundAccount.ID, decision)
	return true
}

// maybePreemptGatewayStickyAmong 在给定候选池内判定并执行 anthropic 侧的粘性抢占。
//
// 用于模型路由（Layer 1）路径：该路径有独立的粘性命中分支，候选必须限定在
// 路由列表命中的账号内，否则会把请求推给一个不在路由范围里的账号，
// 回落后又绑回原账号，白跑一轮。candidates 应当已通过与该层一致的可调度性过滤。
func (s *GatewayService) maybePreemptGatewayStickyAmong(
	ctx context.Context,
	groupID *int64,
	sessionHash string,
	boundAccount *Account,
	candidates []*Account,
	pc stickyPreemptionConfig,
) bool {
	if boundAccount == nil || s.cache == nil || sessionHash == "" {
		return false
	}
	if !pc.higherPriority && !pc.samePriorityRotate {
		return false
	}
	pool := stickyPreemptionCandidatesFromAccounts(candidates)
	if len(pool) == 0 {
		return false
	}
	decision := evaluateStickyPreemption(
		stickyPreemptionCandidateFromAccount(boundAccount),
		pool,
		pc.higherPriority,
		pc.samePriorityRotate,
		pc.minIdle,
		time.Now(),
	)
	if !decision.preempt {
		return false
	}
	if err := s.cache.DeleteSessionAccountID(ctx, derefGroupID(groupID), sessionHash); err != nil {
		slog.Warn("sticky.preempt_delete_failed",
			"group_id", derefGroupID(groupID),
			"bound_account_id", boundAccount.ID,
			"error", err,
		)
		return false
	}
	logStickyPreemption(derefGroupID(groupID), sessionHash, boundAccount.ID, decision)
	return true
}

// stickyPreemptionConfigFromScheduling 读取调度配置里的两个开关。
func stickyPreemptionConfigFromScheduling(cfg config.GatewaySchedulingConfig) stickyPreemptionConfig {
	return stickyPreemptionConfig{
		higherPriority:     cfg.StickyPriorityReclaimEnabled,
		samePriorityRotate: cfg.StickySamePriorityRotateInterval > 0,
		minIdle:            cfg.StickySamePriorityRotateInterval,
	}
}

// openaiStickyPreemptionCandidates 收集 openai 兼容侧「本次请求真正可选」的账号。
//
// 过滤链与 Layer 2 候选构建保持一致（isOpenAICompatibleAccountEligibleForRequest +
// 影子母账号健康 + 运行时封锁 + 上游渠道模型限制），避免为一个选不中的账号放弃绑定。
func (s *OpenAIGatewayService) openaiStickyPreemptionCandidates(
	ctx context.Context,
	accounts []Account,
	boundAccountID int64,
	isExcluded func(int64) bool,
	groupID *int64,
	platform string,
	requestedModel string,
	requireCompact bool,
	requiredCapability OpenAIEndpointCapability,
	needsUpstreamCheck bool,
) []stickyPreemptionCandidate {
	out := make([]stickyPreemptionCandidate, 0, len(accounts))
	parentCache := make(map[int64]*Account)
	parentLookup := func(id int64) *Account {
		if a, ok := parentCache[id]; ok {
			return a
		}
		if s.accountRepo == nil {
			return nil
		}
		a, _ := s.accountRepo.GetByID(ctx, id)
		parentCache[id] = a
		return a
	}
	for i := range accounts {
		acc := &accounts[i]
		if acc.ID == boundAccountID {
			continue
		}
		if isExcluded != nil && isExcluded(acc.ID) {
			continue
		}
		if !isOpenAICompatibleAccountEligibleForRequest(ctx, acc, platform, requestedModel, false, requiredCapability) {
			continue
		}
		if !parentHealthyForShadow(acc, parentLookup) {
			continue
		}
		if s.isOpenAIAccountRequestRuntimeBlocked(acc, requestedModel) {
			continue
		}
		if needsUpstreamCheck && groupID != nil &&
			s.isUpstreamModelRestrictedByChannel(ctx, *groupID, acc, requestedModel, requireCompact) {
			continue
		}
		out = append(out, stickyPreemptionCandidate{ID: acc.ID, Priority: acc.Priority, LastUsedAt: acc.LastUsedAt})
	}
	return out
}

// maybePreemptOpenAISticky 判定并执行 openai 兼容侧的粘性抢占。
// 返回 true 表示绑定已删除，调用方应放弃这次粘性命中，回落到负载感知选择。
func (s *OpenAIGatewayService) maybePreemptOpenAISticky(
	ctx context.Context,
	groupID *int64,
	sessionHash string,
	boundAccount *Account,
	accounts []Account,
	isExcluded func(int64) bool,
	platform string,
	requestedModel string,
	requireCompact bool,
	requiredCapability OpenAIEndpointCapability,
	needsUpstreamCheck bool,
	pc stickyPreemptionConfig,
) bool {
	if boundAccount == nil || sessionHash == "" {
		return false
	}
	if !pc.higherPriority && !pc.samePriorityRotate {
		return false
	}
	candidates := s.openaiStickyPreemptionCandidates(
		ctx, accounts, boundAccount.ID, isExcluded, groupID,
		platform, requestedModel, requireCompact, requiredCapability, needsUpstreamCheck,
	)
	if len(candidates) == 0 {
		return false
	}
	decision := evaluateStickyPreemption(
		stickyPreemptionCandidateFromAccount(boundAccount),
		candidates,
		pc.higherPriority,
		pc.samePriorityRotate,
		pc.minIdle,
		time.Now(),
	)
	if !decision.preempt {
		return false
	}
	if err := s.deleteStickySessionAccountID(ctx, groupID, sessionHash); err != nil {
		slog.Warn("sticky.preempt_delete_failed",
			"group_id", derefGroupID(groupID),
			"bound_account_id", boundAccount.ID,
			"error", err,
		)
		return false
	}
	logStickyPreemption(derefGroupID(groupID), sessionHash, boundAccount.ID, decision)
	return true
}
