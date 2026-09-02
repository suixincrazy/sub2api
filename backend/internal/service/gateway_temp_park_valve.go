package service

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// isNoAvailableAccountsErr 判断选号错误是否属于「这个分组现在一个号都选不出来」。
// 多个选号返回点会把 ErrNoAvailableAccounts 包一层原因说明，所以必须用 errors.Is。
func isNoAvailableAccountsErr(err error) bool {
	return err != nil && errors.Is(err, ErrNoAvailableAccounts)
}

// tempParkValveOutcome 描述一次阀门尝试的结论，仅用于日志与测试断言。
type tempParkValveOutcome string

const (
	tempParkValveDisabled    tempParkValveOutcome = "disabled"
	tempParkValveNotEligible tempParkValveOutcome = "not_eligible"
	tempParkValveNoCandidate tempParkValveOutcome = "no_candidate"
	tempParkValveReleased    tempParkValveOutcome = "released"
	tempParkValveClearFailed tempParkValveOutcome = "clear_failed"
)

// tempParkValveBucketRefreshTimeout 给放行后的分桶刷新兜一个上限：这条路径本来
// 是要回 503 的，多花十几毫秒换一次成功调度值得，但不能让慢库把请求拖长。
const tempParkValveBucketRefreshTimeout = 2 * time.Second

// releaseEarliestTempParkedAccount 是整档候选被清空时的最后一道放行阀。
//
// 触发前提：常规选号已经判定「这个分组现在一个号都选不出来」，也就是本次请求必然
// 以 503 no available accounts 结束。在这个前提下放行一个「只差临时停调窗口」的号，
// 严格优于确定失败——最坏情况是这一发在上游又失败一次，那也走的是 failover 链路，
// 客户端还有换号的机会；而 503 是终态。
//
// 只放行**一个**号，挑 temp_unschedulable_until 最早到点的那个：它离自然恢复最近，
// 是候选里"最不坏"的一个。不批量放行是为了不把整组惩罚一次性抹掉——阈值机制本身
// 要留着，被放行的号下一发再越线会被重新 park。
//
// 返回被放行的账号 ID（0 表示没放行）。调用方拿到非 0 应当重跑一次常规选号，
// 而不是直接使用这里返回的账号：重选会把粘性、利润门、并发槽、模型支持等所有门
// 再走一遍，避免阀门变成绕过这些门的后路。
func (s *GatewayService) releaseEarliestTempParkedAccount(
	ctx context.Context,
	groupID *int64,
	platform string,
	useMixed bool,
	requestedModel string,
	excludedIDs map[int64]struct{},
) (int64, tempParkValveOutcome) {
	if s == nil || s.accountRepo == nil {
		return 0, tempParkValveNotEligible
	}
	if s.cfg == nil || !s.cfg.Gateway.TempParkReleaseValveEnabled {
		return 0, tempParkValveDisabled
	}
	// 无分组请求走的是"未分组账号"池，没有对应的 park 查询，且这条路径上不存在
	// 本阀门要解决的"整组同时被关"形态（那形态的前提是同一分组共用一套阈值）。
	if groupID == nil || *groupID <= 0 {
		return 0, tempParkValveNotEligible
	}

	platforms := []string{platform}
	if useMixed {
		platforms = append(platforms, PlatformAntigravity)
	}

	parked, err := s.accountRepo.ListTempParkedByGroupIDAndPlatforms(ctx, *groupID, platforms)
	if err != nil {
		slog.Warn("temp_park_valve_query_failed",
			"group_id", *groupID,
			"platform", platform,
			"error", err)
		return 0, tempParkValveNoCandidate
	}

	var (
		best      *Account
		bestUntil time.Time
	)
	for i := range parked {
		acc := &parked[i]
		if acc.TempUnschedulableUntil == nil {
			// 查询谓词已经排除了这种情况；真出现说明读到的是过期快照，跳过。
			continue
		}
		if _, excluded := excludedIDs[acc.ID]; excluded {
			// 本次请求已经在这个号上失败过，放它回来只会原地再失败一次。
			continue
		}
		if acc.Platform == PlatformAntigravity && !acc.IsMixedSchedulableForGateway(useMixed) {
			continue
		}
		if requestedModel != "" && !s.isModelSupportedByAccountWithContext(ctx, acc, requestedModel) {
			continue
		}
		if !s.isAccountSchedulableForQuota(acc) {
			continue
		}
		if !s.isGatewayAccountProfitEligible(ctx, acc) {
			continue
		}
		if best == nil || acc.TempUnschedulableUntil.Before(bestUntil) {
			best = acc
			bestUntil = *acc.TempUnschedulableUntil
		}
	}
	if best == nil {
		return 0, tempParkValveNoCandidate
	}

	// 清 park 而不是临时豁免：见 config.TempParkReleaseValveEnabled 的说明。
	// 走 rateLimitService 而不是直接打仓储，是为了同时清掉 Redis 侧的 temp_unsched
	// 缓存与模型级限流残留，否则管理端看到的状态会和调度实际行为不一致。
	var clearErr error
	if s.rateLimitService != nil {
		clearErr = s.rateLimitService.ClearTempUnschedulable(ctx, best.ID)
	} else {
		clearErr = s.accountRepo.ClearTempUnschedulable(ctx, best.ID)
	}
	if clearErr != nil {
		slog.Warn("temp_park_valve_clear_failed",
			"group_id", *groupID,
			"account_id", best.ID,
			"error", clearErr)
		return 0, tempParkValveClearFailed
	}

	// 清掉 park 只改了数据库与单账号快照，调度热路径读的是分桶候选表——被 park 的号
	// 压根不在桶里，不刷桶的话本次重选照样看不到它（只能等 outbox worker 那一秒）。
	// 刷新失败不回滚放行：park 已经清掉，下一发或 outbox 追上后照样受益。
	if s.schedulerSnapshot != nil {
		refreshCtx, cancel := context.WithTimeout(ctx, tempParkValveBucketRefreshTimeout)
		if err := s.schedulerSnapshot.RefreshAccountBuckets(refreshCtx, best.ID, "temp_park_valve"); err != nil {
			slog.Warn("temp_park_valve_bucket_refresh_failed",
				"group_id", *groupID,
				"account_id", best.ID,
				"error", err)
		}
		cancel()
	}

	slog.Warn("temp_park_valve_released",
		"group_id", *groupID,
		"platform", platform,
		"account_id", best.ID,
		"parked_until", bestUntil.Format(time.RFC3339),
		"remaining_park_ms", time.Until(bestUntil).Milliseconds(),
		"model", requestedModel,
		"parked_candidates", len(parked),
		"reason", "group_would_return_503_no_available_accounts")

	return best.ID, tempParkValveReleased
}

// IsMixedSchedulableForGateway 与 listSchedulableAccounts 里对 Antigravity 混合调度的
// 判断保持一致：只有显式开启混合调度的 Antigravity 账号才允许被 Anthropic/Gemini
// 平台的请求选中。
func (a *Account) IsMixedSchedulableForGateway(useMixed bool) bool {
	if a == nil {
		return false
	}
	if a.Platform != PlatformAntigravity {
		return true
	}
	return useMixed && a.IsMixedSchedulingEnabled()
}
