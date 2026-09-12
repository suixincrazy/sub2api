package service

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// anthropicHoldbackMaxSameAccountRetries 是持流判定丢弃后在**同一账号**上的重试次数上限。
//
// 与 upstream429MaxRetries 同值、同机制：都是服务层自带循环 + 固定常量，不走 handler 的
// 账号级 pool_mode_retry_count 预算。必须走服务层而不是 UpstreamFailoverError 上的
// SameAccountRetryMax 字段——那个字段在 effectiveSameAccountRetryLimit / sameAccountRetryAllowed
// 里是**取小**的封顶（min(6, pool_mode_retry_count=3) = 3），给不出 6 次。
//
// 标定依据（2026-09-12 审计两份会话 transcript）：62 次非豁免断流里，40 次能在服务端日志上
// 精确配到 disposition=delivered 的事件，零次 discarded。每一条链都是「丢弃 -> 立刻换号 ->
// 新账号又给短回合 -> 再丢弃」，从未在同一账号上重试过第二发。典型链：
//
//	A ln=2115  acct=13 lat=261675ms discards=4 switches=3  -> 次数预算耗尽被迫交付
//	B ln=2362  acct=13 lat=43723ms  discards=2 switches=2  -> 墙钟预算耗尽被迫交付
//
// 原设计的假设是「同一个坏中转上重试只会再截断一次」，所以刻意不设同号重试。这条假设被
// 上面的实证推翻：连续换 2-4 个号都给出同形态短回合，说明截断根因更可能在上游侧的瞬时状态
// 而非某个中转账号的固有属性，同号重试因此有价值。
const anthropicHoldbackMaxSameAccountRetries = 6

// anthropicHoldbackRetryDelay 同号重试的间隔。
//
// 取 500ms 与 handler 的 sameAccountRetryDelay 对齐。不做指数退避：截断是上游瞬时状态，
// 退避只会把 6 次重试的墙钟推到丢弃代价预算之外，反而让重试没跑完就被迫交付。
const anthropicHoldbackRetryDelay = 500 * time.Millisecond

type anthropicHoldbackRetryKey struct{}

// anthropicHoldbackRetryState 的作用域是「一次转发调用 × 一个账号」，与 429 那套一致。
// 嵌套的协议适配器共享同一个 state，重试预算因此不会相乘。
type anthropicHoldbackRetryState struct {
	accountID int64
	mu        sync.Mutex
	retries   int
}

func anthropicHoldbackRetryStateFrom(ctx context.Context, account *Account) *anthropicHoldbackRetryState {
	if ctx == nil || account == nil {
		return nil
	}
	state, _ := ctx.Value(anthropicHoldbackRetryKey{}).(*anthropicHoldbackRetryState)
	if state != nil && state.accountID == account.ID {
		return state
	}
	return nil
}

// isAnthropicHoldbackDiscard 判定这个错误是不是持流判定主动丢弃产生的。
//
// 认 Reason 而不认 StatusCode：两档丢弃都是 502，而 502 在这条链上还有别的来源
// （传输层失败、上游真的返回 502），那些不该走同号重试。
func isAnthropicHoldbackDiscard(err error) (*UpstreamFailoverError, bool) {
	var failure *UpstreamFailoverError
	if !errors.As(err, &failure) || failure == nil {
		return nil, false
	}
	switch failure.Reason {
	case GatewayFailureReason("anthropic_short_turn_holdback"),
		GatewayFailureReason("anthropic_block_order_violation"):
		return failure, true
	}
	return nil, false
}

// retryAnthropicHoldback 在同一账号上重试被持流判定丢弃的转发，最多
// anthropicHoldbackMaxSameAccountRetries 次，之后把错误交回 handler 去换号。
//
// 与 retryUpstream429 的三处共同约束：
//   - 已经写出语义字节（IsResponseCommitted）就不再重试，避免同一客户端流里重放。
//   - ctx 取消立即退出。
//   - 同一账号的 state 已存在时直接透传，防止嵌套适配器让预算相乘。
//
// 一处持流特有的约束：每次重试都要把丢弃计数退回去。计数额度
// （anthropicShortTurnDiscardBudget / anthropicEmptyAnswerDiscardBudget）按它自己的注释
// 管的是「换几次号」，同号重试不换号，不该消耗它——否则 6 次同号重试会在第 5 发就把
// 4 次的额度打穿，判定退化成无条件放行，正好制造出这次要修的那种断流。
func retryAnthropicHoldback[T any](
	ctx context.Context,
	c *gin.Context,
	account *Account,
	forward func(context.Context) (T, error),
) (T, error) {
	if account == nil || account.Platform != PlatformAnthropic {
		return forward(ctx)
	}
	if anthropicHoldbackRetryStateFrom(ctx, account) != nil {
		return forward(ctx)
	}

	state := &anthropicHoldbackRetryState{accountID: account.ID}
	ctx = context.WithValue(ctx, anthropicHoldbackRetryKey{}, state)
	// 流式终态处理拿到的 ctx 来自 gin 而不是转发参数，必须让它看到同一个 state，
	// 否则嵌套层会各自新建一份预算。与 beginUpstream429Retry 同一处理。
	if c != nil && c.Request != nil {
		original := c.Request
		c.Request = c.Request.WithContext(ctx)
		defer func() { c.Request = original }()
	}

	for {
		attemptStart := time.Now()
		result, err := forward(ctx)
		failure, isDiscard := isAnthropicHoldbackDiscard(err)
		if !isDiscard {
			return result, err
		}
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		if !failure.ShouldRetryNextAccount() || (c != nil && IsResponseCommitted(c)) {
			return result, err
		}

		state.mu.Lock()
		if state.retries >= anthropicHoldbackMaxSameAccountRetries {
			retries := state.retries
			state.mu.Unlock()
			slog.WarnContext(ctx, "anthropic_holdback_same_account_retry_exhausted",
				"account_id", account.ID, "reason", string(failure.Reason), "retries", retries)
			return result, err
		}
		state.retries++
		retry := state.retries
		state.mu.Unlock()

		// 同号重试不算换号，退回这一发记上的丢弃计数。见函数注释。
		refundAnthropicHoldbackDiscard(c, failure.Reason)

		slog.WarnContext(ctx, "anthropic_holdback_same_account_retry",
			"account_id", account.ID, "account_name", account.Name,
			"reason", string(failure.Reason),
			"retry", retry, "max_retries", anthropicHoldbackMaxSameAccountRetries)

		if waitErr := sleepWithContext(ctx, anthropicHoldbackRetryDelay); waitErr != nil {
			noteAnthropicDiscardBudgetCredit(c, time.Since(attemptStart))
			return result, waitErr
		}
		// 墙钟也要退。这是「和 429 一样的机制」的必要条件而不是额外优待：
		// forwardAnthropicAPIKeyPassthroughOnce 的 defer 只在丢弃计数没变时补记退款，而这一发
		// 计数确实涨过（上面刚退回去），那个 defer 因此判定为「有丢弃」不退款。不在这里补退的话，
		// 20000ms 的 AnthropicHoldbackDiscardBudgetMs 会被前两三发同号重试烧穿，
		// anthropicHoldbackVerdict 随即退化成无条件放行，6 次重试永远跑不完——正好复现
		// TestAnthropicDiscardBudgetExcludesHTTPFailureWallClock 锚定的那种 delivered。
		//
		// 退掉之后客户端的零字节等待并非无界：死气预算（AnthropicHoldbackDeadAirBudgetMs，
		// 默认 25s）跨 failover 累加且**不**参与这套退款，仍然逐发累计地卡着同号重试。
		// 两条线分工因此保持原样——丢弃代价预算管「还要不要再换号」，死气预算管「客户端还能等多久」。
		noteAnthropicDiscardBudgetCredit(c, time.Since(attemptStart))
	}
}
