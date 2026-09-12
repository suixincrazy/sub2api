//go:build unit

package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestAnthropicHoldbackSameAccountRetry 验证持流丢弃后在同一账号上重试 6 次，
// 第 7 发才返回错误让 handler 换号。
func TestAnthropicHoldbackSameAccountRetry(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(nil)
	c.Request = &http.Request{}

	account := &Account{ID: 42, Platform: PlatformAnthropic, Name: "test-acct"}
	ctx := context.Background()

	var attempts int
	forward := func(ctx context.Context) (string, error) {
		attempts++
		// 前 6 次失败，第 7 次成功
		if attempts <= 6 {
			noteAnthropicHoldbackDiscard(c)
			return "", &UpstreamFailoverError{
				StatusCode:             http.StatusBadGateway,
				Scope:                  GatewayFailureScopeRequest,
				RequestScopedTransient: true,
				Reason:                 GatewayFailureReason("anthropic_short_turn_holdback"),
			}
		}
		return "ok", nil
	}

	result, err := retryAnthropicHoldback(ctx, c, account, forward)
	if err != nil {
		t.Fatalf("expected success after 6 retries, got error: %v", err)
	}
	if result != "ok" {
		t.Errorf("expected result 'ok', got %q", result)
	}
	if attempts != 7 {
		t.Errorf("expected 7 attempts (1 initial + 6 retries), got %d", attempts)
	}
}

// TestAnthropicHoldbackSameAccountRetryExhausted 验证重试用尽后返回错误让 handler 换号。
func TestAnthropicHoldbackSameAccountRetryExhausted(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(nil)
	c.Request = &http.Request{}

	account := &Account{ID: 13, Platform: PlatformAnthropic, Name: "bad-acct"}
	ctx := context.Background()

	var attempts int
	forward := func(ctx context.Context) (int, error) {
		attempts++
		noteAnthropicHoldbackDiscard(c)
		return 0, &UpstreamFailoverError{
			StatusCode:             http.StatusBadGateway,
			Scope:                  GatewayFailureScopeRequest,
			RequestScopedTransient: true,
			Reason:                 GatewayFailureReason("anthropic_short_turn_holdback"),
		}
	}

	_, err := retryAnthropicHoldback(ctx, c, account, forward)
	if err == nil {
		t.Fatal("expected error after exhausting retries, got nil")
	}
	var failover *UpstreamFailoverError
	if !errors.As(err, &failover) {
		t.Fatalf("expected UpstreamFailoverError, got %T: %v", err, err)
	}
	if failover.Reason != GatewayFailureReason("anthropic_short_turn_holdback") {
		t.Errorf("expected reason anthropic_short_turn_holdback, got %s", failover.Reason)
	}
	// 1 initial + 6 retries = 7 attempts before giving up
	if attempts != 7 {
		t.Errorf("expected 7 attempts before exhaustion, got %d", attempts)
	}
}

// TestAnthropicHoldbackRetryRefundsCountBudget 验证同号重试退回丢弃计数，
// 否则 6 次重试会在第 4 发就把 anthropicShortTurnDiscardBudget=4 打穿。
func TestAnthropicHoldbackRetryRefundsCountBudget(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(nil)
	c.Request = &http.Request{}

	account := &Account{ID: 8, Platform: PlatformAnthropic, Name: "refund-acct"}
	ctx := context.Background()

	var attempts int
	forward := func(ctx context.Context) (string, error) {
		attempts++
		used := anthropicHoldbackDiscardsUsed(c)
		// 退款机制工作时，used 应该稳定在 1（每次 note 后立刻 refund）
		if used > 1 {
			return "", fmt.Errorf("discard counter leaked to %d at attempt %d, refund broken", used, attempts)
		}
		if attempts <= 3 {
			noteAnthropicHoldbackDiscard(c)
			return "", &UpstreamFailoverError{
				StatusCode:             http.StatusBadGateway,
				Scope:                  GatewayFailureScopeRequest,
				RequestScopedTransient: true,
				Reason:                 GatewayFailureReason("anthropic_short_turn_holdback"),
			}
		}
		return "refund-ok", nil
	}

	result, err := retryAnthropicHoldback(ctx, c, account, forward)
	if err != nil {
		t.Fatalf("expected success, got error: %v", err)
	}
	if result != "refund-ok" {
		t.Errorf("expected 'refund-ok', got %q", result)
	}
	finalUsed := anthropicHoldbackDiscardsUsed(c)
	// 最后一次成功，没有 note，退款也没跑，计数应该还是 0
	if finalUsed != 0 {
		t.Errorf("expected final discard count 0, got %d", finalUsed)
	}
}

// TestAnthropicHoldbackRetryBlockOrderVariant 验证 block_order_violation 走同一套重试逻辑。
func TestAnthropicHoldbackRetryBlockOrderVariant(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(nil)
	c.Request = &http.Request{}

	account := &Account{ID: 7, Platform: PlatformAnthropic, Name: "order-acct"}
	ctx := context.Background()

	var attempts int
	forward := func(ctx context.Context) (string, error) {
		attempts++
		if attempts <= 2 {
			noteAnthropicBlockOrderDiscard(c)
			return "", &UpstreamFailoverError{
				StatusCode:             http.StatusBadGateway,
				Scope:                  GatewayFailureScopeRequest,
				RequestScopedTransient: true,
				Reason:                 GatewayFailureReason("anthropic_block_order_violation"),
			}
		}
		return "order-ok", nil
	}

	result, err := retryAnthropicHoldback(ctx, c, account, forward)
	if err != nil {
		t.Fatalf("expected success, got error: %v", err)
	}
	if result != "order-ok" {
		t.Errorf("expected 'order-ok', got %q", result)
	}
	if attempts != 3 {
		t.Errorf("expected 3 attempts, got %d", attempts)
	}
}

// TestAnthropicHoldbackRetryRespectsContext 验证 ctx 取消立即退出，不继续重试。
func TestAnthropicHoldbackRetryRespectsContext(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(nil)
	c.Request = &http.Request{}

	account := &Account{ID: 99, Platform: PlatformAnthropic}
	ctx, cancel := context.WithCancel(context.Background())

	var attempts int
	forward := func(ctx context.Context) (string, error) {
		attempts++
		if attempts == 2 {
			cancel()
		}
		noteAnthropicHoldbackDiscard(c)
		return "", &UpstreamFailoverError{
			StatusCode:             http.StatusBadGateway,
			Scope:                  GatewayFailureScopeRequest,
			RequestScopedTransient: true,
			Reason:                 GatewayFailureReason("anthropic_short_turn_holdback"),
		}
	}

	_, err := retryAnthropicHoldback(ctx, c, account, forward)
	if err == nil {
		t.Fatal("expected context cancellation error, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got: %v", err)
	}
	// 第 2 次 forward 取消了 ctx，sleep 应该立即返回，不再有第 3 次 forward
	if attempts > 2 {
		t.Errorf("expected ≤2 attempts before cancel, got %d", attempts)
	}
}

// TestAnthropicHoldbackRetryNonRetryableError 验证非持流丢弃的错误不重试，直接返回。
func TestAnthropicHoldbackRetryNonRetryableError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(nil)
	c.Request = &http.Request{}

	account := &Account{ID: 5, Platform: PlatformAnthropic}
	ctx := context.Background()

	var attempts int
	forward := func(ctx context.Context) (string, error) {
		attempts++
		return "", &UpstreamFailoverError{
			StatusCode: http.StatusTooManyRequests,
			Scope:      GatewayFailureScopeAccount,
			Reason:     GatewayFailureReason("rate_limit"),
		}
	}

	_, err := retryAnthropicHoldback(ctx, c, account, forward)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if attempts != 1 {
		t.Errorf("non-retryable error should not retry; expected 1 attempt, got %d", attempts)
	}
}
