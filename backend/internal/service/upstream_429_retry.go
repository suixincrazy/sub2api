package service

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

const upstream429MaxRetries = 6

type upstream429RetryKey struct{}

// A scope belongs to one forwarding call and one account, never the account
// globally. Nested protocol adapters share it so retry budgets cannot multiply.
type upstream429RetryState struct {
	accountID int64
	mu        sync.Mutex
	exhausted bool
	applying  bool
	retries   int
	headers   http.Header
	apply     func(context.Context)
}

func upstream429State(ctx context.Context, account *Account) *upstream429RetryState {
	if ctx == nil || account == nil {
		return nil
	}
	state, _ := ctx.Value(upstream429RetryKey{}).(*upstream429RetryState)
	if state != nil && state.accountID == account.ID {
		return state
	}
	return nil
}

func upstream429RetryExhausted(ctx context.Context, account *Account) bool {
	state := upstream429State(ctx, account)
	if state == nil {
		return false
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.exhausted
}

func deferUpstream429SideEffects(ctx context.Context, account *Account, status int, headers http.Header, apply func(context.Context)) bool {
	if status != http.StatusTooManyRequests {
		return false
	}
	state := upstream429State(ctx, account)
	if state == nil {
		return false
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.applying {
		return false
	}
	if state.exhausted {
		return true
	}
	state.headers = headers.Clone()
	state.apply = apply
	return true
}

func upstream429RetryEnabled(account *Account) bool {
	return account != nil && (account.Platform == PlatformOpenAI || account.Platform == PlatformAnthropic)
}

func beginUpstream429Retry(ctx context.Context, c *gin.Context, account *Account) (context.Context, *upstream429RetryState, func()) {
	if !upstream429RetryEnabled(account) {
		return ctx, nil, func() {}
	}
	if state := upstream429State(ctx, account); state != nil {
		return ctx, state, func() {}
	}
	state := &upstream429RetryState{accountID: account.ID}
	ctx = context.WithValue(ctx, upstream429RetryKey{}, state)
	restore := func() {}
	// Stream terminal handlers get their context from Gin rather than the
	// forwarding argument. They must see the same deferred health side effects.
	if c != nil && c.Request != nil {
		original := c.Request
		c.Request = c.Request.WithContext(ctx)
		restore = func() { c.Request = original }
	}
	return ctx, state, restore
}

func (state *upstream429RetryState) reset() {
	if state == nil {
		return
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	state.retries, state.exhausted, state.applying = 0, false, false
	state.headers, state.apply = nil, nil
}

func (state *upstream429RetryState) retry(ctx context.Context, c *gin.Context, account *Account, err error) (bool, error) {
	var failure *UpstreamFailoverError
	if state == nil || !errors.As(err, &failure) || failure.StatusCode != http.StatusTooManyRequests {
		return false, err
	}
	failure.RetryableOnSameAccount = false
	failure.SameAccountRetryDeadline = time.Time{}
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	if !failure.ShouldRetryNextAccount() || (c != nil && IsResponseCommitted(c)) {
		return false, err
	}
	state.mu.Lock()
	if state.exhausted {
		state.mu.Unlock()
		return false, err
	}
	retry, apply, headers := state.retries, state.apply, state.headers
	if retry == upstream429MaxRetries {
		state.exhausted, state.applying = true, true
		state.mu.Unlock()
		if apply != nil {
			apply(ctx)
		}
		state.mu.Lock()
		state.applying = false
		state.mu.Unlock()
		slog.WarnContext(ctx, "upstream_429_retry_exhausted", "account_id", account.ID, "platform", account.Platform, "retries", retry)
		return false, err
	}
	state.retries++
	state.mu.Unlock()
	if headers == nil {
		headers = failure.ResponseHeaders
	}
	delay := openAIOAuth429SameAccountRetryDelay(headers, time.Time{})
	slog.WarnContext(ctx, "upstream_429_same_account_retry", "account_id", account.ID, "platform", account.Platform, "retry", retry+1, "max_retries", upstream429MaxRetries, "delay_ms", delay.Milliseconds())
	if waitErr := sleepWithContext(ctx, delay); waitErr != nil {
		return false, waitErr
	}
	return true, err
}

func retryUpstream429[T any](ctx context.Context, c *gin.Context, account *Account, forward func(context.Context) (T, error)) (T, error) {
	if !upstream429RetryEnabled(account) || upstream429State(ctx, account) != nil {
		return forward(ctx)
	}
	ctx, state, restore := beginUpstream429Retry(ctx, c, account)
	defer restore()
	for {
		before := OpenAICompactKeepaliveAdjustedWrittenSize(c)
		result, err := forward(ctx)
		if OpenAICompactKeepaliveAdjustedWrittenSize(c) != before {
			var failure *UpstreamFailoverError
			if errors.As(err, &failure) && failure.StatusCode == http.StatusTooManyRequests {
				failure.RetryableOnSameAccount = false
				failure.SameAccountRetryDeadline = time.Time{}
			}
			return result, err
		}
		again, err := state.retry(ctx, c, account, err)
		if !again {
			return result, err
		}
	}
}
